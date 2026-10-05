package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

// levels is a complexity answer with the given probabilities and 0 on every other level.
func levels(probs map[int]float64) jev.Answer {
	a := jev.Answer{Type: jev.TypeChoice, Probabilities: map[string]float64{}}
	best := 0
	for i := range complexityLevels {
		a.Probabilities[strconv.Itoa(i)] = probs[i]
		if probs[i] > probs[best] {
			best = i
		}
	}
	a.Choice = strconv.Itoa(best)
	return a
}

func docAnswers(level int, evidence float64) map[string]jev.Answer {
	return map[string]jev.Answer{
		questionComplexity: levels(map[int]float64{level: 1}),
		questionEvidence:   {Type: jev.TypeNoul, Noul: evidence},
	}
}

// docMock answers every doc request with fn, given the doc chunk (zero for the whole-docs state).
func docMock(fn func(c prompt.DocChunk, whole bool) (map[string]jev.Answer, error)) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		switch st := req.State.(type) {
		case prompt.DocsState:
			return fn(prompt.DocChunk{}, true)
		case prompt.DocChunkState:
			return fn(st.Doc, false)
		}
		return nil, fmt.Errorf("unexpected state %T", req.State)
	}}
}

func docChunkStates(client *mocks.JevClientMock) []prompt.DocChunk {
	var out []prompt.DocChunk
	for _, c := range client.AskCalls() {
		if st, ok := c.Req.State.(prompt.DocChunkState); ok {
			out = append(out, st.Doc)
		}
	}
	return out
}

// docsOf is a doc of about n state tokens, marked at its start so a mock can tell the docs apart.
func docsOf(mark string, tokens int) string {
	return mark + "\n" + filler(tokens*3) // 3 bytes per token
}

func unprocessable() error { return &jev.StatusError{Status: 422} }

func TestAskDocsExpectedValue(t *testing.T) {
	cfg, cat := embedded(t)
	tests := []struct {
		name  string
		probs map[int]float64
		want  float64
	}{
		{name: "certain", probs: map[int]float64{7: 1}, want: 7},
		{name: "split between two levels", probs: map[int]float64{3: 0.5, 7: 0.5}, want: 5},
		{name: "not the argmax", probs: map[int]float64{0: 0.4, 10: 0.3, 9: 0.3}, want: 5.7},
		{name: "all on zero", probs: map[int]float64{0: 1}, want: 0},
		{name: "sum above 1 within tolerance stays in range", probs: map[int]float64{10: 1, 9: 0.009},
			want: (10 + 9*0.009) / 1.009},
		{name: "sum below 1 within tolerance", probs: map[int]float64{4: 0.5, 6: 0.495}, want: (2 + 6*0.495) / 0.995},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := docMock(func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				return map[string]jev.Answer{questionComplexity: levels(tc.probs),
					questionEvidence: {Type: jev.TypeNoul, Noul: 0.8}}, nil
			})
			got, err := newRouter(t, cfg, cat, client).complexity(context.Background(),
				&prompt.Result{Docs: []string{"a small Go CLI"}})
			require.NoError(t, err)
			require.Len(t, got.Chunks, 1)
			assert.InDelta(t, tc.want, got.Chunks[0].Score, 1e-9)
			assert.InDelta(t, 0.8, got.Chunks[0].Evidence, 1e-9)
		})
	}
}

func TestReduce(t *testing.T) {
	tests := []struct {
		name   string
		scores []DocScore
		want   float64
	}{
		{name: "one score", scores: []DocScore{{Score: 6.94, Evidence: 0.3}}, want: 6.9},
		{name: "a 9 with high evidence beats two 2s with low evidence",
			scores: []DocScore{{Score: 2, Evidence: 0.1}, {Score: 9, Evidence: 0.9}, {Score: 2, Evidence: 0.1}},
			want:   7.7}, // (0.2 + 8.1 + 0.2) / 1.1
		{name: "zero evidence has no effect",
			scores: []DocScore{{Score: 8, Evidence: 0.5}, {Score: 1, Evidence: 0}, {Score: 1, Evidence: 0}},
			want:   8},
		{name: "all-zero evidence gives a plain mean",
			scores: []DocScore{{Score: 2}, {Score: 9}, {Score: 2}},
			want:   4.3},
		{name: "rounds half up to one decimal", scores: []DocScore{{Score: 4.25, Evidence: 1}}, want: 4.3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := reduce(tc.scores)
			assert.InDelta(t, tc.want, got.Complexity, 1e-9)
			assert.Equal(t, tc.scores, got.Chunks)
		})
	}
}

