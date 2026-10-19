package bootstraprunner

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/otlpconfig"
)

const (
	IsolatedTestTOFUPolicy       = "isolated_test_tofu_v1"
	AutoPinFreshAllocationPolicy = "auto_pin_fresh_allocation_v1"
	ManualApprovalPolicy         = "manual_approval_v1"

	maxAllocationFutureSkew = 2 * time.Minute
	maxAutoPinWindow        = 10 * time.Minute
	addressReuseCooldown    = 24 * time.Hour
)

type Request struct {
	OperationID              string                   `json:"operationId,omitempty"`
	AuthorizationID          string                   `json:"authorizationId,omitempty"`
	JobID                    string                   `json:"jobId"`
	NodeID                   string                   `json:"nodeId"`
	InstanceID               string                   `json:"instanceId"`
	Address                  string                   `json:"address"`
	SSHPort                  int                      `json:"sshPort"`
	EnrollmentToken          string                   `json:"enrollmentToken"`
	ExpectedProvider         ExpectedProvider         `json:"expectedProvider"`
	HostKeyPolicy            string                   `json:"hostKeyPolicy"`
	AllocationCreatedAt      string                   `json:"allocationCreatedAt"`
	AutoPinExpiresAt         string                   `json:"autoPinExpiresAt"`
	ExpectedProviderIdentity ExpectedProviderIdentity `json:"expectedProviderIdentity"`
	Config                   BootstrapConfig          `json:"config"`
	ExpectedCapacity         ExpectedCapacity         `json:"expectedCapacity"`
}

type ExpectedProviderIdentity struct {
	Provider          string `json:"provider"`
	InstanceID        string `json:"instanceId"`
	Address           string `json:"address"`
	SSHPort           int    `json:"sshPort"`
	InstanceName      string `json:"instanceName"`
	PackageCode       string `json:"packageCode"`
	RegionCode        string `json:"regionCode"`
	ZoneCode          string `json:"zoneCode"`
	ImageResourceUUID string `json:"imageResourceUUID"`
	FirewallUUID      string `json:"firewallUUID"`
	SSHKeyUUID        string `json:"sshKeyUUID"`
}

type ExpectedProvider struct {
	InstanceName      string `json:"instanceName"`
	PackageCode       string `json:"packageCode"`
	RegionCode        string `json:"regionCode"`
	ZoneCode          string `json:"zoneCode"`
	ImageResourceUUID string `json:"imageResourceUUID"`
	FirewallUUID      string `json:"firewallUUID"`
	SSHKeyUUID        string `json:"sshKeyUUID"`
}

type BootstrapConfig struct {
	ReleaseManifestURL    string `json:"releaseManifestUrl"`
	ReleaseManifestSHA256 string `json:"releaseManifestSha256"`
	DockerPackageVersion  string `json:"dockerPackageVersion"`
	ControlPlaneURL       string `json:"controlPlaneUrl"`
	LogicalRegion         string `json:"logicalRegion"`
	ObjectStorageEndpoint string `json:"objectStorageEndpoint"`
	ObjectStorageRegion   string `json:"objectStorageRegion"`
	ObjectStorageBucket   string `json:"objectStorageBucket"`
	OTLPEndpoint          string `json:"otlpEndpoint,omitempty"`
	OTLPHeaders           string `json:"otlpHeaders,omitempty"`
	WorkspaceVolumeGiB    int    `json:"workspaceVolumeGiB"`
	BootstrapTimeout      int    `json:"bootstrapTimeoutSeconds"`
}

type ExpectedCapacity struct {
	WorkloadClasses   []string `json:"workloadClasses"`
	TotalMemoryMiB    int      `json:"totalMemoryMiB"`
	TotalSharedCPU    int      `json:"totalSharedCpu"`
	TotalWorkspaceGiB int      `json:"totalWorkspaceGiB"`
}

type Executor interface {
	Probe(context.Context, string, int) (knownHosts, fingerprint string, err error)
	Apply(context.Context, string, int, string, string, string) error
}

type Handler struct {
	runningMu            sync.Mutex
	running              map[string]*runningJob
	cancelled            map[string]string
	testGrant            *TestTOFUAuthorization
	grantLookup          GrantLookup
	legacyFixtureAutoPin bool // set only by package-local inherited tests; never by exported constructors
	root                 string
	secret               string
	approvalSecret       string
	executor             Executor
	now                  func() time.Time
	afterPin             func() error
	afterApply           func() error
}

type jobState struct {
	Version                  int                      `json:"version"`
	JobID                    string                   `json:"jobId"`
	NodeID                   string                   `json:"nodeId"`
	InstanceID               string                   `json:"instanceId"`
	Address                  string                   `json:"address"`
	SSHPort                  int                      `json:"sshPort"`
	ExpectedProvider         ExpectedProvider         `json:"expectedProvider"`
	HostKeyPolicy            string                   `json:"hostKeyPolicy"`
	AllocationCreatedAt      string                   `json:"allocationCreatedAt"`
	AutoPinExpiresAt         string                   `json:"autoPinExpiresAt"`
	ExpectedProviderIdentity ExpectedProviderIdentity `json:"expectedProviderIdentity"`
	RequestDigest            string                   `json:"requestDigest"`
	Status                   string                   `json:"status"`
	ErrorCode                string                   `json:"errorCode,omitempty"`
	Retryable                bool                     `json:"retryable"`
	Terminal                 bool                     `json:"terminal"`
	FirstObservedAt          string                   `json:"firstObservedAt,omitempty"`
	KnownHosts               string                   `json:"knownHosts,omitempty"`
	Fingerprint              string                   `json:"fingerprint,omitempty"`
	Decision                 string                   `json:"decision,omitempty"`
	ApplyStarted             bool                     `json:"applyStarted"`
	ApplySucceeded           bool                     `json:"applySucceeded"`
	CompletedAt              string                   `json:"completedAt,omitempty"`
	RetiredAt                string                   `json:"retiredAt,omitempty"`
	AddressReuseAfter        string                   `json:"addressReuseAfter,omitempty"`
}

type allocationRegistry struct {
	Version  int                 `json:"version"`
	Bindings []allocationBinding `json:"bindings"`
}

