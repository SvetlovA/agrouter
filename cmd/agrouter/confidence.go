package main

import "github.com/SvetlovA/agrouter/pkg/router"

// confidenceJSON records Jev's confidence and arithmetic means for reporting only. Each routing
// stage's confidence is conditional on the stages before it, not the confidence of the whole choice.
type confidenceJSON struct {
	CLI               *float64 `json:"cli,omitempty"`
	Model             *float64 `json:"model,omitempty"`
	Effort            *float64 `json:"effort,omitempty"`
	ProjectComplexity *float64 `json:"project_complexity,omitempty"`
	*confidenceDetailsJSON
}

// confidenceDetailsJSON is present only in verbose output.
type confidenceDetailsJSON struct {
	Stages           []stageJSON                `json:"stages,omitempty"`
	ComplexityChunks []complexityConfidenceJSON `json:"complexity_chunks,omitempty"`
}

// stageJSON is one routing stage in order: skipped ones carry only the value their options share,
// asked ones Jev's whole-state answer or the per-chunk answers and pooled scores.
type stageJSON struct {
	Level         string                  `json:"level"`
	Choice        string                  `json:"choice"`
	Skipped       bool                    `json:"skipped"`
	Confidence    *float64                `json:"confidence,omitempty"`
	Probabilities map[string]float64      `json:"probabilities,omitempty"`
	Chunks        []routingConfidenceJSON `json:"chunks,omitempty"`
	PooledScores  []scoreJSON             `json:"pooled_scores,omitempty"`
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

type scoreJSON struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
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
	out := &confidenceJSON{}
	if verbose && len(d.Stages) > 0 {
		out.confidenceDetailsJSON = &confidenceDetailsJSON{}
		for _, st := range d.Stages {
			out.Stages = append(out.Stages, verboseStage(st))
		}
	}
	// an undecided routing reports no stage confidence: its completed stages chose nothing
	if d.OptionID != "" {
		for _, st := range d.Stages {
			c := stageConfidence(st)
			switch st.Level {
			case router.LevelCLI:
				out.CLI = c
			case router.LevelModel:
				out.Model = c
			case router.LevelEffort:
				out.Effort = c
			}
		}
	}
	if d.Complexity != nil {
		var sum float64
		for _, c := range d.Complexity.Chunks {
			sum += c.Confidence
			if verbose {
				if out.confidenceDetailsJSON == nil {
					out.confidenceDetailsJSON = &confidenceDetailsJSON{}
				}
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
	if *out == (confidenceJSON{}) && d.Complexity == nil {
		return nil
	}
	return out
}

// stageConfidence is Jev's confidence in an asked stage: the whole-state answer's, or the mean of the
// chunk answers' for a pooled stage. It is nil for a skipped stage or a pool without chunks.
func stageConfidence(st router.Stage) *float64 {
	switch {
	case st.Answer != nil:
		c := st.Answer.Confidence
		return &c
	case st.Pooled != nil && len(st.Pooled.Chunks) > 0:
		var sum float64
		for _, c := range st.Pooled.Chunks {
			sum += c.Confidence
		}
		average := sum / float64(len(st.Pooled.Chunks))
		return &average
	}
	return nil
}

func verboseStage(st router.Stage) stageJSON {
	out := stageJSON{Level: st.Level, Choice: st.Choice, Skipped: st.Skipped, Confidence: stageConfidence(st)}
	if st.Answer != nil {
		out.Probabilities = st.Answer.Probabilities
	}
	if st.Pooled != nil {
		for _, c := range st.Pooled.Chunks {
			out.Chunks = append(out.Chunks, routingConfidenceJSON{
				Field: c.Field, Index: c.Index, Of: c.Of, Confidence: c.Confidence,
				Choice: c.Choice, Relevance: c.Relevance, Probabilities: c.Probabilities,
			})
		}
		for _, s := range st.Pooled.Scores {
			out.PooledScores = append(out.PooledScores, scoreJSON{Name: s.ID, Score: s.Score})
		}
	}
	return out
}