func TestComplexitySingleRequest(t *testing.T) {
	cfg, cat := embedded(t)
	client := docMock(func(_ prompt.DocChunk, whole bool) (map[string]jev.Answer, error) {
		if !whole {
			return nil, errors.New("docs that fit sent in chunks")
		}
		return docAnswers(6, 0.9), nil
	})
	r := newRouter(t, cfg, cat, client)
	docs := &prompt.Result{Prompt: "fix the typo", Docs: []string{"# Shop\n\nThree services.\n", "", "Use gofmt.\n"}}
	require.True(t, docs.DocsFit(r.budget))

	got, err := r.complexity(context.Background(), docs)
	require.NoError(t, err)
	assert.Equal(t, &ComplexityResult{Chunks: []DocScore{{Index: 1, Of: 1, Score: 6, Evidence: 0.9}}, Complexity: 6}, got)

	calls := client.AskCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, prompt.DocsState{Docs: []string{"# Shop\n\nThree services.\n", "Use gofmt.\n"}}, calls[0].Req.State)
	assert.Equal(t, complexityQuestions(cfg.Agrouter), calls[0].Req.Questions)
	assert.Equal(t, cfg.Agrouter.JevModel, calls[0].Req.Model)
}

func TestComplexityChunked(t *testing.T) {
	cfg, cat := embedded(t)
	r := newRouter(t, cfg, cat, &mocks.JevClientMock{})
	b := r.budget
	docs := &prompt.Result{Docs: []string{docsOf("ENTERPRISE", 2*b.Doc), docsOf("STYLE", 3*b.Doc)}}
	chunks := docs.SplitDocs(b)
	require.Greater(t, len(chunks), 4)

	var mu sync.Mutex
	inFlight, peak := 0, 0
	all := make(chan struct{})
	client := docMock(func(c prompt.DocChunk, whole bool) (map[string]jev.Answer, error) {
		if whole {
			return nil, errors.New("docs over budget sent whole")
		}
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		if inFlight == len(chunks) {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("not every one of %d doc chunks in flight", len(chunks))
		}
		if c.Doc == 0 {
			return docAnswers(9, 0.9), nil
		}
		return docAnswers(2, 0.1), nil
	})
	r = newRouter(t, cfg, cat, client)

	got, err := r.complexity(context.Background(), docs)
	require.NoError(t, err)
	assert.Equal(t, len(chunks), peak, "every doc chunk in flight at once")
	require.Len(t, got.Chunks, len(chunks))
	var sum, total float64
	for i, ch := range got.Chunks {
		assert.Equal(t, i+1, ch.Index)
		assert.Equal(t, len(chunks), ch.Of)
		sum += ch.Score * ch.Evidence
		total += ch.Evidence
	}
	assert.InDelta(t, math.Round(sum/total*10)/10, got.Complexity, 1e-9)
	assert.Greater(t, got.Complexity, 5.5, "the evidence-weighted 9s outweigh the 2s")
	for _, c := range client.AskCalls() {
		assert.Equal(t, complexityQuestions(cfg.Agrouter), c.Req.Questions, "same questions in every doc request")
	}
}

