package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type daemonGateway struct {
	heartbeats chan struct{}
}

func (g *daemonGateway) Next(ctx context.Context, _ string) (controlplane.Command, error) {
	<-ctx.Done()
	return controlplane.Command{}, ctx.Err()
}
func (*daemonGateway) Ack(context.Context, string, controlplane.CommandResult) error { return nil }
func (*daemonGateway) Register(context.Context, controlplane.NodeCapacity) error     { return nil }
func (g *daemonGateway) Heartbeat(context.Context, controlplane.NodeStatus) error {
	select {
	case g.heartbeats <- struct{}{}:
	default:
	}
	return nil
}
func (*daemonGateway) ReportStarted(context.Context, string, string) error { return nil }
func (*daemonGateway) ReportStoppedAndSynced(context.Context, controlplane.SyncResult) error {
	return nil
}

func TestDaemonSendsHeartbeatAfterRegistration(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gateway := &daemonGateway{heartbeats: make(chan struct{}, 1)}
	daemon := Daemon{
		NodeID: "node_1", Source: gateway, Reporter: gateway,
		Executor:          NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{}),
		HeartbeatInterval: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	select {
	case <-gateway.heartbeats:
	case <-time.After(time.Second):
		t.Fatal("daemon did not send heartbeat after registration")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type transientSource struct {
	daemonGateway
	calls atomic.Int32
}

func (s *transientSource) Next(ctx context.Context, nodeID string) (controlplane.Command, error) {
	if s.calls.Add(1) == 1 {
		return controlplane.Command{}, errors.New("temporary network failure")
	}
	return s.daemonGateway.Next(ctx, nodeID)
}

func TestDaemonRetriesTransientCommandFailure(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := &transientSource{daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)}}
	daemon := Daemon{
		NodeID: "node_1", Source: source, Reporter: source,
		Executor: NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := daemon.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if source.calls.Load() < 2 {
		t.Fatalf("transient next failure was not retried: %d calls", source.calls.Load())
	}
}

type leaseSource struct {
	daemonGateway
	acks atomic.Int32
}

func (s *leaseSource) Ack(context.Context, string, controlplane.CommandResult) error {
	s.acks.Add(1)
	return nil
}

func (*leaseSource) RenewLease(context.Context, string, controlplane.CommandLease) (controlplane.CommandLease, error) {
	return controlplane.CommandLease{}, errors.New("lease denied")
}

type blockingWorkspace struct{ fakeWorkspace }

func (w *blockingWorkspace) Sync(ctx context.Context, _ controlplane.Command) (controlplane.SyncResult, error) {
	<-ctx.Done()
	return controlplane.SyncResult{}, ctx.Err()
}

func TestLeaseLossPreventsStaleAcknowledgement(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, runtime := &blockingWorkspace{}, &fakeRuntime{}
	executor := NewExecutor("node_1", store, workspace, runtime)
	if _, err := executor.Execute(context.Background(), testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")); err != nil {
		t.Fatal(err)
	}
	source := &leaseSource{daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)}}
	daemon := Daemon{NodeID: "node_1", Source: source, Reporter: source, Executor: executor}
	command := testCommand(controlplane.StopAndSync, "stop_1", "node_1", "assignment_1")
	command.Lease = controlplane.CommandLease{
		Token: "lease", Generation: "1", ExpiresAt: time.Now().Add(50 * time.Millisecond).UTC().Format(time.RFC3339Nano),
	}
	if err := daemon.Process(context.Background(), command); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("process error = %v, want lease loss", err)
	}
	if source.acks.Load() != 0 {
		t.Fatal("stale lease command was acknowledged")
	}
}
