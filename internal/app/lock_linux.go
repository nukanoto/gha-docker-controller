//go:build linux

// lock_linux.go holds /run/gha-docker-controller/controller.lock via
// syscall.Flock to prevent multiple daemons on the same host.
package app

import (
	"fmt"
	"os"
	"syscall"
)

// lockDir and lockPath are the fixed host lock paths. The controller
// container distribution bind-mounts the same path from the host.
const (
	lockDir  = "/run/gha-docker-controller"
	lockPath = lockDir + "/controller.lock"
)

// lockFile is the host lock held for the whole process lifetime.
type lockFile struct {
	// f is the lock file fd. Set to nil after release.
	f *os.File
}

// acquireLock acquires the host lock non-blocking. It creates the directory
// with 0750 and the file with 0600 if missing. If another process already
// holds the lock, a fatal error is returned. The lock is held until process
// exit.
func acquireLock() (*lockFile, error) {
	if err := os.MkdirAll(lockDir, 0o750); err != nil {
		return nil, fmt.Errorf("create lock directory %s: %w", lockDir, err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	// LOCK_NB makes an existing process's lock fatal immediately instead of
	// blocking.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire host lock %s: %w (another instance may be running)", lockPath, err)
	}
	return &lockFile{f: f}, nil
}

// release releases the flock and closes the file. It is idempotent and safe
// to call multiple times. Called in the last shutdown phase.
func (l *lockFile) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
