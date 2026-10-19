package bootstraprunner

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"
)

func newInheritedFixtureHandler(root, secret, approval string, e Executor) (*Handler, error) {
	h, err := NewHandler(root, secret, approval, e)
	if err == nil {
		h.legacyFixtureAutoPin = true
	}
	return h, err
}
func TestIsolatedTOFUDefaultRejectsAndExactAuthorization(t *testing.T) {
	for _, which := range []string{"default-old", "default-new", "missing-operation", "wrong-ip", "wrong-authorization", "exact"} {
		t.Run(which, func(t *testing.T) {
			r := validRequest()
			r.HostKeyPolicy = IsolatedTestTOFUPolicy
			r.OperationID = "mot-op-fixture"
			r.AuthorizationID = "FIXTURE-NOT-HUMAN-APPROVAL"
			g := TestTOFUAuthorization{AuthorizationID: r.AuthorizationID, OperationID: r.OperationID, JobID: r.JobID, InstanceID: r.InstanceID, Address: r.Address, AllocationCreatedAt: r.AllocationCreatedAt, FirstPinExpiresAt: r.AutoPinExpiresAt, EmptyIsolatedTestVM: true, AcceptFirstConnectionMITMRisk: true}
			e := newFakeExecutor()
			root := t.TempDir()
			secret := string(bytes.Repeat([]byte("r"), 32))
			approval := string(bytes.Repeat([]byte("a"), 32))
			h, err := NewIsolatedTestHandler(root, secret, approval, e, g)
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "default-old":
				h, err = NewHandler(root, secret, approval, e)
				r.HostKeyPolicy = AutoPinFreshAllocationPolicy
			case "default-new":
				h, err = NewHandler(root, secret, approval, e)
			case "missing-operation":
				r.OperationID = ""
			case "wrong-ip":
				g.Address = "8.8.4.4"
				h, err = NewIsolatedTestHandler(root, secret, approval, e, g)
			case "wrong-authorization":
				r.AuthorizationID = "different"
			}
			if err != nil {
				t.Fatal(err)
			}
			h.now = func() time.Time { return testNow }
			s := httptest.NewServer(h)
			defer s.Close()
			res := post(t, s.URL+"/v1/bootstrap-jobs", r, "r")
			if which == "exact" {
				if res.Code != 200 || res.Status != "completed" {
					t.Fatalf("%+v", res)
				}
				res = post(t, s.URL+"/v1/bootstrap-jobs", r, "r")
				p, a := e.counts()
				if res.Code != 200 || p != 1 || a != 1 {
					t.Fatalf("duplicate apply: %+v %d %d", res, p, a)
				}
			} else {
				p, a := e.counts()
				if res.Code != 403 || p != 0 || a != 0 {
					t.Fatalf("unauthorized execution: %+v %d %d", res, p, a)
				}
			}
		})
	}
}
func TestIsolatedTOFURequiresRiskAndWindow(t *testing.T) {
	r := validRequest()
	g := TestTOFUAuthorization{AuthorizationID: "fixture", OperationID: "op", JobID: r.JobID, InstanceID: r.InstanceID, Address: r.Address, AllocationCreatedAt: r.AllocationCreatedAt, FirstPinExpiresAt: r.AutoPinExpiresAt, EmptyIsolatedTestVM: true}
	if _, e := NewIsolatedTestHandler(t.TempDir(), "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", newFakeExecutor(), g); e == nil {
		t.Fatal("missing risk consent accepted")
	}
	g.AcceptFirstConnectionMITMRisk = true
	g.FirstPinExpiresAt = canonicalTime(testNow.Add(time.Hour))
	if _, e := NewIsolatedTestHandler(t.TempDir(), "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", newFakeExecutor(), g); e == nil {
		t.Fatal("unbounded window accepted")
	}
}
