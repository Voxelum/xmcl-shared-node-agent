package bootstraprunner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type r3Fixture struct {
	h                                 *Handler
	executor                          *fakeExecutor
	root, dir, statePath, route, body string
	input                             Request
	state                             jobState
}

func newR3Fixture(t *testing.T) r3Fixture {
	t.Helper()
	f := r3Fixture{root: t.TempDir(), executor: newFakeExecutor(), input: validManualRequest()}
	var serverURL string
	h, server := newTestServer(t, f.root, f.executor)
	f.h, serverURL = h, server.URL
	if r := post(t, serverURL+"/v1/bootstrap-jobs", f.input, "r"); r.Code != 202 {
		t.Fatal(r)
	}
	if r := post(t, serverURL+"/v1/bootstrap-jobs/"+f.input.JobID+"/approve", map[string]string{"fingerprint": pinnedFingerprint}, "a"); r.Code != 200 {
		t.Fatal(r)
	}
	if r := post(t, serverURL+"/v1/bootstrap-jobs", f.input, "r"); r.Status != "completed" {
		t.Fatal(r)
	}
	f.dir = filepath.Join(f.root, "jobs", f.input.JobID)
	f.statePath = filepath.Join(f.dir, "state.json")
	var err error
	f.state, err = readJobState(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	f.route = "/v1/bootstrap-jobs/" + f.input.JobID + "/reconcile"
	f.body = fmt.Sprintf(`{"requestDigest":%q,"instanceId":%q,"releaseManifestSha256":%q}`, f.state.RequestDigest, f.input.InstanceID, f.input.Config.ReleaseManifestSHA256)
	return f
}

func r3Snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			files[rel] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func r3Request(f r3Fixture, body, auth, method string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, f.route, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	return w
}

func TestR3StrictJSONAndAuthenticationAreReadOnly(t *testing.T) {
	f := newR3Fixture(t)
	cases := map[string]string{
		"duplicate-digest":      strings.Replace(f.body, `"requestDigest":`, `"requestDigest":"bad","requestDigest":`, 1),
		"duplicate-manifest":    strings.TrimSuffix(f.body, "}") + `,"releaseManifestSha256":null}`,
		"duplicate-escaped-key": strings.TrimSuffix(f.body, "}") + `,"instance\u0049d":"bad"}`,
		"wrong-case-digest":     strings.Replace(f.body, "requestDigest", "RequestDigest", 1),
		"wrong-case-instance":   strings.Replace(f.body, "instanceId", "INSTANCEID", 1),
		"wrong-case-manifest":   strings.Replace(f.body, "releaseManifestSha256", "releaseManifestSHA256", 1),
		"null-instance":         strings.Replace(f.body, fmt.Sprintf("%q", f.input.InstanceID), "null", 1),
		"number-instance":       strings.Replace(f.body, fmt.Sprintf("%q", f.input.InstanceID), "12", 1),
		"bool-instance":         strings.Replace(f.body, fmt.Sprintf("%q", f.input.InstanceID), "true", 1),
		"object-instance":       strings.Replace(f.body, fmt.Sprintf("%q", f.input.InstanceID), "{}", 1),
		"array-instance":        strings.Replace(f.body, fmt.Sprintf("%q", f.input.InstanceID), "[]", 1),
		"unknown-field":         strings.TrimSuffix(f.body, "}") + `,"enrollmentToken":"must-not-be-accepted"}`,
		"trailing-object":       f.body + `{}`,
		"trailing-null":         f.body + `null`,
		"null-body":             "null", "array-body": "[]", "missing-fields": "{}", "truncated": f.body[:len(f.body)-1],
		"oversize": f.body + strings.Repeat(" ", 2100),
	}
	before := r3Snapshot(t, f.root)
	p0, a0 := f.executor.counts()
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := r3Request(f, body, "Bearer "+strings.Repeat("a", 32), "POST")
			if w.Code != 400 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(before, r3Snapshot(t, f.root)) {
				t.Fatal("invalid JSON mutated records")
			}
		})
	}
	for _, auth := range []string{"", "Bearer " + strings.Repeat("r", 32), "Bearer wrong", "Basic abc"} {
		w := r3Request(f, f.body, auth, "POST")
		if w.Code != 401 {
			t.Fatalf("auth accepted: %d", w.Code)
		}
	}
	if w := r3Request(f, f.body, "Bearer "+strings.Repeat("a", 32), "GET"); w.Code != 405 {
		t.Fatal("method accepted")
	}
	if !reflect.DeepEqual(before, r3Snapshot(t, f.root)) {
		t.Fatal("auth/method rejection mutated records")
	}
	if p, a := f.executor.counts(); p != p0 || a != a0 {
		t.Fatal("rejection invoked executor")
	}
}

