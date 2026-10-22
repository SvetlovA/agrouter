package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

const (
	optHard = "claude-opus-5-5@high"
	optEasy = "claude-haiku-5-5@low"
)

// filler is n bytes of plain lines, the long material a task works on.
func filler(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "log line %06d: nothing to see here\n", i)
	}
	return b.String()[:n]
}

// favoring is a stage answer putting 0.9 on id and the rest evenly over the other names.
func favoring(names []string, id string, confidence float64) jev.Answer {
	probs := make(map[string]float64, len(names))
	for _, n := range names {
		probs[n] = 0.1 / float64(len(names)-1)
	}
	probs[id] = 0.9
	return jev.Answer{Type: jev.TypeChoice, Choice: id, Probabilities: probs, Confidence: confidence}
}

// chunkAnswers are a chunk request's answers to a model stage question and relevance.
func chunkAnswers(route jev.Answer, relevance float64) map[string]jev.Answer {
	return map[string]jev.Answer{LevelModel: route, questionRelevance: {Type: jev.TypeNoul, Noul: relevance}}
}

// chunked is a mock answering chunk requests with fn; a single request fails.
func chunked(t *testing.T, fn func(jev.Request, prompt.ChunkState) (map[string]jev.Answer, error)) *mocks.JevClientMock {
	t.Helper()
	return &mocks.JevClientMock{AskFunc: answersOnly(func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		st, ok := req.State.(prompt.ChunkState)
		if !ok {
			return nil, fmt.Errorf("unexpected state %T", req.State)
		}
		return fn(req, st)
	})}
}

// byMarker answers the chunks containing "HARD" as relevant and favoring the strong option, and the
// rest as filler favoring the cheap one, at every stage.
func byMarker(req jev.Request, st prompt.ChunkState) (map[string]jev.Answer, error) {
	if strings.Contains(st.Chunk.Text, "HARD") {
		return stageAnswers(req, optHard, 0.4, 0.95), nil
	}
	return stageAnswers(req, optEasy, 0.99, 0.01), nil
}

// stageCalls are the requests asked at level, in call order.
func stageCalls(client *mocks.JevClientMock, level string) []jev.Request {
	var out []jev.Request
	for _, c := range client.AskCalls() {
		if id, _, _ := stageAsked(c.Req); id == level {
			out = append(out, c.Req)
		}
	}
	return out
}

func slots(answers ...map[string]jev.Answer) []*slot {
	out := make([]*slot, len(answers))
	for i, a := range answers {
		out[i] = &slot{answer: routeAnswer{probs: a[LevelModel].Probabilities,
			result: ChunkResult{Relevance: a[questionRelevance].Noul}}}
	}
	return out
}

func repeatAnswers(n int, a map[string]jev.Answer) []map[string]jev.Answer {
	out := make([]map[string]jev.Answer, n)
	for i := range out {
		out[i] = a
	}
	return out
}

