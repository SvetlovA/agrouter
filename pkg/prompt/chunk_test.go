package prompt

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokens(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 1, 3: 1, 4: 2, 3000: 1000} {
		assert.Equal(t, want, Tokens(n), "bytes %d", n)
	}
}

func TestNewBudget(t *testing.T) {
	t.Run("from the questions", func(t *testing.T) {
		b, err := NewBudget(Questions{Route: 3000, ChunkRoute: 6000, Relevance: 300, Complexity: 1500, Evidence: 900})
		require.NoError(t, err)
		assert.Equal(t, Budget{State: 29_000, Chunk: 28_000, Doc: 29_500}, b)
	})
	t.Run("longest doc question sets the doc budget", func(t *testing.T) {
		b, err := NewBudget(Questions{Complexity: 300, Evidence: 9000})
		require.NoError(t, err)
		assert.Equal(t, 27_000, b.Doc)
	})
	t.Run("doc state plus both doc questions within 64k", func(t *testing.T) {
		b, err := NewBudget(Questions{Complexity: 60_000, Evidence: 60_000})
		require.NoError(t, err)
		assert.LessOrEqual(t, b.Doc+20_000+20_000, totalLimit)
		assert.LessOrEqual(t, b.Doc+20_000, stateLimit)
	})
	t.Run("doc questions over budget", func(t *testing.T) {
		_, err := NewBudget(Questions{Complexity: 84_003})
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		assert.Contains(t, err.Error(), "complexity_question and complexity_evidence leave 1999 tokens for the doc state")
		_, err = NewBudget(Questions{Complexity: 84_000})
		require.NoError(t, err)
	})
	t.Run("longest chunk question sets the chunk budget", func(t *testing.T) {
		b, err := NewBudget(Questions{ChunkRoute: 300, Relevance: 9000})
		require.NoError(t, err)
		assert.Equal(t, 27_000, b.Chunk)
	})
	t.Run("state plus both questions within 64k", func(t *testing.T) {
		b, err := NewBudget(Questions{ChunkRoute: 60_000, Relevance: 60_000})
		require.NoError(t, err)
		assert.LessOrEqual(t, b.Chunk+20_000+20_000, totalLimit)
		assert.LessOrEqual(t, b.Chunk+20_000, stateLimit)
	})
	t.Run("question over budget", func(t *testing.T) {
		_, err := NewBudget(Questions{Route: 84_003})
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		assert.Contains(t, err.Error(), "question leaves 1999 tokens")
	})
	t.Run("chunk questions over budget beside a maximal anchor", func(t *testing.T) {
		_, err := NewBudget(Questions{ChunkRoute: 72_003})
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		assert.Contains(t, err.Error(), "leave 1999 tokens beside the anchor")
		_, err = NewBudget(Questions{ChunkRoute: 72_000})
		require.NoError(t, err)
	})
}

func TestChunkRoom(t *testing.T) {
	maxIndex := strconv.Itoa(math.MaxInt)
	assert.Len(t, maxIndex, indexDigits)
	b := Budget{State: 29_000, Chunk: 28_000}
	envelope := len(`{"anchor":,"chunk":{"field":"prompt","index":` + maxIndex + `,"of":` + maxIndex + `,"text":""}}`)
	assert.Equal(t, 28_000*3-12_000-envelope, chunkRoom(b, 12_000))
}

func TestJSONLen(t *testing.T) {
	for _, s := range []string{
		"", "plain", "quote \" and \\ back", "\n\r\t\b\f", "\x00\x01\x1f", "<a> & b",
		"é ✓ 🙂", "\u2028\u2029",
	} {
		data, err := json.Marshal(s)
		require.NoError(t, err)
		assert.Equal(t, len(data)-2, jsonLen(s), "%q", s)
	}
	// invalid UTF-8: each invalid byte counts as the escaped \ufffd of encoding/json v1, an upper
	// bound for the raw 3-byte rune of the v2-based encoder
	for _, s := range []string{"bad \xff utf-8", "trailing \xe2\x82"} {
		data, err := json.Marshal(s)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, jsonLen(s), len(data)-2, "%q", s)
		valid := strings.ToValidUTF8(s, "")
		assert.Equal(t, len(valid)+len(`\ufffd`)*(len(s)-len(valid)), jsonLen(s), "%q", s)
	}
}

