package bootstraprunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Grant lookup is read-only; only the separate root broker can persist grants.
// The Unix socket is a deployment capability, never selected by a job request.
type GrantLookup func(string) (*TestTOFUAuthorization, error)

func (h *Handler) WithGrantLookup(lookup GrantLookup) *Handler { h.grantLookup = lookup; return h }
func GrantDigest(g TestTOFUAuthorization) string {
	b, _ := json.Marshal(g)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	b, _ = json.Marshal(m)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func GrantJobID(operation, instance string) string {
	s := sha256.Sum256([]byte("xmcl-lightnode-bootstrap-job-v1\n" + operation + "\n" + instance))
	return "lightnode-bootstrap-" + hex.EncodeToString(s[:])[:40]
}
func GrantNodeID(operation string) string {
	n := regexp.MustCompile(`[^A-Za-z0-9-]`).ReplaceAllString(operation, "-")
	if len(n) > 48 {
		s := sha256.Sum256([]byte("xmcl-lightnode-allocation-id-v1\n" + operation))
		n = n[:8] + "-" + hex.EncodeToString(s[:])[:32] + "-" + n[len(n)-6:]
	}
	return "shared-node-" + n
}
func ValidateDeliveredGrant(g TestTOFUAuthorization) error {
	address, err := netip.ParseAddr(g.Address)
	if err != nil || !isPublicCanonicalIPv4(address, g.Address) || !strings.HasPrefix(g.InstanceID, "ecs-") || !identifier.MatchString(g.InstanceID) || g.JobID != GrantJobID(g.OperationID, g.InstanceID) {
		return errors.New("grant target mismatch")
	}
	created, e1 := time.Parse(time.RFC3339, g.AllocationCreatedAt)
	expires, e2 := time.Parse(time.RFC3339, g.FirstPinExpiresAt)
	if !identifier.MatchString(g.JobID) || !identifier.MatchString(g.OperationID) || !identifier.MatchString(g.AuthorizationID) || !g.EmptyIsolatedTestVM || !g.AcceptFirstConnectionMITMRisk || g.InstanceID == "" || g.Address == "" || e1 != nil || e2 != nil || created.UTC().Format(time.RFC3339) != g.AllocationCreatedAt || expires.UTC().Format(time.RFC3339) != g.FirstPinExpiresAt || !expires.After(created) || expires.Sub(created) > maxAutoPinWindow {
		return errors.New("invalid grant")
	}
	return nil
}
func UnixGrantLookup(socket string) GrantLookup {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }}
	return func(job string) (*TestTOFUAuthorization, error) {
		if !identifier.MatchString(job) {
			return nil, errors.New("invalid job")
		}
		r, e := c.Get("http://grant.local/v1/grants/" + job)
		if e != nil {
			return nil, e
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return nil, errors.New("grant unavailable")
		}
		g, e := DecodeTestTOFUAuthorization(io.LimitReader(r.Body, 8193))
		if e != nil {
			return nil, e
		}
		if e = ValidateDeliveredGrant(g); e != nil {
			return nil, e
		}
		if g.JobID != job {
			return nil, errors.New("foreign grant")
		}
		return &g, nil
	}
}

// Independent submit/approval/grant secrets must never alias.
func ValidateGrantSecret(grant, submit, approval string) bool {
	return len(strings.TrimSpace(grant)) >= 32 && grant != submit && grant != approval
}
