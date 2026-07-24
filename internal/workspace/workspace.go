package workspace

import (
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
	"strconv"
	"strings"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	"github.com/voxelum/xmcl-shared-node-agent/internal/objectstore"
	"github.com/voxelum/xmcl-shared-node-agent/internal/quota"
)

type ObjectStore interface {
	Download(ctx context.Context, key string) ([]byte, error)
	DownloadTo(ctx context.Context, key string, destination io.Writer) (int64, error)
	Exists(ctx context.Context, key string) (bool, error)
	Upload(ctx context.Context, key string, data []byte, contentType string) error
	UploadFile(ctx context.Context, key, path, contentType string) (int64, error)
	List(ctx context.Context, prefix string) ([]objectstore.ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

const (
	maxManifestBytes  int64 = 1 << 20
	maxWorkspaceBytes int64 = 64 << 30
)

type File struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type Manifest struct {
	SchemaVersion int64  `json:"schemaVersion"`
	ServiceID     string `json:"serviceId"`
	AssignmentID  string `json:"assignmentId"`
	Revision      int64  `json:"revision"`
	CreatedAt     string `json:"createdAt"`
	SizeBytes     int64  `json:"sizeBytes"`
	SHA256        string `json:"sha256"`
	Files         []File `json:"files"`
}

type Manager struct {
	root    string
	store   ObjectStore
	quota   quota.Applier
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	metrics storageMetrics
}

func New(root string, store ObjectStore, quotaApplier quota.Applier) *Manager {
	return &Manager{root: root, store: store, quota: quotaApplier, now: time.Now, sleep: sleep}
}

func (m *Manager) Validate(ctx context.Context) error {
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
		return "", fmt.Errorf("invalid service ID")
	}
	path := filepath.Join(m.root, serviceID)
	relative, err := filepath.Rel(m.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("service workspace escapes configured root")
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
		return "", fmt.Errorf("workspace revision must be non-negative")
	}
	prefix, err := cleanPrefix(command.Workspace.ObjectPrefix)
	if err != nil {
		return "", err
	}
	workspacePath, err := m.Path(command.ServiceID)
	if err != nil {
		return "", err
	}
	manifestKey := manifestKey(prefix, command.Workspace.Revision)
	manifestExists, err := m.store.Exists(ctx, manifestKey)
	if err != nil {
		return "", fmt.Errorf("check workspace manifest: %w", err)
	}
	if !manifestExists {
		if command.Workspace.Revision != 0 {
			return "", errors.New("workspace manifest is required for revisions greater than zero")
		}
		workspacePath, err := m.activateEmptyWorkspace(command, workspacePath)
		if err != nil {
			return "", err
		}
		if err := m.quota.Apply(ctx, workspacePath, command.Resources.WorkspaceGiB); err != nil {
			return "", fmt.Errorf("apply workspace quota: %w", err)
		}
		m.metrics.logicalBytes.Store(0)
		success = true
		return workspacePath, nil
	}
	manifestData, err := m.downloadManifest(ctx, manifestKey)
	if err != nil {
		return "", fmt.Errorf("download workspace manifest: %w", err)
	}

	m.metrics.restoreBytes.Add(int64(len(manifestData)))
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return "", fmt.Errorf("decode workspace manifest: %w", err)
	}
	if err := validateManifest(manifest, command); err != nil {
		return "", err
	}
	if command.Workspace.SHA256 != "" && command.Workspace.SHA256 != digest(manifestData) {
		return "", errors.New("workspace manifest does not match the assigned hash")
	}
	staging := filepath.Join(m.root, ".staging", command.ServiceID+"-"+command.CommandID)
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("clean restore staging directory: %w", err)
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return "", fmt.Errorf("create restore staging directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	for _, file := range manifest.Files {
		target, err := safeChild(staging, file.Path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return "", fmt.Errorf("create workspace directory: %w", err)
		}
		dataSize, dataHash, err := m.downloadFile(ctx, fileKey(prefix, command.Workspace.Revision, file.Path), target, file.SizeBytes)
		if err != nil {
			return "", fmt.Errorf("download workspace file %q: %w", file.Path, err)
		}
		m.metrics.restoreBytes.Add(dataSize)
		if dataSize != file.SizeBytes || dataHash != file.SHA256 {
			return "", fmt.Errorf("workspace file %q does not match its manifest hash", file.Path)
		}
	}
	if err := os.RemoveAll(workspacePath); err != nil {
		return "", fmt.Errorf("replace previous workspace: %w", err)
	}
	if err := os.Rename(staging, workspacePath); err != nil {
		return "", fmt.Errorf("activate restored workspace: %w", err)
	}
	cleanup = false
	if err := m.quota.Apply(ctx, workspacePath, command.Resources.WorkspaceGiB); err != nil {
		return "", fmt.Errorf("apply workspace quota: %w", err)
	}
	m.metrics.logicalBytes.Store(manifest.SizeBytes)
	success = true
	return workspacePath, nil
}

