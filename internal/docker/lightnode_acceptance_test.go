//go:build integration

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/klauspost/compress/zstd"
	"github.com/voxelum/xmcl-shared-node-agent/internal/agent"
	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	"github.com/voxelum/xmcl-shared-node-agent/internal/quota"
	runtimecontract "github.com/voxelum/xmcl-shared-node-agent/internal/runtime"
	"github.com/voxelum/xmcl-shared-node-agent/internal/workspace"
)

const acceptanceImageIdentity = "ghcr.io/voxelum/xmcl-shared-minecraft-runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type memoryObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

type acceptanceGateway struct {
	*controlplane.MemoryGateway
	registered int
	heartbeats chan controlplane.NodeStatus
}

func (g *acceptanceGateway) Register(context.Context, controlplane.NodeCapacity) error {
	g.registered++
	return nil
}

func (g *acceptanceGateway) Heartbeat(_ context.Context, status controlplane.NodeStatus) error {
	g.heartbeats <- status
	return nil
}

func newMemoryObjectStore() *memoryObjectStore {
	return &memoryObjectStore{objects: make(map[string][]byte)}
}

func (s *memoryObjectStore) grant(key, method string) controlplane.WorkspaceGrant {
	return controlplane.WorkspaceGrant{
		Key: key, Method: method,
		URL:       "https://objects.mock-lightnode.invalid/bucket/" + key,
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
}

func (s *memoryObjectStore) RestoreWorkspaceGrants(_ context.Context, command controlplane.Command, purpose string, keys []string) (controlplane.WorkspaceGrantResponse, error) {
	if purpose != "blobs" || len(keys) != 1 || command.RuntimeContent == nil ||
		keys[0] != command.RuntimeContent.Key {
		return controlplane.WorkspaceGrantResponse{}, fmt.Errorf("unexpected restore grant request")
	}
	return controlplane.WorkspaceGrantResponse{
		ContractVersion: controlplane.WorkspaceGrantContractVersion,
		Grants:          []controlplane.WorkspaceGrant{s.grant(keys[0], "GET")},
	}, nil
}

func (s *memoryObjectStore) SyncWorkspaceGrants(_ context.Context, _ controlplane.Command, _ controlplane.WorkspaceManifest, _ string, keys []string) (controlplane.WorkspaceGrantResponse, error) {
	grants := make([]controlplane.WorkspaceGrant, 0, len(keys))
	for _, key := range keys {
		grants = append(grants, s.grant(key, "PUT"))
	}
	return controlplane.WorkspaceGrantResponse{
		ContractVersion: controlplane.WorkspaceGrantContractVersion,
		Grants:          grants,
	}, nil
}

func (s *memoryObjectStore) PublishWorkspaceGrant(_ context.Context, command controlplane.Command, manifest controlplane.WorkspaceManifest, _ string) (controlplane.WorkspaceGrantResponse, error) {
	key := fmt.Sprintf("%s/revisions/%d/manifest.json", command.Workspace.ObjectPrefix, manifest.Revision)
	return controlplane.WorkspaceGrantResponse{
		ContractVersion: controlplane.WorkspaceGrantContractVersion,
		Grants:          []controlplane.WorkspaceGrant{s.grant(key, "PUT")},
	}, nil
}

func (s *memoryObjectStore) Download(_ context.Context, grant controlplane.WorkspaceGrant, key string, limit int64, destination io.Writer) (workspace.TransferResult, error) {
	if grant.Key != key || grant.Method != "GET" {
		return workspace.TransferResult{}, fmt.Errorf("download grant mismatch")
	}
	s.mu.Lock()
	data, ok := s.objects[key]
	s.mu.Unlock()
	if !ok || int64(len(data)) > limit {
		return workspace.TransferResult{}, fmt.Errorf("mock object unavailable")
	}
	if _, err := destination.Write(data); err != nil {
		return workspace.TransferResult{}, err
	}
	return workspace.TransferResult{Size: int64(len(data)), SHA256: digest(data)}, nil
}

func (s *memoryObjectStore) Upload(_ context.Context, grant controlplane.WorkspaceGrant, key string, source io.Reader, size int64, expectedSHA256 string) (workspace.TransferResult, error) {
	if grant.Key != key || grant.Method != "PUT" {
		return workspace.TransferResult{}, fmt.Errorf("upload grant mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(source, size+1))
	if err != nil {
		return workspace.TransferResult{}, err
	}
	if int64(len(data)) != size || digest(data) != expectedSHA256 {
		return workspace.TransferResult{}, fmt.Errorf("mock upload integrity mismatch")
	}
	s.mu.Lock()
	s.objects[key] = append([]byte(nil), data...)
	s.mu.Unlock()
	return workspace.TransferResult{Size: size, SHA256: expectedSHA256}, nil
}

func digest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func compilerContent(t *testing.T) ([]byte, []string, int64) {
	t.Helper()
	source := []byte("import java.net.ServerSocket; public class MockServer { public static void main(String[] args) throws Exception { try (ServerSocket server = new ServerSocket(25565)) { while (true) { server.accept().close(); } } } }\n")
	compileRoot := t.TempDir()
	sourcePath := filepath.Join(compileRoot, "MockServer.java")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	compile := exec.Command(
		"java", "-m", "jdk.compiler/com.sun.tools.javac.Main", "MockServer.java",
	)
	compile.Dir = compileRoot
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile mock Minecraft server: %v: %s", err, output)
	}
	mockServer, err := os.ReadFile(filepath.Join(compileRoot, "MockServer.class"))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := map[string]any{
		"schemaVersion":          1,
		"runtimeCatalogRevision": runtimecontract.CatalogSHA256(),
		"minecraftVersion":       "1.21.1",
		"loader": map[string]any{
			"kind": "neoforge", "version": "21.1.115",
		},
		"java": map[string]any{
			"component": "java-runtime-delta", "major": 21,
			"jreId": "java-runtime-delta-21",
		},
		"launch": map[string]any{
			"path": ".xmcl/launch.sh", "kind": "generated-server-launcher",
			"arguments": []string{"MockServer"},
		},
	}
	runtimeJSON, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{
			name: ".xmcl/runtime.json", mode: 0o644,
			data: runtimeJSON,
		},
		{
			name: ".xmcl/launch.sh", mode: 0o755,
			data: []byte("#!/bin/sh\nset -eu\n: \"${XMCL_JAVA:?XMCL_JAVA is required}\"\nexec \"$XMCL_JAVA\" MockServer\n"),
		},
		{
			name: "MockServer.class", mode: 0o644,
			data: mockServer,
		},
	}
	var tarBytes bytes.Buffer
	tarWriter := tar.NewWriter(&tarBytes)
	var logicalSize int64
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: file.name, Mode: file.mode, Size: int64(len(file.data)),
			ModTime: time.Unix(0, 0).UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(file.data); err != nil {
			t.Fatal(err)
		}
		logicalSize += int64(len(file.data))
		paths = append(paths, file.name)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(tarBytes.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes(), paths, logicalSize
}

func TestMockLightNodeDockerLifecycle(t *testing.T) {
	image := os.Getenv("XMCL_ACCEPTANCE_RUNTIME_IMAGE")
	workspaceRoot := os.Getenv("XMCL_ACCEPTANCE_WORKSPACE_ROOT")
	if image == "" || workspaceRoot == "" {
		t.Skip("set XMCL_ACCEPTANCE_RUNTIME_IMAGE and XMCL_ACCEPTANCE_WORKSPACE_ROOT")
	}
	absoluteRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		client: api, image: acceptanceImageIdentity, dockerImage: image,
		stopTimeout: 10 * time.Second, healthWait: 2 * time.Minute,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := runtime.Validate(ctx); err != nil {
		t.Fatal(err)
	}

	const serviceID = "acceptance-service"
	name, err := containerName(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = api.ContainerRemove(context.Background(), name, container.RemoveOptions{Force: true})
	})

	store := newMemoryObjectStore()
	content, paths, logicalSize := compilerContent(t)
	contentKey := "shared-hosting/acceptance-account/acceptance-service/compiler-content/content.tar.zst"
	store.objects[contentKey] = content
	contentSHA := digest(content)

	manager := workspace.New(absoluteRoot, store, store, quota.NewHelper(absoluteRoot))
	if err := manager.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(absoluteRoot, ".acceptance-state")
	if err := os.RemoveAll(stateRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })
	state, err := agent.NewFileStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := agent.NewFileLocker(filepath.Join(stateRoot, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	executor := agent.NewExecutor("mock-lightnode-node", state, manager, runtime, locker)
	gateway := &acceptanceGateway{
		MemoryGateway: controlplane.NewMemoryGateway(2),
		heartbeats:    make(chan controlplane.NodeStatus, 1),
	}
	daemon := agent.Daemon{
		NodeID: "mock-lightnode-node", Source: gateway, Reporter: gateway,
		Executor: executor,
		Capacity: controlplane.NodeCapacity{
			TotalMemoryMiB: 2048, TotalSharedCPU: 2, TotalWorkspaceGiB: 2,
		},
		Status: func() controlplane.NodeStatus {
			return controlplane.NodeStatus{
				ContractVersion: controlplane.SharedNodeContractVersion,
				Status:          "ready",
				AgentVersion:    "mock-lightnode",
				Ingress:         controlplane.Ingress{Host: "mock-lightnode.local"},
			}
		},
	}
	runContext, stopRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(runContext) }()
	status := <-gateway.heartbeats
	stopRun()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if gateway.registered != 1 || !status.Valid() ||
		status.NodeID != "mock-lightnode-node" {
		t.Fatalf("registration=%d heartbeat=%#v", gateway.registered, status)
	}
	lease := controlplane.CommandLease{
		Token: "acceptance-lease", Generation: 1,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
	start := controlplane.Command{
		CommandID: "acceptance-start", Kind: controlplane.RestoreAndStart,
		NodeID: "mock-lightnode-node", ServiceID: serviceID,
		AssignmentID: "acceptance-assignment", AccountID: "acceptance-account",
		Workspace: controlplane.Workspace{
			ObjectPrefix: "shared-hosting/acceptance-account/acceptance-service",
			Revision:     0,
		},
		RuntimeContent: &controlplane.WorkspaceBlob{
			Key: contentKey, SHA256: contentSHA,
			CompressedSize: int64(len(content)), LogicalSize: logicalSize,
			Paths: paths,
		},
		EULAAccepted: true,
		Resources: controlplane.Resources{
			MemoryMiB: 512, SharedCPU: 1, BurstCPU: 1, WorkspaceGiB: 1,
		},
		Connection: &controlplane.Connection{Host: "127.0.0.1", HostPort: 25575},
		Lease:      lease,
	}
	if err := daemon.Process(ctx, start); err != nil {
		t.Fatal(err)
	}
	if len(gateway.Started) != 1 || gateway.Started[0].AssignmentID != start.AssignmentID {
		t.Fatalf("started reports = %#v", gateway.Started)
	}
	running, err := runtime.Running(ctx)
	if err != nil || len(running) != 1 || running[0].ServiceID != serviceID {
		t.Fatalf("running services = %#v, %v", running, err)
	}

	stop := start
	stop.CommandID = "acceptance-stop"
	stop.Kind = controlplane.StopAndSync
	stop.Lease = lease
	if err := daemon.Process(ctx, stop); err != nil {
		t.Fatal(err)
	}
	if len(gateway.Synced) != 1 || gateway.Synced[0].Revision != 1 {
		t.Fatalf("sync reports = %#v", gateway.Synced)
	}
	if _, err := os.Stat(filepath.Join(absoluteRoot, serviceID)); !os.IsNotExist(err) {
		t.Fatalf("released workspace still exists: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	foundManifest := false
	for key := range store.objects {
		if strings.HasSuffix(key, "/revisions/1/manifest.json") {
			foundManifest = true
		}
	}
	if !foundManifest {
		t.Fatal("workspace manifest was not published")
	}
}