type allocationBinding struct {
	JobID                   string `json:"jobId"`
	InstanceID              string `json:"instanceId"`
	Address                 string `json:"address"`
	SSHPort                 int    `json:"sshPort"`
	Provider                string `json:"provider"`
	PolicyName              string `json:"policyName"`
	RequestDigest           string `json:"requestDigest"`
	BoundAt                 string `json:"boundAt"`
	Status                  string `json:"status"`
	UpdatedAt               string `json:"updatedAt"`
	AddressReuseAfter       string `json:"addressReuseAfter,omitempty"`
	OperatorCleanupRequired bool   `json:"operatorCleanupRequired"`
	QuarantineReason        string `json:"quarantineReason,omitempty"`
	RetiredAt               string `json:"retiredAt,omitempty"`
}

var (
	allocationRegistryMu        sync.Mutex
	jobLocksMu                  sync.Mutex
	jobLocks                    = map[string]*sync.Mutex{}
	errInvalidHostKeyPolicy     = errors.New("invalid host key policy")
	errProviderIdentityMismatch = errors.New("provider identity mismatch")
	ErrEnrollmentRejected       = errors.New("enrollment rejected")
	ErrRetryableApply           = errors.New("retryable apply failure")
	errApplyOutcomeUnknown      = errors.New("apply outcome unknown")
)

func NewHandler(root, secret, approvalSecret string, executor Executor) (*Handler, error) {
	sameSecret := len(secret) == len(approvalSecret) &&
		subtle.ConstantTimeCompare([]byte(secret), []byte(approvalSecret)) == 1
	if root == "" || len(secret) < 32 || len(approvalSecret) < 32 ||
		sameSecret || executor == nil {
		return nil, errors.New("bootstrap runner configuration is invalid")
	}
	if err := os.MkdirAll(filepath.Join(root, "jobs"), 0700); err != nil {
		return nil, fmt.Errorf("create bootstrap job root: %w", err)
	}
	return &Handler{
		root: root, secret: secret, approvalSecret: approvalSecret,
		executor: executor, now: time.Now,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/bootstrap-jobs")
	if path == "" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "", "rejected", "method_not_allowed")
		} else if !authorized(r, h.secret) {
			writeError(w, http.StatusUnauthorized, "", "rejected", "unauthorized")
		} else {
			h.createOrResume(w, r)
		}
		return
	}
	if strings.HasSuffix(path, "/approve") {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "", "rejected", "method_not_allowed")
			return
		}
		if !authorized(r, h.approvalSecret) {
			writeError(w, http.StatusUnauthorized, "", "rejected", "unauthorized")
			return
		}
		h.approve(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/approve"))
		return
	}
	if strings.HasSuffix(path, "/retire") {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "", "failed", "method_not_allowed")
			return
		}
		if !authorized(r, h.approvalSecret) {
			writeError(w, http.StatusUnauthorized, "", "failed", "unauthorized")
			return
		}
		h.retire(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/retire"))
		return
	}
	if strings.HasSuffix(path, "/cancel") {
		if r.Method != http.MethodPost {
			writeError(w, 405, "", "rejected", "method_not_allowed")
			return
		}
		if !authorized(r, h.approvalSecret) {
			writeError(w, 401, "", "rejected", "unauthorized")
			return
		}
		h.cancelJob(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/cancel"))
		return
	}
	if strings.HasSuffix(path, "/reconcile") {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "", "rejected", "method_not_allowed")
			return
		}
		if !authorized(r, h.approvalSecret) {
			writeError(w, http.StatusUnauthorized, "", "rejected", "unauthorized")
			return
		}
		h.reconcile(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/reconcile"))
		return
	}
	if strings.HasSuffix(path, "/status") {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "", "rejected", "method_not_allowed")
			return
		}
		if !authorized(r, h.approvalSecret) {
			writeError(w, http.StatusUnauthorized, "", "rejected", "unauthorized")
			return
		}
		h.status(w, strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/status"))
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) createOrResume(w http.ResponseWriter, r *http.Request) {
	var input Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "", "rejected", "invalid_request")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, input.JobID, "rejected", "invalid_request")
		return
	}
	now := h.now()
	policyTimes, err := validateAt(input, now)
	if err != nil {
		switch {
		case errors.Is(err, errProviderIdentityMismatch):
			writeError(w, http.StatusConflict, input.JobID, "failed", "provider_identity_mismatch", false)
		case errors.Is(err, errInvalidHostKeyPolicy):
			writeError(w, http.StatusBadRequest, input.JobID, "failed", "invalid_host_key_policy", false)
		default:
			writeError(w, http.StatusBadRequest, input.JobID, "failed", "invalid_request", false)
		}
		return
	}
	if isAutoPin(input.HostKeyPolicy) && !h.authorizedTOFU(input) {
		writeError(w, http.StatusForbidden, input.JobID, "failed", "invalid_host_key_policy", false)
		return
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		writeError(w, http.StatusBadRequest, input.JobID, "rejected", "invalid_request")
		return
	}
	requestDigest := fmt.Sprintf("%x", sha256.Sum256(canonical))

	lock := h.jobLock(input.JobID)
	lock.Lock()
	defer lock.Unlock()
	var end func()
	var allowed bool
	r, end, allowed = h.beginRunning(r, input, requestDigest)
	if !allowed {
		writeError(w, 409, input.JobID, "failed", "terminal_apply_failure", false)
		return
	}
	defer end()

	if code, err := h.reserveAllocation(input, requestDigest, now); err != nil || code != "" {
		if code == "" {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
		} else {
			writeError(w, http.StatusConflict, input.JobID, "failed", code, false)
		}
		return
	}

	jobDir := filepath.Join(h.root, "jobs", input.JobID)
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
		return
	}
	statePath := filepath.Join(jobDir, "state.json")
	state, err := readJobState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		state = jobState{
			Version: 1, JobID: input.JobID, NodeID: input.NodeID,
			InstanceID: input.InstanceID, Address: input.Address, SSHPort: input.SSHPort,
			ExpectedProvider: input.ExpectedProvider, HostKeyPolicy: input.HostKeyPolicy,
			AllocationCreatedAt:      input.AllocationCreatedAt,
			AutoPinExpiresAt:         input.AutoPinExpiresAt,
			ExpectedProviderIdentity: input.ExpectedProviderIdentity,
			RequestDigest:            requestDigest,
			Status:                   "probing_host_key",
			FirstObservedAt:          canonicalTime(now),
		}
		if err = writeJobState(statePath, state); err == nil {
			err = writeSafeRequestFiles(jobDir, input, requestDigest)
		}
	} else if err == nil && state.RequestDigest != requestDigest {
		writeError(w, http.StatusConflict, input.JobID, "conflict", "idempotency_conflict")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
		return
	}
	if state.Status == "completed" {
		if err := os.Remove(filepath.Join(jobDir, "bootstrap.env")); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "failed", "runner_unavailable", true)
			return
		}
		if err := h.updateAllocation(input.JobID, "completed", "", h.now()); err != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "failed", "runner_unavailable", true)
			return
		}
		writeStatus(w, http.StatusOK, input.JobID, "completed", state.Fingerprint)
		return
	}
	if state.Status == "failed" && state.Terminal {
		writeStateError(w, http.StatusConflict, state, state.ErrorCode, false)
		return
	}
	if state.ApplySucceeded {
		h.finalizeSuccessfulApply(w, input, statePath, jobDir, state)
		return
	}
	if state.ApplyStarted {
		state.Status, state.ErrorCode = "failed", "apply_outcome_unknown"
		state.Retryable, state.Terminal = false, true
		if writeJobState(statePath, state) != nil ||
			h.updateAllocation(input.JobID, "failed", state.ErrorCode, h.now()) != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "failed", "runner_unavailable", true)
			return
		}
		writeStateError(w, http.StatusConflict, state, state.ErrorCode, false)
		return
	}
	if state.FirstObservedAt == "" {
		state.FirstObservedAt = canonicalTime(now)
		state.Status, state.ErrorCode = "probing_host_key", ""
		state.Retryable, state.Terminal = false, false
		if writeJobState(statePath, state) != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "failed", "runner_unavailable", true)
			return
		}
	}
	if isAutoPin(input.HostKeyPolicy) &&
		state.Fingerprint == "" && now.After(policyTimes.deadline) {
		state.Status, state.ErrorCode, state.Terminal = "failed", "auto_pin_deadline_expired", true
		state.Retryable = false
		if writeJobState(statePath, state) != nil ||
			h.updateAllocation(input.JobID, "failed", state.ErrorCode, h.now()) != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
			return
		}
		writeStateError(w, http.StatusConflict, state, state.ErrorCode, false)
		return
	}
	if state.FirstObservedAt != "" && isAutoPin(input.HostKeyPolicy) {
		firstObserved, parseErr := parseCanonicalTime(state.FirstObservedAt)
		if parseErr != nil || firstObserved.After(policyTimes.deadline) {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
			return
		}
	}

	knownHosts, observedFingerprint, probeErr := h.executor.Probe(
		r.Context(), input.Address, input.SSHPort,
	)
	if probeErr != nil || !validProbe(
		knownHosts, observedFingerprint, input.Address, input.SSHPort,
	) {
		state.Status, state.ErrorCode, state.Retryable = "failed", "host_key_probe_failed", true
		state.Terminal = false
		if writeJobState(statePath, state) != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
			return
		}
		writeStateError(w, http.StatusServiceUnavailable, state, state.ErrorCode, true)
		return
	}
	knownHosts = strings.TrimSpace(knownHosts)
	observedFingerprint = strings.TrimSpace(observedFingerprint)

	if isAutoPin(input.HostKeyPolicy) &&
		state.Fingerprint == "" && h.now().After(policyTimes.deadline) {
		state.Status, state.ErrorCode, state.Terminal = "failed", "auto_pin_deadline_expired", true
		state.Retryable = false
		if writeJobState(statePath, state) != nil ||
			h.updateAllocation(input.JobID, "failed", state.ErrorCode, h.now()) != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "failed", "runner_unavailable", true)
			return
		}
		writeStateError(w, http.StatusConflict, state, state.ErrorCode, false)
		return
	}

	if state.Fingerprint != "" &&
		(state.Fingerprint != observedFingerprint || state.KnownHosts != knownHosts) {
		state.Status, state.Decision, state.ErrorCode = "failed", "quarantined", "host_key_changed"
		state.Retryable, state.Terminal = false, true
		if writeJobState(statePath, state) != nil ||
			h.updateAllocation(input.JobID, "failed", "host_key_changed", h.now()) != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
			return
		}
		writeStateError(w, http.StatusConflict, state, state.ErrorCode, false)
		return
	}

	if state.Fingerprint == "" {
		state.KnownHosts, state.Fingerprint = knownHosts, observedFingerprint
		if isAutoPin(input.HostKeyPolicy) {
			state.Decision, state.Status = "auto_approved", "host_key_auto_pinned"
		} else {
			state.Decision, state.Status = "pending", "awaiting_host_key_approval"
		}
		state.ErrorCode = ""
		if writeJobState(statePath, state) != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
			return
		}
		if h.afterPin != nil {
			if err := h.afterPin(); err != nil {
				writeError(
					w, http.StatusServiceUnavailable, input.JobID,
					state.Status, "runner_unavailable", true,
				)
				return
			}
		}
	}

	if input.HostKeyPolicy == ManualApprovalPolicy && state.Decision != "approved" {
		writeStatus(
			w, http.StatusAccepted, input.JobID,
			"awaiting_host_key_approval", state.Fingerprint,
		)
		return
	}
	h.apply(w, r, input, statePath, jobDir, state)
}

