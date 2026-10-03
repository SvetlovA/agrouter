package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

// evalMock answers the route question with pick (probability 1, confidence 0.8) and relevance with
// 0.5, over whatever options each request sends.
func evalMock(pick string) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		out := map[string]jev.Answer{}
		for id, q := range req.Questions {
			if q.Type == jev.TypeNoul {
				out[id] = jev.Answer{Type: jev.TypeNoul, Noul: 0.5}
				continue
			}
			probs := map[string]float64{}
			for _, name := range q.Criteria.Names() {
				probs[name] = 0
			}
			probs[pick] = 1
			out[id] = jev.Answer{Type: jev.TypeChoice, Choice: pick, Probabilities: probs, Confidence: 0.8}
		}
		return out, nil
	}}
}

func TestLoadEvalCases_SeedSet(t *testing.T) {
	_, cat := embedded(t)
	cases, err := loadEvalCases(evalDir, cat)
	require.NoError(t, err)

	byName := map[string]evalCase{}
	for _, c := range cases {
		byName[c.Name] = c
	}
	for _, name := range []string{"ralphex-task", "ralphex-review", "ralphex-plan", "trivial-typo", "trivial-rename",
		"large-refactor", "oversized-buried-requirement", "oversized-buried-requirement-max", "distant-chunk-dependency"} {
		assert.Contains(t, byName, name)
	}
	task := byName["ralphex-task"]
	assert.Empty(t, task.Prompt, "ralphex sends its prompt on stdin")
	assert.Contains(t, task.Stdin, "### Task 3")

	buried := byName["oversized-buried-requirement"]
	assert.True(t, strings.HasPrefix(buried.Stdin, "Requirement:"), "filler after the requirement")
	assert.Greater(t, len(buried.Stdin), 1_000_000)

	distant := byName["distant-chunk-dependency"]
	assert.True(t, strings.HasSuffix(distant.Stdin, "}\n"), "reconcile at the end, after the filler")
	assert.Contains(t, distant.Stdin[len(distant.Stdin)-3000:], "func reconcile")
}

