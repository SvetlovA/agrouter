package router

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

const (
	optHard = "claude-opus-5-5@high"
	optEasy = "claude-haiku-4-5"
)

// filler is n bytes of plain lines, the long material a task works on.
func filler(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "log line %06d: nothing to see here\n", i)
	}
	return b.String()[:n]
}

// favoring is a route answer putting 0.9 on id and the rest evenly over the other options.
func favoring(opts []catalog.Option, id string, confidence float64) jev.Answer {
	probs := make(map[string]float64, len(opts))
	for _, o := range opts {
		probs[o.ID] = 0.1 / float64(len(opts)-1)
	}
	probs[id] = 0.9
	return jev.Answer{Type: jev.TypeChoice, Choice: id, Probabilities: probs, Confidence: confidence}
}

func chunkAnswers(route jev.Answer, relevance float64) map[string]jev.Answer {
	return map[string]jev.Answer{questionRoute: route, questionRelevance: {Type: jev.TypeNoul, Noul: relevance}}
}

// chunked is a mock answering chunk requests with fn; a single request panics the test.
func chunked(t *testing.T, fn func(prompt.ChunkState) (map[string]jev.Answer, error)) *mocks.JevClientMock {
	t.Helper()
	return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		st, ok := req.State.(prompt.ChunkState)
		if !ok {
			return nil, fmt.Errorf("unexpected state %T", req.State)
		}
		return fn(st)
	}}
}

// byMarker answers the chunks containing "HARD" as relevant and favoring the strong option, and the
// rest as filler favoring the cheap one.
func byMarker(opts []catalog.Option) func(prompt.ChunkState) (map[string]jev.Answer, error) {
	return func(st prompt.ChunkState) (map[string]jev.Answer, error) {
		if strings.Contains(st.Chunk.Text, "HARD") {
			return chunkAnswers(favoring(opts, optHard, 0.4), 0.95), nil
		}
		return chunkAnswers(favoring(opts, optEasy, 0.99), 0.01), nil
	}
}

func chunkStates(client *mocks.JevClientMock) []prompt.ChunkState {
	var out []prompt.ChunkState
	for _, c := range client.AskCalls() {
		if st, ok := c.Req.State.(prompt.ChunkState); ok {
			out = append(out, st)
		}
	}
	return out
}

