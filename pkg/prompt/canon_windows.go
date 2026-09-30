//go:build windows

package prompt

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// fileNameNormalized is GetFinalPathNameByHandle's FILE_NAME_NORMALIZED with VOLUME_NAME_DOS (both 0).
const fileNameNormalized = 0

// openCanonical opens path, following symlinks and junctions, and returns the file with its canonical
// path as Windows reports it for the open handle, so the path checked is the file that is read.
// filepath.EvalSymlinks is not used: since Go 1.23 it no longer resolves junctions (mount points).
func openCanonical(path string) (*os.File, string, error) {
	f, err := os.Open(path) //nolint:gosec // the caller checks the canonical path before reading
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", path, err)
	}
	h := windows.Handle(f.Fd())
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized)
	if err != nil || n == 0 || int(n) > len(buf) {
		_ = f.Close()
		return nil, "", fmt.Errorf("canonical path of %s: %w", path, err)
	}
	return f, stripExtendedPrefix(windows.UTF16ToString(buf[:n])), nil
}

// stripExtendedPrefix turns `\\?\C:\x` into `C:\x` and `\\?\UNC\srv\share` into `\\srv\share`.
func stripExtendedPrefix(p string) string {
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(p, `\\?\`)
}

// samePathKey folds case: Windows paths are case-insensitive.
func samePathKey(p string) string { return strings.ToLower(p) }
