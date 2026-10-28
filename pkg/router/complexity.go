package router

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

// DocScore is one doc request's answers, for recording and debug output.
type DocScore struct {
	Index      int // as sent; the whole-docs request is 1 of 1
	Of         int
	Score      float64 // Jev's Score normalized from its level range to 0 to 10
	Confidence float64 // recorded only; never part of the reduce weight
	Evidence   float64 // the Noul as answered: the request's weight in the reduce
	Estimated  int     // agrouter's token estimate of the request
	Reported   *int    // the provider's input-token count for it; nil when it gave none
}

// ComplexityResult is how the docs were scored: every doc request's answers in order and the
// reduced project complexity, 0 to 10 with one decimal.
type ComplexityResult struct {
	Chunks     []DocScore
	Complexity float64
}

// complexity scores the docs: one request when they all fit, and otherwise one per doc chunk, all
// at once. Any failure or the deadline is an error, and a rejected request is never resent: supplied
// docs are never dropped.
func (r *Router) complexity(ctx context.Context, captured *prompt.Result) (*ComplexityResult, error) {
	questions := complexityQuestions(r.cfg.Agrouter)
	chunks := captured.SplitDocs(r.budget)
	if chunks == nil {
		score, err := r.askDocs(ctx, captured.DocsState(), questions)
		if err != nil {
			return nil, err
		}
		score.Index, score.Of = 1, 1
		return reduce([]DocScore{score}), nil
	}
	f := fanout[prompt.DocChunk, DocScore]{
		name: "doc chunk",
		ask: func(ctx context.Context, c prompt.DocChunk) (DocScore, error) {
			score, err := r.askDocs(ctx, prompt.DocChunkState{Doc: c}, questions)
			score.Index, score.Of = c.Index, c.Of
			return score, err
		},
	}
	seq, err := f.run(ctx, chunks)
	if err != nil {
		return nil, err
	}
	scores := make([]DocScore, len(seq))
	for i, p := range seq {
		scores[i] = p.answer
	}
	return reduce(scores), nil
}

// askDocs sends one doc request and scores its validated answers.
func (r *Router) askDocs(ctx context.Context, state any, questions map[string]jev.Question) (DocScore, error) {
	resp, err := r.jev.Ask(ctx, jev.Request{Model: r.cfg.Classifier.Model, State: state, Questions: questions})
	if err != nil {
		return DocScore{}, fmt.Errorf("doc request: %w", r.sized(err, state, questions))
	}
	answers := resp.Answers
	level, ok := answers[questionComplexity]
	if !ok {
		return DocScore{}, fmt.Errorf("%w: %q", jev.ErrEmptyAnswer, questionComplexity)
	}
	evidence, ok := answers[questionEvidence]
	if !ok {
		return DocScore{}, fmt.Errorf("%w: %q", jev.ErrEmptyAnswer, questionEvidence)
	}
	// the client validates Score answers; also check completeness for alternate JevClient implementations
	var total float64
	for i := range complexityLevels {
		p, ok := level.Probabilities[strconv.Itoa(i)]
		if !ok {
			return DocScore{}, fmt.Errorf("%w: no probability for complexity %d", jev.ErrInvalidAnswer, i)
		}
		total += p
	}
	if total <= 0 {
		return DocScore{}, fmt.Errorf("%w: complexity probabilities sum to %g", jev.ErrInvalidAnswer, total)
	}
	maxLevel := float64(len(complexityLevels) - 1)
	if math.IsNaN(level.Score) || math.IsInf(level.Score, 0) || level.Score < 0 || level.Score > maxLevel {
		return DocScore{}, fmt.Errorf("%w: complexity score outside the level range", jev.ErrInvalidAnswer)
	}
	return DocScore{Score: level.Score * 10 / maxLevel, Confidence: level.Confidence, Evidence: evidence.Noul,
		Estimated: r.estimate(state, questions), Reported: resp.InputTokens}, nil
}

// reduce combines the doc scores into the project complexity: the mean weighted by evidence (a
// plain mean when every evidence is 0), rounded to one decimal.
func reduce(scores []DocScore) *ComplexityResult {
	var sum, total float64
	for _, s := range scores {
		sum += s.Evidence * s.Score
		total += s.Evidence
	}
	if total == 0 {
		sum = 0
		for _, s := range scores {
			sum += s.Score
		}
		total = float64(len(scores))
	}
	return &ComplexityResult{Chunks: scores, Complexity: math.Round(sum/total*10) / 10}
}
