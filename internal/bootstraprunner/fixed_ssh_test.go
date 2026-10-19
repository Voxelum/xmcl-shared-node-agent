package bootstraprunner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestFixedSSHIncompleteCommandWaitRemainsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		contextErr error
	}{
		{"wait-delay-with-live-parent", exec.ErrWaitDelay, nil},
		{"wrapped-wait-delay", fmt.Errorf("wrapped: %w", exec.ErrWaitDelay), nil},
		{"context-deadline", errors.New("killed"), context.DeadlineExceeded},
		{"context-cancelled", errors.New("killed"), context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyApplyCommandFailure(tc.err, tc.contextErr)
			if !errors.Is(err, errApplyOutcomeUnknown) || errors.Is(err, ErrRetryableApply) {
				t.Fatalf("incomplete wait must remain unknown, not retryable: %v", err)
			}
		})
	}
}

func TestFixedSSHUnclassifiedCommandFailureIsNotRetried(t *testing.T) {
	err := classifyApplyCommandFailure(errors.New("unrecognized failure"), nil)
	if err == nil || errors.Is(err, ErrRetryableApply) {
		t.Fatalf("unclassified command failure must fail closed: %v", err)
	}
}
