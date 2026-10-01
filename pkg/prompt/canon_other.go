//go:build !windows

package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// openCanonical resolves path's symlinks and opens the result, returning the file and its canonical path.
// It opens without blocking, so a FIFO without a writer does not hang; the caller rejects non-regular files.
func openCanonical(path string) (*os.File, string, error) {
	canon, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve %s: %w", path, err)
	}
	f, err := os.OpenFile(canon, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", canon, err)
	}
	return f, canon, nil
}

// samePathKey returns p unchanged: paths are case-sensitive here.
func samePathKey(p string) string { return p }
