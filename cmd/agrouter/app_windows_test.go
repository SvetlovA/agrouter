//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/runner"
)

func TestApp_PromptFileThroughBatchShim(t *testing.T) {
	// an npm-style .cmd shim cannot receive a line break in an argument, so a multi-line prompt file
	// fails to start the child; this is documented, not worked around
	e := newEnv(t)
	bin := t.TempDir()
	shim := "@echo off\r\n\"" + helperCommand(t) + "\" %*\r\nexit /b %errorlevel%\r\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "fakecli.cmd"), []byte(shim), 0o600))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e.globalConfig(strings.ReplaceAll(syntheticCLIs(t), helperCommand(t), "fakecli"))
	require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "task.md"), []byte("line one\nline two\n"), 0o600))

	r := e.run([]string{"exec", "--cli=alpha", "--model=strong", "--effort=low", "--prompt-file", "task.md"}, nil)

	assert.Equal(t, runner.ExitStartFailure, r.code)
	assert.Empty(t, r.stdout)
	_, rest, ok := strings.Cut(r.stderr, "\n")
	require.True(t, ok, r.stderr)
	assert.True(t, strings.HasPrefix(rest, "agrouter: cannot start "), rest)
	assert.Contains(t, rest, "line break")
	assert.NoFileExists(t, filepath.Join(e.out, "argv.json"), "the shim must not run")
}
