package prompt

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Fields a chunk comes from.
const (
	FieldPrompt = "prompt"
	FieldFiles  = "files"
)

var (
	// ErrAttachmentsOverAnchor means the attachments exceed their anchor share even summarised, so
	// Jev cannot decide.
	ErrAttachmentsOverAnchor = errors.New("attachments over their anchor share")
	// ErrTooManyChunks means the state needs more than max_chunks chunks, so Jev cannot decide.
	ErrTooManyChunks = errors.New("state needs more chunks than max_chunks")
)

// minFileShare is the least anchor room, in bytes, worth giving one file's head and tail.
const minFileShare = 64

// State is the state of a single Jev request.
type State struct {
	Prompt      string       `json:"prompt"`
	Files       []string     `json:"files,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// State returns the captured prompt as a single request's state.
func (r *Result) State() State {
	return State{Prompt: r.Prompt, Files: r.Files, Attachments: r.Attachments}
}

// Excerpt is text in the anchor: a whole field (Whole), or the head and tail of a longer one.
type Excerpt struct {
	Whole bool
	Head  string // the whole text when Whole
	Tail  string
}

// MarshalJSON encodes a whole field as a string and an excerpt as {"head","tail"}.
func (e Excerpt) MarshalJSON() ([]byte, error) {
	if e.Whole {
		return json.Marshal(e.Head) //nolint:wrapcheck // plain string encoding
	}
	return json.Marshal(struct { //nolint:wrapcheck // plain struct encoding
		Head string `json:"head"`
		Tail string `json:"tail,omitempty"`
	}{e.Head, e.Tail})
}

// Anchor is the part of the state repeated in every chunk request.
type Anchor struct {
	Attachments []Attachment `json:"attachments,omitempty"`
	Prompt      *Excerpt     `json:"prompt,omitempty"`
	Files       []Excerpt    `json:"files,omitempty"`
}

// Chunk is one part of the text not kept whole in the anchor.
type Chunk struct {
	Field string `json:"field"` // FieldPrompt or FieldFiles
	File  int    `json:"-"`     // index into Result.Files for FieldFiles
	Index int    `json:"index"` // 1-based position in the whole chunk sequence
	Of    int    `json:"of"`
	Text  string `json:"text"`
}

// Split is a state too large for one request: the anchor and the chunks, one request each.
type Split struct {
	Anchor Anchor
	Chunks []Chunk
}

// ChunkState is the state of one chunk request.
type ChunkState struct {
	Anchor Anchor `json:"anchor"`
	Chunk  Chunk  `json:"chunk"`
}

// Fits reports whether the whole state fits in one request.
func (r *Result) Fits(b Budget) bool {
	return Tokens(mustLen(r.State())) <= b.State
}

// Split returns nil when the state fits in one request, and otherwise the anchor and the chunks
// that together carry every captured byte. It returns ErrAttachmentsOverAnchor or ErrTooManyChunks
// when Jev cannot decide, and ErrQuestionsOverBudget when the budget leaves no room for chunk text.
func (r *Result) Split(b Budget, maxChunks int) (*Split, error) {
	if r.Fits(b) {
		return nil, nil //nolint:nilnil // nil split: send the state whole
	}
	anchor, promptWhole, err := r.anchor()
	if err != nil {
		return nil, err
	}
	if promptWhole && nonEmpty(r.Files) == 0 {
		// only the attachments overflowed: the prompt moves from the anchor to the chunks, so
		// there is always a chunk to ask about
		anchor.Prompt, promptWhole = nil, false
	}
	room := chunkRoom(b, mustLen(anchor), maxChunks)
	if room < len(`�`) {
		return nil, fmt.Errorf("%w: no room for chunk text", ErrQuestionsOverBudget)
	}

	var chunks []Chunk
	if !promptWhole {
		for _, text := range cut(r.Prompt, room) {
			chunks = append(chunks, Chunk{Field: FieldPrompt, Text: text})
		}
		if r.Prompt == "" && nonEmpty(r.Files) == 0 {
			chunks = []Chunk{{Field: FieldPrompt}}
		}
	}
	for i, f := range r.Files {
		for _, text := range cut(f, room) {
			chunks = append(chunks, Chunk{Field: FieldFiles, File: i, Text: text})
		}
	}
	if len(chunks) > maxChunks {
		return nil, fmt.Errorf("%w: %d chunks, max_chunks is %d", ErrTooManyChunks, len(chunks), maxChunks)
	}
	for i := range chunks {
		chunks[i].Index, chunks[i].Of = i+1, len(chunks)
	}
	return &Split{Anchor: anchor, Chunks: chunks}, nil
}

// anchor builds the anchor within AnchorTokens: attachments (summarised when over their share),
// then the prompt, whole or head and tail, then head and tail of the files while room lasts. It
// reports whether the prompt is whole in it.
func (r *Result) anchor() (Anchor, bool, error) {
	limit := AnchorTokens * bytesPerToken
	var a Anchor
	if len(r.Attachments) > 0 {
		a.Attachments = r.Attachments
		if Tokens(mustLen(a.Attachments)) > attachmentShare {
			a.Attachments = summarise(r.Attachments)
		}
		if n := Tokens(mustLen(a.Attachments)); n > attachmentShare {
			return Anchor{}, false, fmt.Errorf("%w: %d tokens summarised, share is %d", ErrAttachmentsOverAnchor, n, attachmentShare)
		}
	}

	a.Prompt = &Excerpt{Whole: true, Head: r.Prompt}
	promptWhole := mustLen(a) <= limit
	if !promptWhole {
		a.Prompt = &Excerpt{}
		a.Prompt.Head, a.Prompt.Tail = headTail(r.Prompt, limit-mustLen(a)-len(`,"tail":""`))
		return a, false, nil
	}

	for i, f := range r.Files {
		if f == "" {
			continue
		}
		overhead := len(`,{"head":"","tail":""}`)
		if a.Files == nil {
			overhead += len(`,"files":[]`)
		}
		share := (limit - mustLen(a) - overhead) / nonEmpty(r.Files[i:])
		if share < minFileShare {
			break
		}
		head, tail := headTail(f, share)
		a.Files = append(a.Files, Excerpt{Head: head, Tail: tail})
	}
	return a, true, nil
}

// headTail returns the head and tail of s within room escaped bytes together, cut on UTF-8
// boundaries; s shorter than room is all head.
func headTail(s string, room int) (head, tail string) {
	if room <= 0 {
		return "", ""
	}
	if jsonLen(s) <= room {
		return s, ""
	}
	head = s[:prefixLen(s, room/2)]
	tail = s[len(s)-suffixLen(s, room-room/2):]
	return head, tail
}

// summarise merges attachments with the same source and type, in order of first appearance.
func summarise(atts []Attachment) []Attachment {
	type key struct{ source, typ string }
	idx := map[key]int{}
	var out []Attachment
	for _, at := range atts {
		k := key{at.Source, at.Type}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, Attachment{Source: at.Source, Type: at.Type})
		}
		out[i].Count += max(at.Count, 1)
		out[i].Bytes += at.Bytes
	}
	return out
}

// cut splits text into pieces of at most room escaped bytes, cutting after the last newline that
// fits, or inside an overlong line on a UTF-8 boundary. The pieces concatenate to text.
func cut(text string, room int) []string {
	var out []string
	for text != "" {
		end, lineEnd, n := 0, 0, 0
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			w := escapedLen(r, size)
			if n+w > room {
				break
			}
			n += w
			end += size
			if r == '\n' {
				lineEnd = end
			}
		}
		if end < len(text) && lineEnd > 0 {
			end = lineEnd
		}
		out = append(out, text[:end])
		text = text[end:]
	}
	return out
}

// prefixLen is the byte length of the longest prefix of s within room escaped bytes.
func prefixLen(s string, room int) int {
	end, n := 0, 0
	for end < len(s) {
		r, size := utf8.DecodeRuneInString(s[end:])
		if n += escapedLen(r, size); n > room {
			break
		}
		end += size
	}
	return end
}

// suffixLen is the byte length of the longest suffix of s within room escaped bytes.
func suffixLen(s string, room int) int {
	start, n := len(s), 0
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:start])
		if n += escapedLen(r, size); n > room {
			break
		}
		start -= size
	}
	return len(s) - start
}

func nonEmpty(files []string) int {
	n := 0
	for _, f := range files {
		if f != "" {
			n++
		}
	}
	return n
}

// mustLen is the length of v encoded as JSON; v is always one of this package's plain types.
func mustLen(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("prompt: encode %T: %v", v, err))
	}
	return len(data)
}
