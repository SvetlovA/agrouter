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
	ProjectComplexity string  `json:"project_complexity"`
	Score             float64 `json:"score"`
	Evidence          float64 `json:"evidence"`
}

func recordedConfidence(d router.Decision, verbose bool) *confidenceJSON {
	asked := askedStages(d)
	if len(asked) == 0 && d.Complexity == nil {
		return nil
	}
	out := &confidenceJSON{}
	if verbose {
		out.confidenceDetailsJSON = &confidenceDetailsJSON{}
	}
	// the last asked stage reports, until per-stage confidence replaces model_selection
	for _, st := range asked {
		if st.Answer != nil {
			if verbose {
				out.Route = &st.Answer.Confidence
				out.Choice = st.Answer.Choice
				out.Probabilities = st.Answer.Probabilities
			}
			out.ModelSelection = &st.Answer.Confidence
			continue
		}
		if verbose {
			for _, s := range st.Pooled.Scores {
				out.PooledScores = append(out.PooledScores, optionScoreJSON{Option: s.ID, Score: s.Score})
			}
		}
		var sum float64
		for _, c := range st.Pooled.Chunks {
			sum += c.Confidence
			if verbose {
				out.RoutingChunks = append(out.RoutingChunks, routingConfidenceJSON{
					Field: c.Field, Index: c.Index, Of: c.Of, Confidence: c.Confidence,
					Choice: c.Choice, Relevance: c.Relevance, Probabilities: c.Probabilities,
				})
			}
		}
		if count := len(st.Pooled.Chunks); count > 0 {
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
					ProjectComplexity: formatComplexity(c.Score), Score: c.Score, Evidence: c.Evidence,
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

// askedStages are d's routing stages Jev answered, in order: none when the routing is undecided.
func askedStages(d router.Decision) []router.Stage {
	if d.OptionID == "" {
		return nil
	}
	var out []router.Stage
	for _, st := range d.Stages {
		if st.Answer != nil || st.Pooled != nil {
			out = append(out, st)
		}
	}
	return out
}
