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

// testPolicy is the default estimator at the default allowance; tests of other ratios set their own.
var testPolicy = Policy{BytesPerToken: 3, RequestTokens: 30_000}

func TestPolicyTokens(t *testing.T) {
	tests := []struct {
		name  string
		ratio float64
		n     int
		want  int
	}{
		{"zero bytes", 3, 0, 0},
		{"one byte", 3, 1, 1},
		{"exact multiple", 3, 3000, 1000},
		{"rounds up", 3, 4, 2},
		{"fractional ratio rounds up", 2.5, 6, 3},
		{"fractional ratio exact", 2.5, 5, 2},
		{"ratio below one", 0.5, 3, 6},
		{"negative bytes", 3, -7, 0},
		{"very large ratio", 1e300, 1, 1},
		{"very small ratio saturates", 1e-300, 1, math.MaxInt},
		{"widest count saturates", 1, math.MaxInt, math.MaxInt},
		{"zero ratio saturates", 0, 1, math.MaxInt},
		{"NaN ratio saturates", math.NaN(), 1, math.MaxInt},
		{"negative ratio is zero", -1, 5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Policy{BytesPerToken: tc.ratio}.Tokens(tc.n))
		})
	}
}

func TestPolicyBytes(t *testing.T) {
	tests := []struct {
		name   string
		ratio  float64
		tokens int
		want   int
	}{
		{"zero tokens", 3, 0, 0},
		{"one token", 3, 1, 3},
		{"many tokens", 3, 1000, 3000},
		{"fractional ratio rounds down", 2.5, 3, 7},
		{"ratio below one rounds down", 0.5, 3, 1},
		{"negative tokens", 3, -7, 0},
		{"very large ratio saturates", 1e300, 1, math.MaxInt},
		{"very small ratio", 1e-300, 1, 0},
		{"widest count saturates", 1, math.MaxInt, math.MaxInt},
		{"NaN ratio saturates", math.NaN(), 1, math.MaxInt},
		{"negative ratio is zero", -1, 5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Policy{BytesPerToken: tc.ratio}.Bytes(tc.tokens))
		})
	}
}

func TestPolicyRoundTrip(t *testing.T) {
	// the estimate never shrinks a size: the bytes of the tokens estimated for n are at least n
	for _, ratio := range []float64{1, 2, 2.5, 3, 4, 8} {
		p := Policy{BytesPerToken: ratio}
		for _, n := range []int{0, 1, 2, 3, 7, 100, 4096, 123_457} {
			assert.GreaterOrEqual(t, p.Bytes(p.Tokens(n)), n, "ratio %g, bytes %d", ratio, n)
		}
	}
}