type parsedPolicyTimes struct {
	created  time.Time
	deadline time.Time
}

func validateAt(input Request, now time.Time) (parsedPolicyTimes, error) {
	if err := validateCommon(input); err != nil {
		return parsedPolicyTimes{}, err
	}
	if input.SSHPort < 1 || input.SSHPort > 65535 || input.SSHPort != 22 {
		return parsedPolicyTimes{}, errInvalidHostKeyPolicy
	}
	identity := ExpectedProviderIdentity{
		Provider: "lightnode", InstanceID: input.InstanceID,
		Address: input.Address, SSHPort: input.SSHPort,
		InstanceName:      input.ExpectedProvider.InstanceName,
		PackageCode:       input.ExpectedProvider.PackageCode,
		RegionCode:        input.ExpectedProvider.RegionCode,
		ZoneCode:          input.ExpectedProvider.ZoneCode,
		ImageResourceUUID: input.ExpectedProvider.ImageResourceUUID,
		FirewallUUID:      input.ExpectedProvider.FirewallUUID,
		SSHKeyUUID:        input.ExpectedProvider.SSHKeyUUID,
	}
	if input.ExpectedProviderIdentity != identity {
		return parsedPolicyTimes{}, errProviderIdentityMismatch
	}
	switch input.HostKeyPolicy {
	case AutoPinFreshAllocationPolicy, IsolatedTestTOFUPolicy:
		created, err := parseCanonicalTime(input.AllocationCreatedAt)
		if err != nil {
			return parsedPolicyTimes{}, fmt.Errorf("%w: %v", errInvalidHostKeyPolicy, err)
		}
		deadline, err := parseCanonicalTime(input.AutoPinExpiresAt)
		if err != nil {
			return parsedPolicyTimes{}, fmt.Errorf("%w: %v", errInvalidHostKeyPolicy, err)
		}
		if created.After(now.Add(maxAllocationFutureSkew)) ||
			!deadline.After(created) || deadline.Sub(created) > maxAutoPinWindow {
			return parsedPolicyTimes{}, errInvalidHostKeyPolicy
		}
		return parsedPolicyTimes{created: created, deadline: deadline}, nil
	case ManualApprovalPolicy:
		created, err := parseCanonicalTime(input.AllocationCreatedAt)
		if err != nil {
			return parsedPolicyTimes{}, fmt.Errorf("%w: %v", errInvalidHostKeyPolicy, err)
		}
		deadline, err := parseCanonicalTime(input.AutoPinExpiresAt)
		if err != nil {
			return parsedPolicyTimes{}, fmt.Errorf("%w: %v", errInvalidHostKeyPolicy, err)
		}
		if created.After(now.Add(maxAllocationFutureSkew)) ||
			!deadline.After(created) || deadline.Sub(created) > maxAutoPinWindow {
			return parsedPolicyTimes{}, errInvalidHostKeyPolicy
		}
		return parsedPolicyTimes{created: created, deadline: deadline}, nil
	default:
		return parsedPolicyTimes{}, errInvalidHostKeyPolicy
	}
}

