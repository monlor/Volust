package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type WriteLimiter struct {
	max int
	dir string
}

func NewWriteLimiter(max int) *WriteLimiter {
	return NewWriteLimiterWithDir(max, filepath.Join(os.TempDir(), "volust-locks", "write-slots"))
}

func NewWriteLimiterWithDir(max int, dir string) *WriteLimiter {
	if max <= 0 {
		return &WriteLimiter{}
	}
	return &WriteLimiter{max: max, dir: dir}
}

func (l *WriteLimiter) Capacity() int {
	if l == nil {
		return 0
	}
	return l.max
}

func (l *WriteLimiter) With(ctx context.Context, fn func() error) error {
	if l == nil || l.max <= 0 {
		return fn()
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	for {
		release, err := l.tryAcquire()
		if err != nil {
			return err
		}
		if release != nil {
			defer release()
			return fn()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type writeSlotRelease func()

var writeSlotLocks = struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}{locks: map[string]*sync.Mutex{}}

func (l *WriteLimiter) tryAcquire() (writeSlotRelease, error) {
	for slot := 0; slot < l.max; slot++ {
		release, err := l.tryAcquireSlot(slot)
		if err != nil {
			return nil, err
		}
		if release != nil {
			return release, nil
		}
	}
	return nil, nil
}

func (l *WriteLimiter) tryAcquireSlot(slot int) (writeSlotRelease, error) {
	path := filepath.Join(l.dir, fmt.Sprintf("slot-%d.lock", slot))
	local := writeSlotLocalLock(path)
	if !local.TryLock() {
		return nil, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		local.Unlock()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		local.Unlock()
		if err == syscall.EWOULDBLOCK {
			return nil, nil
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		local.Unlock()
	}, nil
}

func writeSlotLocalLock(path string) *sync.Mutex {
	writeSlotLocks.mu.Lock()
	defer writeSlotLocks.mu.Unlock()
	local := writeSlotLocks.locks[path]
	if local == nil {
		local = &sync.Mutex{}
		writeSlotLocks.locks[path] = local
	}
	return local
}
