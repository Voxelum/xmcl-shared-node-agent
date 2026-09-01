package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxControlPlaneResponseBytes = 1 << 20

// Client is the authenticated outbound client for shared-node control-plane
// endpoints. It deliberately relies on the standard TLS verifier and only
// accepts HTTPS control-plane URLs.
type Client struct {
	baseURL             *url.URL
	nodeID              string
	instanceID          string
	region              string
	bootstrapCredential string
	credentialPath      string
	httpClient          *http.Client
	now                 func() time.Time
	nonce               func() (string, error)
	pollInterval        time.Duration

	authMu     sync.RWMutex
	mu         sync.RWMutex
	credential string
	expiresAt  string
}

type ClientOptions struct {
	BaseURL             string
	NodeID              string
	InstanceID          string
	Region              string
	BootstrapCredential string
	CredentialPath      string
	HTTPClient          *http.Client
	Now                 func() time.Time
	Nonce               func() (string, error)
	PollInterval        time.Duration
}

// HTTPError is returned for non-successful control-plane responses.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("control plane returned HTTP %d", e.StatusCode)
}

func NewClient(options ClientOptions) (*Client, error) {
	if options.NodeID == "" {
		return nil, errors.New("control-plane node ID is required")
	}
	if !validRegion(options.Region) {
		return nil, errors.New("control-plane shared-node region is invalid")
	}
	if options.CredentialPath == "" {
		return nil, errors.New("control-plane credential path is required")
	}
	baseURL, err := url.Parse(options.BaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" {
		return nil, errors.New("control-plane URL must be an absolute HTTPS URL")
	}
	if baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("control-plane URL must not include a query or fragment")
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 45 * time.Second}
	}
	client := &Client{
		baseURL:             baseURL,
		nodeID:              options.NodeID,
		instanceID:          options.InstanceID,
		region:              options.Region,
		bootstrapCredential: options.BootstrapCredential,
		credentialPath:      options.CredentialPath,
		httpClient:          httpClient,
		now:                 options.Now,
		nonce:               options.Nonce,
		pollInterval:        options.PollInterval,
	}
	if client.now == nil {
		client.now = time.Now
	}
	if client.nonce == nil {
		client.nonce = randomNonce
	}
	if client.pollInterval <= 0 {
		client.pollInterval = time.Second
	}
	if credential, expiresAt, err := client.readCredential(); err != nil {
		return nil, err
	} else {
		client.credential = credential
		client.expiresAt = expiresAt
	}
	if client.credential == "" && client.bootstrapCredential == "" {
		return nil, errors.New("control-plane bootstrap credential is required without a persisted node credential")
	}
	return client, nil
}

// Register consumes the one-time bootstrap credential and atomically persists
// the short-lived node credential returned by the control plane. Bootstrap is
// intentionally never used to refresh an already enrolled node.
func (c *Client) Register(ctx context.Context, capacity NodeCapacity) error {
	if c.bootstrapCredential == "" {
		return errors.New("control-plane bootstrap credential is unavailable")
	}
	if c.instanceID == "" {
		return errors.New("control-plane instance ID is required for registration")
	}
	body, err := json.Marshal(struct {
		NodeID            string `json:"nodeId"`
		InstanceID        string `json:"instanceId"`
		Region            string `json:"region"`
		TotalMemoryMiB    int64  `json:"totalMemoryMiB"`
		TotalSharedCPU    int64  `json:"totalSharedCpu"`
		TotalWorkspaceGiB int64  `json:"totalWorkspaceGiB"`
	}{
		NodeID: c.nodeID, InstanceID: c.instanceID, Region: c.region, TotalMemoryMiB: capacity.TotalMemoryMiB,
		TotalSharedCPU: capacity.TotalSharedCPU, TotalWorkspaceGiB: capacity.TotalWorkspaceGiB,
	})
	if err != nil {
		return fmt.Errorf("encode registration: %w", err)
	}
	response, err := c.send(ctx, "/v1/internal/shared-nodes/register", body, "SharedNode-Bootstrap", c.bootstrapCredential)
	if err != nil {
		return err
	}
	var registered struct {
		NodeID     string `json:"nodeId"`
		Credential string `json:"credential"`
		ExpiresAt  string `json:"expiresAt"`
	}
	if err := json.Unmarshal(response, &registered); err != nil {
		return fmt.Errorf("decode registration response: %w", err)
	}
	if registered.NodeID != c.nodeID {
		return errors.New("registration response has a different node ID")
	}
	if err := c.setCredential(registered.Credential, registered.ExpiresAt); err != nil {
		return err
	}
	c.bootstrapCredential = ""
	return nil
}

