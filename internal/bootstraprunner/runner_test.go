package bootstraprunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	pinnedFingerprint  = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	changedFingerprint = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

var testNow = time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)

type fakeExecutor struct {
	mu          sync.Mutex
	probes      int
	applies     int
	knownHosts  string
	fingerprint string
	env         string
	probeErr    error
	applyErr    error
	onApply     func(string) error
	probePort   int
	applyPort   int
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{
		knownHosts:  "8.8.8.8 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest",
		fingerprint: pinnedFingerprint,
	}
}

func (e *fakeExecutor) Probe(_ context.Context, _ string, port int) (string, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probes++
	e.probePort = port
	return e.knownHosts, e.fingerprint, e.probeErr
}

func (e *fakeExecutor) Apply(
	_ context.Context,
	_ string,
	port int,
	_, _ string,
	envPath string,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applies++
	e.applyPort = port
	value, err := os.ReadFile(envPath)
	e.env = string(value)
	if err == nil && e.onApply != nil {
		err = e.onApply(envPath)
	}
	if err == nil {
		err = e.applyErr
	}
	return err
}

func (e *fakeExecutor) counts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.probes, e.applies
}

func validRequest() Request {
	return validAutoRequest(testNow)
}

func validAutoRequest(now time.Time) Request {
	input := Request{
		JobID: "capacity_tpe_1", NodeID: "node-ln-1",
		InstanceID: "ecs-tpe-1", Address: "8.8.8.8", SSHPort: 22,
		EnrollmentToken: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		ExpectedProvider: ExpectedProvider{
			InstanceName: "xmcl-tpe-capacity-1", PackageCode: "taipei-c4m8-s50-d100",
			RegionCode: "cn-taiwan-2", ZoneCode: "cn-taiwan-2-a",
			ImageResourceUUID: "img-tpe-agent", FirewallUUID: "fw-tpe-xmcl",
			SSHKeyUUID: "key-xmcl-bootstrap",
		},
		HostKeyPolicy:       AutoPinFreshAllocationPolicy,
		AllocationCreatedAt: canonicalTime(now.Add(-time.Minute)),
		AutoPinExpiresAt:    canonicalTime(now.Add(5 * time.Minute)),
		Config: BootstrapConfig{
			ReleaseManifestURL:    "https://github.com/Voxelum/xmcl-shared-node-agent/releases/download/v0.3.1/release-manifest.json",
			ReleaseManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			DockerPackageVersion:  "26.1.3-0ubuntu1~24.04.1",
			ControlPlaneURL:       "https://api-staging.xmcl.app",
			LogicalRegion:         "tpe",
			ObjectStorageEndpoint: "https://objects.example.com",
			ObjectStorageRegion:   "azure",
			ObjectStorageBucket:   "xmcl-shared",
			OTLPEndpoint:          "https://otel.example.com",
			OTLPHeaders:           "authorization=node-scoped-value",
			WorkspaceVolumeGiB:    40,
			BootstrapTimeout:      600,
		},
		ExpectedCapacity: ExpectedCapacity{
			WorkloadClasses: []string{"standard", "large"},
			TotalMemoryMiB:  8192, TotalSharedCPU: 4, TotalWorkspaceGiB: 32,
		},
	}
	input.ExpectedProviderIdentity = ExpectedProviderIdentity{
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
	return input
}

func validManualRequest() Request {
	input := validAutoRequest(testNow)
	input.HostKeyPolicy = ManualApprovalPolicy
	return input
}

func newTestServer(t *testing.T, root string, executor *fakeExecutor) (*Handler, *httptest.Server) {
	t.Helper()
	handler, err := newInheritedFixtureHandler(
		root,
		string(bytes.Repeat([]byte("r"), 32)),
		string(bytes.Repeat([]byte("a"), 32)),
		executor,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return testNow }
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return handler, server
}

func TestFreshAutoPinAppliesWithoutApproval(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	_, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)

	result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if result.Code != http.StatusOK || result.Status != "completed" {
		t.Fatalf("auto bootstrap response = %#v", result)
	}
	if result.Fingerprint != pinnedFingerprint || result.Retryable {
		t.Fatalf("auto bootstrap result metadata = %#v", result)
	}
	if probes, applies := executor.counts(); probes != 1 || applies != 1 {
		t.Fatalf("probe/apply calls = %d/%d", probes, applies)
	}
	if executor.probePort != input.SSHPort || executor.applyPort != input.SSHPort {
		t.Fatalf("probe/apply ports = %d/%d", executor.probePort, executor.applyPort)
	}
	approval := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/approve",
		map[string]string{"fingerprint": pinnedFingerprint}, "a")
	if approval.Code != http.StatusConflict ||
		approval.ErrorCode != "invalid_host_key_policy" {
		t.Fatalf("auto approval response = %#v", approval)
	}
}

