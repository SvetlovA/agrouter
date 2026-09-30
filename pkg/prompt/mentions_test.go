package prompt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tree creates <tmp>/cwd and its sibling <tmp>/cwd2 with the given files (slash paths, relative to tmp)
// and returns the cwd path.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	tmp := t.TempDir()
	for _, d := range []string{"cwd", "cwd2"} {
		require.NoError(t, os.MkdirAll(filepath.Join(tmp, d), 0o750))
	}
	for name, content := range files {
		p := filepath.Join(tmp, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	return filepath.Join(tmp, "cwd")
}

// mention captures positional as the prompt and reads the files it mentions inside cwd.
func mention(t *testing.T, ctx context.Context, cwd, positional string, limit int64) *Result {
	t.Helper()
	res, err := Capture(t.Context(), positional, nil, limit)
	require.NoError(t, err)
	require.NoError(t, res.ReadMentions(ctx, cwd, limit))
	return res
}

func TestCandidates(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "whitespace tokens", text: "fix  src/a.go\tnow\n", want: []string{"fix", "src/a.go", "now"}},
		{name: "double quotes with spaces", text: `read "my file.md" please`, want: []string{"read", "my file.md", "please"}},
		{name: "backticks", text: "see `dir/a b.go`.", want: []string{"see", "dir/a b.go", "."}},
		{name: "markdown link", text: "see [the plan](docs/x y.md) now",
			want: []string{"see", "[the", "plan", "docs/x y.md", "now"}},
		{name: "markdown link in angle brackets", text: "[p](<a b.md>)", want: []string{"[p", "a b.md"}},
		{name: "single quotes at word boundaries", text: "open 'a b.txt', ok", want: []string{"open", "a b.txt", ",", "ok"}},
		{name: "apostrophes stay words", text: "don't touch it's file", want: []string{"don't", "touch", "it's", "file"}},
		{name: "mixed order kept", text: "b.txt then \"a.txt\" then c.txt",
			want: []string{"b.txt", "then", "a.txt", "then", "c.txt"}},
		{name: "unclosed quote is a token", text: `say "hi`, want: []string{"say", `"hi`}},
		{name: "quote closes on the same line only", text: "`a\nb`", want: []string{"`a", "b`"}},
		{name: "empty span skipped", text: `"" x`, want: []string{`""`, "x"}},
		{name: "urls dropped", text: "get https://example.com/a.go and `http://x/y` ok", want: []string{"get", "and", "ok"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, candidates(tc.text))
		})
	}
}

func TestVariants(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{in: "a.go", want: []string{"a.go"}},
		{in: "a.go.", want: []string{"a.go.", "a.go"}},
		{in: "(a.go),", want: []string{"(a.go),", "a.go"}},
		{in: "src/a.go:42", want: []string{"src/a.go:42", "src/a.go"}},
		{in: "src/a.go:42:7", want: []string{"src/a.go:42:7", "src/a.go"}},
		{in: "src/a.go:42:7:", want: []string{"src/a.go:42:7:", "src/a.go:42:7", "src/a.go"}},
		{in: `C:\x\a.go`, want: []string{`C:\x\a.go`}},
		{in: `C:\x\a.go:12`, want: []string{`C:\x\a.go:12`, `C:\x\a.go`}},
		{in: "C:12", want: []string{"C:12"}},
		{in: "a.go:x", want: []string{"a.go:x"}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, variants(tc.in))
		})
	}
}

func TestReadMentions_Found(t *testing.T) {
	cwd := tree(t, map[string]string{
		"cwd/my file.md":   "spaces",
		"cwd/dir/a b.go":   "backtick",
		"cwd/docs/x y.md":  "link",
		"cwd/q s.txt":      "single",
		"cwd/src/a.go":     "package a",
		"cwd/src/b.go":     "package b",
		"cwd/end.txt":      "punct",
		"cwd/paren.txt":    "paren",
		"cwd/dir/nested.x": "nested",
	})
	abs := filepath.Join(cwd, "dir", "nested.x")
	tests := []struct {
		name   string
		prompt string
		want   []string
	}{
		{name: "double quotes", prompt: `read "my file.md"`, want: []string{"spaces"}},
		{name: "backticks", prompt: "see `dir/a b.go`", want: []string{"backtick"}},
		{name: "markdown link", prompt: "see [the plan](docs/x y.md)", want: []string{"link"}},
		{name: "single quotes", prompt: "open 'q s.txt'", want: []string{"single"}},
		{name: "line suffix", prompt: "fix src/a.go:42", want: []string{"package a"}},
		{name: "line and column suffix", prompt: "fix src/b.go:42:7", want: []string{"package b"}},
		{name: "trailing punctuation", prompt: "see end.txt.", want: []string{"punct"}},
		{name: "brackets", prompt: "(see paren.txt)", want: []string{"paren"}},
		{name: "absolute path inside with a suffix", prompt: "fix " + abs + ":3", want: []string{"nested"}},
		{name: "backslash path", prompt: `fix dir\nested.x`, want: winOnly([]string{"nested"})},
		{name: "dedupe by canonical path", prompt: "src/a.go ./src/a.go `src/a.go` src/../src/a.go:1",
			want: []string{"package a"}},
		{name: "textual order across quoted and plain", prompt: "src/b.go then \"my file.md\" then src/a.go",
			want: []string{"package b", "spaces", "package a"}},
		{name: "no candidates", prompt: "just words", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := mention(t, t.Context(), cwd, tc.prompt, testLimit)
			require.NoError(t, res.Undecidable)
			assert.Equal(t, tc.want, res.Files)
			assert.Empty(t, res.Attachments)
			assert.Equal(t, tc.prompt, res.Prompt, "the prompt is sent as written")
		})
	}
}

