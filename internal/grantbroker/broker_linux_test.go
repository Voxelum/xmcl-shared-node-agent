//go:build linux

package grantbroker

import (
	"bytes"
	"context"
	"encoding/json"
	br "github.com/voxelum/xmcl-shared-node-agent/internal/bootstraprunner"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const grantSecret = "gggggggggggggggggggggggggggggggg"
const submitSecret = "ssssssssssssssssssssssssssssssss"
const approvalSecret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func fixture() br.Request {
	now := time.Now().UTC().Truncate(time.Second)
	r := br.Request{OperationID: "operation-a", AuthorizationID: "fixture-approval-not-real", JobID: "job-a", NodeID: "node-a", InstanceID: "ecs-a", Address: "8.8.8.8", SSHPort: 22, EnrollmentToken: strings.Repeat("z", 64), HostKeyPolicy: br.IsolatedTestTOFUPolicy, AllocationCreatedAt: now.Add(-time.Minute).Format(time.RFC3339), AutoPinExpiresAt: now.Add(5 * time.Minute).Format(time.RFC3339), ExpectedProvider: br.ExpectedProvider{InstanceName: "xmcl-tpe-a", PackageCode: "package-a", RegionCode: "cn-taiwan-2", ZoneCode: "cn-taiwan-2-a", ImageResourceUUID: "image-a", FirewallUUID: "firewall-a", SSHKeyUUID: "key-a"}, Config: br.BootstrapConfig{ReleaseManifestURL: "https://github.com/Voxelum/xmcl-shared-node-agent/releases/download/test-fixture/release-manifest.json", ReleaseManifestSHA256: strings.Repeat("a", 64), DockerPackageVersion: "26.1.3-0ubuntu1~24.04.1", ControlPlaneURL: "https://api-staging.xmcl.app", LogicalRegion: "tpe", ObjectStorageEndpoint: "https://xmclcampstaging.blob.core.windows.net", ObjectStorageRegion: "azure", ObjectStorageBucket: "camp-staging", WorkspaceVolumeGiB: 40, BootstrapTimeout: 300}, ExpectedCapacity: br.ExpectedCapacity{WorkloadClasses: []string{"standard"}, TotalMemoryMiB: 8192, TotalSharedCPU: 4, TotalWorkspaceGiB: 32}}
	p := r.ExpectedProvider
	r.ExpectedProviderIdentity = br.ExpectedProviderIdentity{Provider: "lightnode", InstanceID: r.InstanceID, Address: r.Address, SSHPort: 22, InstanceName: p.InstanceName, PackageCode: p.PackageCode, RegionCode: p.RegionCode, ZoneCode: p.ZoneCode, ImageResourceUUID: p.ImageResourceUUID, FirewallUUID: p.FirewallUUID, SSHKeyUUID: p.SSHKeyUUID}
	r.JobID = br.GrantJobID(r.OperationID, r.InstanceID)
	r.NodeID = br.GrantNodeID(r.OperationID)
	return r
}
func grant(r br.Request) br.TestTOFUAuthorization {
	return br.TestTOFUAuthorization{AuthorizationID: r.AuthorizationID, OperationID: r.OperationID, JobID: r.JobID, InstanceID: r.InstanceID, Address: r.Address, AllocationCreatedAt: r.AllocationCreatedAt, FirstPinExpiresAt: r.AutoPinExpiresAt, EmptyIsolatedTestVM: true, AcceptFirstConnectionMITMRisk: true}
}
func post(h http.Handler, path, secret string, v any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(v)
	req := httptest.NewRequest("POST", path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+secret)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	return out
}

type executor struct{ probes, applies int }

func (e *executor) Probe(context.Context, string, int) (string, string, error) {
	e.probes++
	return "8.8.8.8 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest", "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil
}
func (e *executor) Apply(context.Context, string, int, string, string, string) error {
	e.applies++
	return nil
}
func TestNonRootBoundary(t *testing.T) {
	if os.Getenv("GRANT_CHILD") == "" {
		t.Skip("subprocess only")
	}
	root := os.Getenv("GRANT_ROOT")
	if os.Geteuid() == 0 {
		t.Fatal("not dropped")
	}
	if _, e := os.ReadFile(filepath.Join(root, "store", "grants.json")); e == nil {
		t.Fatal("non-root read protected store")
	}
	if e := os.WriteFile(filepath.Join(root, "store", "grants.json"), []byte("{}"), 0600); e == nil {
		t.Fatal("non-root write protected store")
	}
	lookup := br.UnixGrantLookup(filepath.Join(root, "broker.sock"))
	base := fixture()
	g, e := lookup(base.JobID)
	if e != nil {
		t.Fatal(e)
	}
	r := fixture()
	r.AllocationCreatedAt = g.AllocationCreatedAt
	r.AutoPinExpiresAt = g.FirstPinExpiresAt
	ex := &executor{}
	h, e := br.NewHandler(filepath.Join(root, "runner"), submitSecret, approvalSecret, ex)
	if e != nil {
		t.Fatal(e)
	}
	h.WithGrantLookup(lookup)
	wrong := r
	wrong.OperationID = "operation-b"
	if o := post(h, "/v1/bootstrap-jobs", submitSecret, wrong); o.Code != 403 {
		t.Fatalf("foreign operation %d %s", o.Code, o.Body)
	}
	if ex.probes != 0 || ex.applies != 0 {
		t.Fatal("foreign side effect")
	}
	o := post(h, "/v1/bootstrap-jobs", submitSecret, r)
	if o.Code != 200 {
		t.Fatalf("exact submit %d %s", o.Code, o.Body)
	}
	expected := 1
	if os.Getenv("GRANT_REPLAY") == "1" {
		expected = 0
	}
	if ex.applies != expected {
		t.Fatalf("apply count %d want %d", ex.applies, expected)
	}
	h, e = br.NewHandler(filepath.Join(root, "runner"), submitSecret, approvalSecret, ex)
	if e != nil {
		t.Fatal(e)
	}
	h.WithGrantLookup(lookup)
	o = post(h, "/v1/bootstrap-jobs", submitSecret, r)
	if o.Code != 200 || ex.applies != expected {
		t.Fatal("restart replay repeated apply")
	}
	// Missing broker authority fails closed even with a persisted completed job.
	h.WithGrantLookup(func(string) (*br.TestTOFUAuthorization, error) { return nil, io.ErrUnexpectedEOF })
	o = post(h, "/v1/bootstrap-jobs", submitSecret, r)
	if o.Code != 403 {
		t.Fatal("lookup outage bypass")
	}
}
func TestProtectedDeliveryRestartIsolation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-only permission test")
	}
	root, e := os.MkdirTemp("", "xmcl-grant-integration-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(root)
	os.Chmod(root, 0755)
	store := filepath.Join(root, "store")
	os.Mkdir(store, 0700)
	os.Mkdir(filepath.Join(root, "runner"), 0700)
	os.Chown(filepath.Join(root, "runner"), 65534, 65534)
	r := fixture()
	g := grant(r)
	b, e := Open(store, grantSecret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Open(store, grantSecret); e == nil {
		t.Fatal("second broker acquired lock")
	}
	if o := post(b, "/v1/grants", submitSecret, g); o.Code != 401 {
		t.Fatal("submit secret authorized grant")
	}
	o := post(b, "/v1/grants", grantSecret, g)
	if o.Code != 200 {
		t.Fatalf("grant %d %s", o.Code, o.Body)
	}
	var ack map[string]any
	json.Unmarshal(o.Body.Bytes(), &ack)
	if ack["grantSha256"] != br.GrantDigest(g) {
		t.Fatal("ACK digest")
	}
	if o = post(b, "/v1/grants", grantSecret, g); o.Code != 200 {
		t.Fatal("idempotent grant")
	}
	foreign := g
	foreign.Address = "8.8.4.4"
	if o = post(b, "/v1/grants", grantSecret, foreign); o.Code != 409 {
		t.Fatal("mutable job")
	}
	foreign = g
	foreign.InstanceID = "ecs-b"
	foreign.JobID = br.GrantJobID(foreign.OperationID, foreign.InstanceID)
	if o = post(b, "/v1/grants", grantSecret, foreign); o.Code != 409 {
		t.Fatal("mutable operation")
	}
	foreign.OperationID = "operation-b"
	foreign.InstanceID = "ecs-b"
	foreign.JobID = br.GrantJobID(foreign.OperationID, foreign.InstanceID)
	if o = post(b, "/v1/grants", grantSecret, foreign); o.Code != 200 {
		t.Fatal("second distinct operation")
	}
	b.Close()
	for round := 0; round < 2; round++ {
		b, e = Open(store, grantSecret)
		if e != nil {
			t.Fatal(e)
		}
		l, e := net.Listen("unix", filepath.Join(root, "broker.sock"))
		if e != nil {
			t.Fatal(e)
		}
		os.Chown(filepath.Join(root, "broker.sock"), 0, 65534)
		os.Chmod(filepath.Join(root, "broker.sock"), 0660)
		srv := &http.Server{Handler: b}
		go srv.Serve(l)
		cmd := exec.Command(os.Args[0], "-test.run=^TestNonRootBoundary$", "-test.v")
		cmd.Env = []string{"GRANT_CHILD=1", "GRANT_ROOT=" + root, "PATH=/usr/bin:/bin"}
		if round == 1 {
			cmd.Env = append(cmd.Env, "GRANT_REPLAY=1")
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		out, err := cmd.CombinedOutput()
		t.Log(string(out))
		srv.Close()
		b.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(store, "grants.json"), []byte("broken"), 0600)
	if _, e = Open(store, grantSecret); e == nil {
		t.Fatal("corrupt registry accepted")
	}
	t.Log("root broker persistence + UID65534 runner + actual Unix lookup + restart replay; provider/SSH executor isolated")
}
