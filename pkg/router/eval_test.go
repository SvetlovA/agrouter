//go:build eval

package router

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/jev"
)

// TestEvalRouting routes every case of testdata/routing against the real Jev and reports accuracy,
// latency per case and in total, and the confidence distribution of each routing stage, which is
// conditional on the stages before it. It reports rather than fails on accuracy: run it with
// make eval-routing (needs TYPESAFE_API_KEY), never in CI.
func TestEvalRouting(t *testing.T) {
	key := os.Getenv(config.EnvAPIKey)
	if key == "" {
		t.Skip(config.EnvAPIKey + " is not set")
	}
	cfg, cat := embedded(t)
	r, err := New(cfg, cat, jev.New(key))
	require.NoError(t, err)
	cases, err := loadEvalCases(evalDir, cat)
	require.NoError(t, err)

	results := make([]evalResult, 0, len(cases))
	for _, c := range cases {
		results = append(results, runEvalCase(context.Background(), r, cfg, cat, c, t.TempDir()))
	}
	for _, line := range resultLines(results) {
		t.Log(line)
	}
	t.Log(scoreEval(results))
}
