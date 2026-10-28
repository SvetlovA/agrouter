package router

import (
	"context"
	"fmt"
	"slices"

	"github.com/SvetlovA/agrouter/pkg/classifier"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

// topOptions is how many criteria debug output lists per chunk and for the pool.
const topOptions = 3

// Score is a criterion's probability in one chunk, or its pooled score.
type Score struct {
	ID    string
	Score float64
}

// ChunkResult is one chunk request's answers, for recording and debug output.
type ChunkResult struct {
	Field         string
	Index         int // 1-based position in the chunk sequence
	Of            int
	Relevance     float64 // the Noul as answered: the chunk's weight in the pool
	Confidence    float64 // Jev's Choice confidence, recorded only; never part of the pool weight
	Choice        string
	Probabilities map[string]float64 // every criterion, retained for verbose reporting
	Top           []Score
	Estimated     int  // agrouter's token estimate of this chunk's request
	Reported      *int // the provider's input-token count for it; nil when it gave none
}

// Pooled is how a split state decided one level: every chunk's answers in sequence order and the pooled
// scores. It has no confidence: a pooled decision is agrouter's, not a Jev choice.
type Pooled struct {
	Chunks []ChunkResult
	Top    []Score
	Scores []Score // every criterion in catalog order, retained for verbose reporting
}

// routeAnswer is one chunk request's validated answers.
type routeAnswer struct {
	probs  map[string]float64
	result ChunkResult
}

// slot is one chunk of a split state and, once asked, its answers.
type slot = piece[prompt.Chunk, routeAnswer]

// pooled asks lv's question of every chunk, all at once, and pools the answers over gs, returning
// the winning group's index. Any chunk failure means Jev cannot decide; a rejected chunk is never resent.
func (r *Router) pooled(ctx context.Context, el *Eligibility, lv level, gs []group, split *prompt.Split) (int, *Pooled, error) {
	questions := map[string]classifier.Question{
		lv.name:           stageQuestion(r.cfg, lv, chunkGuide, gs, el.effort),
		questionRelevance: relevanceQuestion(),
	}
	names := groupLabels(gs)
	f := fanout[prompt.Chunk, routeAnswer]{
		name: "chunk",
		ask: func(ctx context.Context, c prompt.Chunk) (routeAnswer, error) {
			return r.askChunk(ctx, lv.name, names, split.Anchor, questions, c)
		},
	}
	seq, err := f.run(ctx, split.Chunks)
	if err != nil {
		return 0, nil, err
	}

	best, top, all := pool(names, seq)
	p := &Pooled{Top: top, Scores: all, Chunks: make([]ChunkResult, len(seq))}
	for i, s := range seq {
		p.Chunks[i] = s.answer.result
	}
	return best, p, nil
}

// askChunk sends one chunk request and returns its validated answers, with a probability under
// question id for every criterion in names.
func (r *Router) askChunk(ctx context.Context, id string, names []string, anchor prompt.Anchor,
	questions map[string]classifier.Question, c prompt.Chunk) (routeAnswer, error) {
	state := prompt.ChunkState{Anchor: anchor, Chunk: c}
	resp, err := r.jev.Ask(ctx, classifier.Request{Model: r.cfg.Classifier.Model, State: state, Questions: questions})
	if err != nil {
		return routeAnswer{}, fmt.Errorf("chunk request: %w", r.sized(err, state, questions))
	}
	answers := resp.Answers
	route, ok := answers[id]
	if !ok {
		return routeAnswer{}, fmt.Errorf("%w: %q", classifier.ErrEmptyAnswer, id)
	}
	relevance, ok := answers[questionRelevance]
	if !ok {
		return routeAnswer{}, fmt.Errorf("%w: %q", classifier.ErrEmptyAnswer, questionRelevance)
	}
	scores, err := scoresOf(route, names)
	if err != nil {
		return routeAnswer{}, err
	}
	return routeAnswer{probs: route.Probabilities, result: ChunkResult{Field: c.Field, Index: c.Index, Of: c.Of,
		Relevance: relevance.Noul, Confidence: route.Confidence, Choice: route.Choice,
		Probabilities: route.Probabilities, Top: ranked(names, scores),
		Estimated: r.estimate(state, questions), Reported: resp.InputTokens}}, nil
}

// pool combines the chunks' probabilities over the ordered criterion names: each chunk weighs its
// raw relevance, a criterion's score is the weighted average of its probabilities (a plain average
// when every relevance is 0), and the highest score wins with the order of names breaking ties. It
// returns the winner's index in names, the top scores and every score. Zero-relevance chunks have no effect,
// but enough low-relevance ones still dilute a relevant chunk: a raw weighted mean has no cap.
func pool(names []string, seq []*slot) (int, []Score, []Score) {
	pooled := pooledScores(names, seq)
	best := 0
	scores := make([]float64, len(pooled))
	for i, s := range pooled {
		scores[i] = s.Score
		if s.Score > pooled[best].Score {
			best = i
		}
	}
	return best, ranked(names, scores), pooled
}

// pooledScores retains the full relevance-weighted result in the order of names.
func pooledScores(names []string, seq []*slot) []Score {
	var total float64
	for _, s := range seq {
		total += s.answer.result.Relevance
	}
	scores := make([]float64, len(names))
	for _, s := range seq {
		w := s.answer.result.Relevance
		if total == 0 {
			w = 1
		}
		for i, name := range names {
			scores[i] += w * s.answer.probs[name]
		}
	}
	if total == 0 {
		total = float64(len(seq))
	}
	out := make([]Score, len(names))
	for i := range scores {
		scores[i] /= total
		out[i] = Score{ID: names[i], Score: scores[i]}
	}
	return out
}

// ranked lists the top criteria by score, the order of names breaking ties.
func ranked(names []string, scores []float64) []Score {
	out := make([]Score, len(names))
	for i, name := range names {
		out[i] = Score{ID: name, Score: scores[i]}
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