func TestWorkerAzureStorageRegionIsAccepted(t *testing.T) {
	executor := newFakeExecutor()
	_, server := newTestServer(t, t.TempDir(), executor)
	input := validAutoRequest(testNow)
	if input.Config.ObjectStorageRegion != "azure" {
		t.Fatalf("fixture storage region = %q", input.Config.ObjectStorageRegion)
	}
	result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if result.Code != http.StatusOK || result.Status != "completed" {
		t.Fatalf("Worker-compatible response = %#v", result)
	}
	if !strings.Contains(executor.env, "XMCL_OBJECT_STORAGE_REGION='azure'\n") {
		t.Fatalf("bootstrap environment does not preserve Worker storage region: %q", executor.env)
	}
}

func TestHandlerRejectsEqualSubmitAndStatusSecrets(t *testing.T) {
	secret := string(bytes.Repeat([]byte("s"), 32))
	if _, err := newInheritedFixtureHandler(t.TempDir(), secret, secret, newFakeExecutor()); err == nil {
		t.Fatal("equal submit and status secrets were accepted")
	}
}

func TestAutoPolicyBoundsRejectBeforeProbe(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Request)
		code   string
	}{
		"stale": {
			mutate: func(input *Request) {
				input.AllocationCreatedAt = canonicalTime(testNow.Add(-10 * time.Minute))
				input.AutoPinExpiresAt = canonicalTime(testNow.Add(-time.Second))
			},
			code: "auto_pin_deadline_expired",
		},
		"future allocation": {
			mutate: func(input *Request) {
				input.AllocationCreatedAt = canonicalTime(testNow.Add(2*time.Minute + time.Second))
				input.AutoPinExpiresAt = canonicalTime(testNow.Add(5 * time.Minute))
			},
			code: "invalid_host_key_policy",
		},
		"non-UTC time": {
			mutate: func(input *Request) {
				input.AllocationCreatedAt = "2026-09-02T23:00:00+08:00"
			},
			code: "invalid_host_key_policy",
		},
		"window over ten minutes": {
			mutate: func(input *Request) {
				input.AllocationCreatedAt = canonicalTime(testNow)
				input.AutoPinExpiresAt = canonicalTime(testNow.Add(10*time.Minute + time.Second))
			},
			code: "invalid_host_key_policy",
		},
		"noncanonical time": {
			mutate: func(input *Request) {
				input.AllocationCreatedAt = "2026-09-02T15:00:00.000Z"
			},
			code: "invalid_host_key_policy",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			executor := newFakeExecutor()
			_, server := newTestServer(t, t.TempDir(), executor)
			input := validAutoRequest(testNow)
			tc.mutate(&input)
			result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
			if result.ErrorCode != tc.code {
				t.Fatalf("response = %#v, want error %q", result, tc.code)
			}
			if probes, _ := executor.counts(); probes != 0 {
				t.Fatalf("unsafe policy made %d probes", probes)
			}
		})
	}
}

func TestProviderIdentityMismatchRejectsBeforeProbe(t *testing.T) {
	executor := newFakeExecutor()
	_, server := newTestServer(t, t.TempDir(), executor)
	input := validAutoRequest(testNow)
	input.ExpectedProviderIdentity.InstanceName = "different-instance-name"
	result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if result.Code != http.StatusConflict ||
		result.ErrorCode != "provider_identity_mismatch" || result.Retryable {
		t.Fatalf("identity mismatch response = %#v", result)
	}
	if probes, applies := executor.counts(); probes != 0 || applies != 0 {
		t.Fatalf("identity mismatch made probe/apply calls = %d/%d", probes, applies)
	}
}

func TestAutoPolicyRejectsNonPublicOrNonCanonicalAddresses(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"100.64.0.1",
		"169.254.1.1",
		"192.0.2.1",
		"198.18.0.1",
		"203.0.113.1",
		"240.0.0.1",
		"::ffff:8.8.8.8",
	} {
		t.Run(address, func(t *testing.T) {
			executor := newFakeExecutor()
			_, server := newTestServer(t, t.TempDir(), executor)
			input := validAutoRequest(testNow)
			input.Address = address
			input.ExpectedProviderIdentity.Address = address
			result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
			if result.Code != http.StatusBadRequest || result.ErrorCode != "invalid_request" {
				t.Fatalf("address %q response = %#v", address, result)
			}
			if probes, applies := executor.counts(); probes != 0 || applies != 0 {
				t.Fatalf("address %q made probe/apply calls = %d/%d", address, probes, applies)
			}
		})
	}
}

func TestSSHPortIsRequiredAndRestrictedToLightNodeMVP(t *testing.T) {
	tests := map[string]int{
		"missing": 0, "negative": -1, "non-MVP": 23, "too large": 65536,
	}
	for name, port := range tests {
		t.Run(name, func(t *testing.T) {
			executor := newFakeExecutor()
			_, server := newTestServer(t, t.TempDir(), executor)
			input := validAutoRequest(testNow)
			input.SSHPort = port
			result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
			if result.Code != http.StatusBadRequest || result.ErrorCode != "invalid_host_key_policy" {
				t.Fatalf("port %d response = %#v", port, result)
			}
			if probes, applies := executor.counts(); probes != 0 || applies != 0 {
				t.Fatalf("port %d made probe/apply calls = %d/%d", port, probes, applies)
			}
		})
	}
}