func validate(input Request) error {
	_, err := validateAt(input, time.Now())
	return err
}

func validateCommon(input Request) error {
	address, addressErr := netip.ParseAddr(input.Address)
	if !identifier.MatchString(input.JobID) || !identifier.MatchString(input.NodeID) ||
		!identifier.MatchString(input.InstanceID) || addressErr != nil ||
		!isPublicCanonicalIPv4(address, input.Address) ||
		!regexp.MustCompile(`^[A-Za-z0-9_-]{32,512}$`).MatchString(input.EnrollmentToken) ||
		!sha256Hex.MatchString(input.Config.ReleaseManifestSHA256) ||
		input.Config.LogicalRegion != "tpe" ||
		input.Config.ObjectStorageRegion != "azure" ||
		input.ExpectedProvider.RegionCode != "cn-taiwan-2" ||
		input.Config.WorkspaceVolumeGiB < input.ExpectedCapacity.TotalWorkspaceGiB ||
		input.Config.BootstrapTimeout < 60 || input.Config.BootstrapTimeout > 1800 ||
		input.ExpectedCapacity.TotalMemoryMiB <= 0 ||
		input.ExpectedCapacity.TotalSharedCPU <= 0 ||
		input.ExpectedCapacity.TotalWorkspaceGiB <= 0 {
		return errors.New("invalid bootstrap request")
	}
	for _, value := range []string{
		input.ExpectedProvider.InstanceName, input.ExpectedProvider.PackageCode,
		input.ExpectedProvider.RegionCode, input.ExpectedProvider.ZoneCode,
		input.ExpectedProvider.ImageResourceUUID, input.ExpectedProvider.FirewallUUID,
		input.ExpectedProvider.SSHKeyUUID,
	} {
		if !identifier.MatchString(value) {
			return errors.New("invalid expected provider identity")
		}
	}
	for _, value := range []string{
		input.Config.ReleaseManifestURL, input.Config.ControlPlaneURL,
		input.Config.ObjectStorageEndpoint,
	} {
		if !strings.HasPrefix(value, "https://") {
			return errors.New("bootstrap URLs must use HTTPS")
		}
	}
	if input.Config.OTLPEndpoint == "" {
		if input.Config.OTLPHeaders != "" {
			return errors.New("OTLP headers require an endpoint")
		}
	} else {
		if err := otlpconfig.ValidateEndpoint(input.Config.OTLPEndpoint); err != nil {
			return err
		}
		if _, err := otlpconfig.ParseHeaders(input.Config.OTLPHeaders); err != nil {
			return err
		}
	}
	return nil
}

func isPublicCanonicalIPv4(address netip.Addr, raw string) bool {
	if !address.Is4() || address.String() != raw || !address.IsGlobalUnicast() ||
		address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range nonPublicIPv4Prefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func (h *Handler) apply(
	w http.ResponseWriter,
	r *http.Request,
	input Request,
	statePath, jobDir string,
	state jobState,
) {
	state.Status, state.ErrorCode, state.ApplyStarted = "applying", "", false
	state.Retryable, state.Terminal = false, false
	if err := writeJobState(statePath, state); err != nil {
		writeError(w, http.StatusServiceUnavailable, input.JobID, "failed", "runner_unavailable", true)
		return
	}
	knownHostsPath := filepath.Join(jobDir, "known_hosts")
	if err := writePrivate(knownHostsPath, []byte(state.KnownHosts+"\n")); err != nil {
		writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
		return
	}
	envPath := filepath.Join(jobDir, "bootstrap.env")
	if err := writePrivate(envPath, []byte(renderEnvironment(input))); err != nil {
		writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
		return
	}
	state.ApplyStarted = true
	if err := writeJobState(statePath, state); err != nil {
		_ = os.Remove(envPath)
		writeError(w, http.StatusServiceUnavailable, input.JobID, "failed", "runner_unavailable", true)
		return
	}
	applyErr := h.executor.Apply(
		r.Context(), input.Address, input.SSHPort,
		knownHostsPath, state.Fingerprint, envPath,
	)
	if applyErr == nil && h.afterApply != nil {
		if err := h.afterApply(); err != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "applying", "runner_unavailable", true)
			return
		}
	}
	if applyErr != nil {
		removeErr := os.Remove(envPath)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "failed", "runner_unavailable", true)
			return
		}
		state.Status = "failed"
		state.ApplyStarted = false
		switch {
		case errors.Is(applyErr, ErrEnrollmentRejected):
			state.ErrorCode, state.Retryable, state.Terminal = "enrollment_rejected", false, true
		case errors.Is(applyErr, ErrRetryableApply):
			state.ErrorCode, state.Retryable, state.Terminal = "apply_failed", true, false
		case errors.Is(applyErr, errApplyOutcomeUnknown):
			state.ErrorCode, state.Retryable, state.Terminal = "apply_outcome_unknown", false, true
		default:
			state.ErrorCode, state.Retryable, state.Terminal = "terminal_apply_failure", false, true
		}
		if writeJobState(statePath, state) != nil {
			writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
			return
		}
		status := http.StatusConflict
		if state.Retryable {
			status = http.StatusServiceUnavailable
		}
		writeStateError(w, status, state, state.ErrorCode, state.Retryable)
		return
	}
	state.Status, state.ErrorCode = "applying", ""
	state.ApplyStarted, state.ApplySucceeded = false, true
	state.Retryable, state.Terminal = false, false
	if writeJobState(statePath, state) != nil {
		_ = os.Remove(envPath)
		writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
		return
	}
	h.finalizeSuccessfulApply(w, input, statePath, jobDir, state)
}

