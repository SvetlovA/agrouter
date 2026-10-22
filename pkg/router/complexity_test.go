package router

import (
	"context"
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
	a := jev.Answer{Type: jev.TypeScore, Confidence: 0.8, Probabilities: map[string]float64{}}
	var total float64
	for i := range complexityLevels {
		a.Probabilities[strconv.Itoa(i)] = probs[i]
		a.Score += float64(i) * probs[i]
		total += probs[i]
	}
	if total > 0 {
		a.Score /= total
	}
	return a
}

func docAnswers(level int, evidence float64) map[string]jev.Answer {
	return map[string]jev.Answer{
		questionComplexity: complexityAnswer(float64(level)),
		questionEvidence:   {Type: jev.TypeNoul, Noul: evidence},
	}
}

// complexityAnswer expresses a 0-to-10 test rating on the Score question's 0-to-9 scale.
func complexityAnswer(rating float64) jev.Answer {
	raw := rating * float64(len(complexityLevels)-1) / 10
	low := int(math.Floor(raw))
	fraction := raw - float64(low)
	return levels(map[int]float64{low: 1 - fraction, low + 1: fraction})
}

// docMock answers every doc request with fn, given the doc chunk (zero for the whole-docs state).
func docMock(fn func(c prompt.DocChunk, whole bool) (map[string]jev.Answer, error)) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: answersOnly(func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		switch st := req.State.(type) {
		case prompt.DocsState:
			return fn(prompt.DocChunk{}, true)
		case prompt.DocChunkState:
			return fn(st.Doc, false)
		}
		return nil, fmt.Errorf("unexpected state %T", req.State)
	})}
}

// docsOf is a doc of about n state tokens, marked at its start so a mock can tell the docs apart.
func docsOf(mark string, tokens int) string {
	return mark + "\n" + filler(tokens*3) // 3 bytes per token
}

