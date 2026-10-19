//go:build linux

// Package grantbroker is a deliberately small privileged boundary. It cannot
// execute jobs, read enrollment tokens or SSH keys, or contact the provider.
package grantbroker

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/voxelum/xmcl-shared-node-agent/internal/bootstraprunner"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Broker struct {
	root, secret string
	mu           sync.Mutex
	lock         *os.File
}
type registry struct {
	Version int                                              `json:"version"`
	Grants  map[string]bootstraprunner.TestTOFUAuthorization `json:"grants"`
}

func Open(root, secret string) (*Broker, error) {
	if os.Geteuid() != 0 || !filepath.IsAbs(root) || len(secret) < 32 {
		return nil, errors.New("root protected store required")
	}
	resolved, e := filepath.EvalSymlinks(root)
	if e != nil || resolved != root {
		return nil, errors.New("unsafe root")
	}
	s, e := os.Lstat(root)
	if e != nil {
		return nil, e
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || !s.IsDir() || s.Mode().Perm() != 0700 {
		return nil, errors.New("unsafe root permissions")
	}
	fd, e := syscall.Open(filepath.Join(root, "broker.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), "broker.lock")
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, e
	}
	b := &Broker{root: root, secret: secret, lock: f}
	if _, e = b.load(); e != nil {
		b.Close()
		return nil, e
	}
	return b, nil
}
func (b *Broker) Close() error { return b.lock.Close() }
func (b *Broker) load() (registry, error) {
	value := registry{Version: 1, Grants: map[string]bootstraprunner.TestTOFUAuthorization{}}
	fd, e := syscall.Open(filepath.Join(b.root, "grants.json"), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(e, syscall.ENOENT) {
		return value, nil
	}
	if e != nil {
		return value, e
	}
	f := os.NewFile(uintptr(fd), "grants.json")
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return value, e
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || !s.Mode().IsRegular() || s.Mode().Perm() != 0600 || s.Size() > 4*1024*1024 {
		return value, errors.New("unsafe registry")
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if e = d.Decode(&value); e != nil {
		return value, e
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return value, errors.New("trailing registry")
	}
	if value.Version != 1 || value.Grants == nil {
		return value, errors.New("invalid registry")
	}
	operations := map[string]bool{}
	for job, g := range value.Grants {
		if bootstraprunner.ValidateDeliveredGrant(g) != nil || job != g.JobID || operations[g.OperationID] {
			return value, errors.New("corrupt registry")
		}
		operations[g.OperationID] = true
	}
	return value, nil
}
func (b *Broker) save(v registry) error {
	data, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(data) > 4*1024*1024 {
		return errors.New("registry full")
	}
	f, e := os.CreateTemp(b.root, ".grant-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(name, filepath.Join(b.root, "grants.json")); e != nil {
		return e
	}
	return b.syncDir()
}
func (b *Broker) syncDir() error {
	f, e := os.Open(b.root)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/v1/grants" && r.Method == "POST" {
		a := r.Header.Get("Authorization")
		want := "Bearer " + b.secret
		if len(a) != len(want) || subtle.ConstantTimeCompare([]byte(a), []byte(want)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		g, e := bootstraprunner.DecodeTestTOFUAuthorization(http.MaxBytesReader(w, r.Body, 8192))
		if e != nil || bootstraprunner.ValidateDeliveredGrant(g) != nil {
			http.Error(w, "invalid grant", 400)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		v, e := b.load()
		if e != nil {
			http.Error(w, "store unavailable", 503)
			return
		}
		old, exists := v.Grants[g.JobID]
		if exists && old != g {
			http.Error(w, "immutable grant conflict", 409)
			return
		}
		for _, prior := range v.Grants {
			if prior.OperationID == g.OperationID && prior != g {
				http.Error(w, "operation conflict", 409)
				return
			}
		}
		if !exists {
			created, _ := time.Parse(time.RFC3339, g.AllocationCreatedAt)
			expires, _ := time.Parse(time.RFC3339, g.FirstPinExpiresAt)
			if time.Now().Before(created.Add(-2*time.Minute)) || !time.Now().Before(expires) {
				http.Error(w, "grant expired", 409)
				return
			}
			v.Grants[g.JobID] = g
			if e = b.save(v); e != nil {
				http.Error(w, "store unconfirmed", 503)
				return
			}
		} else if e = b.syncDir(); e != nil {
			http.Error(w, "store unconfirmed", 503)
			return
		}
		// ACK means file+rename+directory fsync, not an in-memory acceptance.
		json.NewEncoder(w).Encode(map[string]any{"schema": "mot-grant-ack-v1", "jobId": g.JobID, "operationId": g.OperationID, "grantSha256": bootstraprunner.GrantDigest(g)})
		return
	}
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/grants/") {
		// Unix-domain socket only; filesystem ACL is the read capability. Reverse proxy
		// MUST expose only exact POST /v1/grants, never this lookup path.
		b.mu.Lock()
		defer b.mu.Unlock()
		v, e := b.load()
		if e != nil {
			http.Error(w, "store unavailable", 503)
			return
		}
		g, ok := v.Grants[strings.TrimPrefix(r.URL.Path, "/v1/grants/")]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		json.NewEncoder(w).Encode(g)
		return
	}
	http.NotFound(w, r)
}
