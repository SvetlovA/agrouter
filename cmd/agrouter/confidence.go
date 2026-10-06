package main

import "github.com/SvetlovA/agrouter/pkg/router"

// confidenceJSON records Jev's confidence and arithmetic means for reporting only.
type confidenceJSON struct {
	ModelSelection    *float64 `json:"model_selection,omitempty"`
	ProjectComplexity *float64 `json:"project_complexity,omitempty"`
	*confidenceDetailsJSON
}

// confidenceDetailsJSON is present only in verbose output.
type confidenceDetailsJSON struct {
	Route            *float64                   `json:"route"`
	Choice           string                     `json:"choice,omitempty"`
	Probabilities    map[string]float64         `json:"probabilities,omitempty"`
	PooledScores     []optionScoreJSON          `json:"pooled_scores,omitempty"`
	RoutingChunks    []routingConfidenceJSON    `json:"routing_chunks,omitempty"`
	ComplexityChunks []complexityConfidenceJSON `json:"complexity_chunks,omitempty"`
}

type routingConfidenceJSON struct {
	Field         string             `json:"field"`
	Index         int                `json:"index"`
	Of            int                `json:"of"`
	Confidence    float64            `json:"confidence"`
	Choice        string             `json:"choice"`
	Relevance     float64            `json:"relevance"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type optionScoreJSON struct {
	Option string  `json:"option"`
	Score  float64 `json:"score"`
}

type complexityConfidenceJSON struct {
	Index             int     `json:"index"`
	Of                int     `json:"of"`
	Confidence        float64 `json:"confidence"`
	ProjectComplexity float64 `json:"project_complexity"`
	Evidence          float64 `json:"evidence"`
}

func recordedConfidence(d router.Decision, verbose bool) *confidenceJSON {
	if d.Answer == nil && d.Pooled == nil && d.Complexity == nil {
		return nil
	}
	out := &confidenceJSON{}
	if verbose {
		out.confidenceDetailsJSON = &confidenceDetailsJSON{}
	}
	if d.Answer != nil {
		if verbose {
			out.Route = &d.Answer.Confidence
			out.Choice = d.Answer.Choice
			out.Probabilities = d.Answer.Probabilities
		}
		out.ModelSelection = &d.Answer.Confidence
	}
	if d.Pooled != nil {
		if verbose {
			for _, s := range d.Pooled.Scores {
				out.PooledScores = append(out.PooledScores, optionScoreJSON{Option: s.ID, Score: s.Score})
			}
		}
		var sum float64
		for _, c := range d.Pooled.Chunks {
			sum += c.Confidence
			if verbose {
				out.RoutingChunks = append(out.RoutingChunks, routingConfidenceJSON{
					Field: c.Field, Index: c.Index, Of: c.Of, Confidence: c.Confidence,
					Choice: c.Choice, Relevance: c.Relevance, Probabilities: c.Probabilities,
				})
			}
		}
		if count := len(d.Pooled.Chunks); count > 0 {
			average := sum / float64(count)
			out.ModelSelection = &average
		}
	}
	if d.Complexity != nil {
		var sum float64
		for _, c := range d.Complexity.Chunks {
			sum += c.Confidence
			if verbose {
				out.ComplexityChunks = append(out.ComplexityChunks, complexityConfidenceJSON{
					Index: c.Index, Of: c.Of, Confidence: c.Confidence,
					ProjectComplexity: c.Score, Evidence: c.Evidence,
				})
			}
		}
		if count := len(d.Complexity.Chunks); count > 0 {
			average := sum / float64(count)
			out.ProjectComplexity = &average
		}
	}
	return out
}