func slots(answers ...map[string]jev.Answer) []*slot {
	out := make([]*slot, len(answers))
	for i, a := range answers {
		out[i] = &slot{done: true, answer: routeAnswer{probs: a[questionRoute].Probabilities,
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
	cfg, cat := embedded(t)
	opts := Eligible(cfg, cat, &args.Request{CLI: "claude"}).Options
	hard := chunkAnswers(favoring(opts, optHard, 0.5), 0.95)
	fill := chunkAnswers(favoring(opts, optEasy, 0.99), 0.01)
	zero := chunkAnswers(favoring(opts, optEasy, 0.99), 0)
	other := 0.1 / float64(len(opts)-1)
	withHard := func(n int, a map[string]jev.Answer) []*slot {
		return slots(append([]map[string]jev.Answer{hard}, repeatAnswers(n, a)...)...)
	}

	t.Run("short hard requirement outweighs long filler", func(t *testing.T) {
		o, top, _ := pool(optionIDs(opts), withHard(5, fill))
		assert.Equal(t, optHard, o)
		assert.Equal(t, optHard, top[0].ID)
		// raw weights: 0.95 and 5 Ã— 0.01
		assert.InDelta(t, (0.95*0.9+0.05*other)/1.0, top[0].Score, 1e-9)
	})
	t.Run("zero-relevance filler of any length has no effect", func(t *testing.T) {
		for _, n := range []int{1, 1000, 100_000} {
			o, top, _ := pool(optionIDs(opts), withHard(n, zero))
			assert.Equal(t, optHard, o, "%d filler chunks", n)
			assert.InDelta(t, 0.9, top[0].Score, 1e-9, "%d filler chunks", n)
		}
	})
	t.Run("enough low-relevance filler dilutes it (documented limit)", func(t *testing.T) {
		// hard wins while 0.95 > n Ã— 0.01: a raw weighted mean has no cap
		o, _, _ := pool(optionIDs(opts), withHard(94, fill))
		assert.Equal(t, optHard, o, "94 Ã— 0.01 = 0.94 < 0.95")
		o, _, _ = pool(optionIDs(opts), withHard(96, fill))
		assert.Equal(t, optEasy, o, "96 Ã— 0.01 = 0.96 > 0.95")
	})
	t.Run("all-zero relevance gives a plain mean and catalog order breaks ties", func(t *testing.T) {
		a := chunkAnswers(favoring(opts, optEasy, 0.9), 0)
		b := chunkAnswers(favoring(opts, optHard, 0.1), 0)
		o, top, _ := pool(optionIDs(opts), slots(a, b))
		require.Len(t, top, topOptions)
		assert.InDelta(t, top[0].Score, top[1].Score, 1e-12, "equal weights, mirrored answers")
		first := optHard // earlier in the catalog than haiku
		if catalogIndex(opts, optEasy) < catalogIndex(opts, optHard) {
			first = optEasy
		}
		assert.Equal(t, first, o)
		assert.Equal(t, first, top[0].ID)
	})
}

func catalogIndex(opts []catalog.Option, id string) int {
	for i, o := range opts {
		if o.ID == id {
			return i
		}
	}
	return -1
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
	assert.Equal(t, "a", chosen)
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
			assert.Equal(t, tc.want, chosen)
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
	el := Eligible(cfg, cat, req)
	c := captured("HARD: keep the public API stable while fixing the parser\n" + filler(600_000))
	split, err := c.Split(newRouter(t, cfg, cat, &mocks.JevClientMock{}).budget)
	require.NoError(t, err)
	require.Greater(t, len(split.Chunks), 4)

	// every request waits until all chunks are in flight: none waits for another to finish
	var inFlight, peak atomic.Int32
	all := make(chan struct{})
	answer := byMarker(el.Options)
	client := chunked(t, func(st prompt.ChunkState) (map[string]jev.Answer, error) {
		n := inFlight.Add(1)
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
		return answer(st)
	})
	r := newRouter(t, cfg, cat, client)

	d, err := r.Route(context.Background(), el, req, c)
	require.NoError(t, err)
	assert.Equal(t, optHard, d.OptionID)
	assert.Equal(t, "claude-opus-5-5", d.Model)
	assert.Equal(t, "high", d.Effort)
	assert.Nil(t, d.Answer, "a pooled decision is not a Jev choice")
	require.NotNil(t, d.Pooled)
	assert.Equal(t, optHard, d.Pooled.Top[0].ID)
	require.Len(t, d.Pooled.Chunks, len(split.Chunks))
	for i, ch := range d.Pooled.Chunks {
		assert.Equal(t, prompt.FieldPrompt, ch.Field)
		assert.Equal(t, i+1, ch.Index)
		assert.Equal(t, len(split.Chunks), ch.Of)
		assert.LessOrEqual(t, len(ch.Top), topOptions)
	}
	assert.InDelta(t, 0.95, d.Pooled.Chunks[0].Relevance, 1e-9)
	assert.Equal(t, len(split.Chunks), int(peak.Load()), "every chunk in flight at once")

	calls := client.AskCalls()
	require.Len(t, calls, len(split.Chunks))
	first := calls[0].Req
	for _, call := range calls {
		assert.Equal(t, first.Questions, call.Req.Questions, "same questions in every chunk")
		st, ok := call.Req.State.(prompt.ChunkState)
		require.True(t, ok)
		assert.Equal(t, split.Anchor, st.Anchor, "anchor repeated")
	}
	assert.Equal(t, jev.TypeNoul, first.Questions[questionRelevance].Type)
	assert.Equal(t, relevanceText, first.Questions[questionRelevance].Instructions)
	in, ok := first.Questions[questionRoute].Instructions.(routeInstructions)
	require.True(t, ok)
	assert.Equal(t, routeText, in.Question)
	assert.Equal(t, chunkGuide, in.State, "a chunk request describes the chunk state")
	assert.Equal(t, cfg.Agrouter.RoutingPolicy, in.Policy)
	assert.Equal(t, ids(el.Options), first.Questions[questionRoute].Criteria.Names())
}

func TestRouteChunkedConfidenceNotAveraged(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cfg, cat, req)
	c := captured("HARD: migrate the schema\n" + filler(200_000))
	tops := make([][]Score, 0, 2)
	for _, conf := range []float64{0.01, 0.99} {
		client := chunked(t, func(st prompt.ChunkState) (map[string]jev.Answer, error) {
			a := favoring(el.Options, optEasy, conf)
			if strings.Contains(st.Chunk.Text, "HARD") {
				a = favoring(el.Options, optHard, 1-conf)
			}
			return chunkAnswers(a, 0.5), nil
		})
		d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, c)
		require.NoError(t, err)
		require.NotNil(t, d.Pooled)
		require.NotEmpty(t, d.Pooled.Chunks)
		assert.InDelta(t, 1-conf, d.Pooled.Chunks[0].Confidence, 1e-9)
		for _, chunk := range d.Pooled.Chunks[1:] {
			assert.InDelta(t, conf, chunk.Confidence, 1e-9)
		}
		tops = append(tops, d.Pooled.Top)
	}
	assert.Equal(t, tops[0], tops[1])
}

func TestRouteChunkGoldenRequest(t *testing.T) {
	cfg, cat := requestFixture(t)
	req := &args.Request{CLI: "alpha"}
	el := Eligible(cfg, cat, req)
	client := chunked(t, func(prompt.ChunkState) (map[string]jev.Answer, error) {
		return chunkAnswers(favoring(el.Options, "fast", 0.9), 0.5), nil
	})
	r := newRouter(t, cfg, cat, client)
	// force a small state into two chunks; 12 of the tokens go to the widest index and count
	r.budget = prompt.Budget{State: 1, Chunk: 132}
	c := &prompt.Result{Prompt: "fix the flaky test in pkg/foo/foo_test.go",
		Files:       []string{"package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tt.Skip(\"flaky\")\n}\n"},
		Attachments: []prompt.Attachment{{Source: "mentioned", Type: "image/png", Bytes: 48213}}}

	_, err := r.Route(context.Background(), el, req, c)
	require.NoError(t, err)
	calls := client.AskCalls()
	require.Len(t, calls, 2)
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

// rejecting answers chunk requests like byMarker, but rejects with a 422 those reject picks.
func rejecting(t *testing.T, opts []catalog.Option, reject func(prompt.ChunkState) bool) *mocks.JevClientMock {
	t.Helper()
	answer := byMarker(opts)
	return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		st, ok := req.State.(prompt.ChunkState)
		if !ok {
			return nil, &jev.StatusError{Status: 422} // the single request
		}
		if reject(st) {
			return nil, &jev.StatusError{Status: 422}
		}
		return answer(st)
	}}
}

