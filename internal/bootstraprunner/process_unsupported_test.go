//go:build !linux

package bootstraprunner

import (
	"context"
	"errors"
	"testing"
)

func TestShellExecutorRejectsUnsupportedPlatformBeforeLaunch(t *testing.T) {
	// Even an absent runner path must return platform rejection, not a launch error.
	e := ShellExecutor{RunnerScript: "must-not-execute", Port: 22}
	if _, _, err := e.Probe(context.Background(), "192.0.2.1", 22); !errors.Is(err, errUnsupportedJobProcess) {
		t.Fatalf("Probe did not fail closed: %v", err)
	}
	if err := e.Apply(context.Background(), "192.0.2.1", 22, "unused", "unused", "unused"); !errors.Is(err, errUnsupportedJobProcess) {
		t.Fatalf("Apply did not fail closed: %v", err)
	}
}
