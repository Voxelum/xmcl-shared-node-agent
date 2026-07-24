// Package workspace implements the v2 immutable workspace manifest protocol.
// Bytes move only through exact command-bound grants; this package has no S3
// credentials, list, stat, or delete operation.
package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	"github.com/voxelum/xmcl-shared-node-agent/internal/quota"
)

const (
	maxManifestBytes  int64 = 1 << 20
	maxWorkspaceBytes int64 = 64 << 30
	// v2 intentionally uses one immutable PUT per archive rather than general
	// multipart credentials, so archive creation stays below S3 multipart limits.
	maxBlobBytes      int64 = 4 << 30
	maxArchiveEntries       = 100_000
	worldTargetBytes  int64 = 192 << 20
	transferAttempts        = 3
)

// Transfer accepts only a broker-issued object URL for an expected key.
type Transfer interface {
	Download(context.Context, controlplane.WorkspaceGrant, string, int64, io.Writer) (TransferResult, error)
	Upload(context.Context, controlplane.WorkspaceGrant, string, io.Reader, int64, string) (TransferResult, error)
}

type TransferResult struct {
	Size   int64
	SHA256 string
}

type Manager struct {
	root     string
	grants   controlplane.WorkspaceGrantClient
	transfer Transfer
	quota    quota.Applier
	now      func() time.Time
	metrics  storageMetrics

	mu       sync.Mutex
	restored map[string]controlplane.WorkspaceManifest
}

func New(root string, grants controlplane.WorkspaceGrantClient, transfer Transfer, quotaApplier quota.Applier) *Manager {
	return &Manager{
		root: root, grants: grants, transfer: transfer, quota: quotaApplier,
		now: time.Now, restored: make(map[string]controlplane.WorkspaceManifest),
	}
}

func (m *Manager) Validate(ctx context.Context) error {
	if m.grants == nil || m.transfer == nil {
		return errors.New("workspace grant client and direct transfer client are required")
	}
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return fmt.Errorf("create workspace root: %w", err)
	}
	if err := m.quota.Validate(ctx); err != nil {
		return fmt.Errorf("validate workspace quota support: %w", err)
	}
	return nil
}

func (m *Manager) Path(serviceID string) (string, error) {
	if !validIdentifier(serviceID) {
		return "", errors.New("invalid service ID")
	}
	path := filepath.Join(m.root, serviceID)
	relative, err := filepath.Rel(m.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("service workspace escapes configured root")
	}
	return path, nil
}