func TestRouteSingleRequest422Resplits(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cfg, cat, req)
	client := rejecting(t, el.Options, func(prompt.ChunkState) bool { return false })
	r := newRouter(t, cfg, cat, client)
	c := captured("HARD: rename the package\n" + filler(40_000))
	require.True(t, c.Fits(r.budget))

	d, err := r.Route(context.Background(), el, req, c)
	require.NoError(t, err)
	require.NoError(t, d.Undecided)
	assert.Equal(t, optHard, d.OptionID)
	require.NotNil(t, d.Pooled)

	calls := client.AskCalls()
	_, single := calls[0].Req.State.(prompt.State)
	assert.True(t, single, "the whole state first")
	states := chunkStates(client)
	require.Len(t, states, len(calls)-1)
	require.Greater(t, len(states), 1)
	for _, st := range states {
		size, err := json.Marshal(st)
		require.NoError(t, err)
		assert.LessOrEqual(t, prompt.Tokens(len(size)), r.budget.Chunk/2, "split at half the budget")
	}
}

func TestRouteChunk422ResplitsOnce(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cfg, cat, req)
	c := captured("HARD: port the scheduler\n" + filler(200_000))
	r := newRouter(t, cfg, cat, &mocks.JevClientMock{})
	split, err := c.Split(r.budget)
	require.NoError(t, err)
	n := len(split.Chunks)

	client := rejecting(t, el.Options, func(st prompt.ChunkState) bool {
		return st.Chunk.Index == 1 && st.Chunk.Of == n // the first chunk, before the re-split
	})
	r = newRouter(t, cfg, cat, client)
	d, err := r.Route(context.Background(), el, req, c)
	require.NoError(t, err)
	require.NoError(t, d.Undecided)
	require.NotNil(t, d.Pooled)
	assert.Equal(t, optHard, d.OptionID)

	pieces := prompt.Halve(split.Chunks[0])
	require.Greater(t, len(pieces), 1)
	assert.Len(t, client.AskCalls(), n+len(pieces))
	require.Len(t, d.Pooled.Chunks, n-1+len(pieces))
	for i, ch := range d.Pooled.Chunks[:len(pieces)] {
		assert.Equal(t, i+1, ch.Index, "the pieces take the rejected chunk's place")
		assert.Equal(t, n-1+len(pieces), ch.Of)
	}
	for _, st := range chunkStates(client) {
		size, err := json.Marshal(st)
		require.NoError(t, err)
		assert.LessOrEqual(t, prompt.Tokens(len(size)), r.budget.Chunk, "chunk %d of %d over budget", st.Chunk.Index, st.Chunk.Of)
	}
}