// CredentialNeedsRotation reports whether the credential is close enough to
// expiry that the daemon must rotate it before continuing normal work. A
// legacy credential without persisted expiry is deliberately rotated rather
// than re-enrolled with bootstrap.
func (c *Client) CredentialNeedsRotation() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.credential == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, c.expiresAt)
	return err != nil || !expiresAt.After(c.now().Add(2*time.Minute))
}

// RotateCredential exchanges a still-authenticated node credential for a new
// short-lived credential. It must be called before expiry; it never falls back
// to the consumed bootstrap credential.
func (c *Client) RotateCredential(ctx context.Context) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	response, err := c.sendNodeLocked(
		ctx,
		"/v1/internal/shared-nodes/"+c.nodeID+"/credentials:rotate",
		nil,
	)
	if err != nil {
		return err
	}
	var rotated struct {
		NodeID     string `json:"nodeId"`
		Credential string `json:"credential"`
		ExpiresAt  string `json:"expiresAt"`
	}
	if err := json.Unmarshal(response, &rotated); err != nil {
		return fmt.Errorf("decode credential rotation response: %w", err)
	}
	if rotated.NodeID != c.nodeID {
		return errors.New("credential rotation response has a different node ID")
	}
	return c.setCredential(rotated.Credential, rotated.ExpiresAt)
}

func (c *Client) Heartbeat(ctx context.Context, status NodeStatus) error {
	if !status.Valid() {
		return errors.New("invalid shared-node heartbeat status")
	}
	body, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("encode heartbeat: %w", err)
	}
	_, err = c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/heartbeat", body)
	return err
}

