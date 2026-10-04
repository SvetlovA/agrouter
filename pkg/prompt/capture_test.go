package prompt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{0xAB}, 64)...)
	jpegBytes = append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), bytes.Repeat([]byte{0x01}, 64)...)
	// an ASCII-looking PDF: valid UTF-8 text throughout, caught only by its signature
	pdfBytes = []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")
)

// replay reads the whole of the child's stdin from a capture.
func replay(t *testing.T, res *Result) []byte {
	t.Helper()
	require.NotNil(t, res.Stdin)
	data, err := io.ReadAll(res.Stdin.Reader())
	require.NoError(t, err)
	return data
}

// onlyReader hides every method but Read, so the input looks like a pipe.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(b []byte) (int, error) { return o.r.Read(b) } //nolint:wrapcheck // a pipe stand-in passes errors through

func TestCapture_PositionalAndStdin(t *testing.T) {
	stdin := "line one\r\nline two, no trailing newline"
	tests := []struct {
		name       string
		positional string
		stdin      io.Reader
		wantPrompt string
		wantStdin  []byte // nil: no stdin at all
	}{
		{name: "positional only", positional: "fix it", wantPrompt: "fix it"},
		{name: "positional with empty stdin", positional: "fix it", stdin: strings.NewReader(""),
			wantPrompt: "fix it", wantStdin: []byte{}},
		{name: "stdin only", stdin: strings.NewReader(stdin), wantPrompt: stdin, wantStdin: []byte(stdin)},
		{name: "both", positional: "summarize", stdin: strings.NewReader(stdin),
			wantPrompt: "summarize\n\n" + stdin, wantStdin: []byte(stdin)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Capture(t.Context(), []string{tc.positional}, tc.stdin)
			require.NoError(t, err)
			require.NoError(t, res.Undecidable)
			assert.Equal(t, tc.wantPrompt, res.Prompt)
			assert.Empty(t, res.Attachments)
			if tc.wantStdin == nil {
				assert.Nil(t, res.Stdin)
				return
			}
			assert.Nil(t, res.Stdin.Rest, "read to EOF")
			// the child's stdin is byte-exact: no join, no CRLF conversion, no trailing newline
			assert.Equal(t, tc.wantStdin, replay(t, res))
		})
	}
}

func TestCapture_Sources(t *testing.T) {
	tests := []struct {
		name       string
		explicit   []string // -p, positional, prompt file
		stdin      string
		wantPrompt string
	}{
		{name: "-p only", explicit: []string{"flag", "", ""}, wantPrompt: "flag"},
		{name: "positional only", explicit: []string{"", "pos", ""}, wantPrompt: "pos"},
		{name: "prompt file only", explicit: []string{"", "", "file\r\n"}, wantPrompt: "file\r\n"},
		{name: "stdin only", explicit: []string{"", "", ""}, stdin: "in", wantPrompt: "in"},
		{name: "all four in order", explicit: []string{"flag", "pos", "file\n"}, stdin: "in",
			wantPrompt: "flag\n\npos\n\nfile\n\n\nin"},
		{name: "gaps are not joined", explicit: []string{"flag", "", "file"}, wantPrompt: "flag\n\nfile"},
		{name: "whitespace is kept", explicit: []string{" ", "", ""}, stdin: "\n", wantPrompt: " \n\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Capture(t.Context(), tc.explicit, strings.NewReader(tc.stdin))
			require.NoError(t, err)
			assert.Equal(t, tc.wantPrompt, res.Prompt)
			assert.Equal(t, []byte(tc.stdin), replay(t, res), "stdin replayed alone, never joined")
		})
	}
}

func TestCapture_NoPrompt(t *testing.T) {
	for _, explicit := range [][]string{nil, {"", "", ""}} {
		_, err := Capture(t.Context(), explicit, nil)
		require.ErrorIs(t, err, ErrNoPrompt)

		_, err = Capture(t.Context(), explicit, strings.NewReader(""))
		require.ErrorIs(t, err, ErrNoPrompt)
	}
	for _, source := range []string{"-p", "positional", "--prompt-file", "stdin"} {
		assert.Contains(t, ErrNoPrompt.Error(), source)
	}
}

