//go:build !windows

package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// openReadFlags opens a file for reading without blocking, so a FIFO without a writer does not hang.
const openReadFlags = os.O_RDONLY | syscall.O_NONBLOCK

// workdir is the working directory mentioned files are read from: its canonical path, for checks, and
// an os.Root opened on it, through which every file is opened.
type workdir struct {
	root string
	dir  *os.Root
}

// openWorkdir opens the absolute working directory abs as an os.Root and resolves its canonical path,
// failing unless that path names the directory that was opened. Files are then opened through the
// os.Root, so a working directory, or a directory beneath it, swapped for a symlink later cannot lead
// a read outside the tree the checks were made against.
func openWorkdir(abs string) (*workdir, error) {
	dir, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("working directory: %w", err)
	}
	opened, err := dir.Stat(".")
	if err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("working directory: %w", err)
	}
	named, err := os.Stat(root)
	if err != nil || !os.SameFile(opened, named) {
		_ = dir.Close()
		return nil, fmt.Errorf("working directory %s: changed while opening", abs)
	}
	return &workdir{root: root, dir: dir}, nil
}

func (w *workdir) Close() error { return w.dir.Close() } //nolint:wrapcheck // closing a directory handle

// open opens the file path names and returns it with its canonical path, which the caller checks
// against the working directory. It opens through the os.Root, which refuses a path that leaves the
// opened directory as it stands at open time, and without blocking, so a FIFO without a writer does
// not hang; the caller rejects non-regular files.
func (w *workdir) open(path string) (*os.File, string, error) {
	canon, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve %s: %w", path, err)
	}
	rel, err := filepath.Rel(w.root, canon)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", canon, err)
	}
	f, err := w.dir.OpenFile(rel, openReadFlags, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", canon, err)
	}
	return f, canon, nil
}

// samePathKey returns p unchanged: paths are case-sensitive here.
func samePathKey(p string) string { return p }
