//go:build !windows

package prompt

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadMentions_FIFONotBlocking(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd/a.txt": "text"})
	require.NoError(t, syscall.Mkfifo(filepath.Join(cwd, "pipe"), 0o600))
	outside := filepath.Join(filepath.Dir(cwd), "cwd2", "pipe")
	require.NoError(t, syscall.Mkfifo(outside, 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res := mention(t, ctx, cwd, "read pipe "+outside+" a.txt", 1<<20)
	require.NoError(t, res.Undecidable)
	assert.Equal(t, []string{"text"}, res.Files)
	assert.Empty(t, res.Attachments)
}

func TestWorkdirOpen(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd/sub/a.txt": "in", "cwd2/b.txt": "out"})
	root, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	outside := filepath.Join(filepath.Dir(root), "cwd2", "b.txt")
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))
	require.NoError(t, os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "dirlink")))
	wd, err := openWorkdir(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = wd.Close() })

	f, canon, err := wd.open(filepath.Join(root, "dirlink", "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "sub", "a.txt"), canon)
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, "in", string(data))
	require.NoError(t, f.Close())

	for _, p := range []string{filepath.Join(root, "link"), outside, filepath.Join(root, "missing")} {
		_, _, err = wd.open(p)
		assert.Error(t, err, p)
	}
}

func TestWorkdirOpen_ReadsOpenedDirectory(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd/a.txt": "opened", "cwd2/a.txt": "swapped in"})
	root, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	wd, err := openWorkdir(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = wd.Close() })

	// the path now names another directory: files still come from the one opened
	require.NoError(t, os.Rename(root, root+".old"))
	require.NoError(t, os.Rename(filepath.Join(filepath.Dir(root), "cwd2"), root))
	f, _, err := wd.open(filepath.Join(root, "a.txt"))
	require.NoError(t, err)
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, "opened", string(data))
	require.NoError(t, f.Close())
}

func TestOpenWorkdir_Missing(t *testing.T) {
	_, err := openWorkdir(filepath.Join(t.TempDir(), "missing"))
	assert.Error(t, err)
}
