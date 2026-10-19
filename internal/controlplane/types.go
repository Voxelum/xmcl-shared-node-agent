package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"sync"
)

const (
	RestoreAndStart = "workspace.restore_and_start"
	StopAndSync     = "workspace.stop_and_sync"
	ContentPrepare  = "content.prepare"
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

// Connection is assigned by the control plane's durable per-node ingress
// reservation. Agents must not derive or substitute a host port locally.
type Connection struct {
	Host     string `json:"host"`
	HostPort int    `json:"hostPort"`
}

func (c Connection) Valid() bool {
	return validIngressHost(c.Host) && c.HostPort >= 1024 && c.HostPort <= 65535
}

type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (c Connection) Endpoint() Endpoint {
	return Endpoint{Host: c.Host, Port: c.HostPort}
}

type Command struct {
	CommandID    string                 `json:"commandId"`
	Kind         string                 `json:"kind"`
	NodeID       string                 `json:"nodeId"`
	ServiceID    string                 `json:"serviceId"`
	AssignmentID string                 `json:"assignmentId"`
	AccountID    string                 `json:"accountId"`
	Attempt      int                    `json:"attempt,omitempty"`
	Prepare      *ContentPrepareRequest `json:"prepare,omitempty"`
	Workspace    Workspace              `json:"workspace"`
	// RuntimeContent is an immutable compiler-owned content layer selected by
	// the control plane. It is never an image, command, environment, URL, or
	// storage credential supplied by a customer.
	RuntimeContent *WorkspaceBlob `json:"runtimeContent,omitempty"`
	// InitialWorld is an immutable, account-selected world seed. It is valid
	// only for revision zero and is obtained through one exact GET grant.
	InitialWorld *InitialWorld `json:"initialWorld,omitempty"`
	// EULAAccepted is set only by a server-side terms policy adapter. The agent
	// never derives it from customer content or an environment variable.
	EULAAccepted bool         `json:"eulaAccepted,omitempty"`
	Resources    Resources    `json:"resources"`
	Connection   *Connection  `json:"connection,omitempty"`
	Lease        CommandLease `json:"-"`
}

type ContentPrepareRequest struct {
	SchemaVersion        int                  `json:"schemaVersion"`
	ExpectedBuildKey     string               `json:"expectedBuildKey"`
	BuildSpec            BuildSpecV1          `json:"buildSpec"`
	Source               BuildSourceLocation  `json:"source"`
	ResolvedDependencies []ResolvedDependency `json:"resolvedDependencies"`
}

func (r *ContentPrepareRequest) UnmarshalJSON(data []byte) error {
	type plain ContentPrepareRequest
	var decoded plain
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	*r = ContentPrepareRequest(decoded)
	return nil
}

// BuildSpecV1 is Worker-owned canonical input. Storage locations and resolved
// URLs deliberately live outside it so equivalent bytes share one build key.
type BuildSpecV1 struct {
	Builder       BuildImageIdentity   `json:"builder"`
	Compatibility BuildCompatibility   `json:"compatibility"`
	Dependencies  []BuildDependency    `json:"dependencies"`
	Files         []BuildFile          `json:"files"`
	Runtime       RuntimeImageIdentity `json:"runtime"`
	SchemaVersion int                  `json:"schemaVersion"`
	Source        BuildSourceIdentity  `json:"source"`
	Target        BuildTarget          `json:"target"`
}

func (s *BuildSpecV1) UnmarshalJSON(data []byte) error {
	type plain BuildSpecV1
	var decoded plain
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	*s = BuildSpecV1(decoded)
	return nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON value has trailing data")
		}
		return err
	}
	return nil
}

type BuildSourceIdentity struct {
	Format string `json:"format"`
	SHA256 string `json:"sha256"`
}

type BuildSourceLocation struct {
	Key       string `json:"key"`
	SizeBytes int64  `json:"sizeBytes"`
}

type BuildCompatibility struct {
	Java             BuildJava   `json:"java"`
	Loader           BuildLoader `json:"loader"`
	MinecraftVersion string      `json:"minecraftVersion"`
}

type BuildJava struct {
	Component string `json:"component"`
	Major     int    `json:"major"`
}

