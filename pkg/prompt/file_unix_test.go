//go:build !windows

package prompt

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadTextFile_Unreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}
	cwd := tree(t, map[string]string{"cwd/secret.md": "text"})
	require.NoError(t, os.Chmod(filepath.Join(cwd, "secret.md"), 0))
	_, err := ReadTextFile(t.Context(), cwd, "secret.md")
	require.ErrorIs(t, err, os.ErrPermission)
	assert.Contains(t, err.Error(), "secret.md")
}

func TestReadTextFile_FIFONotBlocking(t *testing.T) {
	cwd := tree(t, map[string]string{})
	require.NoError(t, syscall.Mkfifo(filepath.Join(cwd, "pipe"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := ReadTextFile(ctx, cwd, "pipe")
	require.ErrorIs(t, err, ErrNotRegular)
}

func TestReadTextFile_Symlink(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd2/target.md": "linked"})
	require.NoError(t, os.Symlink(filepath.Join(filepath.Dir(cwd), "cwd2", "target.md"), filepath.Join(cwd, "link.md")))
	got, err := ReadTextFile(t.Context(), cwd, "link.md")
	require.NoError(t, err)
	assert.Equal(t, "linked", got)
}
