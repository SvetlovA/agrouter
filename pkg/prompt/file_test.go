package prompt

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadTextFile(t *testing.T) {
	large := strings.Repeat("line of text\r\n", SniffLen) // well past the sniffed prefix
	cwd := tree(t, map[string]string{
		"cwd/plain.md":       "# Title\r\nbody\n\n  trailing  \n",
		"cwd/sub/nested.txt": "nested",
		"cwd/empty.txt":      "",
		"cwd/large.txt":      large,
		"cwd/image.png":      string(pngBytes),
		"cwd/nul.txt":        "text\x00more",
		"cwd/late-nul.txt":   strings.Repeat("a", SniffLen+10) + "\x00",
		"cwd/latin1.txt":     "caf\xe9",
		"outside.txt":        "outside the tree",
	})
	require.NoError(t, os.Mkdir(filepath.Join(cwd, "dir"), 0o750))

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr error
	}{
		{name: "bytes unchanged, CRLF kept", path: "plain.md", want: "# Title\r\nbody\n\n  trailing  \n"},
		{name: "relative subdirectory", path: filepath.Join("sub", "nested.txt"), want: "nested"},
		{name: "absolute path", path: filepath.Join(cwd, "plain.md"), want: "# Title\r\nbody\n\n  trailing  \n"},
		{name: "outside the working directory", path: filepath.Join("..", "outside.txt"), want: "outside the tree"},
		{name: "empty file", path: "empty.txt", want: ""},
		{name: "larger than the sniffed prefix", path: "large.txt", want: large},
		{name: "missing", path: "nope.md", wantErr: ErrFileMissing},
		{name: "missing directory", path: filepath.Join("nope", "a.md"), wantErr: ErrFileMissing},
		{name: "directory", path: "dir", wantErr: ErrNotRegular},
		{name: "empty path is the working directory", path: "", wantErr: ErrNotRegular},
		{name: "binary signature", path: "image.png", wantErr: ErrBinaryFile},
		{name: "NUL byte", path: "nul.txt", wantErr: ErrBinaryFile},
		{name: "NUL byte past the sniffed prefix", path: "late-nul.txt", wantErr: ErrBinaryFile},
		{name: "invalid UTF-8", path: "latin1.txt", wantErr: ErrBinaryFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadTextFile(t.Context(), cwd, tt.path)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Contains(t, err.Error(), tt.path)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadTextFile_Deadline(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd/a.md": "text"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ReadTextFile(ctx, cwd, "a.md")
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "a.md")
}

func TestReadUpTo(t *testing.T) {
	data := bytes.Repeat([]byte("x"), readSize*2+5)
	tests := []struct {
		name    string
		n       int64
		wantLen int
		wantEOF bool
	}{
		{name: "prefix", n: 10, wantLen: 10},
		{name: "to EOF", n: -1, wantLen: len(data), wantEOF: true},
		{name: "limit past the end", n: int64(len(data)) + 1, wantLen: len(data), wantEOF: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf, eof, err := readUpTo(t.Context(), bytes.NewReader(data), nil, tt.n)
			require.NoError(t, err)
			assert.Len(t, buf, tt.wantLen)
			assert.Equal(t, tt.wantEOF, eof)
		})
	}
}