func TestPoolRelevanceWeights(t *testing.T) {
	_, cat := embedded(t)
	opts := ids(Eligible(cat, &args.Request{CLI: "claude"}).Options)
	hard := chunkAnswers(favoring(opts, optHard, 0.5), 0.95)
	fill := chunkAnswers(favoring(opts, optEasy, 0.99), 0.01)
	zero := chunkAnswers(favoring(opts, optEasy, 0.99), 0)
	other := 0.1 / float64(len(opts)-1)
	withHard := func(n int, a map[string]jev.Answer) []*slot {
		return slots(append([]map[string]jev.Answer{hard}, repeatAnswers(n, a)...)...)
	}

	t.Run("short hard requirement outweighs long filler", func(t *testing.T) {
		i, top, _ := pool(opts, withHard(5, fill))
		o := opts[i]
		assert.Equal(t, optHard, o)
		assert.Equal(t, optHard, top[0].ID)
		// raw weights: 0.95 and 5 Ã— 0.01
		assert.InDelta(t, (0.95*0.9+0.05*other)/1.0, top[0].Score, 1e-9)
	})
	t.Run("zero-relevance filler of any length has no effect", func(t *testing.T) {
		for _, n := range []int{1, 1000, 100_000} {
			i, top, _ := pool(opts, withHard(n, zero))
			o := opts[i]
			assert.Equal(t, optHard, o, "%d filler chunks", n)
			assert.InDelta(t, 0.9, top[0].Score, 1e-9, "%d filler chunks", n)
		}
	})
	t.Run("enough low-relevance filler dilutes it (documented limit)", func(t *testing.T) {
		// hard wins while 0.95 > n Ã— 0.01: a raw weighted mean has no cap
		i, _, _ := pool(opts, withHard(94, fill))
		o := opts[i]
		assert.Equal(t, optHard, o, "94 Ã— 0.01 = 0.94 < 0.95")
		i, _, _ = pool(opts, withHard(96, fill))
		o = opts[i]
		assert.Equal(t, optEasy, o, "96 Ã— 0.01 = 0.96 > 0.95")
	})
	t.Run("all-zero relevance gives a plain mean and catalog order breaks ties", func(t *testing.T) {
		a := chunkAnswers(favoring(opts, optEasy, 0.9), 0)
		b := chunkAnswers(favoring(opts, optHard, 0.1), 0)
		i, top, _ := pool(opts, slots(a, b))
		o := opts[i]
		require.Len(t, top, topOptions)
		assert.InDelta(t, top[0].Score, top[1].Score, 1e-12, "equal weights, mirrored answers")
		first := optHard // earlier in the catalog than haiku
		if slices.Index(opts, optEasy) < slices.Index(opts, optHard) {
			first = optEasy
		}
		assert.Equal(t, first, o)
		assert.Equal(t, first, top[0].ID)
	})
}

func TestRankedStableTies(t *testing.T) {
	top := ranked([]string{"a", "b", "c", "d"}, []float64{0.2, 0.3, 0.3, 0.2})
	assert.Equal(t, []Score{{"b", 0.3}, {"c", 0.3}, {"a", 0.2}}, top)
}

func TestPoolRetainsEveryNameInOrder(t *testing.T) {
	names := []string{"a", "b", "c", "d"}
	first := chunkAnswers(jev.Answer{Probabilities: map[string]float64{"a": 0.1, "b": 0.2, "c": 0.3, "d": 0.4}}, 0.25)
	second := chunkAnswers(jev.Answer{Probabilities: map[string]float64{"a": 0.4, "b": 0.3, "c": 0.2, "d": 0.1}}, 0.75)
	chosen, top, all := pool(names, slots(first, second))
	assert.Equal(t, "a", names[chosen])
	assert.Len(t, top, 3)
	require.Len(t, all, 4)
	for i, want := range []float64{0.325, 0.275, 0.225, 0.175} {
		assert.Equal(t, names[i], all[i].ID)
		assert.InDelta(t, want, all[i].Score, 1e-9)
	}
}

func TestPoolNamesOtherThanOptionIDs(t *testing.T) {
	// a later stage's criteria: effort labels or CLIs, not option IDs
	probs := func(p map[string]float64, relevance float64) map[string]jev.Answer {
		return chunkAnswers(jev.Answer{Probabilities: p}, relevance)
	}
	tests := []struct {
		name    string
		names   []string
		answers []map[string]jev.Answer
		want    string
		top     []Score
	}{
		{
			name:  "weighted by relevance",
			names: []string{"low", "medium", "high"},
			answers: []map[string]jev.Answer{
				probs(map[string]float64{"low": 0.1, "medium": 0.2, "high": 0.7}, 0.9),
				probs(map[string]float64{"low": 0.8, "medium": 0.1, "high": 0.1}, 0.1),
			},
			want: "high",
			top:  []Score{{"high", 0.64}, {"medium", 0.19}, {"low", 0.17}},
		},
		{
			name:    "ties keep the order of names",
			names:   []string{"beta", "alpha"},
			answers: []map[string]jev.Answer{probs(map[string]float64{"alpha": 0.5, "beta": 0.5}, 0)},
			want:    "beta",
			top:     []Score{{"beta", 0.5}, {"alpha", 0.5}},
		},
		{
			name:    "probabilities for unlisted names are ignored",
			names:   []string{"alpha", "beta"},
			answers: []map[string]jev.Answer{probs(map[string]float64{"alpha": 0.2, "beta": 0.3, "alpha@high": 0.5}, 1)},
			want:    "beta",
			top:     []Score{{"beta", 0.3}, {"alpha", 0.2}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chosen, top, all := pool(tc.names, slots(tc.answers...))
			assert.Equal(t, tc.want, tc.names[chosen])
			require.Len(t, top, len(tc.top))
			for i, want := range tc.top {
				assert.Equal(t, want.ID, top[i].ID)
				assert.InDelta(t, want.Score, top[i].Score, 1e-9)
			}
			require.Len(t, all, len(tc.names))
			for i, name := range tc.names {
				assert.Equal(t, name, all[i].ID, "every score in the order of names")
			}
		})
	}
}

