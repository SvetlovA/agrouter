package jev

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAskScore(t *testing.T) {
	q := Question{Type: TypeScore, Instructions: "rate complexity", Levels: []string{"small", "medium", "large"}}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.JSONEq(t, `{"model":"test-jev","state":{"docs":["three services"]},"questions":{
			"complexity":{"type":"score","instructions":"rate complexity","criteria":["small","medium","large"]}}}`, string(data))
		_, _ = io.WriteString(w, `{"answers":{"complexity":{"type":"score","score":1.5,
			"probabilities":{"0":0,"1":0.5,"2":0.5},"confidence":0.7,
			"legend":{"0":"small","1":"medium","2":"large"}}}}`)
	})
	answers, err := c.Ask(t.Context(), Request{Model: "test-jev", State: map[string]any{"docs": []string{"three services"}},
		Questions: map[string]Question{"complexity": q}})
	require.NoError(t, err)
	assert.Equal(t, Answer{Type: TypeScore, Score: 1.5, Confidence: 0.7,
		Probabilities: map[string]float64{"0": 0, "1": 0.5, "2": 0.5},
		Legend:        map[string]string{"0": "small", "1": "medium", "2": "large"}}, answers["complexity"])
}

func TestScoreValidation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Question, *wireAnswer)
		valid  bool
	}{
		{name: "fractional score", valid: true},
		{name: "zero score and confidence", valid: true, change: func(_ *Question, w *wireAnswer) {
			*w.Score, *w.Confidence = 0, 0
			w.Probabilities = map[string]float64{"0": 1, "1": 0}
		}},
		{name: "maximum score", valid: true, change: func(_ *Question, w *wireAnswer) {
			*w.Score = 1
			w.Probabilities = map[string]float64{"0": 0, "1": 1}
		}},
		{name: "missing score", change: func(_ *Question, w *wireAnswer) { w.Score = nil }},
		{name: "negative score", change: func(_ *Question, w *wireAnswer) { *w.Score = -0.1 }},
		{name: "score above maximum", change: func(_ *Question, w *wireAnswer) { *w.Score = 1.1 }},
		{name: "nan score", change: func(_ *Question, w *wireAnswer) { *w.Score = math.NaN() }},
		{name: "infinite score", change: func(_ *Question, w *wireAnswer) { *w.Score = math.Inf(1) }},
		{name: "missing confidence", change: func(_ *Question, w *wireAnswer) { w.Confidence = nil }},
		{name: "invalid confidence", change: func(_ *Question, w *wireAnswer) { *w.Confidence = 1.1 }},
		{name: "missing probability", change: func(_ *Question, w *wireAnswer) { delete(w.Probabilities, "1") }},
		{name: "unknown probability", change: func(_ *Question, w *wireAnswer) {
			delete(w.Probabilities, "1")
			w.Probabilities["2"] = 0.75
		}},
		{name: "invalid probability", change: func(_ *Question, w *wireAnswer) { w.Probabilities["1"] = -0.1 }},
		{name: "invalid probability sum", change: func(_ *Question, w *wireAnswer) { w.Probabilities["1"] = 0.5 }},
		{name: "missing legend", change: func(_ *Question, w *wireAnswer) { w.Legend = nil }},
		{name: "unknown legend level", change: func(_ *Question, w *wireAnswer) {
			delete(w.Legend, "1")
			w.Legend["2"] = "large"
		}},
		{name: "changed legend description", change: func(_ *Question, w *wireAnswer) { w.Legend["1"] = "different" }},
		{name: "wrong type", change: func(_ *Question, w *wireAnswer) { w.Type = TypeChoice }},
		{name: "too few levels", change: func(q *Question, _ *wireAnswer) { q.Levels = []string{"only"} }},
		{name: "too many levels", change: func(q *Question, _ *wireAnswer) { q.Levels = make([]string, 11) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			score, confidence := 0.75, 0.6
			q := Question{Type: TypeScore, Levels: []string{"small", "large"}}
			w := wireAnswer{Type: TypeScore, Score: &score, Confidence: &confidence,
				Probabilities: map[string]float64{"0": 0.25, "1": 0.75}, Legend: map[string]string{"0": "small", "1": "large"}}
			if tc.change != nil {
				tc.change(&q, &w)
			}
			got, err := validate(q, w)
			if tc.valid {
				require.NoError(t, err)
				assert.InDelta(t, *w.Score, got.Score, 1e-9)
				assert.InDelta(t, *w.Confidence, got.Confidence, 1e-9)
				return
			}
			require.Error(t, err)
			// malformed Score responses follow the same cannot-decide path as other answers
			if data, marshalErr := json.Marshal(map[string]any{"answers": map[string]wireAnswer{"complexity": w}}); marshalErr == nil {
				_, err = decode(data, map[string]Question{"complexity": q})
				require.ErrorIs(t, err, ErrMalformed)
			}
		})
	}
}
