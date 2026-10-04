package router

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

// topOptions is how many options debug output lists per chunk and for the pool.
const topOptions = 3

// Score is an option's probability in one chunk, or its pooled score.
type Score struct {
	ID    string
	Score float64
}

// ChunkResult is one chunk request's answers, for debug output.
type ChunkResult struct {
	Field     string
	Index     int // as sent; a re-split renumbers the chunks after it
	Of        int
	Relevance float64 // the Noul as answered: the chunk's weight in the pool
	Top       []Score
}

// Pooled is how a split state was decided: every chunk's answers in sequence order and the pooled
// scores. It has no confidence: a pooled decision is agrouter's, not a Jev choice.
type Pooled struct {
	Chunks []ChunkResult
	Top    []Score
}

// routeAnswer is one chunk request's validated answers.
type routeAnswer struct {
	probs  map[string]float64
	result ChunkResult
}

// slot is one chunk of a split state and, once asked, its answers.
type slot = piece[prompt.Chunk, routeAnswer]

// single sends the whole state in one Choice request. A 422 on it re-splits the whole state at half
// the chunk budget, once, and pools the chunks.
func (r *Router) single(ctx context.Context, el *Eligibility, captured *prompt.Result) (outcome, error) {
	state := captured.State()
	o, answer, err := r.ask(ctx, el, state)
	if err == nil {
		return outcome{option: o, answer: answer}, nil
	}
	if !errors.Is(err, jev.ErrUnprocessable) {
		return outcome{}, err
	}
	if captured.StateTokens() < prompt.MinStateTokens {
		return outcome{}, fmt.Errorf("%w: %w", errUnsplittable, err)
	}
	// State 0 forces the split even though the state fit the full budget
	split, splitErr := captured.Split(prompt.Budget{Chunk: r.budget.Chunk / 2})
	if splitErr != nil {
		return outcome{}, fmt.Errorf("re-split after %w: %w", err, splitErr)
	}
	return r.pooled(ctx, el, split, true)
}

// pooled asks one request per chunk, all at once, and pools the answers. A 422 on a
// chunk re-splits it once at half size; any other failure, a second 422, or a 422 on a chunk under
// the minimum state size means Jev cannot decide.
func (r *Router) pooled(ctx context.Context, el *Eligibility, split *prompt.Split, resplit bool) (outcome, error) {
	ag := r.cfg.Agrouter
	questions := map[string]jev.Question{
		questionRoute:     routeQuestion(r.cfg, ag.ChunkQuestion, el.Options, el.effort, r.enc),
		questionRelevance: relevanceQuestion(ag.Relevance),
	}
	f := fanout[prompt.Chunk, routeAnswer]{
		name: "chunk",
		ask: func(ctx context.Context, c prompt.Chunk) (routeAnswer, error) {
			return r.askChunk(ctx, el, split.Anchor, questions, c)
		},
		halve: prompt.Halve,
		text:  func(c prompt.Chunk) string { return c.Text },
		pos:   func(c prompt.Chunk) (int, int) { return c.Index, c.Of },
		number: func(c prompt.Chunk, index, of int) prompt.Chunk {
			c.Index, c.Of = index, of
			return c
		},
	}
	seq, err := f.run(ctx, split.Chunks, resplit)
	if err != nil {
		return outcome{}, err
	}

	o, top := pool(el.Options, seq)
	p := &Pooled{Top: top, Chunks: make([]ChunkResult, len(seq))}
	for i, s := range seq {
		p.Chunks[i] = s.answer.result
	}
	return outcome{option: o, pooled: p}, nil
}

// askChunk sends one chunk request and returns its validated answers.
func (r *Router) askChunk(ctx context.Context, el *Eligibility, anchor prompt.Anchor,
	questions map[string]jev.Question, c prompt.Chunk) (routeAnswer, error) {
	answers, err := r.jev.Ask(ctx, jev.Request{
		Model:     r.cfg.Agrouter.JevModel,
		State:     prompt.ChunkState{Anchor: anchor, Chunk: c},
		Questions: questions,
	})
	if err != nil {
		return routeAnswer{}, fmt.Errorf("chunk request: %w", err)
	}
	route, ok := answers[questionRoute]
	if !ok {
		return routeAnswer{}, fmt.Errorf("%w: no %q answer", jev.ErrMalformed, questionRoute)
	}
	relevance, ok := answers[questionRelevance]
	if !ok {
		return routeAnswer{}, fmt.Errorf("%w: no %q answer", jev.ErrMalformed, questionRelevance)
	}
	scores := make([]float64, len(el.Options))
	for i, o := range el.Options {
		p, ok := route.Probabilities[o.ID]
		if !ok {
			return routeAnswer{}, fmt.Errorf("%w: no probability for %q", jev.ErrMalformed, o.ID)
		}
		scores[i] = p
	}
	return routeAnswer{probs: route.Probabilities, result: ChunkResult{Field: c.Field, Index: c.Index, Of: c.Of,
		Relevance: relevance.Noul, Top: ranked(el.Options, scores)}}, nil
}

// pool combines the chunks' probabilities: each chunk weighs its raw relevance, an option's score is
// the weighted average of its probabilities (a plain average when every relevance is 0), and the
// highest score wins with catalog order breaking ties. Zero-relevance chunks have no effect, but
// enough low-relevance ones still dilute a relevant chunk: a raw weighted mean has no cap.
func pool(opts []catalog.Option, seq []*slot) (catalog.Option, []Score) {
	var total float64
	for _, s := range seq {
		total += s.answer.result.Relevance
	}
	scores := make([]float64, len(opts))
	for _, s := range seq {
		w := s.answer.result.Relevance
		if total == 0 {
			w = 1
		}
		for i, o := range opts {
			scores[i] += w * s.answer.probs[o.ID]
		}
	}
	if total == 0 {
		total = float64(len(seq))
	}
	best := 0
	for i := range scores {
		scores[i] /= total
		if scores[i] > scores[best] {
			best = i
		}
	}
	return opts[best], ranked(opts, scores)
}

// ranked lists the top options by score, catalog order breaking ties.
func ranked(opts []catalog.Option, scores []float64) []Score {
	out := make([]Score, len(opts))
	for i, o := range opts {
		out[i] = Score{ID: o.ID, Score: scores[i]}
	}
	slices.SortStableFunc(out, func(a, b Score) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	return out[:min(len(out), topOptions)]
}