func (m *Manager) Restore(ctx context.Context, command controlplane.Command) (string, error) {
	success := false
	defer func() {
		if !success {
			m.metrics.restoreFailures.Add(1)
		}
	}()
	if command.Workspace.Revision < 0 {
		return "", errors.New("workspace revision must be non-negative")
	}
	if !validWorkspaceLimit(command) {
		return "", errors.New("workspace quota is outside supported restore limits")
	}
	if _, err := workspacePrefix(command); err != nil {
		return "", err
	}
	workspacePath, err := m.Path(command.ServiceID)
	if err != nil {
		return "", err
	}
	if command.Workspace.Revision == 0 {
		path, err := m.activateEmptyWorkspace(command, workspacePath)
		if err != nil {
			return "", err
		}
		if err := m.quota.Apply(ctx, path, command.Resources.WorkspaceGiB); err != nil {
			return "", fmt.Errorf("apply workspace quota: %w", err)
		}
		success = true
		return path, nil
	}

	manifestKey := manifestKey(command.Workspace.ObjectPrefix, command.Workspace.Revision)
	response, err := m.grants.RestoreWorkspaceGrants(ctx, command, "manifest", nil)
	if err != nil {
		return "", fmt.Errorf("request manifest grant: %w", err)
	}
	manifestGrant, err := oneGrant(response, manifestKey, "GET")
	if err != nil {
		return "", err
	}
	var manifestBytes bytes.Buffer
	var result TransferResult
	for attempt := 0; attempt < transferAttempts; attempt++ {
		manifestBytes.Reset()
		result, err = m.transfer.Download(ctx, manifestGrant, manifestKey, maxManifestBytes, &manifestBytes)
		if err == nil {
			break
		}
		if attempt == transferAttempts-1 {
			return "", fmt.Errorf("download manifest: %w", err)
		}
		if err := sleep(ctx, time.Duration(1<<attempt)*100*time.Millisecond); err != nil {
			return "", err
		}
	}
	if err != nil {
		return "", fmt.Errorf("download manifest: %w", err)
	}
	m.metrics.restoreBytes.Add(result.Size)
	if command.Workspace.SHA256 != "" && command.Workspace.SHA256 != result.SHA256 {
		return "", errors.New("workspace manifest does not match assigned hash")
	}
	var manifest controlplane.WorkspaceManifest
	if err := json.Unmarshal(manifestBytes.Bytes(), &manifest); err != nil {
		return "", fmt.Errorf("decode workspace v2 manifest: %w", err)
	}
	if err := validateManifest(manifest, command); err != nil {
		return "", err
	}

	descriptors := manifestDescriptors(manifest)
	keys := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		keys = append(keys, descriptor.Key)
	}
	blobGrants, err := m.grants.RestoreWorkspaceGrants(ctx, command, "blobs", keys)
	if err != nil {
		return "", fmt.Errorf("request blob grants: %w", err)
	}
	grants, err := grantsByKey(blobGrants, keys, "GET")
	if err != nil {
		return "", err
	}
	stagingBase := filepath.Join(m.root, ".staging", command.ServiceID+"-"+command.CommandID)
	if err := os.RemoveAll(stagingBase); err != nil {
		return "", fmt.Errorf("clean restore staging directory: %w", err)
	}
	defer os.RemoveAll(stagingBase)
	extractionRoot := filepath.Join(stagingBase, "workspace")
	archiveRoot := filepath.Join(stagingBase, "blobs")
	if err := os.MkdirAll(archiveRoot, 0o750); err != nil {
		return "", fmt.Errorf("create restore staging directory: %w", err)
	}
	seen := make(map[string]struct{})
	var extractedBytes int64
	for index, descriptor := range descriptors {
		archivePath := filepath.Join(archiveRoot, fmt.Sprintf("%03d.tar.zst", index))
		download, err := m.downloadArchive(
			ctx,
			grants[descriptor.Key],
			descriptor,
			archivePath,
		)
		if err != nil {
			return "", err
		}
		m.metrics.restoreBytes.Add(download.Size)
		if download.Size != descriptor.CompressedSize || download.SHA256 != descriptor.SHA256 {
			return "", errors.New("workspace blob does not match its descriptor")
		}
		extracted, err := extractArchive(archivePath, extractionRoot, descriptor, seen, workspaceLimit(command))
		if err != nil {
			return "", err
		}
		extractedBytes += extracted
		if extractedBytes > workspaceLimit(command) {
			return "", errors.New("workspace exceeds configured extraction limit")
		}
	}
	if extractedBytes != manifest.LogicalSize {
		return "", errors.New("workspace archive logical size does not match manifest")
	}
	if err := m.activateStaging(ctx, command, extractionRoot, workspacePath); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.restored[command.ServiceID] = manifest
	m.mu.Unlock()
	m.metrics.logicalBytes.Store(manifest.LogicalSize)
	m.metrics.actualObjectBytes.Store(result.Size + compressedSize(manifestDescriptors(manifest)))
	success = true
	return workspacePath, nil
}