func TestNewBudget(t *testing.T) {
	policy := func(ratio float64, requestTokens int) Policy {
		return Policy{BytesPerToken: ratio, RequestTokens: requestTokens}
	}
	q := Questions{Route: 3000, ChunkRoute: 6000, Relevance: 300, Complexity: 1500, Evidence: 900}

	t.Run("each state gets the allowance less its own questions", func(t *testing.T) {
		b, err := NewBudget(policy(3, 30_000), q)
		require.NoError(t, err)
		assert.Equal(t, Budget{Policy: policy(3, 30_000), State: 29_000, Chunk: 27_900, Doc: 29_200}, b)
	})
	t.Run("a larger allowance grows every state", func(t *testing.T) {
		b, err := NewBudget(policy(3, 130_000), q)
		require.NoError(t, err)
		assert.Equal(t, Budget{Policy: policy(3, 130_000), State: 129_000, Chunk: 127_900, Doc: 129_200}, b)
	})
	t.Run("a smaller ratio estimates the questions larger", func(t *testing.T) {
		b, err := NewBudget(policy(1.5, 30_000), q)
		require.NoError(t, err)
		assert.Equal(t, Budget{Policy: policy(1.5, 30_000), State: 28_000, Chunk: 25_800, Doc: 28_400}, b)
	})
	t.Run("a fractional ratio rounds each question up", func(t *testing.T) {
		b, err := NewBudget(policy(2.5, 30_000), Questions{Route: 3001, ChunkRoute: 6001, Relevance: 301, Complexity: 1501, Evidence: 901})
		require.NoError(t, err)
		assert.Equal(t, 30_000-1201, b.State)
		assert.Equal(t, 30_000-2401-121, b.Chunk)
		assert.Equal(t, 30_000-601-361, b.Doc)
	})
	t.Run("single state over budget", func(t *testing.T) {
		_, err := NewBudget(policy(3, 30_000), Questions{Route: 84_003})
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		assert.Contains(t, err.Error(), "request_tokens 30000 less the routing_policy question leaves 1999 tokens for the state, need 2000")
		_, err = NewBudget(policy(3, 30_000), Questions{Route: 84_000})
		require.NoError(t, err)
	})
	t.Run("chunk state over budget beside a maximal anchor", func(t *testing.T) {
		_, err := NewBudget(policy(3, 30_000), Questions{ChunkRoute: 36_000, Relevance: 36_003})
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		assert.Contains(t, err.Error(), "request_tokens 30000 less the routing_policy chunk questions leaves 1999 tokens beside the anchor")
		_, err = NewBudget(policy(3, 30_000), Questions{ChunkRoute: 36_000, Relevance: 36_000})
		require.NoError(t, err)
	})
	t.Run("both chunk questions count, not only the longer", func(t *testing.T) {
		b, err := NewBudget(policy(3, 30_000), Questions{ChunkRoute: 9000, Relevance: 9000})
		require.NoError(t, err)
		assert.Equal(t, 30_000-3000-3000, b.Chunk)
	})
	t.Run("doc state over budget", func(t *testing.T) {
		_, err := NewBudget(policy(3, 30_000), Questions{Complexity: 42_000, Evidence: 42_003})
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		assert.Contains(t, err.Error(), "request_tokens 30000 less the complexity_policy questions leaves 1999 tokens for the doc state")
		_, err = NewBudget(policy(3, 30_000), Questions{Complexity: 42_000, Evidence: 42_000})
		require.NoError(t, err)
	})
	t.Run("every state over budget is reported", func(t *testing.T) {
		_, err := NewBudget(policy(3, 2500), q)
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		for _, want := range []string{"for the state", "beside the anchor", "for the doc state"} {
			assert.Contains(t, err.Error(), want)
		}
	})
	t.Run("an allowance below the questions is negative, not wrapped", func(t *testing.T) {
		b, err := NewBudget(policy(3, 100), Questions{Route: 3000, ChunkRoute: 3000, Relevance: 3000, Complexity: 3000, Evidence: 3000})
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		assert.Equal(t, 100-1000, b.State)
		assert.Equal(t, 100-2000, b.Chunk)
		assert.Equal(t, 100-2000, b.Doc)
	})
	t.Run("an allowance of zero leaves nothing", func(t *testing.T) {
		_, err := NewBudget(policy(3, 0), q)
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
	})
}

func TestNewBudgetExtremeRatios(t *testing.T) {
	q := Questions{Route: 3000, ChunkRoute: 6000, Relevance: 300, Complexity: 1500, Evidence: 900}

	t.Run("very small ratio saturates the estimates without wrapping", func(t *testing.T) {
		for _, ratio := range []float64{1e-300, 5e-324} {
			b, err := NewBudget(Policy{BytesPerToken: ratio, RequestTokens: 30_000}, q)
			require.ErrorIs(t, err, ErrQuestionsOverBudget, "ratio %g", ratio)
			assert.Negative(t, b.State, "ratio %g", ratio)
			assert.Negative(t, b.Chunk, "ratio %g", ratio)
			assert.Negative(t, b.Doc, "ratio %g", ratio)
		}
	})
	t.Run("widest questions at ratio one never wrap around", func(t *testing.T) {
		widest := Questions{Route: math.MaxInt, ChunkRoute: math.MaxInt, Relevance: math.MaxInt, Complexity: math.MaxInt, Evidence: math.MaxInt}
		b, err := NewBudget(Policy{BytesPerToken: 1, RequestTokens: 30_000}, widest)
		require.ErrorIs(t, err, ErrQuestionsOverBudget)
		assert.Negative(t, b.State)
		assert.Negative(t, b.Chunk)
		assert.Negative(t, b.Doc)
	})
	t.Run("very large ratio estimates every question at one token", func(t *testing.T) {
		b, err := NewBudget(Policy{BytesPerToken: 1e300, RequestTokens: 30_000}, q)
		require.NoError(t, err)
		assert.Equal(t, Budget{BytesPerToken: 1e300, RequestTokens: 30_000,
			State: 29_999, Chunk: 29_998, Doc: 29_998}, b)
	})
	t.Run("a very large ratio does not overflow the room", func(t *testing.T) {
		b := Budget{BytesPerToken: 1e300, RequestTokens: 30_000, State: 29_999, Chunk: 29_998, Doc: 29_998}
		assert.Positive(t, chunkRoom(b, 12_000))
		assert.Positive(t, docRoom(b.Policy, b.Doc))
	})
}


