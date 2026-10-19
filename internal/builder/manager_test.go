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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	runtimecontract "github.com/voxelum/xmcl-shared-node-agent/internal/runtime"
	"github.com/voxelum/xmcl-shared-node-agent/internal/workspace"
)

type memoryBuildTransfer struct {
	objects     map[string][]byte
	uploadOrder []string
}

func (t *memoryBuildTransfer) Download(_ context.Context, grant controlplane.WorkspaceGrant, key string, limit int64, destination io.Writer) (workspace.TransferResult, error) {
	if grant.Key != key || grant.Method != "GET" {
		return workspace.TransferResult{}, workspace.ErrInvalidGrant
	}
	data, ok := t.objects[key]
	if !ok || int64(len(data)) > limit {
		return workspace.TransferResult{}, fmt.Errorf("object is unavailable")
	}
	if _, err := destination.Write(data); err != nil {
		return workspace.TransferResult{}, err
	}
	return workspace.TransferResult{Size: int64(len(data)), SHA256: testDigest(data)}, nil
}

func (t *memoryBuildTransfer) Upload(_ context.Context, grant controlplane.WorkspaceGrant, key string, source io.Reader, size int64, expectedSHA string) (workspace.TransferResult, error) {
	if grant.Key != key || grant.Method != "PUT" {
		return workspace.TransferResult{}, workspace.ErrInvalidGrant
	}
	if len(grant.Headers) != 2 || grant.Headers["If-None-Match"] != "*" ||
		grant.Headers["x-ms-blob-type"] != "BlockBlob" {
		return workspace.TransferResult{}, workspace.ErrInvalidGrant
	}
	data, err := io.ReadAll(io.LimitReader(source, size+1))
	if err != nil {
		return workspace.TransferResult{}, err
	}
	if int64(len(data)) != size || testDigest(data) != expectedSHA {
		return workspace.TransferResult{}, fmt.Errorf("upload descriptor mismatch")
	}
	if _, exists := t.objects[key]; exists {
		return workspace.TransferResult{}, workspace.ErrAlreadyExists
	}
	t.objects[key] = data
	t.uploadOrder = append(t.uploadOrder, key)
	return workspace.TransferResult{Size: size, SHA256: expectedSHA}, nil
}

type buildGrantFixture struct {
	hit                       bool
	cacheStatus               string
	manifestGrantOnIncomplete bool
	failStage                 string
	stages                    []string
	leases                    []controlplane.CommandLease
	afterStage                func(string)
}

