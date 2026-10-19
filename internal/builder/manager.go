package builder

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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	runtimecontract "github.com/voxelum/xmcl-shared-node-agent/internal/runtime"
	"github.com/voxelum/xmcl-shared-node-agent/internal/workspace"
)

const maxManifestBytes int64 = 1 << 20

type Runner interface {
	Run(context.Context, controlplane.Command, string) error
}

type Metrics interface {
	RecordBuild(context.Context, string, string, time.Duration)
}

type Manager struct {
	root     string
	identity Identity
	grants   controlplane.BuildGrantClient
	transfer workspace.Transfer
	runner   Runner
	client   *http.Client
	locks    sync.Map
	metrics  Metrics
}

func (m *Manager) SetMetrics(metrics Metrics) {
	m.metrics = metrics
}

type mountedDependency struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

type dependencyIndex struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Dependencies  []mountedDependency `json:"dependencies"`
}

type builderResult struct {
	SchemaVersion int                        `json:"schemaVersion"`
	BuildKey      string                     `json:"buildKey"`
	Content       controlplane.WorkspaceBlob `json:"content"`
}

func New(root string, identity Identity, grants controlplane.BuildGrantClient, transfer workspace.Transfer, runner Runner, client *http.Client) (*Manager, error) {
	if root == "" || grants == nil || transfer == nil || runner == nil {
		return nil, errors.New("builder manager requires root, grants, transfer, and runner")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve builder root: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Manager{root: absolute, identity: identity, grants: grants, transfer: transfer, runner: runner, client: &clone}, nil
}

func (m *Manager) Validate() error {
	if !digestImage(m.identity.BuilderImage, BuilderImageName) ||
		!digestImage(m.identity.RuntimeImage, RuntimeImageName) ||
		!shaPattern.MatchString(m.identity.ToolchainCatalogRevision) ||
		!shaPattern.MatchString(m.identity.RuntimeCatalogRevision) {
		return errors.New("invalid local builder identity")
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return err
	}
	return m.pruneStaging()
}

func (m *Manager) Prepare(ctx context.Context, command controlplane.Command) (result controlplane.PreparedContentResult, resultErr error) {
	started := time.Now()
	defer func() {
		if m.metrics == nil {
			return
		}
		code := ""
		var failure *Failure
		if errors.As(resultErr, &failure) {
			code = failure.Code
		} else if resultErr != nil {
			code = "unclassified"
		}
		m.metrics.RecordBuild(ctx, result.Cache, code, time.Since(started))
	}()
	if err := ValidateRequest(command, m.identity); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	buildKey := command.Prepare.ExpectedBuildKey
	lock := m.buildLock(buildKey)
	lock.Lock()
	defer lock.Unlock()
	contentKey, manifestKey, _ := CacheKeys(buildKey)
	stage := filepath.Join(m.root, digestBytes([]byte(command.CommandID)))
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return controlplane.PreparedContentResult{}, retryable("docker_unavailable", fmt.Errorf("create build staging: %w", err))
	}
	defer os.RemoveAll(stage)
	if err := savePhase(stage, "cache_querying", buildKey); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	lookup, err := m.requestGrants(ctx, command, buildKey, "manifest-read", []string{manifestKey}, nil, "")
	if err != nil {
		return controlplane.PreparedContentResult{}, grantUnavailable(err)
	}
	if lookup.CacheStatus == "hit" ||
		incompleteCacheStatus(lookup.CacheStatus) && len(lookup.Grants) == 1 {
		result, err := m.restoreHit(ctx, command, stage, buildKey, contentKey, manifestKey, lookup)
		if err != nil {
			return controlplane.PreparedContentResult{}, err
		}
		_ = savePhase(stage, "prepared", buildKey)
		return result, nil
	}
	if !incompleteCacheStatus(lookup.CacheStatus) || len(lookup.Grants) != 0 {
		return controlplane.PreparedContentResult{}, terminal("invalid_prepare_command", errors.New("invalid manifest lookup response"))
	}

	if err := savePhase(stage, "source_downloading", buildKey); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	sourcePath := filepath.Join(stage, "source.pack")
	sourceGrant, err := m.singleGrant(ctx, command, buildKey, "source-read", command.Prepare.Source.Key, nil, "")
	if err != nil {
		return controlplane.PreparedContentResult{}, grantUnavailable(err)
	}
	sourceFile, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o400)
	if err != nil {
		return controlplane.PreparedContentResult{}, retryable("source_download_failed", err)
	}
	sourceResult, downloadErr := m.transfer.Download(ctx, sourceGrant, command.Prepare.Source.Key, command.Prepare.Source.SizeBytes, sourceFile)
	closeErr := sourceFile.Close()
	if downloadErr != nil || closeErr != nil {
		if errors.Is(downloadErr, workspace.ErrInvalidGrant) {
			return controlplane.PreparedContentResult{}, terminal("invalid_prepare_command", downloadErr)
		}
		return controlplane.PreparedContentResult{}, retryable("source_download_failed", firstError(downloadErr, closeErr))
	}
	if sourceResult.Size != command.Prepare.Source.SizeBytes ||
		sourceResult.SHA256 != command.Prepare.BuildSpec.Source.SHA256 {
		return controlplane.PreparedContentResult{}, terminal("source_hash_mismatch", errors.New("source bytes do not match build specification"))
	}
	if err := validateSourceMagic(sourcePath, command.Prepare.BuildSpec.Source.Format); err != nil {
		return controlplane.PreparedContentResult{}, terminal("source_pack_invalid", err)
	}

	if err := savePhase(stage, "dependencies_downloading", buildKey); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	dependenciesPath := filepath.Join(stage, "dependencies")
	if err := os.RemoveAll(dependenciesPath); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	if err := os.MkdirAll(dependenciesPath, 0o700); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	index := dependencyIndex{SchemaVersion: 1, Dependencies: make([]mountedDependency, 0, len(command.Prepare.BuildSpec.Dependencies))}
	for i, dependency := range command.Prepare.BuildSpec.Dependencies {
		name := fmt.Sprintf("%03d-%s", i, dependency.SHA256)
		path := filepath.Join(dependenciesPath, name)
		if err := m.downloadDependency(ctx, command.Prepare.ResolvedDependencies[i].URL, dependency, path); err != nil {
			return controlplane.PreparedContentResult{}, err
		}
		index.Dependencies = append(index.Dependencies, mountedDependency{
			ID: dependency.ID, Path: name, SHA256: dependency.SHA256, SizeBytes: dependency.SizeBytes,
		})
	}
	if err := os.Chmod(dependenciesPath, 0o500); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	specBytes, err := CanonicalJSON(command.Prepare.BuildSpec)
	if err != nil {
		return controlplane.PreparedContentResult{}, terminal("invalid_build_spec", err)
	}
	indexBytes, err := CanonicalJSON(index)
	if err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "build-spec.json"), append(specBytes, '\n'), 0o400); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "dependencies.json"), append(indexBytes, '\n'), 0o400); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	outputPath := filepath.Join(stage, "output")
	if err := os.RemoveAll(outputPath); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	if err := os.MkdirAll(outputPath, 0o700); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	_ = savePhase(stage, "builder_running", buildKey)
	if err := m.runner.Run(ctx, command, stage); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	_ = savePhase(stage, "output_validating", buildKey)
	content, err := m.validateBuilderOutput(stage, command.Prepare.BuildSpec, buildKey, contentKey)
	if err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	manifest := controlplane.PreparedContentManifest{
		SchemaVersion: 1, BuildKey: buildKey, BuildSpec: command.Prepare.BuildSpec, Content: content,
	}
	manifestBytes, err := CanonicalJSON(manifest)
	if err != nil {
		return controlplane.PreparedContentResult{}, terminal("builder_output_invalid", err)
	}
	if int64(len(manifestBytes)) > maxManifestBytes {
		return controlplane.PreparedContentResult{}, terminal("builder_output_invalid", errors.New("prepared content manifest exceeds configured limit"))
	}
	manifestSHA := digestBytes(manifestBytes)
	contentPath := filepath.Join(outputPath, "content.tar.zst")

	_ = savePhase(stage, "content_publishing", buildKey)
	contentGrant, err := m.singleGrant(ctx, command, buildKey, "content-write", contentKey, &manifest, manifestSHA)
	if err != nil {
		return controlplane.PreparedContentResult{}, grantUnavailable(err)
	}
	if err := m.uploadOrReconcile(ctx, command, buildKey, "content-read", contentGrant, contentKey, contentPath, content.CompressedSize, content.SHA256); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	_ = savePhase(stage, "manifest_publishing", buildKey)
	manifestPath := filepath.Join(stage, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	manifestGrant, err := m.singleGrant(ctx, command, buildKey, "manifest-write", manifestKey, &manifest, manifestSHA)
	if err != nil {
		return controlplane.PreparedContentResult{}, grantUnavailable(err)
	}
	if err := m.uploadOrReconcile(ctx, command, buildKey, "manifest-read", manifestGrant, manifestKey, manifestPath, int64(len(manifestBytes)), manifestSHA); err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	_ = savePhase(stage, "prepared", buildKey)
	return preparedResult("built", buildKey, manifestKey, manifestSHA, manifest), nil
}

