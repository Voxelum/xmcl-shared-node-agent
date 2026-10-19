package bootstraprunner

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStrictTestAuthorizationJSON(t *testing.T) {
	g := TestTOFUAuthorization{AuthorizationID: "fixture", OperationID: "op", JobID: "job", InstanceID: "ecs-fixture", Address: "8.8.8.8", AllocationCreatedAt: canonicalTime(testNow), FirstPinExpiresAt: canonicalTime(testNow.Add(time.Minute)), EmptyIsolatedTestVM: true, AcceptFirstConnectionMITMRisk: true}
	b, _ := json.Marshal(g)
	s := string(b)
	if _, e := DecodeTestTOFUAuthorization(strings.NewReader(s)); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{strings.Replace(s, `"operationId":"op"`, `"operationId":"op","operationId":"bad"`, 1), strings.Replace(s, `"operationId"`, `"OperationId"`, 1), strings.Replace(s, `"operationId":"op"`, `"operationId":null`, 1), s + `{}`, strings.Replace(s, `"emptyIsolatedTestVM":true`, `"emptyIsolatedTestVM":"true"`, 1)} {
		if _, e := DecodeTestTOFUAuthorization(strings.NewReader(bad)); e == nil {
			t.Fatal("bad JSON accepted")
		}
	}
}
func TestIsolatedTOFUWindowAndChangedPin(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed-pin", true: "expired-first-window"}[expired], func(t *testing.T) {
			r := validRequest()
			r.HostKeyPolicy = IsolatedTestTOFUPolicy
			r.AuthorizationID = "fixture"
			r.OperationID = "op"
			g := TestTOFUAuthorization{AuthorizationID: r.AuthorizationID, OperationID: r.OperationID, JobID: r.JobID, InstanceID: r.InstanceID, Address: r.Address, AllocationCreatedAt: r.AllocationCreatedAt, FirstPinExpiresAt: r.AutoPinExpiresAt, EmptyIsolatedTestVM: true, AcceptFirstConnectionMITMRisk: true}
			ex := newFakeExecutor()
			h, e := NewIsolatedTestHandler(t.TempDir(), strings.Repeat("r", 32), strings.Repeat("a", 32), ex, g)
			if e != nil {
				t.Fatal(e)
			}
			h.now = func() time.Time { return testNow }
			s := httptest.NewServer(h)
			defer s.Close()
			if expired {
				h.now = func() time.Time { return testNow.Add(20 * time.Minute) }
			} else {
				h.afterPin = func() error { return errors.New("local crash before apply") }
				post(t, s.URL+"/v1/bootstrap-jobs", r, "r")
				h.afterPin = nil
				ex.fingerprint = changedFingerprint
				ex.knownHosts = "8.8.8.8 ssh-ed25519 AAAAchanged"
			}
			res := post(t, s.URL+"/v1/bootstrap-jobs", r, "r")
			if res.Status != "failed" || res.Retryable {
				t.Fatalf("unsafe result %+v", res)
			}
			_, applies := ex.counts()
			if applies != 0 {
				t.Fatal("unauthorized apply")
			}
		})
	}
}
