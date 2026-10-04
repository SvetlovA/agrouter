// Package prompt builds the state agrouter sends to Jev from the explicit prompt texts (-p, the
// positional prompt, --prompt-file), stdin and the files the prompt mentions, and keeps stdin for the
// child byte-for-byte.
package prompt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Sources of an attachment.
const (
	SourceStdin     = "stdin"
	SourceMentioned = "mentioned"
)

// readSize is the size of one read from stdin.
const readSize = 32 << 10

// ErrNoPrompt means every prompt source was empty; the caller exits 2.
var ErrNoPrompt = errors.New("no prompt: give -p, a positional prompt, --prompt-file or stdin")

// sourceSep joins the prompt sources.
const sourceSep = "\n\n"

// Join joins the non-empty texts, in order, with a blank line. Bytes are kept as they are.
func Join(texts ...string) string {
	kept := make([]string, 0, len(texts))
	for _, t := range texts {
		if t != "" {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, sourceSep)
}

// Attachment is the metadata of one binary input: never its content or name.
type Attachment struct {
	Source string `json:"source"` // SourceStdin or SourceMentioned
	Type   string `json:"type"`   // detected media type or TypeUnknown
	Bytes  int64  `json:"bytes"`
	Count  int    `json:"count,omitempty"` // set only on a chunk anchor's summary: entries merged, Bytes their total
}

// Stdin is the caller's stdin as the child gets it: the bytes agrouter buffered, then the rest.
type Stdin struct {
	Buffered []byte    // read during capture, replayed first
	Rest     io.Reader // the unread remainder for live relay; nil when stdin was read to EOF
}

// Reader returns the whole of stdin: the buffered bytes, then the rest.
func (s *Stdin) Reader() io.Reader {
	if s.Rest == nil {
		return bytes.NewReader(s.Buffered)
	}
	return io.MultiReader(bytes.NewReader(s.Buffered), s.Rest)
}

// Result is the captured prompt.
type Result struct {
	Prompt      string       // Jev's prompt: the explicit texts, then stdin text, joined by Join
	Files       []string     // contents of the text files the prompt mentions (ReadMentions)
	Attachments []Attachment // binary stdin and binary mentioned files, if any
	Stdin       *Stdin       // nil when there was no stdin
	// Docs are the --doc contents, in order: routing context only, never in the routing state, the
	// child's argv or stdin, and not scanned for mentions.
	Docs []string
	// Project is what the docs say about the codebase, set by the router after the complexity stage;
	// nil without docs. It goes in the single state and in the anchor of every chunk state.
	Project *Project
	// Undecidable is why Jev cannot decide from this capture (the routing context's error); Prompt,
	// Files and Attachments are then empty, and Stdin still replays everything.
	Undecidable error
}

// StdinOf returns f as the prompt's stdin, or nil when f is a terminal (or another character device),
// which agrouter does not read.
func StdinOf(f *os.File) io.Reader {
	if f == nil {
		return nil
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return f
}

// Capture reads stdin (nil for none) to EOF under ctx, with no size limit, and builds Jev's prompt
// from the explicit texts (-p, positional, --prompt-file, in that order) followed by stdin text. It
// returns ErrNoPrompt when every explicit text is empty and there is no stdin byte, and an error when
// stdin cannot be read.
func Capture(ctx context.Context, explicit []string, stdin io.Reader) (*Result, error) {
	res := &Result{}
	c := &capturer{ctx: ctx, eof: true}
	if stdin != nil {
		c.eof = false
		c.pump = startPump(stdin)
		undecidable, err := c.run(stdin)
		res.Stdin = &Stdin{Buffered: c.buf}
		if !c.eof {
			res.Stdin.Rest = c.pump
		}
		if err != nil {
			return nil, err
		}
		if undecidable != nil {
			res.Undecidable = undecidable
			return res, nil //nolint:nilerr // undecidable is recorded in res, not returned
		}
	}
	given := Join(explicit...)
	if given == "" && len(c.buf) == 0 {
		return nil, ErrNoPrompt
	}

	if c.binary {
		res.Prompt = given
		res.Attachments = []Attachment{{Source: SourceStdin, Type: c.mediaType, Bytes: c.size}}
		return res, nil
	}
	res.Prompt = Join(given, string(c.buf))
	return res, nil
}

// capturer reads stdin through a pump so a read blocked past the deadline loses no bytes.
type capturer struct {
	ctx  context.Context
	pump *pump
	buf  []byte
	eof  bool

	binary    bool
	mediaType string
	size      int64 // binary stdin size
}

// run captures stdin. It returns why Jev cannot decide, if so, and an error when stdin fails.
func (c *capturer) run(stdin io.Reader) (undecidable, err error) {
	if err := c.fill(SniffLen); err != nil {
		return c.split(err)
	}
	prefix := c.buf[:min(len(c.buf), SniffLen)]
	c.binary, c.mediaType = Detect(prefix, c.eof && len(c.buf) <= SniffLen)

	if !c.binary {
		return c.readText(stdin)
	}
	return c.finishBinary(stdin)
}

// finishBinary measures binary stdin: the size comes from stat, or from counting.
func (c *capturer) finishBinary(stdin io.Reader) (undecidable, readErr error) {
	if size, ok := regularSize(stdin); ok {
		c.size = size
		return nil, nil
	}
	if err := c.fill(-1); err != nil {
		return c.split(err)
	}
	c.size = int64(len(c.buf))
	return nil, nil
}

// readText reads text stdin to EOF and detects again over everything read: binary anywhere still
// makes it an attachment, measured by finishBinary.
func (c *capturer) readText(stdin io.Reader) (undecidable, readErr error) {
	if err := c.fill(-1); err != nil {
		return c.split(err)
	}
	if c.binary, c.mediaType = Detect(c.buf, true); c.binary {
		return c.finishBinary(stdin)
	}
	return nil, nil
}

// split sorts a fill error into "cannot decide" (the routing deadline) or a read error.
func (c *capturer) split(err error) (undecidable, readErr error) {
	if c.ctx.Err() != nil && errors.Is(err, c.ctx.Err()) {
		return fmt.Errorf("capture stdin: %w", err), nil
	}
	return nil, fmt.Errorf("read stdin: %w", err)
}

// fill reads until at least n bytes are buffered (n < 0: to EOF), EOF, the deadline or a read error.
func (c *capturer) fill(n int64) error {
	for !c.eof && (n < 0 || int64(len(c.buf)) < n) {
		data, err := c.pump.next(c.ctx)
		c.buf = append(c.buf, data...)
		if errors.Is(err, io.EOF) {
			c.eof = true
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// regularSize returns the size of stdin when it is a regular file.
func regularSize(r io.Reader) (int64, bool) {
	f, ok := r.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return 0, false
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	return fi.Size(), true
}

// chunk is one read from stdin.
type chunk struct {
	data []byte
	err  error
}

// pump reads stdin in a goroutine. Capture takes chunks from it under the routing context; once
// capture stops, the pump is the unread remainder, serving any chunk already read first.
type pump struct {
	ch      chan chunk
	pending []byte
	err     error
}

func startPump(r io.Reader) *pump {
	p := &pump{ch: make(chan chunk)}
	go func() {
		for {
			buf := make([]byte, readSize)
			n, err := r.Read(buf)
			if n == 0 && err == nil {
				continue
			}
			p.ch <- chunk{data: buf[:n], err: err}
			if err != nil {
				return
			}
		}
	}()
	return p
}

// next returns the next chunk read from stdin, or the context's error when it ends first.
func (p *pump) next(ctx context.Context) ([]byte, error) {
	select {
	case c := <-p.ch:
		if c.err != nil {
			p.err = c.err
		}
		return c.data, c.err
	case <-ctx.Done():
		return nil, ctx.Err() //nolint:wrapcheck // sorted and wrapped by capturer.split
	}
}

// Read implements io.Reader over the rest of stdin, without a deadline.
func (p *pump) Read(b []byte) (int, error) {
	for len(p.pending) == 0 {
		if p.err != nil {
			return 0, p.err
		}
		c := <-p.ch
		p.pending, p.err = c.data, c.err
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}
