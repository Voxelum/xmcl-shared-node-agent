package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/builder"
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
		Platform:        controlplane.BuildTarget{OS: "linux", Architecture: "amd64"},
		Runtime: controlplane.RuntimeImageIdentity{
			Image:                  "ghcr.io/voxelum/xmcl-shared-minecraft-runtime@sha256:" + strings.Repeat("a", 64),
			RuntimeCatalogRevision: strings.Repeat("b", 64),
		},
		Builder: controlplane.BuildImageIdentity{
			Image:                    "ghcr.io/voxelum/xmcl-shared-node-builder@sha256:" + strings.Repeat("c", 64),
			Schema:                   "agent_builder_v1",
			ToolchainCatalogRevision: strings.Repeat("d", 64),
		},
		AvailableBuildSlots: 1,
	}
}

type prepareAckGateway struct {
	daemonGateway
	result controlplane.CommandResult
	acks   atomic.Int32
}

func (g *prepareAckGateway) Ack(_ context.Context, _ string, _ controlplane.CommandLease, result controlplane.CommandResult) error {
	g.result = result
	g.acks.Add(1)
	return nil
}

func TestDaemonAcknowledgesClassifiedRetryablePrepareFailure(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	preparer := &fakePreparer{err: &builder.Failure{
		Code: "builder_timeout", Class: "retryable", Err: errors.New("builder timed out"),
	}}
	executor := NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{})
	executor.SetContentPreparer(preparer)
	gateway := &prepareAckGateway{daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)}}
	daemon := Daemon{NodeID: "node_1", Source: gateway, Reporter: gateway, Executor: executor}
	command := testCommand(controlplane.ContentPrepare, "prepare_retryable", "node_1", "assignment_1")
	command.Lease = controlplane.CommandLease{Token: "lease", Generation: 1}
	if err := daemon.Process(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if gateway.acks.Load() != 1 || gateway.result.Status != "failed" ||
		gateway.result.Code != "builder_timeout" || preparer.calls != 1 {
		t.Fatalf("ack result = %#v, acks = %d, calls = %d", gateway.result, gateway.acks.Load(), preparer.calls)
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

type failingCommandSource struct {
	daemonGateway
	delivered atomic.Bool
	command   controlplane.Command
}

func (s *failingCommandSource) Next(ctx context.Context, _ string) (controlplane.Command, error) {
	if !s.delivered.Swap(true) {
		return s.command, nil
	}
	<-ctx.Done()
	return controlplane.Command{}, ctx.Err()
}

func TestDaemonReportsCommandProcessingFailure(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	command := testCommand(controlplane.RestoreAndStart, "start_1", "node_1", "assignment_1")
	source := &failingCommandSource{
		daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)},
		command:       command,
	}
	runtime := &fakeRuntime{startErr: errors.New("runtime unavailable")}
	reported := make(chan error, 1)
	daemon := Daemon{
		NodeID: "node_1", Source: source, Reporter: source,
		Executor: NewExecutor("node_1", store, &fakeWorkspace{}, runtime),
		Status:   validStatus,
		CommandFailure: func(failed controlplane.Command, err error) {
			if failed.CommandID != command.CommandID {
				t.Errorf("reported command = %q", failed.CommandID)
			}
			reported <- err
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "runtime unavailable") {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("command processing failure was not reported")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type leaseSource struct {
	daemonGateway
	acks atomic.Int32
}

type renewingLeaseSource struct {
	daemonGateway
	renewed      controlplane.CommandLease
	renewedReady chan struct{}
	readyOnce    sync.Once
	ackLease     controlplane.CommandLease
}

func (s *renewingLeaseSource) RenewLease(context.Context, string, controlplane.CommandLease) (controlplane.CommandLease, error) {
	s.readyOnce.Do(func() { close(s.renewedReady) })
	return s.renewed, nil
}

func (s *renewingLeaseSource) Ack(_ context.Context, _ string, lease controlplane.CommandLease, _ controlplane.CommandResult) error {
	s.ackLease = lease
	return nil
}

type leaseSnapshotPreparer struct {
	renewedReady <-chan struct{}
	expected     controlplane.CommandLease
	lease        controlplane.CommandLease
}

func (p *leaseSnapshotPreparer) Prepare(ctx context.Context, command controlplane.Command) (controlplane.PreparedContentResult, error) {
	select {
	case <-p.renewedReady:
	case <-ctx.Done():
		return controlplane.PreparedContentResult{}, ctx.Err()
	}
	for {
		p.lease = controlplane.CurrentCommandLease(ctx, command.Lease)
		if p.lease == p.expected {
			return controlplane.PreparedContentResult{
				Cache: "built", BuildKey: strings.Repeat("a", 64),
			}, nil
		}
		select {
		case <-ctx.Done():
			return controlplane.PreparedContentResult{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRenewedLeaseSnapshotIsSharedByExecutionAndAck(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renewed := controlplane.CommandLease{
		Token: "renewed-token", Generation: 2,
		ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}
	source := &renewingLeaseSource{
		daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)},
		renewed:       renewed,
		renewedReady:  make(chan struct{}),
	}
	preparer := &leaseSnapshotPreparer{renewedReady: source.renewedReady, expected: renewed}
	executor := NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{})
	executor.SetContentPreparer(preparer)
	daemon := Daemon{NodeID: "node_1", Source: source, Reporter: source, Executor: executor}
	command := testCommand(controlplane.ContentPrepare, "prepare-renewed", "node_1", "assignment_1")
	command.Lease = controlplane.CommandLease{
		Token: "initial-token", Generation: 1,
		ExpiresAt: time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano),
	}
	if err := daemon.Process(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if preparer.lease != renewed || source.ackLease != renewed {
		t.Fatalf("execution/ack leases = %#v/%#v, want %#v", preparer.lease, source.ackLease, renewed)
	}
}

type ackLeaseLossSource struct {
	daemonGateway
	firstCommand controlplane.Command
	nextCommand  controlplane.Command
	nextCalls    atomic.Int32
	firstAcks    atomic.Int32
	nextAcks     atomic.Int32
	nextAcked    chan struct{}
}

func (s *ackLeaseLossSource) Next(ctx context.Context, _ string) (controlplane.Command, error) {
	switch s.nextCalls.Add(1) {
	case 1:
		return s.firstCommand, nil
	case 2:
		return s.nextCommand, nil
	default:
		<-ctx.Done()
		return controlplane.Command{}, ctx.Err()
	}
}

func (s *ackLeaseLossSource) Ack(_ context.Context, commandID string, _ controlplane.CommandLease, _ controlplane.CommandResult) error {
	if commandID == s.firstCommand.CommandID {
		s.firstAcks.Add(1)
		return errors.New("temporary acknowledgement failure")
	}
	if commandID == s.nextCommand.CommandID {
		s.nextAcks.Add(1)
		select {
		case s.nextAcked <- struct{}{}:
		default:
		}
		return nil
	}
	return errors.New("unexpected command acknowledgement")
}

func (*ackLeaseLossSource) RenewLease(context.Context, string, controlplane.CommandLease) (controlplane.CommandLease, error) {
	return controlplane.CommandLease{}, errors.New("lease denied")
}

func TestAckLeaseLossStopsRetryAndPollsNextCommand(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := testCommand(controlplane.ContentPrepare, "prepare-lost-ack", "node_1", "assignment_1")
	first.Lease = controlplane.CommandLease{
		Token: "lease", Generation: 1,
		ExpiresAt: time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano),
	}
	next := testCommand(controlplane.ContentPrepare, "prepare-after-loss", "node_1", "assignment_1")
	source := &ackLeaseLossSource{
		daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)},
		firstCommand:  first,
		nextCommand:   next,
		nextAcked:     make(chan struct{}, 1),
	}
	preparer := &fakePreparer{result: controlplane.PreparedContentResult{
		Cache: "built", BuildKey: strings.Repeat("a", 64),
	}}
	executor := NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{})
	executor.SetContentPreparer(preparer)
	failures := make(chan error, 1)
	daemon := Daemon{
		NodeID: "node_1", Source: source, Reporter: source, Executor: executor,
		Status: validStatus,
		CommandFailure: func(_ controlplane.Command, err error) {
			failures <- err
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	select {
	case <-source.nextAcked:
	case <-time.After(time.Second):
		t.Fatal("daemon did not poll and acknowledge the next command after lease loss")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failures:
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("first command failure = %v, want lease loss", err)
		}
	default:
		t.Fatal("lease loss was not reported")
	}
	if source.firstAcks.Load() != 1 || source.nextAcks.Load() != 1 ||
		source.nextCalls.Load() < 2 || preparer.calls != 2 {
		t.Fatalf(
			"first/next ack, polls, prepares = %d/%d, %d, %d",
			source.firstAcks.Load(), source.nextAcks.Load(), source.nextCalls.Load(), preparer.calls,
		)
	}
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

type startedReportLeaseLossSource struct {
	daemonGateway
	reports atomic.Int32
	acks    atomic.Int32
}

func (s *startedReportLeaseLossSource) ReportStarted(context.Context, string, string, controlplane.Endpoint) error {
	s.reports.Add(1)
	return errors.New("temporary started report failure")
}

func (s *startedReportLeaseLossSource) Ack(context.Context, string, controlplane.CommandLease, controlplane.CommandResult) error {
	s.acks.Add(1)
	return nil
}

func (*startedReportLeaseLossSource) RenewLease(context.Context, string, controlplane.CommandLease) (controlplane.CommandLease, error) {
	return controlplane.CommandLease{}, errors.New("lease denied")
}

func TestLeaseLossStopsCommandReportRetry(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := &startedReportLeaseLossSource{
		daemonGateway: daemonGateway{heartbeats: make(chan struct{}, 1)},
	}
	daemon := Daemon{
		NodeID: "node_1", Source: source, Reporter: source,
		Executor: NewExecutor("node_1", store, &fakeWorkspace{}, &fakeRuntime{}),
	}
	command := testCommand(controlplane.RestoreAndStart, "start-report-loss", "node_1", "assignment_1")
	command.Lease = controlplane.CommandLease{
		Token: "lease", Generation: 1,
		ExpiresAt: time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano),
	}
	if err := daemon.Process(t.Context(), command); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("process error = %v, want lease loss", err)
	}
	if source.reports.Load() != 1 || source.acks.Load() != 0 {
		t.Fatalf("started reports/acks = %d/%d, want 1/0", source.reports.Load(), source.acks.Load())
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
