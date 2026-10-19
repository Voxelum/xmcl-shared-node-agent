//go:build linux

package bootstraprunner

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type processCancelExecutor struct{ pidFile string }

func (e *processCancelExecutor) Probe(context.Context, string, int) (string, string, error) {
	return newFakeExecutor().knownHosts, pinnedFingerprint, nil
}
func (e *processCancelExecutor) Apply(ctx context.Context, _ string, _ int, _, _, _ string) error {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "fixture", e.pidFile)
	c.WaitDelay = time.Second
	cleanup, err := configureJobProcess(c)
	if err != nil {
		return err
	}
	defer cleanup()
	_ = c.Run()
	return errApplyOutcomeUnknown
}
func TestHTTPExactCancelKillsLinuxJobGroupAndAllowsRetire(t *testing.T) {
	sibling := exec.Command("sleep", "30")
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { sibling.Process.Kill(); sibling.Wait() }()
	r := validRequest()
	e := &processCancelExecutor{pidFile: filepath.Join(t.TempDir(), "child.pid")}
	root := t.TempDir()
	h, err := newInheritedFixtureHandler(root, strings.Repeat("r", 32), strings.Repeat("a", 32), e)
	if err != nil {
		t.Fatal(err)
	}
	h.now = func() time.Time { return testNow }
	s := httptest.NewServer(h)
	defer s.Close()
	done := make(chan response, 1)
	go func() { done <- post(t, s.URL+"/v1/bootstrap-jobs", r, "r") }()
	var pid int
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); {
		if b, err := os.ReadFile(e.pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child not started")
	}
	binding := cancelBinding(r)
	wrong := cancelBinding(r)
	wrong["requestDigest"] = strings.Repeat("b", 64)
	bad := post(t, s.URL+"/v1/bootstrap-jobs/"+r.JobID+"/cancel", wrong, "a")
	if bad.Code != 409 {
		t.Fatalf("%+v", bad)
	}
	res := post(t, s.URL+"/v1/bootstrap-jobs/"+r.JobID+"/cancel", binding, "a")
	if res.Code != 200 {
		t.Fatalf("%+v", res)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("apply did not stop")
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil && !strings.Contains(string(b), ") Z ") {
		t.Fatalf("child executing: %s", b)
	}
	b, err = os.ReadFile("/proc/" + strconv.Itoa(sibling.Process.Pid) + "/stat")
	if err != nil || strings.Contains(string(b), ") Z ") {
		t.Fatal("other job was killed")
	}
	res = post(t, s.URL+"/v1/bootstrap-jobs/"+r.JobID+"/retire", map[string]any{"instanceId": r.InstanceID, "address": r.Address, "sshPort": 22}, "a")
	if res.Code != 200 || res.Status != "retired" {
		t.Fatalf("%+v", res)
	}
}