func (g *buildGrantFixture) BuildGrants(_ context.Context, command controlplane.Command, request controlplane.BuildGrantRequest) (controlplane.BuildGrantResponse, error) {
	g.stages = append(g.stages, request.Stage)
	g.leases = append(g.leases, command.Lease)
	if g.afterStage != nil {
		g.afterStage(request.Stage)
	}
	response := controlplane.BuildGrantResponse{ContractVersion: controlplane.BuildGrantContractVersion}
	if request.Stage == "manifest-read" && len(g.stages) == 1 {
		response.CacheStatus = g.cacheStatus
		if response.CacheStatus == "" && g.hit {
			response.CacheStatus = "hit"
		}
		if response.CacheStatus == "" {
			response.CacheStatus = "miss"
		}
		if response.CacheStatus != "hit" && !g.manifestGrantOnIncomplete {
			return response, nil
		}
	}
	if request.Stage == g.failStage {
		g.failStage = ""
		return controlplane.BuildGrantResponse{}, errors.New("simulated grant interruption")
	}
	method := "GET"
	if strings.HasSuffix(request.Stage, "-write") {
		method = "PUT"
		if request.Manifest == nil || request.ManifestSHA256 == "" {
			return controlplane.BuildGrantResponse{}, fmt.Errorf("write grant omitted canonical manifest")
		}
	}
	if len(request.Keys) != 1 {
		return controlplane.BuildGrantResponse{}, fmt.Errorf("unexpected build grant keys")
	}
	response.Grants = []controlplane.WorkspaceGrant{{
		Key: request.Keys[0], Method: method, URL: "https://grant.invalid",
		ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
	if method == "PUT" {
		response.Grants[0].Headers = map[string]string{
			"If-None-Match": "*", "x-ms-blob-type": "BlockBlob",
		}
	}
	return response, nil
}

type outputRunner struct {
	runs int
}

func (r *outputRunner) Run(_ context.Context, command controlplane.Command, stage string) error {
	r.runs++
	archive, paths, logicalSize := preparedArchive(command.Prepare.BuildSpec)
	output := filepath.Join(stage, "output")
	if err := os.WriteFile(filepath.Join(output, "content.tar.zst"), archive, 0o600); err != nil {
		return err
	}
	result := map[string]any{
		"schemaVersion": 1,
		"buildKey":      command.Prepare.ExpectedBuildKey,
		"content": map[string]any{
			"sha256": testDigest(archive), "compressedSize": len(archive),
			"logicalSize": logicalSize, "paths": paths,
		},
	}
	data, _ := json.Marshal(result)
	return os.WriteFile(filepath.Join(output, "result.json"), append(data, '\n'), 0o600)
}

func TestManagerPublishesContentBeforeManifestAndRestoresReadyHit(t *testing.T) {
	source := []byte{'P', 'K', 3, 4}
	spec := managerSpec(testDigest(source))
	buildKey, err := BuildKey(spec)
	if err != nil {
		t.Fatal(err)
	}

	sourceKey := "shared-hosting/account-1/service-1/build-inputs/source.zip"
	command := controlplane.Command{
		CommandID: "command-1", Kind: controlplane.ContentPrepare, NodeID: "node-1",
		ServiceID: "service-1", AssignmentID: "assignment-1", AccountID: "account-1",
		Attempt: 1, Lease: controlplane.CommandLease{Token: "lease", Generation: 1},
		Prepare: &controlplane.ContentPrepareRequest{
			SchemaVersion: 1, ExpectedBuildKey: buildKey, BuildSpec: spec,
			Source:               controlplane.BuildSourceLocation{Key: sourceKey, SizeBytes: int64(len(source))},
			ResolvedDependencies: []controlplane.ResolvedDependency{},
		},
	}
	transfer := &memoryBuildTransfer{objects: map[string][]byte{sourceKey: source}}
	grants := &buildGrantFixture{}
	runner := &outputRunner{}
	manager, err := New(t.TempDir(), identityFor(spec), grants, transfer, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Prepare(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	contentKey, manifestKey, _ := CacheKeys(buildKey)
	if result.Cache != "built" || result.Content.Key != contentKey ||
		len(transfer.uploadOrder) != 2 ||
		transfer.uploadOrder[0] != contentKey || transfer.uploadOrder[1] != manifestKey {
		t.Fatalf("publication result = %#v, order = %#v", result, transfer.uploadOrder)
	}
	if strings.Join(grants.stages, ",") != "manifest-read,source-read,content-write,manifest-write" {
		t.Fatalf("build grant stages = %#v", grants.stages)
	}

	hitGrants := &buildGrantFixture{hit: true}
	hitRunner := &outputRunner{}
	hitManager, err := New(t.TempDir(), identityFor(spec), hitGrants, transfer, hitRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	command.CommandID = "command-redelivery"
	hit, err := hitManager.Prepare(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if hit.Cache != "hit" || hitRunner.runs != 0 ||
		strings.Join(hitGrants.stages, ",") != "manifest-read,content-read" {
		t.Fatalf("cache hit = %#v, runs = %d, stages = %#v", hit, hitRunner.runs, hitGrants.stages)
	}
}

func TestManagerUsesCurrentLeaseForEveryGrantStage(t *testing.T) {
	source := []byte{'P', 'K', 3, 4}
	spec := managerSpec(testDigest(source))
	buildKey, err := BuildKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey := "shared-hosting/account-1/service-1/build-inputs/source.zip"
	initial := controlplane.CommandLease{Token: "initial-token", Generation: 1}
	renewed := controlplane.CommandLease{Token: "renewed-token", Generation: 2}
	command := controlplane.Command{
		CommandID: "command-renewed-grants", Kind: controlplane.ContentPrepare, NodeID: "node-1",
		ServiceID: "service-1", AssignmentID: "assignment-1", AccountID: "account-1",
		Attempt: 1, Lease: initial,
		Prepare: &controlplane.ContentPrepareRequest{
			SchemaVersion: 1, ExpectedBuildKey: buildKey, BuildSpec: spec,
			Source:               controlplane.BuildSourceLocation{Key: sourceKey, SizeBytes: int64(len(source))},
			ResolvedDependencies: []controlplane.ResolvedDependency{},
		},
	}
	var leaseMu sync.RWMutex
	current := initial
	grants := &buildGrantFixture{}
	grants.afterStage = func(stage string) {
		if stage == "manifest-read" {
			leaseMu.Lock()
			current = renewed
			leaseMu.Unlock()
		}
	}
	ctx := controlplane.WithCommandLeaseSnapshot(t.Context(), func() controlplane.CommandLease {
		leaseMu.RLock()
		defer leaseMu.RUnlock()
		return current
	})
	transfer := &memoryBuildTransfer{objects: map[string][]byte{sourceKey: source}}
	manager, err := New(t.TempDir(), identityFor(spec), grants, transfer, &outputRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(ctx, command); err != nil {
		t.Fatal(err)
	}
	if len(grants.leases) != 4 || grants.leases[0] != initial {
		t.Fatalf("grant leases = %#v", grants.leases)
	}
	for index, lease := range grants.leases[1:] {
		if lease != renewed {
			t.Fatalf("grant %d reused stale lease %#v", index+1, lease)
		}
	}
	if strings.Join(grants.stages, ",") != "manifest-read,source-read,content-write,manifest-write" {
		t.Fatalf("grant stages = %#v", grants.stages)
	}
}

func TestManagerRecoversPublicationRedelivery(t *testing.T) {
	source := []byte{'P', 'K', 3, 4}
	spec := managerSpec(testDigest(source))
	buildKey, err := BuildKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey := "shared-hosting/account-1/service-1/build-inputs/source.zip"
	command := controlplane.Command{
		CommandID: "command-publication", Kind: controlplane.ContentPrepare, NodeID: "node-1",
		ServiceID: "service-1", AssignmentID: "assignment-1", AccountID: "account-1",
		Attempt: 1, Lease: controlplane.CommandLease{Token: "lease", Generation: 1},
		Prepare: &controlplane.ContentPrepareRequest{
			SchemaVersion: 1, ExpectedBuildKey: buildKey, BuildSpec: spec,
			Source:               controlplane.BuildSourceLocation{Key: sourceKey, SizeBytes: int64(len(source))},
			ResolvedDependencies: []controlplane.ResolvedDependency{},
		},
	}
	contentKey, manifestKey, _ := CacheKeys(buildKey)
	transfer := &memoryBuildTransfer{objects: map[string][]byte{sourceKey: source}}
	interrupted := &buildGrantFixture{failStage: "manifest-write"}
	first, err := New(t.TempDir(), identityFor(spec), interrupted, transfer, &outputRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Prepare(t.Context(), command); err == nil {
		t.Fatal("expected interruption after immutable content publication")
	}
	if _, ok := transfer.objects[contentKey]; !ok {
		t.Fatal("content was not published before interruption")
	}
	if _, ok := transfer.objects[manifestKey]; ok {
		t.Fatal("manifest was published despite interruption")
	}

	contentRedelivery := &buildGrantFixture{cacheStatus: "publishing_content"}
	secondRunner := &outputRunner{}
	second, err := New(t.TempDir(), identityFor(spec), contentRedelivery, transfer, secondRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := second.Prepare(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cache != "built" || secondRunner.runs != 1 ||
		strings.Join(contentRedelivery.stages, ",") != "manifest-read,source-read,content-write,content-read,manifest-write" {
		t.Fatalf("content redelivery result = %#v, stages = %#v", result, contentRedelivery.stages)
	}

	manifestRedelivery := &buildGrantFixture{
		cacheStatus: "publishing_manifest", manifestGrantOnIncomplete: true,
	}
	thirdRunner := &outputRunner{}
	third, err := New(t.TempDir(), identityFor(spec), manifestRedelivery, transfer, thirdRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err = third.Prepare(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cache != "hit" || thirdRunner.runs != 0 ||
		strings.Join(manifestRedelivery.stages, ",") != "manifest-read,content-read" {
		t.Fatalf("manifest redelivery result = %#v, stages = %#v", result, manifestRedelivery.stages)
	}
}

func TestManagerRejectsOversizedHitBeforeContentGrant(t *testing.T) {
	source := []byte{'P', 'K', 3, 4}
	spec := managerSpec(testDigest(source))
	buildKey, err := BuildKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey := "shared-hosting/account-1/service-1/build-inputs/source.zip"
	command := controlplane.Command{
		CommandID: "command-oversized-hit", Kind: controlplane.ContentPrepare, NodeID: "node-1",
		ServiceID: "service-1", AssignmentID: "assignment-1", AccountID: "account-1",
		Attempt: 1, Lease: controlplane.CommandLease{Token: "lease", Generation: 1},
		Prepare: &controlplane.ContentPrepareRequest{
			SchemaVersion: 1, ExpectedBuildKey: buildKey, BuildSpec: spec,
			Source:               controlplane.BuildSourceLocation{Key: sourceKey, SizeBytes: int64(len(source))},
			ResolvedDependencies: []controlplane.ResolvedDependency{},
		},
	}
	contentKey, manifestKey, _ := CacheKeys(command.Prepare.ExpectedBuildKey)
	manifest := controlplane.PreparedContentManifest{
		SchemaVersion: 1,
		BuildKey:      command.Prepare.ExpectedBuildKey,
		BuildSpec:     spec,
		Content: controlplane.WorkspaceBlob{
			Key:    contentKey,
			SHA256: strings.Repeat("a", 64), CompressedSize: MaxContentBytes + 1,
			LogicalSize: 1, Paths: []string{".xmcl/launch.sh", ".xmcl/runtime.json"},
		},
	}
	manifestBytes, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	transfer := &memoryBuildTransfer{objects: map[string][]byte{manifestKey: manifestBytes}}
	grants := &buildGrantFixture{hit: true}
	manager, err := New(t.TempDir(), identityFor(spec), grants, transfer, &outputRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Prepare(t.Context(), command)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "cache_manifest_invalid" {
		t.Fatalf("Prepare() error = %v", err)
	}
	if strings.Join(grants.stages, ",") != "manifest-read" {
		t.Fatalf("stages = %#v", grants.stages)
	}
}

func managerSpec(sourceSHA string) controlplane.BuildSpecV1 {
	return controlplane.BuildSpecV1{
		SchemaVersion: 1,
		Source:        controlplane.BuildSourceIdentity{Format: "zip", SHA256: sourceSHA},
		Compatibility: controlplane.BuildCompatibility{
			MinecraftVersion: "1.21.1",
			Loader:           controlplane.BuildLoader{Kind: "neoforge", Version: "21.1.115"},
			Java:             controlplane.BuildJava{Component: "java-runtime-delta", Major: 21},
		},
		Files:        []controlplane.BuildFile{},
		Dependencies: []controlplane.BuildDependency{},
		Builder: controlplane.BuildImageIdentity{
			Image:  BuilderImageName + "@sha256:" + strings.Repeat("a", 64),
			Schema: Schema, ToolchainCatalogRevision: strings.Repeat("c", 64),
		},
		Runtime: controlplane.RuntimeImageIdentity{
			Image:                  RuntimeImageName + "@sha256:" + strings.Repeat("b", 64),
			RuntimeCatalogRevision: runtimecontract.CatalogSHA256(),
		},
		Target: controlplane.BuildTarget{OS: "linux", Architecture: "amd64"},
	}
}

func TestManagerValidatePrunesOnlyBuildStagingDirectories(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, strings.Repeat("a", 64))
	unrelated := filepath.Join(root, "operator-owned")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := managerSpec(strings.Repeat("1", 64))
	manager, err := New(root, identityFor(spec), &buildGrantFixture{}, &memoryBuildTransfer{objects: map[string][]byte{}}, &outputRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale build staging remains: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated builder-root entry was removed: %v", err)
	}
}

func preparedArchive(spec controlplane.BuildSpecV1) ([]byte, []string, int64) {
	descriptor, _ := json.Marshal(map[string]any{
		"schemaVersion": 1, "runtimeCatalogRevision": spec.Runtime.RuntimeCatalogRevision,
		"minecraftVersion": spec.Compatibility.MinecraftVersion,
		"java": map[string]any{
			"component": spec.Compatibility.Java.Component, "major": spec.Compatibility.Java.Major,
			"jreId": "java-runtime-delta-21",
		},
		"loader": map[string]any{
			"kind": spec.Compatibility.Loader.Kind, "version": spec.Compatibility.Loader.Version,
		},
		"launch": map[string]any{
			"kind": "generated-server-launcher", "path": ".xmcl/launch.sh",
			"arguments": []string{"@libraries/net/neoforged/args.txt"},
		},
	})
	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{".xmcl/launch.sh", 0o755, []byte("#!/bin/sh\n")},
		{".xmcl/runtime.json", 0o644, descriptor},
	}
	var raw bytes.Buffer
	writer := tar.NewWriter(&raw)
	var logical int64
	paths := make([]string, 0, len(files))
	for _, file := range files {
		_ = writer.WriteHeader(&tar.Header{
			Name: file.name, Mode: file.mode, Size: int64(len(file.data)), ModTime: time.Unix(0, 0),
		})
		_, _ = writer.Write(file.data)
		logical += int64(len(file.data))
		paths = append(paths, file.name)
	}
	_ = writer.Close()
	var compressed bytes.Buffer
	encoder, _ := zstd.NewWriter(&compressed)
	_, _ = encoder.Write(raw.Bytes())
	_ = encoder.Close()
	return compressed.Bytes(), paths, logical
}

func testDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