func (m *Manager) Sync(ctx context.Context, command controlplane.Command) (controlplane.SyncResult, error) {
	success := false
	defer func() {
		if !success {
			m.metrics.syncFailures.Add(1)
		}
	}()
	if command.Kind != controlplane.StopAndSync {
		return controlplane.SyncResult{}, errors.New("workspace sync requires stop-and-sync command")
	}
	if !validWorkspaceLimit(command) {
		return controlplane.SyncResult{}, errors.New("workspace quota is outside supported sync limits")
	}
	prefix, err := workspacePrefix(command)
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	workspacePath, err := m.Path(command.ServiceID)
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	files, createdAt, err := collectFiles(workspacePath)
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	revision := command.Workspace.Revision + 1
	stagingBase := filepath.Join(m.root, ".staging", command.ServiceID+"-"+command.CommandID+"-sync")
	if err := os.RemoveAll(stagingBase); err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("clean sync staging: %w", err)
	}
	defer os.RemoveAll(stagingBase)
	if err := os.MkdirAll(stagingBase, 0o750); err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("create sync staging: %w", err)
	}
	m.mu.Lock()
	previous := m.restored[command.ServiceID]
	m.mu.Unlock()
	manifest, archives, err := m.buildManifest(stagingBase, prefix, command, revision, files, createdAt, previous)
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("encode workspace v2 manifest: %w", err)
	}
	manifestSHA := digest(manifestBytes)
	response, err := m.grants.SyncWorkspaceGrants(ctx, command, manifest, manifestSHA)
	if err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("request sync grants: %w", err)
	}
	descriptorKeys := make([]string, 0, len(archives))
	for _, archive := range archives {
		descriptorKeys = append(descriptorKeys, archive.descriptor.Key)
	}
	uploadGrants, err := grantsByKeySubset(response, descriptorKeys, "PUT")
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	for _, archive := range archives {
		grant, upload := uploadGrants[archive.descriptor.Key]
		if !upload {
			continue // The control plane proved this immutable descriptor is published.
		}
		if err := m.uploadArchive(ctx, grant, archive); err != nil {
			return controlplane.SyncResult{}, err
		}
	}
	publish, err := m.grants.PublishWorkspaceGrant(ctx, command, manifest, manifestSHA)
	if err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("request manifest publish grant: %w", err)
	}
	manifestGrant, err := oneGrant(publish, manifestKey(command.Workspace.ObjectPrefix, revision), "PUT")
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	uploadedManifest, err := m.uploadBytes(
		ctx,
		manifestGrant,
		manifestGrant.Key,
		manifestBytes,
		manifestSHA,
	)
	if err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("publish workspace manifest: %w", err)
	}
	if uploadedManifest {
		m.metrics.syncBytes.Add(int64(len(manifestBytes)))
	}
	m.metrics.logicalBytes.Store(manifest.LogicalSize)
	m.metrics.actualObjectBytes.Store(int64(len(manifestBytes)) + compressedSize(manifestDescriptors(manifest)))
	m.mu.Lock()
	m.restored[command.ServiceID] = manifest
	m.mu.Unlock()
	success = true
	return controlplane.SyncResult{
		ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		Revision: revision, SizeBytes: manifest.LogicalSize, ManifestSHA: manifestSHA,
	}, nil
}

func (m *Manager) buildManifest(stagingBase, prefix string, command controlplane.Command, revision int64, files []workspaceFile, createdAt time.Time, previous controlplane.WorkspaceManifest) (controlplane.WorkspaceManifest, []archive, error) {
	layers := classify(files)
	var archives []archive
	content, err := buildArchive(filepath.Join(stagingBase, "content.tar.zst"), layers.content)
	if err != nil {
		return controlplane.WorkspaceManifest{}, nil, err
	}
	content.descriptor.Key = prefix + "/content/" + content.descriptor.SHA256 + ".tar.zst"
	if previous.Content != nil && sameArchive(*previous.Content, content.descriptor) {
		content.descriptor = *previous.Content
	}
	archives = append(archives, content)
	var config *archive
	if len(layers.config) > 0 {
		value, err := buildArchive(filepath.Join(stagingBase, "config.tar.zst"), layers.config)
		if err != nil {
			return controlplane.WorkspaceManifest{}, nil, err
		}
		value.descriptor.Key = fmt.Sprintf("%s/revisions/%d/config.tar.zst", prefix, revision)
		if previous.Config != nil && sameArchive(*previous.Config, value.descriptor) {
			value.descriptor = *previous.Config
		}
		config = &value
		archives = append(archives, value)
	}
	for index, group := range layers.world {
		value, err := buildArchive(filepath.Join(stagingBase, fmt.Sprintf("world-%03d.tar.zst", index)), group)
		if err != nil {
			return controlplane.WorkspaceManifest{}, nil, err
		}
		value.descriptor.Key = fmt.Sprintf("%s/revisions/%d/world/world-%03d.tar.zst", prefix, revision, index)
		for _, previousWorld := range previous.World {
			if sameArchive(previousWorld, value.descriptor) {
				value.descriptor = previousWorld
				break
			}
		}
		archives = append(archives, value)
	}
	manifest := controlplane.WorkspaceManifest{
		SchemaVersion: 2, ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		Revision: revision, CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
		Content: &content.descriptor, World: make([]controlplane.WorkspaceBlob, 0, len(layers.world)),
	}
	if config != nil {
		manifest.Config = &config.descriptor
	}
	for _, value := range archives {
		manifest.LogicalSize += value.descriptor.LogicalSize
		if strings.Contains(value.descriptor.Key, "/world/") {
			manifest.World = append(manifest.World, value.descriptor)
		}
	}
	manifest.AggregateSHA256 = aggregateDescriptors(manifestDescriptors(manifest))
	manifest.ManifestHash = manifest.AggregateSHA256
	return manifest, archives, nil
}

