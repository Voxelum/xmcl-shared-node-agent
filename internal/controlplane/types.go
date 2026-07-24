package controlplane

import (
	"context"
	"errors"
	"sync"
)

const (
	RestoreAndStart = "workspace.restore_and_start"
	StopAndSync     = "workspace.stop_and_sync"
)

type Workspace struct {
	ObjectPrefix string `json:"objectPrefix"`
	Revision     int64  `json:"revision"`
	SizeBytes    int64  `json:"sizeBytes"`
	SHA256       string `json:"sha256,omitempty"`
	SyncedAt     string `json:"syncedAt,omitempty"`
}

type Resources struct {
	MemoryMiB    int64 `json:"memoryMiB"`
	SharedCPU    int64 `json:"sharedCpu"`
	BurstCPU     int64 `json:"burstCpu"`
	WorkspaceGiB int64 `json:"workspaceGiB"`
}

type Command struct {
	CommandID    string    `json:"commandId"`
	Kind         string    `json:"kind"`
	NodeID       string    `json:"nodeId"`
	ServiceID    string    `json:"serviceId"`
	AssignmentID string    `json:"assignmentId"`
	AccountID    string    `json:"accountId"`
	Workspace    Workspace `json:"workspace"`
	Resources    Resources `json:"resources"`
}

type SyncResult struct {
	ServiceID    string `json:"serviceId"`
	AssignmentID string `json:"assignmentId"`
	Revision     int64  `json:"revision"`
	SizeBytes    int64  `json:"sizeBytes"`
	ManifestSHA  string `json:"manifestSha256"`
}

type CommandResult struct {
	Status  string      `json:"status"`
	Message string      `json:"message,omitempty"`
	Sync    *SyncResult `json:"sync,omitempty"`
}

type NodeCapacity struct {
	TotalMemoryMiB    int64 `json:"totalMemoryMiB"`
	TotalSharedCPU    int64 `json:"totalSharedCpu"`
	TotalWorkspaceGiB int64 `json:"totalWorkspaceGiB"`
}

type NodeStatus struct {
	NodeID           string `json:"nodeId"`
	FreeWorkspaceGiB int64  `json:"freeWorkspaceGiB"`
	ActiveContainers int    `json:"activeContainers"`
	AgentVersion     string `json:"agentVersion"`
	Draining         bool   `json:"draining"`
	DrainReady       bool   `json:"drainReady"`
}

type CommandSource interface {
	Next(ctx context.Context, nodeID string) (Command, error)
	Ack(ctx context.Context, commandID string, result CommandResult) error
}

type Reporter interface {
	Register(ctx context.Context, node NodeCapacity) error
	Heartbeat(ctx context.Context, status NodeStatus) error
	ReportStarted(ctx context.Context, serviceID, assignmentID string) error
	ReportStoppedAndSynced(ctx context.Context, result SyncResult) error
}

var ErrTransportUnconfigured = errors.New("control-plane transport is not configured: install an authenticated mTLS transport adapter")

// UnconfiguredSource deliberately prevents an operator from accidentally using
// an invented or unauthenticated control-plane protocol.
type UnconfiguredSource struct{}

func (UnconfiguredSource) Next(context.Context, string) (Command, error) {
	return Command{}, ErrTransportUnconfigured
}
func (UnconfiguredSource) Ack(context.Context, string, CommandResult) error {
	return ErrTransportUnconfigured
}

type UnconfiguredReporter struct{}

func (UnconfiguredReporter) Register(context.Context, NodeCapacity) error {
	return ErrTransportUnconfigured
}
func (UnconfiguredReporter) Heartbeat(context.Context, NodeStatus) error {
	return ErrTransportUnconfigured
}
func (UnconfiguredReporter) ReportStarted(context.Context, string, string) error {
	return ErrTransportUnconfigured
}
func (UnconfiguredReporter) ReportStoppedAndSynced(context.Context, SyncResult) error {
	return ErrTransportUnconfigured
}

// MemoryGateway is a deterministic fake for unit tests and transport adapters.
type MemoryGateway struct {
	Commands chan Command
	mu       sync.Mutex
	Acks     map[string]CommandResult
	Started  [][2]string
	Synced   []SyncResult
}

func NewMemoryGateway(buffer int) *MemoryGateway {
	return &MemoryGateway{Commands: make(chan Command, buffer), Acks: make(map[string]CommandResult)}
}

func (g *MemoryGateway) Next(ctx context.Context, nodeID string) (Command, error) {
	for {
		select {
		case <-ctx.Done():
			return Command{}, ctx.Err()
		case command := <-g.Commands:
			if command.NodeID == nodeID {
				return command, nil
			}
		}
	}
}

func (g *MemoryGateway) Ack(_ context.Context, id string, result CommandResult) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Acks[id] = result
	return nil
}

func (g *MemoryGateway) Register(context.Context, NodeCapacity) error { return nil }
func (g *MemoryGateway) Heartbeat(context.Context, NodeStatus) error  { return nil }

func (g *MemoryGateway) ReportStarted(_ context.Context, serviceID, assignmentID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Started = append(g.Started, [2]string{serviceID, assignmentID})
	return nil
}

func (g *MemoryGateway) ReportStoppedAndSynced(_ context.Context, result SyncResult) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Synced = append(g.Synced, result)
	return nil
}