type BuildLoader struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

type BuildFile struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"sizeBytes"`
	TargetPath string `json:"targetPath"`
}

type BuildDependency struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"sizeBytes"`
	TargetPath string `json:"targetPath"`
}

type ResolvedDependency struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type BuildImageIdentity struct {
	Image                    string `json:"image"`
	Schema                   string `json:"schema"`
	ToolchainCatalogRevision string `json:"toolchainCatalogRevision"`
}

type RuntimeImageIdentity struct {
	Image                  string `json:"image"`
	RuntimeCatalogRevision string `json:"runtimeCatalogRevision"`
}

type BuildTarget struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type InitialWorld struct {
	SeedID    string `json:"seedId"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
	WorldName string `json:"worldName"`
}

// CommandLease carries optional fields supplied by newer control planes.  The
// all v2 workspace grant requests require these fields.
type CommandLease struct {
	Token      string `json:"leaseToken,omitempty"`
	Generation int64  `json:"leaseGeneration,omitempty"`
	ExpiresAt  string `json:"leaseExpiresAt,omitempty"`
}

type SyncResult struct {
	ServiceID    string       `json:"serviceId"`
	AssignmentID string       `json:"assignmentId"`
	Revision     int64        `json:"revision"`
	SizeBytes    int64        `json:"sizeBytes"`
	ManifestSHA  string       `json:"manifestSha256"`
	CommandID    string       `json:"-"`
	Lease        CommandLease `json:"-"`
}

type StoppedReport struct {
	ServiceID    string
	AssignmentID string
	CommandID    string
	Lease        CommandLease
}

// WorkspaceBlob is an immutable compressed archive and its complete allowed
// extraction mapping. No storage credentials or arbitrary keys are represented.
type WorkspaceBlob struct {
	Key            string   `json:"key"`
	SHA256         string   `json:"sha256"`
	CompressedSize int64    `json:"compressedSize"`
	LogicalSize    int64    `json:"logicalSize"`
	Paths          []string `json:"paths"`
}

type WorkspaceManifest struct {
	SchemaVersion   int             `json:"schemaVersion"`
	ServiceID       string          `json:"serviceId"`
	AssignmentID    string          `json:"assignmentId"`
	Revision        int64           `json:"revision"`
	CreatedAt       string          `json:"createdAt"`
	LogicalSize     int64           `json:"logicalSize"`
	ManifestHash    string          `json:"manifestHash"`
	AggregateSHA256 string          `json:"aggregateSha256"`
	Content         *WorkspaceBlob  `json:"content,omitempty"`
	Config          *WorkspaceBlob  `json:"config,omitempty"`
	World           []WorkspaceBlob `json:"world"`
}

type WorkspaceGrant struct {
	Key       string            `json:"key"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	ExpiresAt string            `json:"expiresAt"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type WorkspaceGrantResponse struct {
	ContractVersion int              `json:"contractVersion"`
	Grants          []WorkspaceGrant `json:"grants"`
}

type BuildGrantRequest struct {
	ContractVersion int                      `json:"contractVersion"`
	CommandID       string                   `json:"commandId"`
	AssignmentID    string                   `json:"assignmentId"`
	LeaseToken      string                   `json:"leaseToken"`
	LeaseGeneration int64                    `json:"leaseGeneration"`
	Attempt         int                      `json:"attempt"`
	BuildKey        string                   `json:"buildKey"`
	Stage           string                   `json:"stage"`
	Keys            []string                 `json:"keys"`
	Manifest        *PreparedContentManifest `json:"manifest"`
	ManifestSHA256  string                   `json:"manifestSha256"`
}

type BuildGrantResponse struct {
	ContractVersion int              `json:"contractVersion"`
	CacheStatus     string           `json:"cacheStatus"`
	Grants          []WorkspaceGrant `json:"grants"`
}

type PreparedContentManifest struct {
	SchemaVersion int           `json:"schemaVersion"`
	BuildKey      string        `json:"buildKey"`
	BuildSpec     BuildSpecV1   `json:"buildSpec"`
	Content       WorkspaceBlob `json:"content"`
}

type PreparedContentResult struct {
	Cache          string                  `json:"cache"`
	BuildKey       string                  `json:"buildKey"`
	ManifestKey    string                  `json:"manifestKey"`
	ManifestSHA256 string                  `json:"manifestSha256"`
	Manifest       PreparedContentManifest `json:"manifest"`
	Content        WorkspaceBlob           `json:"content"`
}

type BuildGrantClient interface {
	BuildGrants(context.Context, Command, BuildGrantRequest) (BuildGrantResponse, error)
}

// WorkspaceGrantClient can issue only direct object URLs bound to a current
// command lease. It intentionally has no list, delete, or credential methods.
type WorkspaceGrantClient interface {
	RestoreWorkspaceGrants(context.Context, Command, string, []string) (WorkspaceGrantResponse, error)
	// SyncWorkspaceGrants prepares or resumes one immutable draft and returns
	// grants only for the requested bounded subset of manifest objects.
	SyncWorkspaceGrants(context.Context, Command, WorkspaceManifest, string, []string) (WorkspaceGrantResponse, error)
	PublishWorkspaceGrant(context.Context, Command, WorkspaceManifest, string) (WorkspaceGrantResponse, error)
}

type CommandResult struct {
	Status   string                 `json:"status"`
	Code     string                 `json:"code,omitempty"`
	Message  string                 `json:"message,omitempty"`
	Sync     *SyncResult            `json:"sync,omitempty"`
	Prepared *PreparedContentResult `json:"prepared,omitempty"`
}

type NodeCapacity struct {
	TotalMemoryMiB    int64 `json:"totalMemoryMiB"`
	TotalSharedCPU    int64 `json:"totalSharedCpu"`
	TotalWorkspaceGiB int64 `json:"totalWorkspaceGiB"`
}

type NodeStatus struct {
	NodeID              string               `json:"-"`
	ContractVersion     int                  `json:"contractVersion"`
	Status              string               `json:"status"`
	Capacity            AvailableCapacity    `json:"capacity"`
	Services            []ServiceStatus      `json:"services,omitempty"`
	AgentVersion        string               `json:"agentVersion"`
	Ingress             Ingress              `json:"ingress"`
	Platform            BuildTarget          `json:"platform"`
	Runtime             RuntimeImageIdentity `json:"runtime"`
	Builder             BuildImageIdentity   `json:"builder"`
	AvailableBuildSlots int64                `json:"availableBuildSlots"`
}

type ServiceStatus struct {
	ServiceID      string  `json:"serviceId"`
	AssignmentID   string  `json:"assignmentId"`
	CPUPercent     float64 `json:"cpuPercent"`
	MemoryUsageMiB int64   `json:"memoryUsageMiB"`
	MemoryLimitMiB int64   `json:"memoryLimitMiB"`
}

type AvailableCapacity struct {
	FreeWorkspaceGiB     int64 `json:"freeWorkspaceGiB"`
	AllocatableMemoryMiB int64 `json:"allocatableMemoryMiB"`
	AllocatableSharedCPU int64 `json:"allocatableSharedCpu"`
	ActiveContainerCount int64 `json:"activeContainerCount"`
}

type Ingress struct {
	Host string `json:"host"`
}

const SharedNodeContractVersion = 3
const WorkspaceGrantContractVersion = 2
const BuildGrantContractVersion = 3

var ingressHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
var runtimeImagePattern = regexp.MustCompile(`^ghcr\.io/voxelum/xmcl-shared-minecraft-runtime@sha256:[a-f0-9]{64}$`)
var builderImagePattern = regexp.MustCompile(`^ghcr\.io/voxelum/xmcl-shared-node-builder@sha256:[a-f0-9]{64}$`)
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s NodeStatus) Valid() bool {
	if s.ContractVersion != SharedNodeContractVersion ||
		(s.Status != "ready" && s.Status != "draining") ||
		s.Capacity.FreeWorkspaceGiB < 0 ||
		s.Capacity.AllocatableMemoryMiB < 0 ||
		s.Capacity.AllocatableSharedCPU < 0 ||
		s.Capacity.ActiveContainerCount < 0 ||
		s.AgentVersion == "" || len(s.AgentVersion) > 128 ||
		!validIngressHost(s.Ingress.Host) ||
		s.Platform.OS != "linux" || s.Platform.Architecture != "amd64" ||
		!runtimeImagePattern.MatchString(s.Runtime.Image) ||
		!sha256Pattern.MatchString(s.Runtime.RuntimeCatalogRevision) ||
		!builderImagePattern.MatchString(s.Builder.Image) ||
		s.Builder.Schema != "agent_builder_v1" ||
		!sha256Pattern.MatchString(s.Builder.ToolchainCatalogRevision) ||
		s.AvailableBuildSlots < 1 {
		return false
	}
	seen := make(map[string]struct{}, len(s.Services))
	for _, service := range s.Services {
		key := service.ServiceID + "\x00" + service.AssignmentID
		if service.ServiceID == "" || service.AssignmentID == "" ||
			math.IsNaN(service.CPUPercent) || math.IsInf(service.CPUPercent, 0) ||
			service.CPUPercent < 0 || service.CPUPercent > 10_000 ||
			service.MemoryUsageMiB < 0 || service.MemoryLimitMiB < 1 ||
			service.MemoryUsageMiB > service.MemoryLimitMiB {
			return false
		}
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func validIngressHost(host string) bool {
	return ingressHostPattern.MatchString(host)
}

type CommandSource interface {
	Next(ctx context.Context, nodeID string) (Command, error)
	Ack(ctx context.Context, commandID string, lease CommandLease, result CommandResult) error
}

type Reporter interface {
	Register(ctx context.Context, node NodeCapacity) error
	Heartbeat(ctx context.Context, status NodeStatus) error
	ReportStarted(ctx context.Context, serviceID, assignmentID string, endpoint Endpoint) error
	ReportStopped(ctx context.Context, report StoppedReport) error
	ReportStoppedAndSynced(ctx context.Context, result SyncResult) error
}

// LeaseRenewer is intentionally separate from CommandSource so existing
// sources remain compatible until the control plane enables lease renewal.
type LeaseRenewer interface {
	RenewLease(ctx context.Context, commandID string, lease CommandLease) (CommandLease, error)
}

var ErrTransportUnconfigured = errors.New("control-plane transport is not configured: install an authenticated mTLS transport adapter")

// UnconfiguredSource deliberately prevents an operator from accidentally using
// an invented or unauthenticated control-plane protocol.
type UnconfiguredSource struct{}

func (UnconfiguredSource) Next(context.Context, string) (Command, error) {
	return Command{}, ErrTransportUnconfigured
}
func (UnconfiguredSource) Ack(context.Context, string, CommandLease, CommandResult) error {
	return ErrTransportUnconfigured
}

type UnconfiguredReporter struct{}

func (UnconfiguredReporter) Register(context.Context, NodeCapacity) error {
	return ErrTransportUnconfigured
}
func (UnconfiguredReporter) Heartbeat(context.Context, NodeStatus) error {
	return ErrTransportUnconfigured
}
func (UnconfiguredReporter) ReportStarted(context.Context, string, string, Endpoint) error {
	return ErrTransportUnconfigured
}
func (UnconfiguredReporter) ReportStopped(context.Context, StoppedReport) error {
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
	Started  []StartedReport
	Stopped  []StoppedReport
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

func (g *MemoryGateway) Ack(_ context.Context, id string, _ CommandLease, result CommandResult) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Acks[id] = result
	return nil
}

func (g *MemoryGateway) Register(context.Context, NodeCapacity) error { return nil }
func (g *MemoryGateway) Heartbeat(context.Context, NodeStatus) error  { return nil }

type StartedReport struct {
	ServiceID    string
	AssignmentID string
	Endpoint     Endpoint
}

func (g *MemoryGateway) ReportStarted(_ context.Context, serviceID, assignmentID string, endpoint Endpoint) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Started = append(g.Started, StartedReport{ServiceID: serviceID, AssignmentID: assignmentID, Endpoint: endpoint})
	return nil
}

func (g *MemoryGateway) ReportStopped(_ context.Context, report StoppedReport) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Stopped = append(g.Stopped, report)
	return nil
}

func (g *MemoryGateway) ReportStoppedAndSynced(_ context.Context, result SyncResult) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Synced = append(g.Synced, result)
	return nil
}
