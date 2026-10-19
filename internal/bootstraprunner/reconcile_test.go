package bootstraprunner

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReconcileCompletedWithoutEnrollmentOrReapply(t *testing.T) {
	root := t.TempDir()
	executor := newFakeExecutor()
	h, server := newTestServer(t, root, executor)
	input := validManualRequest() // explicit pinned-fixture approval; no TOFU path
	post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	result := post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/approve", map[string]string{"fingerprint": pinnedFingerprint}, "a")
	if result.Code != http.StatusOK {
		t.Fatalf("fixture approval: %#v", result)
	}
	result = post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
	if result.Code != http.StatusOK || result.Status != "completed" {
		t.Fatalf("fixture apply: %#v", result)
	}
	dir := filepath.Join(root, "jobs", input.JobID)
	state, err := readJobState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"requestDigest": state.RequestDigest, "instanceId": input.InstanceID, "releaseManifestSha256": input.Config.ReleaseManifestSHA256}
	endpoint := server.URL + "/v1/bootstrap-jobs/" + input.JobID + "/reconcile"
	probes, applies := executor.counts()
	completedAt := state.CompletedAt
	h.now = func() time.Time { return testNow.Add(48 * time.Hour) }
	for i := 0; i < 2; i++ {
		result = post(t, endpoint, expected, "a")
		if result.Code != http.StatusOK || result.Status != "completed" {
			t.Fatalf("reconcile: %#v", result)
		}
	}
	if p, a := executor.counts(); p != probes || a != applies {
		t.Fatalf("reconcile called SSH: %d/%d", p, a)
	}
	if _, err := os.Stat(filepath.Join(dir, "bootstrap.env")); !os.IsNotExist(err) {
		t.Fatalf("secret environment recreated: %v", err)
	}
	after, _ := readJobState(filepath.Join(dir, "state.json"))
	if after.CompletedAt != completedAt {
		t.Fatal("completion timestamp changed")
	}
	for field := range expected {
		bad := map[string]string{}
		for k, v := range expected {
			bad[k] = v
		}
		if field == "instanceId" {
			bad[field] = "ecs-other"
		} else {
			bad[field] = strings.Repeat("b", 64)
		}
		if response := post(t, endpoint, bad, "a"); response.Code != http.StatusConflict {
			t.Fatalf("mismatched %s accepted: %#v", field, response)
		}
	}
	if response := post(t, endpoint, expected, "r"); response.Code != http.StatusUnauthorized {
		t.Fatal("submit secret authorized reconcile")
	}
	// Resume a recorded successful apply whose local completion write was interrupted.
	state.Status = "applying"
	state.Terminal = false
	state.CompletedAt = ""
	state.ApplySucceeded = true
	state.ApplyStarted = false
	if err := writeJobState(filepath.Join(dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bootstrap.env"), []byte("consumed-fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if response := post(t, endpoint, expected, "a"); response.Code != http.StatusOK {
		t.Fatalf("finalize-only: %#v", response)
	}
	if _, err := os.Stat(filepath.Join(dir, "bootstrap.env")); !os.IsNotExist(err) {
		t.Fatal("consumed token not removed")
	}
	// A started/unknown remote apply has no success proof and must not be replayed.
	state.Status = "failed"
	state.ApplySucceeded = false
	state.ApplyStarted = true
	state.ErrorCode = "apply_outcome_unknown"
	if err := writeJobState(filepath.Join(dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	if response := post(t, endpoint, expected, "a"); response.Code != http.StatusConflict {
		t.Fatalf("unknown promoted: %#v", response)
	}
	if p, a := executor.counts(); p != probes || a != applies {
		t.Fatal("negative reconcile called SSH")
	}
}