func TestR3ContradictoryCompletionAndRegistryAreReadOnly(t *testing.T) {
	type mutation struct {
		state    func(*jobState)
		binding  func(*allocationBinding)
		registry func(*allocationRegistry)
		saved    func(*Request)
	}
	cases := map[string]mutation{
		"state-job":                 {state: func(s *jobState) { s.JobID = "wrong-job" }},
		"state-instance":            {state: func(s *jobState) { s.InstanceID = "wrong-instance" }},
		"state-digest":              {state: func(s *jobState) { s.RequestDigest = strings.Repeat("d", 64) }},
		"state-node":                {state: func(s *jobState) { s.NodeID = "wrong-node" }},
		"state-provider-identity":   {state: func(s *jobState) { s.ExpectedProviderIdentity.InstanceID = "wrong-instance" }},
		"no-apply-success":          {state: func(s *jobState) { s.ApplySucceeded = false }},
		"unknown-even-with-success": {state: func(s *jobState) { s.ErrorCode = "apply_outcome_unknown" }},
		"retired-error":             {state: func(s *jobState) { s.ErrorCode = "retired" }},
		"unavailable-error":         {state: func(s *jobState) { s.ErrorCode = "runner_unavailable" }},
		"arbitrary-error":           {state: func(s *jobState) { s.ErrorCode = "enrollment_rejected" }},
		"nonterminal":               {state: func(s *jobState) { s.Terminal = false }},
		"retryable":                 {state: func(s *jobState) { s.Retryable = true }},
		"started":                   {state: func(s *jobState) { s.ApplyStarted = true }},
		"missing-completed-at":      {state: func(s *jobState) { s.CompletedAt = "" }},
		"invalid-completed-at":      {state: func(s *jobState) { s.CompletedAt = "invalid" }},
		"retired-state":             {state: func(s *jobState) { s.Status = "retired" }},
		"retired-at":                {state: func(s *jobState) { s.RetiredAt = canonicalTime(testNow) }},
		"retirement-cooldown":       {state: func(s *jobState) { s.AddressReuseAfter = canonicalTime(testNow) }},
		"unapproved-pin":            {state: func(s *jobState) { s.Decision = "pending" }},
		"saved-job":                 {saved: func(s *Request) { s.JobID = "wrong-job" }},
		"saved-manifest":            {saved: func(s *Request) { s.Config.ReleaseManifestSHA256 = strings.Repeat("b", 64) }},
		"registry-job":              {binding: func(b *allocationBinding) { b.JobID = "wrong-job" }},
		"registry-instance":         {binding: func(b *allocationBinding) { b.InstanceID = "wrong-instance" }},
		"registry-digest":           {binding: func(b *allocationBinding) { b.RequestDigest = strings.Repeat("e", 64) }},
		"registry-address":          {binding: func(b *allocationBinding) { b.Address = "8.8.4.4" }},
		"registry-port":             {binding: func(b *allocationBinding) { b.SSHPort = 2222 }},
		"registry-provider":         {binding: func(b *allocationBinding) { b.Provider = "wrong" }},
		"registry-policy":           {binding: func(b *allocationBinding) { b.PolicyName = "wrong" }},
		"registry-retired":          {binding: func(b *allocationBinding) { b.Status = "retired" }},
		"registry-failed":           {binding: func(b *allocationBinding) { b.Status = "failed" }},
		"registry-not-completed":    {binding: func(b *allocationBinding) { b.Status = "probing_host_key" }},
		"registry-retired-at":       {binding: func(b *allocationBinding) { b.RetiredAt = canonicalTime(testNow) }},
		"registry-quarantine":       {binding: func(b *allocationBinding) { b.QuarantineReason = "apply_outcome_unknown" }},
		"registry-missing":          {registry: func(r *allocationRegistry) { r.Bindings = nil }},
		"registry-duplicate":        {registry: func(r *allocationRegistry) { r.Bindings = append(r.Bindings, r.Bindings[0]) }},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newR3Fixture(t)
			if mutate.state != nil {
				mutate.state(&f.state)
				if err := writeJobState(f.statePath, f.state); err != nil {
					t.Fatal(err)
				}
			}
			if mutate.binding != nil || mutate.registry != nil {
				registry, err := f.h.readRegistry()
				if err != nil {
					t.Fatal(err)
				}
				if mutate.binding != nil {
					mutate.binding(&registry.Bindings[0])
				}
				if mutate.registry != nil {
					mutate.registry(&registry)
				}
				if err := atomicWriteJSON(filepath.Join(f.root, "allocation-registry.json"), registry); err != nil {
					t.Fatal(err)
				}
			}
			if mutate.saved != nil {
				var saved Request
				raw, err := os.ReadFile(filepath.Join(f.dir, "request.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(raw, &saved); err != nil {
					t.Fatal(err)
				}
				mutate.saved(&saved)
				if err = atomicWriteJSON(filepath.Join(f.dir, "request.json"), saved); err != nil {
					t.Fatal(err)
				}
			}
			before := r3Snapshot(t, f.root)
			p0, a0 := f.executor.counts()
			w := r3Request(f, f.body, "Bearer "+strings.Repeat("a", 32), "POST")
			if w.Code != 409 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(before, r3Snapshot(t, f.root)) {
				t.Fatal("conflicting records mutated")
			}
			if p, a := f.executor.counts(); p != p0 || a != a0 {
				t.Fatal("conflict invoked executor")
			}
		})
	}
}

