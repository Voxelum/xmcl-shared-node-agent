package bootstraprunner

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func cancelBinding(r Request) map[string]string {
	b, _ := json.Marshal(r)
	return map[string]string{"requestDigest": fmt.Sprintf("%x", sha256.Sum256(b)), "instanceId": r.InstanceID, "releaseManifestSha256": r.Config.ReleaseManifestSHA256}
}
func TestExactCancelTerminalizesRetryableBeforeRetire(t *testing.T) {
	e := newFakeExecutor()
	e.applyErr = ErrRetryableApply
	root := t.TempDir()
	_, s := newTestServer(t, root, e)
	r := validRequest()
	for n := 0; n < 3; n++ {
		res := post(t, s.URL+"/v1/bootstrap-jobs", r, "r")
		if !res.Retryable {
			t.Fatalf("%+v", res)
		}
	}
	binding := cancelBinding(r)
	wrong := cancelBinding(r)
	wrong["instanceId"] = "ecs-other"
	before := readStateForTest(t, root, r.JobID)
	res := post(t, s.URL+"/v1/bootstrap-jobs/"+r.JobID+"/cancel", wrong, "a")
	if res.Code != 409 {
		t.Fatalf("%+v", res)
	}
	after := readStateForTest(t, root, r.JobID)
	if before != after {
		t.Fatal("mismatched cancel changed state")
	}
	res = post(t, s.URL+"/v1/bootstrap-jobs/"+r.JobID+"/cancel", binding, "r")
	if res.Code != 401 {
		t.Fatal("submit secret may cancel")
	}
	res = post(t, s.URL+"/v1/bootstrap-jobs/"+r.JobID+"/cancel", binding, "a")
	if res.Code != 200 || res.Retryable {
		t.Fatalf("%+v", res)
	}
	res = post(t, s.URL+"/v1/bootstrap-jobs/"+r.JobID+"/retire", map[string]any{"instanceId": r.InstanceID, "address": r.Address, "sshPort": 22}, "a")
	if res.Code != 200 || res.Status != "retired" {
		t.Fatalf("%+v", res)
	}
	_, applies := e.counts()
	post(t, s.URL+"/v1/bootstrap-jobs", r, "r")
	_, replayed := e.counts()
	if applies != replayed {
		t.Fatal("cancelled job applied again")
	}
}