func (h *Handler) finalizeSuccessfulApply(
	w http.ResponseWriter,
	input Request,
	statePath, jobDir string,
	state jobState,
) {
	if !reconcilableCompletion(state) {
		writeError(w, http.StatusConflict, state.JobID, "failed", "apply_outcome_unknown", false)
		return
	}
	envPath := filepath.Join(jobDir, "bootstrap.env")
	if err := os.Remove(envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.writeFinalizationFailure(w, statePath, state)
		return
	}
	if err := writeSafeRequestFiles(jobDir, input, state.RequestDigest); err != nil {
		h.writeFinalizationFailure(w, statePath, state)
		return
	}
	if err := h.updateAllocation(input.JobID, "completed", "", h.now()); err != nil {
		h.writeFinalizationFailure(w, statePath, state)
		return
	}
	// Completed is a durable fact. Metadata repair must never rewrite or demote
	// it; only an applying first-finalization may transition the job state.
	if state.Status == "completed" {
		writeStatus(w, http.StatusOK, input.JobID, "completed", state.Fingerprint)
		return
	}
	state.Status, state.ErrorCode = "completed", ""
	if state.CompletedAt == "" {
		state.CompletedAt = canonicalTime(h.now())
	}
	state.Retryable, state.Terminal = false, true
	if err := writeJobState(statePath, state); err != nil {
		writeError(w, http.StatusServiceUnavailable, input.JobID, "unavailable", "runner_unavailable")
		return
	}
	writeStatus(w, http.StatusOK, input.JobID, "completed", state.Fingerprint)
}

