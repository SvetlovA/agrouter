//go:build !windows

package prompt

import (
	"fmt"
	"os"
	"path/filepath"
)

// openCanonical resolves path's symlinks and opens the result, returning the file and its canonical path.
func openCanonical(path string) (*os.File, string, error) {
	canon, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve %s: %w", path, err)
	}
	f, err := os.Open(canon) //nolint:gosec // the caller checks the canonical path before reading
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", canon, err)
	}
	return f, canon, nil
}

// samePathKey returns p unchanged: paths are case-sensitive here.
func samePathKey(p string) string { return p }