func TestApplyExitClassification(t *testing.T) {
	if err := classifyApplyExit(75); !errors.Is(err, ErrRetryableApply) {
		t.Fatalf("temporary SSH exit classification = %v", err)
	}
	if err := classifyApplyExit(77); !errors.Is(err, ErrEnrollmentRejected) {
		t.Fatalf("enrollment exit classification = %v", err)
	}
	if err := classifyApplyExit(76); !errors.Is(err, errApplyOutcomeUnknown) {
		t.Fatalf("unknown apply outcome classification = %v", err)
	}
	if err := classifyApplyExit(70); errors.Is(err, ErrRetryableApply) ||
		errors.Is(err, ErrEnrollmentRejected) {
		t.Fatalf("terminal apply exit classification = %v", err)
	}
}

func TestCrashAfterApplyRequiresWorkerReconciliation(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	handler, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)
	handler.afterApply = func() error { return errors.New("simulated process crash") }

	first := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if first.Status != "applying" || first.ErrorCode != "runner_unavailable" ||
		!first.Retryable {
		t.Fatalf("interrupted apply response = %#v", first)
	}
	handler.afterApply = nil
	replay := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if replay.Status != "failed" || replay.ErrorCode != "apply_outcome_unknown" ||
		replay.Retryable {
		t.Fatalf("unknown apply replay = %#v", replay)
	}
	if probes, applies := executor.counts(); probes != 1 || applies != 1 {
		t.Fatalf("unknown apply was repeated: probe/apply = %d/%d", probes, applies)
	}
}

func TestExactReplayAfterDeadlineResumesDurablePin(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	handler, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)
	crashed := false
	handler.afterPin = func() error {
		if !crashed {
			crashed = true
			return errors.New("simulated crash")
		}
		return nil
	}
	first := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if first.ErrorCode != "runner_unavailable" || !first.Retryable {
		t.Fatalf("first response = %#v", first)
	}
	state := readStateForTest(t, root, input.JobID)
	if state.FirstObservedAt == "" || state.Fingerprint != pinnedFingerprint ||
		state.Decision != "auto_approved" || state.Status != "host_key_auto_pinned" {
		t.Fatalf("durable pin state = %#v", state)
	}

	handler.now = func() time.Time { return testNow.Add(20 * time.Minute) }
	second := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	third := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if second.Status != "completed" || third.Status != "completed" {
		t.Fatalf("replay responses = %#v / %#v", second, third)
	}
	if probes, applies := executor.counts(); probes != 2 || applies != 1 {
		t.Fatalf("probe/apply calls = %d/%d", probes, applies)
	}
}

func TestProbeFailurePersistsObservationButCannotPinAfterDeadline(t *testing.T) {
	executor := newFakeExecutor()
	executor.probeErr = errors.New("temporary probe failure")
	root := t.TempDir()
	handler, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)

	first := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if first.ErrorCode != "host_key_probe_failed" || !first.Retryable {
		t.Fatalf("first response = %#v", first)
	}
	state := readStateForTest(t, root, input.JobID)
	if state.FirstObservedAt != canonicalTime(testNow) || state.Fingerprint != "" {
		t.Fatalf("failed-probe state = %#v", state)
	}

	executor.mu.Lock()
	executor.probeErr = nil
	executor.mu.Unlock()
	handler.now = func() time.Time { return testNow.Add(20 * time.Minute) }
	expired := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if expired.Code != http.StatusConflict ||
		expired.ErrorCode != "auto_pin_deadline_expired" || expired.Retryable {
		t.Fatalf("expired replay = %#v", expired)
	}
	if probes, applies := executor.counts(); probes != 1 || applies != 0 {
		t.Fatalf("probe/apply calls = %d/%d", probes, applies)
	}
}

func TestMalformedKnownHostsFailsSafely(t *testing.T) {
	tests := map[string]string{
		"address mismatch":  "8.8.4.4 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest",
		"bracketed port 22": "[8.8.8.8]:22 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest",
		"wrong key type":    "8.8.8.8 ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAITest",
		"missing key":       "8.8.8.8 ssh-ed25519",
		"comment field":     "8.8.8.8 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest comment",
		"invalid base64":    "8.8.8.8 ssh-ed25519 not_base64",
		"multiple lines":    "8.8.8.8 ssh-ed25519 AAAA\n8.8.8.8 ssh-ed25519 BBBB",
	}
	for name, knownHosts := range tests {
		t.Run(name, func(t *testing.T) {
			executor := newFakeExecutor()
			executor.knownHosts = knownHosts
			_, server := newTestServer(t, t.TempDir(), executor)
			result := post(t, server.URL+"/v1/bootstrap-jobs", validAutoRequest(testNow), "r")
			if result.Status != "failed" ||
				result.ErrorCode != "host_key_probe_failed" || !result.Retryable {
				t.Fatalf("malformed known_hosts response = %#v", result)
			}
			if probes, applies := executor.counts(); probes != 1 || applies != 0 {
				t.Fatalf("probe/apply calls = %d/%d", probes, applies)
			}
		})
	}
}