func (m *Manager) uploadArchive(ctx context.Context, grant controlplane.WorkspaceGrant, archive archive) error {
	for attempt := 0; attempt < transferAttempts; attempt++ {
		file, err := os.Open(archive.path)
		if err != nil {
			return fmt.Errorf("open workspace archive: %w", err)
		}
		result, uploadErr := m.transfer.Upload(ctx, grant, archive.descriptor.Key, file, archive.descriptor.CompressedSize, archive.descriptor.SHA256)
		closeErr := file.Close()
		if uploadErr == nil && closeErr == nil {
			if result.Size != archive.descriptor.CompressedSize || result.SHA256 != archive.descriptor.SHA256 {
				return errors.New("uploaded blob does not match descriptor")
			}
			m.metrics.syncBytes.Add(result.Size)
			return nil
		}
		if errors.Is(uploadErr, ErrAlreadyExists) {
			return nil
		}
		if uploadErr == nil {
			uploadErr = closeErr
		}
		if attempt == transferAttempts-1 {
			return fmt.Errorf("upload workspace blob: %w", uploadErr)
		}

		if err := sleep(ctx, time.Duration(1<<attempt)*100*time.Millisecond); err != nil {
			return err
		}
	}
	return errors.New("unreachable upload retry")
}

func (m *Manager) downloadArchive(
	ctx context.Context,
	grant controlplane.WorkspaceGrant,
	descriptor controlplane.WorkspaceBlob,
	path string,
) (TransferResult, error) {
	for attempt := 0; attempt < transferAttempts; attempt++ {
		_ = os.Remove(path)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
		if err != nil {
			return TransferResult{}, fmt.Errorf("create staged archive: %w", err)
		}
		result, transferErr := m.transfer.Download(
			ctx,
			grant,
			descriptor.Key,
			descriptor.CompressedSize,
			file,
		)
		closeErr := file.Close()
		if transferErr == nil && closeErr == nil {
			return result, nil
		}
		_ = os.Remove(path)
		if transferErr == nil {
			transferErr = closeErr
		}
		if attempt == transferAttempts-1 {
			return TransferResult{}, fmt.Errorf("download workspace blob: %w", transferErr)
		}
		if err := sleep(ctx, time.Duration(1<<attempt)*100*time.Millisecond); err != nil {
			return TransferResult{}, err
		}
	}
	return TransferResult{}, errors.New("unreachable download retry")
}

func (m *Manager) uploadBytes(
	ctx context.Context,
	grant controlplane.WorkspaceGrant,
	key string,
	value []byte,
	expectedSHA string,
) (bool, error) {
	for attempt := 0; attempt < transferAttempts; attempt++ {
		result, err := m.transfer.Upload(
			ctx,
			grant,
			key,
			bytes.NewReader(value),
			int64(len(value)),
			expectedSHA,
		)
		if err == nil {
			if result.Size != int64(len(value)) || result.SHA256 != expectedSHA {
				return false, errors.New("uploaded manifest does not match descriptor")
			}
			return true, nil
		}
		if errors.Is(err, ErrAlreadyExists) {
			return false, nil
		}
		if attempt == transferAttempts-1 {
			return false, err
		}
		if err := sleep(ctx, time.Duration(1<<attempt)*100*time.Millisecond); err != nil {
			return false, err
		}
	}
	return false, errors.New("unreachable upload retry")
}

func (m *Manager) activateEmptyWorkspace(command controlplane.Command, workspacePath string) (string, error) {
	staging := filepath.Join(m.root, ".staging", command.ServiceID+"-"+command.CommandID, "workspace")
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return "", err
	}
	if err := replaceWorkspace(staging, workspacePath); err != nil {
		return "", err
	}
	return workspacePath, nil
}

