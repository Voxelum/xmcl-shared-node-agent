package bootstraprunner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestIndependentR2ConcurrentReconcileRetire(t *testing.T) {
	root := t.TempDir()
	executor := newFakeExecutor()
	h, s := newTestServer(t, root, executor)
	input := validManualRequest()
	post(t, s.URL+"/v1/bootstrap-jobs", input, "r")
	post(t, s.URL+"/v1/bootstrap-jobs/"+input.JobID+"/approve", map[string]string{"fingerprint": pinnedFingerprint}, "a")
	if result := post(t, s.URL+"/v1/bootstrap-jobs", input, "r"); result.Status != "completed" {
		t.Fatal(result)
	}
	sp := filepath.Join(root, "jobs", input.JobID, "state.json")
	before, err := readJobState(sp)
	if err != nil {
		t.Fatal(err)
	}
	reconcile := fmt.Sprintf(`{"requestDigest":%q,"instanceId":%q,"releaseManifestSha256":%q}`, before.RequestDigest, input.InstanceID, input.Config.ReleaseManifestSHA256)
	retire := fmt.Sprintf(`{"instanceId":%q,"address":%q,"sshPort":22}`, input.InstanceID, input.Address)
	p0, a0 := executor.counts()
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			route, body := "reconcile", reconcile
			if i%2 == 1 {
				route, body = "retire", retire
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/bootstrap-jobs/"+input.JobID+"/"+route, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if (route == "retire" && w.Code != 200) || (route == "reconcile" && w.Code != 200 && w.Code != 409) {
				results <- fmt.Sprintf("%s: %d %s", route, w.Code, w.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for bad := range results {
		t.Error(bad)
	}
	after, err := readJobState(sp)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := h.readRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "retired" || reg.Bindings[0].Status != "retired" || after.CompletedAt != before.CompletedAt {
		t.Fatal("retirement/completion record lost")
	}
	if p, a := executor.counts(); p != p0 || a != a0 {
		t.Fatal("unexpected SSH executor call")
	}
	t.Log("32 concurrent reconcile/retire operations: terminal retirement retained; timestamp unchanged; no executor calls")
}
