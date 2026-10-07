package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

// evalMock answers each stage question with pick's part (probability 1, confidence 0.8; see
// pickAt), the complexity Score with its last level, and the Nouls (relevance, evidence) with 0.5,
// over whatever criteria each request sends.
func evalMock(pick string) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		out := map[string]jev.Answer{}
		for id, q := range req.Questions {
			if q.Type == jev.TypeNoul {
				out[id] = jev.Answer{Type: jev.TypeNoul, Noul: 0.5}
				continue
			}
			if q.Type == jev.TypeScore {
				out[id] = levels(map[int]float64{len(q.Levels) - 1: 1})
				continue
			}
			names := q.Criteria.Names()
			choice := pickAt(id, q, pick)
			if !slices.Contains(names, choice) {
				choice = names[len(names)-1]
			}
			probs := map[string]float64{}
			for _, name := range names {
				probs[name] = 0
			}
			probs[choice] = 1
			out[id] = jev.Answer{Type: jev.TypeChoice, Choice: choice, Probabilities: probs, Confidence: 0.8}
		}
		return out, nil
	}}
}

// pickAt is the criterion name of option id pick ("<section>@<effort>" or "<section>") in the stage
// question q asked under id: its CLI, found in the cli stage's instructions, its section, or its
// effort label.
func pickAt(id string, q jev.Question, pick string) string {
	section, effort, _ := strings.Cut(pick, "@")
	switch id {
	case LevelModel:
		return section
	case LevelEffort:
		return effort
	}
	in, _ := q.Instructions.(stageInstructions)
	for _, c := range in.CLIs {
		if has(c.Value.(cliEntry).Models, section) {
			return c.Name
		}
	}
	return pick
}

func TestLoadEvalCases_SeedSet(t *testing.T) {
	cfg, cat := embedded(t)
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

	small, enterprise := byName["complexity-refactor-small-script"], byName["complexity-refactor-enterprise"]
	assert.Equal(t, small.Prompt, enterprise.Prompt, "the pair differs only by its doc")
	assert.NotEqual(t, small.Acceptable, enterprise.Acceptable)
	require.Len(t, small.DocTexts, 1)
	require.Len(t, enterprise.DocTexts, 1)
	assert.NotEqual(t, small.DocTexts[0], enterprise.DocTexts[0])
	for _, name := range []string{"complexity-typo-enterprise", "complexity-style-only-doc", "complexity-unrelated-module"} {
		assert.NotEmpty(t, byName[name].DocTexts, name)
	}

	midText := byName["complexity-buried-task-and-fact"]
	assert.Empty(t, midText.Prompt)
	assertMidText(t, midText.FileText, "Action item for the agent")
	require.Len(t, midText.DocTexts, 1)
	assertMidText(t, midText.DocTexts[0], "## Outbound calls")
	b := newRouter(t, cfg, cat, &mocks.JevClientMock{}).budget
	assert.Greater(t, len((&prompt.Result{Docs: midText.DocTexts}).SplitDocs(b)), 1, "the doc is scored in chunks")

	distant := byName["distant-chunk-dependency"]
	assert.True(t, strings.HasSuffix(distant.Stdin, "}\n"), "reconcile at the end, after the filler")
	assert.Contains(t, distant.Stdin[len(distant.Stdin)-3000:], "func reconcile")
}

// assertMidText checks that marker sits in the middle half of text.
func assertMidText(t *testing.T, text, marker string) {
	t.Helper()
	i := strings.Index(text, marker)
	assert.Greater(t, i, len(text)/4, "%q is mid-text", marker)
	assert.Less(t, i, 3*len(text)/4, "%q is mid-text", marker)
}