func (m *Manager) activateStaging(ctx context.Context, command controlplane.Command, staging, workspacePath string) error {
	backup, err := replaceWorkspaceWithBackup(staging, workspacePath)
	if err != nil {
		return err
	}
	if err := m.quota.Apply(ctx, workspacePath, command.Resources.WorkspaceGiB); err != nil {
		_ = os.RemoveAll(workspacePath)
		if backup != "" {
			_ = os.Rename(backup, workspacePath)
		}
		return fmt.Errorf("apply workspace quota: %w", err)
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func (m *Manager) Release(_ context.Context, command controlplane.Command) error {
	path, err := m.Path(command.ServiceID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove released local workspace: %w", err)
	}
	m.mu.Lock()
	delete(m.restored, command.ServiceID)
	m.mu.Unlock()
	return nil
}

type workspaceFile struct {
	path string
	full string
	size int64
}

type archive struct {
	path       string
	descriptor controlplane.WorkspaceBlob
}

type layers struct {
	content []workspaceFile
	config  []workspaceFile
	world   [][]workspaceFile
}

func classify(files []workspaceFile) layers {
	result := layers{}
	worldGroups := map[string][]workspaceFile{}
	for _, file := range files {
		parts := strings.Split(file.path, "/")
		switch parts[0] {
		case "world", "world_nether", "world_the_end":
			group := parts[0]
			if len(parts) > 1 {
				group += "/" + parts[1]
			}
			worldGroups[group] = append(worldGroups[group], file)
		case "config", "defaultconfigs":
			result.config = append(result.config, file)
		default:
			result.content = append(result.content, file)
		}
	}
	keys := make([]string, 0, len(worldGroups))
	for key := range worldGroups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := worldGroups[key]
		sortFiles(group)
		var shard []workspaceFile
		var size int64
		for _, file := range group {
			if len(shard) > 0 && size+file.size > worldTargetBytes {
				result.world = append(result.world, shard)
				shard, size = nil, 0
			}
			shard = append(shard, file)
			size += file.size
		}
		if len(shard) > 0 {
			result.world = append(result.world, shard)
		}
	}
	sortFiles(result.content)
	sortFiles(result.config)
	return result
}

func buildArchive(path string, files []workspaceFile) (archive, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return archive{}, err
	}
	encoder, err := zstd.NewWriter(file)
	if err != nil {
		_ = file.Close()
		return archive{}, err
	}
	writer := tar.NewWriter(encoder)
	var logicalSize int64
	for _, source := range files {
		if !validWorkspacePath(source.path) || source.size < 0 || source.size > maxWorkspaceBytes {
			_ = writer.Close()
			_ = encoder.Close()
			_ = file.Close()
			return archive{}, errors.New("invalid workspace archive path or file size")
		}
		header := &tar.Header{
			Name: source.path, Mode: 0o640, Size: source.size, Typeflag: tar.TypeReg,
			ModTime: time.Unix(0, 0).UTC(), AccessTime: time.Time{}, ChangeTime: time.Time{},
			Format: tar.FormatPAX,
		}
		if err := writer.WriteHeader(header); err != nil {
			_ = writer.Close()
			_ = encoder.Close()
			_ = file.Close()
			return archive{}, err
		}
		input, err := os.Open(source.full)
		if err != nil {
			_ = writer.Close()
			_ = encoder.Close()
			_ = file.Close()
			return archive{}, err
		}
		written, copyErr := io.Copy(writer, io.LimitReader(input, source.size+1))
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil || written != source.size {
			_ = writer.Close()
			_ = encoder.Close()
			_ = file.Close()
			return archive{}, errors.New("workspace file changed while being archived")
		}
		logicalSize += written
	}
	if err := writer.Close(); err != nil {
		_ = encoder.Close()
		_ = file.Close()
		return archive{}, err
	}
	if err := encoder.Close(); err != nil {
		_ = file.Close()
		return archive{}, err
	}
	if err := file.Close(); err != nil {
		return archive{}, err
	}
	size, sum, err := hashFile(path, maxBlobBytes)
	if err != nil {
		return archive{}, err
	}
	paths := make([]string, 0, len(files))
	for _, source := range files {
		paths = append(paths, source.path)
	}
	return archive{
		path: path,
		descriptor: controlplane.WorkspaceBlob{
			SHA256: sum, CompressedSize: size, LogicalSize: logicalSize, Paths: paths,
		},
	}, nil
}

