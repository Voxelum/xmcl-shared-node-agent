package bootstraprunner

import (
	"errors"
	"time"
)

// TestTOFUAuthorization is trusted operator configuration, NOT an HTTP grant,
// provider-verified host identity or procurement authority. One new empty VM only.
// The caller must obtain the receipt via the existing verified MOT ownership gate.
type TestTOFUAuthorization struct {
	AuthorizationID               string `json:"authorizationId"`
	OperationID                   string `json:"operationId"`
	JobID                         string `json:"jobId"`
	InstanceID                    string `json:"instanceId"`
	Address                       string `json:"address"`
	AllocationCreatedAt           string `json:"allocationCreatedAt"`
	FirstPinExpiresAt             string `json:"firstPinExpiresAt"`
	EmptyIsolatedTestVM           bool   `json:"emptyIsolatedTestVM"`
	AcceptFirstConnectionMITMRisk bool   `json:"acceptFirstConnectionMitmRisk"`
}

func NewIsolatedTestHandler(root, secret, approvalSecret string, executor Executor, grant TestTOFUAuthorization) (*Handler, error) {
	created, e1 := time.Parse(time.RFC3339, grant.AllocationCreatedAt)
	expires, e2 := time.Parse(time.RFC3339, grant.FirstPinExpiresAt)
	if grant.AuthorizationID == "" || grant.OperationID == "" || grant.JobID == "" || grant.InstanceID == "" || grant.Address == "" || !grant.EmptyIsolatedTestVM || !grant.AcceptFirstConnectionMITMRisk || e1 != nil || e2 != nil || !expires.After(created) || expires.Sub(created) > maxAutoPinWindow {
		return nil, errors.New("invalid isolated test authorization")
	}
	h, err := NewHandler(root, secret, approvalSecret, executor)
	if err == nil {
		h.testGrant = &grant
	}
	return h, err
}
func isAutoPin(policy string) bool {
	return policy == AutoPinFreshAllocationPolicy || policy == IsolatedTestTOFUPolicy
}
func (h *Handler) authorizedTOFU(r Request) bool {
	if h.legacyFixtureAutoPin && r.HostKeyPolicy == AutoPinFreshAllocationPolicy {
		return true
	}
	g := h.testGrant
	if h.grantLookup != nil {
		var err error
		g, err = h.grantLookup(r.JobID)
		if err != nil || r.NodeID != GrantNodeID(r.OperationID) {
			return false
		}
	}
	return g != nil && r.HostKeyPolicy == IsolatedTestTOFUPolicy && r.AuthorizationID == g.AuthorizationID && r.OperationID == g.OperationID && r.JobID == g.JobID && r.InstanceID == g.InstanceID && r.Address == g.Address && r.SSHPort == 22 && r.AllocationCreatedAt == g.AllocationCreatedAt && r.AutoPinExpiresAt == g.FirstPinExpiresAt
}
