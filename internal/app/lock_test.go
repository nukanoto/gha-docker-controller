//go:build linux

// lock_test.go verifies the host lock. The lock path is fixed at
// /run/gha-docker-controller/controller.lock and cannot be made
// configurable, so verification with the real directory only runs as root
// (which can write to /run); everything else verifies the pure helpers
// (release idempotency) and the real syscall error paths. No fake servers or
// stubs are used.
package app

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestLock_ReleaseIsIdempotent verifies that release safely no-ops for a nil
// receiver, a zero value, and double calls. It uses a real file and confirms
// the fd is closed after release via os.ErrClosed.
func TestLock_ReleaseIsIdempotent(t *testing.T) {
	// A nil receiver and a zero value no-op without panicking.
	var nilLock *lockFile
	nilLock.release()
	(&lockFile{}).release()

	// Double-releasing a real file is safe and the fd is reliably closed.
	f, err := os.CreateTemp(t.TempDir(), "lock-*")
	if err != nil {
		t.Fatalf("一時 file の作成に失敗しました: %v", err)
	}
	l := &lockFile{f: f}
	l.release()
	l.release()
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("release 後も file が閉じられていません: %v", err)
	}
}

// TestLock_AcquireFailsWithoutRunWritePermission verifies with real syscalls
// that non-root acquireLock fails to create the fixed path directory and
// returns a wrapped error. Skipped when /run is writable.
func TestLock_AcquireFailsWithoutRunWritePermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では /run へ書き込めるため、この環境依存 test は対象外")
	}
	l, err := acquireLock()
	if err == nil {
		// If the environment allows writing to /run/gha-docker-controller,
		// always release the acquired lock before skipping.
		l.release()
		t.Skip("環境が lock 取得を許可しているため、この環境依存 test は対象外")
	}
	if !strings.Contains(err.Error(), "create lock directory") {
		t.Fatalf("期待した error 文面と異なります: %v", err)
	}
}

// TestLock_AcquireConflictAndPermissionsOnFixedPath verifies real flock
// conflict and file/directory permissions on the fixed path. A second
// process on the same host fails immediately via LOCK_NB, re-acquisition
// works after release, and the file is created with 0600 while the directory
// stays within 0750. Requires write access to /run, so it only runs as root.
func TestLock_AcquireConflictAndPermissionsOnFixedPath(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("固定 path は /run 配下のため、root 以外では検証できない")
	}
	first, err := acquireLock()
	if err != nil {
		// Skip without interfering if a real daemon holds the lock.
		t.Skipf("既に host lock が保持されています: %v", err)
	}
	defer first.release()

	// 1. The file is created within 0600 and the directory within 0750. The
	// umask only removes group/other bits, so the file must match exactly and
	// the directory must not have bits outside 0750.
	fi, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("lock file の stat に失敗しました: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file の permission が 0600 ではありません: %o", fi.Mode().Perm())
	}
	di, err := os.Stat(lockDir)
	if err != nil {
		t.Fatalf("lock directory の stat に失敗しました: %v", err)
	}
	if !di.IsDir() {
		t.Fatalf("lock path の親が directory ではありません: %v", di.Mode())
	}
	if perm := di.Mode().Perm(); perm&^0o750 != 0 {
		t.Fatalf("lock directory の permission が 0750 の範囲外です: %o", perm)
	}

	// 2. A second process on the same host fails immediately via LOCK_NB
	// instead of blocking.
	second, err := acquireLock()
	if err == nil {
		second.release()
		t.Fatal("2 個目の lock 取得が成功しました: 競合検出が機能していません")
	}
	if !strings.Contains(err.Error(), "another instance") {
		t.Fatalf("競合 error の文面が期待と異なります: %v", err)
	}

	// 3. Re-acquisition from the same process works after release.
	first.release()
	again, err := acquireLock()
	if err != nil {
		t.Fatalf("release 後の再取得に失敗しました: %v", err)
	}
	again.release()

	// 4. Cleanup: remove the file and directory this test created. If
	// another process recreated them, do not remove (ignore remove
	// failures).
	_ = os.Remove(lockPath)
	_ = os.Remove(lockDir)
}