func TestRouteChunked(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cat, req)
	c := captured("HARD: keep the public API stable while fixing the parser\n" + filler(600_000))
	split, err := c.Split(newRouter(t, cfg, cat, &mocks.JevClientMock{}).budget)
	require.NoError(t, err)
	require.Greater(t, len(split.Chunks), 4)

	// every request waits until all chunks are in flight: none waits for another to finish
	var inFlight, peak atomic.Int32
	all := make(chan struct{})
	client := chunked(t, func(req jev.Request, st prompt.ChunkState) (map[string]jev.Answer, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p; p = peak.Load() {
			if peak.CompareAndSwap(p, n) {
				break
			}
		}
		if int(n) == len(split.Chunks) {
			close(all)
		}
		select {
		case <-all:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("only %d of %d chunks in flight", inFlight.Load(), len(split.Chunks))
		}
		return byMarker(req, st)
	})
	r := newRouter(t, cfg, cat, client)

	d, err := r.Route(context.Background(), el, req, c)
	require.NoError(t, err)
	assert.Equal(t, optHard, d.OptionID)
	assert.Equal(t, "claude-opus-5-5", d.Model)
	assert.Equal(t, "high", d.Effort)
	require.Len(t, d.Stages, 3)
	assert.True(t, d.Stages[0].Skipped, "--cli fixes the cli stage")
	for i, want := range []string{"claude-opus-5-5", "high"} {
		st := d.Stages[i+1]
		assert.Nil(t, st.Answer, "a pooled decision is not a Jev choice")
		require.NotNil(t, st.Pooled, st.Level)
		assert.Equal(t, want, st.Choice)
		assert.Equal(t, want, st.Pooled.Top[0].ID)
		require.Len(t, st.Pooled.Chunks, len(split.Chunks))
		for i, ch := range st.Pooled.Chunks {
			assert.Equal(t, prompt.FieldPrompt, ch.Field)
			assert.Equal(t, i+1, ch.Index)
			assert.Equal(t, len(split.Chunks), ch.Of)
			assert.LessOrEqual(t, len(ch.Top), topOptions)
		}
		assert.InDelta(t, 0.95, st.Pooled.Chunks[0].Relevance, 1e-9)
	}
	assert.Equal(t, len(split.Chunks), int(peak.Load()), "every chunk in flight at once")

	require.Len(t, client.AskCalls(), 2*len(split.Chunks))
	for _, level := range []string{LevelModel, LevelEffort} {
		calls := stageCalls(client, level)
		require.Len(t, calls, len(split.Chunks))
		first := calls[0]
		for _, call := range calls {
			assert.Equal(t, first.Questions, call.Questions, "same questions in every chunk")
			st, ok := call.State.(prompt.ChunkState)
			require.True(t, ok)
			assert.Equal(t, split.Anchor, st.Anchor, "anchor repeated")
		}
		assert.Equal(t, jev.TypeNoul, first.Questions[questionRelevance].Type)
		assert.Equal(t, relevanceText, first.Questions[questionRelevance].Instructions)
		in, ok := first.Questions[level].Instructions.(stageInstructions)
		require.True(t, ok)
		assert.Equal(t, chunkGuide, in.State, "a chunk request describes the chunk state")
		assert.Equal(t, cfg.Agrouter.RoutingPolicy, in.Policy)
	}
	model := stageCalls(client, LevelModel)[0].Questions[LevelModel]
	assert.Equal(t, modelText, model.Instructions.(stageInstructions).Question)
	assert.Equal(t, groupLabels(groups(el.Options, routeLevels[1])), model.Criteria.Names())
}

