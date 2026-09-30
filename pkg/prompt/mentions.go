package prompt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// ReadMentions reads the files r.Prompt mentions inside the working directory cwd: text files go into
// r.Files, binary ones into r.Attachments (SourceMentioned). Mentioned text counts against limit, the
// same text capture limit passed to Capture, and reading stops at ctx's deadline; either sets
// r.Undecidable and empties the prompt, files and attachments. Missing, unreadable and out-of-tree
// candidates, and directories, are ignored. It does nothing when r is already undecidable, and returns
// an error only when cwd cannot be resolved.
func (r *Result) ReadMentions(ctx context.Context, cwd string, limit int64) error {
	if r.Undecidable != nil {
		return nil //nolint:nilerr // Undecidable is a routing outcome, not a failure of this call
	}
	root, err := canonicalDir(cwd)
	if err != nil {
		return err
	}
	m := &mentions{ctx: ctx, root: root, remaining: limit - r.TextBytes,
		tried: map[string]bool{}, seen: map[string]bool{}}
	undecidable := m.read(r.Prompt)
	r.TextBytes += m.textBytes
	if undecidable != nil {
		r.Prompt, r.Files, r.Attachments, r.Undecidable = "", nil, nil, undecidable
		return nil //nolint:nilerr // undecidable is recorded in r, not returned
	}
	r.Files = append(r.Files, m.files...)
	r.Attachments = append(r.Attachments, m.attachments...)
	return nil
}

// canonicalDir returns the canonical path of the working directory.
func canonicalDir(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	f, root, err := openCanonical(abs)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	_ = f.Close()
	return root, nil
}

// mentions reads the files one prompt mentions.
type mentions struct {
	ctx       context.Context
	root      string          // canonical working directory
	remaining int64           // text bytes left under the capture limit
	tried     map[string]bool // candidate spellings already tried
	seen      map[string]bool // canonical paths already read (case-folded on Windows)

	files       []string
	attachments []Attachment
	textBytes   int64
}

// read reads every file text mentions, in textual order, and returns why Jev cannot decide, if so.
func (m *mentions) read(text string) error {
	for _, c := range candidates(text) {
		for _, v := range variants(c) {
			if err := m.ctx.Err(); err != nil {
				return fmt.Errorf("read mentioned files: %w", err)
			}
			done, err := m.try(v)
			if err != nil {
				return err
			}
			if done {
				break
			}
		}
	}
	return nil
}

// try reads the file one candidate spelling names. It reports whether the spelling named a regular
// file inside the working directory, and returns why Jev cannot decide, if so.
func (m *mentions) try(name string) (bool, error) {
	if name == "" || m.tried[name] {
		return false, nil
	}
	m.tried[name] = true
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.root, path)
	}
	f, canon, err := openCanonical(path)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !inside(m.root, canon) {
		return false, nil
	}
	key := samePathKey(canon)
	if m.seen[key] {
		return true, nil
	}
	m.seen[key] = true
	return true, m.readFile(f, fi.Size())
}

// readFile sniffs one mentioned file and adds it as text or as an attachment. Only text is read past
// the sniffed prefix, up to the remaining capacity plus one byte to detect overflow.
func (m *mentions) readFile(f io.Reader, size int64) error {
	buf, eof, err := m.readUpTo(f, nil, SniffLen)
	if err != nil {
		return m.split(err)
	}
	if binary, mediaType := Detect(buf, eof); binary {
		m.attach(mediaType, size)
		return nil
	}
	if !eof {
		if buf, eof, err = m.readUpTo(f, buf, max(m.remaining+1, 0)); err != nil {
			return m.split(err)
		}
	}
	binary, mediaType := Detect(buf, eof)
	switch {
	case binary:
		m.attach(mediaType, size)
	case int64(len(buf)) > m.remaining:
		m.textBytes += int64(len(buf))
		return ErrCaptureLimit
	default:
		m.files = append(m.files, string(buf))
		m.textBytes += int64(len(buf))
		m.remaining -= int64(len(buf))
	}
	return nil
}

func (m *mentions) attach(mediaType string, size int64) {
	m.attachments = append(m.attachments, Attachment{Source: SourceMentioned, Type: mediaType, Bytes: size})
}

// split sorts a read error: the routing deadline means Jev cannot decide; any other error is an
// unreadable file, which is ignored.
func (m *mentions) split(err error) error {
	if m.ctx.Err() != nil && errors.Is(err, m.ctx.Err()) {
		return fmt.Errorf("read mentioned files: %w", err)
	}
	return nil
}

