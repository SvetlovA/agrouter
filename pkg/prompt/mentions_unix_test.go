//go:build !windows

package prompt

import (
	"context"
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
