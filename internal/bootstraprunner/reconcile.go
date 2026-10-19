package bootstraprunner

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

type reconciliationRequest struct {
	RequestDigest         string
	InstanceID            string
	ReleaseManifestSHA256 string
}

// Like retirement, use token-level exact keys. Struct decoding is deliberately
// not used: it accepts case-folded names, duplicate keys and null string values.
func decodeReconciliationRequest(reader io.Reader) (reconciliationRequest, error) {
	var input reconciliationRequest
	decoder := json.NewDecoder(reader)
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return input, errors.New("reconciliation requires an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return input, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return input, errors.New("duplicate or invalid reconciliation key")
		}
		seen[key] = true
		token, err = decoder.Token()
		if err != nil {
			return input, err
		}
		value, ok := token.(string)
		if !ok {
			return input, errors.New("reconciliation values must be non-null strings")
		}
		switch key {
		case "requestDigest":
			input.RequestDigest = value
		case "instanceId":
			input.InstanceID = value
		case "releaseManifestSha256":
			input.ReleaseManifestSHA256 = value
		default:
			return input, errors.New("unknown reconciliation key")
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 3 {
		return input, errors.New("incomplete reconciliation request")
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return input, errors.New("trailing reconciliation JSON")
	}
	return input, nil
}

// Reconciliation never probes or applies. Its entire validation and finalization
// run under the existing job lock, shared with retire and submit.
func (h *Handler) reconcile(w http.ResponseWriter, r *http.Request, jobID string) {
	expected, err := decodeReconciliationRequest(http.MaxBytesReader(w, r.Body, 2048))
	if !identifier.MatchString(jobID) || err != nil || !sha256Hex.MatchString(expected.RequestDigest) ||
		!sha256Hex.MatchString(expected.ReleaseManifestSHA256) || !identifier.MatchString(expected.InstanceID) {
		writeError(w, http.StatusBadRequest, jobID, "rejected", "invalid_request")
		return
	}
	lock := h.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	jobDir := filepath.Join(h.root, "jobs", jobID)
	statePath := filepath.Join(jobDir, "state.json")
	state, err := readJobState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, jobID, "not_found", "job_not_found")
		return
	}
	var saved Request
	raw, readErr := os.ReadFile(filepath.Join(jobDir, "request.json"))
	if err != nil || readErr != nil || json.Unmarshal(raw, &saved) != nil {
		writeError(w, http.StatusServiceUnavailable, jobID, "unavailable", "runner_unavailable")
		return
	}
	if state.JobID != jobID || state.RequestDigest != expected.RequestDigest || state.InstanceID != expected.InstanceID ||
		saved.JobID != jobID || saved.InstanceID != expected.InstanceID ||
		saved.Config.ReleaseManifestSHA256 != expected.ReleaseManifestSHA256 ||
		state.NodeID != saved.NodeID || state.Address != saved.Address || state.SSHPort != saved.SSHPort ||
		state.ExpectedProvider != saved.ExpectedProvider || state.ExpectedProviderIdentity != saved.ExpectedProviderIdentity ||
		state.HostKeyPolicy != saved.HostKeyPolicy || state.AllocationCreatedAt != saved.AllocationCreatedAt ||
		state.AutoPinExpiresAt != saved.AutoPinExpiresAt {
		writeError(w, http.StatusConflict, jobID, "conflict", "idempotency_conflict")
		return
	}
	if !reconcilableCompletion(state) {
		writeError(w, http.StatusConflict, jobID, "failed", "apply_outcome_unknown", false)
		return
	}
	matches, registryErr := h.reconciliationAllocationMatches(state)
	if registryErr != nil {
		writeError(w, http.StatusServiceUnavailable, jobID, "unavailable", "runner_unavailable")
		return
	}
	if !matches {
		writeError(w, http.StatusConflict, jobID, "conflict", "idempotency_conflict")
		return
	}
	h.finalizeSuccessfulApply(w, saved, statePath, jobDir, state)
}

func reconcilableCompletion(state jobState) bool {
	if !state.ApplySucceeded || state.ApplyStarted || state.RetiredAt != "" || state.AddressReuseAfter != "" ||
		!validProbe(state.KnownHosts, state.Fingerprint, state.Address, state.SSHPort) {
		return false
	}
	if (state.HostKeyPolicy != ManualApprovalPolicy || state.Decision != "approved") &&
		(!isAutoPin(state.HostKeyPolicy) || state.Decision != "auto_approved") {
		return false
	}
	switch state.Status {
	case "completed":
		completed, err := parseCanonicalTime(state.CompletedAt)
		return state.Terminal && !state.Retryable && state.ErrorCode == "" && err == nil && !completed.IsZero()
	case "applying":
		return !state.Terminal && state.CompletedAt == "" &&
			((state.ErrorCode == "" && !state.Retryable) || (state.ErrorCode == "runner_unavailable" && state.Retryable))
	default:
		return false
	}
}

func (h *Handler) reconciliationAllocationMatches(state jobState) (bool, error) {
	allocationRegistryMu.Lock()
	defer allocationRegistryMu.Unlock()
	registry, err := h.readRegistry()
	if err != nil {
		return false, err
	}
	found := 0
	matches := false
	for _, binding := range registry.Bindings {
		if binding.JobID != state.JobID {
			continue
		}
		found++
		phaseMatches := binding.Status == "completed" ||
			(state.Status == "applying" && binding.Status == "probing_host_key")
		matches = phaseMatches && binding.InstanceID == state.InstanceID && binding.RequestDigest == state.RequestDigest &&
			binding.Address == state.Address && binding.SSHPort == state.SSHPort &&
			binding.Provider == state.ExpectedProviderIdentity.Provider && binding.PolicyName == state.HostKeyPolicy &&
			binding.RetiredAt == "" && binding.QuarantineReason == ""
	}
	return found == 1 && matches, nil
}
