package router

import (
	"context"
	"errors"
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
}

// ComplexityResult is how the docs were scored: every doc request's answers in order and the
// reduced project complexity, 0 to 10 with one decimal.
type ComplexityResult struct {
	Chunks     []DocScore
	Complexity float64
}

// complexity scores the docs: one request when they all fit, and otherwise one per doc chunk, all
// at once. A 422 on the whole docs splits them at half their size or of the doc budget, whichever is
// smaller, once; a 422 on a doc chunk halves it, once. Any other failure, a second 422 or the deadline is an error: supplied docs are
// never dropped.
func (r *Router) complexity(ctx context.Context, captured *prompt.Result) (*ComplexityResult, error) {
	questions := complexityQuestions(r.cfg.Agrouter)
	chunks, resplit := captured.SplitDocs(r.budget), false
	if chunks == nil {
		score, err := r.askDocs(ctx, captured.DocsState(), questions)
		if err == nil {
			score.Index, score.Of = 1, 1
			return reduce([]DocScore{score}), nil
		}
		if !errors.Is(err, jev.ErrUnprocessable) {
			return nil, err
		}
		if captured.DocsTokens() < prompt.MinStateTokens {
			return nil, fmt.Errorf("%w: %w", errUnsplittable, err)
		}
		// half of the docs' own size, so a single doc well under the budget is still cut in two
		chunks, resplit = captured.DocChunks(min(r.budget.Doc, captured.DocsTokens())/2), true
	}
	seq, err := r.docFanout(questions).run(ctx, chunks, resplit)
	if err != nil {
		return nil, err
	}
	scores := make([]DocScore, len(seq))
	for i, p := range seq {
		scores[i] = p.answer
	}
	return reduce(scores), nil
}

// docFanout asks doc chunk requests with questions.
func (r *Router) docFanout(questions map[string]jev.Question) fanout[prompt.DocChunk, DocScore] {
	return fanout[prompt.DocChunk, DocScore]{
		name: "doc chunk",
		ask: func(ctx context.Context, c prompt.DocChunk) (DocScore, error) {
			score, err := r.askDocs(ctx, prompt.DocChunkState{Doc: c}, questions)
			score.Index, score.Of = c.Index, c.Of
			return score, err
		},
		halve: prompt.HalveDoc,
		text:  func(c prompt.DocChunk) string { return c.Text },
		number: func(c prompt.DocChunk, index, of int) prompt.DocChunk {
			c.Index, c.Of = index, of
			return c
		},
	}
}

// askDocs sends one doc request and scores its validated answers.
func (r *Router) askDocs(ctx context.Context, state any, questions map[string]jev.Question) (DocScore, error) {
	answers, err := r.jev.Ask(ctx, jev.Request{Model: r.cfg.Agrouter.JevModel, State: state, Questions: questions})
	if err != nil {
		return DocScore{}, fmt.Errorf("doc request: %w", err)
	}
	level, ok := answers[questionComplexity]
	if !ok {
		return DocScore{}, fmt.Errorf("%w: no %q answer", jev.ErrMalformed, questionComplexity)
	}
	evidence, ok := answers[questionEvidence]
	if !ok {
		return DocScore{}, fmt.Errorf("%w: no %q answer", jev.ErrMalformed, questionEvidence)
	}
	// the client validates Score answers; also check completeness for alternate JevClient implementations
	var total float64
	for i := range complexityLevels {
		p, ok := level.Probabilities[strconv.Itoa(i)]
		if !ok {
			return DocScore{}, fmt.Errorf("%w: no probability for complexity %d", jev.ErrMalformed, i)
		}
		total += p
	}
	if total <= 0 {
		return DocScore{}, fmt.Errorf("%w: complexity probabilities sum to %g", jev.ErrMalformed, total)
	}
	maxLevel := float64(len(complexityLevels) - 1)
	if math.IsNaN(level.Score) || math.IsInf(level.Score, 0) || level.Score < 0 || level.Score > maxLevel {
		return DocScore{}, fmt.Errorf("%w: complexity score outside the level range", jev.ErrMalformed)
	}
	return DocScore{Score: level.Score * 10 / maxLevel, Confidence: level.Confidence, Evidence: evidence.Noul}, nil
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