func TestRequestMutationConflicts(t *testing.T) {
	executor := newFakeExecutor()
	_, server := newTestServer(t, t.TempDir(), executor)
	input := validAutoRequest(testNow)
	if got := post(t, server.URL+"/v1/bootstrap-jobs", input, "r"); got.Status != "completed" {
		t.Fatalf("initial response = %#v", got)
	}
	input.Config.ObjectStorageBucket = "xmcl-shared-mutated"
	result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if result.Code != http.StatusConflict || result.ErrorCode != "idempotency_conflict" {
		t.Fatalf("mutation response = %#v", result)
	}
}

func TestAllocationIdentityConflicts(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Request)
		code   string
	}{
		"same instance different job": {
			mutate: func(input *Request) { input.JobID = "capacity_tpe_2" },
			code:   "provider_identity_mismatch",
		},
		"different instance same address": {
			mutate: func(input *Request) {
				input.JobID = "capacity_tpe_2"
				input.InstanceID = "ecs-tpe-2"
				input.ExpectedProviderIdentity.InstanceID = input.InstanceID
			},
			code: "provider_identity_mismatch",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			executor := newFakeExecutor()
			root := t.TempDir()
			_, server := newTestServer(t, root, executor)
			first := validAutoRequest(testNow)
			if got := post(t, server.URL+"/v1/bootstrap-jobs", first, "r"); got.Status != "completed" {
				t.Fatalf("initial response = %#v", got)
			}
			second := validAutoRequest(testNow)
			tc.mutate(&second)
			result := post(t, server.URL+"/v1/bootstrap-jobs", second, "r")
			if result.ErrorCode != tc.code {
				t.Fatalf("conflict response = %#v", result)
			}
			if probes, applies := executor.counts(); probes != 1 || applies != 1 {
				t.Fatalf("conflicting allocation made calls: %d/%d", probes, applies)
			}
		})
	}
}

func TestChangedFingerprintQuarantinesAndNeverApplies(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	handler, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)
	handler.afterPin = func() error { return errors.New("simulated crash") }
	_ = post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	executor.mu.Lock()
	executor.fingerprint = changedFingerprint
	executor.mu.Unlock()

	changed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	replay := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if changed.Status != "failed" || changed.ErrorCode != "host_key_changed" ||
		replay.Status != "failed" {
		t.Fatalf("quarantine responses = %#v / %#v", changed, replay)
	}
	if probes, applies := executor.counts(); probes != 2 || applies != 0 {
		t.Fatalf("probe/apply calls = %d/%d", probes, applies)
	}
	registryBytes, err := os.ReadFile(filepath.Join(root, "allocation-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(registryBytes, []byte(`"operatorCleanupRequired":true`)) ||
		!bytes.Contains(registryBytes, []byte(`"quarantineReason":"host_key_changed"`)) {
		t.Fatalf("registry lacks fail-closed quarantine metadata: %s", registryBytes)
	}
}

func TestApplySeesDurablePinAndSecretsOnlyInTransientEnvironment(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	_, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)
	executor.onApply = func(envPath string) error {
		state := readStateForTest(t, root, input.JobID)
		if state.Fingerprint != pinnedFingerprint || state.KnownHosts == "" ||
			state.Decision != "auto_approved" || state.Status != "applying" {
			return errors.New("apply ran before durable pin")
		}
		value, err := os.ReadFile(envPath)
		if err != nil {
			return err
		}
		if !bytes.Contains(value, []byte(input.EnrollmentToken)) {
			return errors.New("transient environment lacks token")
		}
		return nil
	}
	result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if result.Status != "completed" {
		t.Fatalf("response = %#v", result)
	}
}

func TestExplicitManualApprovalBehavior(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	_, server := newTestServer(t, root, executor)
	input := validManualRequest()

	first := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if first.Code != http.StatusAccepted || first.Status != "awaiting_host_key_approval" {
		t.Fatalf("manual create response = %#v", first)
	}
	status := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/status", nil, "a")
	if status.SSHPort != input.SSHPort {
		t.Fatalf("status SSH port = %d, want %d", status.SSHPort, input.SSHPort)
	}
	if _, applies := executor.counts(); applies != 0 {
		t.Fatal("manual request applied before approval")
	}
	approved := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/approve",
		map[string]string{"fingerprint": pinnedFingerprint}, "a")
	if approved.Status != "approved" {
		t.Fatalf("approval response = %#v", approved)
	}
	completed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if completed.Status != "completed" {
		t.Fatalf("manual replay response = %#v", completed)
	}
	if probes, applies := executor.counts(); probes != 2 || applies != 1 {
		t.Fatalf("probe/apply calls = %d/%d", probes, applies)
	}

	missing := validAutoRequest(testNow)
	missing.JobID, missing.InstanceID, missing.Address = "missing_policy", "missing-instance", "8.8.4.4"
	missing.ExpectedProviderIdentity.InstanceID = missing.InstanceID
	missing.ExpectedProviderIdentity.Address = missing.Address
	missing.HostKeyPolicy = ""
	result := post(t, server.URL+"/v1/bootstrap-jobs", missing, "r")
	if result.ErrorCode != "invalid_host_key_policy" {
		t.Fatalf("missing policy inferred manual behavior: %#v", result)
	}
}

