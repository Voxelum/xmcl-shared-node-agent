package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClientSignsRequestsAndPersistsRotatedCredential(t *testing.T) {
	t.Parallel()

	const (
		nodeID       = "node-1"
		bootstrap    = "bootstrap-secret"
		credential   = "node-1.initial-secret"
		rotated      = "node-1.rotated-secret"
		timestampMS  = "1710000000123"
		leaseToken   = "lease-token"
		leaseVersion = "7"
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
			if string(body) != `{"nodeId":"node-1","region":"taipei","totalMemoryMiB":2048,"totalSharedCpu":2,"totalWorkspaceGiB":10}` {
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
			_, _ = w.Write([]byte(`{"nodeId":"node-1","credential":"node-1.initial-secret"}`))
		case "/v1/internal/shared-nodes/node-1/heartbeat":
			if len(body) != 0 {
				t.Fatalf("heartbeat body = %s", body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/internal/shared-nodes/node-1/commands:next":
			_, _ = w.Write([]byte(`{"command":{"commandId":"command-1","kind":"workspace.restore_and_start","nodeId":"node-1","serviceId":"service-1","assignmentId":"assignment-1"},"leaseToken":"lease-token","leaseGeneration":"7","leaseExpiresAt":"2026-01-01T00:00:00Z"}`))
		case "/v1/internal/shared-nodes/node-1/commands/command-1/lease-renew":
			if string(body) != `{"leaseToken":"lease-token","leaseGeneration":"7","leaseExpiresAt":"2026-01-01T00:00:00Z"}` {
				t.Fatalf("lease renewal body = %s", body)
			}
			activeCredential = rotated
			_, _ = w.Write([]byte(`{"leaseToken":"new-token","leaseGeneration":"8","leaseExpiresAt":"2026-01-01T00:01:00Z","credential":"node-1.rotated-secret"}`))
		case "/v1/internal/shared-nodes/node-1/commands/command-1/ack":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/internal/shared-nodes/node-1/assignments/assignment-1/started":
			if string(body) != `{"serviceId":"service-1"}` {
				t.Fatalf("started body = %s", body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/internal/shared-nodes/node-1/assignments/assignment-1/stopped-synced":
			if string(body) != `{"serviceId":"service-1","revision":3,"sizeBytes":42,"sha256":"abc"}` {
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
	if err := client.Heartbeat(ctx, NodeStatus{}); err != nil {
		t.Fatal(err)
	}
	command, err := client.Next(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if command.Lease.Token != leaseToken || command.Lease.Generation != leaseVersion {
		t.Fatalf("command lease = %#v", command.Lease)
	}
	if _, err := client.RenewLease(ctx, command.CommandID, command.Lease); err != nil {
		t.Fatal(err)
	}
	if err := client.Ack(ctx, command.CommandID, CommandResult{Status: "started"}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportStarted(ctx, command.ServiceID, command.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportStoppedAndSynced(ctx, SyncResult{
		ServiceID: command.ServiceID, AssignmentID: command.AssignmentID, Revision: 3, SizeBytes: 42, ManifestSHA: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	if requests != 7 {
		t.Fatalf("requests = %d, want 7", requests)
	}
	persisted, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted) != rotated+"\n" {
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
		BaseURL:             "https://control-plane.example",
		NodeID:              "node-1",
		BootstrapCredential: "bootstrap",
		CredentialPath:      credentialPath,
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
