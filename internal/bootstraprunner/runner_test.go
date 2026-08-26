package bootstraprunner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type fakeExecutor struct {
	probes  int
	applies int
	env     string
}

func (e *fakeExecutor) Probe(context.Context, string) (string, string, error) {
	e.probes++
	return "[203.0.113.10]:22 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest",
		"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil
}

func (e *fakeExecutor) Apply(_ context.Context, _, _, _ string, envPath string) error {
	e.applies++
	value, err := os.ReadFile(envPath)
	e.env = string(value)
	return err
}

func validRequest() Request {
	return Request{
		JobID: "capacity_mow_1", NodeID: "node-ln-1",
		InstanceID: "ecs-mow-1", Address: "203.0.113.10",
		EnrollmentToken: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ExpectedProvider: ExpectedProvider{
			InstanceName: "xmcl-mow-capacity-1", PackageCode: "moscow-c4m8-s50-d100",
			RegionCode: "ru-moscow-1", ZoneCode: "ru-moscow-1-a",
			ImageResourceUUID: "img-mow-agent", FirewallUUID: "fw-mow-xmcl",
			SSHKeyUUID: "key-xmcl-bootstrap",
		},
		Config: BootstrapConfig{
			ReleaseManifestURL:    "https://github.com/Voxelum/xmcl-shared-node-agent/releases/download/v0.3.1/release-manifest.json",
			ReleaseManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			DockerPackageVersion:  "26.1.3-0ubuntu1~24.04.1",
			ControlPlaneURL:       "https://api-staging.xmcl.app",
			LogicalRegion:         "mow",
			ObjectStorageEndpoint: "https://objects.example.com",
			ObjectStorageRegion:   "mow",
			ObjectStorageBucket:   "xmcl-shared",
			WorkspaceVolumeGiB:    40,
			BootstrapTimeout:      600,
		},
		ExpectedCapacity: ExpectedCapacity{
			WorkloadClasses: []string{"standard", "large"},
			TotalMemoryMiB:  8192, TotalSharedCPU: 4, TotalWorkspaceGiB: 32,
		},
	}
}

func TestRunnerPersistsProbeApprovalAndIdempotentApply(t *testing.T) {
	executor := &fakeExecutor{}
	handler, err := NewHandler(t.TempDir(), string(bytes.Repeat([]byte("r"), 32)), string(bytes.Repeat([]byte("a"), 32)), executor)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	input := validRequest()

	first := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if first.Code != http.StatusAccepted || first.Status != "awaiting_host_key_approval" {
		t.Fatalf("first response = %#v", first)
	}
	status := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/status", nil, "a")
	if status.Status != "awaiting_host_key_approval" ||
		status.Fingerprint != "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" ||
		status.InstanceID != input.InstanceID ||
		status.Address != input.Address ||
		status.ExpectedProvider != input.ExpectedProvider {
		t.Fatalf("status response = %#v", status)
	}
	wrong := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/approve", map[string]string{
		"fingerprint": "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
	}, "a")
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong approval status = %d", wrong.Code)
	}
	approved := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/approve", map[string]string{
		"fingerprint": "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}, "a")
	if approved.Code != http.StatusOK {
		t.Fatalf("approval status = %d", approved.Code)
	}
	completed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	replayed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if completed.Status != "completed" || replayed.Status != "completed" {
		t.Fatalf("completion responses = %#v %#v", completed, replayed)
	}
	if executor.probes != 1 || executor.applies != 1 {
		t.Fatalf("probe/apply calls = %d/%d", executor.probes, executor.applies)
	}
	if !bytes.Contains([]byte(executor.env), []byte("XMCL_ENROLLMENT_TOKEN='aaaaaaaa")) {
		t.Fatal("bootstrap environment did not contain the enrollment token")
	}
}

func TestRunnerRejectsChangedIdempotentPayload(t *testing.T) {
	executor := &fakeExecutor{}
	root := t.TempDir()
	handler, err := NewHandler(root, string(bytes.Repeat([]byte("r"), 32)), string(bytes.Repeat([]byte("a"), 32)), executor)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	input := validRequest()
	_ = post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	input.Address = "203.0.113.11"
	response := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if response.Code != http.StatusConflict {
		t.Fatalf("changed payload status = %d", response.Code)
	}
	info, err := os.Stat(filepath.Join(root, "jobs", input.JobID, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("request persistence mode = %v, error = %v", info.Mode().Perm(), err)
	}
}

type response struct {
	Code             int
	Status           string
	Fingerprint      string
	InstanceID       string
	Address          string
	ExpectedProvider ExpectedProvider
}

func post(t *testing.T, url string, body any, secretByte string) response {
	t.Helper()
	value, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(value))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+string(bytes.Repeat([]byte(secretByte), 32)))
	request.Header.Set("Content-Type", "application/json")
	result, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	var payload struct {
		Status           string           `json:"status"`
		Fingerprint      string           `json:"fingerprint"`
		InstanceID       string           `json:"instanceId"`
		Address          string           `json:"address"`
		ExpectedProvider ExpectedProvider `json:"expectedProvider"`
	}
	_ = json.NewDecoder(result.Body).Decode(&payload)
	return response{
		Code: result.StatusCode, Status: payload.Status,
		Fingerprint: payload.Fingerprint,
		InstanceID:  payload.InstanceID, Address: payload.Address,
		ExpectedProvider: payload.ExpectedProvider,
	}
}