func (m *Manager) restoreHit(ctx context.Context, command controlplane.Command, stage, buildKey, contentKey, manifestKey string, lookup controlplane.BuildGrantResponse) (controlplane.PreparedContentResult, error) {
	grant, err := exactlyOneGrant(lookup, manifestKey, http.MethodGet)
	if err != nil {
		return controlplane.PreparedContentResult{}, terminal("invalid_prepare_command", err)
	}
	var manifestBuffer bytes.Buffer
	download, err := m.transfer.Download(ctx, grant, manifestKey, maxManifestBytes, &manifestBuffer)
	if err != nil {
		if errors.Is(err, workspace.ErrInvalidGrant) {
			return controlplane.PreparedContentResult{}, terminal("invalid_prepare_command", err)
		}
		return controlplane.PreparedContentResult{}, retryable("grant_unavailable", err)
	}
	var manifest controlplane.PreparedContentManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBuffer.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || !decodeEnded(decoder) || manifest.SchemaVersion != 1 ||
		manifest.BuildKey != buildKey || manifest.Content.Key != contentKey ||
		!reflect.DeepEqual(manifest.BuildSpec, command.Prepare.BuildSpec) {
		return controlplane.PreparedContentResult{}, terminal("cache_manifest_invalid", errors.New("cached manifest does not match requested build"))
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil || !bytes.Equal(canonical, manifestBuffer.Bytes()) {
		return controlplane.PreparedContentResult{}, terminal("cache_manifest_invalid", errors.New("cached manifest is not canonical"))
	}
	if download.SHA256 != digestBytes(canonical) {
		return controlplane.PreparedContentResult{}, terminal("cache_manifest_invalid", errors.New("cached manifest hash is invalid"))
	}
	if err := validateContentDescriptor(manifest.Content); err != nil {
		return controlplane.PreparedContentResult{}, terminal("cache_manifest_invalid", err)
	}
	contentGrant, err := m.singleGrant(ctx, command, buildKey, "content-read", contentKey, nil, "")
	if err != nil {
		return controlplane.PreparedContentResult{}, grantUnavailable(err)
	}
	contentPath := filepath.Join(stage, "cached-content.tar.zst")
	file, err := os.OpenFile(contentPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return controlplane.PreparedContentResult{}, err
	}
	result, downloadErr := m.transfer.Download(ctx, contentGrant, contentKey, manifest.Content.CompressedSize, file)
	closeErr := file.Close()
	if downloadErr != nil || closeErr != nil {
		if errors.Is(downloadErr, workspace.ErrInvalidGrant) {
			return controlplane.PreparedContentResult{}, terminal("invalid_prepare_command", downloadErr)
		}
		return controlplane.PreparedContentResult{}, retryable("grant_unavailable", firstError(downloadErr, closeErr))
	}
	if result.Size != manifest.Content.CompressedSize || result.SHA256 != manifest.Content.SHA256 {
		return controlplane.PreparedContentResult{}, terminal("cache_content_invalid", errors.New("cached content does not match manifest"))
	}
	if err := validateContentArchive(contentPath, manifest.Content, manifest.BuildSpec); err != nil {
		return controlplane.PreparedContentResult{}, terminal("cache_content_invalid", err)
	}
	return preparedResult("hit", buildKey, manifestKey, download.SHA256, manifest), nil
}

func preparedResult(cache, buildKey, manifestKey, manifestSHA string, manifest controlplane.PreparedContentManifest) controlplane.PreparedContentResult {
	return controlplane.PreparedContentResult{
		Cache: cache, BuildKey: buildKey, ManifestKey: manifestKey, ManifestSHA256: manifestSHA,
		Manifest: manifest, Content: manifest.Content,
	}
}

func incompleteCacheStatus(status string) bool {
	switch status {
	case "miss", "publishing_content", "publishing_manifest":
		return true
	default:
		return false
	}
}

func (m *Manager) requestGrants(ctx context.Context, command controlplane.Command, buildKey, stage string, keys []string, manifest *controlplane.PreparedContentManifest, manifestSHA string) (controlplane.BuildGrantResponse, error) {
	command.Lease = controlplane.CurrentCommandLease(ctx, command.Lease)
	return m.grants.BuildGrants(ctx, command, controlplane.BuildGrantRequest{
		BuildKey: buildKey, Stage: stage, Keys: keys, Manifest: manifest, ManifestSHA256: manifestSHA,
	})
}

func (m *Manager) singleGrant(ctx context.Context, command controlplane.Command, buildKey, stage, key string, manifest *controlplane.PreparedContentManifest, manifestSHA string) (controlplane.WorkspaceGrant, error) {
	response, err := m.requestGrants(ctx, command, buildKey, stage, []string{key}, manifest, manifestSHA)
	if err != nil {
		return controlplane.WorkspaceGrant{}, err
	}
	grant, err := exactlyOneGrant(response, key, map[bool]string{true: http.MethodPut, false: http.MethodGet}[strings.HasSuffix(stage, "-write")])
	if err != nil {
		return controlplane.WorkspaceGrant{}, terminal("invalid_prepare_command", err)
	}
	if response.CacheStatus != "" {
		return controlplane.WorkspaceGrant{}, terminal("invalid_prepare_command", errors.New("unexpected cache status on build grant"))
	}
	return grant, nil
}

func exactlyOneGrant(response controlplane.BuildGrantResponse, key, method string) (controlplane.WorkspaceGrant, error) {
	if len(response.Grants) != 1 || response.Grants[0].Key != key || response.Grants[0].Method != method {
		return controlplane.WorkspaceGrant{}, errors.New("build grant response does not match exact operation")
	}
	return response.Grants[0], nil
}

func (m *Manager) downloadDependency(ctx context.Context, rawURL string, dependency controlplane.BuildDependency, destination string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" ||
		parsed.Fragment != "" || !approvedDependencyURL(dependency, parsed.Hostname()) {
		return terminal("dependency_rejected", errors.New("dependency URL is not approved"))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return terminal("dependency_rejected", errors.New("dependency request is invalid"))
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := m.client.Do(request)
	if err != nil {
		return retryable("dependency_download_failed", errors.New("approved dependency origin is unavailable"))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != dependency.SizeBytes ||
		response.Request == nil || response.Request.URL.String() != parsed.String() {
		if response.StatusCode >= 500 {
			return retryable("dependency_download_failed", errors.New("approved dependency origin failed"))
		}
		return terminal("dependency_rejected", errors.New("dependency response does not match descriptor"))
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, dependency.SizeBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return retryable("dependency_download_failed", firstError(copyErr, closeErr))
	}
	if written != dependency.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != dependency.SHA256 {
		return terminal("dependency_hash_mismatch", errors.New("dependency bytes do not match build specification"))
	}
	return nil
}

func approvedDependencyURL(dependency controlplane.BuildDependency, host string) bool {
	host = strings.ToLower(host)
	if dependency.Kind == "mod" {
		switch {
		case strings.HasPrefix(dependency.ID, "modrinth:"):
			return host == "cdn.modrinth.com"
		case strings.HasPrefix(dependency.ID, "curseforge:"):
			return host == "forgecdn.net" || strings.HasSuffix(host, ".forgecdn.net")
		default:
			return false
		}
	}
	switch host {
	case "maven.fabricmc.net", "maven.minecraftforge.net", "maven.neoforged.net",
		"maven.quiltmc.org", "libraries.minecraft.net", "piston-data.mojang.com":
		return true
	default:
		return false
	}
}

func (m *Manager) validateBuilderOutput(stage string, spec controlplane.BuildSpecV1, buildKey, contentKey string) (controlplane.WorkspaceBlob, error) {
	output := filepath.Join(stage, "output")
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 2 {
		return controlplane.WorkspaceBlob{}, terminal("builder_output_invalid", errors.New("builder output must contain exactly result.json and content.tar.zst"))
	}
	names := []string{entries[0].Name(), entries[1].Name()}
	sort.Strings(names)
	if names[0] != "content.tar.zst" || names[1] != "result.json" {
		return controlplane.WorkspaceBlob{}, terminal("builder_output_invalid", errors.New("builder output contains unexpected files"))
	}
	resultPath := filepath.Join(output, "result.json")
	resultInfo, statErr := os.Lstat(resultPath)
	if statErr != nil || !resultInfo.Mode().IsRegular() || resultInfo.Mode()&os.ModeSymlink != 0 ||
		resultInfo.Size() < 1 || resultInfo.Size() > maxManifestBytes {
		return controlplane.WorkspaceBlob{}, terminal("builder_output_invalid", errors.New("builder result is unavailable or oversized"))
	}
	resultBytes, err := os.ReadFile(resultPath)
	if err != nil {
		return controlplane.WorkspaceBlob{}, terminal("builder_output_invalid", errors.New("builder result is unavailable"))
	}
	var result builderResult
	decoder := json.NewDecoder(bytes.NewReader(resultBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || !decodeEnded(decoder) || result.SchemaVersion != 1 ||
		result.BuildKey != buildKey || result.Content.Key != "" {
		return controlplane.WorkspaceBlob{}, terminal("builder_output_invalid", errors.New("builder result contract is invalid"))
	}
	content := result.Content
	content.Key = contentKey
	contentPath := filepath.Join(output, "content.tar.zst")
	info, err := os.Lstat(contentPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > MaxContentBytes ||
		info.Size() != content.CompressedSize {
		return controlplane.WorkspaceBlob{}, terminal("builder_output_invalid", errors.New("builder content file is invalid"))
	}
	file, err := os.Open(contentPath)
	if err != nil {
		return controlplane.WorkspaceBlob{}, err
	}
	hash := sha256.New()
	_, hashErr := io.Copy(hash, file)
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil || hex.EncodeToString(hash.Sum(nil)) != content.SHA256 {
		return controlplane.WorkspaceBlob{}, terminal("builder_output_invalid", errors.New("builder content hash is invalid"))
	}
	if err := validateContentArchive(contentPath, content, spec); err != nil {
		return controlplane.WorkspaceBlob{}, terminal("builder_output_invalid", err)
	}
	return content, nil
}

func validateContentArchive(path string, content controlplane.WorkspaceBlob, spec controlplane.BuildSpecV1) error {
	if err := validateContentDescriptor(content); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder, err := zstd.NewReader(file, zstd.WithDecoderMaxMemory(uint64(MaxContentBytes)))
	if err != nil {
		return err
	}
	defer decoder.Close()
	extraction, err := os.MkdirTemp("", "xmcl-builder-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(extraction)
	expected := make(map[string]struct{}, len(content.Paths))
	for _, value := range content.Paths {
		expected[value] = struct{}{}
	}
	reader := tar.NewReader(decoder)
	var total int64
	seen := map[string]struct{}{}
	previousPath := ""
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > MaxContentBytes-total ||
			(header.Mode&0o777 != 0o644 && header.Mode&0o777 != 0o755) {
			return errors.New("prepared content member is invalid")
		}
		if previousPath != "" && header.Name <= previousPath {
			return errors.New("prepared content members are not deterministically sorted")
		}
		previousPath = header.Name
		if _, ok := expected[header.Name]; !ok {
			return errors.New("prepared content has undeclared member")
		}
		if _, duplicate := seen[header.Name]; duplicate {
			return errors.New("prepared content has duplicate member")
		}
		target := filepath.Join(extraction, filepath.FromSlash(header.Name))
		relative, err := filepath.Rel(extraction, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("prepared content member escapes validation root")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode&0o777))
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(reader, header.Size+1))
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil || written != header.Size {
			return errors.New("prepared content member size is invalid")
		}
		total += written
		seen[header.Name] = struct{}{}
	}
	if total != content.LogicalSize || len(seen) != len(expected) {
		return errors.New("prepared content does not match descriptor")
	}
	descriptor, err := runtimecontract.ValidateWorkspace(extraction, content.SHA256)
	if err != nil {
		return err
	}
	if descriptor.MinecraftVersion != spec.Compatibility.MinecraftVersion ||
		descriptor.Loader.Kind != spec.Compatibility.Loader.Kind ||
		descriptor.Loader.Version != spec.Compatibility.Loader.Version ||
		descriptor.Java.Component != spec.Compatibility.Java.Component ||
		descriptor.Java.Major != spec.Compatibility.Java.Major ||
		descriptor.RuntimeCatalogRevision != spec.Runtime.RuntimeCatalogRevision {
		return errors.New("prepared runtime descriptor does not match build specification")
	}
	return nil
}

func validateContentDescriptor(content controlplane.WorkspaceBlob) error {
	if !shaPattern.MatchString(content.SHA256) || content.CompressedSize < 1 ||
		content.CompressedSize > MaxContentBytes || content.LogicalSize < 1 ||
		len(content.Paths) < 2 || !sort.StringsAreSorted(content.Paths) {
		return errors.New("prepared content descriptor is invalid")
	}
	collisions := make(map[string]struct{}, len(content.Paths))
	for _, value := range content.Paths {
		if !safePreparedPath(value) {
			return errors.New("prepared content path is unsafe")
		}
		collision := collisionKey(value)
		if _, duplicate := collisions[collision]; duplicate {
			return errors.New("prepared content paths collide")
		}
		collisions[collision] = struct{}{}
	}
	return nil
}

func (m *Manager) uploadOrReconcile(ctx context.Context, command controlplane.Command, buildKey, readStage string, grant controlplane.WorkspaceGrant, key, path string, size int64, expectedSHA string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	_, uploadErr := m.transfer.Upload(ctx, grant, key, file, size, expectedSHA)
	closeErr := file.Close()
	if uploadErr == nil && closeErr == nil {
		return nil
	}
	if errors.Is(uploadErr, workspace.ErrInvalidGrant) {
		return terminal("invalid_prepare_command", uploadErr)
	}
	readGrant, grantErr := m.singleGrant(ctx, command, buildKey, readStage, key, nil, "")
	if grantErr != nil {
		var failure *Failure
		if errors.As(grantErr, &failure) {
			return grantErr
		}
		return retryable(mapPublishCode(readStage), firstError(uploadErr, grantErr))
	}
	result, reconcileErr := m.transfer.Download(ctx, readGrant, key, size, io.Discard)
	if reconcileErr != nil {
		return retryable(mapPublishCode(readStage), reconcileErr)
	}
	if result.Size != size || result.SHA256 != expectedSHA {
		return terminal("immutable_cache_conflict", errors.New("existing immutable cache object differs"))
	}
	return nil
}

func mapPublishCode(readStage string) string {
	if readStage == "manifest-read" {
		return "manifest_publish_uncertain"
	}
	return "content_publish_uncertain"
}

func validateSourceMagic(path, format string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return err
	}
	if magic != [4]byte{'P', 'K', 3, 4} {
		return errors.New("source is not a ZIP-compatible pack")
	}
	if format != "mrpack" && format != "zip" {
		return errors.New("unsupported source format")
	}
	return nil
}

func savePhase(root, phase, buildKey string) error {
	data, err := json.Marshal(struct {
		SchemaVersion int    `json:"schemaVersion"`
		BuildKey      string `json:"buildKey"`
		Phase         string `json:"phase"`
	}{SchemaVersion: 1, BuildKey: buildKey, Phase: phase})
	if err != nil {
		return err
	}
	temp := filepath.Join(root, "state.json.tmp")
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(root, "state.json"))
}

func (m *Manager) buildLock(buildKey string) *sync.Mutex {
	lock, _ := m.locks.LoadOrStore(buildKey, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *Manager) pruneStaging() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if len(entry.Name()) != 64 || !shaPattern.MatchString(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func decodeEnded(decoder *json.Decoder) bool {
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

func safePreparedPath(value string) bool {
	if !safePath(value) {
		return false
	}
	lower := strings.ToLower(value)
	if lower == ".xmcl/runtime.json" || lower == ".xmcl/launch.sh" {
		return true
	}
	return !strings.HasPrefix(lower, ".xmcl/") &&
		lower != "world" && !strings.HasPrefix(lower, "world/") &&
		lower != "eula.txt" &&
		!strings.HasPrefix(lower, "jre/") && !strings.HasPrefix(lower, "java/")
}

func digestBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func firstError(left, right error) error {
	if left != nil {
		return left
	}
	return right
}

func grantUnavailable(err error) error {
	var failure *Failure
	if errors.As(err, &failure) {
		return err
	}
	return retryable("grant_unavailable", err)
}