// readUpTo appends reads from f to buf until it holds n bytes or f ends, checking the routing
// context between reads. It reports whether f ended.
func (m *mentions) readUpTo(f io.Reader, buf []byte, n int64) ([]byte, bool, error) {
	for int64(len(buf)) < n {
		if err := m.ctx.Err(); err != nil {
			return buf, false, err //nolint:wrapcheck // sorted and wrapped by mentions.split
		}
		chunk := make([]byte, min(n-int64(len(buf)), readSize))
		k, err := f.Read(chunk)
		buf = append(buf, chunk[:k]...)
		if errors.Is(err, io.EOF) {
			return buf, true, nil
		}
		if err != nil {
			return buf, false, fmt.Errorf("read: %w", err)
		}
	}
	return buf, false, nil
}

// inside reports whether p lies strictly inside root, by directory boundary rather than string prefix.
func inside(root, p string) bool {
	rel, err := filepath.Rel(samePathKey(root), samePathKey(p))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// candidates returns the possible paths in text in textual order: quoted, backtick and Markdown-link
// spans, and the whitespace-separated tokens of the text around them. URLs are dropped.
func candidates(text string) []string {
	var out []string
	add := func(c string) {
		if c != "" && !strings.Contains(c, "://") {
			out = append(out, c)
		}
	}
	last := 0
	for i := 0; i < len(text); {
		start, end, inner, ok := span(text, i)
		if !ok {
			i++
			continue
		}
		for tok := range strings.FieldsSeq(text[last:start]) {
			add(tok)
		}
		add(inner)
		last, i = end, end
	}
	for tok := range strings.FieldsSeq(text[last:]) {
		add(tok)
	}
	return out
}

// span finds a quoted, backtick or Markdown-link span starting at text[i]. It returns the span's bounds
// (delimiters included) and its content.
func span(text string, i int) (start, end int, inner string, ok bool) {
	switch c := text[i]; {
	case c == '`' || c == '"':
		return closeSpan(text, i, i+1, c, func(int) bool { return true })
	case c == '\'' && (i == 0 || opensQuote(rune(text[i-1]))):
		// a single quote only opens and closes at word boundaries, so apostrophes stay words
		return closeSpan(text, i, i+1, c, func(j int) bool { return j+1 == len(text) || closesQuote(rune(text[j+1])) })
	case c == ']' && strings.HasPrefix(text[i:], "]("):
		start, end, inner, ok = closeSpan(text, i, i+2, ')', func(int) bool { return true })
		inner = strings.TrimSuffix(strings.TrimPrefix(inner, "<"), ">")
		return start, end, inner, ok
	}
	return 0, 0, "", false
}

// closeSpan finds the delimiter that closes a span opened at text[start], searching from from on the
// same line; accept vets each closing candidate by its index.
func closeSpan(text string, start, from int, delim byte, accept func(int) bool) (int, int, string, bool) {
	for j := from; j < len(text) && text[j] != '\n'; j++ {
		if text[j] == delim && accept(j) {
			if j == from {
				return 0, 0, "", false
			}
			return start, j + 1, text[from:j], true
		}
	}
	return 0, 0, "", false
}

func opensQuote(r rune) bool  { return unicode.IsSpace(r) || strings.ContainsRune("([{<", r) }
func closesQuote(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(".,;:!?)]}>", r) }

// lineSuffix matches a numeric :line or :line:column suffix.
var lineSuffix = regexp.MustCompile(`^(.+?)(?::\d+){1,2}$`)

// variants returns the spellings to try for one candidate, in order: as written, without surrounding
// brackets and trailing punctuation, then also without a :line or :line:column suffix.
func variants(c string) []string {
	out := []string{c}
	trimmed := trimPunct(c)
	if trimmed != c {
		out = append(out, trimmed)
	}
	if mt := lineSuffix.FindStringSubmatch(trimmed); len(mt) > 1 && !isDrive(mt[1]) {
		out = append(out, mt[1])
	}
	return out
}

// trimPunct removes surrounding brackets and quotes, and trailing punctuation.
func trimPunct(c string) string {
	for {
		t := strings.TrimLeft(c, "([{<\"'")
		t = strings.TrimRight(t, ")]}>\"'.,;:!?")
		if t == c {
			return t
		}
		c = t
	}
}

// isDrive reports whether p is a bare Windows drive such as "C", which a :line suffix never leaves.
func isDrive(p string) bool {
	return len(p) == 1 && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}
