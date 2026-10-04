package router

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

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

// slot is one chunk of the sequence and, once asked, its answers.
type slot struct {
	chunk prompt.Chunk
	// resplit marks a chunk that already comes from a re-split: another 422 cannot decide
	resplit bool
	done    bool
	probs   map[string]float64
	result  ChunkResult
	err     error // a 422 to re-split; other errors end routing at once
}

// errUnsplittable wraps a 422 that re-splitting cannot help.
var errUnsplittable = errors.New("rejected again after a re-split, or already under the minimum chunk size")

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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ag := r.cfg.Agrouter
	questions := map[string]jev.Question{
		questionRoute:     routeQuestion(r.cfg, ag.ChunkQuestion, el.Options, el.effort, r.enc),
		questionRelevance: relevanceQuestion(ag.Relevance),
	}

	seq := make([]*slot, len(split.Chunks))
	for i, c := range split.Chunks {
		seq[i] = &slot{chunk: c, resplit: resplit}
	}
	for {
		if err := r.round(ctx, cancel, el, split.Anchor, questions, seq); err != nil {
			return outcome{}, err
		}
		next, again, err := resplitRejected(seq)
		if err != nil {
			return outcome{}, err
		}
		if !again {
			break
		}
		seq = next
	}

	o, top := pool(el.Options, seq)
	p := &Pooled{Top: top, Chunks: make([]ChunkResult, len(seq))}
	for i, s := range seq {
		p.Chunks[i] = s.result
	}
	return outcome{option: o, pooled: p}, nil
}

// round asks every pending chunk of seq at once: Jev runs remotely, and 429s are retried within the
// deadline. The first failure other than a 422 cancels the rest and is returned; 422s are left on
// their slots.
func (r *Router) round(ctx context.Context, cancel context.CancelFunc, el *Eligibility, anchor prompt.Anchor,
	questions map[string]jev.Question, seq []*slot) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, s := range seq {
		if s.done {
			continue
		}
		wg.Go(func() {
			err := r.askChunk(ctx, el, anchor, questions, s)
			if err == nil || errors.Is(err, jev.ErrUnprocessable) {
				s.err = err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if firstErr == nil {
				firstErr = fmt.Errorf("chunk %d of %d: %w", s.chunk.Index, s.chunk.Of, err)
				cancel()
			}
		})
	}
	wg.Wait()
	// the deadline only matters when it left a chunk unasked; answers that all came in stand
	unasked := slices.ContainsFunc(seq, func(s *slot) bool { return !s.done && s.err == nil })
	if firstErr == nil && ctx.Err() != nil && unasked {
		return fmt.Errorf("chunk requests: %w", ctx.Err())
	}
	return firstErr
}

// askChunk sends one chunk request and records its validated answers on s.
func (r *Router) askChunk(ctx context.Context, el *Eligibility, anchor prompt.Anchor,
	questions map[string]jev.Question, s *slot) error {
	answers, err := r.jev.Ask(ctx, jev.Request{
		Model:     r.cfg.Agrouter.JevModel,
		State:     prompt.ChunkState{Anchor: anchor, Chunk: s.chunk},
		Questions: questions,
	})
	if err != nil {
		return fmt.Errorf("chunk request: %w", err)
	}
	route, ok := answers[questionRoute]
	if !ok {
		return fmt.Errorf("%w: no %q answer", jev.ErrMalformed, questionRoute)
	}
	relevance, ok := answers[questionRelevance]
	if !ok {
		return fmt.Errorf("%w: no %q answer", jev.ErrMalformed, questionRelevance)
	}
	scores := make([]float64, len(el.Options))
	for i, o := range el.Options {
		p, ok := route.Probabilities[o.ID]
		if !ok {
			return fmt.Errorf("%w: no probability for %q", jev.ErrMalformed, o.ID)
		}
		scores[i] = p
	}
	s.done, s.probs = true, route.Probabilities
	s.result = ChunkResult{Field: s.chunk.Field, Index: s.chunk.Index, Of: s.chunk.Of,
		Relevance: relevance.Noul, Top: ranked(el.Options, scores)}
	return nil
}

// resplitRejected replaces every chunk rejected with a 422 by its halves and renumbers the
// sequence. again is false when nothing was rejected. A chunk that cannot be re-split (a second
// 422, or text under the minimum state size) means Jev cannot decide.
func resplitRejected(seq []*slot) (next []*slot, again bool, err error) {
	for _, s := range seq {
		if s.err == nil {
			next = append(next, s)
			continue
		}
		if s.resplit || prompt.Tokens(len(s.chunk.Text)) < prompt.MinStateTokens {
			return nil, false, fmt.Errorf("chunk %d of %d: %w: %w", s.chunk.Index, s.chunk.Of, errUnsplittable, s.err)
		}
		again = true
		for _, c := range prompt.Halve(s.chunk) {
			next = append(next, &slot{chunk: c, resplit: true})
		}
	}
	for i, s := range next {
		s.chunk.Index, s.chunk.Of = i+1, len(next)
	}
	return next, again, nil
}

// pool combines the chunks' probabilities: each chunk weighs its raw relevance, an option's score is
// the weighted average of its probabilities (a plain average when every relevance is 0), and the
// highest score wins with catalog order breaking ties. Zero-relevance chunks have no effect, but
// enough low-relevance ones still dilute a relevant chunk: a raw weighted mean has no cap.
func pool(opts []catalog.Option, seq []*slot) (catalog.Option, []Score) {
	var total float64
	for _, s := range seq {
		total += s.result.Relevance
	}
	scores := make([]float64, len(opts))
	for _, s := range seq {
		w := s.result.Relevance
		if total == 0 {
			w = 1
		}
		for i, o := range opts {
			scores[i] += w * s.probs[o.ID]
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
