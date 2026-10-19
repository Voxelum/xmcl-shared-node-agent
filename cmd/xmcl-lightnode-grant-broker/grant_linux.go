//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func openProtectedGrant(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || os.Geteuid() != 0 {
		return nil, errors.New("absolute root-owned authorization required")
	}
	// O_NOFOLLOW applies to the opened file; permissions/owner checked on its fd,
	// not the pre-open pathname. Also reject a writable immediate parent.
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0022 != 0 {
		return nil, errors.New("unsafe authorization parent")
	}
	ps, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || ps.Uid != 0 {
		return nil, errors.New("unsafe authorization parent owner")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 8192 {
		f.Close()
		return nil, errors.New("invalid authorization ownership, mode, type or size")
	}
	return f, nil
}
