package workspace

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type testQuota struct{}

func (testQuota) Validate(context.Context) error             { return nil }
func (testQuota) Apply(context.Context, string, int64) error { return nil }
func (testQuota) Prepare(context.Context, string) error      { return nil }
func (testQuota) Seal(context.Context, string) error         { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type memoryTransfer struct {
	objects  map[string][]byte
	uploaded []string
	failKey  string
	failures int
}

func (t *memoryTransfer) Download(_ context.Context, grant controlplane.WorkspaceGrant, key string, limit int64, destination io.Writer) (TransferResult, error) {
	if grant.Key != key {
		return TransferResult{}, errors.New("unexpected key")
	}
	data, ok := t.objects[key]
	if !ok {
		return TransferResult{}, errors.New("missing object")
	}
	if int64(len(data)) > limit {
		return TransferResult{}, errors.New("limit exceeded")
	}
	if _, err := destination.Write(data); err != nil {
		return TransferResult{}, err
	}
	return TransferResult{Size: int64(len(data)), SHA256: digest(data)}, nil
}

func (t *memoryTransfer) Upload(_ context.Context, grant controlplane.WorkspaceGrant, key string, source io.Reader, size int64, expectedSHA string) (TransferResult, error) {
	if grant.Key != key || grant.Method != "PUT" {
		return TransferResult{}, errors.New("unexpected upload grant")
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return TransferResult{}, err
	}
	if int64(len(data)) != size || digest(data) != expectedSHA {
		return TransferResult{}, errors.New("upload descriptor mismatch")
	}
	if key == t.failKey && t.failures > 0 {
		t.failures--
		return TransferResult{}, errors.New("injected transfer interruption")
	}
	t.objects[key] = data
	t.uploaded = append(t.uploaded, key)
	return TransferResult{Size: size, SHA256: expectedSHA}, nil
}

type memoryGrants struct {
	syncCalls       int
	published       []controlplane.WorkspaceManifest
	reuseContent    bool
	reuseUnchanged  bool
	lastManifest    controlplane.WorkspaceManifest
	lastManifestSHA string
	manifestSHAs    []string
	restoreObjects  map[string]controlplane.WorkspaceGrant
}

func (g *memoryGrants) RestoreWorkspaceGrants(_ context.Context, _ controlplane.Command, _ string, keys []string) (controlplane.WorkspaceGrantResponse, error) {
	var grants []controlplane.WorkspaceGrant
	for _, key := range keys {
		grant, ok := g.restoreObjects[key]
		if !ok {
			return controlplane.WorkspaceGrantResponse{}, errors.New("restore not configured")
		}
		grants = append(grants, grant)
	}
	return controlplane.WorkspaceGrantResponse{ContractVersion: controlplane.WorkspaceGrantContractVersion, Grants: grants}, nil
}

func (g *memoryGrants) SyncWorkspaceGrants(_ context.Context, _ controlplane.Command, manifest controlplane.WorkspaceManifest, manifestSHA string, requested []string) (controlplane.WorkspaceGrantResponse, error) {
	g.syncCalls++
	previous := g.lastManifest
	g.lastManifest, g.lastManifestSHA = manifest, manifestSHA
	g.manifestSHAs = append(g.manifestSHAs, manifestSHA)
	var grants []controlplane.WorkspaceGrant
	requestedKeys := make(map[string]struct{}, len(requested))
	for _, key := range requested {
		requestedKeys[key] = struct{}{}
	}
	for _, descriptor := range manifestDescriptors(manifest) {
		if _, requested := requestedKeys[descriptor.Key]; !requested {
			continue
		}
		reused := false
		if g.reuseContent && g.syncCalls > 1 && manifest.Content != nil && descriptor.Key == manifest.Content.Key {
			reused = true
		}
		if !reused && g.reuseUnchanged && g.syncCalls > 1 {
			for _, old := range manifestDescriptors(previous) {
				if sameArchive(old, descriptor) {
					reused = true
					break
				}
			}
		}
		if !reused {
			grants = append(grants, putGrant(descriptor.Key))
		}
	}
	return controlplane.WorkspaceGrantResponse{ContractVersion: controlplane.WorkspaceGrantContractVersion, Grants: grants}, nil
}

func (g *memoryGrants) PublishWorkspaceGrant(_ context.Context, command controlplane.Command, manifest controlplane.WorkspaceManifest, manifestSHA string) (controlplane.WorkspaceGrantResponse, error) {
	g.published = append(g.published, manifest)
	return controlplane.WorkspaceGrantResponse{
		ContractVersion: controlplane.WorkspaceGrantContractVersion,
		Grants:          []controlplane.WorkspaceGrant{putGrant(manifestKey(command.Workspace.ObjectPrefix, manifest.Revision))},
	}, nil
}

func putGrant(key string) controlplane.WorkspaceGrant {
	return controlplane.WorkspaceGrant{
		Key: key, Method: "PUT", URL: "https://xmclstaging.blob.core.windows.net/bucket/" + key,
		ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		Headers: map[string]string{
			"if-none-match": "*", "x-ms-blob-type": "BlockBlob",
		},
	}
}

func getGrant(key string) controlplane.WorkspaceGrant {
	return controlplane.WorkspaceGrant{
		Key: key, Method: "GET", URL: "https://xmclstaging.blob.core.windows.net/bucket/" + key,
		ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
	}
}

func stopCommand() controlplane.Command {
	return controlplane.Command{
		CommandID: "command_1234567890", Kind: controlplane.StopAndSync, NodeID: "node_1",
		ServiceID: "service_1", AccountID: "account_1", AssignmentID: "assignment_1",
		Workspace: controlplane.Workspace{ObjectPrefix: "shared-hosting/account_1/service_1/", Revision: 2},
		Resources: controlplane.Resources{MemoryMiB: 1024, SharedCPU: 1, BurstCPU: 1, WorkspaceGiB: 1},
		Lease:     controlplane.CommandLease{Token: "12345678-1234-1234-1234-123456789abc", Generation: 1},
	}
}

func TestSyncClassifiesV2LayersAndPublishesManifestLast(t *testing.T) {
	root := t.TempDir()
	grants := &memoryGrants{}
	transfer := &memoryTransfer{objects: make(map[string][]byte)}
	manager := New(root, grants, transfer, testQuota{})
	command := stopCommand()
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}

	for name, content := range map[string]string{
		"world/region/r.0.0.mca": "world",
		"config/settings.toml":   "config",
		"mods/mod.jar":           "content",
		"server.properties":      "bootstrap",
	} {
		file := filepath.Join(path, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	result, err := manager.Sync(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	manifest := grants.lastManifest
	if manifest.SchemaVersion != 2 || manifest.Content == nil || manifest.Config == nil || len(manifest.World) != 1 {
		t.Fatalf("unexpected v2 manifest: %#v", manifest)
	}
	if got, want := manifest.World[0].Paths, []string{"world/region/r.0.0.mca"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("world mapping = %v, want %v", got, want)
	}
	if len(transfer.uploaded) != 4 || transfer.uploaded[len(transfer.uploaded)-1] != manifestKey(command.Workspace.ObjectPrefix, result.Revision) {
		t.Fatalf("manifest must be uploaded last, got %v", transfer.uploaded)
	}
}

func TestSyncPublishesAnEmptyWorkspaceWhenStartNeverReachedTheNode(t *testing.T) {
	root := t.TempDir()
	grants := &memoryGrants{}
	transfer := &memoryTransfer{objects: make(map[string][]byte)}
	manager := New(root, grants, transfer, testQuota{})
	command := stopCommand()

	result, err := manager.Sync(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != command.Workspace.Revision+1 || result.SizeBytes != 0 {
		t.Fatalf("empty sync result = %#v", result)
	}
	if len(grants.published) != 1 || grants.published[0].LogicalSize != 0 {
		t.Fatalf("published empty manifest = %#v", grants.published)
	}
}

func TestRestoreInitialWorldUsesOnlyTheSelectedSeed(t *testing.T) {
	var compressed bytes.Buffer
	writer := zip.NewWriter(&compressed)
	for name, content := range map[string]string{
		"world/level.dat": "selected-world",
		"world.json":      `{"schemaVersion":1}`,
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	command := controlplane.Command{
		CommandID: "initial_world_command", Kind: controlplane.RestoreAndStart,
		NodeID: "node_1", ServiceID: "service_1", AccountID: "account_1", AssignmentID: "assignment_1",
		Workspace: controlplane.Workspace{ObjectPrefix: "shared-hosting/account_1/service_1/", Revision: 0},
		Resources: controlplane.Resources{WorkspaceGiB: 1},
		InitialWorld: &controlplane.InitialWorld{
			SeedID: "seed_1", SHA256: digest(compressed.Bytes()), SizeBytes: int64(compressed.Len()), WorldName: "Selected",
		},
	}
	key, err := initialWorldKey(command)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(t.TempDir(), &memoryGrants{
		restoreObjects: map[string]controlplane.WorkspaceGrant{key: getGrant(key)},
	}, &memoryTransfer{objects: map[string][]byte{key: compressed.Bytes()}}, testQuota{})
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreInitialWorld(context.Background(), command, path); err != nil {
		t.Fatal(err)
	}
	world, err := os.ReadFile(filepath.Join(path, "world", "level.dat"))
	if err != nil || string(world) != "selected-world" {
		t.Fatalf("restored selected world = %q, %v", world, err)
	}
	if _, err := os.Stat(filepath.Join(path, "world.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed manifest was written into workspace: %v", err)
	}
}

func TestSyncReusesUnchangedContentDigestWithoutUpload(t *testing.T) {
	root := t.TempDir()
	grants := &memoryGrants{reuseContent: true}
	transfer := &memoryTransfer{objects: make(map[string][]byte)}
	manager := New(root, grants, transfer, testQuota{})
	command := stopCommand()
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "mods"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "mods", "stable.jar"), []byte("stable"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Sync(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	firstContent := grants.lastManifest.Content
	transfer.uploaded = nil
	command.Workspace.Revision++
	if _, err := manager.Sync(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if grants.lastManifest.Content == nil || firstContent.SHA256 != grants.lastManifest.Content.SHA256 {
		t.Fatal("unchanged content did not preserve its digest")
	}
	for _, key := range transfer.uploaded {
		if key == grants.lastManifest.Content.Key {
			t.Fatalf("unchanged content was uploaded again: %v", transfer.uploaded)
		}
	}
}

func TestSyncUploadsOnlyChangedWorldShard(t *testing.T) {
	root := t.TempDir()
	grants := &memoryGrants{reuseUnchanged: true}
	transfer := &memoryTransfer{objects: make(map[string][]byte)}
	manager := New(root, grants, transfer, testQuota{})
	command := stopCommand()
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"world/region/r.0.0.mca":        "overworld",
		"world_nether/region/r.0.0.mca": "nether",
	} {
		file := filepath.Join(path, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Sync(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	first := grants.lastManifest
	transfer.uploaded = nil
	if err := os.WriteFile(filepath.Join(path, "world", "region", "r.0.0.mca"), []byte("changed-overworld"), 0o640); err != nil {
		t.Fatal(err)
	}
	command.Workspace.Revision++
	if _, err := manager.Sync(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	var changed int
	for _, key := range transfer.uploaded {
		if strings.Contains(key, "/world/") {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("uploaded world shards = %v, want one replacement", transfer.uploaded)
	}
	if len(first.World) != len(grants.lastManifest.World) {
		t.Fatal("world shard count changed unexpectedly")
	}
}

func TestSyncRetryUsesTheSameRevisionAndPublishesOnlyOnce(t *testing.T) {
	root := t.TempDir()
	grants := &memoryGrants{}
	command := stopCommand()
	manifestKey := manifestKey(command.Workspace.ObjectPrefix, command.Workspace.Revision+1)
	transfer := &memoryTransfer{
		objects: make(map[string][]byte), failKey: manifestKey, failures: transferAttempts,
	}
	manager := New(root, grants, transfer, testQuota{})
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "mods"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "mods", "stable.jar"), []byte("stable"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Sync(context.Background(), command); err == nil {
		t.Fatal("interrupted manifest publication unexpectedly succeeded")
	}
	// A fresh manager simulates an agent restart after the control plane
	// accepted the immutable draft but before its manifest publish completed.
	manager = New(root, grants, transfer, testQuota{})
	result, err := manager.Sync(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != command.Workspace.Revision+1 || len(grants.manifestSHAs) != 2 ||
		grants.manifestSHAs[0] != grants.manifestSHAs[1] {
		t.Fatalf("retry produced a different manifest revision: result=%#v hashes=%v", result, grants.manifestSHAs)
	}
	published := 0
	for _, key := range transfer.uploaded {
		if key == manifestKey {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("manifest completed %d times, want once", published)
	}
}

func TestSyncRejectsSymlinkAndOversizedPaths(t *testing.T) {
	root := t.TempDir()
	manager := New(root, &memoryGrants{}, &memoryTransfer{objects: make(map[string][]byte)}, testQuota{})
	command := stopCommand()
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(path, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := manager.Sync(context.Background(), command); err == nil {
		t.Fatal("symlink workspace input was accepted")
	}
}

func TestExtractArchiveRejectsTraversalDuplicateAndBomb(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe.tar.zst")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := zstdEncoder(file)
	if err != nil {
		t.Fatal(err)
	}
	writer := tarWriter(encoder)
	for _, name := range []string{"world/ok", "world/ok"} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o640, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	size, sum, err := hashFile(path, maxBlobBytes)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := controlplane.WorkspaceBlob{Key: "x", SHA256: sum, CompressedSize: size, LogicalSize: 2, Paths: []string{"world/ok"}}
	if _, err := extractArchive(path, t.TempDir(), descriptor, map[string]struct{}{}, 10); err == nil {
		t.Fatal("duplicate archive member was accepted")
	}
}

func TestExtractArchiveRejectsTraversalSymlinkAndOversizedMembers(t *testing.T) {
	tests := []struct {
		name       string
		header     tar.Header
		descriptor controlplane.WorkspaceBlob
		limit      int64
	}{
		{
			name:       "traversal",
			header:     tar.Header{Name: "../escape", Mode: 0o640, Size: 1, Typeflag: tar.TypeReg},
			descriptor: controlplane.WorkspaceBlob{Paths: []string{"../escape"}, LogicalSize: 1},
			limit:      10,
		},
		{
			name:       "symlink",
			header:     tar.Header{Name: "world/link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "../../escape"},
			descriptor: controlplane.WorkspaceBlob{Paths: []string{"world/link"}},
			limit:      10,
		},
		{
			name:       "decompression_bomb",
			header:     tar.Header{Name: "world/large", Mode: 0o640, Size: 128, Typeflag: tar.TypeReg},
			descriptor: controlplane.WorkspaceBlob{Paths: []string{"world/large"}, LogicalSize: 128},
			limit:      10,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unsafe.tar.zst")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			encoder, err := zstdEncoder(file)
			if err != nil {
				t.Fatal(err)
			}
			writer := tarWriter(encoder)
			if err := writer.WriteHeader(&test.header); err != nil {
				t.Fatal(err)
			}
			if test.header.Size > 0 {
				if _, err := writer.Write([]byte(strings.Repeat("x", int(test.header.Size)))); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := encoder.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			size, sum, err := hashFile(path, maxBlobBytes)
			if err != nil {
				t.Fatal(err)
			}
			test.descriptor.Key = "shared-hosting/a/s/revisions/1/world/test.tar.zst"
			test.descriptor.SHA256 = sum
			test.descriptor.CompressedSize = size
			if _, err := extractArchive(path, t.TempDir(), test.descriptor, map[string]struct{}{}, test.limit); err == nil {
				t.Fatal("unsafe archive member was accepted")
			}
		})
	}
}

func TestValidateManifestRejectsForeignKeysAndLayerMappings(t *testing.T) {
	command := stopCommand()
	command.Kind = controlplane.RestoreAndStart
	manifest := controlplane.WorkspaceManifest{
		SchemaVersion: 2, ServiceID: command.ServiceID, AssignmentID: "previous_assignment",
		Revision: command.Workspace.Revision, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Content: &controlplane.WorkspaceBlob{
			Key:    "shared-hosting/account_1/service_1/content/" + strings.Repeat("a", 64) + ".tar.zst",
			SHA256: strings.Repeat("a", 64), CompressedSize: 1, LogicalSize: 0,
		},
		World: []controlplane.WorkspaceBlob{{
			Key:    "shared-hosting/account_1/service_1/revisions/2/world/world-000.tar.zst",
			SHA256: strings.Repeat("b", 64), CompressedSize: 1, LogicalSize: 1,
			Paths: []string{"world/region/r.0.0.mca"},
		}},
		LogicalSize: 1,
	}
	manifest.AggregateSHA256 = aggregateDescriptors(manifestDescriptors(manifest))
	manifest.ManifestHash = manifest.AggregateSHA256
	if err := validateManifest(manifest, command); err != nil {
		t.Fatalf("valid v2 manifest was rejected: %v", err)
	}
	foreign := manifest
	foreign.World = append([]controlplane.WorkspaceBlob(nil), manifest.World...)
	foreign.World[0].Key = "shared-hosting/other/service/revisions/2/world/world-000.tar.zst"
	foreign.AggregateSHA256 = aggregateDescriptors(manifestDescriptors(foreign))
	foreign.ManifestHash = foreign.AggregateSHA256
	if err := validateManifest(foreign, command); err == nil {
		t.Fatal("foreign workspace key was accepted")
	}
	wrongLayer := manifest
	wrongLayer.Content = &controlplane.WorkspaceBlob{
		Key: manifest.Content.Key, SHA256: manifest.Content.SHA256,
		CompressedSize: 1, LogicalSize: 1, Paths: []string{"world/region/r.0.0.mca"},
	}
	wrongLayer.World = nil
	wrongLayer.LogicalSize = 1
	wrongLayer.AggregateSHA256 = aggregateDescriptors(manifestDescriptors(wrongLayer))
	wrongLayer.ManifestHash = wrongLayer.AggregateSHA256
	if err := validateManifest(wrongLayer, command); err == nil {
		t.Fatal("world data in immutable content layer was accepted")
	}
}

func TestDirectTransferRejectsForeignURL(t *testing.T) {
	transfer, err := NewDirectTransfer("https://xmclstaging.blob.core.windows.net", "bucket", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transfer.Download(context.Background(), controlplane.WorkspaceGrant{
		Key: "shared-hosting/a/s/content/x.tar.zst", Method: "GET",
		URL:       "https://evil.example/bucket/shared-hosting/a/s/content/x.tar.zst",
		ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
	}, "shared-hosting/a/s/content/x.tar.zst", 10, io.Discard)
	if err == nil {
		t.Fatal("foreign grant URL was accepted")
	}
}

func TestDirectTransferRequiresAzureBlobOrigin(t *testing.T) {
	for _, endpoint := range []string{
		"https://objects.example.com",
		"https://xmclstaging.blob.core.windows.net/container",
		"http://xmclstaging.blob.core.windows.net",
	} {
		if _, err := NewDirectTransfer(endpoint, "bucket", nil); err == nil {
			t.Fatalf("non-Azure Blob origin %q was accepted", endpoint)
		}
	}
}

func TestDirectTransferRequiresImmutableAzurePutHeaders(t *testing.T) {
	transfer, err := NewDirectTransfer(
		"https://xmclstaging.blob.core.windows.net",
		"bucket",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	key := "shared-hosting/a/s/revisions/1/manifest.json"
	grant := controlplane.WorkspaceGrant{
		Key: key, Method: "PUT",
		URL:       "https://xmclstaging.blob.core.windows.net/bucket/" + key + "?sig=opaque",
		ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		Headers: map[string]string{
			"If-None-Match": "*", "x-ms-blob-type": "BlockBlob",
		},
	}
	if err := transfer.validateGrant(grant, key, "PUT"); err != nil {
		t.Fatal(err)
	}
	delete(grant.Headers, "x-ms-blob-type")
	if err := transfer.validateGrant(grant, key, "PUT"); err == nil {
		t.Fatal("Azure PUT without blob type was accepted")
	}
}

func TestDirectTransferRecognizesAzureImmutableConflict(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusConflict,
		Header:     http.Header{"X-Ms-Error-Code": {"BlobAlreadyExists"}},
	}
	if !isAlreadyExistsResponse(response) {
		t.Fatal("Azure immutable conflict was not recognized")
	}
}

func TestDirectTransferVerifiesAzureImmutableConflict(t *testing.T) {
	key := "shared-hosting/a/s/revisions/1/manifest.json"
	expected := []byte("expected immutable bytes")
	for name, existing := range map[string][]byte{
		"matching":   expected,
		"mismatched": []byte("different immutable bytes"),
	} {
		t.Run(name, func(t *testing.T) {
			var methods []string
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				methods = append(methods, request.Method)
				if request.Method == http.MethodPut {
					return &http.Response{
						StatusCode: http.StatusConflict,
						Header:     http.Header{"X-Ms-Error-Code": {"BlobAlreadyExists"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				}
				return &http.Response{
					StatusCode:    http.StatusOK,
					ContentLength: int64(len(existing)),
					Body:          io.NopCloser(bytes.NewReader(existing)),
				}, nil
			})}
			transfer, err := NewDirectTransfer(
				"https://xmclstaging.blob.core.windows.net",
				"bucket",
				client,
			)
			if err != nil {
				t.Fatal(err)
			}
			grant := controlplane.WorkspaceGrant{
				Key: key, Method: "PUT",
				URL:       "https://xmclstaging.blob.core.windows.net/bucket/" + key + "?sp=rcw&sig=opaque",
				ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
				Headers: map[string]string{
					"If-None-Match": "*", "x-ms-blob-type": "BlockBlob",
				},
			}
			result, uploadErr := transfer.Upload(
				t.Context(),
				grant,
				key,
				bytes.NewReader(expected),
				int64(len(expected)),
				digest(expected),
			)
			if name == "matching" {
				if uploadErr != nil || result.Size != int64(len(expected)) ||
					result.SHA256 != digest(expected) {
					t.Fatalf("matching reuse = %#v, %v", result, uploadErr)
				}
			} else if uploadErr == nil {
				t.Fatal("mismatched immutable object was reused")
			}
			if strings.Join(methods, ",") != "PUT,GET" {
				t.Fatalf("methods = %v", methods)
			}
		})
	}
}

// Keep archive construction local to the test without exposing compression
// internals from production code.
func zstdEncoder(writer io.Writer) (*zstd.Encoder, error) { return zstd.NewWriter(writer) }
func tarWriter(writer io.Writer) *tar.Writer              { return tar.NewWriter(writer) }
