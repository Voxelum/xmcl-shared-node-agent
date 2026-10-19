package bootstraprunner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type runningJob struct {
	binding reconciliationRequest
	cancel  context.CancelFunc
	done    chan struct{}
}

func (h *Handler) beginRunning(r *http.Request, input Request, digest string) (*http.Request, func(), bool) {
	h.runningMu.Lock()
	defer h.runningMu.Unlock()
	if h.cancelled == nil {
		h.cancelled = map[string]string{}
	}
	if h.cancelled[input.JobID] != "" {
		return r, func() {}, false
	}
	if h.running == nil {
		h.running = map[string]*runningJob{}
	}
	ctx, cancel := context.WithCancel(r.Context())
	job := &runningJob{binding: reconciliationRequest{RequestDigest: digest, InstanceID: input.InstanceID, ReleaseManifestSHA256: input.Config.ReleaseManifestSHA256}, cancel: cancel, done: make(chan struct{})}
	h.running[input.JobID] = job
	return r.WithContext(ctx), func() {
		cancel()
		h.runningMu.Lock()
		delete(h.running, input.JobID)
		close(job.done)
		h.runningMu.Unlock()
	}, true
}

// Cancel is operator-only and exact-bound. It never makes an unknown remote apply
// successful. The lifecycle calls this AFTER exact provider destruction proof.
// A local process-group stop must acknowledge before any terminal/retirement write.
func (h *Handler) cancelJob(w http.ResponseWriter, r *http.Request, id string) {
	expected, err := decodeReconciliationRequest(http.MaxBytesReader(w, r.Body, 2048))
	if err != nil || !identifier.MatchString(id) || !identifier.MatchString(expected.InstanceID) || !sha256Hex.MatchString(expected.RequestDigest) || !sha256Hex.MatchString(expected.ReleaseManifestSHA256) {
		writeError(w, 400, id, "rejected", "invalid_request")
		return
	}
	h.runningMu.Lock()
	active := h.running[id]
	if active != nil && active.binding != expected {
		h.runningMu.Unlock()
		writeError(w, 409, id, "conflict", "idempotency_conflict")
		return
	}
	if active != nil {
		if h.cancelled == nil {
			h.cancelled = map[string]string{}
		}
		h.cancelled[id] = expected.RequestDigest
		active.cancel()
	}
	h.runningMu.Unlock()
	if active != nil {
		select {
		case <-active.done:
		case <-time.After(10 * time.Second):
			writeError(w, 503, id, "unavailable", "runner_unavailable")
			return
		case <-r.Context().Done():
			writeError(w, 503, id, "unavailable", "runner_unavailable")
			return
		}
	}
	lock := h.jobLock(id)
	deadline := time.Now().Add(10 * time.Second)
	for !lock.TryLock() {
		h.runningMu.Lock()
		a := h.running[id]
		if a != nil && a.binding != expected {
			h.runningMu.Unlock()
			writeError(w, 409, id, "conflict", "idempotency_conflict")
			return
		}
		if a != nil {
			if h.cancelled == nil {
				h.cancelled = map[string]string{}
			}
			h.cancelled[id] = expected.RequestDigest
			a.cancel()
		}
		h.runningMu.Unlock()
		if time.Now().After(deadline) || r.Context().Err() != nil {
			writeError(w, 503, id, "unavailable", "runner_unavailable")
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer lock.Unlock()
	dir := filepath.Join(h.root, "jobs", id)
	p := filepath.Join(dir, "state.json")
	state, err := readJobState(p)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, 404, id, "not_found", "job_not_found")
		return
	}
	raw, re := os.ReadFile(filepath.Join(dir, "request.json"))
	var saved Request
	if err != nil || re != nil || json.Unmarshal(raw, &saved) != nil {
		writeError(w, 503, id, "unavailable", "runner_unavailable")
		return
	}
	if state.JobID != id || saved.JobID != id || state.RequestDigest != expected.RequestDigest || state.InstanceID != expected.InstanceID || saved.InstanceID != state.InstanceID || saved.Config.ReleaseManifestSHA256 != expected.ReleaseManifestSHA256 || state.Address != saved.Address || state.SSHPort != saved.SSHPort || state.ExpectedProviderIdentity != saved.ExpectedProviderIdentity || state.HostKeyPolicy != saved.HostKeyPolicy {
		writeError(w, 409, id, "conflict", "idempotency_conflict")
		return
	}
	allocationRegistryMu.Lock()
	registry, re := h.readRegistry()
	matches := 0
	valid := false
	for _, b := range registry.Bindings {
		if b.JobID == id {
			matches++
			valid = b.InstanceID == state.InstanceID && b.RequestDigest == state.RequestDigest && b.Address == state.Address && b.SSHPort == state.SSHPort && b.PolicyName == state.HostKeyPolicy && b.Provider == state.ExpectedProviderIdentity.Provider
		}
	}
	allocationRegistryMu.Unlock()
	if re != nil {
		writeError(w, 503, id, "unavailable", "runner_unavailable")
		return
	}
	if matches != 1 || !valid {
		writeError(w, 409, id, "conflict", "idempotency_conflict")
		return
	}
	if state.Status == "completed" || state.Status == "retired" {
		writeStatus(w, 200, id, state.Status, state.Fingerprint)
		return
	}
	// Success evidence is not overwritten; use R3 reconcile for that case.
	if state.ApplySucceeded {
		writeError(w, 409, id, "failed", "apply_outcome_unknown", false)
		return
	}
	state.Status = "failed"
	state.ErrorCode = "terminal_apply_failure"
	state.Retryable = false
	state.Terminal = true
	state.ApplyStarted = false
	if err = os.Remove(filepath.Join(dir, "bootstrap.env")); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, 503, id, "unavailable", "runner_unavailable")
		return
	}
	if writeJobState(p, state) != nil || h.updateAllocation(id, "failed", state.ErrorCode, h.now()) != nil {
		writeError(w, 503, id, "unavailable", "runner_unavailable")
		return
	}
	writeStateError(w, 200, state, state.ErrorCode, false)
}