func (m *Manager) activateEmptyWorkspace(command controlplane.Command, workspacePath string) (string, error) {
	staging := filepath.Join(m.root, ".staging", command.ServiceID+"-"+command.CommandID)
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("clean empty workspace staging directory: %w", err)
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return "", fmt.Errorf("create empty workspace staging directory: %w", err)
	}
	if err := os.RemoveAll(workspacePath); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("replace previous workspace: %w", err)
	}
	if err := os.Rename(staging, workspacePath); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("activate empty workspace: %w", err)
	}
	return workspacePath, nil
}

func (m *Manager) Sync(ctx context.Context, command controlplane.Command) (controlplane.SyncResult, error) {
	success := false
	defer func() {
		if !success {
			m.metrics.syncFailures.Add(1)
		}
	}()
	workspacePath, err := m.Path(command.ServiceID)
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	prefix, err := cleanPrefix(command.Workspace.ObjectPrefix)
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	files, err := collectFiles(workspacePath)
	if err != nil {
		return controlplane.SyncResult{}, err
	}
	revision := command.Workspace.Revision + 1
	manifestObjectKey := manifestKey(prefix, revision)
	exists, err := m.store.Exists(ctx, manifestObjectKey)
	if err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("check existing revision manifest: %w", err)
	}
	if exists {
		data, err := m.downloadManifest(ctx, manifestObjectKey)
		if err != nil {
			return controlplane.SyncResult{}, fmt.Errorf("read existing revision manifest: %w", err)
		}
		var manifest Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return controlplane.SyncResult{}, fmt.Errorf("decode existing revision manifest: %w", err)
		}
		if err := validatePublishedManifest(manifest, command, revision); err != nil {
			return controlplane.SyncResult{}, fmt.Errorf("existing revision is not this command's completed workspace: %w", err)
		}
		m.metrics.logicalBytes.Store(manifest.SizeBytes)
		success = true
		return controlplane.SyncResult{
			ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
			Revision: revision, SizeBytes: manifest.SizeBytes, ManifestSHA: digest(data),
		}, nil
	}
	manifest := Manifest{
		SchemaVersion: 1,
		ServiceID:     command.ServiceID,
		AssignmentID:  command.AssignmentID,
		Revision:      revision,
		CreatedAt:     m.now().UTC().Format(time.RFC3339Nano),
		Files:         make([]File, 0, len(files)),
	}
	for _, path := range files {
		localPath := filepath.Join(workspacePath, filepath.FromSlash(path))
		size, fileHash, err := hashFile(localPath)
		if err != nil {
			return controlplane.SyncResult{}, fmt.Errorf("read workspace file %q: %w", path, err)
		}
		file := File{Path: path, SizeBytes: size, SHA256: fileHash}
		manifest.Files = append(manifest.Files, file)
		manifest.SizeBytes += file.SizeBytes
		if err := m.uploadFile(ctx, fileKey(prefix, revision, path), localPath, file.SizeBytes); err != nil {
			return controlplane.SyncResult{}, fmt.Errorf("upload workspace file %q: %w", path, err)
		}
	}
	manifest.SHA256 = aggregateHash(manifest.Files)
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("encode workspace manifest: %w", err)
	}
	manifestSHA := digest(manifestData)
	// Publishing this object last makes every visible revision fully restorable.
	if err := m.upload(ctx, manifestObjectKey, manifestData, "application/json"); err != nil {
		return controlplane.SyncResult{}, fmt.Errorf("publish workspace manifest: %w", err)
	}
	m.metrics.logicalBytes.Store(manifest.SizeBytes)
	success = true
	return controlplane.SyncResult{
		ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		Revision: revision, SizeBytes: manifest.SizeBytes, ManifestSHA: manifestSHA,
	}, nil
}

