package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/router"
)

func TestRecordedConfidence(t *testing.T) {
	tests := []struct {
		name        string
		decision    router.Decision
		want        string
		wantSummary string
	}{
		{name: "no Jev answer", want: `{"cli":"alpha","model":null,"effort":null}`, wantSummary: `{"cli":"alpha","model":null,"effort":null}`},
		{name: "zero is a recorded value", decision: router.Decision{Answer: &jev.Answer{Confidence: 0}},
			want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"route":0,"model_selection":0}}`, wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"model_selection":0}}`},
		{name: "whole route and document answer", decision: router.Decision{
			Answer: &jev.Answer{Confidence: 0.73}, Complexity: &router.ComplexityResult{Chunks: []router.DocScore{
				{Index: 1, Of: 1, Score: 7, Evidence: 0.9, Confidence: 0.41},
			}}}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"route":0.73,"model_selection":0.73,"project_complexity":0.41,"complexity_chunks":[{"index":1,"of":1,"confidence":0.41,"project_complexity":7,"evidence":0.9}]},"project_complexity":0}`, wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"model_selection":0.73,"project_complexity":0.41},"project_complexity":0}`},
		{name: "pooled route and documents preserve each confidence", decision: router.Decision{
			Pooled: &router.Pooled{Chunks: []router.ChunkResult{
				{Field: "prompt", Index: 1, Of: 2, Relevance: 0.9, Confidence: 0},
				{Field: "file", Index: 2, Of: 2, Relevance: 0.2, Confidence: 0.87},
			}}, Complexity: &router.ComplexityResult{Chunks: []router.DocScore{
				{Index: 1, Of: 2, Score: 2, Evidence: 0.6, Confidence: 0.12},
				{Index: 2, Of: 2, Score: 8, Evidence: 0.8, Confidence: 0.94},
			}}}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"route":null,"model_selection":0.435,"project_complexity":0.53,"routing_chunks":[{"field":"prompt","index":1,"of":2,"confidence":0,"choice":"","relevance":0.9,"probabilities":null},{"field":"file","index":2,"of":2,"confidence":0.87,"choice":"","relevance":0.2,"probabilities":null}],"complexity_chunks":[{"index":1,"of":2,"confidence":0.12,"project_complexity":2,"evidence":0.6},{"index":2,"of":2,"confidence":0.94,"project_complexity":8,"evidence":0.8}]},"project_complexity":0}`, wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"model_selection":0.435,"project_complexity":0.53},"project_complexity":0}`},
		{name: "routing failed after complexity", decision: router.Decision{
			Complexity: &router.ComplexityResult{Chunks: []router.DocScore{{Index: 1, Of: 1, Confidence: 0.68}}},
		}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"route":null,"project_complexity":0.68,"complexity_chunks":[{"index":1,"of":1,"confidence":0.68,"project_complexity":0,"evidence":0}]},"project_complexity":0}`, wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"project_complexity":0.68},"project_complexity":0}`},
		{name: "empty chunk lists have no averages", decision: router.Decision{
			Pooled: &router.Pooled{}, Complexity: &router.ComplexityResult{},
		}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"route":null},"project_complexity":0}`, wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{},"project_complexity":0}`},
		{name: "zero chunk averages are retained", decision: router.Decision{
			Pooled:     &router.Pooled{Chunks: []router.ChunkResult{{Field: "prompt", Index: 1, Of: 1}}},
			Complexity: &router.ComplexityResult{Chunks: []router.DocScore{{Index: 1, Of: 1}}},
		}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"route":null,"model_selection":0,"project_complexity":0,"routing_chunks":[{"field":"prompt","index":1,"of":1,"confidence":0,"choice":"","relevance":0,"probabilities":null}],"complexity_chunks":[{"index":1,"of":1,"confidence":0,"project_complexity":0,"evidence":0}]},"project_complexity":0}`, wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"model_selection":0,"project_complexity":0},"project_complexity":0}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.decision.CLI = "alpha"
			data, err := json.Marshal(selection(tc.decision, true))
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(data))
			data, err = json.Marshal(selection(tc.decision, false))
			require.NoError(t, err)
			assert.JSONEq(t, tc.wantSummary, string(data))
		})
	}
}
