package bootstraprunner

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type Request struct {
	JobID            string           `json:"jobId"`
	NodeID           string           `json:"nodeId"`
	InstanceID       string           `json:"instanceId"`
	Address          string           `json:"address"`
	EnrollmentToken  string           `json:"enrollmentToken"`
	ExpectedProvider ExpectedProvider `json:"expectedProvider"`
	Config           BootstrapConfig  `json:"config"`
	ExpectedCapacity ExpectedCapacity `json:"expectedCapacity"`
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
	Probe(context.Context, string) (knownHosts, fingerprint string, err error)
	Apply(context.Context, string, string, string, string) error
}

type Handler struct {
	root           string
	secret         string
	approvalSecret string
	executor       Executor
	mu             sync.Mutex
	locks          map[string]*sync.Mutex
}

func NewHandler(root, secret, approvalSecret string, executor Executor) (*Handler, error) {
	if root == "" || len(secret) < 32 || len(approvalSecret) < 32 || executor == nil {
		return nil, errors.New("bootstrap runner configuration is invalid")
	}
	if err := os.MkdirAll(filepath.Join(root, "jobs"), 0700); err != nil {
		return nil, fmt.Errorf("create bootstrap job root: %w", err)
	}
	return &Handler{
		root: root, secret: secret, approvalSecret: approvalSecret,
		executor: executor, locks: map[string]*sync.Mutex{},
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/bootstrap-jobs")
	if path == "" {
		if r.Method != http.MethodPost {
			http.Error(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, h.secret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.createOrResume(w, r)
		return
	}
	if strings.HasSuffix(path, "/approve") {
		if r.Method != http.MethodPost {
			http.Error(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, h.approvalSecret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		jobID := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/approve")
		h.approve(w, r, jobID)
		return
	}
	if strings.HasSuffix(path, "/status") {
		if r.Method != http.MethodPost {
			http.Error(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, h.approvalSecret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		jobID := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/status")
		h.status(w, jobID)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) createOrResume(w http.ResponseWriter, r *http.Request) {
	var input Request
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	if err := validate(input); err != nil {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	lock := h.jobLock(input.JobID)
	lock.Lock()
	defer lock.Unlock()

	jobDir := filepath.Join(h.root, "jobs", input.JobID)
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		http.Error(w, "runner_unavailable", http.StatusServiceUnavailable)
		return
	}
	canonical, _ := json.Marshal(input)
	requestPath := filepath.Join(jobDir, "request.json")
	if existing, err := os.ReadFile(requestPath); err == nil {
		if !bytes.Equal(existing, canonical) {
			http.Error(w, "idempotency_conflict", http.StatusConflict)
			return
		}
	} else if !errors.Is(err, os.ErrNotExist) || writePrivate(requestPath, canonical) != nil {
		http.Error(w, "runner_unavailable", http.StatusServiceUnavailable)
		return
	}
	if exists(filepath.Join(jobDir, "completed")) {
		writeStatus(w, http.StatusOK, input.JobID, "completed")
		return
	}
	knownHostsPath := filepath.Join(jobDir, "known_hosts")
	fingerprintPath := filepath.Join(jobDir, "fingerprint")
	if !exists(knownHostsPath) {
		knownHosts, fingerprint, err := h.executor.Probe(r.Context(), input.Address)
		if err != nil || writePrivate(knownHostsPath, []byte(knownHosts+"\n")) != nil ||
			writePrivate(fingerprintPath, []byte(fingerprint+"\n")) != nil {
			http.Error(w, "probe_failed", http.StatusServiceUnavailable)
			return
		}
		writeStatus(w, http.StatusAccepted, input.JobID, "awaiting_host_key_approval")
		return
	}
	if !exists(filepath.Join(jobDir, "approved")) {
		writeStatus(w, http.StatusAccepted, input.JobID, "awaiting_host_key_approval")
		return
	}
	fingerprintBytes, err := os.ReadFile(fingerprintPath)
	if err != nil {
		http.Error(w, "runner_unavailable", http.StatusServiceUnavailable)
		return
	}
	envPath := filepath.Join(jobDir, "bootstrap.env")
	if err := writePrivate(envPath, []byte(renderEnvironment(input))); err != nil {
		http.Error(w, "runner_unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.executor.Apply(
		r.Context(), input.Address, knownHostsPath,
		strings.TrimSpace(string(fingerprintBytes)), envPath,
	); err != nil {
		http.Error(w, "apply_failed", http.StatusServiceUnavailable)
		return
	}
	if err := writePrivate(filepath.Join(jobDir, "completed"), []byte("completed\n")); err != nil {
		http.Error(w, "runner_unavailable", http.StatusServiceUnavailable)
		return
	}
	writeStatus(w, http.StatusOK, input.JobID, "completed")
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request, jobID string) {
	if !identifier.MatchString(jobID) {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	var input struct {
		Fingerprint string `json:"fingerprint"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	lock := h.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	jobDir := filepath.Join(h.root, "jobs", jobID)
	expected, err := os.ReadFile(filepath.Join(jobDir, "fingerprint"))
	if err != nil || subtle.ConstantTimeCompare(
		[]byte(strings.TrimSpace(string(expected))),
		[]byte(input.Fingerprint),
	) != 1 {
		http.Error(w, "host_key_conflict", http.StatusConflict)
		return
	}
	if err := writePrivate(filepath.Join(jobDir, "approved"), []byte(input.Fingerprint+"\n")); err != nil {
		http.Error(w, "runner_unavailable", http.StatusServiceUnavailable)
		return
	}
	writeStatus(w, http.StatusOK, jobID, "approved")
}

func (h *Handler) status(w http.ResponseWriter, jobID string) {
	if !identifier.MatchString(jobID) {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	jobDir := filepath.Join(h.root, "jobs", jobID)
	fingerprintBytes, err := os.ReadFile(filepath.Join(jobDir, "fingerprint"))
	if err != nil {
		http.Error(w, "job_not_found", http.StatusNotFound)
		return
	}
	requestBytes, err := os.ReadFile(filepath.Join(jobDir, "request.json"))
	if err != nil {
		http.Error(w, "runner_unavailable", http.StatusServiceUnavailable)
		return
	}
	var request Request
	if err := json.Unmarshal(requestBytes, &request); err != nil {
		http.Error(w, "runner_unavailable", http.StatusServiceUnavailable)
		return
	}
	state := "awaiting_host_key_approval"
	if exists(filepath.Join(jobDir, "completed")) {
		state = "completed"
	} else if exists(filepath.Join(jobDir, "approved")) {
		state = "approved"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		JobID            string           `json:"jobId"`
		Status           string           `json:"status"`
		Fingerprint      string           `json:"fingerprint"`
		NodeID           string           `json:"nodeId"`
		InstanceID       string           `json:"instanceId"`
		Address          string           `json:"address"`
		ExpectedProvider ExpectedProvider `json:"expectedProvider"`
	}{
		JobID: jobID, Status: state,
		Fingerprint: strings.TrimSpace(string(fingerprintBytes)),
		NodeID:      request.NodeID, InstanceID: request.InstanceID,
		Address: request.Address, ExpectedProvider: request.ExpectedProvider,
	})
}

func (h *Handler) jobLock(jobID string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.locks[jobID] == nil {
		h.locks[jobID] = &sync.Mutex{}
	}
	return h.locks[jobID]
}

type ShellExecutor struct {
	RunnerScript    string
	BootstrapScript string
	ServiceFile     string
	PrivateKey      string
	SSHUser         string
	Port            int
}

func (e ShellExecutor) Probe(ctx context.Context, address string) (string, string, error) {
	command := exec.CommandContext(ctx, e.RunnerScript, "probe")
	command.Env = append(os.Environ(),
		"XMCL_LIGHTNODE_HOST="+address,
		"XMCL_LIGHTNODE_PORT="+strconv.Itoa(e.Port),
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
	address, knownHosts, hostFingerprint, envPath string,
) error {
	command := exec.CommandContext(ctx, e.RunnerScript, "apply")
	command.Env = append(os.Environ(),
		"XMCL_LIGHTNODE_HOST="+address,
		"XMCL_LIGHTNODE_PORT="+strconv.Itoa(e.Port),
		"XMCL_LIGHTNODE_USER="+e.SSHUser,
		"XMCL_LIGHTNODE_PRIVATE_KEY="+e.PrivateKey,
		"XMCL_LIGHTNODE_KNOWN_HOSTS="+knownHosts,
		"XMCL_LIGHTNODE_HOST_KEY_SHA256="+hostFingerprint,
		"XMCL_LIGHTNODE_BOOTSTRAP_ENV="+envPath,
		"XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT="+e.BootstrapScript,
		"XMCL_LIGHTNODE_AGENT_SERVICE="+e.ServiceFile,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("apply bootstrap: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

var (
	identifier  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	sha256Hex   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	fingerprint = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

func validate(input Request) error {
	ip := net.ParseIP(input.Address)
	if !identifier.MatchString(input.JobID) || !identifier.MatchString(input.NodeID) ||
		!identifier.MatchString(input.InstanceID) || ip == nil || ip.To4() == nil ||
		!regexp.MustCompile(`^[A-Za-z0-9_-]{32,512}$`).MatchString(input.EnrollmentToken) ||
		!sha256Hex.MatchString(input.Config.ReleaseManifestSHA256) ||
		input.Config.LogicalRegion != "mow" && input.Config.LogicalRegion != "tpe" ||
		input.Config.WorkspaceVolumeGiB < input.ExpectedCapacity.TotalWorkspaceGiB ||
		input.Config.BootstrapTimeout < 60 || input.Config.BootstrapTimeout > 1800 ||
		input.ExpectedCapacity.TotalMemoryMiB <= 0 ||
		input.ExpectedCapacity.TotalSharedCPU <= 0 ||
		input.ExpectedCapacity.TotalWorkspaceGiB <= 0 {
		return errors.New("invalid bootstrap request")
	}
	for _, value := range []string{
		input.ExpectedProvider.InstanceName,
		input.ExpectedProvider.PackageCode,
		input.ExpectedProvider.RegionCode,
		input.ExpectedProvider.ZoneCode,
		input.ExpectedProvider.ImageResourceUUID,
		input.ExpectedProvider.FirewallUUID,
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
	return nil
}

func renderEnvironment(input Request) string {
	values := [][2]string{
		{"XMCL_NODE_ID", input.NodeID},
		{"XMCL_REGION", input.Config.LogicalRegion},
		{"XMCL_CONTROL_PLANE_URL", input.Config.ControlPlaneURL},
		{"XMCL_ENROLLMENT_TOKEN", input.EnrollmentToken},
		{"XMCL_INGRESS_HOST", input.Address},
		{"XMCL_OBJECT_STORAGE_ENDPOINT", input.Config.ObjectStorageEndpoint},
		{"XMCL_OBJECT_STORAGE_REGION", input.Config.ObjectStorageRegion},
		{"XMCL_OBJECT_STORAGE_BUCKET", input.Config.ObjectStorageBucket},
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
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return len(value) == len(secret) &&
		subtle.ConstantTimeCompare([]byte(value), []byte(secret)) == 1
}

func writePrivate(path string, value []byte) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, value, 0600); err != nil {
		return err
	}
	if err := os.Chmod(temp, 0600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeStatus(w http.ResponseWriter, status int, jobID, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"jobId":  jobID,
		"status": state,
	})
}