func TestCompletedJobCleansSecrets(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	_, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)
	if got := post(t, server.URL+"/v1/bootstrap-jobs", input, "r"); got.Status != "completed" {
		t.Fatalf("response = %#v", got)
	}
	jobRoot := filepath.Join(root, "jobs", input.JobID)
	for _, name := range []string{"state.json", "request.json"} {
		value, err := os.ReadFile(filepath.Join(jobRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(value, []byte(input.EnrollmentToken)) ||
			bytes.Contains(value, []byte(input.Config.OTLPHeaders)) {
			t.Fatalf("%s retained credentials", name)
		}
	}
	if _, err := os.Stat(filepath.Join(jobRoot, "bootstrap.env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed job retained bootstrap.env: %v", err)
	}
}

func TestSuccessfulApplyIsNotRepeatedWhenEnvironmentCleanupFails(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	_, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)
	executor.onApply = func(envPath string) error {
		value, err := os.ReadFile(envPath)
		if err != nil {
			return err
		}
		if err := os.Remove(envPath); err != nil {
			return err
		}
		if err := os.Mkdir(envPath, 0700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(envPath, "blocks-removal"), value, 0600)
	}

	first := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if first.ErrorCode != "runner_unavailable" || !first.Retryable {
		t.Fatalf("cleanup failure response = %#v", first)
	}
	if state := readStateForTest(t, root, input.JobID); state.Status != "applying" ||
		!state.ApplySucceeded {
		t.Fatalf("successful apply checkpoint = %#v", state)
	}
	status := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/status", nil, "a")
	if status.Status != "applying" {
		t.Fatalf("status during cleanup retry = %#v", status)
	}
	retained, err := os.ReadFile(
		filepath.Join(root, "jobs", input.JobID, "bootstrap.env", "blocks-removal"),
	)
	if err != nil || !bytes.Contains(retained, []byte(input.EnrollmentToken)) {
		t.Fatalf("test did not retain the token-bearing cleanup artifact: %v", err)
	}
	if probes, applies := executor.counts(); probes != 1 || applies != 1 {
		t.Fatalf("probe/apply calls = %d/%d", probes, applies)
	}

	if err := os.RemoveAll(filepath.Join(root, "jobs", input.JobID, "bootstrap.env")); err != nil {
		t.Fatal(err)
	}
	replayed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if replayed.Status != "completed" {
		t.Fatalf("cleanup replay = %#v", replayed)
	}
	if probes, applies := executor.counts(); probes != 1 || applies != 1 {
		t.Fatalf("successful work was repeated: %d/%d calls", probes, applies)
	}
}

func TestApplyFailurePersistsOnlySafeStatus(t *testing.T) {
	executor := newFakeExecutor()
	executor.applyErr = errors.New("raw output token=super-secret")
	root := t.TempDir()
	_, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)

	failed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if failed.Code != http.StatusConflict ||
		failed.Status != "failed" || failed.ErrorCode != "terminal_apply_failure" {
		t.Fatalf("apply failure response = %#v", failed)
	}
	status := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/status", nil, "a")
	if status.Status != "failed" || status.ErrorCode != "terminal_apply_failure" {
		t.Fatalf("operator status = %#v", status)
	}
	stateBytes, err := os.ReadFile(filepath.Join(root, "jobs", input.JobID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"raw output", "super-secret", input.EnrollmentToken, input.Config.OTLPHeaders,
	} {
		if strings.Contains(string(stateBytes), forbidden) {
			t.Fatalf("state retained unsafe value %q", forbidden)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", input.JobID, "bootstrap.env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed job retained bootstrap.env: %v", err)
	}
}

func TestRetryableFailuresResumeThroughFreshProbe(t *testing.T) {
	tests := map[string]struct {
		configure func(*fakeExecutor)
		code      string
		applies   int
	}{
		"probe": {
			configure: func(executor *fakeExecutor) {
				executor.probeErr = errors.New("temporary probe failure")
			},
			code: "host_key_probe_failed", applies: 0,
		},
		"apply": {
			configure: func(executor *fakeExecutor) {
				executor.applyErr = ErrRetryableApply
			},
			code: "apply_failed", applies: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			executor := newFakeExecutor()
			tc.configure(executor)
			_, server := newTestServer(t, t.TempDir(), executor)
			input := validAutoRequest(testNow)
			failed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
			if failed.Status != "failed" || failed.ErrorCode != tc.code || !failed.Retryable {
				t.Fatalf("retryable response = %#v", failed)
			}
			executor.mu.Lock()
			executor.probeErr, executor.applyErr = nil, nil
			executor.mu.Unlock()
			completed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
			if completed.Status != "completed" {
				t.Fatalf("retry response = %#v", completed)
			}
			if probes, applies := executor.counts(); probes != 2 || applies != tc.applies+1 {
				t.Fatalf("probe/apply calls = %d/%d", probes, applies)
			}
		})
	}
}