func extractArchive(path, root string, descriptor controlplane.WorkspaceBlob, seen map[string]struct{}, limit int64) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader, err := zstd.NewReader(file)
	if err != nil {
		return 0, fmt.Errorf("open zstd archive: %w", err)
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	expected := make(map[string]struct{}, len(descriptor.Paths))
	for _, path := range descriptor.Paths {
		if !validWorkspacePath(path) {
			return 0, errors.New("manifest contains invalid archive path")
		}
		if _, duplicate := expected[path]; duplicate {
			return 0, errors.New("manifest contains duplicate archive path")
		}
		expected[path] = struct{}{}
	}
	var total int64
	entries := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read tar archive: %w", err)
		}
		entries++
		if entries > maxArchiveEntries || header.Typeflag != tar.TypeReg || !validWorkspacePath(header.Name) ||
			header.Size < 0 || header.Size > limit || total+header.Size > limit {
			return 0, errors.New("workspace archive violates extraction limits")
		}
		if _, allowed := expected[header.Name]; !allowed {
			return 0, errors.New("workspace archive member is not in manifest mapping")
		}
		if _, duplicate := seen[header.Name]; duplicate {
			return 0, errors.New("workspace archive contains duplicate local path")
		}
		target, err := safeChild(root, header.Name)
		if err != nil {
			return 0, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return 0, err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			return 0, err
		}
		written, copyErr := io.Copy(output, io.LimitReader(archive, header.Size+1))
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil || written != header.Size {
			_ = os.Remove(target)
			return 0, errors.New("workspace archive member has invalid size")
		}
		seen[header.Name] = struct{}{}
		delete(expected, header.Name)
		total += written
	}
	if len(expected) != 0 || total != descriptor.LogicalSize {
		return 0, errors.New("workspace archive does not match manifest path mapping")
	}
	return total, nil
}

func validateManifest(manifest controlplane.WorkspaceManifest, command controlplane.Command) error {
	if manifest.SchemaVersion != 2 || manifest.ServiceID != command.ServiceID ||
		manifest.Revision != command.Workspace.Revision || manifest.AssignmentID == "" ||
		!validSHA256(manifest.ManifestHash) || !validSHA256(manifest.AggregateSHA256) ||
		manifest.ManifestHash != manifest.AggregateSHA256 || manifest.Content == nil ||
		manifest.LogicalSize < 0 || manifest.LogicalSize > workspaceLimit(command) {
		return errors.New("workspace v2 manifest does not match assigned service or revision")
	}
	prefix, err := workspacePrefix(command)
	if err != nil {
		return err
	}
	descriptors := manifestDescriptors(manifest)
	if len(descriptors) == 0 || len(descriptors) > 130 || aggregateDescriptors(descriptors) != manifest.AggregateSHA256 {
		return errors.New("workspace manifest blob descriptor aggregate is invalid")
	}
	keys, paths := make(map[string]struct{}), make(map[string]struct{})
	var logicalSize, compressedSize int64
	for _, descriptor := range descriptors {
		if !validDescriptor(descriptor) {
			return errors.New("workspace manifest has an invalid blob descriptor")
		}
		if _, duplicate := keys[descriptor.Key]; duplicate {
			return errors.New("workspace manifest has duplicate blob key")
		}
		keys[descriptor.Key] = struct{}{}
		for _, path := range descriptor.Paths {
			if !validWorkspacePath(path) {
				return errors.New("workspace manifest has an unsafe local path")
			}
			if _, duplicate := paths[path]; duplicate {
				return errors.New("workspace manifest has duplicate local path")
			}
			paths[path] = struct{}{}
		}
		logicalSize += descriptor.LogicalSize
		compressedSize += descriptor.CompressedSize
	}
	if logicalSize != manifest.LogicalSize || compressedSize > maxWorkspaceBytes || len(paths) > maxArchiveEntries {
		return errors.New("workspace manifest logical size is invalid")
	}
	if manifest.Content.Key != prefix+"/content/"+manifest.Content.SHA256+".tar.zst" ||
		!allPaths(manifest.Content.Paths, isContentPath) {
		return errors.New("workspace manifest content layer is invalid")
	}
	if manifest.Config != nil {
		if !revisionLayerKey(prefix, manifest.Config.Key, "config.tar.zst") ||
			!allPaths(manifest.Config.Paths, isConfigPath) {
			return errors.New("workspace manifest config layer is invalid")
		}
	}
	for _, descriptor := range manifest.World {
		if !worldLayerKey(prefix, descriptor.Key) || !allPaths(descriptor.Paths, isWorldPath) {
			return errors.New("workspace manifest world layer is invalid")
		}
	}
	return nil
}