func TestR3CompletedMaintenanceFailureNeverDemotes(t *testing.T) {
	for _, name := range []string{"metadata", "cleanup", "registry-unavailable"} {
		t.Run(name, func(t *testing.T) {
			f := newR3Fixture(t)
			switch name {
			case "metadata":
				p := filepath.Join(f.dir, "request.sha256")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			case "cleanup":
				p := filepath.Join(f.dir, "bootstrap.env")
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "keep"), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			case "registry-unavailable":
				p := filepath.Join(f.root, "allocation-registry.json")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(f.statePath)
			if err != nil {
				t.Fatal(err)
			}
			p0, a0 := f.executor.counts()
			w := r3Request(f, f.body, "Bearer "+strings.Repeat("a", 32), "POST")
			if w.Code != 503 {
				t.Fatalf("expected maintenance failure, got %d %s", w.Code, w.Body.String())
			}
			after, err := os.ReadFile(f.statePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("durable completed fact changed on maintenance failure")
			}
			if p, a := f.executor.counts(); p != p0 || a != a0 {
				t.Fatal("maintenance failure invoked executor")
			}
		})
	}
}

func TestR3ApplyingFinalizeFailureCanRecoverWithoutReapply(t *testing.T) {
	f := newR3Fixture(t)
	f.state.Status = "applying"
	f.state.Terminal = false
	f.state.CompletedAt = ""
	if err := writeJobState(f.statePath, f.state); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(f.dir, "request.sha256")
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	p0, a0 := f.executor.counts()
	w := r3Request(f, f.body, "Bearer "+strings.Repeat("a", 32), "POST")
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, err := readJobState(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "applying" || !after.ApplySucceeded || after.ApplyStarted || after.Terminal || !after.Retryable || after.ErrorCode != "runner_unavailable" || after.CompletedAt != "" {
		t.Fatalf("invalid pending finalization: %#v", after)
	}
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	w = r3Request(f, f.body, "Bearer "+strings.Repeat("a", 32), "POST")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, err = readJobState(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reconcilableCompletion(after) || after.Status != "completed" {
		t.Fatal("finalization did not recover")
	}
	if p, a := f.executor.counts(); p != p0 || a != a0 {
		t.Fatal("recovery reapplied")
	}
}