func TestRouteChunkedConfidenceNotAveraged(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cat, req)
	c := captured("HARD: migrate the schema\n" + filler(200_000))
	tops := make([][]Score, 0, 2)
	for _, conf := range []float64{0.01, 0.99} {
		client := chunked(t, func(req jev.Request, st prompt.ChunkState) (map[string]jev.Answer, error) {
			if strings.Contains(st.Chunk.Text, "HARD") {
				return stageAnswers(req, optHard, 1-conf, 0.5), nil
			}
			return stageAnswers(req, optEasy, conf, 0.5), nil
		})
		d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, c)
		require.NoError(t, err)
		require.Len(t, d.Stages, 3)
		p := d.Stages[1].Pooled
		require.NotNil(t, p)
		require.NotEmpty(t, p.Chunks)
		assert.InDelta(t, 1-conf, p.Chunks[0].Confidence, 1e-9)
		for _, chunk := range p.Chunks[1:] {
			assert.InDelta(t, conf, chunk.Confidence, 1e-9)
		}
		tops = append(tops, p.Top)
	}
	assert.Equal(t, tops[0], tops[1])
}

func TestRouteChunkGoldenRequest(t *testing.T) {
	cfg, cat := requestFixture(t)
	req := &args.Request{CLI: "alpha"}
	el := Eligible(cat, req)
	client := chunked(t, func(req jev.Request, _ prompt.ChunkState) (map[string]jev.Answer, error) {
		return stageAnswers(req, "fast", 0.9, 0.5), nil
	})
	r := newRouter(t, cfg, cat, client)
	// force a small state into two chunks; 12 of the tokens go to the widest index and count
	r.budget = prompt.Budget{State: 1, Chunk: 132}
	c := &prompt.Result{Prompt: "fix the flaky test in pkg/foo/foo_test.go",
		Files:       []string{"package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tt.Skip(\"flaky\")\n}\n"},
		Attachments: []prompt.Attachment{{Source: "mentioned", Type: "image/png", Bytes: 48213}}}

	d, err := r.Route(context.Background(), el, req, c)
	require.NoError(t, err)
	assert.Equal(t, "fast", d.OptionID)
	calls := client.AskCalls()
	require.Len(t, calls, 2, "two chunks of the model stage; fast has no efforts")
	first := calls[0].Req // requests run in parallel: pick the first chunk's
	if st, _ := first.State.(prompt.ChunkState); st.Chunk.Index != 1 {
		first = calls[1].Req
	}

	got, err := json.MarshalIndent(first, "", "  ")
	require.NoError(t, err)
	golden := filepath.Join("testdata", "request_chunk.json")
	if *update {
		require.NoError(t, os.WriteFile(golden, append(got, '\n'), 0o600))
	}
	want, err := os.ReadFile(golden) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got)+"\n")
}

