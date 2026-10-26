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

// DocsTokens is the estimated size in tokens of the whole-docs state under p.
func (r *Result) DocsTokens(p Policy) int {
	return p.Tokens(mustLen(r.DocsState()))
}

// DocsFit reports whether every doc fits in one doc request.
func (r *Result) DocsFit(b Budget) bool {
	return r.DocsTokens(b.Policy) <= b.Doc
}

// SplitDocs returns nil when every doc fits in one doc request, and otherwise the doc chunks that
// together carry every doc byte, each within b.Doc.
func (r *Result) SplitDocs(b Budget) []DocChunk {
	if r.DocsFit(b) {
		return nil
	}
	return r.DocChunks(b, b.Doc)
}

// DocChunks cuts every doc with text into doc chunks whose states are within tokens under b's
// policy, on line and UTF-8 boundaries, and numbers them. A doc never shares a chunk with another.
func (r *Result) DocChunks(b Budget, tokens int) []DocChunk {
	room := max(docRoom(b.Policy, tokens), len(`\u0000`)) // at least the widest escaped rune, so cut advances
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