// winOnly returns want on Windows, where a backslash separates paths, and nil elsewhere.
func winOnly(want []string) []string {
	if runtime.GOOS == "windows" {
		return want
	}
	return nil
}

func TestReadMentions_CaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive paths are a Windows property")
	}
	cwd := tree(t, map[string]string{"cwd/a.go": "a"})
	res := mention(t, t.Context(), cwd, "A.GO and a.go", testLimit)
	assert.Equal(t, []string{"a"}, res.Files)
}

func TestReadMentions_NoRecursionAndURLs(t *testing.T) {
	cwd := tree(t, map[string]string{
		"cwd/a.md":             "see b.md",
		"cwd/b.md":             "not read",
		"cwd/example.com/x.go": "not read either",
	})
	res := mention(t, t.Context(), cwd, "run a.md and https://example.com/x.go", testLimit)
	require.NoError(t, res.Undecidable)
	assert.Equal(t, []string{"see b.md"}, res.Files)
}

func TestReadMentions_Binary(t *testing.T) {
	cwd := tree(t, map[string]string{
		"cwd/shot.png": string(pngBytes),
		"cwd/doc.pdf":  string(pdfBytes),
		"cwd/nul.bin":  "text\x00more",
		"cwd/big.bin":  strings.Repeat("a", SniffLen+10) + "\x00",
		"cwd/note.txt": "text",
	})
	res := mention(t, t.Context(), cwd, "look at shot.png, note.txt, doc.pdf nul.bin big.bin", testLimit)
	require.NoError(t, res.Undecidable)
	assert.Equal(t, []string{"text"}, res.Files)
	assert.Equal(t, []Attachment{
		{Source: SourceMentioned, Type: "image/png", Bytes: int64(len(pngBytes))},
		{Source: SourceMentioned, Type: "application/pdf", Bytes: int64(len(pdfBytes))},
		{Source: SourceMentioned, Type: TypeUnknown, Bytes: 9},
		{Source: SourceMentioned, Type: TypeUnknown, Bytes: SniffLen + 11},
	}, res.Attachments)
	assert.Equal(t, int64(len(res.Prompt)+len("text")), res.TextBytes, "binary never counts as text")
}

func TestReadMentions_BinaryWithStdinAttachment(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd/shot.png": string(pngBytes)})
	res, err := Capture(t.Context(), "compare with shot.png", strings.NewReader(string(jpegBytes)), testLimit)
	require.NoError(t, err)
	require.NoError(t, res.ReadMentions(t.Context(), cwd, testLimit))
	assert.Equal(t, []Attachment{
		{Source: SourceStdin, Type: "image/jpeg", Bytes: int64(len(jpegBytes))},
		{Source: SourceMentioned, Type: "image/png", Bytes: int64(len(pngBytes))},
	}, res.Attachments)
}

func TestReadMentions_Limit(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd/a.txt": "12345", "cwd/b.txt": "67890", "cwd/p.png": string(pngBytes)})
	prompt := "a.txt b.txt p.png"
	base := int64(len(prompt))

	t.Run("exactly at the limit", func(t *testing.T) {
		res := mention(t, t.Context(), cwd, prompt, base+10)
		require.NoError(t, res.Undecidable)
		assert.Equal(t, []string{"12345", "67890"}, res.Files)
		assert.Len(t, res.Attachments, 1, "binary never counts against the limit")
		assert.Equal(t, base+10, res.TextBytes)
	})
	t.Run("one byte over", func(t *testing.T) {
		res := mention(t, t.Context(), cwd, prompt, base+9)
		require.ErrorIs(t, res.Undecidable, ErrCaptureLimit)
		assert.Empty(t, res.Prompt)
		assert.Empty(t, res.Files)
		assert.Empty(t, res.Attachments)
		assert.Equal(t, base+10, res.TextBytes, "reads the remaining capacity plus one byte")
	})
	t.Run("the prompt already filled it", func(t *testing.T) {
		res := mention(t, t.Context(), cwd, prompt, base)
		require.ErrorIs(t, res.Undecidable, ErrCaptureLimit)
	})
	t.Run("stdin text shares the limit", func(t *testing.T) {
		res, err := Capture(t.Context(), prompt, strings.NewReader("xx"), base+11)
		require.NoError(t, err)
		require.NoError(t, res.Undecidable)
		require.NoError(t, res.ReadMentions(t.Context(), cwd, base+11))
		require.ErrorIs(t, res.Undecidable, ErrCaptureLimit)
	})
}

