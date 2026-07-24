package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type memoryObjects struct {
	mu       sync.Mutex
	objects  map[string][]byte
	failPut  bool
	uploaded []string
}

func (s *memoryObjects) Download(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), data...), nil
}

func (s *memoryObjects) Upload(_ context.Context, key string, data []byte, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut {
		return errors.New("storage unavailable")
	}
	s.objects[key] = append([]byte(nil), data...)
	s.uploaded = append(s.uploaded, key)
	return nil
}

type testQuota struct{}

func (testQuota) Validate(context.Context) error             { return nil }
func (testQuota) Apply(context.Context, string, int64) error { return nil }

func restoreCommand() controlplane.Command {
	return controlplane.Command{
		CommandID: "command_1", Kind: controlplane.RestoreAndStart, NodeID: "node_1",
		ServiceID: "service_1", AssignmentID: "assignment_1",
		Workspace: controlplane.Workspace{ObjectPrefix: "accounts/account_1", Revision: 2},
		Resources: controlplane.Resources{MemoryMiB: 1024, SharedCPU: 1, BurstCPU: 1, WorkspaceGiB: 1},
	}
}

func TestRestoreRejectsTraversalManifestPath(t *testing.T) {
	command := restoreCommand()
	manifest := Manifest{
		SchemaVersion: 1, ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		Revision: command.Workspace.Revision, SizeBytes: 1,
		Files: []File{{Path: "../outside", SizeBytes: 1, SHA256: digest([]byte("x"))}},
	}
	manifest.SHA256 = aggregateHash(manifest.Files)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	objects := &memoryObjects{objects: map[string][]byte{
		revisionKey(command.Workspace.ObjectPrefix, command.Workspace.Revision, "manifest.json"): data,
	}}
	manager := New(t.TempDir(), objects, testQuota{})
	if _, err := manager.Restore(context.Background(), command); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
}

func TestRestoreHashMismatchDoesNotActivateWorkspace(t *testing.T) {
	command := restoreCommand()
	manifest := Manifest{
		SchemaVersion: 1, ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		Revision: command.Workspace.Revision, SizeBytes: 3,
		Files: []File{{Path: "world/level.dat", SizeBytes: 3, SHA256: digest([]byte("expected"))}},
	}
	manifest.SHA256 = aggregateHash(manifest.Files)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	objects := &memoryObjects{objects: map[string][]byte{
		revisionKey(command.Workspace.ObjectPrefix, command.Workspace.Revision, "manifest.json"):   data,
		revisionKey(command.Workspace.ObjectPrefix, command.Workspace.Revision, "world/level.dat"): []byte("bad"),
	}}
	manager := New(t.TempDir(), objects, testQuota{})
	if _, err := manager.Restore(context.Background(), command); err == nil {
		t.Fatal("expected file hash mismatch to fail restore")
	}
}

func TestSyncPublishesManifestLast(t *testing.T) {
	command := restoreCommand()
	command.Kind = controlplane.StopAndSync
	root := t.TempDir()
	manager := New(root, &memoryObjects{objects: make(map[string][]byte)}, testQuota{})
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "world"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "world", "level.dat"), []byte("world"), 0o640); err != nil {
		t.Fatal(err)
	}
	objects := manager.store.(*memoryObjects)
	if _, err := manager.Sync(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if len(objects.uploaded) != 2 || objects.uploaded[1] != revisionKey(command.Workspace.ObjectPrefix, 3, "manifest.json") {
		t.Fatalf("manifest must be published last, got %v", objects.uploaded)
	}
}
