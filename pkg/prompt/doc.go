package prompt

// DocsState is the state of a doc request carrying every doc whole.
type DocsState struct {
	Docs []string `json:"docs"`
}

// DocChunk is one part of the docs, cut to fit one doc request.
type DocChunk struct {
	Doc   int    `json:"-"`     // index into Result.Docs
	Index int    `json:"index"` // 1-based position in the whole doc chunk sequence
	Of    int    `json:"of"`
	Text  string `json:"text"`
}

// DocChunkState is the state of a doc request carrying one doc chunk.
type DocChunkState struct {
	Doc DocChunk `json:"doc"`
}

// HasDocs reports whether any doc has text, so there is something to score.
func (r *Result) HasDocs() bool {
	return nonEmpty(r.Docs) > 0
}

// DocsState returns the docs with text, in order, as one doc request's state.
func (r *Result) DocsState() DocsState {
	docs := make([]string, 0, len(r.Docs))
	for _, d := range r.Docs {
		if d != "" {
			docs = append(docs, d)
		}
	}
	return DocsState{Docs: docs}
}

// DocsTokens is the size of the whole-docs state in tokens.
func (r *Result) DocsTokens() int {
	return Tokens(mustLen(r.DocsState()))
}

// DocsFit reports whether every doc fits in one doc request.
func (r *Result) DocsFit(b Budget) bool {
	return r.DocsTokens() <= b.Doc
}

// SplitDocs returns nil when every doc fits in one doc request, and otherwise the doc chunks that
// together carry every doc byte, each within b.Doc.
func (r *Result) SplitDocs(b Budget) []DocChunk {
	if r.DocsFit(b) {
		return nil
	}
	return r.DocChunks(b.Doc)
}

// DocChunks cuts every doc with text into doc chunks whose states are within tokens, on line and
// UTF-8 boundaries, and numbers them. A doc never shares a chunk with another.
func (r *Result) DocChunks(tokens int) []DocChunk {
	room := max(docRoom(tokens), len(`\u0000`)) // at least the widest escaped rune, so cut advances
	var chunks []DocChunk
	for i, d := range r.Docs {
		for _, text := range cut(d, room) {
			chunks = append(chunks, DocChunk{Doc: i, Text: text})
		}
	}
	for i := range chunks {
		chunks[i].Index, chunks[i].Of = i+1, len(chunks)
	}
	return chunks
}

// HalveDoc cuts c into pieces of at most half its escaped text, like Halve, for a doc chunk Jev
// rejected with a 422. The pieces keep c's doc; numbering them is the caller's.
func HalveDoc(c DocChunk) []DocChunk {
	pieces := halveText(c.Text)
	out := make([]DocChunk, len(pieces))
	for i, text := range pieces {
		out[i] = DocChunk{Doc: c.Doc, Text: text}
	}
	return out
}
