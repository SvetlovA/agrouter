package prompt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Errors of ReadTextFile, wrapped with the path as given.
var (
	ErrFileMissing = errors.New("no such file")
	ErrNotRegular  = errors.New("not a regular file")
	ErrBinaryFile  = errors.New("binary file, expected text")
)

// ReadTextFile reads an explicitly named text file, such as a prompt file or a project doc. A relative
// path is resolved against the working directory cwd; there is no restriction to the working directory
// tree. The file must be a regular file holding text (see Detect), and is returned byte for byte.
// Unlike mentioned files, a missing, non-regular, binary or unreadable file is an error, which names
// path. Reading stops at ctx's deadline with an error wrapping ctx's error.
func ReadTextFile(ctx context.Context, cwd, path string) (string, error) {
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(cwd, full)
	}
	f, err := os.OpenFile(full, openReadFlags, 0) //nolint:gosec // the caller names the file on purpose
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: %w", path, ErrFileMissing)
	}
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	// sniff the prefix first, so a large binary file is not read whole
	buf, eof, err := readUpTo(ctx, f, nil, SniffLen)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if binary, _ := Detect(buf, eof); binary {
		return "", fmt.Errorf("%s: %w", path, ErrBinaryFile)
	}
	if !eof {
		if buf, _, err = readUpTo(ctx, f, buf, -1); err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
	}
	if binary, _ := Detect(buf, true); binary {
		return "", fmt.Errorf("%s: %w", path, ErrBinaryFile)
	}
	return string(buf), nil
}