func TestAskDocsExpectedValue(t *testing.T) {
	cfg, cat := embedded(t)
	tests := []struct {
		name  string
		probs map[int]float64
		want  float64
	}{
		{name: "certain", probs: map[int]float64{7: 1}, want: 7 * 10.0 / 9},
		{name: "split between two levels", probs: map[int]float64{3: 0.5, 7: 0.5}, want: 5 * 10.0 / 9},
		{name: "not the argmax", probs: map[int]float64{0: 0.4, 9: 0.3, 8: 0.3}, want: 5.1 * 10 / 9},
		{name: "all on zero", probs: map[int]float64{0: 1}, want: 0},
		{name: "maximum normalizes to 10", probs: map[int]float64{9: 1}, want: 10},
		{name: "sum above 1 within tolerance stays in range", probs: map[int]float64{9: 1, 8: 0.009},
			want: (9 + 8*0.009) / 1.009 * 10 / 9},
		{name: "sum below 1 within tolerance", probs: map[int]float64{4: 0.5, 6: 0.495}, want: (2 + 6*0.495) / 0.995 * 10 / 9},
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
			assert.InDelta(t, 0.8, got.Chunks[0].Confidence, 1e-9)
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

func TestReduceConfidenceRecordedOnly(t *testing.T) {
	results := make([]float64, 0, 2)
	for _, confidence := range []float64{0, 1} {
		scores := []DocScore{{Index: 1, Of: 2, Score: 9, Evidence: 0.9, Confidence: confidence},
			{Index: 2, Of: 2, Score: 1, Evidence: 0.1, Confidence: 1 - confidence}}
		got := reduce(scores)
		assert.Equal(t, scores, got.Chunks, "confidence is retained exactly as supplied")
		results = append(results, got.Complexity)
	}
	assert.Equal(t, []float64{8.2, 8.2}, results, "only evidence weights complexity")
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
	calls := client.AskCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, &ComplexityResult{Chunks: []DocScore{{Index: 1, Of: 1, Score: 6, Evidence: 0.9, Confidence: 0.8,
		Estimated: r.estimate(calls[0].Req.State, calls[0].Req.Questions)}}, Complexity: 6}, got,
		"the provider reported no tokens: unknown, never zero")
	assert.Positive(t, got.Chunks[0].Estimated)

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

	tests := []struct {
		name     string
		captured *prompt.Result
		ask      func(c prompt.DocChunk, whole bool) (map[string]jev.Answer, error)
		want     error
	}{
		{name: "missing complexity answer", captured: small, want: jev.ErrEmptyAnswer,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				a := docAnswers(3, 1)
				delete(a, questionComplexity)
				return a, nil
			}},
		{name: "missing evidence answer", captured: big, want: jev.ErrEmptyAnswer,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				a := docAnswers(3, 1)
				delete(a, questionEvidence)
				return a, nil
			}},
		{name: "probability missing for a level", captured: small, want: jev.ErrInvalidAnswer,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) {
				a := docAnswers(3, 1)
				delete(a[questionComplexity].Probabilities, "9")
				return a, nil
			}},
		{name: "probabilities all zero", captured: small, want: jev.ErrInvalidAnswer,
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
		{name: "whole-docs overflow", captured: small, want: jev.ErrTooLong,
			ask: func(prompt.DocChunk, bool) (map[string]jev.Answer, error) { return nil, tooLong() }},
		{name: "overflow on a doc chunk", captured: big, want: jev.ErrTooLong,
			ask: func(c prompt.DocChunk, _ bool) (map[string]jev.Answer, error) {
				if strings.HasPrefix(c.Text, "START") {
					return nil, tooLong()
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

func TestComplexityRejectedRequestsAreNotResent(t *testing.T) {
	cfg, cat := embedded(t)
	b := newRouter(t, cfg, cat, &mocks.JevClientMock{}).budget
	small := &prompt.Result{Docs: []string{"a small CLI"}}
	big := &prompt.Result{Docs: []string{docsOf("START", 3*b.Doc)}}
	n := len(big.SplitDocs(b))
	require.Greater(t, n, 2)

	for _, rj := range rejections {
		t.Run(rj.name+"/whole docs", func(t *testing.T) {
			client := docMock(func(prompt.DocChunk, bool) (map[string]jev.Answer, error) { return nil, rj.err })
			got, err := newRouter(t, cfg, cat, client).complexity(context.Background(), small)
			assert.Nil(t, got)
			requireRejected(t, err, rj)
			assert.Len(t, client.AskCalls(), 1, "the rejected request is not sent again")
		})
		t.Run(rj.name+"/doc chunk", func(t *testing.T) {
			g := newGate(n)
			client := docMock(func(c prompt.DocChunk, whole bool) (map[string]jev.Answer, error) {
				if whole {
					return nil, errors.New("docs over budget sent whole")
				}
				if !g.wait() {
					return nil, fmt.Errorf("not every one of %d doc chunks in flight", n)
				}
				if c.Index == 1 {
					return nil, rj.err
				}
				return docAnswers(3, 1), nil
			})
			got, err := newRouter(t, cfg, cat, client).complexity(context.Background(), big)
			assert.Nil(t, got)
			requireRejected(t, err, rj)
			assert.Contains(t, err.Error(), fmt.Sprintf("doc chunk 1 of %d", n))
			// the doc chunks, each once at its original size
			var indexes []int
			for _, call := range client.AskCalls() {
				st, ok := call.Req.State.(prompt.DocChunkState)
				require.True(t, ok)
				assert.Equal(t, n, st.Doc.Of, "not re-split")
				indexes = append(indexes, st.Doc.Index)
			}
			slices.Sort(indexes)
			assert.Len(t, indexes, n)
			assert.Equal(t, 1, indexes[0])
			assert.Equal(t, n, indexes[n-1])
			assert.Len(t, slices.Compact(indexes), n, "no chunk asked twice")
		})
	}
}

// requireRejected asserts err is the rejection rj: a StatusError with its status, matching
// jev.ErrTooLong only for the overflow.
func requireRejected(t *testing.T, err error, rj rejection) {
	t.Helper()
	var serr *jev.StatusError
	require.ErrorAs(t, err, &serr)
	assert.Equal(t, rj.status, serr.Status)
	assert.Equal(t, rj.tooLong, errors.Is(err, jev.ErrTooLong))
}

func TestComplexityStopsOnDeadline(t *testing.T) {
	synctest.Test(t, testComplexityStopsOnDeadline)
}

func testComplexityStopsOnDeadline(t *testing.T) {
	cfg, cat := embedded(t)
	var mu sync.Mutex
	started := 0
	client := &mocks.JevClientMock{AskFunc: answersOnly(func(ctx context.Context, _ jev.Request) (map[string]jev.Answer, error) {
		mu.Lock()
		started++
		mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	})}
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
		client := &mocks.JevClientMock{AskFunc: answersOnly(func(ctx context.Context, _ jev.Request) (map[string]jev.Answer, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := newRouter(t, cfg, cat, client).complexity(ctx, &prompt.Result{Docs: []string{"a CLI"}})
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
