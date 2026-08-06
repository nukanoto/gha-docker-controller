//go:build linux

package config

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// secretFileMaxSize is the read limit (1 MiB) for secret files. Private keys
// (PEM) and PATs are a few hundred bytes, so oversized files are rejected as
// misconfiguration.
const secretFileMaxSize = 1 << 20

// readSecretFile opens the secret file without following symlinks, confirms
// with fstat that it is a regular file, and returns the content. The returned
// mode is used for the permission warning. The content never appears in
// errors, and callers must not include it in warnings either.
func readSecretFile(path string) ([]byte, os.FileMode, error) {
	// O_NOFOLLOW rejects a symlink in the final component and O_NONBLOCK
	// avoids blocking on open for FIFOs and the like. O_CLOEXEC prevents the
	// descriptor from leaking across exec.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open secret file: %w", err)
	}
	defer f.Close()
	// fstat runs on the already open fd to prevent path swapping (TOCTOU).
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat secret file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("secret file is not a regular file (mode %s)", fi.Mode())
	}
	if fi.Size() > secretFileMaxSize {
		return nil, 0, fmt.Errorf("secret file is too large (limit %d bytes)", secretFileMaxSize)
	}
	// Even when the size at fstat time is within the limit, another process
	// could append after open and grow the file past the limit during the
	// read. LimitReader reads at most limit + 1 bytes, so growth after open
	// is also detected and rejected.
	content, err := readSecretLimited(f)
	if err != nil {
		return nil, 0, err
	}
	return content, fi.Mode(), nil
}

// readSecretLimited reads at most limit + 1 bytes and returns an error when
// the limit is exceeded. Content up to exactly the limit is accepted. Input
// equivalent to growth after open is also rejected here. The content never
// appears in errors.
func readSecretLimited(r io.Reader) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(r, secretFileMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	if len(content) > secretFileMaxSize {
		return nil, fmt.Errorf("secret file is too large (limit %d bytes)", secretFileMaxSize)
	}
	return content, nil
}