// RefreshObjectBytes records the total physical object bytes retained under a
// service prefix, including retained historical revisions.
func (m *Manager) RefreshObjectBytes(ctx context.Context, objectPrefix string) error {
	prefix, err := cleanPrefix(objectPrefix)
	if err != nil {
		return err
	}
	objects, err := m.store.List(ctx, prefix+"/")
	if err != nil {
		return fmt.Errorf("list workspace objects: %w", err)
	}
	var total int64
	for _, object := range objects {
		total += object.Size
	}
	m.metrics.actualObjectBytes.Store(total)
	return nil
}

const incompleteRevisionGrace = 24 * time.Hour

// CleanupIncomplete removes only stale revisions that have no manifest. A
// manifest is the commit marker, so complete and currently referenced
// revisions are never candidates for deletion.
func (m *Manager) CleanupIncomplete(ctx context.Context, objectPrefix string, currentRevision int64) error {
	prefix, err := cleanPrefix(objectPrefix)
	if err != nil {
		return err
	}
	revisionPrefix := prefix + "/revisions/"
	objects, err := m.store.List(ctx, revisionPrefix)
	if err != nil {
		return fmt.Errorf("list incomplete revisions: %w", err)
	}
	type revisionObjects struct {
		objects  []objectstore.ObjectInfo
		manifest bool
		newest   time.Time
	}
	revisions := make(map[int64]*revisionObjects)
	for _, object := range objects {
		relative := strings.TrimPrefix(object.Key, revisionPrefix)
		parts := strings.SplitN(relative, "/", 2)
		if len(parts) != 2 {
			continue
		}
		revision, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || revision < 0 {
			continue
		}
		entry := revisions[revision]
		if entry == nil {
			entry = &revisionObjects{}
			revisions[revision] = entry
		}
		entry.objects = append(entry.objects, object)
		entry.manifest = entry.manifest || parts[1] == "manifest.json"
		if object.LastModified.After(entry.newest) {
			entry.newest = object.LastModified
		}
	}
	for revision, entry := range revisions {
		if revision == currentRevision || entry.manifest || m.now().Sub(entry.newest) < incompleteRevisionGrace {
			continue
		}
		for _, object := range entry.objects {
			if err := m.store.Delete(ctx, object.Key); err != nil {
				return fmt.Errorf("delete incomplete revision %d: %w", revision, err)
			}
		}
	}
	return nil
}

// Release evicts the local execution copy only after the control plane has
// acknowledged a durable sync. Object storage remains authoritative.
func (m *Manager) Release(_ context.Context, command controlplane.Command) error {
	path, err := m.Path(command.ServiceID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove released local workspace: %w", err)
	}
	return nil
}

func validateManifest(manifest Manifest, command controlplane.Command) error {
	if manifest.SchemaVersion != 1 || manifest.ServiceID != command.ServiceID || manifest.AssignmentID != command.AssignmentID ||
		manifest.Revision != command.Workspace.Revision {
		return errors.New("workspace manifest does not match the assigned service, assignment, or revision")
	}

	var size int64
	workspaceLimit := command.Resources.WorkspaceGiB * 1024 * 1024 * 1024
	if workspaceLimit < 1 || workspaceLimit > maxWorkspaceBytes {
		workspaceLimit = maxWorkspaceBytes
	}
	for _, file := range manifest.Files {
		if _, err := safeChild(string(filepath.Separator), file.Path); err != nil {
			return fmt.Errorf("invalid workspace manifest path %q: %w", file.Path, err)
		}
		if file.SizeBytes < 0 || file.SizeBytes > maxWorkspaceBytes || len(file.SHA256) != sha256.Size*2 {
			return fmt.Errorf("invalid workspace manifest file %q", file.Path)
		}
		size += file.SizeBytes
	}
	if size > workspaceLimit || size != manifest.SizeBytes || aggregateHash(manifest.Files) != manifest.SHA256 {
		return errors.New("workspace manifest aggregate hash or size is invalid")
	}

	return nil
}

