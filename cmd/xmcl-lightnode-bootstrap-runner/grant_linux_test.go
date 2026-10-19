//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedGrantRealRootFile(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("real root fixture required")
	}
	d := t.TempDir()
	os.Chmod(d, 0700)
	p := filepath.Join(d, "grant.json")
	os.WriteFile(p, []byte("{}"), 0600)
	f, e := openProtectedGrant(p)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	link := filepath.Join(d, "link")
	os.Symlink(p, link)
	if f, e := openProtectedGrant(link); e == nil {
		f.Close()
		t.Fatal("symlink accepted")
	}
	os.Chmod(p, 0644)
	if f, e := openProtectedGrant(p); e == nil {
		f.Close()
		t.Fatal("bad mode accepted")
	}
	os.Chmod(p, 0600)
	os.Chown(p, 65534, 65534)
	if f, e := openProtectedGrant(p); e == nil {
		f.Close()
		t.Fatal("nonroot owner accepted")
	}
	os.Chown(p, 0, 0)
	os.Chmod(d, 0777)
	if f, e := openProtectedGrant(p); e == nil {
		f.Close()
		t.Fatal("writable parent accepted")
	}
	os.Chmod(d, 0700)
}