func TestRunnerPreservesTaipeiOnlyValidation(t *testing.T) {
	tests := map[string]func(*Request){
		"logical region":  func(input *Request) { input.Config.LogicalRegion = "mow" },
		"provider region": func(input *Request) { input.ExpectedProvider.RegionCode = "ru-moscow-1" },
		"storage region":  func(input *Request) { input.Config.ObjectStorageRegion = "cn-taiwan-2" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := validAutoRequest(time.Now())
			mutate(&input)
			if err := validate(input); err == nil {
				t.Fatal("expected non-Taipei request to be rejected")
			}
		})
	}
}

func TestConcurrentAddressReservationAllowsOnlyOneJob(t *testing.T) {
	executor := newFakeExecutor()
	_, server := newTestServer(t, t.TempDir(), executor)
	first := validAutoRequest(testNow)
	second := validAutoRequest(testNow)
	second.JobID, second.InstanceID = "capacity_tpe_2", "ecs-tpe-2"
	second.ExpectedProviderIdentity.InstanceID = second.InstanceID

	var wg sync.WaitGroup
	results := make(chan response, 2)
	for _, input := range []Request{first, second} {
		wg.Add(1)
		go func(input Request) {
			defer wg.Done()
			results <- post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
		}(input)
	}
	wg.Wait()
	close(results)
	var completed, conflicted int
	for result := range results {
		if result.Status == "completed" {
			completed++
		}
		if result.ErrorCode == "provider_identity_mismatch" {
			conflicted++
		}
	}
	if completed != 1 || conflicted != 1 {
		t.Fatalf("completed/conflicted = %d/%d", completed, conflicted)
	}
	if probes, applies := executor.counts(); probes != 1 || applies != 1 {
		t.Fatalf("probe/apply calls = %d/%d", probes, applies)
	}
}

func TestRetireRouteStatusSecretAuthenticationAndStrictJSON(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	handler, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)
	if got := post(t, server.URL+"/v1/bootstrap-jobs", input, "r"); got.Status != "completed" {
		t.Fatalf("create response = %#v", got)
	}
	url := server.URL + "/v1/bootstrap-jobs/" + input.JobID + "/retire"
	body := retirementRequest{
		InstanceID: input.InstanceID, Address: input.Address, SSHPort: input.SSHPort,
	}
	if got := post(t, url, body, "r"); got.Code != http.StatusUnauthorized {
		t.Fatalf("submit secret retirement response = %#v", got)
	}
	statusSecret := string(bytes.Repeat([]byte("a"), 32))
	exactBody := fmt.Sprintf(
		`{"instanceId":%q,"address":%q,"sshPort":%d}`,
		input.InstanceID, input.Address, input.SSHPort,
	)
	if got := postRaw(t, url, exactBody, statusSecret); got.Code != http.StatusUnauthorized {
		t.Fatalf("prefixless status secret response = %#v", got)
	}
	duplicateAuth := httptest.NewRequest(
		http.MethodPost,
		"/v1/bootstrap-jobs/"+input.JobID+"/retire",
		strings.NewReader(exactBody),
	)
	duplicateAuth.Header.Add("Authorization", "Bearer "+statusSecret)
	duplicateAuth.Header.Add("Authorization", "Bearer "+statusSecret)
	duplicateAuthResult := httptest.NewRecorder()
	handler.ServeHTTP(duplicateAuthResult, duplicateAuth)
	if duplicateAuthResult.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate authorization response = %d", duplicateAuthResult.Code)
	}
	for name, raw := range map[string]string{
		"case variant": fmt.Sprintf(
			`{"InstanceId":%q,"address":%q,"sshPort":%d}`,
			input.InstanceID, input.Address, input.SSHPort,
		),
		"duplicate": fmt.Sprintf(
			`{"instanceId":"wrong","instanceId":%q,"address":%q,"sshPort":%d}`,
			input.InstanceID, input.Address, input.SSHPort,
		),
	} {
		if got := postRaw(t, url, raw, "Bearer "+statusSecret); got.Code != http.StatusBadRequest ||
			got.ErrorCode != "invalid_request" {
			t.Fatalf("%s retirement JSON response = %#v", name, got)
		}
	}
	if got := post(t, url, map[string]any{
		"instanceId": input.InstanceID, "address": input.Address,
		"sshPort": input.SSHPort, "unknown": true,
	}, "a"); got.Code != http.StatusBadRequest || got.ErrorCode != "invalid_request" {
		t.Fatalf("unknown JSON response = %#v", got)
	}
	if got := post(t, url, nil, "a"); got.Code != http.StatusBadRequest ||
		got.ErrorCode != "invalid_request" {
		t.Fatalf("missing JSON fields response = %#v", got)
	}
	unknown := post(t, server.URL+"/v1/bootstrap-jobs/unknown-job/retire", body, "a")
	if unknown.Code != http.StatusNotFound || unknown.ErrorCode != "job_not_found" {
		t.Fatalf("unknown job response = %#v", unknown)
	}
	retired := post(t, url, body, "a")
	if retired.Code != http.StatusOK || retired.Status != "retired" ||
		retired.ErrorCode != "" || retired.Retryable {
		t.Fatalf("retirement response = %#v", retired)
	}
	status := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/status", nil, "a")
	if status.Status != "retired" || status.RetiredAt != retired.RetiredAt ||
		status.AddressReuseAfter != retired.AddressReuseAfter {
		t.Fatalf("retired status = %#v", status)
	}
}

