package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type fakeWorkspace struct {
	restores int
	syncs    int
	releases int
	syncErr  error
}

func (*fakeWorkspace) Path(serviceID string) (string, error) {
	return "/work/" + serviceID, nil
}

func (w *fakeWorkspace) Restore(_ context.Context, _ controlplane.Command) (string, error) {
	w.restores++
	return "/work/service_1", nil
}

func (w *fakeWorkspace) Sync(_ context.Context, command controlplane.Command) (controlplane.SyncResult, error) {
	w.syncs++
	if w.syncErr != nil {
		return controlplane.SyncResult{}, w.syncErr
	}
	return controlplane.SyncResult{ServiceID: command.ServiceID, AssignmentID: command.AssignmentID, Revision: 2}, nil
}

func (w *fakeWorkspace) Release(context.Context, controlplane.Command) error {
	w.releases++
	return nil
}

type fakeRuntime struct {
	starts   int
	stops    int
	running  []RunningService
	startErr error
}

func (r *fakeRuntime) Start(context.Context, controlplane.Command, string) error {
	r.starts++
	return r.startErr
}
func (r *fakeRuntime) Stop(context.Context, controlplane.Command) error {
	r.stops++
	return nil
}
func (r *fakeRuntime) Running(context.Context) ([]RunningService, error) { return r.running, nil }

func testCommand(kind, id, node, assignment string) controlplane.Command {
	return controlplane.Command{
		CommandID: id, Kind: kind, NodeID: node, ServiceID: "service_1", AssignmentID: assignment,
		Workspace:  controlplane.Workspace{ObjectPrefix: "services/service_1", Revision: 1},
		Resources:  controlplane.Resources{MemoryMiB: 512, SharedCPU: 1, BurstCPU: 1, WorkspaceGiB: 1},
		Connection: &controlplane.Connection{Host: "public-node.example", HostPort: 25565},
	}
}

func TestCommandIDExecutesOnceAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	firstWorkspace, firstRuntime := &fakeWorkspace{}, &fakeRuntime{}
	first := NewExecutor("node_1", store, firstWorkspace, firstRuntime)
	command := testCommand(controlplane.RestoreAndStart, "command_1", "node_1", "assignment_1")
	if result, err := first.Execute(context.Background(), command); err != nil || result.Status != "started" {
		t.Fatalf("first execution = %#v, %v", result, err)
	}
	reopened, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	secondWorkspace, secondRuntime := &fakeWorkspace{}, &fakeRuntime{}
	second := NewExecutor("node_1", reopened, secondWorkspace, secondRuntime)
	if result, err := second.Execute(context.Background(), command); err != nil || result.Status != "started" {
		t.Fatalf("replayed execution = %#v, %v", result, err)
	}
	if firstRuntime.starts != 1 || secondRuntime.starts != 0 || secondWorkspace.restores != 0 {
		t.Fatalf("command replay performed work: first=%d second=%d restores=%d", firstRuntime.starts, secondRuntime.starts, secondWorkspace.restores)
	}
}

func TestStopAndSyncCompletesWhenStartNeverReachedTheNode(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, runtime := &fakeWorkspace{}, &fakeRuntime{}
	executor := NewExecutor("node_1", store, workspace, runtime)
	command := testCommand(
		controlplane.StopAndSync,
		"stop_1",
		"node_1",
		"assignment_1",
	)

	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.Status != "stopped" {
		t.Fatalf("stop phase = %#v, %v", result, err)
	}
	if runtime.stops != 0 {
		t.Fatalf("missing assignment stopped a nonexistent container %d times", runtime.stops)
	}
	if err := executor.MarkStoppedReported(command.ServiceID, command.AssignmentID); err != nil {
		t.Fatal(err)
	}
	result, err = executor.Execute(context.Background(), command)
	if err != nil || result.Status != "stopped-and-synced" {
		t.Fatalf("sync phase = %#v, %v", result, err)
	}
	if workspace.syncs != 1 {
		t.Fatalf("empty workspace syncs = %d, want 1", workspace.syncs)
	}
}