func TestRouteChunkFailuresCannotDecide(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cfg, cat, req)
	big := captured("START HARD: port the scheduler\n" + filler(200_000))
	tail := captured(filler(145_000))
	tail.Files = []string{"short tail file\n"} // a separate short chunk regardless of question size

	r := newRouter(t, cfg, cat, &mocks.JevClientMock{})
	tailSplit, err := tail.Split(r.budget)
	require.NoError(t, err)
	last := tailSplit.Chunks[len(tailSplit.Chunks)-1]
	require.Less(t, prompt.Tokens(len(last.Text)), prompt.MinStateTokens)

	answer := byMarker(el.Options)
	tests := []struct {
		name     string
		captured *prompt.Result
		ask      func(jev.Request) (map[string]jev.Answer, error)
		want     error
	}{
		{name: "second 422 on a re-split piece", captured: big, want: errUnsplittable,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				if strings.HasPrefix(st.Chunk.Text, "START") {
					return nil, &jev.StatusError{Status: 422}
				}
				return answer(st)
			}},
		{name: "422 on a chunk under 2k tokens", captured: tail, want: errUnsplittable,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				if st.Chunk.Index == st.Chunk.Of {
					return nil, &jev.StatusError{Status: 422}
				}
				return answer(st)
			}},
		{name: "one chunk failing after retries", captured: big, want: jev.ErrOverloaded,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				if st.Chunk.Index == 2 {
					return nil, &jev.StatusError{Status: 529}
				}
				return answer(st)
			}},
		{name: "missing relevance answer", captured: big, want: jev.ErrMalformed,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				a, err := answer(st)
				delete(a, questionRelevance)
				return a, err
			}},
		{name: "missing route answer", captured: big, want: jev.ErrMalformed,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				a, err := answer(st)
				delete(a, questionRoute)
				return a, err
			}},
		{name: "probability missing for an option", captured: big, want: jev.ErrMalformed,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, _ := req.State.(prompt.ChunkState)
				a, err := answer(st)
				route := a[questionRoute]
				probs := map[string]float64{}
				for k, v := range route.Probabilities {
					if k != optEasy {
						probs[k] = v
					}
				}
				route.Probabilities = probs
				a[questionRoute] = route
				return a, err
			}},
		{name: "single request 422 on a state under 2k tokens", captured: captured("HARD: tiny"), want: errUnsplittable,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				return nil, &jev.StatusError{Status: 422}
			}},
		{name: "single request 422 then a chunk 422", captured: captured("START HARD\n" + filler(40_000)),
			want: errUnsplittable,
			ask: func(req jev.Request) (map[string]jev.Answer, error) {
				st, ok := req.State.(prompt.ChunkState)
				if !ok || strings.HasPrefix(st.Chunk.Text, "START") {
					return nil, &jev.StatusError{Status: 422}
				}
				return answer(st)
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
				return tc.ask(req)
			}}
			r := newRouter(t, cfg, cat, client)
			d, err := r.Route(context.Background(), el, req, tc.captured)
			require.NoError(t, err)
			require.ErrorIs(t, d.Undecided, tc.want)
			assert.Equal(t, Decision{CLI: "claude", Pinned: true, Undecided: d.Undecided}, d)
		})
	}
}

func TestRouteChunkedCannotDecideCLIUnknown(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{}
	el := Eligible(cfg, cat, req)
	client := chunked(t, func(prompt.ChunkState) (map[string]jev.Answer, error) {
		return nil, context.DeadlineExceeded
	})
	_, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, captured(filler(200_000)))
	require.ErrorIs(t, err, ErrCannotDecide)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRouteChunkedStopsOnDeadline(t *testing.T) {
	synctest.Test(t, testRouteChunkedStopsOnDeadline)
}

func testRouteChunkedStopsOnDeadline(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cfg, cat, req)
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
	el := Eligible(cfg, cat, req)
	client := &mocks.JevClientMock{AskFunc: func(ctx context.Context, _ jev.Request) (map[string]jev.Answer, error) {
		<-ctx.Done() // every chunk answers just as the deadline passes
		return chunkAnswers(favoring(el.Options, "claude-sonnet-5-5@low", 0.9), 0.5), nil
	}}
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
	assert.Len(t, client.AskCalls(), len(split.Chunks), "all chunks were asked before the deadline")
}