func TestRetireRejectsAllNonterminalStates(t *testing.T) {
	tests := map[string]struct {
		status         string
		retryable      bool
		applySucceeded bool
	}{
		"probing":          {status: "probing_host_key"},
		"pinned":           {status: "host_key_auto_pinned"},
		"approved":         {status: "approved"},
		"applying":         {status: "applying"},
		"applying cleanup": {status: "applying", applySucceeded: true},
		"retryable failed": {status: "failed", retryable: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			executor := newFakeExecutor()
			root := t.TempDir()
			handler, server := newTestServer(t, root, executor)
			handler.afterPin = func() error { return errors.New("stop after pin") }
			input := validAutoRequest(testNow)
			_ = post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
			state := readStateForTest(t, root, input.JobID)
			state.Status = tc.status
			state.Retryable = tc.retryable
			state.Terminal = false
			state.ApplySucceeded = tc.applySucceeded
			if err := writeJobState(
				filepath.Join(root, "jobs", input.JobID, "state.json"), state,
			); err != nil {
				t.Fatal(err)
			}
			result := post(
				t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/retire",
				retirementRequest{
					InstanceID: input.InstanceID, Address: input.Address, SSHPort: input.SSHPort,
				}, "a",
			)
			if result.Code != http.StatusConflict ||
				result.ErrorCode != "allocation_not_terminal" || result.Retryable {
				t.Fatalf("premature retirement response = %#v", result)
			}
		})
	}
}

func TestRetireIdentityAndCrashIdempotency(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	handler, server := newTestServer(t, root, executor)
	input := validAutoRequest(testNow)
	_ = post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	url := server.URL + "/v1/bootstrap-jobs/" + input.JobID + "/retire"
	body := retirementRequest{
		InstanceID: input.InstanceID, Address: input.Address, SSHPort: input.SSHPort,
	}
	mutated := body
	mutated.Address = "8.8.4.4"
	if got := post(t, url, mutated, "a"); got.Code != http.StatusConflict ||
		got.ErrorCode != "retirement_identity_mismatch" || got.Retryable {
		t.Fatalf("mutated retirement response = %#v", got)
	}

	handler.now = func() time.Time { return testNow.Add(time.Hour) }
	first := post(t, url, body, "a")
	if first.Status != "retired" {
		t.Fatalf("first retirement = %#v", first)
	}
	state := readStateForTest(t, root, input.JobID)
	state.Status, state.RetiredAt, state.AddressReuseAfter = "completed", "", ""
	if err := writeJobState(filepath.Join(root, "jobs", input.JobID, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return testNow.Add(48 * time.Hour) }
	replayed := post(t, url, body, "a")
	if replayed.Status != "retired" ||
		replayed.RetiredAt != first.RetiredAt ||
		replayed.AddressReuseAfter != first.AddressReuseAfter {
		t.Fatalf("crash/idempotent replay = %#v / %#v", first, replayed)
	}
}

func TestRetiredAddressReuseAndRestartPersistence(t *testing.T) {
	executor := newFakeExecutor()
	root := t.TempDir()
	handler, server := newTestServer(t, root, executor)
	original := validAutoRequest(testNow)
	_ = post(t, server.URL+"/v1/bootstrap-jobs", original, "r")
	retired := post(
		t, server.URL+"/v1/bootstrap-jobs/"+original.JobID+"/retire",
		retirementRequest{
			InstanceID: original.InstanceID, Address: original.Address, SSHPort: original.SSHPort,
		}, "a",
	)
	if retired.Status != "retired" {
		t.Fatalf("retirement = %#v", retired)
	}
	beforeProbes, beforeApplies := executor.counts()
	oldReplay := post(t, server.URL+"/v1/bootstrap-jobs", original, "r")
	if oldReplay.Code != http.StatusConflict || oldReplay.ErrorCode != "allocation_retired" {
		t.Fatalf("old job replay = %#v", oldReplay)
	}
	if probes, applies := executor.counts(); probes != beforeProbes || applies != beforeApplies {
		t.Fatalf("old job replay made calls = %d/%d", probes, applies)
	}

	handler.now = func() time.Time { return testNow.Add(23 * time.Hour) }
	early := validAutoRequest(testNow.Add(23 * time.Hour))
	early.JobID, early.InstanceID = "capacity_tpe_2", "ecs-tpe-2"
	early.ExpectedProviderIdentity.InstanceID = early.InstanceID
	if got := post(t, server.URL+"/v1/bootstrap-jobs", early, "r"); got.Code != http.StatusConflict ||
		got.ErrorCode != "provider_identity_mismatch" {
		t.Fatalf("pre-cooldown reuse = %#v", got)
	}
	if probes, applies := executor.counts(); probes != beforeProbes || applies != beforeApplies {
		t.Fatalf("pre-cooldown reuse made calls = %d/%d", probes, applies)
	}

	permanent := validAutoRequest(testNow.Add(25 * time.Hour))
	permanent.JobID, permanent.Address = "capacity_tpe_3", "8.8.4.4"
	permanent.ExpectedProviderIdentity.Address = permanent.Address
	handler.now = func() time.Time { return testNow.Add(25 * time.Hour) }
	if got := post(t, server.URL+"/v1/bootstrap-jobs", permanent, "r"); got.Code != http.StatusConflict ||
		got.ErrorCode != "provider_identity_mismatch" {
		t.Fatalf("retired instance reuse = %#v", got)
	}
	if err := handler.updateAllocation(
		original.JobID, "completed", "", testNow.Add(25*time.Hour),
	); err == nil {
		t.Fatal("updateAllocation overwrote a retired binding")
	}
	registryBeforeRestart, err := handler.readRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if registryBeforeRestart.Bindings[0].Status != "retired" {
		t.Fatalf("retired tombstone was changed: %#v", registryBeforeRestart.Bindings[0])
	}

	server.Close()
	restartHandler, restartServer := newTestServer(t, root, executor)
	restartHandler.now = func() time.Time { return testNow.Add(25 * time.Hour) }
	replacement := validAutoRequest(testNow.Add(25 * time.Hour))
	replacement.JobID, replacement.InstanceID = "capacity_tpe_4", "ecs-tpe-4"
	replacement.ExpectedProviderIdentity.InstanceID = replacement.InstanceID
	got := post(t, restartServer.URL+"/v1/bootstrap-jobs", replacement, "r")
	if got.Status != "completed" {
		t.Fatalf("post-cooldown restart reuse = %#v", got)
	}
	registry, err := restartHandler.readRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Bindings) != 2 || registry.Bindings[0].Status != "retired" ||
		registry.Bindings[1].JobID != replacement.JobID {
		t.Fatalf("registry history = %#v", registry.Bindings)
	}
}