func (h *Handler) writeFinalizationFailure(
	w http.ResponseWriter,
	statePath string,
	state jobState,
) {
	if state.Status == "completed" {
		writeResult(w, http.StatusServiceUnavailable, state.JobID, "completed", "runner_unavailable", false, state.Fingerprint)
		return
	}
	state.Status, state.ErrorCode, state.Retryable = "applying", "runner_unavailable", true
	state.Terminal = false
	_ = writeJobState(statePath, state)
	writeResult(
		w, http.StatusServiceUnavailable, state.JobID, state.Status,
		state.ErrorCode, true, state.Fingerprint,
	)
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request, jobID string) {
	if !identifier.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, jobID, "rejected", "invalid_request")
		return
	}
	var input struct {
		Fingerprint string `json:"fingerprint"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, jobID, "rejected", "invalid_request")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, jobID, "rejected", "invalid_request")
		return
	}
	lock := h.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	statePath := filepath.Join(h.root, "jobs", jobID, "state.json")
	state, err := readJobState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, jobID, "not_found", "job_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, jobID, "unavailable", "runner_unavailable")
		return
	}
	if state.HostKeyPolicy != ManualApprovalPolicy {
		writeError(w, http.StatusConflict, jobID, "failed", "invalid_host_key_policy", false)
		return
	}
	if state.Status == "retired" {
		writeError(w, http.StatusConflict, jobID, "failed", "allocation_retired", false)
		return
	}
	if state.Status == "completed" {
		writeStatus(w, http.StatusOK, jobID, "completed", state.Fingerprint)
		return
	}
	if state.Status == "failed" && state.Terminal {
		writeStateError(w, http.StatusConflict, state, state.ErrorCode, false)
		return
	}
	if state.Fingerprint == "" || subtle.ConstantTimeCompare(
		[]byte(state.Fingerprint), []byte(input.Fingerprint),
	) != 1 {
		writeError(w, http.StatusConflict, jobID, "failed", "host_key_conflict", false)
		return
	}
	state.Decision, state.Status, state.ErrorCode = "approved", "approved", ""
	state.Retryable, state.Terminal = false, false
	if err := writeJobState(statePath, state); err != nil {
		writeError(w, http.StatusServiceUnavailable, jobID, "unavailable", "runner_unavailable")
		return
	}
	writeStatus(w, http.StatusOK, jobID, "approved", state.Fingerprint)
}

type retirementRequest struct {
	InstanceID string `json:"instanceId"`
	Address    string `json:"address"`
	SSHPort    int    `json:"sshPort"`
}

func (h *Handler) retire(w http.ResponseWriter, r *http.Request, jobID string) {
	if !identifier.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, jobID, "failed", "invalid_request", false)
		return
	}
	input, err := decodeRetirementRequest(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, jobID, "failed", "invalid_request", false)
		return
	}
	address, addressErr := netip.ParseAddr(input.Address)
	if !identifier.MatchString(input.InstanceID) || addressErr != nil ||
		!isPublicCanonicalIPv4(address, input.Address) || input.SSHPort != 22 {
		writeError(w, http.StatusBadRequest, jobID, "failed", "invalid_request", false)
		return
	}

	lock := h.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	statePath := filepath.Join(h.root, "jobs", jobID, "state.json")
	state, err := readJobState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, jobID, "failed", "job_not_found", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, jobID, "failed", "runner_unavailable", true)
		return
	}
	if state.JobID != jobID || input.InstanceID != state.InstanceID ||
		input.Address != state.Address ||
		input.SSHPort != state.SSHPort {
		writeError(w, http.StatusConflict, jobID, "failed", "retirement_identity_mismatch", false)
		return
	}
	if state.Status != "retired" &&
		state.Status != "completed" &&
		!(state.Status == "failed" && state.Terminal) {
		writeError(w, http.StatusConflict, jobID, "failed", "allocation_not_terminal", false)
		return
	}

	allocationRegistryMu.Lock()
	defer allocationRegistryMu.Unlock()
	registry, err := h.readRegistry()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, jobID, "failed", "runner_unavailable", true)
		return
	}
	index := -1
	for i := range registry.Bindings {
		if registry.Bindings[i].JobID == jobID {
			index = i
			break
		}
	}
	if index == -1 {
		writeError(w, http.StatusServiceUnavailable, jobID, "failed", "runner_unavailable", true)
		return
	}
	binding := &registry.Bindings[index]
	if input.InstanceID != binding.InstanceID || input.Address != binding.Address ||
		input.SSHPort != binding.SSHPort {
		writeError(w, http.StatusConflict, jobID, "failed", "retirement_identity_mismatch", false)
		return
	}
	if binding.Status != "retired" {
		retirementTime := h.now()
		retiredAt := canonicalTime(retirementTime)
		binding.Status = "retired"
		binding.RetiredAt = retiredAt
		binding.UpdatedAt = retiredAt
		binding.AddressReuseAfter = canonicalTime(retirementTime.Add(addressReuseCooldown))
		binding.OperatorCleanupRequired = false
		if err := atomicWriteJSON(filepath.Join(h.root, "allocation-registry.json"), registry); err != nil {
			writeError(w, http.StatusServiceUnavailable, jobID, "failed", "runner_unavailable", true)
			return
		}
	}
	if binding.RetiredAt == "" || binding.AddressReuseAfter == "" {
		writeError(w, http.StatusServiceUnavailable, jobID, "failed", "runner_unavailable", true)
		return
	}
	state.Status = "retired"
	state.ErrorCode = ""
	state.Retryable = false
	state.Terminal = true
	state.RetiredAt = binding.RetiredAt
	state.AddressReuseAfter = binding.AddressReuseAfter
	if err := writeJobState(statePath, state); err != nil {
		writeError(w, http.StatusServiceUnavailable, jobID, "failed", "runner_unavailable", true)
		return
	}
	writeRetirementResult(w, state)
}

func decodeRetirementRequest(reader io.Reader) (retirementRequest, error) {
	var input retirementRequest
	decoder := json.NewDecoder(reader)
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return input, errors.New("retirement request must be an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return input, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return input, errors.New("retirement request contains an invalid key")
		}
		seen[key] = true
		switch key {
		case "instanceId":
			err = decoder.Decode(&input.InstanceID)
		case "address":
			err = decoder.Decode(&input.Address)
		case "sshPort":
			err = decoder.Decode(&input.SSHPort)
		default:
			return input, errors.New("retirement request contains an unknown key")
		}
		if err != nil {
			return input, err
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') ||
		!seen["instanceId"] || !seen["address"] || !seen["sshPort"] {
		return input, errors.New("retirement request is incomplete")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return input, errors.New("retirement request contains trailing data")
	}
	return input, nil
}

func (h *Handler) status(w http.ResponseWriter, jobID string) {
	if !identifier.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, jobID, "rejected", "invalid_request")
		return
	}
	state, err := readJobState(filepath.Join(h.root, "jobs", jobID, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, jobID, "not_found", "job_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, jobID, "unavailable", "runner_unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		JobID                    string                   `json:"jobId"`
		Status                   string                   `json:"status"`
		ErrorCode                string                   `json:"errorCode,omitempty"`
		Retryable                bool                     `json:"retryable"`
		Fingerprint              string                   `json:"fingerprint,omitempty"`
		NodeID                   string                   `json:"nodeId"`
		InstanceID               string                   `json:"instanceId"`
		Address                  string                   `json:"address"`
		SSHPort                  int                      `json:"sshPort"`
		ExpectedProvider         ExpectedProvider         `json:"expectedProvider"`
		HostKeyPolicy            string                   `json:"hostKeyPolicy"`
		AllocationCreatedAt      string                   `json:"allocationCreatedAt"`
		AutoPinExpiresAt         string                   `json:"autoPinExpiresAt"`
		ExpectedProviderIdentity ExpectedProviderIdentity `json:"expectedProviderIdentity"`
		FirstObservedAt          string                   `json:"firstObservedAt,omitempty"`
		RetiredAt                string                   `json:"retiredAt,omitempty"`
		AddressReuseAfter        string                   `json:"addressReuseAfter,omitempty"`
	}{
		JobID: state.JobID, Status: state.Status, ErrorCode: state.ErrorCode,
		Retryable:   state.Retryable,
		Fingerprint: state.Fingerprint, NodeID: state.NodeID,
		InstanceID: state.InstanceID, Address: state.Address, SSHPort: state.SSHPort,
		ExpectedProvider: state.ExpectedProvider, HostKeyPolicy: state.HostKeyPolicy,
		AllocationCreatedAt:      state.AllocationCreatedAt,
		AutoPinExpiresAt:         state.AutoPinExpiresAt,
		ExpectedProviderIdentity: state.ExpectedProviderIdentity,
		FirstObservedAt:          state.FirstObservedAt,
		RetiredAt:                state.RetiredAt,
		AddressReuseAfter:        state.AddressReuseAfter,
	})
}

func (h *Handler) reserveAllocation(input Request, digest string, now time.Time) (string, error) {
	allocationRegistryMu.Lock()
	defer allocationRegistryMu.Unlock()
	registry, err := h.readRegistry()
	if err != nil {
		return "", err
	}
	for _, binding := range registry.Bindings {
		if binding.JobID == input.JobID {
			if binding.Status == "retired" {
				return "allocation_retired", nil
			}
			if binding.InstanceID != input.InstanceID {
				return "provider_identity_mismatch", nil
			}
			if binding.Address != input.Address || binding.SSHPort != input.SSHPort ||
				binding.RequestDigest != digest {
				return "idempotency_conflict", nil
			}
			return "", nil
		}
		if binding.InstanceID == input.InstanceID {
			return "provider_identity_mismatch", nil
		}
		if binding.Address == input.Address {
			if binding.Status != "retired" {
				return "provider_identity_mismatch", nil
			}
			reuseAfter, parseErr := parseCanonicalTime(binding.AddressReuseAfter)
			if parseErr != nil {
				return "", errors.New("invalid retired allocation cooldown")
			}
			if now.Before(reuseAfter) {
				return "provider_identity_mismatch", nil
			}
		}
	}
	at := canonicalTime(now)
	registry.Bindings = append(registry.Bindings, allocationBinding{
		JobID: input.JobID, InstanceID: input.InstanceID, Address: input.Address,
		SSHPort:  input.SSHPort,
		Provider: input.ExpectedProviderIdentity.Provider, PolicyName: input.HostKeyPolicy,
		RequestDigest: digest, BoundAt: at, UpdatedAt: at, Status: "probing_host_key",
		AddressReuseAfter:       canonicalTime(now.Add(addressReuseCooldown)),
		OperatorCleanupRequired: true,
	})
	return "", atomicWriteJSON(filepath.Join(h.root, "allocation-registry.json"), registry)
}

func (h *Handler) readRegistry() (allocationRegistry, error) {
	var registry allocationRegistry
	value, err := os.ReadFile(filepath.Join(h.root, "allocation-registry.json"))
	if errors.Is(err, os.ErrNotExist) {
		return allocationRegistry{Version: 1, Bindings: []allocationBinding{}}, nil
	}
	if err != nil {
		return registry, err
	}
	if err := json.Unmarshal(value, &registry); err != nil || registry.Version != 1 {
		return registry, errors.New("invalid allocation registry")
	}
	return registry, nil
}

func (h *Handler) updateAllocation(jobID, status, reason string, now time.Time) error {
	allocationRegistryMu.Lock()
	defer allocationRegistryMu.Unlock()
	registry, err := h.readRegistry()
	if err != nil {
		return err
	}
	for i := range registry.Bindings {
		if registry.Bindings[i].JobID != jobID {
			continue
		}
		if registry.Bindings[i].Status == status &&
			registry.Bindings[i].QuarantineReason == reason {
			return nil
		}
		if registry.Bindings[i].Status == "retired" {
			return errors.New("retired allocation cannot be updated")
		}
		registry.Bindings[i].Status = status
		registry.Bindings[i].UpdatedAt = canonicalTime(now)
		registry.Bindings[i].AddressReuseAfter = canonicalTime(now.Add(addressReuseCooldown))
		registry.Bindings[i].OperatorCleanupRequired = true
		registry.Bindings[i].QuarantineReason = reason
		return atomicWriteJSON(filepath.Join(h.root, "allocation-registry.json"), registry)
	}
	return errors.New("allocation binding is missing")
}

func (h *Handler) jobLock(jobID string) *sync.Mutex {
	key := filepath.Clean(h.root) + "\x00" + jobID
	jobLocksMu.Lock()
	defer jobLocksMu.Unlock()
	if jobLocks[key] == nil {
		jobLocks[key] = &sync.Mutex{}
	}
	return jobLocks[key]
}

func readJobState(path string) (jobState, error) {
	var state jobState
	value, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(value, &state); err != nil || state.Version != 1 {
		return state, errors.New("invalid bootstrap job state")
	}
	return state, nil
}

func writeJobState(path string, state jobState) error {
	return atomicWriteJSON(path, state)
}

func writeSafeRequestFiles(jobDir string, input Request, digest string) error {
	input.EnrollmentToken = ""
	input.Config.OTLPHeaders = ""
	redacted, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(jobDir, "request.json"), redacted); err != nil {
		return err
	}
	return writePrivate(filepath.Join(jobDir, "request.sha256"), []byte(digest+"\n"))
}

func atomicWriteJSON(path string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writePrivate(path, append(encoded, '\n'))
}

func writePrivate(path string, value []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func parseCanonicalTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || parsed.UTC().Format(time.RFC3339) != value {
		return time.Time{}, errors.New("timestamp is not canonical UTC RFC3339")
	}
	return parsed, nil
}

func canonicalTime(value time.Time) string {
	return value.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func validProbe(knownHosts, value, address string, port int) bool {
	if knownHosts == "" || knownHosts != strings.TrimSpace(knownHosts) ||
		strings.ContainsAny(knownHosts, "\r\n") {
		return false
	}
	fields := strings.Fields(knownHosts)
	expectedHost := address
	if port != 22 {
		expectedHost = fmt.Sprintf("[%s]:%d", address, port)
	}
	return len(fields) == 3 &&
		fields[0] == expectedHost &&
		fields[1] == "ssh-ed25519" &&
		base64Key.MatchString(fields[2]) &&
		fingerprint.MatchString(strings.TrimSpace(value))
}

type ShellExecutor struct {
	RunnerScript           string
	BootstrapScript        string
	ServiceFile            string
	PrivateKey             string
	ManagedPublicKey       string
	ManagedPublicKeySHA256 string
	SSHUser                string
	Port                   int
}

func (e ShellExecutor) Probe(ctx context.Context, address string, port int) (string, string, error) {
	if e.Port != 0 && e.Port != port {
		return "", "", errors.New("request SSH port does not match executor configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, e.RunnerScript, "probe")
	command.WaitDelay = 5 * time.Second
	cleanup, processErr := configureJobProcess(command)
	if processErr != nil {
		return "", "", processErr
	}
	defer cleanup()
	command.Env = append(os.Environ(),
		"XMCL_LIGHTNODE_HOST="+address,
		"XMCL_LIGHTNODE_PORT="+strconv.Itoa(port),
	)
	output, err := command.Output()
	if err != nil {
		return "", "", fmt.Errorf("probe SSH host key: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 {
		return "", "", errors.New("probe returned an invalid host key record")
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 || !fingerprint.MatchString(fields[1]) {
		return "", "", errors.New("probe returned an invalid fingerprint")
	}
	return lines[0], fields[1], nil
}

func (e ShellExecutor) Apply(
	ctx context.Context,
	address string,
	port int,
	knownHosts, hostFingerprint, envPath string,
) error {
	if e.Port != 0 && e.Port != port {
		return errors.New("request SSH port does not match executor configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, e.RunnerScript, "apply")
	command.WaitDelay = 5 * time.Second
	cleanup, processErr := configureJobProcess(command)
	if processErr != nil {
		return processErr
	}
	defer cleanup()
	command.Env = append(os.Environ(),
		"XMCL_LIGHTNODE_HOST="+address,
		"XMCL_LIGHTNODE_PORT="+strconv.Itoa(port),
		"XMCL_LIGHTNODE_USER="+e.SSHUser,
		"XMCL_LIGHTNODE_PRIVATE_KEY="+e.PrivateKey,
		"XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY="+e.ManagedPublicKey,
		"XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY_SHA256="+e.ManagedPublicKeySHA256,
		"XMCL_LIGHTNODE_KNOWN_HOSTS="+knownHosts,
		"XMCL_LIGHTNODE_HOST_KEY_SHA256="+hostFingerprint,
		"XMCL_LIGHTNODE_BOOTSTRAP_ENV="+envPath,
		"XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT="+e.BootstrapScript,
		"XMCL_LIGHTNODE_AGENT_SERVICE="+e.ServiceFile,
	)
	if _, err := command.CombinedOutput(); err != nil {
		return classifyApplyCommandFailure(err, ctx.Err())
	}
	return nil
}

func classifyApplyCommandFailure(err, contextErr error) error {
	// A wait timeout is not proof that the remote setup did not execute, even
	// when the direct child exited and only inherited output pipes remain open.
	if contextErr != nil || errors.Is(err, exec.ErrWaitDelay) {
		return fmt.Errorf("%w: command wait did not complete", errApplyOutcomeUnknown)
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return classifyApplyExit(exitError.ExitCode())
	}
	return fmt.Errorf("apply bootstrap failed")
}

func classifyApplyExit(code int) error {
	switch code {
	case 75:
		return fmt.Errorf("%w: SSH transport failed", ErrRetryableApply)
	case 76:
		return errApplyOutcomeUnknown
	case 77:
		return ErrEnrollmentRejected
	default:
		return fmt.Errorf("apply bootstrap failed")
	}
}

var (
	identifier            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	sha256Hex             = regexp.MustCompile(`^[a-f0-9]{64}$`)
	fingerprint           = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	base64Key             = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
	nonPublicIPv4Prefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
)

func renderEnvironment(input Request) string {
	values := [][2]string{
		{"XMCL_NODE_ID", input.NodeID},
		{"XMCL_INSTANCE_ID", input.InstanceID},
		{"XMCL_REGION", input.Config.LogicalRegion},
		{"XMCL_CONTROL_PLANE_URL", input.Config.ControlPlaneURL},
		{"XMCL_ENROLLMENT_TOKEN", input.EnrollmentToken},
		{"XMCL_INGRESS_HOST", input.Address},
		{"XMCL_OBJECT_STORAGE_ENDPOINT", input.Config.ObjectStorageEndpoint},
		{"XMCL_OBJECT_STORAGE_REGION", input.Config.ObjectStorageRegion},
		{"XMCL_OBJECT_STORAGE_BUCKET", input.Config.ObjectStorageBucket},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", input.Config.OTLPEndpoint},
		{"OTEL_EXPORTER_OTLP_HEADERS", input.Config.OTLPHeaders},
		{"XMCL_RELEASE_MANIFEST_URL", input.Config.ReleaseManifestURL},
		{"XMCL_RELEASE_MANIFEST_SHA256", input.Config.ReleaseManifestSHA256},
		{"XMCL_DOCKER_PACKAGE_VERSION", input.Config.DockerPackageVersion},
		{"XMCL_TOTAL_MEMORY_MIB", strconv.Itoa(input.ExpectedCapacity.TotalMemoryMiB)},
		{"XMCL_TOTAL_SHARED_CPU", strconv.Itoa(input.ExpectedCapacity.TotalSharedCPU)},
		{"XMCL_TOTAL_WORKSPACE_GIB", strconv.Itoa(input.ExpectedCapacity.TotalWorkspaceGiB)},
		{"XMCL_WORKSPACE_VOLUME_GIB", strconv.Itoa(input.Config.WorkspaceVolumeGiB)},
		{"XMCL_BOOTSTRAP_TIMEOUT_SECONDS", strconv.Itoa(input.Config.BootstrapTimeout)},
	}
	var result strings.Builder
	for _, value := range values {
		fmt.Fprintf(&result, "%s=%s\n", value[0], shellValue(value[1]))
	}
	return result.String()
}

func shellValue(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func authorized(r *http.Request, secret string) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return false
	}
	value := strings.TrimPrefix(values[0], "Bearer ")
	return len(value) == len(secret) &&
		subtle.ConstantTimeCompare([]byte(value), []byte(secret)) == 1
}

func writeStatus(w http.ResponseWriter, status int, jobID, state string, fingerprints ...string) {
	value := ""
	if len(fingerprints) != 0 {
		value = fingerprints[0]
	}
	writeResult(w, status, jobID, state, "", false, value)
}

func writeRetirementResult(w http.ResponseWriter, state jobState) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		JobID             string `json:"jobId"`
		Status            string `json:"status"`
		ErrorCode         string `json:"errorCode"`
		Retryable         bool   `json:"retryable"`
		InstanceID        string `json:"instanceId"`
		Address           string `json:"address"`
		SSHPort           int    `json:"sshPort"`
		RetiredAt         string `json:"retiredAt"`
		AddressReuseAfter string `json:"addressReuseAfter"`
	}{
		JobID: state.JobID, Status: "retired", InstanceID: state.InstanceID,
		Address: state.Address, SSHPort: state.SSHPort,
		RetiredAt: state.RetiredAt, AddressReuseAfter: state.AddressReuseAfter,
	})
}

func writeError(
	w http.ResponseWriter,
	status int,
	jobID, state, code string,
	explicitRetryable ...bool,
) {
	switch state {
	case "probing_host_key", "host_key_auto_pinned", "awaiting_host_key_approval",
		"approved", "applying", "completed", "failed", "retired":
	default:
		state = "failed"
	}
	retryable := code == "runner_unavailable" ||
		code == "host_key_probe_failed" || code == "apply_failed"
	if len(explicitRetryable) != 0 {
		retryable = explicitRetryable[0]
	}
	writeResult(w, status, jobID, state, code, retryable, "")
}

func writeStateError(
	w http.ResponseWriter,
	status int,
	state jobState,
	code string,
	retryable bool,
) {
	writeResult(
		w, status, state.JobID, "failed",
		code, retryable, state.Fingerprint,
	)
}

func writeResult(
	w http.ResponseWriter,
	httpStatus int,
	jobID, status, errorCode string,
	retryable bool,
	fingerprint string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(struct {
		JobID       string `json:"jobId"`
		Status      string `json:"status"`
		ErrorCode   string `json:"errorCode"`
		Retryable   bool   `json:"retryable"`
		Fingerprint string `json:"fingerprint,omitempty"`
	}{
		JobID: jobID, Status: status, ErrorCode: errorCode,
		Retryable: retryable, Fingerprint: fingerprint,
	})
}
