package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	"github.com/voxelum/xmcl-shared-node-agent/internal/objectstore"
)

type memoryObjects struct {
	mu       sync.Mutex
	objects  map[string][]byte
	failPut  bool
	uploaded []string
	modified map[string]time.Time
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

func (s *memoryObjects) DownloadTo(ctx context.Context, key string, destination io.Writer) (int64, error) {
	data, err := s.Download(ctx, key)
	if err != nil {
		return 0, err
	}
	count, err := destination.Write(data)
	return int64(count), err
}

func (s *memoryObjects) Exists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok, nil
}

func (s *memoryObjects) Upload(_ context.Context, key string, data []byte, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut {
		return errors.New("storage unavailable")
	}
	s.objects[key] = append([]byte(nil), data...)
	s.uploaded = append(s.uploaded, key)
	if s.modified == nil {
		s.modified = make(map[string]time.Time)
	}
	s.modified[key] = time.Now()
	return nil
}

func (s *memoryObjects) UploadFile(ctx context.Context, key, path, contentType string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if err := s.Upload(ctx, key, data, contentType); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func (s *memoryObjects) List(_ context.Context, prefix string) ([]objectstore.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects := make([]objectstore.ObjectInfo, 0)
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, objectstore.ObjectInfo{Key: key, LastModified: s.modified[key], Size: int64(len(s.objects[key]))})
		}
	}
	return objects, nil
}

func (s *memoryObjects) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	delete(s.modified, key)
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
		manifestKey(command.Workspace.ObjectPrefix, command.Workspace.Revision): data,
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
		manifestKey(command.Workspace.ObjectPrefix, command.Workspace.Revision):                data,
		fileKey(command.Workspace.ObjectPrefix, command.Workspace.Revision, "world/level.dat"): []byte("bad"),
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
	if len(objects.uploaded) != 2 || objects.uploaded[0] != fileKey(command.Workspace.ObjectPrefix, 3, "world/level.dat") ||
		objects.uploaded[1] != manifestKey(command.Workspace.ObjectPrefix, 3) {
		t.Fatalf("manifest must be published last, got %v", objects.uploaded)
	}
	if err := manager.RefreshObjectBytes(context.Background(), command.Workspace.ObjectPrefix); err != nil {
		t.Fatal(err)
	}
	metrics := manager.Metrics()
	if metrics.LogicalBytes != int64(len("world")) || metrics.SyncBytes <= metrics.LogicalBytes || metrics.ActualObjectBytes != metrics.SyncBytes {
		t.Fatalf("unexpected storage metrics: %#v", metrics)
	}
}

func TestRestoreRequiresManifestAndExpectedManifestHash(t *testing.T) {
	command := restoreCommand()
	file := []byte("world")
	objects := &memoryObjects{objects: map[string][]byte{
		fileKey(command.Workspace.ObjectPrefix, command.Workspace.Revision, "world/level.dat"): file,
	}}
	manager := New(t.TempDir(), objects, testQuota{})
	if _, err := manager.Restore(context.Background(), command); err == nil {
		t.Fatal("partial revision without a manifest must not restore")
	}

	manifest := Manifest{
		SchemaVersion: 1, ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		Revision: command.Workspace.Revision, SizeBytes: int64(len(file)),
		Files: []File{{Path: "world/level.dat", SizeBytes: int64(len(file)), SHA256: digest(file)}},
	}
	manifest.SHA256 = aggregateHash(manifest.Files)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	objects.objects[manifestKey(command.Workspace.ObjectPrefix, command.Workspace.Revision)] = data
	command.Workspace.SHA256 = digest([]byte("wrong"))
	if _, err := manager.Restore(context.Background(), command); err == nil {
		t.Fatal("manifest hash mismatch must block restore")
	}
}

func TestRestoreRevisionZeroCreatesEmptyWorkspace(t *testing.T) {
	command := restoreCommand()
	command.Workspace.Revision = 0
	objects := &memoryObjects{objects: make(map[string][]byte)}
	manager := New(t.TempDir(), objects, testQuota{})
	path, err := manager.Restore(context.Background(), command)
	if err != nil {
		t.Fatalf("revision zero empty workspace was rejected: %v", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("new workspace should be empty, got %v", entries)
	}
}

func TestSyncIsIdempotentForCompletedRevision(t *testing.T) {
	command := restoreCommand()
	command.Kind = controlplane.StopAndSync
	root := t.TempDir()
	objects := &memoryObjects{objects: make(map[string][]byte)}
	manager := New(root, objects, testQuota{})
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "server.properties"), []byte("motd=test"), 0o640); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Sync(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	uploads := len(objects.uploaded)
	second, err := manager.Sync(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(objects.uploaded) != uploads {
		t.Fatalf("completed revision was uploaded again: first=%#v second=%#v uploads=%d", first, second, len(objects.uploaded))
	}
}

func TestCleanupIncompletePreservesCompleteAndCurrentRevisions(t *testing.T) {
	command := restoreCommand()
	prefix := command.Workspace.ObjectPrefix
	stale := time.Now().Add(-incompleteRevisionGrace - time.Hour)
	objects := &memoryObjects{
		objects: map[string][]byte{
			fileKey(prefix, 1, "partial.dat"):     []byte("partial"),
			manifestKey(prefix, 2):                []byte("complete"),
			fileKey(prefix, 3, "current-partial"): []byte("current"),
		},
		modified: map[string]time.Time{
			fileKey(prefix, 1, "partial.dat"):     stale,
			manifestKey(prefix, 2):                stale,
			fileKey(prefix, 3, "current-partial"): stale,
		},
	}

	manager := New(t.TempDir(), objects, testQuota{})
	if err := manager.CleanupIncomplete(context.Background(), prefix, 3); err != nil {
		t.Fatal(err)
	}
	if _, exists := objects.objects[fileKey(prefix, 1, "partial.dat")]; exists {
		t.Fatal("stale incomplete revision was retained")
	}
	if _, exists := objects.objects[manifestKey(prefix, 2)]; !exists {
		t.Fatal("complete historical revision was deleted")
	}
	if _, exists := objects.objects[fileKey(prefix, 3, "current-partial")]; !exists {
		t.Fatal("current revision was deleted")
	}
}

func TestFailedUploadLeavesPriorRevisionRestorable(t *testing.T) {
	command := restoreCommand()
	file := []byte("previous-world")
	manifest := Manifest{
		SchemaVersion: 1, ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		Revision: command.Workspace.Revision, SizeBytes: int64(len(file)),
		Files: []File{{Path: "world/level.dat", SizeBytes: int64(len(file)), SHA256: digest(file)}},
	}
	manifest.SHA256 = aggregateHash(manifest.Files)
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	objects := &memoryObjects{objects: map[string][]byte{
		manifestKey(command.Workspace.ObjectPrefix, command.Workspace.Revision):                manifestData,
		fileKey(command.Workspace.ObjectPrefix, command.Workspace.Revision, "world/level.dat"): file,
	}}
	manager := New(t.TempDir(), objects, testQuota{})
	manager.sleep = func(context.Context, time.Duration) error { return nil }
	path, err := manager.Path(command.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "new-world.dat"), []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	command.Kind = controlplane.StopAndSync
	objects.failPut = true
	if _, err := manager.Sync(context.Background(), command); err == nil {
		t.Fatal("expected failed upload")
	}
	objects.failPut = false
	command.Kind = controlplane.RestoreAndStart
	if _, err := manager.Restore(context.Background(), command); err != nil {
		t.Fatalf("prior complete revision stopped being restorable: %v", err)
	}
}
