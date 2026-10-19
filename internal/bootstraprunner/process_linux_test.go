//go:build linux

package bootstraprunner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJobProcessCancellationKillsChildNotOtherJob(t *testing.T) {
	sibling := exec.Command("sleep", "30")
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sibling.Process.Kill(); _ = sibling.Wait() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "fixture", pidFile)
	command.WaitDelay = time.Second
	cleanup, err := configureJobProcess(command)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	started := time.Now()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var childPID int
	for time.Since(started) < 2*time.Second {
		value, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(value)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err = command.Wait()
	cleanup()
	if err == nil || childPID == 0 || time.Since(started) > 4*time.Second {
		t.Fatalf("unbounded/invalid cancellation: child=%d err=%v", childPID, err)
	}
	// A zombie is terminated (not executing); it may await the host's init reaper.
	for deadline := time.Now().Add(time.Second); ; {
		stat, readErr := os.ReadFile("/proc/" + strconv.Itoa(childPID) + "/stat")
		if os.IsNotExist(readErr) || (readErr == nil && strings.Contains(string(stat), ") Z ")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child still executing: %s (%v)", stat, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(sibling.Process.Pid) + "/stat")
	if err != nil || strings.Contains(string(stat), ") Z ") {
		t.Fatalf("unrelated job was killed: %s %v", stat, err)
	}
}
