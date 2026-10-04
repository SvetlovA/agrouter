package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

func TestComplexityQuestion(t *testing.T) {
	q := complexityQuestion("How complex?")
	assert.Equal(t, jev.TypeChoice, q.Type)
	assert.Equal(t, "How complex?", q.Instructions)
	names := q.Criteria.Names()
	require.Len(t, names, 11)
	for i, name := range names {
		assert.Equal(t, strconv.Itoa(i), name, "levels 0 to 10 in order")
	}
	for _, c := range q.Criteria {
		assert.NotEmpty(t, c.Value, "level %s is anchored", c.Name)
	}
}

func TestEvidenceQuestion(t *testing.T) {
	q := evidenceQuestion("Is it evidence?")
	assert.Equal(t, jev.TypeNoul, q.Type)
	assert.Equal(t, "Is it evidence?", q.Instructions)
	assert.Equal(t, []string{"true", "false"}, q.Criteria.Names())
}

func TestComplexityQuestionsFromConfig(t *testing.T) {
	cfg, _ := requestFixture(t)
	qs := complexityQuestions(cfg.Agrouter)
	require.Len(t, qs, 2)
	assert.Equal(t, cfg.Agrouter.ComplexityQuestion, qs[questionComplexity].Instructions)
	assert.Equal(t, cfg.Agrouter.ComplexityEvidence, qs[questionEvidence].Instructions)
}

func TestComplexityGoldenRequest(t *testing.T) {
	cfg, cat := requestFixture(t)
	r := newRouter(t, cfg, cat, nil)
	docs := &prompt.Result{Prompt: "rename the flag", Files: []string{"package flags"},
		Docs: []string{"# Payments\n\nTwelve services, PCI scope, Kafka between them.\n", "Use gofmt.\n"}}
	require.True(t, docs.DocsFit(r.Budget()))

	// force the docs into chunks: the envelope reserves the widest index and count
	chunks := docs.DocChunks(40)
	require.Greater(t, len(chunks), 1)

	tests := []struct {
		name  string
		state any
	}{
		{"complexity", docs.DocsState()},
		{"complexity_chunk", prompt.DocChunkState{Doc: chunks[0]}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := jev.Request{Model: cfg.Agrouter.JevModel, State: tc.state, Questions: complexityQuestions(cfg.Agrouter)}
			got, err := json.MarshalIndent(req, "", "  ")
			require.NoError(t, err)
			assert.NotContains(t, string(got), "rename the flag", "the prompt never reaches a doc request")
			assert.NotContains(t, string(got), "package flags", "nor the mentioned files")

			golden := filepath.Join("testdata", "request_"+tc.name+".json")
			if *update {
				require.NoError(t, os.WriteFile(golden, append(got, '\n'), 0o600))
			}
			want, err := os.ReadFile(golden) //nolint:gosec // test fixture path
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got)+"\n")
		})
	}
}
