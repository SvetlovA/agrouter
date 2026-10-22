package prompt

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocRoom(t *testing.T) {
	maxIndex := strconv.Itoa(math.MaxInt)
	envelope := len(`{"doc":{"index":` + maxIndex + `,"of":` + maxIndex + `,"text":""}}`)
	for _, ratio := range []float64{2, 2.5, 3, 4} {
		assert.Equal(t, int(2000*ratio)-envelope, docRoom(Policy{BytesPerToken: ratio}, 2000), "ratio %g", ratio)
	}
}

func TestDocsState(t *testing.T) {
	r := &Result{Prompt: "fix it", Files: []string{"package a"}, Docs: []string{"# Project", "", "rules"}}
	assert.True(t, r.HasDocs())
	assert.Equal(t, DocsState{Docs: []string{"# Project", "rules"}}, r.DocsState(), "empty docs dropped, order kept")
	data, err := json.Marshal(r.DocsState())
	require.NoError(t, err)
	assert.JSONEq(t, `{"docs":["# Project","rules"]}`, string(data))

	assert.False(t, (&Result{Prompt: "fix it"}).HasDocs())
	assert.False(t, (&Result{Prompt: "fix it", Docs: []string{"", ""}}).HasDocs())
}

// checkDocChunks asserts every doc chunk state fits tokens under p, the numbering is whole, and each
// doc's chunks concatenate back to it in order.
func checkDocChunks(t *testing.T, p Policy, r *Result, tokens int, chunks []DocChunk) {
	t.Helper()
	require.NotEmpty(t, chunks)
	got := make([]string, len(r.Docs))
	last := -1
	for i, c := range chunks {
		assert.Equal(t, i+1, c.Index)
		assert.Equal(t, len(chunks), c.Of)
		assert.NotEmpty(t, c.Text)
		assert.True(t, utf8.ValidString(c.Text))
		assert.GreaterOrEqual(t, c.Doc, last, "docs in order")
		last = c.Doc
		got[c.Doc] += c.Text

		// the widest index and count, so renumbering never outgrows the budget
		wide := DocChunkState{Doc: DocChunk{Index: math.MaxInt, Of: math.MaxInt, Text: c.Text}}
		assert.LessOrEqual(t, p.Tokens(mustLen(wide)), tokens, "chunk %d", c.Index)
	}
	assert.Equal(t, r.Docs, got)
}

func TestSplitDocs(t *testing.T) {
	b := Budget{Policy: testPolicy, State: 29_000, Chunk: 28_000, Doc: 2000}

	t.Run("all docs fit one request", func(t *testing.T) {
		r := &Result{Docs: []string{"# Small", "rules"}}
		require.True(t, r.DocsFit(b))
		assert.Nil(t, r.SplitDocs(b))
	})
	t.Run("together over the budget, each doc in its own chunks", func(t *testing.T) {
		r := &Result{Docs: []string{lines("a", 50, 80), "", lines("b", 50, 80)}}
		require.LessOrEqual(t, testPolicy.Tokens(len(r.Docs[0])), b.Doc)
		require.False(t, r.DocsFit(b))
		chunks := r.SplitDocs(b)
		require.Len(t, chunks, 2, "a doc never shares a chunk")
		assert.Equal(t, 0, chunks[0].Doc)
		assert.Equal(t, 2, chunks[1].Doc)
		checkDocChunks(t, b.Policy, r, b.Doc, chunks)
	})
	t.Run("a long doc on line boundaries", func(t *testing.T) {
		r := &Result{Docs: []string{lines("arch", 1000, 100)}}
		chunks := r.SplitDocs(b)
		require.Greater(t, len(chunks), 10)
		for _, c := range chunks {
			assert.True(t, strings.HasSuffix(c.Text, "\n"), "cut on a line boundary")
		}
		checkDocChunks(t, b.Policy, r, b.Doc, chunks)
	})
	t.Run("escaped and multibyte text without newlines", func(t *testing.T) {
		r := &Result{Docs: []string{strings.Repeat(`é"<€`, 5000)}}
		checkDocChunks(t, b.Policy, r, b.Doc, r.SplitDocs(b))
	})
	t.Run("forced at half the budget", func(t *testing.T) {
		r := &Result{Docs: []string{"# Small", lines("x", 20, 80)}}
		require.True(t, r.DocsFit(b))
		chunks := r.DocChunks(b, b.Doc/2)
		require.Len(t, chunks, 2)
		checkDocChunks(t, b.Policy, r, b.Doc/2, chunks)
	})
	t.Run("a budget below the envelope still advances", func(t *testing.T) {
		r := &Result{Docs: []string{"abc"}}
		chunks := r.DocChunks(b, 0)
		require.Len(t, chunks, 1)
		assert.Equal(t, "abc", chunks[0].Text)
	})
}

func TestSplitDocsFollowsRatio(t *testing.T) {
	r := &Result{Docs: []string{lines("arch", 1000, 100)}}
	// the same text is more tokens at a smaller ratio: it fits at 8 bytes per token and splits at 2
	wide := Budget{BytesPerToken: 8, RequestTokens: 30_000, Doc: 20_000}
	require.True(t, r.DocsFit(wide))
	assert.Nil(t, r.SplitDocs(wide))

	var last int
	for _, ratio := range []float64{4, 2.5, 2, 1.5} {
		b := Budget{BytesPerToken: ratio, RequestTokens: 30_000, Doc: 20_000}
		chunks := r.SplitDocs(b)
		require.NotEmpty(t, chunks, "ratio %g", ratio)
		checkDocChunks(t, b.Policy, r, b.Doc, chunks)
		assert.GreaterOrEqual(t, len(chunks), last, "a smaller ratio never gives fewer chunks")
		last = len(chunks)
	}
	assert.Greater(t, last, 1)
}

func TestDocChunkStateJSON(t *testing.T) {
	data, err := json.Marshal(DocChunkState{Doc: DocChunk{Doc: 3, Index: 2, Of: 5, Text: "a\n"}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"doc":{"index":2,"of":5,"text":"a\n"}}`, string(data), "the doc index stays out of the state")
}
