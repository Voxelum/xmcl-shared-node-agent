package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNodeStatusRejectsNonFiniteServiceCPU(t *testing.T) {
	t.Parallel()
	for name, cpu := range map[string]float64{
		"nan":      math.NaN(),
		"positive": math.Inf(1),
		"negative": math.Inf(-1),
	} {
		t.Run(name, func(t *testing.T) {
			status := NodeStatus{
				ContractVersion: SharedNodeContractVersion,
				Status:          "ready",
				AgentVersion:    "test-agent",
				Ingress:         Ingress{Host: "node.example"},
				Services: []ServiceStatus{{
					ServiceID: "service-1", AssignmentID: "assignment-1",
					CPUPercent: cpu, MemoryUsageMiB: 1, MemoryLimitMiB: 2,
				}},
			}
			if status.Valid() {
				t.Fatalf("CPU percentage %v was accepted", cpu)
			}
		})
	}
}

func TestClientSignsRequestsAndPersistsRotatedCredential(t *testing.T) {
	t.Parallel()

	const (
		nodeID             = "node-1"
		bootstrap          = "bootstrap-secret"
		credential         = "node-1.initial-secret"
		timestampMS        = "1710000000123"
		leaseToken         = "lease-token"
		leaseVersion int64 = 7
	)
	activeCredential := credential
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Helper()
		requests++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		auth := r.Header.Get("Authorization")
		secret := activeCredential
		if r.URL.Path == "/v1/internal/shared-nodes/register" {
			secret = bootstrap
			if auth != "SharedNode-Bootstrap "+bootstrap {
				t.Fatalf("register authorization = %q", auth)
			}
			if string(body) != `{"nodeId":"node-1","instanceId":"instance-1","region":"sgp","totalMemoryMiB":2048,"totalSharedCpu":2,"totalWorkspaceGiB":10}` {
				t.Fatalf("register body = %s", body)
			}
		} else if auth != "SharedNode "+activeCredential {
			t.Fatalf("%s authorization = %q", r.URL.Path, auth)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.Header.Get("X-XMCL-Timestamp") != timestampMS {
			t.Fatalf("timestamp = %q", r.Header.Get("X-XMCL-Timestamp"))
		}
		if r.Header.Get("X-XMCL-Nonce") == "" {
			t.Fatal("missing nonce")
		}
		hash := sha256.Sum256(body)
		bodyHash := hex.EncodeToString(hash[:])
		if r.Header.Get("X-XMCL-Body-SHA256") != bodyHash {
			t.Fatalf("body hash = %q", r.Header.Get("X-XMCL-Body-SHA256"))
		}
		payload := strings.Join([]string{
			r.Method, r.URL.Path, timestampMS, r.Header.Get("X-XMCL-Nonce"), bodyHash,
		}, "\n")
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(payload))
		if signature := r.Header.Get("X-XMCL-Signature"); signature != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatalf("signature = %q", signature)
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/internal/shared-nodes/register":
			_, _ = w.Write([]byte(`{"nodeId":"node-1","credential":"node-1.initial-secret","expiresAt":"2026-01-01T00:00:00Z"}`))
		case "/v1/internal/shared-nodes/node-1/heartbeat":
			if string(body) != `{"contractVersion":2,"status":"ready","capacity":{"freeWorkspaceGiB":8,"allocatableMemoryMiB":1536,"allocatableSharedCpu":1,"activeContainerCount":1},"agentVersion":"test-agent","ingress":{"host":"public-node.example"}}` {
				t.Fatalf("heartbeat body = %s", body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/internal/shared-nodes/node-1/commands:next":
			_, _ = w.Write([]byte(`{"command":{"commandId":"command-1","kind":"workspace.restore_and_start","nodeId":"node-1","serviceId":"service-1","assignmentId":"assignment-1","connection":{"host":"public-node.example","hostPort":25572}},"leaseToken":"lease-token","leaseGeneration":7,"leaseExpiresAt":"2026-01-01T00:00:00Z"}`))
		case "/v1/internal/shared-nodes/node-1/commands/command-1/lease-renew":
			if string(body) != `{"leaseToken":"lease-token","leaseGeneration":7}` {
				t.Fatalf("lease renewal body = %s", body)
			}
			_, _ = w.Write([]byte(`{"leaseExpiresAt":"2026-01-01T00:01:00Z"}`))
		case "/v1/internal/shared-nodes/node-1/commands/command-1/ack":
			if string(body) != `{"leaseToken":"lease-token","leaseGeneration":7}` {
				t.Fatalf("acknowledgement body = %s", body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/internal/shared-nodes/node-1/assignments/assignment-1/started":
			if string(body) != `{"serviceId":"service-1","endpoint":{"host":"public-node.example","port":25572}}` {
				t.Fatalf("started body = %s", body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/internal/shared-nodes/node-1/assignments/assignment-1/stopped":
			if string(body) != `{"serviceId":"service-1","commandId":"command-1","leaseToken":"lease-token","leaseGeneration":7}` {
				t.Fatalf("stopped body = %s", body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/internal/shared-nodes/node-1/assignments/assignment-1/stopped-synced":
			if string(body) != `{"serviceId":"service-1","commandId":"command-1","leaseToken":"lease-token","leaseGeneration":7,"revision":3,"sizeBytes":42,"sha256":"abc"}` {
				t.Fatalf("stopped-synced body = %s", body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	credentialPath := filepath.Join(t.TempDir(), "state", "control-plane-credential")
	client, err := NewClient(ClientOptions{
		BaseURL:             server.URL,
		NodeID:              nodeID,
		InstanceID:          "instance-1",
		Region:              "sgp",
		BootstrapCredential: bootstrap,
		CredentialPath:      credentialPath,
		HTTPClient:          server.Client(),
		Now:                 func() time.Time { return time.UnixMilli(1710000000123) },
		Nonce:               func() (string, error) { return "nonce-1", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := client.Register(ctx, NodeCapacity{TotalMemoryMiB: 2048, TotalSharedCPU: 2, TotalWorkspaceGiB: 10}); err != nil {
		t.Fatal(err)
	}
	if client.bootstrapCredential != "" {
		t.Fatal("consumed bootstrap credential remained in memory")
	}
	if err := client.Heartbeat(ctx, NodeStatus{
		ContractVersion: SharedNodeContractVersion,
		Status:          "ready",
		Capacity: AvailableCapacity{
			FreeWorkspaceGiB: 8, AllocatableMemoryMiB: 1536, AllocatableSharedCPU: 1, ActiveContainerCount: 1,
		},
		AgentVersion: "test-agent",
		Ingress:      Ingress{Host: "public-node.example"},
	}); err != nil {
		t.Fatal(err)
	}
	command, err := client.Next(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if command.Lease.Token != leaseToken || command.Lease.Generation != leaseVersion {
		t.Fatalf("command lease = %#v", command.Lease)
	}
	if command.Connection == nil || command.Connection.Host != "public-node.example" || command.Connection.HostPort != 25572 {
		t.Fatalf("command connection = %#v", command.Connection)
	}
	renewed, err := client.RenewLease(ctx, command.CommandID, command.Lease)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Token != leaseToken || renewed.Generation != leaseVersion || renewed.ExpiresAt != "2026-01-01T00:01:00Z" {
		t.Fatalf("renewed lease = %#v", renewed)
	}
	if err := client.Ack(ctx, command.CommandID, renewed, CommandResult{Status: "started"}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportStarted(ctx, command.ServiceID, command.AssignmentID, command.Connection.Endpoint()); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportStopped(ctx, StoppedReport{
		ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		CommandID: command.CommandID, Lease: renewed,
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportStoppedAndSynced(ctx, SyncResult{
		ServiceID: command.ServiceID, AssignmentID: command.AssignmentID,
		CommandID: command.CommandID, Lease: renewed,
		Revision: 3, SizeBytes: 42, ManifestSHA: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	if requests != 8 {
		t.Fatalf("requests = %d, want 8", requests)
	}
	persisted, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted) != `{"credential":"node-1.initial-secret","expiresAt":"2026-01-01T00:00:00Z"}`+"\n" {
		t.Fatalf("persisted credential = %q", persisted)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(credentialPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("credential permissions = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestNewClientLoadsPersistedCredential(t *testing.T) {
	t.Parallel()

	credentialPath := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credentialPath, []byte("node-1.secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientOptions{
		BaseURL:        "https://control-plane.example",
		NodeID:         "node-1",
		Region:         "sgp",
		CredentialPath: credentialPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.mu.RLock()
	defer client.mu.RUnlock()
	if client.credential != "node-1.secret" {
		t.Fatalf("credential = %q", client.credential)
	}
}

func TestClientRotatesPersistedCredentialBeforeExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/internal/shared-nodes/register":
			_, _ = w.Write([]byte(`{"nodeId":"node-1","credential":"node-1.first","expiresAt":"2026-01-01T00:10:00Z"}`))
		case "/v1/internal/shared-nodes/node-1/credentials:rotate":
			if r.Header.Get("Authorization") != "SharedNode node-1.first" {
				t.Fatalf("rotation authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"nodeId":"node-1","credential":"node-1.second","expiresAt":"2026-01-01T00:30:00Z"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "credential")
	client, err := NewClient(ClientOptions{
		BaseURL: server.URL, NodeID: "node-1", Region: "sgp",
		InstanceID:          "instance-1",
		BootstrapCredential: "bootstrap", CredentialPath: path, HTTPClient: server.Client(),
		Now: func() time.Time { return now }, Nonce: func() (string, error) { return "nonce", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Register(t.Context(), NodeCapacity{TotalMemoryMiB: 1, TotalSharedCPU: 1, TotalWorkspaceGiB: 1}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(9 * time.Minute)
	if !client.CredentialNeedsRotation() {
		t.Fatal("credential near expiry did not require rotation")
	}
	if err := client.RotateCredential(t.Context()); err != nil {
		t.Fatal(err)
	}
	if client.CredentialNeedsRotation() {
		t.Fatal("rotated credential still requires rotation")
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"credential":"node-1.second"`) ||
		!strings.Contains(string(data), `"expiresAt":"2026-01-01T00:30:00Z"`) {
		t.Fatalf("persisted rotated credential = %q, %v", data, err)
	}
}

func TestNewClientRejectsMissingOrInvalidRegion(t *testing.T) {
	t.Parallel()

	for _, region := range []string{"", "SGP", "sgp!"} {
		_, err := NewClient(ClientOptions{
			BaseURL:             "https://control-plane.example",
			NodeID:              "node-1",
			Region:              region,
			BootstrapCredential: "bootstrap",
			CredentialPath:      filepath.Join(t.TempDir(), "credential"),
		})
		if err == nil || !strings.Contains(err.Error(), "region") {
			t.Fatalf("region %q was unexpectedly accepted: %v", region, err)
		}
	}
}

func TestHeartbeatRejectsMissingV2Status(t *testing.T) {
	t.Parallel()

	client, err := NewClient(ClientOptions{
		BaseURL:             "https://control-plane.example",
		NodeID:              "node-1",
		Region:              "sgp",
		BootstrapCredential: "bootstrap",
		CredentialPath:      filepath.Join(t.TempDir(), "credential"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(t.Context(), NodeStatus{}); err == nil {
		t.Fatal("empty heartbeat status was accepted")
	}
}

func TestClientRequestsLeaseBoundWorkspaceGrant(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/v1/internal/shared-nodes/node-1/workspace-grants/restore" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "SharedNode node-1.credential" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"contractVersion":2`) ||
			!strings.Contains(string(body), `"commandId":"command-1"`) ||
			!strings.Contains(string(body), `"leaseToken":"lease-token"`) {
			t.Fatalf("grant request body = %s", body)
		}
		_, _ = w.Write([]byte(`{"contractVersion":2,"grants":[{"key":"shared-hosting/account-1/service-1/revisions/1/manifest.json","method":"GET","url":"https://xmclstaging.blob.core.windows.net/workspaces/shared-hosting/account-1/service-1/revisions/1/manifest.json?signature=only-a-url","expiresAt":"2026-01-01T01:00:00Z"}]}`))
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		BaseURL:             server.URL,
		NodeID:              "node-1",
		Region:              "sgp",
		BootstrapCredential: "bootstrap",
		CredentialPath:      filepath.Join(t.TempDir(), "credential"),
		HTTPClient:          server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.setCredential("node-1.credential"); err != nil {
		t.Fatal(err)
	}
	grants, err := client.RestoreWorkspaceGrants(t.Context(), Command{
		CommandID: "command-1", AssignmentID: "assignment-1",
		Lease: CommandLease{Token: "lease-token", Generation: 1},
	}, "manifest", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants.Grants) != 1 || grants.Grants[0].Method != "GET" ||
		strings.Contains(grants.Grants[0].URL, "accessKey") {
		t.Fatalf("workspace grants = %#v", grants)
	}
}