func TestTerminalFailedAllocationCanRetire(t *testing.T) {
	executor := newFakeExecutor()
	executor.applyErr = errors.New("terminal apply failure")
	_, server := newTestServer(t, t.TempDir(), executor)
	input := validAutoRequest(testNow)
	failed := post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if failed.Status != "failed" || failed.Retryable {
		t.Fatalf("terminal setup = %#v", failed)
	}
	retired := post(
		t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/retire",
		retirementRequest{
			InstanceID: input.InstanceID, Address: input.Address, SSHPort: input.SSHPort,
		}, "a",
	)
	if retired.Status != "retired" {
		t.Fatalf("terminal failed retirement = %#v", retired)
	}
}

func readStateForTest(t *testing.T, root, jobID string) jobState {
	t.Helper()
	state, err := readJobState(filepath.Join(root, "jobs", jobID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

type response struct {
	Code              int
	Status            string
	ErrorCode         string
	Fingerprint       string
	InstanceID        string
	Address           string
	SSHPort           int
	ExpectedProvider  ExpectedProvider
	HostKeyPolicy     string
	FirstObservedAt   string
	Retryable         bool
	RetiredAt         string
	AddressReuseAfter string
}

func post(t *testing.T, url string, body any, secretByte string) response {
	t.Helper()
	value, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return postRaw(
		t, url, string(value),
		"Bearer "+string(bytes.Repeat([]byte(secretByte), 32)),
	)
}

func postRaw(t *testing.T, url, body, authorization string) response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	result, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	var payload struct {
		Status            string           `json:"status"`
		ErrorCode         string           `json:"errorCode"`
		Fingerprint       string           `json:"fingerprint"`
		InstanceID        string           `json:"instanceId"`
		Address           string           `json:"address"`
		SSHPort           int              `json:"sshPort"`
		ExpectedProvider  ExpectedProvider `json:"expectedProvider"`
		HostKeyPolicy     string           `json:"hostKeyPolicy"`
		FirstObservedAt   string           `json:"firstObservedAt"`
		Retryable         bool             `json:"retryable"`
		RetiredAt         string           `json:"retiredAt"`
		AddressReuseAfter string           `json:"addressReuseAfter"`
	}
	_ = json.NewDecoder(result.Body).Decode(&payload)
	return response{
		Code: result.StatusCode, Status: payload.Status, ErrorCode: payload.ErrorCode,
		Fingerprint: payload.Fingerprint, InstanceID: payload.InstanceID,
		Address: payload.Address, SSHPort: payload.SSHPort,
		ExpectedProvider: payload.ExpectedProvider,
		HostKeyPolicy:    payload.HostKeyPolicy, FirstObservedAt: payload.FirstObservedAt,
		Retryable: payload.Retryable,
		RetiredAt: payload.RetiredAt, AddressReuseAfter: payload.AddressReuseAfter,
	}
}
