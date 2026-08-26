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
func (*daemonGateway) Ack(context.Context, string, controlplane.CommandLease, controlplane.CommandResult) error {
	return nil
}
func (*daemonGateway) Register(context.Context, controlplane.NodeCapacity) error { return nil }
func (g *daemonGateway) Heartbeat(context.Context, controlplane.NodeStatus) error {
	select {
	case g.heartbeats <- struct{}{}:
	default:
	}
	return nil
}
func (*daemonGateway) ReportStarted(context.Context, string, string, controlplane.Endpoint) error {
	return nil
}
func (*daemonGateway) ReportStopped(context.Context, controlplane.StoppedReport) error {
	return nil
}
func (*daemonGateway) ReportStoppedAndSynced(context.Context, controlplane.SyncResult) error {
	return nil
}

func validStatus() controlplane.NodeStatus {
	return controlplane.NodeStatus{
		ContractVersion: controlplane.SharedNodeContractVersion,
		Status:          "ready",
		Capacity:        controlplane.AvailableCapacity{},
		AgentVersion:    "test-agent",
		Ingress:         controlplane.Ingress{Host: "public-node.example"},
	}
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
		Status:            validStatus,
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

func TestDaemonRecordsReadinessAfterConfirmedHeartbeat(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gateway := &daemonGateway{heartbeats: make(chan struct{}, 1)}
	ready := make(chan struct{}, 1)
	daemon := Daemon{
		NodeID: "node_1", Source: gateway, Reporter: gateway,
		Executor: NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{}),
		Status:   validStatus,
		Ready: func() error {
			ready <- struct{}{}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("daemon did not record readiness after its initial heartbeat")
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
		Status:   validStatus,
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

type interruptedAckGateway struct {
	daemonGateway
	cancel         context.CancelFunc
	acks           atomic.Int32
	syncReports    atomic.Int32
	stoppedReports atomic.Int32
}

func (g *interruptedAckGateway) Ack(context.Context, string, controlplane.CommandLease, controlplane.CommandResult) error {
	g.acks.Add(1)
	if g.cancel != nil {
		g.cancel()
		return context.Canceled
	}
	return nil
}

func (g *interruptedAckGateway) ReportStopped(context.Context, controlplane.StoppedReport) error {
	g.stoppedReports.Add(1)
	return nil
}

func (g *interruptedAckGateway) ReportStoppedAndSynced(context.Context, controlplane.SyncResult) error {
	g.syncReports.Add(1)
	return nil
}

func TestStopSyncReleasesBeforeAckAndRecoversAfterInterruption(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, runtime := &fakeWorkspace{}, &fakeRuntime{}
	executor := NewExecutor("node_1", store, workspace, runtime)
	if _, err := executor.Execute(context.Background(), testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")); err != nil {
		t.Fatal(err)
	}
	command := testCommand(controlplane.StopAndSync, "stop_1", "node_1", "assignment_1")
	ctx, cancel := context.WithCancel(context.Background())
	gateway := &interruptedAckGateway{
		daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)},
		cancel:        cancel,
	}
	daemon := Daemon{NodeID: "node_1", Source: gateway, Reporter: gateway, Executor: executor}
	if err := daemon.Process(ctx, command); !errors.Is(err, context.Canceled) {
		t.Fatalf("process error = %v, want interrupted acknowledgement", err)
	}
	if workspace.releases != 1 || gateway.acks.Load() != 1 {
		t.Fatalf("release/ack calls = %d/%d, want 1/1", workspace.releases, gateway.acks.Load())
	}

	gateway.cancel = nil
	if err := daemon.Process(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if workspace.syncs != 1 {
		t.Fatalf("workspace syncs = %d, want cached result without another sync", workspace.syncs)
	}
	if workspace.releases != 2 || gateway.syncReports.Load() != 2 || gateway.acks.Load() != 2 {
		t.Fatalf(
			"release/report/ack calls = %d/%d/%d, want idempotent replay 2/2/2",
			workspace.releases,
			gateway.syncReports.Load(),
			gateway.acks.Load(),
		)
	}
}

func (s *leaseSource) Ack(context.Context, string, controlplane.CommandLease, controlplane.CommandResult) error {
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
		Token: "lease", Generation: 1, ExpiresAt: time.Now().Add(50 * time.Millisecond).UTC().Format(time.RFC3339Nano),
	}
	if err := daemon.Process(context.Background(), command); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("process error = %v, want lease loss", err)
	}
	if source.acks.Load() != 0 {
		t.Fatal("stale lease command was acknowledged")
	}
}

type persistedCredentialGateway struct {
	daemonGateway
	registered atomic.Int32
}

func (g *persistedCredentialGateway) Register(context.Context, controlplane.NodeCapacity) error {
	g.registered.Add(1)
	return nil
}
func (*persistedCredentialGateway) HasCredential() bool                { return true }
func (*persistedCredentialGateway) IsAuthenticationFailure(error) bool { return false }
func (*persistedCredentialGateway) InvalidateCredential() error        { return nil }

func TestDaemonReusesPersistedCredentialOnRestart(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gateway := &persistedCredentialGateway{daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)}}
	daemon := Daemon{
		NodeID: "node_1", Source: gateway, Reporter: gateway,
		Executor: NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{}),
		Status:   validStatus,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	select {
	case <-gateway.heartbeats:
	case <-time.After(time.Second):
		t.Fatal("daemon did not start with the persisted credential")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if gateway.registered.Load() != 0 {
		t.Fatalf("registration calls = %d, want 0", gateway.registered.Load())
	}
}

type rejectedCredentialGateway struct {
	daemonGateway
	hasCredential atomic.Bool
	invalidated   atomic.Int32
	registered    atomic.Int32
}

func (g *rejectedCredentialGateway) Register(context.Context, controlplane.NodeCapacity) error {
	g.registered.Add(1)
	g.hasCredential.Store(true)
	return nil
}
func (g *rejectedCredentialGateway) HasCredential() bool { return g.hasCredential.Load() }
func (*rejectedCredentialGateway) IsAuthenticationFailure(err error) bool {
	var responseError *controlplane.HTTPError
	return errors.As(err, &responseError) && responseError.StatusCode == 401
}
func (g *rejectedCredentialGateway) InvalidateCredential() error {
	g.invalidated.Add(1)
	g.hasCredential.Store(false)
	return nil
}
func (g *rejectedCredentialGateway) Heartbeat(ctx context.Context, status controlplane.NodeStatus) error {
	if g.invalidated.Load() == 0 {
		return &controlplane.HTTPError{StatusCode: 401}
	}
	return g.daemonGateway.Heartbeat(ctx, status)
}

func TestDaemonNeverReusesBootstrapAfterCredentialRejection(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gateway := &rejectedCredentialGateway{daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)}}
	gateway.hasCredential.Store(true)
	daemon := Daemon{
		NodeID: "node_1", Source: gateway, Reporter: gateway,
		Executor: NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{}),
		Status:   validStatus,
	}
	recovered, err := daemon.recoverAuthentication(context.Background(), &controlplane.HTTPError{StatusCode: 401})
	if !recovered || err == nil {
		t.Fatalf("authentication rejection recovery = %v, %v", recovered, err)
	}
	if gateway.invalidated.Load() != 0 || gateway.registered.Load() != 0 {
		t.Fatalf("invalidations = %d registrations = %d; bootstrap must not be retried", gateway.invalidated.Load(), gateway.registered.Load())
	}
}