func (m *Manager) downloadManifest(ctx context.Context, key string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := &limitedWriter{writer: &buffer, remaining: maxManifestBytes}
	if _, err := m.store.DownloadTo(ctx, key, writer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (m *Manager) downloadFile(ctx context.Context, key, path string, expectedSize int64) (int64, string, error) {
	if expectedSize < 0 || expectedSize > maxWorkspaceBytes {
		return 0, "", errors.New("workspace file exceeds the configured maximum")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	writer := &limitedWriter{writer: io.MultiWriter(file, hash), remaining: expectedSize}
	size, downloadErr := m.store.DownloadTo(ctx, key, writer)
	closeErr := file.Close()
	if downloadErr != nil {
		_ = os.Remove(path)
		return size, "", downloadErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return size, "", closeErr
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func hashFile(path string) (int64, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, "", err
	}
	if info.Size() > maxWorkspaceBytes {
		return 0, "", errors.New("workspace file exceeds the configured maximum")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return size, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("object exceeds configured transfer limit")
	}
	count, err := w.writer.Write(data)
	w.remaining -= int64(count)
	return count, err
}

func validatePublishedManifest(manifest Manifest, command controlplane.Command, revision int64) error {
	copy := command
	copy.Workspace.Revision = revision
	return validateManifest(manifest, copy)
}

func collectFiles(root string) ([]string, error) {
	var files []string
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
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk workspace: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func safeChild(root, name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", errors.New("path must be a relative slash-separated name")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("path contains an empty, dot, or traversal segment")
		}
	}
	child := filepath.Join(root, filepath.FromSlash(name))
	relative, err := filepath.Rel(root, child)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace root")
	}
	return child, nil
}

func cleanPrefix(prefix string) (string, error) {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", errors.New("workspace object prefix is required")
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			return "", errors.New("workspace object prefix is invalid")
		}
	}
	return prefix, nil
}

func manifestKey(prefix string, revision int64) string {
	return prefix + "/revisions/" + fmt.Sprintf("%d", revision) + "/manifest.json"
}

func fileKey(prefix string, revision int64, path string) string {
	return prefix + "/revisions/" + fmt.Sprintf("%d", revision) + "/files/" + path
}

func (m *Manager) upload(ctx context.Context, key string, data []byte, contentType string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = m.store.Upload(ctx, key, data, contentType); err == nil {
			m.metrics.syncBytes.Add(int64(len(data)))
			return nil
		}
		if attempt < 2 {
			if waitErr := m.sleep(ctx, time.Duration(1<<attempt)*100*time.Millisecond); waitErr != nil {
				return waitErr
			}
		}
	}
	return err
}

func (m *Manager) uploadFile(ctx context.Context, key, path string, expectedSize int64) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var uploaded int64
		uploaded, err = m.store.UploadFile(ctx, key, path, "application/octet-stream")
		if err == nil {
			if uploaded != expectedSize {
				return fmt.Errorf("uploaded size %d does not match expected size %d", uploaded, expectedSize)
			}
			m.metrics.syncBytes.Add(uploaded)
			return nil
		}
		if attempt < 2 {
			if waitErr := m.sleep(ctx, time.Duration(1<<attempt)*100*time.Millisecond); waitErr != nil {
				return waitErr
			}
		}
	}
	return err
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

func aggregateHash(files []File) string {
	hash := sha256.New()
	for _, file := range files {
		_, _ = hash.Write([]byte(file.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(fmt.Sprintf("%d", file.SizeBytes)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(file.SHA256))
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