func (c *Client) Next(ctx context.Context, nodeID string) (Command, error) {
	if nodeID != c.nodeID {
		return Command{}, fmt.Errorf("command requested for node %q, client belongs to %q", nodeID, c.nodeID)
	}
	path := "/v1/internal/shared-nodes/" + c.nodeID + "/commands:next"
	for {
		response, err := c.sendNode(ctx, path, nil)
		if err != nil {
			return Command{}, err
		}
		var next struct {
			Command         *wireCommand `json:"command"`
			LeaseToken      string       `json:"leaseToken"`
			LeaseGeneration int64        `json:"leaseGeneration"`
			LeaseExpiresAt  string       `json:"leaseExpiresAt"`
		}
		if err := json.Unmarshal(response, &next); err != nil {
			return Command{}, fmt.Errorf("decode next command response: %w", err)
		}
		if next.Command != nil {
			command := next.Command.Command
			command.Lease = CommandLease{
				Token: next.Command.LeaseToken, Generation: next.Command.LeaseGeneration, ExpiresAt: next.Command.LeaseExpiresAt,
			}
			if next.LeaseToken != "" {
				command.Lease.Token = next.LeaseToken
			}
			if next.LeaseGeneration != 0 {
				command.Lease.Generation = next.LeaseGeneration
			}
			if next.LeaseExpiresAt != "" {
				command.Lease.ExpiresAt = next.LeaseExpiresAt
			}
			return command, nil
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return Command{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// Ack records an acknowledgement and its bounded outcome for the current lease.
func (c *Client) Ack(ctx context.Context, commandID string, lease CommandLease, result CommandResult) error {
	if lease.Token == "" || lease.Generation < 1 {
		return errors.New("cannot acknowledge a command without a lease token and generation")
	}
	if result.Status == "" {
		return errors.New("cannot acknowledge a command without a result status")
	}
	body, err := json.Marshal(struct {
		Token      string `json:"leaseToken"`
		Generation int64  `json:"leaseGeneration"`
		Status     string `json:"status"`
		Code       string `json:"code,omitempty"`
		Message    string `json:"message,omitempty"`
	}{
		Token: lease.Token, Generation: lease.Generation,
		Status: result.Status, Code: result.Code, Message: result.Message,
	})
	if err != nil {
		return fmt.Errorf("encode acknowledgement: %w", err)
	}
	_, err = c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/commands/"+commandID+"/ack", body)
	return err
}

func (c *Client) ReportStarted(ctx context.Context, serviceID, assignmentID string, endpoint Endpoint) error {
	body, err := json.Marshal(struct {
		ServiceID string   `json:"serviceId"`
		Endpoint  Endpoint `json:"endpoint"`
	}{ServiceID: serviceID, Endpoint: endpoint})
	if err != nil {
		return fmt.Errorf("encode started report: %w", err)
	}
	_, err = c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/assignments/"+assignmentID+"/started", body)
	return err
}

func (c *Client) ReportStopped(ctx context.Context, report StoppedReport) error {
	if report.CommandID == "" || report.Lease.Token == "" ||
		report.Lease.Generation < 1 {
		return errors.New("stopped report requires the current command lease")
	}
	body, err := json.Marshal(struct {
		ServiceID       string `json:"serviceId"`
		CommandID       string `json:"commandId"`
		LeaseToken      string `json:"leaseToken"`
		LeaseGeneration int64  `json:"leaseGeneration"`
	}{
		ServiceID: report.ServiceID, CommandID: report.CommandID,
		LeaseToken: report.Lease.Token, LeaseGeneration: report.Lease.Generation,
	})
	if err != nil {
		return fmt.Errorf("encode stopped report: %w", err)
	}
	_, err = c.sendNode(
		ctx,
		"/v1/internal/shared-nodes/"+c.nodeID+"/assignments/"+
			report.AssignmentID+"/stopped",
		body,
	)
	return err
}

func (c *Client) ReportStoppedAndSynced(ctx context.Context, result SyncResult) error {
	if result.CommandID == "" || result.Lease.Token == "" || result.Lease.Generation < 1 {
		return errors.New("stopped-and-synced report requires the current command lease")
	}
	body, err := json.Marshal(struct {
		ServiceID       string `json:"serviceId"`
		CommandID       string `json:"commandId"`
		LeaseToken      string `json:"leaseToken"`
		LeaseGeneration int64  `json:"leaseGeneration"`
		Revision        int64  `json:"revision"`
		SizeBytes       int64  `json:"sizeBytes"`
		SHA256          string `json:"sha256,omitempty"`
	}{
		ServiceID: result.ServiceID, CommandID: result.CommandID,
		LeaseToken: result.Lease.Token, LeaseGeneration: result.Lease.Generation,
		Revision: result.Revision, SizeBytes: result.SizeBytes, SHA256: result.ManifestSHA,
	})
	if err != nil {
		return fmt.Errorf("encode stopped-and-synced report: %w", err)
	}
	_, err = c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/assignments/"+result.AssignmentID+"/stopped-synced", body)
	return err
}

// RenewLease extends the current command lease. The control plane currently
// returns only leaseExpiresAt, so token and generation are retained unless a
// future response explicitly replaces them.
func (c *Client) RenewLease(ctx context.Context, commandID string, lease CommandLease) (CommandLease, error) {
	if lease.Token == "" || lease.Generation < 1 {
		return CommandLease{}, errors.New("cannot renew a command without a lease token and generation")
	}
	body, err := json.Marshal(struct {
		Token      string `json:"leaseToken"`
		Generation int64  `json:"leaseGeneration"`
	}{Token: lease.Token, Generation: lease.Generation})
	if err != nil {
		return CommandLease{}, fmt.Errorf("encode lease renewal: %w", err)
	}
	response, err := c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/commands/"+commandID+"/lease-renew", body)
	if err != nil {
		return CommandLease{}, err
	}
	var renewed struct {
		Token      *string `json:"leaseToken"`
		Generation *int64  `json:"leaseGeneration"`
		ExpiresAt  string  `json:"leaseExpiresAt"`
	}
	if err := json.Unmarshal(response, &renewed); err != nil {
		return CommandLease{}, fmt.Errorf("decode lease renewal response: %w", err)
	}
	if renewed.ExpiresAt == "" {
		return CommandLease{}, errors.New("lease renewal response is missing lease expiry")
	}
	if renewed.Token != nil {
		lease.Token = *renewed.Token
	}
	if renewed.Generation != nil {
		lease.Generation = *renewed.Generation
	}
	lease.ExpiresAt = renewed.ExpiresAt
	return lease, nil
}

// HasCredential reports whether a locally validated node credential was loaded
// or issued. It deliberately does not claim that the remote control plane still
// accepts the credential.
func (c *Client) HasCredential() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.credential != ""
}

// IsAuthenticationFailure identifies the only response statuses that may
// safely trigger credential invalidation and re-enrollment.
func (c *Client) IsAuthenticationFailure(err error) bool {
	var responseError *HTTPError
	return errors.As(err, &responseError) &&
		(responseError.StatusCode == http.StatusUnauthorized || responseError.StatusCode == http.StatusForbidden)
}

// InvalidateCredential removes a credential only after an explicit remote
// authentication rejection. It never logs or returns the credential value.
func (c *Client) InvalidateCredential() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Remove(c.credentialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove rejected control-plane credential: %w", err)
	}
	c.credential = ""
	c.expiresAt = ""
	return nil
}

func (c *Client) RestoreWorkspaceGrants(ctx context.Context, command Command, stage string, keys []string) (WorkspaceGrantResponse, error) {
	return c.workspaceGrants(ctx, "restore", command, stage, keys, nil, "")
}

func (c *Client) SyncWorkspaceGrants(ctx context.Context, command Command, manifest WorkspaceManifest, manifestSHA256 string, keys []string) (WorkspaceGrantResponse, error) {
	return c.workspaceGrants(ctx, "sync", command, "", keys, &manifest, manifestSHA256)
}

func (c *Client) PublishWorkspaceGrant(ctx context.Context, command Command, manifest WorkspaceManifest, manifestSHA256 string) (WorkspaceGrantResponse, error) {
	return c.workspaceGrants(ctx, "publish", command, "", nil, &manifest, manifestSHA256)
}

func (c *Client) workspaceGrants(ctx context.Context, operation string, command Command, stage string, keys []string, manifest *WorkspaceManifest, manifestSHA256 string) (WorkspaceGrantResponse, error) {
	if command.CommandID == "" || command.AssignmentID == "" || command.Lease.Token == "" || command.Lease.Generation < 1 {
		return WorkspaceGrantResponse{}, errors.New("workspace grant requires a currently leased command")
	}
	body, err := json.Marshal(struct {
		ContractVersion int                `json:"contractVersion"`
		CommandID       string             `json:"commandId"`
		AssignmentID    string             `json:"assignmentId"`
		LeaseToken      string             `json:"leaseToken"`
		LeaseGeneration int64              `json:"leaseGeneration"`
		Stage           string             `json:"stage,omitempty"`
		Keys            []string           `json:"keys,omitempty"`
		Manifest        *WorkspaceManifest `json:"manifest,omitempty"`
		ManifestSHA256  string             `json:"manifestSha256,omitempty"`
	}{
		ContractVersion: WorkspaceGrantContractVersion,
		CommandID:       command.CommandID, AssignmentID: command.AssignmentID,
		LeaseToken: command.Lease.Token, LeaseGeneration: command.Lease.Generation,
		Stage: stage, Keys: keys, Manifest: manifest, ManifestSHA256: manifestSHA256,
	})
	if err != nil {
		return WorkspaceGrantResponse{}, fmt.Errorf("encode workspace grant request: %w", err)
	}
	response, err := c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/workspace-grants/"+operation, body)
	if err != nil {
		return WorkspaceGrantResponse{}, err
	}
	var grants WorkspaceGrantResponse
	if err := json.Unmarshal(response, &grants); err != nil {
		return WorkspaceGrantResponse{}, fmt.Errorf("decode workspace grant response: %w", err)
	}
	if grants.ContractVersion != WorkspaceGrantContractVersion {
		return WorkspaceGrantResponse{}, errors.New("workspace grant response has an unsupported contract version")
	}
	return grants, nil
}

func (c *Client) sendNode(ctx context.Context, path string, body []byte) ([]byte, error) {
	c.authMu.RLock()
	defer c.authMu.RUnlock()
	return c.sendNodeLocked(ctx, path, body)
}

func (c *Client) sendNodeLocked(ctx context.Context, path string, body []byte) ([]byte, error) {
	c.mu.RLock()
	credential := c.credential
	c.mu.RUnlock()
	if credential == "" {
		return nil, errors.New("control-plane node credential is unavailable; register first")
	}
	return c.send(ctx, path, body, "SharedNode", credential)
}

func (c *Client) send(ctx context.Context, path string, body []byte, scheme, credential string) ([]byte, error) {
	if body == nil {
		body = []byte{}
	}
	timestamp := fmt.Sprintf("%d", c.now().UnixMilli())
	nonce, err := c.nonce()
	if err != nil {
		return nil, fmt.Errorf("generate request nonce: %w", err)
	}
	bodyHash := sha256.Sum256(body)
	payload := strings.Join([]string{
		http.MethodPost, path, timestamp, nonce, hex.EncodeToString(bodyHash[:]),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(credential))
	_, _ = mac.Write([]byte(payload))

	requestURL := *c.baseURL
	requestURL.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	requestURL.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create control-plane request: %w", err)
	}
	request.Header.Set("Authorization", scheme+" "+credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-XMCL-Timestamp", timestamp)
	request.Header.Set("X-XMCL-Nonce", nonce)
	request.Header.Set("X-XMCL-Body-SHA256", hex.EncodeToString(bodyHash[:]))
	request.Header.Set("X-XMCL-Signature", hex.EncodeToString(mac.Sum(nil)))

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send control-plane request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxControlPlaneResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read control-plane response: %w", err)
	}
	if len(responseBody) > maxControlPlaneResponseBytes {
		return nil, errors.New("control-plane response exceeds maximum size")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &HTTPError{StatusCode: response.StatusCode}
	}
	return responseBody, nil
}

func (c *Client) readCredential() (string, string, error) {
	data, err := os.ReadFile(c.credentialPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read control-plane credential: %w", err)
	}
	var persisted struct {
		Credential string `json:"credential"`
		ExpiresAt  string `json:"expiresAt"`
	}
	if json.Unmarshal(data, &persisted) == nil && persisted.Credential != "" {
		if err := validateCredential(c.nodeID, persisted.Credential); err != nil {
			return "", "", err
		}
		if _, err := time.Parse(time.RFC3339, persisted.ExpiresAt); err != nil {
			return "", "", errors.New("control-plane credential expiry is invalid")
		}
		return persisted.Credential, persisted.ExpiresAt, nil
	}
	// Upgrade legacy files by retaining the credential only long enough to
	// perform an authenticated rotation. Missing expiry never enables bootstrap.
	credential := strings.TrimSpace(string(data))
	if err := validateCredential(c.nodeID, credential); err != nil {
		return "", "", err
	}
	return credential, "", nil
}

func (c *Client) setCredential(credential string, expiresAt ...string) error {
	if err := validateCredential(c.nodeID, credential); err != nil {
		return err
	}
	expiry := ""
	if len(expiresAt) > 0 {
		expiry = expiresAt[0]
		if parsed, err := time.Parse(time.RFC3339, expiry); err != nil || !parsed.After(c.now()) {
			return errors.New("control-plane credential expiry is invalid")
		}
	}
	persisted, err := json.Marshal(struct {
		Credential string `json:"credential"`
		ExpiresAt  string `json:"expiresAt"`
	}{Credential: credential, ExpiresAt: expiry})
	if err != nil {
		return fmt.Errorf("encode control-plane credential: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.credentialPath), 0o700); err != nil {
		return fmt.Errorf("create control-plane credential directory: %w", err)
	}
	temporary := c.credentialPath + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create control-plane credential: %w", err)
	}
	if _, err := file.Write(append(persisted, '\n')); err == nil {
		err = file.Chmod(0o600)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("write control-plane credential: %w", err)
	}
	if err := os.Rename(temporary, c.credentialPath); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("persist control-plane credential: %w", err)
	}
	c.credential = credential
	c.expiresAt = expiry
	return nil
}

func validateCredential(nodeID, credential string) error {
	if !strings.HasPrefix(credential, nodeID+".") || len(credential) == len(nodeID)+1 {
		return errors.New("control-plane credential does not belong to this node")
	}
	return nil
}

func validRegion(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			continue
		}
		if character == '-' && index > 0 && index < len(value)-1 {
			continue
		}
		return false
	}
	return value[len(value)-1] != '-'
}

func randomNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

type wireCommand struct {
	Command
	LeaseToken      string `json:"leaseToken"`
	LeaseGeneration int64  `json:"leaseGeneration"`
	LeaseExpiresAt  string `json:"leaseExpiresAt"`
}

var _ CommandSource = (*Client)(nil)
var _ Reporter = (*Client)(nil)
var _ LeaseRenewer = (*Client)(nil)
var _ WorkspaceGrantClient = (*Client)(nil)