func TestChunkRoom(t *testing.T) {
	maxIndex := strconv.Itoa(math.MaxInt)
	assert.Len(t, maxIndex, indexDigits)
	envelope := len(`{"anchor":,"chunk":{"field":"prompt","index":` + maxIndex + `,"of":` + maxIndex + `,"text":""}}`)
	for _, ratio := range []float64{2, 2.5, 3, 4} {
		b := Budget{BytesPerToken: ratio, RequestTokens: 30_000, State: 29_000, Chunk: 28_000}
		assert.Equal(t, int(28_000*ratio)-12_000-envelope, chunkRoom(b, 12_000), "ratio %g", ratio)
	}
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

func TestSplitFollowsRatio(t *testing.T) {
	r := &Result{Prompt: lines("p", 3000, 80), Files: []string{lines("f", 3000, 80)}}
	for _, ratio := range []float64{1.5, 2, 2.5, 3, 4, 8} {
		t.Run(strconv.FormatFloat(ratio, 'g', -1, 64), func(t *testing.T) {
			b := Budget{BytesPerToken: ratio, RequestTokens: 30_000, State: 5_000, Chunk: 20_000}
			s, err := r.Split(b)
			require.NoError(t, err)
			checkSplit(t, r, b, s)
		})
	}
}

func TestFitsFollowsRatio(t *testing.T) {
	r := &Result{Prompt: strings.Repeat("x", 60_000)}
	// 60 KB is 30k tokens at 2 bytes per token and 7.5k at 8
	assert.False(t, r.Fits(Budget{BytesPerToken: 2, RequestTokens: 30_000, State: 10_000}))
	assert.True(t, r.Fits(Budget{BytesPerToken: 8, RequestTokens: 30_000, State: 10_000}))
	assert.Greater(t, r.StateTokens(Policy{BytesPerToken: 2}), r.StateTokens(Policy{BytesPerToken: 8}))
}

func TestAnchorFollowsRatio(t *testing.T) {
	r := &Result{Prompt: lines("p", 4000, 80)}
	var last int
	for _, ratio := range []float64{2, 3, 4} {
		b := Budget{BytesPerToken: ratio, RequestTokens: 30_000, State: 1_000, Chunk: AnchorTokens + 10_000}
		s, err := r.Split(b)
		require.NoError(t, err)
		checkSplit(t, r, b, s)
		size := mustLen(s.Anchor)
		assert.LessOrEqual(t, size, int(AnchorTokens*ratio), "ratio %g: the anchor limit is AnchorTokens × ratio bytes", ratio)
		assert.Greater(t, size, last, "a larger ratio gives a larger anchor in bytes")
		last = size
	}
}

func TestSplitWithProject(t *testing.T) {
	b := Budget{Policy: testPolicy, State: 1_000, Chunk: 4_000 + MinStateTokens}
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
	assert.LessOrEqual(t, b.Tokens(mustLen(s.Anchor)), AnchorTokens)
	assert.LessOrEqual(t, b.Tokens(mustLen(s.Anchor.Attachments)), attachmentShare)

	prompt, files := "", []string(nil)
	if r.Files != nil {
		files = make([]string, len(r.Files))
	}
	promptChunked := false
	for i, c := range s.Chunks {
		assert.Equal(t, i+1, c.Index)
		assert.Equal(t, len(s.Chunks), c.Of)
		assert.True(t, utf8.ValidString(c.Text), "chunk %d cut inside a character", c.Index)
		assert.LessOrEqual(t, b.Tokens(mustLen(ChunkState{Anchor: s.Anchor, Chunk: c})), b.Chunk, "chunk %d over budget", c.Index)
		widest := c
		widest.Index, widest.Of = math.MaxInt, math.MaxInt
		assert.LessOrEqual(t, b.Tokens(mustLen(ChunkState{Anchor: s.Anchor, Chunk: widest})), b.Chunk,
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
	b := Budget{Policy: testPolicy, State: 29_000, Chunk: 28_000}

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
		require.False(t, r.Fits(Budget{Policy: testPolicy, State: 3000}))
		s, err := r.Split(Budget{Policy: testPolicy, State: 3000, Chunk: b.Chunk})
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
		s, err := r.Split(Budget{Policy: testPolicy, State: 1000, Chunk: b.Chunk})
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Field: FieldPrompt, Index: 1, Of: 1}}, s.Chunks)
	})

	t.Run("summary still over 1k", func(t *testing.T) {
		atts := make([]Attachment, 0, 200)
		for i := range 200 {
			atts = append(atts, Attachment{Source: SourceMentioned, Type: fmt.Sprintf("application/x-kind-%d", i), Bytes: 1})
		}
		r := &Result{Prompt: "p", Attachments: atts}
		_, err := r.Split(Budget{Policy: testPolicy, State: 1000, Chunk: b.Chunk})
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
		_, err := r.Split(Budget{Policy: testPolicy, State: 1000, Chunk: 4000})
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
