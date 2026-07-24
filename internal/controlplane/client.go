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
	bootstrapCredential string
	credentialPath      string
	httpClient          *http.Client
	now                 func() time.Time
	nonce               func() (string, error)
	pollInterval        time.Duration

	mu         sync.RWMutex
	credential string
}

type ClientOptions struct {
	BaseURL             string
	NodeID              string
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
	if options.BootstrapCredential == "" {
		return nil, errors.New("control-plane bootstrap credential is required")
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
	if credential, err := client.readCredential(); err != nil {
		return nil, err
	} else {
		client.credential = credential
	}
	return client, nil
}

// Register uses the bootstrap credential and atomically replaces any previous
// node credential with the short-lived credential issued by the control plane.
func (c *Client) Register(ctx context.Context, capacity NodeCapacity) error {
	body, err := json.Marshal(struct {
		NodeID            string `json:"nodeId"`
		Region            string `json:"region"`
		TotalMemoryMiB    int64  `json:"totalMemoryMiB"`
		TotalSharedCPU    int64  `json:"totalSharedCpu"`
		TotalWorkspaceGiB int64  `json:"totalWorkspaceGiB"`
	}{
		NodeID: c.nodeID, Region: "taipei", TotalMemoryMiB: capacity.TotalMemoryMiB,
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
	}
	if err := json.Unmarshal(response, &registered); err != nil {
		return fmt.Errorf("decode registration response: %w", err)
	}
	if registered.NodeID != c.nodeID {
		return errors.New("registration response has a different node ID")
	}
	return c.setCredential(registered.Credential)
}

// Heartbeat follows the current wire contract, whose endpoint authenticates an
// empty body. Status is retained in the Reporter interface for compatibility
// with a future heartbeat payload.
func (c *Client) Heartbeat(ctx context.Context, _ NodeStatus) error {
	_, err := c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/heartbeat", nil)
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
			LeaseGeneration string       `json:"leaseGeneration"`
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
			if next.LeaseGeneration != "" {
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

// Ack follows the current endpoint contract, which records an acknowledgement
// without a result payload.
func (c *Client) Ack(ctx context.Context, commandID string, _ CommandResult) error {
	_, err := c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/commands/"+commandID+"/ack", nil)
	return err
}

func (c *Client) ReportStarted(ctx context.Context, serviceID, assignmentID string) error {
	body, err := json.Marshal(struct {
		ServiceID string `json:"serviceId"`
	}{ServiceID: serviceID})
	if err != nil {
		return fmt.Errorf("encode started report: %w", err)
	}
	_, err = c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/assignments/"+assignmentID+"/started", body)
	return err
}

func (c *Client) ReportStoppedAndSynced(ctx context.Context, result SyncResult) error {
	body, err := json.Marshal(struct {
		ServiceID string `json:"serviceId"`
		Revision  int64  `json:"revision"`
		SizeBytes int64  `json:"sizeBytes"`
		SHA256    string `json:"sha256,omitempty"`
	}{
		ServiceID: result.ServiceID, Revision: result.Revision, SizeBytes: result.SizeBytes, SHA256: result.ManifestSHA,
	})
	if err != nil {
		return fmt.Errorf("encode stopped-and-synced report: %w", err)
	}
	_, err = c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/assignments/"+result.AssignmentID+"/stopped-synced", body)
	return err
}

// RenewLease supports the planned lease endpoint. It is safe to call only when
// a token or generation was issued; control planes without the endpoint return
// their normal HTTP error and no acknowledgement is sent by this client.
func (c *Client) RenewLease(ctx context.Context, commandID string, lease CommandLease) (CommandLease, error) {
	if lease.Token == "" && lease.Generation == "" {
		return CommandLease{}, errors.New("cannot renew a command without lease fields")
	}
	body, err := json.Marshal(lease)
	if err != nil {
		return CommandLease{}, fmt.Errorf("encode lease renewal: %w", err)
	}
	response, err := c.sendNode(ctx, "/v1/internal/shared-nodes/"+c.nodeID+"/commands/"+commandID+"/lease-renew", body)
	if err != nil {
		return CommandLease{}, err
	}
	var renewed struct {
		CommandLease
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(response, &renewed); err != nil {
		return CommandLease{}, fmt.Errorf("decode lease renewal response: %w", err)
	}
	if renewed.Credential != "" {
		if err := c.setCredential(renewed.Credential); err != nil {
			return CommandLease{}, err
		}
	}
	return renewed.CommandLease, nil
}

func (c *Client) sendNode(ctx context.Context, path string, body []byte) ([]byte, error) {
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

func (c *Client) readCredential() (string, error) {
	credential, err := os.ReadFile(c.credentialPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read control-plane credential: %w", err)
	}
	credential = []byte(strings.TrimSpace(string(credential)))
	if err := validateCredential(c.nodeID, string(credential)); err != nil {
		return "", err
	}
	return string(credential), nil
}

func (c *Client) setCredential(credential string) error {
	if err := validateCredential(c.nodeID, credential); err != nil {
		return err
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
	if _, err := file.WriteString(credential + "\n"); err == nil {
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
	return nil
}

func validateCredential(nodeID, credential string) error {
	if !strings.HasPrefix(credential, nodeID+".") || len(credential) == len(nodeID)+1 {
		return errors.New("control-plane credential does not belong to this node")
	}
	return nil
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
	LeaseGeneration string `json:"leaseGeneration"`
	LeaseExpiresAt  string `json:"leaseExpiresAt"`
}

var _ CommandSource = (*Client)(nil)
var _ Reporter = (*Client)(nil)
var _ LeaseRenewer = (*Client)(nil)
