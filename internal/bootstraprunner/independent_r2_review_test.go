package bootstraprunner

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reviewer-only fixture; no production source changes, no SSH/network except loopback httptest.
func TestIndependentR2ReconcileSafety(t *testing.T) {
	cases := []string{"duplicate-key", "case-folded-key", "null-overwrite", "state-job-mismatch", "registry-instance-digest-mismatch", "completed-unknown-contradiction", "completed-write-failure"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			executor := newFakeExecutor()
			h, server := newTestServer(t, root, executor)
			input := validManualRequest()
			post(t, server.URL+"/v1/bootstrap-jobs", input, "r")
			post(t, server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/approve", map[string]string{"fingerprint": pinnedFingerprint}, "a")
			if result := post(t, server.URL+"/v1/bootstrap-jobs", input, "r"); result.Status != "completed" {
				t.Fatal(result)
			}
			dir := filepath.Join(root, "jobs", input.JobID)
			sp := filepath.Join(dir, "state.json")
			state, err := readJobState(sp)
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"requestDigest":%q,"instanceId":%q,"releaseManifestSha256":%q}`, state.RequestDigest, input.InstanceID, input.Config.ReleaseManifestSHA256)
			expected := http.StatusConflict
			switch name {
			case "duplicate-key":
				body = strings.Replace(body, `"instanceId":`, `"instanceId":"wrong-instance","instanceId":`, 1)
				expected = 400
			case "case-folded-key":
				body = strings.Replace(body, `"instanceId":`, `"INSTANCEID":`, 1)
				expected = 400
			case "null-overwrite":
				body = strings.TrimSuffix(body, "}") + `,"instanceId":null}`
				expected = 400
			case "state-job-mismatch":
				state.JobID = "different-job"
				if err := writeJobState(sp, state); err != nil {
					t.Fatal(err)
				}
			case "registry-instance-digest-mismatch":
				reg, err := h.readRegistry()
				if err != nil {
					t.Fatal(err)
				}
				reg.Bindings[0].InstanceID = "different-instance"
				reg.Bindings[0].RequestDigest = strings.Repeat("e", 64)
				if err := atomicWriteJSON(filepath.Join(root, "allocation-registry.json"), reg); err != nil {
					t.Fatal(err)
				}
			case "completed-unknown-contradiction":
				state.ErrorCode = "apply_outcome_unknown"
				state.ApplySucceeded = false
				if err := writeJobState(sp, state); err != nil {
					t.Fatal(err)
				}
			case "completed-write-failure":
				// A directory at the metadata destination forces a portable rename failure.
				p := filepath.Join(dir, "request.sha256")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
				expected = 503
			}
			p0, a0 := executor.counts()
			req, err := http.NewRequest("POST", server.URL+"/v1/bootstrap-jobs/"+input.JobID+"/reconcile", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			after, err := readJobState(sp)
			if err != nil {
				t.Fatal(err)
			}
			safe, _ := json.Marshal(map[string]any{"http": resp.StatusCode, "body": string(raw), "stateStatus": after.Status, "stateJobId": after.JobID, "stateError": after.ErrorCode, "completedAtPreserved": after.CompletedAt == state.CompletedAt})
			t.Log(string(safe))
			if p, a := executor.counts(); p != p0 || a != a0 {
				t.Fatal("unexpected executor call")
			}
			if resp.StatusCode != expected {
				t.Errorf("unsafe/inexact reconciliation accepted: got %d want %d", resp.StatusCode, expected)
			}
			if name == "completed-write-failure" && after.Status != "completed" {
				t.Errorf("durable completed state downgraded to %q on metadata failure", after.Status)
			}
		})
	}
}
