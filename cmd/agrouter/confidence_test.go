package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/router"
)

func TestRecordedConfidence(t *testing.T) {
	answer := func(choice string, confidence float64) *jev.Answer {
		return &jev.Answer{Choice: choice, Confidence: confidence, Probabilities: map[string]float64{choice: 1}}
	}
	const none = `{"cli":"alpha","model":null,"effort":null}`
	tests := []struct {
		name        string
		decision    router.Decision
		want        string
		wantSummary string
	}{
		{name: "no Jev answer", want: none, wantSummary: none},
		{name: "three asked stages", decision: router.Decision{OptionID: "x", Stages: []router.Stage{
			{Level: "cli", Choice: "alpha", Answer: answer("alpha", 0.82)},
			{Level: "model", Choice: "m", Answer: answer("m", 0.64)},
			{Level: "effort", Choice: "low", Answer: answer("low", 0.41)},
		}}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"cli":0.82,"model":0.64,"effort":0.41,"stages":[
			{"level":"cli","choice":"alpha","skipped":false,"confidence":0.82,"probabilities":{"alpha":1}},
			{"level":"model","choice":"m","skipped":false,"confidence":0.64,"probabilities":{"m":1}},
			{"level":"effort","choice":"low","skipped":false,"confidence":0.41,"probabilities":{"low":1}}]}}`,
			wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"cli":0.82,"model":0.64,"effort":0.41}}`},
		{name: "skipped stages are omitted from confidence but listed in verbose", decision: router.Decision{OptionID: "x", Stages: []router.Stage{
			{Level: "cli", Choice: "alpha", Skipped: true},
			{Level: "model", Choice: "m", Answer: answer("m", 0.5)},
			{Level: "effort", Choice: "", Skipped: true},
		}}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"model":0.5,"stages":[
			{"level":"cli","choice":"alpha","skipped":true},
			{"level":"model","choice":"m","skipped":false,"confidence":0.5,"probabilities":{"m":1}},
			{"level":"effort","choice":"","skipped":true}]}}`,
			wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"model":0.5}}`},
		{name: "one option: every stage skipped", decision: router.Decision{OptionID: "x", Stages: []router.Stage{
			{Level: "cli", Choice: "alpha", Skipped: true},
			{Level: "model", Choice: "m", Skipped: true},
			{Level: "effort", Choice: "low", Skipped: true},
		}}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"stages":[
			{"level":"cli","choice":"alpha","skipped":true},
			{"level":"model","choice":"m","skipped":true},
			{"level":"effort","choice":"low","skipped":true}]}}`, wantSummary: none},
		{name: "zero is a recorded value", decision: router.Decision{OptionID: "x", Stages: []router.Stage{
			{Level: "effort", Choice: "low", Answer: &jev.Answer{Choice: "low", Confidence: 0}},
		}}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"effort":0,"stages":[{"level":"effort","choice":"low","skipped":false,"confidence":0}]}}`,
			wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"effort":0}}`},
		{name: "a pooled stage reports the mean of its chunks; documents preserve each confidence", decision: router.Decision{
			OptionID: "x", Stages: []router.Stage{{Level: "model", Choice: "b", Pooled: &router.Pooled{
				Chunks: []router.ChunkResult{
					{Field: "prompt", Index: 1, Of: 2, Relevance: 0.9, Confidence: 0, Choice: "a", Probabilities: map[string]float64{"a": 1}},
					{Field: "file", Index: 2, Of: 2, Relevance: 0.2, Confidence: 0.87, Choice: "b", Probabilities: map[string]float64{"b": 1}},
				},
				Scores: []router.Score{{ID: "a", Score: 0.25}, {ID: "b", Score: 0.75}},
			}}}, Complexity: &router.ComplexityResult{Chunks: []router.DocScore{
				{Index: 1, Of: 2, Score: 2, Evidence: 0.6, Confidence: 0.12},
				{Index: 2, Of: 2, Score: 8, Evidence: 0.8, Confidence: 0.94},
			}}}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"model":0.435,"project_complexity":0.53,"stages":[
			{"level":"model","choice":"b","skipped":false,"confidence":0.435,
			 "chunks":[{"field":"prompt","index":1,"of":2,"confidence":0,"choice":"a","relevance":0.9,"probabilities":{"a":1}},
			           {"field":"file","index":2,"of":2,"confidence":0.87,"choice":"b","relevance":0.2,"probabilities":{"b":1}}],
			 "pooled_scores":[{"name":"a","score":0.25},{"name":"b","score":0.75}]}],
			"complexity_chunks":[{"index":1,"of":2,"confidence":0.12,"project_complexity":"2.0/10","evidence":0.6,"score":2},{"index":2,"of":2,"confidence":0.94,"project_complexity":"8.0/10","evidence":0.8,"score":8}]},"project_complexity":"0.0/10"}`,
			wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"model":0.435,"project_complexity":0.53},"project_complexity":"0.0/10"}`},
		{name: "undecided with one CLI: no stage keys, completed stages in verbose", decision: router.Decision{
			Undecided: errors.New("model stage failed"), Stages: []router.Stage{
				{Level: "cli", Choice: "alpha", Skipped: true},
				{Level: "model", Choice: "m", Answer: answer("m", 0.7)},
			}}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"stages":[
			{"level":"cli","choice":"alpha","skipped":true},
			{"level":"model","choice":"m","skipped":false,"confidence":0.7,"probabilities":{"m":1}}]}}`, wantSummary: none},
		{name: "routing failed after complexity", decision: router.Decision{
			Complexity: &router.ComplexityResult{Chunks: []router.DocScore{{Index: 1, Of: 1, Confidence: 0.68}}},
		}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"project_complexity":0.68,"complexity_chunks":[{"index":1,"of":1,"confidence":0.68,"project_complexity":"0.0/10","evidence":0,"score":0}]},"project_complexity":"0.0/10"}`,
			wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"project_complexity":0.68},"project_complexity":"0.0/10"}`},
		{name: "empty chunk lists have no averages", decision: router.Decision{
			OptionID: "x", Stages: []router.Stage{{Level: "effort", Pooled: &router.Pooled{}}}, Complexity: &router.ComplexityResult{},
		}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"stages":[{"level":"effort","choice":"","skipped":false}]},"project_complexity":"0.0/10"}`,
			wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{},"project_complexity":"0.0/10"}`},
		{name: "zero chunk averages are retained", decision: router.Decision{
			OptionID: "x", Stages: []router.Stage{{Level: "effort", Pooled: &router.Pooled{Chunks: []router.ChunkResult{{Field: "prompt", Index: 1, Of: 1}}}}},
			Complexity: &router.ComplexityResult{Chunks: []router.DocScore{{Index: 1, Of: 1}}},
		}, want: `{"cli":"alpha","model":null,"effort":null,"confidence":{"effort":0,"project_complexity":0,"stages":[{"level":"effort","choice":"","skipped":false,"confidence":0,"chunks":[{"field":"prompt","index":1,"of":1,"confidence":0,"choice":"","relevance":0,"probabilities":null}]}],"complexity_chunks":[{"index":1,"of":1,"confidence":0,"project_complexity":"0.0/10","evidence":0,"score":0}]},"project_complexity":"0.0/10"}`,
			wantSummary: `{"cli":"alpha","model":null,"effort":null,"confidence":{"effort":0,"project_complexity":0},"project_complexity":"0.0/10"}`},
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