func TestJoin(t *testing.T) {
	assert.Empty(t, Join())
	assert.Empty(t, Join("", ""))
	assert.Equal(t, "a", Join("", "a", ""))
	assert.Equal(t, "a\n\nb\r\n\n\nc", Join("a", "", "b\r\n", "c"))
}

func TestCapture_BinaryStdin(t *testing.T) {
	t.Run("with a positional prompt", func(t *testing.T) {
		res, err := Capture(t.Context(), []string{"describe this image"}, bytes.NewReader(pngBytes))
		require.NoError(t, err)
		assert.Equal(t, "describe this image", res.Prompt, "binary stdin stays out of Jev's prompt")
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "image/png", Bytes: int64(len(pngBytes))}}, res.Attachments)
		assert.Equal(t, pngBytes, replay(t, res))
	})

	t.Run("binary only is still a prompt", func(t *testing.T) {
		res, err := Capture(t.Context(), nil, bytes.NewReader(jpegBytes))
		require.NoError(t, err)
		assert.Empty(t, res.Prompt)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "image/jpeg", Bytes: int64(len(jpegBytes))}}, res.Attachments)
	})

	t.Run("ascii-looking pdf", func(t *testing.T) {
		res, err := Capture(t.Context(), nil, bytes.NewReader(pdfBytes))
		require.NoError(t, err)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "application/pdf", Bytes: int64(len(pdfBytes))}}, res.Attachments)
	})

	t.Run("NUL past the prefix", func(t *testing.T) {
		data := append(bytes.Repeat([]byte("text "), 4000), 0, 'x')
		res, err := Capture(t.Context(), []string{"p"}, bytes.NewReader(data))
		require.NoError(t, err)
		assert.Equal(t, "p", res.Prompt)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: TypeUnknown, Bytes: int64(len(data))}}, res.Attachments)
		assert.Equal(t, data, replay(t, res))
	})

	t.Run("invalid UTF-8 at the end of stdin", func(t *testing.T) {
		data := []byte("almost text \xe2\x82")
		res, err := Capture(t.Context(), nil, bytes.NewReader(data))
		require.NoError(t, err)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: TypeUnknown, Bytes: int64(len(data))}}, res.Attachments)
	})
}

func TestCapture_UTF8AcrossThePrefixBoundary(t *testing.T) {
	// "€" (3 bytes) straddles the 8 KiB sniff boundary; the whole stdin is valid text
	data := append(bytes.Repeat([]byte("a"), SniffLen-1), []byte("€ and more text")...)
	res, err := Capture(t.Context(), nil, onlyReader{bytes.NewReader(data)})
	require.NoError(t, err)
	assert.Empty(t, res.Attachments)
	assert.Equal(t, string(data), res.Prompt)
}

func TestCapture_LargeBinaryIsAnAttachment(t *testing.T) {
	data := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0xCD}, 200<<10)...)

	t.Run("pipe: size counted", func(t *testing.T) {
		res, err := Capture(t.Context(), []string{"look"}, onlyReader{bytes.NewReader(data)})
		require.NoError(t, err)
		require.NoError(t, res.Undecidable)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "image/png", Bytes: int64(len(data))}}, res.Attachments)
		assert.Nil(t, res.Stdin.Rest)
		assert.Equal(t, data, replay(t, res))
	})

	t.Run("regular file: size from stat, rest unread", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "big.png")
		require.NoError(t, os.WriteFile(path, data, 0o600))
		f, err := os.Open(path) //nolint:gosec // the test's own temp file
		require.NoError(t, err)
		defer f.Close()

		res, err := Capture(t.Context(), []string{"look"}, StdinOf(f))
		require.NoError(t, err)
		require.NoError(t, res.Undecidable)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "image/png", Bytes: int64(len(data))}}, res.Attachments)
		assert.Less(t, len(res.Stdin.Buffered), len(data), "a regular file is not read past the prefix")
		assert.NotNil(t, res.Stdin.Rest)
		assert.Equal(t, data, replay(t, res))
	})
}

