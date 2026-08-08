//go:build linux

package config

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// secretFileMaxSize bounds secret-file reads.
const secretFileMaxSize = 1 << 20

// readSecretFile opens a regular, non-symlink secret file and returns its mode.
func readSecretFile(path string) ([]byte, os.FileMode, error) {
	// These flags prevent symlink traversal, FIFO blocking, and fd inheritance.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open secret file: %w", err)
	}
	defer f.Close()
	// Stat the open descriptor so a path swap cannot change the checked file.
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
	content, err := readSecretLimited(f)
	if err != nil {
		return nil, 0, err
	}
	return content, fi.Mode(), nil
}

// readSecretLimited detects files that exceed the read limit, including files
// that grow after they are opened.
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