func TestDifferentAssignmentIsRejectedWhileServiceActive(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	workspace, runtime := &fakeWorkspace{}, &fakeRuntime{}
	executor := NewExecutor("node_1", store, workspace, runtime)
	if _, err := executor.Execute(context.Background(), testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), testCommand(controlplane.RestoreAndStart, "start_2", "node_1", "assignment_2"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || workspace.restores != 1 || runtime.starts != 1 {
		t.Fatalf("different assignment was not rejected safely: %#v", result)
	}
}

func TestStartingStateResumesWithoutReplacingWorkspace(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := &fakeWorkspace{}
	runtime := &fakeRuntime{startErr: errors.New("Docker temporarily unavailable")}
	executor := NewExecutor("node_1", store, workspace, runtime)
	command := testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")
	if _, err := executor.Execute(context.Background(), command); err == nil {
		t.Fatal("initial Docker failure was not retained for retry")
	}
	active, exists, err := store.Active(command.ServiceID)
	if err != nil || !exists || active.Phase != "starting" {
		t.Fatalf("starting state = %#v exists=%v err=%v", active, exists, err)
	}
	runtime.startErr = nil
	if result, err := executor.Execute(context.Background(), command); err != nil || result.Status != "started" {
		t.Fatalf("starting resume = %#v, %v", result, err)
	}
	if workspace.restores != 1 || runtime.starts != 2 {
		t.Fatalf("resume restored workspace again: restores=%d starts=%d", workspace.restores, runtime.starts)
	}
}

func TestRestoreRejectsCommandWithoutAssignedConnection(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, runtime := &fakeWorkspace{}, &fakeRuntime{}
	executor := NewExecutor("node_1", store, workspace, runtime)
	command := testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")
	command.Connection = nil
	result, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || workspace.restores != 0 || runtime.starts != 0 {
		t.Fatalf("missing assigned connection was not rejected: %#v", result)
	}
}

func TestFailedSyncRetainsActiveAssignment(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, runtime := &fakeWorkspace{syncErr: errors.New("Azure Blob unavailable")}, &fakeRuntime{}
	executor := NewExecutor("node_1", store, workspace, runtime)
	start := testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")
	if _, err := executor.Execute(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	stop := testCommand(controlplane.StopAndSync, "stop_1", "node_1", "assignment_1")
	if result, err := executor.Execute(context.Background(), stop); err != nil || result.Status != "stopped" {
		t.Fatalf("stop phase = %#v, %v", result, err)
	}
	if err := executor.MarkStoppedReported(stop.ServiceID, stop.AssignmentID); err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), stop)
	var retry *RetryableError
	if !errors.As(err, &retry) {
		t.Fatalf("sync failure error = %v, want retryable", err)
	}
	active, exists, err := store.Active("service_1")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || active.AssignmentID != "assignment_1" || runtime.stops != 1 {
		t.Fatalf("failed sync incorrectly released assignment: %#v %v", active, exists)
	}
	if _, cached, err := store.Result(stop.CommandID); err != nil || cached {
		t.Fatalf("transient stop was cached: cached=%v err=%v", cached, err)
	}
}

func TestSuccessfulStopReleasesServiceForAnotherNode(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace, runtime := &fakeWorkspace{}, &fakeRuntime{}
	first := NewExecutor("node_1", store, workspace, runtime)
	if _, err := first.Execute(context.Background(), testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")); err != nil {
		t.Fatal(err)
	}
	stop := testCommand(controlplane.StopAndSync, "stop_1", "node_1", "assignment_1")
	if result, err := first.Execute(context.Background(), stop); err != nil || result.Status != "stopped" {
		t.Fatalf("stop phase = %#v, %v", result, err)
	}
	if err := first.MarkStoppedReported(stop.ServiceID, stop.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if result, err := first.Execute(context.Background(), stop); err != nil || result.Status != "stopped-and-synced" {
		t.Fatalf("stop = %#v, %v", result, err)
	}
	reopened, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	nextWorkspace, nextRuntime := &fakeWorkspace{}, &fakeRuntime{}
	second := NewExecutor("node_2", reopened, nextWorkspace, nextRuntime)
	if result, err := second.Execute(context.Background(), testCommand(controlplane.RestoreAndStart, "start_2", "node_2", "assignment_2")); err != nil || result.Status != "started" {
		t.Fatalf("new assignment = %#v, %v", result, err)
	}
}

func TestDaemonDoesNotReportStopBeforeSuccessfulSync(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, runtime := &fakeWorkspace{syncErr: errors.New("Azure Blob unavailable")}, &fakeRuntime{}
	gateway := controlplane.NewMemoryGateway(1)
	executor := NewExecutor("node_1", store, workspace, runtime)
	daemon := Daemon{NodeID: "node_1", Source: gateway, Reporter: gateway, Executor: executor}
	if err := daemon.Process(context.Background(), testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")); err != nil {
		t.Fatal(err)
	}
	if len(gateway.Started) != 1 || gateway.Started[0].Endpoint != (controlplane.Endpoint{Host: "public-node.example", Port: 25565}) {
		t.Fatalf("started endpoint = %#v", gateway.Started)
	}
	if err := daemon.Process(context.Background(), testCommand(controlplane.StopAndSync, "stop_1", "node_1", "assignment_1")); err == nil {
		t.Fatal("transient sync failure was acknowledged")
	}
	if len(gateway.Synced) != 0 {
		t.Fatal("reported stopped-and-synced despite failed manifest publication")
	}
	if _, acknowledged := gateway.Acks["stop_1"]; acknowledged {
		t.Fatal("transient stop failure was acknowledged")
	}
	if workspace.releases != 0 {
		t.Fatal("released local workspace despite failed sync")
	}
}

func TestDaemonReleasesWorkspaceAfterAcknowledgedSync(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, runtime := &fakeWorkspace{}, &fakeRuntime{}
	gateway := controlplane.NewMemoryGateway(1)
	executor := NewExecutor("node_1", store, workspace, runtime)
	daemon := Daemon{NodeID: "node_1", Source: gateway, Reporter: gateway, Executor: executor}
	if err := daemon.Process(context.Background(), testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")); err != nil {
		t.Fatal(err)
	}
	if err := daemon.Process(context.Background(), testCommand(controlplane.StopAndSync, "stop_1", "node_1", "assignment_1")); err != nil {
		t.Fatal(err)
	}
	if workspace.releases != 1 || gateway.Acks["stop_1"].Status != "stopped-and-synced" {
		t.Fatalf("workspace was not released only after acknowledged sync: releases=%d ack=%#v", workspace.releases, gateway.Acks["stop_1"])
	}
}

func TestReconcileRejectsUntrackedRunningContainer(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{
		running: []RunningService{{ServiceID: "service_1", AssignmentID: "assignment_1"}},
	})
	if err := executor.Reconcile(context.Background()); err == nil {
		t.Fatal("expected untracked running container to be rejected")
	}
}