func TestComplexityFailures(t *testing.T) {
	cfg, cat := embedded(t)
	b := newRouter(t, cfg, cat, &mocks.JevClientMock{}).budget
	small := &prompt.Result{Docs: []string{"a small CLI"}}
	big := &prompt.Result{Docs: []string{docsOf("START", 3*b.Doc)}}
	tail := &prompt.Result{Docs: []string{docsOf("A", b.Doc), "short tail doc"}}

	tests := []struct {
		name     string
		captured *prompt.Result
		ask      func(c prompt.DocChunk, whole bool) (map[string]jev.Answer, error)
		want     error
	}{
		{name: "missing complexity answer", captured: small, want: jev.ErrMalformed,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				a := docAnswers(3, 1)
				delete(a, questionComplexity)
				return a, nil
			}},
		{name: "missing evidence answer", captured: big, want: jev.ErrMalformed,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				a := docAnswers(3, 1)
				delete(a, questionEvidence)
				return a, nil
			}},
		{name: "probability missing for a level", captured: small, want: jev.ErrMalformed,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				a := docAnswers(3, 1)
				delete(a[questionComplexity].Probabilities, "10")
				return a, nil
			}},
		{name: "probabilities all zero", captured: small, want: jev.ErrMalformed,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				return map[string]jev.Answer{questionComplexity: levels(nil),
					questionEvidence: {Type: jev.TypeNoul, Noul: 1}}, nil
			}},
		{name: "whole-docs request failing", captured: small, want: jev.ErrOverloaded,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				return nil, &jev.StatusError{Status: 529}
			}},
		{name: "one doc chunk failing", captured: big, want: jev.ErrOverloaded,
			ask: func(c prompt.DocChunk, _ bool) (map[string]jev.Answer, error) {
				if c.Index == 2 {
					return nil, &jev.StatusError{Status: 529}
				}
				return docAnswers(3, 1), nil
			}},
		{name: "whole-docs 422 under the minimum state size", captured: small, want: errUnsplittable,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) { return nil, unprocessable() }},
		{name: "422 on a doc chunk under the minimum state size", captured: tail, want: errUnsplittable,
			ask: func(c prompt.DocChunk, _ bool) (map[string]jev.Answer, error) {
				if c.Doc == 1 {
					return nil, unprocessable()
				}
				return docAnswers(3, 1), nil
			}},
		{name: "second 422 on a halved doc chunk", captured: big, want: errUnsplittable,
			ask: func(c prompt.DocChunk, _ bool) (map[string]jev.Answer, error) {
				if strings.HasPrefix(c.Text, "START") {
					return nil, unprocessable()
				}
				return docAnswers(3, 1), nil
			}},
		{name: "422 on a chunk after the whole-docs retry", want: errUnsplittable,
			captured: &prompt.Result{Docs: []string{docsOf("START", b.Doc/2)}},
			ask: func(c prompt.DocChunk, whole bool) (map[string]jev.Answer, error) {
				if whole || strings.HasPrefix(c.Text, "START") {
					return nil, unprocessable()
				}
				return docAnswers(3, 1), nil
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newRouter(t, cfg, cat, docMock(tc.ask)).complexity(context.Background(), tc.captured)
			require.ErrorIs(t, err, tc.want)
			assert.Nil(t, got)
		})
	}
}

func TestComplexityWholeDocs422SplitsAtHalfBudget(t *testing.T) {
	cfg, cat := embedded(t)
	client := docMock(func(_ prompt.DocChunk, whole bool) (map[string]jev.Answer, error) {
		if whole {
			return nil, unprocessable()
		}
		return docAnswers(4, 0.5), nil
	})
	r := newRouter(t, cfg, cat, client)
	b := r.budget
	docs := []string{docsOf("ARCH", b.Doc*2/3), "Use gofmt.\n", docsOf("DEPS", b.Doc/4)}
	captured := &prompt.Result{Docs: docs}
	require.True(t, captured.DocsFit(b), "%d > %d", captured.DocsTokens(), b.Doc)

	got, err := r.complexity(context.Background(), captured)
	require.NoError(t, err)
	assert.InDelta(t, 4, got.Complexity, 1e-9)

	calls := client.AskCalls()
	_, whole := calls[0].Req.State.(prompt.DocsState)
	assert.True(t, whole, "the whole docs first")
	states := docChunkStates(client)
	require.Len(t, states, len(calls)-1)
	require.Greater(t, len(states), len(docs))
	assert.Len(t, got.Chunks, len(states))
	for _, c := range states {
		assert.LessOrEqual(t, docStateTokens(t, c), b.Doc/2, "split at half the doc budget")
	}
	assert.Equal(t, docs, joinDocs(states, len(docs)), "every doc byte kept")
}

func TestComplexityWholeDocs422HalvesOneSmallDoc(t *testing.T) {
	cfg, cat := embedded(t)
	client := docMock(func(_ prompt.DocChunk, whole bool) (map[string]jev.Answer, error) {
		if whole {
			return nil, unprocessable()
		}
		return docAnswers(4, 0.5), nil
	})
	r := newRouter(t, cfg, cat, client)
	b := r.budget
	// one doc under half the doc budget: cutting at half the budget would resend it whole
	doc := docsOf("ONE", b.Doc/4)
	captured := &prompt.Result{Docs: []string{doc}}
	require.GreaterOrEqual(t, captured.DocsTokens(), prompt.MinStateTokens)

	_, err := r.complexity(context.Background(), captured)
	require.NoError(t, err)

	states := docChunkStates(client)
	require.Greater(t, len(states), 1, "the rejected doc is cut in two or more")
	for _, c := range states {
		assert.LessOrEqual(t, docStateTokens(t, c), (captured.DocsTokens()+1)/2, "each piece about half the docs")
	}
	assert.Equal(t, []string{doc}, joinDocs(states, 1), "every doc byte kept")
}