func TestReadMentions_Deadline(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd/a.txt": "a"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res := mention(t, ctx, cwd, "a.txt", testLimit)
	require.ErrorIs(t, res.Undecidable, context.Canceled)
	assert.Empty(t, res.Prompt)
	assert.Empty(t, res.Files)

	t.Run("mid-file", func(t *testing.T) {
		m := &mentions{ctx: ctx, remaining: testLimit}
		err := m.readFile(strings.NewReader("text"), 4)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestReadMentions_UnreadableFileIgnored(t *testing.T) {
	m := &mentions{ctx: t.Context(), remaining: testLimit}
	require.NoError(t, m.readFile(failingReader{}, 4))
	assert.Empty(t, m.files)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, os.ErrPermission }

func TestReadMentions_AlreadyUndecidable(t *testing.T) {
	res := &Result{Prompt: "", Undecidable: ErrCaptureLimit}
	require.NoError(t, res.ReadMentions(t.Context(), filepath.Join(t.TempDir(), "missing"), testLimit))
	assert.Equal(t, ErrCaptureLimit, res.Undecidable)
}

func TestReadMentions_BadCwd(t *testing.T) {
	res := &Result{Prompt: "a.txt"}
	require.Error(t, res.ReadMentions(t.Context(), filepath.Join(t.TempDir(), "missing"), testLimit))
}

func TestReadMentions_Escapes(t *testing.T) {
	cwd := tree(t, map[string]string{
		"secret.txt":      "outside",
		"cwd2/secret.txt": "sibling",
		"cwd/dir/in.txt":  "inside",
	})
	tmp := filepath.Dir(cwd)
	tests := []struct {
		name   string
		prompt string
	}{
		{name: "dot-dot traversal", prompt: "../secret.txt"},
		{name: "dot-dot into the sibling", prompt: "../cwd2/secret.txt"},
		{name: "absolute path outside", prompt: filepath.Join(tmp, "secret.txt")},
		{name: "absolute path in the sibling", prompt: filepath.Join(tmp, "cwd2", "secret.txt")},
		{name: "quoted traversal", prompt: "`dir/../../secret.txt`"},
		{name: "missing file", prompt: "nope.txt"},
		{name: "directory", prompt: "dir dir/ ."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := mention(t, t.Context(), cwd, tc.prompt, testLimit)
			require.NoError(t, res.Undecidable)
			assert.Empty(t, res.Files)
			assert.Empty(t, res.Attachments)
		})
	}
}

func TestReadMentions_LinkEscapingTree(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd2/secret.txt": "sibling", "cwd/dir/in.txt": "inside"})
	tmp := filepath.Dir(cwd)
	link(t, filepath.Join(tmp, "cwd2"), filepath.Join(cwd, "out"))
	link(t, filepath.Join(cwd, "dir"), filepath.Join(cwd, "in"))

	res := mention(t, t.Context(), cwd, "out/secret.txt in/in.txt dir/in.txt", testLimit)
	require.NoError(t, res.Undecidable)
	assert.Equal(t, []string{"inside"}, res.Files, "a link inside the tree is followed and deduped")
}

// link makes a directory link at name pointing to target: a junction on Windows (which Go's
// EvalSymlinks does not resolve), a symlink elsewhere.
func link(t *testing.T, target, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", name, target).CombinedOutput()
		require.NoError(t, err, string(out))
		return
	}
	require.NoError(t, os.Symlink(target, name))
}

func TestInside(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"tmp", "cwd")
	tests := []struct {
		p    string
		want bool
	}{
		{p: filepath.Join(root, "a.go"), want: true},
		{p: filepath.Join(root, "..foo"), want: true},
		{p: root, want: false},
		{p: filepath.Dir(root), want: false},
		{p: root + "2", want: false},
		{p: filepath.Join(root+"2", "a.go"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.p, func(t *testing.T) {
			assert.Equal(t, tc.want, inside(root, tc.p))
		})
	}
}
