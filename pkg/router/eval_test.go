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

// TestEvalRouting routes every case of testdata/routing against the real Jev with both encodings and
// reports accuracy and the confidence distribution. It reports rather than fails on accuracy: run it
// with make eval-routing (needs TYPESAFE_API_KEY), never in CI.
func TestEvalRouting(t *testing.T) {
	key := os.Getenv(config.EnvAPIKey)
	if key == "" {
		t.Skip(config.EnvAPIKey + " is not set")
	}
	for _, enc := range []Encoding{EncodingCompact, EncodingFull} {
		t.Run(enc.String(), func(t *testing.T) {
			cfg, cat := embedded(t)
			evalQuestions(cfg, enc)
			r, err := New(cfg, cat, jev.New(key), enc)
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
			t.Logf("%s encoding: %s", enc, scoreEval(results))
		})
	}
}