func TestComplexityDocChunk422ResplitsOnce(t *testing.T) {
	cfg, cat := embedded(t)
	b := newRouter(t, cfg, cat, &mocks.JevClientMock{}).budget
	docs := []string{docsOf("FIRST", 2*b.Doc), docsOf("SECOND", 2*b.Doc)}
	captured := &prompt.Result{Docs: docs}
	chunks := captured.SplitDocs(b)
	n := len(chunks)

	var mu sync.Mutex
	asked := map[prompt.DocChunk]int{} // by doc and text: the docs' filler repeats
	client := docMock(func(c prompt.DocChunk, _ bool) (map[string]jev.Answer, error) {
		mu.Lock()
		asked[prompt.DocChunk{Doc: c.Doc, Text: c.Text}]++
		mu.Unlock()
		if c.Index == 1 && c.Of == n { // the first chunk, before the re-split
			return nil, unprocessable()
		}
		return docAnswers(5, 1), nil
	})
	r := newRouter(t, cfg, cat, client)

	seq, err := r.docFanout(complexityQuestions(cfg.Agrouter)).run(context.Background(), chunks, false)
	require.NoError(t, err)
	pieces := prompt.HalveDoc(chunks[0])
	require.Greater(t, len(pieces), 1)
	assert.Len(t, client.AskCalls(), n+len(pieces))
	require.Len(t, seq, n-1+len(pieces))
	final := make([]prompt.DocChunk, len(seq))
	for i, p := range seq {
		final[i] = p.chunk
		assert.Equal(t, i+1, p.chunk.Index, "renumbered in sequence order")
		assert.Equal(t, len(seq), p.chunk.Of)
		assert.LessOrEqual(t, docStateTokens(t, p.chunk), b.Doc, "renumbered doc chunk within the doc budget")
	}
	for _, c := range chunks[1:] {
		assert.Equal(t, 1, asked[prompt.DocChunk{Doc: c.Doc, Text: c.Text}], "a completed doc chunk is not asked again")
	}
	assert.Equal(t, docs, joinDocs(final, len(docs)), "every doc byte kept across the re-split")
	for _, c := range docChunkStates(client) {
		assert.LessOrEqual(t, docStateTokens(t, c), b.Doc, "doc chunk %d of %d over budget", c.Index, c.Of)
	}

	// through the stage: the pieces take the rejected chunk's place
	got, err := r.complexity(context.Background(), captured)
	require.NoError(t, err)
	require.Len(t, got.Chunks, n-1+len(pieces))
	assert.InDelta(t, 5, got.Complexity, 1e-9)
}

func docStateTokens(t *testing.T, c prompt.DocChunk) int {
	t.Helper()
	data, err := json.Marshal(prompt.DocChunkState{Doc: c})
	require.NoError(t, err)
	return prompt.Tokens(len(data))
}

// joinDocs rebuilds n docs from doc chunks in sequence order; empty docs stay empty.
func joinDocs(chunks []prompt.DocChunk, n int) []string {
	chunks = slices.Clone(chunks)
	slices.SortStableFunc(chunks, func(a, b prompt.DocChunk) int { return a.Index - b.Index })
	out := make([]string, n)
	for _, c := range chunks {
		out[c.Doc] += c.Text
	}
	return out
}

func TestComplexityStopsOnDeadline(t *testing.T) {
	synctest.Test(t, testComplexityStopsOnDeadline)
}

func testComplexityStopsOnDeadline(t *testing.T) {
	cfg, cat := embedded(t)
	var mu sync.Mutex
	started := 0
	client := &mocks.JevClientMock{AskFunc: func(ctx context.Context, _ jev.Request) (map[string]jev.Answer, error) {
		mu.Lock()
		started++
		mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := newRouter(t, cfg, cat, client)
	captured := &prompt.Result{Docs: []string{docsOf("BIG", 4*r.budget.Doc)}}
	chunks := captured.SplitDocs(r.budget)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	got, err := r.complexity(ctx, captured)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, got)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, len(chunks), started, "every doc chunk starts at once")
}

func TestComplexitySingleRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, cat := embedded(t)
		client := &mocks.JevClientMock{AskFunc: func(ctx context.Context, _ jev.Request) (map[string]jev.Answer, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := newRouter(t, cfg, cat, client).complexity(ctx, &prompt.Result{Docs: []string{"a CLI"}})
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