func TestLoadEvalCase_Invalid(t *testing.T) {
	_, cat := embedded(t)
	tests := map[string]string{
		"unknown field":     `{"prompt":"x","acceptable":["claude-haiku-4-5"],"extra":1}`,
		"no acceptable":     `{"prompt":"x"}`,
		"unknown option":    `{"prompt":"x","acceptable":["gpt-9@low"]}`,
		"no prompt":         `{"acceptable":["claude-haiku-4-5"]}`,
		"bad filler":        `{"prompt":"x","filler":{"text":"a","bytes":10,"position":"middle"},"acceptable":["claude-haiku-4-5"]}`,
		"no stdin file":     `{"prompt":"x","stdin_file":"missing.txt","acceptable":["claude-haiku-4-5"]}`,
		"malformed json":    `{"prompt":`,
		"empty filler txt":  `{"prompt":"x","filler":{"bytes":10,"position":"after"},"acceptable":["claude-haiku-4-5"]}`,
		"no prompt file":    `{"prompt_file":"missing.txt","acceptable":["claude-haiku-4-5"]}`,
		"binary prompt":     `{"prompt_file":"bin.dat","acceptable":["claude-haiku-4-5"]}`,
		"empty prompt file": `{"prompt_file":"empty.txt","acceptable":["claude-haiku-4-5"]}`,
		"no doc file":       `{"prompt":"x","docs":[{"file":"missing.md"}],"acceptable":["claude-haiku-4-5"]}`,
		"binary doc":        `{"prompt":"x","docs":[{"file":"bin.dat"}],"acceptable":["claude-haiku-4-5"]}`,
		"doc text & file":   `{"prompt":"x","docs":[{"text":"a","file":"doc.md"}],"acceptable":["claude-haiku-4-5"]}`,
		"empty doc":         `{"prompt":"x","docs":[{}],"acceptable":["claude-haiku-4-5"]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0, 1, 2, 0xff}, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("doc"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o600))
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

func TestLoadEvalCase_PromptFileAndDocs(t *testing.T) {
	_, cat := embedded(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "task.txt"), []byte("do the task\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("# big system\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.json"), []byte(`{"prompt_file":"task.txt",`+
		`"docs":[{"file":"a.md"},{"text":"inline doc"}],"acceptable":["claude-haiku-4-5"]}`), 0o600))
	cases, err := loadEvalCases(dir, cat)
	require.NoError(t, err)
	require.Len(t, cases, 1)
	c := cases[0]
	assert.Equal(t, "task.txt", c.PromptFile)
	assert.Equal(t, "do the task\n", c.FileText)
	assert.Equal(t, []string{"# big system\n", "inline doc"}, c.DocTexts, "docs in order, files byte for byte")
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
		assert.Equal(t, []evalStage{{Level: LevelCLI, Confidence: 0.8, Correct: true},
			{Level: LevelModel, Confidence: 0.8, Correct: true}}, res.Stages, "the effort stage is skipped")
		require.Len(t, client.AskCalls(), 2, "the cli and model stages; haiku has no efforts")
		for _, call := range client.AskCalls() {
			assert.Equal(t, []string{"Call get() here.\n"}, call.Req.State.(prompt.State).Files)
		}
	})

	t.Run("prompt file and docs: stage 1 scores the docs, stage 2 gets the prompt file text", func(t *testing.T) {
		client := evalMock("claude-haiku-4-5")
		r := newRouter(t, cfg, cat, client)
		c := evalCase{Name: "docs", Prompt: "first", PromptFile: "task.txt", FileText: "from the file",
			DocTexts: []string{"# a large system\n"}, Acceptable: []string{"claude-haiku-4-5"}}
		res := runEvalCase(ctx, r, cfg, cat, c, t.TempDir())
		require.NoError(t, res.Err)
		assert.True(t, res.Correct)
		assert.Equal(t, "10.0", res.Project, "the mock rates the docs at the last level")

		calls := client.AskCalls()
		require.Len(t, calls, 3, "the docs, then the cli and model stages")
		assert.Contains(t, calls[0].Req.Questions, questionComplexity)
		state := calls[1].Req.State.(prompt.State)
		assert.Equal(t, "first\n\nfrom the file", state.Prompt)
		require.NotNil(t, state.Project)
		assert.InDelta(t, 10, state.Project.Complexity, 1e-9)
	})

	t.Run("wrong", func(t *testing.T) {
		r := newRouter(t, cfg, cat, evalMock("gpt-6-astra@ultra"))
		res := runEvalCase(ctx, r, cfg, cat, evalCase{Name: "w", Prompt: "x", Acceptable: []string{"claude-haiku-4-5"}},
			t.TempDir())
		require.NoError(t, res.Err)
		assert.False(t, res.Correct)
		assert.Equal(t, "gpt-6-astra@ultra", res.Chosen)
		require.NotEmpty(t, res.Stages)
		for _, st := range res.Stages {
			assert.False(t, st.Correct, st.Level)
		}
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

	t.Run("every seed case routes, and the oversized ones are split", func(t *testing.T) {
		cases, err := loadEvalCases(evalDir, cat)
		require.NoError(t, err)
		r, err := New(cfg, cat, evalMock("claude-opus-5-5@high"))
		require.NoError(t, err)
		for _, c := range cases {
			res := runEvalCase(ctx, r, cfg, cat, c, t.TempDir())
			require.NoError(t, res.Err, c.Name)
			split := strings.HasPrefix(c.Name, "oversized") || strings.HasPrefix(c.Name, "distant")
			assert.Equal(t, split, res.Split, c.Name)
			for _, st := range res.Stages {
				assert.Equal(t, split, st.Pooled, "%s %s", c.Name, st.Level)
				assert.InDelta(t, 0.8, st.Confidence, 1e-9, "%s %s: pooled stages report the chunk mean", c.Name, st.Level)
			}
		}
	})
}

func TestEvalStages(t *testing.T) {
	a := catalog.Option{ID: "m1@low", CLI: "c1", Section: "m1", Effort: "low"}
	b := catalog.Option{ID: "m1@high", CLI: "c1", Section: "m1", Effort: "high"}
	c := catalog.Option{ID: "m2", CLI: "c2", Section: "m2"}
	asked := func(level, choice string, confidence float64) Stage {
		return Stage{Level: level, Choice: choice, Answer: &jev.Answer{Confidence: confidence}}
	}
	pooled := Stage{Level: LevelEffort, Choice: "high", Pooled: &Pooled{Chunks: []ChunkResult{{Confidence: 0.2}, {Confidence: 0.6}}}}

	tests := map[string]struct {
		stages     []Stage
		acceptable []catalog.Option
		want       []evalStage
	}{
		"all asked and right": {
			stages:     []Stage{asked(LevelCLI, "c1", 0.9), asked(LevelModel, "m1", 0.5), asked(LevelEffort, "low", 0.3)},
			acceptable: []catalog.Option{a, c},
			want: []evalStage{{Level: LevelCLI, Confidence: 0.9, Correct: true},
				{Level: LevelModel, Confidence: 0.5, Correct: true}, {Level: LevelEffort, Confidence: 0.3, Correct: true}},
		},
		"a wrong stage makes every later stage wrong": {
			stages:     []Stage{asked(LevelCLI, "c1", 0.9), asked(LevelModel, "m1", 0.5), asked(LevelEffort, "low", 0.3)},
			acceptable: []catalog.Option{c},
			want: []evalStage{{Level: LevelCLI, Confidence: 0.9}, {Level: LevelModel, Confidence: 0.5},
				{Level: LevelEffort, Confidence: 0.3}},
		},
		"skipped stages narrow but are not reported; pooled stages report the chunk mean": {
			stages: []Stage{{Level: LevelCLI, Choice: "c1", Skipped: true}, {Level: LevelModel, Choice: "m1", Skipped: true},
				pooled},
			acceptable: []catalog.Option{a, b},
			want:       []evalStage{{Level: LevelEffort, Confidence: 0.4, Pooled: true, Correct: true}},
		},
		"the effort of the wrong option": {
			stages:     []Stage{asked(LevelEffort, "high", 0.7)},
			acceptable: []catalog.Option{a},
			want:       []evalStage{{Level: LevelEffort, Confidence: 0.7}},
		},
		"no stages": {acceptable: []catalog.Option{a}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := evalStages(tt.stages, tt.acceptable)
			require.Len(t, got, len(tt.want))
			for i := range tt.want {
				assert.Equal(t, tt.want[i].Level, got[i].Level)
				assert.InDelta(t, tt.want[i].Confidence, got[i].Confidence, 1e-9)
				assert.Equal(t, tt.want[i].Pooled, got[i].Pooled)
				assert.Equal(t, tt.want[i].Correct, got[i].Correct)
			}
		})
	}
}

func TestScoreEval(t *testing.T) {
	results := []evalResult{
		{Case: "a", Chosen: "x", Correct: true, Duration: time.Second,
			Stages: []evalStage{{Level: LevelCLI, Confidence: 0.95, Correct: true}, {Level: LevelModel, Confidence: 0.45, Correct: true}}},
		{Case: "b", Chosen: "x", Correct: true, Stages: []evalStage{{Level: LevelModel, Confidence: 1, Correct: true}}},
		{Case: "c", Chosen: "y", Duration: 2 * time.Second,
			Stages: []evalStage{{Level: LevelCLI, Confidence: 0.97, Correct: true}, {Level: LevelModel, Confidence: 0.42}}},
		{Case: "d", Chosen: "x", Correct: true, Split: true,
			Stages: []evalStage{{Level: LevelEffort, Confidence: 0.3, Pooled: true, Correct: true}}},
		{Case: "e", Err: errors.New("timeout"), Duration: 3 * time.Second},
		{Case: "f", Chosen: "x", Correct: true, Project: "6.9"},
		{Case: "g", Err: errors.New("cannot decide"), Project: "2.0"},
	}
	rep := scoreEval(results)
	assert.Equal(t, 7, rep.Total)
	assert.Equal(t, 4, rep.Correct)
	assert.Equal(t, 2, rep.Errors)
	assert.Equal(t, 6*time.Second, rep.Duration)
	assert.InDelta(t, 4.0/7, rep.Accuracy(), 1e-9)
	assert.Equal(t, 2, rep.Levels[LevelCLI].Correct[9])
	assert.Equal(t, 1, rep.Levels[LevelModel].Correct[9], "1.0 falls in the top decile")
	assert.Equal(t, 1, rep.Levels[LevelModel].Wrong[4])
	assert.Equal(t, "accuracy 4/7 (57.1%), 2 error(s), latency 6s total, 857ms per case\n"+
		"cli confidence (conditional)  correct  wrong\n"+
		"  0.9-1.0         2      0\n"+
		"model confidence (conditional)  correct  wrong\n"+
		"  0.4-0.5         1      1\n"+
		"  0.9-1.0         1      0\n"+
		"effort confidence (conditional)  correct  wrong\n"+
		"  0.3-0.4         1      0\n", rep.String())
	assert.Zero(t, evalReport{}.Accuracy())
	assert.Equal(t, "accuracy 0/0 (0.0%), 0 error(s), latency 0s total\n", evalReport{}.String())

	assert.Equal(t, []string{
		"ERROR e: timeout in 3s",
		"ERROR g: cannot decide in 0s",
		"WRONG c: y, cli 0.970, model 0.420 in 2s",
		"ok    a: x, cli 0.950, model 0.450 in 1s",
		"ok    b: x, model 1.000 in 0s",
		"ok    d: x, effort 0.300 (split) in 0s",
		"ok    f: x in 0s, project 6.9",
	}, resultLines(results))
}