func TestLoadEvalCase_Invalid(t *testing.T) {
	_, cat := embedded(t)
	tests := map[string]string{
		"unknown field":    `{"prompt":"x","acceptable":["claude-haiku-4-5"],"extra":1}`,
		"no acceptable":    `{"prompt":"x"}`,
		"unknown option":   `{"prompt":"x","acceptable":["gpt-9@low"]}`,
		"no prompt":        `{"acceptable":["claude-haiku-4-5"]}`,
		"bad filler":       `{"prompt":"x","filler":{"text":"a","bytes":10,"position":"middle"},"acceptable":["claude-haiku-4-5"]}`,
		"no stdin file":    `{"prompt":"x","stdin_file":"missing.txt","acceptable":["claude-haiku-4-5"]}`,
		"malformed json":   `{"prompt":`,
		"empty filler txt": `{"prompt":"x","filler":{"bytes":10,"position":"after"},"acceptable":["claude-haiku-4-5"]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "case.json"), []byte(content), 0o600))
			_, err := loadEvalCases(dir, cat)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "case.json")
		})
	}

	_, err := loadEvalCases(t.TempDir(), cat)
	require.ErrorContains(t, err, "no cases")
}

func TestLoadEvalCase_Filler(t *testing.T) {
	_, cat := embedded(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.json"),
		[]byte(`{"stdin":"REQ","filler":{"text":"ab","bytes":5,"position":"before"},"acceptable":["claude-haiku-4-5"]}`), 0o600))
	cases, err := loadEvalCases(dir, cat)
	require.NoError(t, err)
	assert.Equal(t, "ababaREQ", cases[0].Stdin)
}

func TestRunEvalCase(t *testing.T) {
	cfg, cat := embedded(t)
	ctx := context.Background()

	t.Run("correct, with the mentioned file in the state", func(t *testing.T) {
		client := evalMock("claude-haiku-4-5")
		r := newRouter(t, cfg, cat, client)
		c := evalCase{Name: "typo", Prompt: "fix the typo in README.md", Files: map[string]string{"README.md": "Call get() here.\n"},
			Acceptable: []string{"claude-haiku-4-5"}}
		res := runEvalCase(ctx, r, cfg, cat, c, t.TempDir())
		require.NoError(t, res.Err)
		assert.True(t, res.Correct)
		assert.False(t, res.Split)
		assert.InDelta(t, 0.8, res.Confidence, 1e-9)
		require.Len(t, client.AskCalls(), 1)
		assert.Equal(t, []string{"Call get() here.\n"}, client.AskCalls()[0].Req.State.(prompt.State).Files)
	})

	t.Run("wrong", func(t *testing.T) {
		r := newRouter(t, cfg, cat, evalMock("gpt-6-astra@ultra"))
		res := runEvalCase(ctx, r, cfg, cat, evalCase{Name: "w", Prompt: "x", Acceptable: []string{"claude-haiku-4-5"}},
			t.TempDir())
		require.NoError(t, res.Err)
		assert.False(t, res.Correct)
		assert.Equal(t, "gpt-6-astra@ultra", res.Chosen)
	})

	t.Run("jev failing with the CLI known is an error, not a guess", func(t *testing.T) {
		r := newRouter(t, cfg, cat, failing(jev.ErrMalformed))
		res := runEvalCase(ctx, r, cfg, cat, evalCase{Name: "f", Prompt: "x", CLI: "claude", Acceptable: []string{"claude-haiku-4-5"}},
			t.TempDir())
		require.ErrorIs(t, res.Err, jev.ErrMalformed)
	})

	t.Run("cannot decide", func(t *testing.T) {
		r := newRouter(t, cfg, cat, failing(jev.ErrMalformed))
		res := runEvalCase(ctx, r, cfg, cat, evalCase{Name: "f", Prompt: "x", Acceptable: []string{"claude-haiku-4-5"}},
			t.TempDir())
		require.ErrorIs(t, res.Err, ErrCannotDecide)
	})

	t.Run("every seed case routes, and the oversized ones are split, with both encodings", func(t *testing.T) {
		cases, err := loadEvalCases(evalDir, cat)
		require.NoError(t, err)
		for _, enc := range []Encoding{EncodingCompact, EncodingFull} {
			cfg, cat := embedded(t)
			evalQuestions(cfg, enc)
			r, err := New(cfg, cat, evalMock("claude-opus-5-5@high"), enc)
			require.NoError(t, err)
			for _, c := range cases {
				res := runEvalCase(ctx, r, cfg, cat, c, t.TempDir())
				require.NoError(t, res.Err, "%s %s", enc, c.Name)
				assert.Equal(t, strings.HasPrefix(c.Name, "oversized") || strings.HasPrefix(c.Name, "distant"), res.Split,
					"%s %s", enc, c.Name)
			}
		}
	})
}

func TestScoreEval(t *testing.T) {
	results := []evalResult{
		{Case: "a", Chosen: "x", Correct: true, Confidence: 0.95},
		{Case: "b", Chosen: "x", Correct: true, Confidence: 1},
		{Case: "c", Chosen: "y", Confidence: 0.42},
		{Case: "d", Chosen: "x", Correct: true, Split: true},
		{Case: "e", Err: errors.New("timeout")},
	}
	rep := scoreEval(results)
	assert.Equal(t, 5, rep.Total)
	assert.Equal(t, 3, rep.Correct)
	assert.Equal(t, 1, rep.Errors)
	assert.InDelta(t, 0.6, rep.Accuracy(), 1e-9)
	assert.Equal(t, 2, rep.CorrectBuckets[9], "1.0 falls in the top decile")
	assert.Equal(t, 1, rep.WrongBuckets[4])
	assert.Equal(t, "accuracy 3/5 (60.0%), 1 error(s)\nconfidence  correct  wrong\n"+
		"0.4-0.5         0      1\n0.9-1.0         2      0\n", rep.String())
	assert.Zero(t, evalReport{}.Accuracy())

	assert.Equal(t, []string{
		"ERROR e: timeout",
		"WRONG c: y confidence 0.420 in 0s",
		"ok    a: x confidence 0.950 in 0s",
		"ok    b: x confidence 1.000 in 0s",
		"ok    d: x (split) in 0s",
	}, resultLines(results))
}

func TestEvalQuestions(t *testing.T) {
	cfg, _ := embedded(t)
	q := cfg.Agrouter.Question
	evalQuestions(cfg, EncodingCompact)
	assert.Equal(t, q, cfg.Agrouter.Question)

	evalQuestions(cfg, EncodingFull)
	assert.NotContains(t, cfg.Agrouter.Question, "Look up")
	assert.NotContains(t, cfg.Agrouter.ChunkQuestion, "Look up")
	assert.True(t, strings.HasPrefix(q, cfg.Agrouter.Question))
}
