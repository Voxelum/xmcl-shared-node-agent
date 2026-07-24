package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

type ServiceLocker interface {
	Lock(ctx context.Context, serviceID string) (func() error, error)
}

type FileLocker struct {
	root string
}

func NewFileLocker(root string) (*FileLocker, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	return &FileLocker{root: root}, nil
}

func (l *FileLocker) Lock(ctx context.Context, serviceID string) (func() error, error) {
	lock := flock.New(filepath.Join(l.root, serviceID+".lock"))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquire service filesystem lock: %w", err)
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("service filesystem lock was not acquired")
	}
	return lock.Unlock, nil
}

type memoryLocker struct {
	locks sync.Map
}

func (l *memoryLocker) Lock(ctx context.Context, serviceID string) (func() error, error) {
	value, _ := l.locks.LoadOrStore(serviceID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if lock.TryLock() {
			return func() error {
				lock.Unlock()
				return nil
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
