package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/monlor/volust/internal/config"
	volustdocker "github.com/monlor/volust/internal/docker"
)

var sourceLocks = newLockManager(filepath.Join(os.TempDir(), "volust-locks"))

type lockManager struct {
	dir    string
	mu     sync.Mutex
	byName map[string]*sync.Mutex
}

func newLockManager(dir string) *lockManager {
	return &lockManager{dir: dir, byName: map[string]*sync.Mutex{}}
}

func SourceLockKey(profile string, spec volustdocker.BackupSpec, source volustdocker.Source) string {
	if source.Type == "volume" && source.VolumeName != "" {
		return "volume\x00" + source.VolumeName
	}
	if source.Type == "bind" && source.HostSource != "" {
		return "bind\x00" + source.HostSource
	}
	return profile + "\x00" + spec.Name + "\x00" + source.ID
}

func RepositoryLockKey(profile config.Profile, appName string) string {
	if profile.Type == config.ProfileWebDAV {
		path := strings.Trim(strings.TrimPrefix(profile.Path, "/"), "/")
		appDir := config.AppRepositoryDir(appName)
		if path == "" {
			path = appDir
		} else {
			path += "/" + appDir
		}
		return "repo\x00webdav\x00" + strings.TrimRight(profile.WebDAV.URL, "/") + "\x00" + path
	}
	return "repo\x00" + profile.RepositoryStringForApp(appName)
}

func BackendWriteKey(profile config.Profile) string {
	return "backend\x00" + profile.BackendKey()
}

func ContainerLockKey(containerID string) string {
	return "container\x00" + containerID
}

func WithSourceLock(ctx context.Context, key string, fn func() error) error {
	return sourceLocks.with(ctx, key, fn)
}

func (m *lockManager) with(ctx context.Context, key string, fn func() error) error {
	waitCtx, cancel := lockWaitContext(ctx)
	defer cancel()

	local, err := m.acquireLocalLock(waitCtx, key)
	if err != nil {
		return err
	}
	defer local.Unlock()

	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(m.dir, lockFileName(key)), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := flock(waitCtx, file); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func (m *lockManager) acquireLocalLock(ctx context.Context, key string) (*sync.Mutex, error) {
	local := m.localLock(key)
	for {
		if local.TryLock() {
			return local, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *lockManager) localLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	local := m.byName[key]
	if local == nil {
		local = &sync.Mutex{}
		m.byName[key] = local
	}
	return local
}

func lockFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".lock"
}

func flock(ctx context.Context, file *os.File) error {
	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return nil
		} else if err != syscall.EWOULDBLOCK {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func lockWaitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := 6 * time.Hour
	if value := strings.TrimSpace(os.Getenv("VOLUST_LOCK_TIMEOUT")); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	return context.WithTimeout(ctx, timeout)
}