func TestRouteChunkFailuresCannotDecide(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cat, req)
	big := captured("START HARD: port the scheduler\n" + filler(200_000))
	answer := byMarker
	tests := []struct {
		name     string
		captured *prompt.Result
		ask      func(jev.Request) (map[string]jev.Answer, error)
		want     error
		status   int // a plain status error of this status, not an overflow
	}{
		{name: "overflow on a chunk", captured: big, want: jev.ErrTooLong,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				if strings.HasPrefix(st.Chunk.Text, "START") {
					return nil, tooLong()
				}
				return answer(req, st)
			}},
		{name: "another 400 on a chunk", captured: big, status: http.StatusBadRequest,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				if st.Chunk.Index == st.Chunk.Of {
					return nil, &jev.StatusError{Status: http.StatusBadRequest, ErrorType: "invalid_request"}
				}
				return answer(req, st)
			}},
		{name: "422 on the last chunk", captured: big, status: http.StatusUnprocessableEntity,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				if st.Chunk.Index == st.Chunk.Of {
					return nil, unprocessable()
				}
				return answer(req, st)
			}},
		{name: "one chunk failing after retries", captured: big, want: jev.ErrOverloaded,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				if st.Chunk.Index == 2 {
					return nil, &jev.StatusError{Status: 529}
				}
				return answer(req, st)
			}},
		{name: "missing relevance answer", captured: big, want: jev.ErrEmptyAnswer,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				a, err := answer(req, st)
				delete(a, questionRelevance)
				return a, err
			}},
		{name: "missing route answer", captured: big, want: jev.ErrEmptyAnswer,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				a, err := answer(req, st)
				id, _, _ := stageAsked(req)
				delete(a, id)
				return a, err
			}},
		{name: "probability missing for an option", captured: big, want: jev.ErrInvalidAnswer,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				a, err := answer(req, st)
				id, _, easy := stageChoice(req, optEasy)
				route := a[id]
				probs := map[string]float64{}
				for k, v := range route.Probabilities {
					if k != easy {
						probs[k] = v
					}
				}
				route.Probabilities = probs
				a[id] = route
				return a, err
			}},
		{name: "single request overflow", captured: captured("HARD: tiny"), want: jev.ErrTooLong,
			ask: func(jev.Request) (map[string]jev.Answer, error) { return nil, tooLong() }},
		{name: "single request 422", captured: captured("HARD: tiny"), status: http.StatusUnprocessableEntity,
			ask: func(jev.Request) (map[string]jev.Answer, error) { return nil, unprocessable() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &mocks.JevClientMock{AskFunc: answersOnly(func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
				return tc.ask(req)
			})}
			r := newRouter(t, cfg, cat, client)
			d, err := r.Route(context.Background(), el, req, tc.captured)
			require.NoError(t, err)
			if tc.want != nil {
				require.ErrorIs(t, d.Undecided, tc.want)
			} else {
				var serr *jev.StatusError
				require.ErrorAs(t, d.Undecided, &serr)
				assert.Equal(t, tc.status, serr.Status)
				require.NotErrorIs(t, d.Undecided, jev.ErrTooLong)
			}
			se, ok := errors.AsType[*StageError](d.Undecided)
			require.True(t, ok, "a chunked failure carries its stage")
			assert.Equal(t, LevelModel, se.Stage)
			assert.Equal(t, Decision{CLI: "claude", Pinned: true, Undecided: d.Undecided,
				Stages: []Stage{{Level: LevelCLI, Choice: "claude", Skipped: true}}}, d, "the model stage failed")
		})
	}
}

func TestRouteChunkedCannotDecideCLIUnknown(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{}
	el := Eligible(cat, req)
	client := chunked(t, func(jev.Request, prompt.ChunkState) (map[string]jev.Answer, error) {
		return nil, context.DeadlineExceeded
	})
	_, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, captured(filler(200_000)))
	require.ErrorIs(t, err, ErrCannotDecide)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	se, ok := errors.AsType[*StageError](err)
	require.True(t, ok)
	assert.Equal(t, LevelCLI, se.Stage)
}

func TestRouteChunkedStopsOnDeadline(t *testing.T) {
	synctest.Test(t, testRouteChunkedStopsOnDeadline)
}

func testRouteChunkedStopsOnDeadline(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cat, req)
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
	c := captured(filler(1_000_000))
	split, err := c.Split(r.budget)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	d, err := r.Route(ctx, el, req, c)
	require.NoError(t, err)
	require.ErrorIs(t, d.Undecided, context.DeadlineExceeded)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, len(split.Chunks), started, "every chunk starts at once")
}

func TestRouteChunkedAnsweredAtDeadlineStands(t *testing.T) {
	synctest.Test(t, testRouteChunkedAnsweredAtDeadlineStands)
}

func testRouteChunkedAnsweredAtDeadlineStands(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cat, req)
	client := &mocks.JevClientMock{AskFunc: answersOnly(func(ctx context.Context, req jev.Request) (map[string]jev.Answer, error) {
		<-ctx.Done() // every chunk answers just as the deadline passes
		return stageAnswers(req, "claude-sonnet-5-5@low", 0.9, 0.5), nil
	})}
	r := newRouter(t, cfg, cat, client)
	c := captured(filler(150_000))
	split, err := c.Split(r.budget)
	require.NoError(t, err)
	require.NotNil(t, split)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	d, err := r.Route(ctx, el, req, c)
	require.NoError(t, err)
	require.NoError(t, d.Undecided)
	assert.Equal(t, "claude-sonnet-5-5@low", d.OptionID)
	assert.Len(t, client.AskCalls(), 2*len(split.Chunks), "every chunk of both levels asked")
}