func TestExcerptJSON(t *testing.T) {
	data, err := json.Marshal(Anchor{Prompt: &Excerpt{Whole: true, Head: "do it"}, Files: []Excerpt{{Head: "a"}, {Head: "h", Tail: "t"}}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"prompt":"do it","files":[{"head":"a"},{"head":"h","tail":"t"}]}`, string(data))
}

func TestState(t *testing.T) {
	r := &Result{Prompt: "p", Files: []string{"f"}, Attachments: []Attachment{{Source: SourceStdin, Type: "image/png", Bytes: 3}}}
	data, err := json.Marshal(r.State())
	require.NoError(t, err)
	assert.JSONEq(t, `{"prompt":"p","files":["f"],"attachments":[{"source":"stdin","type":"image/png","bytes":3}]}`, string(data))

	r.Docs = []string{"project doc"}
	data, err = json.Marshal(r.State())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "project doc", "docs are not routing state")

	r.Project = &Project{Complexity: 0}
	data, err = json.Marshal(r.State())
	require.NoError(t, err)
	assert.Contains(t, string(data), `"project":{"complexity":0}`, "a zero complexity is still sent")
}

func TestSplitWithProject(t *testing.T) {
	b := Budget{State: 1_000, Chunk: 4_000 + MinStateTokens}
	r := &Result{Prompt: lines("p", 10, 80), Files: []string{lines("f", 400, 80)}, Project: &Project{Complexity: 6.9}}
	s, err := r.Split(b)
	require.NoError(t, err)
	checkSplit(t, r, b, s)
	assert.Equal(t, &Project{Complexity: 6.9}, s.Anchor.Project, "every chunk repeats the project in the anchor")

	// a prompt that overflows the anchor leaves the project in it, within the anchor share
	r = &Result{Prompt: lines("p", 400, 80), Project: &Project{Complexity: 6.9}}
	s, err = r.Split(b)
	require.NoError(t, err)
	checkSplit(t, r, b, s)
	assert.Equal(t, &Project{Complexity: 6.9}, s.Anchor.Project)
	assert.Contains(t, string(mustJSON(t, s.Anchor)), `"project":{"complexity":6.9}`)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

// lines returns n numbered lines, each about width bytes.
func lines(prefix string, n, width int) string {
	var sb strings.Builder
	for i := range n {
		line := fmt.Sprintf("%s line %d ", prefix, i)
		sb.WriteString(line + strings.Repeat("x", max(width-len(line)-1, 0)) + "\n")
	}
	return sb.String()
}

// checkSplit asserts every chunk request's state is within budget and every captured byte comes
// back from the anchor-only fields and the chunks.
func checkSplit(t *testing.T, r *Result, b Budget, s *Split) {
	t.Helper()
	require.NotNil(t, s)
	require.NotEmpty(t, s.Chunks)
	assert.LessOrEqual(t, Tokens(mustLen(s.Anchor)), AnchorTokens)
	assert.LessOrEqual(t, Tokens(mustLen(s.Anchor.Attachments)), attachmentShare)

	prompt, files := "", []string(nil)
	if r.Files != nil {
		files = make([]string, len(r.Files))
	}
	promptChunked := false
	for i, c := range s.Chunks {
		assert.Equal(t, i+1, c.Index)
		assert.Equal(t, len(s.Chunks), c.Of)
		assert.True(t, utf8.ValidString(c.Text), "chunk %d cut inside a character", c.Index)
		assert.LessOrEqual(t, Tokens(mustLen(ChunkState{Anchor: s.Anchor, Chunk: c})), b.Chunk, "chunk %d over budget", c.Index)
		widest := c
		widest.Index, widest.Of = math.MaxInt, math.MaxInt
		assert.LessOrEqual(t, Tokens(mustLen(ChunkState{Anchor: s.Anchor, Chunk: widest})), b.Chunk,
			"chunk %d over budget once renumbered to the widest index", c.Index)
		switch c.Field {
		case FieldPrompt:
			promptChunked = true
			prompt += c.Text
		case FieldFiles:
			files[c.File] += c.Text
		default:
			t.Fatalf("chunk %d: field %q", c.Index, c.Field)
		}
	}
	if !promptChunked {
		require.NotNil(t, s.Anchor.Prompt)
		require.True(t, s.Anchor.Prompt.Whole, "a prompt not in the chunks is whole in the anchor")
		prompt = s.Anchor.Prompt.Head
	} else if s.Anchor.Prompt != nil {
		assert.False(t, s.Anchor.Prompt.Whole, "a whole prompt is not repeated in the chunks")
	}
	assert.Equal(t, r.Prompt, prompt)
	assert.Equal(t, r.Files, files)
}

func TestSplit(t *testing.T) {
	b := Budget{State: 29_000, Chunk: 28_000}

	t.Run("a state under budget is not split", func(t *testing.T) {
		r := &Result{Prompt: lines("p", 100, 80), Files: []string{lines("f", 100, 80)}}
		assert.True(t, r.Fits(b))
		s, err := r.Split(b)
		require.NoError(t, err)
		assert.Nil(t, s)
	})

	t.Run("docs never make a short prompt leave the anchor", func(t *testing.T) {
		huge := []string{lines("doc", 4000, 100), lines("doc2", 4000, 100)}
		short := &Result{Prompt: "fix the typo", Docs: huge}
		assert.True(t, short.Fits(b), "docs don't count toward the state")
		s, err := short.Split(b)
		require.NoError(t, err)
		assert.Nil(t, s)

		withFiles := &Result{Prompt: "fix a.go", Files: []string{lines("a", 1000, 100)}, Docs: huge}
		s, err = withFiles.Split(b)
		require.NoError(t, err)
		checkSplit(t, withFiles, b, s)
		assert.Equal(t, &Excerpt{Whole: true, Head: "fix a.go"}, s.Anchor.Prompt)
		for _, c := range s.Chunks {
			assert.Equal(t, FieldFiles, c.Field, "no doc text in the chunks")
			assert.NotContains(t, c.Text, "doc line")
		}
	})

	t.Run("short instruction plus long stdin keeps the instruction in every anchor", func(t *testing.T) {
		r := &Result{Prompt: "run the next task in the plan\n\n" + lines("stdin", 4000, 100) + "report when done"}
		s, err := r.Split(b)
		require.NoError(t, err)
		checkSplit(t, r, b, s)
		assert.Greater(t, len(s.Chunks), 1)
		require.NotNil(t, s.Anchor.Prompt)
		assert.True(t, strings.HasPrefix(s.Anchor.Prompt.Head, "run the next task in the plan\n\n"))
		assert.True(t, strings.HasSuffix(s.Anchor.Prompt.Tail, "report when done"))
		assert.Empty(t, s.Anchor.Files, "an overflowing prompt takes the room left")
	})

	t.Run("whole prompt and head and tail of files", func(t *testing.T) {
		r := &Result{Prompt: "fix a.go and b.go", Files: []string{lines("a", 1000, 100), "", lines("b", 1000, 100), "tiny"}}
		s, err := r.Split(b)
		require.NoError(t, err)
		checkSplit(t, r, b, s)
		assert.Equal(t, &Excerpt{Whole: true, Head: "fix a.go and b.go"}, s.Anchor.Prompt)
		require.Len(t, s.Anchor.Files, 3)
		assert.True(t, strings.HasPrefix(s.Anchor.Files[0].Head, "a line 0 "))
		assert.True(t, strings.HasSuffix(s.Anchor.Files[0].Tail, "x\n"))
		assert.True(t, strings.HasPrefix(s.Anchor.Files[1].Head, "b line 0 "))
		assert.Equal(t, Excerpt{Head: "tiny"}, s.Anchor.Files[2])
		assert.Equal(t, FieldFiles, s.Chunks[0].Field)
	})

	t.Run("files stop filling the anchor when room runs out", func(t *testing.T) {
		r := &Result{Prompt: strings.Repeat("p", 11_900), Files: []string{lines("a", 1000, 100), lines("b", 1000, 100)}}
		s, err := r.Split(b)
		require.NoError(t, err)
		checkSplit(t, r, b, s)
		assert.True(t, s.Anchor.Prompt.Whole)
		assert.Empty(t, s.Anchor.Files)
	})

	t.Run("attachments in every anchor even when the prompt overflows", func(t *testing.T) {
		atts := []Attachment{{Source: SourceStdin, Type: "image/png", Bytes: 48_213}, {Source: SourceMentioned, Type: "application/pdf", Bytes: 7}}
		r := &Result{Prompt: lines("p", 3000, 100), Attachments: atts}
		s, err := r.Split(b)
		require.NoError(t, err)
		checkSplit(t, r, b, s)
		assert.Equal(t, atts, s.Anchor.Attachments)
		assert.False(t, s.Anchor.Prompt.Whole)
		data, err := json.Marshal(ChunkState{Anchor: s.Anchor, Chunk: s.Chunks[1]})
		require.NoError(t, err)
		assert.Contains(t, string(data), `{"anchor":{"attachments":[{"source":"stdin","type":"image/png","bytes":48213},`)
		assert.Contains(t, string(data), `"chunk":{"field":"prompt","index":2,"of":`)
	})

	t.Run("long attachment list summarized by source and type", func(t *testing.T) {
		atts := make([]Attachment, 0, 301)
		for i := range 150 {
			atts = append(atts,
				Attachment{Source: SourceMentioned, Type: "image/png", Bytes: int64(i)},
				Attachment{Source: SourceMentioned, Type: "application/pdf", Bytes: 1})
		}
		atts = append(atts, Attachment{Source: SourceStdin, Type: "image/png", Bytes: 5})
		r := &Result{Prompt: "describe these", Attachments: atts}
		require.False(t, r.Fits(Budget{State: 3000}))
		s, err := r.Split(Budget{State: 3000, Chunk: b.Chunk})
		require.NoError(t, err)
		checkSplit(t, r, b, s)
		assert.Equal(t, []Attachment{
			{Source: SourceMentioned, Type: "image/png", Bytes: 149 * 150 / 2, Count: 150},
			{Source: SourceMentioned, Type: "application/pdf", Bytes: 150, Count: 150},
			{Source: SourceStdin, Type: "image/png", Bytes: 5, Count: 1},
		}, s.Anchor.Attachments)
		// only the attachments overflowed: the prompt is the one chunk
		assert.Nil(t, s.Anchor.Prompt)
		assert.Equal(t, []Chunk{{Field: FieldPrompt, Index: 1, Of: 1, Text: "describe these"}}, s.Chunks)
	})

	t.Run("binary-only stdin with many attachments still has a chunk", func(t *testing.T) {
		atts := make([]Attachment, 0, 200)
		for i := range 200 {
			atts = append(atts, Attachment{Source: SourceStdin, Type: "image/png", Bytes: int64(i)})
		}
		r := &Result{Attachments: atts}
		s, err := r.Split(Budget{State: 1000, Chunk: b.Chunk})
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Field: FieldPrompt, Index: 1, Of: 1}}, s.Chunks)
	})

	t.Run("summary still over 1k", func(t *testing.T) {
		atts := make([]Attachment, 0, 200)
		for i := range 200 {
			atts = append(atts, Attachment{Source: SourceMentioned, Type: fmt.Sprintf("application/x-kind-%d", i), Bytes: 1})
		}
		r := &Result{Prompt: "p", Attachments: atts}
		_, err := r.Split(Budget{State: 1000, Chunk: b.Chunk})
		require.ErrorIs(t, err, ErrAttachmentsOverAnchor)
	})

	t.Run("any number of chunks", func(t *testing.T) {
		r := &Result{Prompt: lines("p", 100_000, 100), Files: []string{lines("f", 20_000, 100)}}
		s, err := r.Split(b)
		require.NoError(t, err)
		assert.Greater(t, len(s.Chunks), 100)
		checkSplit(t, r, b, s)
	})

	t.Run("no room for chunk text", func(t *testing.T) {
		r := &Result{Prompt: lines("p", 4000, 100)}
		_, err := r.Split(Budget{State: 1000, Chunk: 4000})
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
	})

	t.Run("multibyte text without newlines", func(t *testing.T) {
		r := &Result{Prompt: strings.Repeat("é✓🙂<", 30_000)}
		s, err := r.Split(b)
		require.NoError(t, err)
		checkSplit(t, r, b, s)
		assert.True(t, utf8.ValidString(s.Anchor.Prompt.Head))
		assert.True(t, utf8.ValidString(s.Anchor.Prompt.Tail))
	})
}

func TestCut(t *testing.T) {
	t.Run("on line boundaries", func(t *testing.T) {
		text := "aaaa\nbbbb\ncccc\ndd"
		assert.Equal(t, []string{"aaaa\nbbbb\n", "cccc\ndd"}, cut(text, 12))
		assert.Equal(t, []string{"aaaa\n", "bbbb\n", "cccc\n", "dd"}, cut(text, 6))
	})
	t.Run("inside an overlong line on UTF-8 boundaries", func(t *testing.T) {
		pieces := cut("ab✓✓cd", 4)
		assert.Equal(t, []string{"ab", "✓", "✓c", "d"}, pieces)
	})
	t.Run("escaped length counts", func(t *testing.T) {
		assert.Equal(t, []string{"<", "<"}, cut("<<", 6))
		assert.Equal(t, []string{"a\n", "b"}, cut("a\nb", 3))
	})
	t.Run("empty", func(t *testing.T) {
		assert.Nil(t, cut("", 10))
	})
}

func TestHeadTail(t *testing.T) {
	head, tail := headTail("0123456789", 6)
	assert.Equal(t, "012", head)
	assert.Equal(t, "789", tail)
	head, tail = headTail("short", 6)
	assert.Equal(t, "short", head)
	assert.Empty(t, tail)
	head, tail = headTail("abc", 0)
	assert.Empty(t, head)
	assert.Empty(t, tail)
	head, tail = headTail("✓✓✓✓", 5)
	assert.Empty(t, head)
	assert.Equal(t, "✓", tail)
}

func TestHalve(t *testing.T) {
	t.Run("lines", func(t *testing.T) {
		text := strings.Repeat("line of text\n", 100)
		pieces := Halve(Chunk{Field: FieldFiles, File: 2, Index: 5, Of: 9, Text: text})
		require.GreaterOrEqual(t, len(pieces), 2)
		var joined strings.Builder
		for _, p := range pieces {
			assert.Equal(t, FieldFiles, p.Field)
			assert.Equal(t, 2, p.File)
			assert.Zero(t, p.Index, "numbering is the caller's")
			assert.LessOrEqual(t, jsonLen(p.Text), (jsonLen(text)+1)/2)
			assert.True(t, strings.HasSuffix(p.Text, "\n"), "cut on a line boundary")
			joined.WriteString(p.Text)
		}
		assert.Equal(t, text, joined.String())
	})
	t.Run("multibyte without newlines", func(t *testing.T) {
		text := strings.Repeat("é€", 50)
		pieces := Halve(Chunk{Field: FieldPrompt, Text: text})
		require.Len(t, pieces, 2)
		for _, p := range pieces {
			assert.True(t, utf8.ValidString(p.Text))
		}
		assert.Equal(t, text, pieces[0].Text+pieces[1].Text)
	})
	t.Run("tiny and empty", func(t *testing.T) {
		assert.Equal(t, []Chunk{{Field: FieldPrompt, Text: "\x01"}}, Halve(Chunk{Field: FieldPrompt, Text: "\x01"}))
		assert.Equal(t, []Chunk{{Field: FieldPrompt}}, Halve(Chunk{Field: FieldPrompt}))
	})
}