func collectFiles(root string) ([]workspaceFile, time.Time, error) {
	var files []workspaceFile
	createdAt := time.Unix(0, 0).UTC()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("workspace contains non-regular file %q", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || info.Size() > maxWorkspaceBytes {
			return errors.New("workspace file exceeds configured maximum")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !validWorkspacePath(relative) {
			return errors.New("workspace contains invalid path")
		}
		files = append(files, workspaceFile{path: relative, full: path, size: info.Size()})
		if info.ModTime().After(createdAt) {
			createdAt = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("walk workspace: %w", err)
	}
	sortFiles(files)
	return files, createdAt, nil
}

func manifestDescriptors(manifest controlplane.WorkspaceManifest) []controlplane.WorkspaceBlob {
	result := make([]controlplane.WorkspaceBlob, 0, 2+len(manifest.World))
	if manifest.Content != nil {
		result = append(result, *manifest.Content)
	}
	if manifest.Config != nil {
		result = append(result, *manifest.Config)
	}
	result = append(result, manifest.World...)
	return result
}

func compressedSize(descriptors []controlplane.WorkspaceBlob) int64 {
	var total int64
	for _, descriptor := range descriptors {
		total += descriptor.CompressedSize
	}
	return total
}

func aggregateDescriptors(descriptors []controlplane.WorkspaceBlob) string {
	hash := sha256.New()
	for _, descriptor := range descriptors {
		_, _ = io.WriteString(hash, descriptor.Key)
		_, _ = hash.Write([]byte{0})
		_, _ = io.WriteString(hash, descriptor.SHA256)
		_, _ = hash.Write([]byte{0})
		_, _ = io.WriteString(hash, fmt.Sprintf("%d:%d", descriptor.CompressedSize, descriptor.LogicalSize))
		_, _ = hash.Write([]byte{0})
		for _, path := range descriptor.Paths {
			_, _ = io.WriteString(hash, path)
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func grantsByKey(response controlplane.WorkspaceGrantResponse, keys []string, method string) (map[string]controlplane.WorkspaceGrant, error) {
	result, err := grantsByKeySubset(response, keys, method)
	if err != nil {
		return nil, err
	}
	if len(result) != len(keys) {
		return nil, errors.New("broker did not grant every required object")
	}
	return result, nil
}

func grantsByKeySubset(response controlplane.WorkspaceGrantResponse, allowed []string, method string) (map[string]controlplane.WorkspaceGrant, error) {
	allowedKeys := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedKeys[key] = struct{}{}
	}
	result := make(map[string]controlplane.WorkspaceGrant, len(response.Grants))
	for _, grant := range response.Grants {
		if grant.Method != method {
			return nil, errors.New("broker grant has unexpected HTTP method")
		}
		if _, allowed := allowedKeys[grant.Key]; !allowed {
			return nil, errors.New("broker grant is for an unexpected object key")
		}
		if _, duplicate := result[grant.Key]; duplicate {
			return nil, errors.New("broker returned duplicate object grant")
		}
		result[grant.Key] = grant
	}
	return result, nil
}

func oneGrant(response controlplane.WorkspaceGrantResponse, key, method string) (controlplane.WorkspaceGrant, error) {
	grants, err := grantsByKey(response, []string{key}, method)
	if err != nil {
		return controlplane.WorkspaceGrant{}, err
	}
	return grants[key], nil
}

func workspacePrefix(command controlplane.Command) (string, error) {
	prefix := strings.TrimSuffix(command.Workspace.ObjectPrefix, "/")
	expected := "shared-hosting/" + command.AccountID + "/" + command.ServiceID
	if prefix != expected || command.AccountID == "" || command.ServiceID == "" {
		return "", errors.New("workspace prefix is not the assigned service prefix")
	}
	return prefix, nil
}

func manifestKey(prefix string, revision int64) string {
	return strings.TrimSuffix(prefix, "/") + fmt.Sprintf("/revisions/%d/manifest.json", revision)
}

func workspaceLimit(command controlplane.Command) int64 {
	limit := command.Resources.WorkspaceGiB * 1024 * 1024 * 1024
	return limit
}

func validWorkspaceLimit(command controlplane.Command) bool {
	return command.Resources.WorkspaceGiB >= 1 &&
		command.Resources.WorkspaceGiB <= maxWorkspaceBytes/(1024*1024*1024)
}

func validDescriptor(value controlplane.WorkspaceBlob) bool {
	return value.Key != "" && validSHA256(value.SHA256) &&
		value.CompressedSize > 0 && value.CompressedSize <= maxBlobBytes &&
		value.LogicalSize >= 0 && value.LogicalSize <= maxWorkspaceBytes &&
		len(value.Paths) <= maxArchiveEntries
}

func sameArchive(left, right controlplane.WorkspaceBlob) bool {
	return left.SHA256 == right.SHA256 &&
		left.CompressedSize == right.CompressedSize &&
		left.LogicalSize == right.LogicalSize &&
		strings.Join(left.Paths, "\x00") == strings.Join(right.Paths, "\x00")
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	if strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validWorkspacePath(value string) bool {
	if value == "" || len(value) > 1024 || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func isWorldPath(path string) bool {
	return path == "world" || strings.HasPrefix(path, "world/") ||
		path == "world_nether" || strings.HasPrefix(path, "world_nether/") ||
		path == "world_the_end" || strings.HasPrefix(path, "world_the_end/")
}

func isConfigPath(path string) bool {
	return path == "config" || strings.HasPrefix(path, "config/") ||
		path == "defaultconfigs" || strings.HasPrefix(path, "defaultconfigs/")
}

func isContentPath(path string) bool {
	return !isWorldPath(path) && !isConfigPath(path)
}

func allPaths(paths []string, predicate func(string) bool) bool {
	for _, path := range paths {
		if !predicate(path) {
			return false
		}
	}
	return true
}

func revisionLayerKey(prefix, key, name string) bool {
	parts := strings.Split(strings.TrimPrefix(key, prefix+"/revisions/"), "/")
	return strings.HasPrefix(key, prefix+"/revisions/") && len(parts) == 2 &&
		parts[0] != "" && strings.TrimLeft(parts[0], "0123456789") == "" && parts[1] == name
}

func worldLayerKey(prefix, key string) bool {
	parts := strings.Split(strings.TrimPrefix(key, prefix+"/revisions/"), "/")
	if !strings.HasPrefix(key, prefix+"/revisions/") || len(parts) != 3 ||
		parts[0] == "" || strings.TrimLeft(parts[0], "0123456789") != "" ||
		parts[1] != "world" || !strings.HasSuffix(parts[2], ".tar.zst") {
		return false
	}
	shard := strings.TrimSuffix(parts[2], ".tar.zst")
	if shard == "" || len(shard) > 128 {
		return false
	}
	for _, character := range shard {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func safeChild(root, name string) (string, error) {
	if !validWorkspacePath(name) {
		return "", errors.New("path must be a safe relative slash-separated name")
	}
	child := filepath.Join(root, filepath.FromSlash(name))
	relative, err := filepath.Rel(root, child)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace root")
	}
	return child, nil
}

func replaceWorkspace(staging, workspacePath string) error {
	backup, err := replaceWorkspaceWithBackup(staging, workspacePath)
	if err == nil && backup != "" {
		err = os.RemoveAll(backup)
	}
	return err
}

func replaceWorkspaceWithBackup(staging, workspacePath string) (string, error) {
	backup := workspacePath + ".previous"
	if err := os.RemoveAll(backup); err != nil {
		return "", err
	}
	if _, err := os.Stat(workspacePath); err == nil {
		if err := os.Rename(workspacePath, backup); err != nil {
			return "", fmt.Errorf("stage previous workspace: %w", err)
		}
	}
	if err := os.Rename(staging, workspacePath); err != nil {
		if _, statErr := os.Stat(backup); statErr == nil {
			_ = os.Rename(backup, workspacePath)
		}
		return "", fmt.Errorf("activate validated workspace: %w", err)
	}
	return backup, nil
}

func hashFile(path string, limit int64) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, limit+1))
	if err != nil {
		return written, "", err
	}
	if written > limit {
		return written, "", errors.New("file exceeds configured limit")
	}
	return written, hex.EncodeToString(hash.Sum(nil)), nil
}

func sortFiles(values []workspaceFile) {
	sort.Slice(values, func(left, right int) bool { return values[left].path < values[right].path })
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