func TestCapture_NoLimit(t *testing.T) {
	data := []byte(strings.Repeat("0123456789abcdef\n", 1<<19)) // 8.5 MiB of text

	t.Run("large text captured whole", func(t *testing.T) {
		res, err := Capture(t.Context(), []string{"summarize"}, onlyReader{bytes.NewReader(data)})
		require.NoError(t, err)
		require.NoError(t, res.Undecidable)
		assert.Equal(t, "summarize\n\n"+string(data), res.Prompt)
		assert.Nil(t, res.Stdin.Rest, "read to EOF")
		assert.Equal(t, data, replay(t, res))
	})

	t.Run("large text with a late NUL is an attachment", func(t *testing.T) {
		bin := append(append([]byte{}, data...), 0)
		res, err := Capture(t.Context(), nil, bytes.NewReader(bin))
		require.NoError(t, err)
		require.NoError(t, res.Undecidable)
		assert.Empty(t, res.Prompt)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: TypeUnknown, Bytes: int64(len(bin))}}, res.Attachments)
		assert.Equal(t, bin, replay(t, res))
	})
}

func TestCapture_Deadline(t *testing.T) {
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("first part ")) }()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	res, err := Capture(ctx, nil, pr)
	require.NoError(t, err)
	require.ErrorIs(t, res.Undecidable, context.DeadlineExceeded)
	assert.Empty(t, res.Prompt)
	require.NotNil(t, res.Stdin.Rest)

	// the caller keeps writing after the deadline; the child still gets every byte
	go func() {
		_, _ = pw.Write([]byte("second part"))
		_ = pw.Close()
	}()
	assert.Equal(t, "first part second part", string(replay(t, res)))
}

func TestCapture_ReadError(t *testing.T) {
	boom := errors.New("boom")
	_, err := Capture(t.Context(), []string{"p"}, io.MultiReader(strings.NewReader("abc"), errReader{boom}))
	require.ErrorIs(t, err, boom)
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestStdinOf(t *testing.T) {
	assert.Nil(t, StdinOf(nil))

	f, err := os.Create(filepath.Join(t.TempDir(), "in.txt"))
	require.NoError(t, err)
	defer f.Close()
	assert.NotNil(t, StdinOf(f), "a redirected file is read")

	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	defer pr.Close()
	defer pw.Close()
	assert.NotNil(t, StdinOf(pr), "a pipe is read")

	closed, err := os.Open(f.Name())
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	assert.Nil(t, StdinOf(closed), "stat failure: nothing to read")
}

func TestDetect(t *testing.T) {
	cut := append(bytes.Repeat([]byte("a"), 10), "€"[:2]...)
	tests := []struct {
		name     string
		data     []byte
		complete bool
		want     bool
		wantType string
	}{
		{name: "png", data: pngBytes, complete: true, want: true, wantType: "image/png"},
		{name: "jpeg", data: jpegBytes, complete: true, want: true, wantType: "image/jpeg"},
		{name: "ascii-looking pdf", data: pdfBytes, complete: true, want: true, wantType: "application/pdf"},
		{name: "zip", data: []byte("PK\x03\x04rest"), complete: true, want: true, wantType: "application/zip"},
		{name: "plain text", data: []byte("fix the flaky test\n"), complete: true},
		{name: "html is text", data: []byte("<html><body>hi</body></html>"), complete: true},
		{name: "ansi escapes are text", data: []byte("\x1b[31mred\x1b[0m"), complete: true},
		{name: "empty", data: nil, complete: true},
		{name: "NUL", data: []byte("abc\x00def"), complete: true, want: true, wantType: TypeUnknown},
		{name: "invalid UTF-8", data: []byte("abc\xffdef"), complete: true, want: true, wantType: TypeUnknown},
		{name: "cut at the prefix boundary is text", data: cut, complete: false},
		{name: "cut at the real end is binary", data: cut, complete: true, want: true, wantType: TypeUnknown},
		{name: "invalid before the boundary is binary", data: []byte("a\xffbc"), complete: false, want: true,
			wantType: TypeUnknown},
		{name: "stray continuation at the end", data: []byte("abc\x82"), complete: false, want: true,
			wantType: TypeUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotType := Detect(tc.data, tc.complete)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantType, gotType)
		})
	}
}
