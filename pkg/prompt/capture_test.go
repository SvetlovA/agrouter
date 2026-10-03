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

const testLimit = 1 << 20

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
			res, err := Capture(t.Context(), tc.positional, tc.stdin, testLimit)
			require.NoError(t, err)
			require.NoError(t, res.Undecidable)
			assert.Equal(t, tc.wantPrompt, res.Prompt)
			assert.Empty(t, res.Attachments)
			assert.Equal(t, int64(len(tc.positional)+len(tc.wantStdin)), res.TextBytes, "the join is not captured text")
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

func TestCapture_NoPrompt(t *testing.T) {
	_, err := Capture(t.Context(), "", nil, testLimit)
	require.ErrorIs(t, err, ErrNoPrompt)

	_, err = Capture(t.Context(), "", strings.NewReader(""), testLimit)
	require.ErrorIs(t, err, ErrNoPrompt)
}

func TestCapture_BinaryStdin(t *testing.T) {
	t.Run("with a positional prompt", func(t *testing.T) {
		res, err := Capture(t.Context(), "describe this image", bytes.NewReader(pngBytes), testLimit)
		require.NoError(t, err)
		assert.Equal(t, "describe this image", res.Prompt, "binary stdin stays out of Jev's prompt")
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "image/png", Bytes: int64(len(pngBytes))}}, res.Attachments)
		assert.Equal(t, int64(len("describe this image")), res.TextBytes, "binary never counts as text")
		assert.Equal(t, pngBytes, replay(t, res))
	})

	t.Run("binary only is still a prompt", func(t *testing.T) {
		res, err := Capture(t.Context(), "", bytes.NewReader(jpegBytes), testLimit)
		require.NoError(t, err)
		assert.Empty(t, res.Prompt)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "image/jpeg", Bytes: int64(len(jpegBytes))}}, res.Attachments)
	})

	t.Run("ascii-looking pdf", func(t *testing.T) {
		res, err := Capture(t.Context(), "", bytes.NewReader(pdfBytes), testLimit)
		require.NoError(t, err)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "application/pdf", Bytes: int64(len(pdfBytes))}}, res.Attachments)
	})

	t.Run("NUL past the prefix", func(t *testing.T) {
		data := append(bytes.Repeat([]byte("text "), 4000), 0, 'x')
		res, err := Capture(t.Context(), "p", bytes.NewReader(data), testLimit)
		require.NoError(t, err)
		assert.Equal(t, "p", res.Prompt)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: TypeUnknown, Bytes: int64(len(data))}}, res.Attachments)
		assert.Equal(t, data, replay(t, res))
	})

	t.Run("invalid UTF-8 at the end of stdin", func(t *testing.T) {
		data := []byte("almost text \xe2\x82")
		res, err := Capture(t.Context(), "", bytes.NewReader(data), testLimit)
		require.NoError(t, err)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: TypeUnknown, Bytes: int64(len(data))}}, res.Attachments)
	})
}

func TestCapture_UTF8AcrossThePrefixBoundary(t *testing.T) {
	// "€" (3 bytes) straddles the 8 KiB sniff boundary; the whole stdin is valid text
	data := append(bytes.Repeat([]byte("a"), SniffLen-1), []byte("€ and more text")...)
	res, err := Capture(t.Context(), "", onlyReader{bytes.NewReader(data)}, testLimit)
	require.NoError(t, err)
	assert.Empty(t, res.Attachments)
	assert.Equal(t, string(data), res.Prompt)
}

func TestCapture_LargeBinaryIsAnAttachment(t *testing.T) {
	const limit = 1024
	data := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0xCD}, 200<<10)...)

	t.Run("pipe: size counted", func(t *testing.T) {
		res, err := Capture(t.Context(), "look", onlyReader{bytes.NewReader(data)}, limit)
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

		res, err := Capture(t.Context(), "look", StdinOf(f), limit)
		require.NoError(t, err)
		require.NoError(t, res.Undecidable)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: "image/png", Bytes: int64(len(data))}}, res.Attachments)
		assert.Less(t, len(res.Stdin.Buffered), len(data), "a regular file is not read past the prefix")
		assert.NotNil(t, res.Stdin.Rest)
		assert.Equal(t, data, replay(t, res))
	})
}

func TestCapture_Limit(t *testing.T) {
	data := []byte(strings.Repeat("0123456789abcdef\n", 4096)) // ~68 KiB of text

	t.Run("over the limit stops capture, stdin replayed in full", func(t *testing.T) {
		res, err := Capture(t.Context(), "", onlyReader{bytes.NewReader(data)}, 10<<10)
		require.NoError(t, err)
		require.ErrorIs(t, res.Undecidable, ErrCaptureLimit)
		assert.Empty(t, res.Prompt, "partial text is never sent")
		assert.Less(t, len(res.Stdin.Buffered), len(data))
		assert.NotNil(t, res.Stdin.Rest)
		assert.Equal(t, data, replay(t, res))
	})

	t.Run("exactly at the limit fits", func(t *testing.T) {
		res, err := Capture(t.Context(), "", bytes.NewReader(data), int64(len(data)))
		require.NoError(t, err)
		require.NoError(t, res.Undecidable)
		assert.Equal(t, string(data), res.Prompt)
	})

	t.Run("the positional prompt counts", func(t *testing.T) {
		res, err := Capture(t.Context(), "xy", bytes.NewReader(data), int64(len(data))+1)
		require.NoError(t, err)
		require.ErrorIs(t, res.Undecidable, ErrCaptureLimit)
		assert.Equal(t, data, replay(t, res))
	})

	t.Run("positional alone over the limit", func(t *testing.T) {
		res, err := Capture(t.Context(), "too long", nil, 3)
		require.NoError(t, err)
		require.ErrorIs(t, res.Undecidable, ErrCaptureLimit)
		assert.Empty(t, res.Prompt)
	})

	t.Run("positional over the limit beside binary stdin", func(t *testing.T) {
		res, err := Capture(t.Context(), "too long", bytes.NewReader(pngBytes), 3)
		require.NoError(t, err)
		require.ErrorIs(t, res.Undecidable, ErrCaptureLimit)
		assert.Empty(t, res.Attachments)
		assert.Equal(t, pngBytes, replay(t, res))
	})

	t.Run("text over the limit with a NUL is an attachment", func(t *testing.T) {
		bin := append(append([]byte{}, data...), 0)
		copy(bin[100:], "\x00")
		res, err := Capture(t.Context(), "", bytes.NewReader(bin), 1<<10)
		require.NoError(t, err)
		require.NoError(t, res.Undecidable)
		assert.Equal(t, []Attachment{{Source: SourceStdin, Type: TypeUnknown, Bytes: int64(len(bin))}}, res.Attachments)
	})
}

func TestCapture_Deadline(t *testing.T) {
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("first part ")) }()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	res, err := Capture(ctx, "", pr, testLimit)
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
	_, err := Capture(t.Context(), "p", io.MultiReader(strings.NewReader("abc"), errReader{boom}), testLimit)
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
