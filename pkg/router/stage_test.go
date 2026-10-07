package router

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

// stageAsked is the routing stage question of req and its id; ok is false for a doc request.
func stageAsked(req jev.Request) (string, jev.Question, bool) {
	for _, lv := range routeLevels {
		if q, ok := req.Questions[lv.name]; ok {
			return lv.name, q, true
		}
	}
	return "", jev.Question{}, false
}

// stageChoice is pick's criterion name in req's stage question (see pickAt), or the first criterion
// when pick has none there.
func stageChoice(req jev.Request, pick string) (string, jev.Question, string) {
	id, q, _ := stageAsked(req)
	choice := pickAt(id, q, pick)
	if !slices.Contains(q.Criteria.Names(), choice) {
		choice = q.Criteria.Names()[0]
	}
	return id, q, choice
}

// certain answers req's stage question choosing pick's criterion with certainty.
func certain(req jev.Request, pick string) map[string]jev.Answer {
	id, q, choice := stageChoice(req, pick)
	probs := map[string]float64{}
	for _, name := range q.Criteria.Names() {
		probs[name] = 0
	}
	probs[choice] = 1
	return map[string]jev.Answer{id: {Type: jev.TypeChoice, Choice: choice, Probabilities: probs, Confidence: 0.9}}
}

// choosing returns a mock answering every stage request with pick's criterion, with certainty.
func choosing(pick string) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		return certain(req, pick), nil
	}}
}

// favorStage answers req's stage question with 0.9 on pick's criterion and the rest evenly over the
// others; evenly over all, choosing the first, when pick has no criterion there.
func favorStage(req jev.Request, pick string, confidence float64) (string, jev.Answer) {
	id, q, choice := stageChoice(req, pick)
	names := q.Criteria.Names()
	probs := make(map[string]float64, len(names))
	if pickAt(id, q, pick) != choice {
		for _, name := range names {
			probs[name] = 1 / float64(len(names))
		}
	} else {
		for _, name := range names {
			probs[name] = 0.1 / float64(len(names)-1)
		}
		probs[choice] = 0.9
	}
	return id, jev.Answer{Type: jev.TypeChoice, Choice: choice, Probabilities: probs, Confidence: confidence}
}

// stageAnswers are a chunk request's answers: favorStage and the relevance.
func stageAnswers(req jev.Request, pick string, confidence, relevance float64) map[string]jev.Answer {
	id, a := favorStage(req, pick, confidence)
	return map[string]jev.Answer{id: a, questionRelevance: {Type: jev.TypeNoul, Noul: relevance}}
}

// askedLevels lists the stage of every routing request, in call order.
func askedLevels(client *mocks.JevClientMock) []string {
	var out []string
	for _, c := range client.AskCalls() {
		if id, _, ok := stageAsked(c.Req); ok {
			out = append(out, id)
		}
	}
	return out
}

// stageSummary drops the answers, keeping what each stage decided.
func stageSummary(stages []Stage) []Stage {
	out := make([]Stage, len(stages))
	for i, st := range stages {
		out[i] = Stage{Level: st.Level, Choice: st.Choice, Skipped: st.Skipped}
	}
	return out
}

func TestGroups(t *testing.T) {
	cfg, cat := requestFixture(t)
	optsOf := func(req *args.Request) []catalog.Option { return Eligible(cfg, cat, req).Options }
	tests := []struct {
		name   string
		opts   []catalog.Option
		level  level
		labels []string
		ids    [][]string
	}{
		{name: "cli over the catalog", opts: cat.Options, level: routeLevels[0], labels: []string{"alpha", "beta"},
			ids: [][]string{{"strong@low", "strong@high", "fast"}, {"worker@low", "worker@high"}}},
		{name: "model within one CLI", opts: catalog.ByCLI(cat.Options, "alpha"), level: routeLevels[1],
			labels: []string{"strong", "fast"}, ids: [][]string{{"strong@low", "strong@high"}, {"fast"}}},
		{name: "effort within one model", opts: catalog.ByModel(cat.Options, "strong"), level: routeLevels[2],
			labels: []string{"low", "high"}, ids: [][]string{{"strong@low"}, {"strong@high"}}},
		{name: "a model without efforts is one effort group", opts: catalog.ByModel(cat.Options, "fast"),
			level: routeLevels[2], labels: []string{""}, ids: [][]string{{"fast"}}},
		{name: "model passthrough: one model group named by the model",
			opts:  optsOf(&args.Request{Model: "unknown-model", ModelSource: args.SourceFlag}),
			level: routeLevels[1], labels: []string{"unknown-model"}, ids: [][]string{{"alpha", "beta"}}},
		{name: "model passthrough: one CLI group each",
			opts:  optsOf(&args.Request{Model: "unknown-model", ModelSource: args.SourceFlag}),
			level: routeLevels[0], labels: []string{"alpha", "beta"}, ids: [][]string{{"alpha"}, {"beta"}}},
		{name: "effort passthrough: one effort group per model",
			opts:  optsOf(&args.Request{CLI: "alpha", Effort: "turbo", EffortSource: args.SourceFlag}),
			level: routeLevels[2], labels: []string{"", ""}, ids: [][]string{{"strong"}, {"fast"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := groups(tc.opts, tc.level)
			assert.Equal(t, tc.labels, groupLabels(gs))
			got := make([][]string, len(gs))
			for i, g := range gs {
				got[i] = ids(g.opts)
			}
			assert.Equal(t, tc.ids, got)
		})
	}
}

func TestRouteStagesFixedByPassedValues(t *testing.T) {
	cfg, cat := requestFixture(t)
	stage := func(level, choice string, skipped bool) Stage {
		return Stage{Level: level, Choice: choice, Skipped: skipped}
	}
	tests := []struct {
		name   string
		req    *args.Request
		pick   string
		asked  []string
		stages []Stage
		want   string // the option chosen
		warned bool
	}{
		{name: "nothing passed: every stage asked", req: &args.Request{}, pick: "strong@high",
			asked:  []string{levelCLI, levelModel, levelEffort},
			stages: []Stage{stage("cli", "alpha", false), stage("model", "strong", false), stage("effort", "high", false)},
			want:   "strong@high"},
		{name: "a model without efforts skips the effort stage", req: &args.Request{}, pick: "fast",
			asked:  []string{levelCLI, levelModel},
			stages: []Stage{stage("cli", "alpha", false), stage("model", "fast", false), stage("effort", "", true)},
			want:   "fast"},
		{name: "--cli: only model and effort asked", req: &args.Request{CLI: "alpha"}, pick: "strong@low",
			asked:  []string{levelModel, levelEffort},
			stages: []Stage{stage("cli", "alpha", true), stage("model", "strong", false), stage("effort", "low", false)},
			want:   "strong@low"},
		{name: "catalog --model: only effort asked", req: &args.Request{Model: "strong-model", ModelSource: args.SourceFlag},
			pick: "strong@high", asked: []string{levelEffort},
			stages: []Stage{stage("cli", "alpha", true), stage("model", "strong", true), stage("effort", "high", false)},
			want:   "strong@high"},
		{name: "passthrough --model: only cli asked", req: &args.Request{Model: "unknown-model", ModelSource: args.SourceFlag},
			pick: "beta", asked: []string{levelCLI},
			stages: []Stage{stage("cli", "beta", false), stage("model", "unknown-model", true), stage("effort", "", true)},
			want:   "beta"},
		{name: "passthrough --model and --effort: the effort passes through",
			req:  &args.Request{Model: "unknown-model", ModelSource: args.SourceFlag, Effort: "turbo", EffortSource: args.SourceFlag},
			pick: "alpha", asked: []string{levelCLI},
			stages: []Stage{stage("cli", "alpha", false), stage("model", "unknown-model", true), stage("effort", "turbo", true)},
			want:   "alpha"},
		{name: "--effort: effort never asked", req: &args.Request{Effort: "high", EffortSource: args.SourceFlag},
			pick: "worker@high", asked: []string{levelCLI},
			stages: []Stage{stage("cli", "beta", false), stage("model", "worker", true), stage("effort", "high", true)},
			want:   "worker@high"},
		{name: "effort passthrough: model asked, the passed effort recorded",
			req:  &args.Request{CLI: "alpha", Effort: "turbo", EffortSource: args.SourceFlag},
			pick: "strong", asked: []string{levelModel},
			stages: []Stage{stage("cli", "alpha", true), stage("model", "strong", false), stage("effort", "turbo", true)},
			want:   "strong"},
		{name: "all three passed: Jev not asked",
			req: &args.Request{CLI: "alpha", Model: "strong-model", ModelSource: args.SourceFlag, Effort: "high",
				EffortSource: args.SourceFlag},
			stages: []Stage{stage("cli", "alpha", true), stage("model", "strong", true), stage("effort", "high", true)},
			want:   "strong@high"},
		{name: "an unusable --cli fixes nothing", req: &args.Request{CLI: "nope"}, pick: "worker@low",
			asked:  []string{levelCLI, levelEffort},
			stages: []Stage{stage("cli", "beta", false), stage("model", "worker", true), stage("effort", "low", false)},
			want:   "worker@low", warned: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := choosing(tc.pick)
			if tc.pick == "" {
				client = &mocks.JevClientMock{} // Ask panics if called
			}
			el := Eligible(cfg, cat, tc.req)
			assert.Equal(t, tc.warned, len(el.Skipped) > 0)
			d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, tc.req, captured("fix it"))
			require.NoError(t, err)
			require.NoError(t, d.Undecided)
			assert.Equal(t, tc.asked, askedLevels(client))
			assert.Equal(t, tc.stages, stageSummary(d.Stages))
			assert.Equal(t, tc.want, d.OptionID)
			assert.Equal(t, tc.req.CLI == "alpha", d.Pinned, "only an accepted --cli pins")
		})
	}
}

func TestRoutePassedValuesInStageCriteria(t *testing.T) {
	cfg, cat := requestFixture(t)
	t.Run("passthrough --model: every cli criterion carries the model", func(t *testing.T) {
		req := &args.Request{Model: "unknown-model", ModelSource: args.SourceFlag}
		client := choosing("beta")
		_, err := newRouter(t, cfg, cat, client).Route(context.Background(), Eligible(cfg, cat, req), req, captured("x"))
		require.NoError(t, err)
		in := client.AskCalls()[0].Req.Questions[levelCLI].Instructions.(stageInstructions)
		require.Equal(t, []string{"alpha", "beta"}, in.CLIs.Names())
		for _, c := range in.CLIs {
			models := c.Value.(cliEntry).Models
			assert.Equal(t, []string{"unknown-model"}, models.Names(), c.Name)
			assert.Equal(t, modelEntry{Description: descModelPassed, Efforts: []string{effortDefault}}, models[0].Value)
		}
	})
	t.Run("--effort passthrough: every model criterion carries the effort", func(t *testing.T) {
		req := &args.Request{CLI: "alpha", Effort: "turbo", EffortSource: args.SourceFlag}
		client := choosing("strong")
		_, err := newRouter(t, cfg, cat, client).Route(context.Background(), Eligible(cfg, cat, req), req, captured("x"))
		require.NoError(t, err)
		in := client.AskCalls()[0].Req.Questions[levelModel].Instructions.(stageInstructions)
		require.Equal(t, []string{"strong", "fast"}, in.Models.Names())
		for _, m := range in.Models {
			assert.Equal(t, []string{"turbo"}, m.Value.(modelEntry).Efforts, m.Name)
		}
		assert.Equal(t, jev.Criteria{{Name: "alpha", Value: jev.Criteria{{Name: "turbo", Value: descEffortPassed}}}}, in.Efforts)
	})
}

func TestRouteStageSequence(t *testing.T) {
	cfg, cat := requestFixture(t)
	client := choosing("strong@high")
	r := newRouter(t, cfg, cat, client)
	req := &args.Request{}
	el := Eligible(cfg, cat, req)
	before := slices.Clone(el.Options)

	d, err := r.Route(context.Background(), el, req, captured("fix it"))
	require.NoError(t, err)
	assert.Equal(t, Decision{CLI: "alpha", Model: "strong-model", Effort: "high", OptionID: "strong@high"},
		Decision{CLI: d.CLI, Model: d.Model, Effort: d.Effort, OptionID: d.OptionID, Pinned: d.Pinned},
		"a CLI Jev chose is not pinned")
	assert.Equal(t, before, el.Options, "the caller's eligibility is not narrowed")

	calls := client.AskCalls()
	require.Len(t, calls, 3)
	wantCriteria := []jev.Criteria{
		{{Name: "alpha", Value: stageCriterion{CLI: "alpha"}}, {Name: "beta", Value: stageCriterion{CLI: "beta"}}},
		{{Name: "strong", Value: stageCriterion{Model: "strong-model", CLI: "alpha"}},
			{Name: "fast", Value: stageCriterion{Model: "fast-model", CLI: "alpha"}}},
		{{Name: "low", Value: stageCriterion{Effort: "low"}}, {Name: "high", Value: stageCriterion{Effort: "high"}}},
	}
	wantText := []string{cliText, modelText, effortText}
	for i, lv := range routeLevels {
		sent := calls[i].Req
		require.Len(t, sent.Questions, 1, lv.name)
		q, ok := sent.Questions[lv.name]
		require.True(t, ok, "question id is the level name")
		assert.Equal(t, prompt.State{Prompt: "fix it"}, sent.State)
		assert.Equal(t, wantCriteria[i], q.Criteria, lv.name)
		in := q.Instructions.(stageInstructions)
		assert.Equal(t, wantText[i], in.Question)
		assert.Equal(t, wholeGuide, in.State)
		assert.Equal(t, cfg.Agrouter.RoutingPolicy, in.Policy)
		require.Len(t, d.Stages, 3)
		assert.Equal(t, lv.name, d.Stages[i].Level)
		require.NotNil(t, d.Stages[i].Answer)
		assert.Equal(t, d.Stages[i].Choice, d.Stages[i].Answer.Choice)
	}

	cliIn := calls[0].Req.Questions[levelCLI].Instructions.(stageInstructions)
	assert.Nil(t, cliIn.Models, "the cli stage nests the models under each CLI")
	assert.Equal(t, jev.Criteria{
		{Name: "alpha", Value: cliEntry{Description: "Alpha agent.", Models: jev.Criteria{
			{Name: "strong", Value: modelEntry{Description: "Strong model.", Efforts: []string{"low", "high"}}},
			{Name: "fast", Value: modelEntry{Description: "Fast model.", Efforts: []string{effortNone}}},
		}}},
		{Name: "beta", Value: cliEntry{Description: "Beta agent.", Models: jev.Criteria{
			{Name: "worker", Value: modelEntry{Description: "Worker model.", Efforts: []string{"low", "high"}}},
		}}},
	}, cliIn.CLIs)
	assert.Equal(t, []string{"alpha", "beta"}, cliIn.Efforts.Names())
	effortIn := calls[2].Req.Questions[levelEffort].Instructions.(stageInstructions)
	assert.Nil(t, effortIn.CLIs)
	assert.Equal(t, []string{"strong"}, effortIn.Models.Names(), "only the chosen model")
	assert.Equal(t, jev.Criteria{{Name: "alpha", Value: jev.Criteria{
		{Name: "low", Value: "Quick reasoning."}, {Name: "high", Value: "Deep reasoning."}}}}, effortIn.Efforts)
}

// failingAt answers with pick until the stage asked under level, which fails with err.
func failingAt(level, pick string, err error) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		if id, _, _ := stageAsked(req); id == level {
			return nil, err
		}
		if _, ok := req.State.(prompt.ChunkState); ok {
			return stageAnswers(req, pick, 0.9, 0.5), nil
		}
		return certain(req, pick), nil
	}}
}

func TestRouteStageFailure(t *testing.T) {
	cfg, cat := requestFixture(t)
	boom := &jev.StatusError{Status: 500}
	t.Run("several CLIs: the cli stage succeeds, the model stage fails", func(t *testing.T) {
		req := &args.Request{}
		client := failingAt(levelModel, "strong@high", boom)
		d, err := newRouter(t, cfg, cat, client).Route(context.Background(), Eligible(cfg, cat, req), req, captured("x"))
		require.ErrorIs(t, err, ErrCannotDecide)
		require.ErrorIs(t, err, boom)
		assert.Contains(t, err.Error(), "model stage")
		assert.Equal(t, []Stage{{Level: "cli", Choice: "alpha"}}, stageSummary(d.Stages), "completed stages kept")
		assert.Empty(t, d.CLI, "the partial decision is discarded")
		assert.Empty(t, d.OptionID)
		assert.False(t, d.Pinned, "a CLI Jev chose never pins")
	})
	t.Run("several CLIs: the model stage succeeds, the effort stage fails", func(t *testing.T) {
		req := &args.Request{}
		client := failingAt(levelEffort, "strong@high", boom)
		d, err := newRouter(t, cfg, cat, client).Route(context.Background(), Eligible(cfg, cat, req), req, captured("x"))
		require.ErrorIs(t, err, ErrCannotDecide)
		assert.Equal(t, []Stage{{Level: "cli", Choice: "alpha"}, {Level: "model", Choice: "strong"}}, stageSummary(d.Stages))
		assert.Empty(t, d.CLI)
	})
	t.Run("one CLI eligible: runs with the caller's fixed values only", func(t *testing.T) {
		req := &args.Request{Model: "strong-model", ModelSource: args.SourceFlag}
		client := failingAt(levelEffort, "strong@high", boom)
		d, err := newRouter(t, cfg, cat, client).Route(context.Background(), Eligible(cfg, cat, req), req, captured("x"))
		require.NoError(t, err)
		require.ErrorIs(t, d.Undecided, boom)
		assert.Equal(t, "alpha", d.CLI)
		assert.Equal(t, "strong-model", d.Model)
		assert.Empty(t, d.Effort, "the effort Jev never chose")
		assert.Empty(t, d.OptionID)
		assert.False(t, d.Pinned)
		assert.Equal(t, []Stage{{Level: "cli", Choice: "alpha", Skipped: true}, {Level: "model", Choice: "strong", Skipped: true}},
			stageSummary(d.Stages))
	})
}

func TestRouteStagesSplitState(t *testing.T) {
	cfg, cat := requestFixture(t)
	c := captured(filler(200_000))
	// chunk 1 favors worker@high, every other chunk strong@low, each with its relevance
	mixed := func(first, rest float64) func(context.Context, jev.Request) (map[string]jev.Answer, error) {
		return func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
			st, ok := req.State.(prompt.ChunkState)
			if !ok {
				return nil, &jev.StatusError{Status: 500}
			}
			if st.Chunk.Index == 1 {
				return stageAnswers(req, "worker@high", 0.6, first), nil
			}
			return stageAnswers(req, "strong@low", 0.8, rest), nil
		}
	}
	route := func(t *testing.T, client *mocks.JevClientMock) (Decision, int, error) {
		t.Helper()
		r := newRouter(t, cfg, cat, client)
		split, err := c.Split(r.budget)
		require.NoError(t, err)
		require.NotNil(t, split)
		require.Greater(t, len(split.Chunks), 2)
		req := &args.Request{}
		d, err := r.Route(context.Background(), Eligible(cfg, cat, req), req, c)
		return d, len(split.Chunks), err
	}

	t.Run("pooled per level by relevance; every chunk narrowed the same", func(t *testing.T) {
		client := &mocks.JevClientMock{AskFunc: mixed(0.9, 0.01)}
		d, n, err := route(t, client)
		require.NoError(t, err)
		assert.Equal(t, "worker@high", d.OptionID, "the relevant chunk wins every level")
		assert.Equal(t, []Stage{{Level: "cli", Choice: "beta"}, {Level: "model", Choice: "worker", Skipped: true},
			{Level: "effort", Choice: "high"}}, stageSummary(d.Stages))
		for _, i := range []int{0, 2} {
			require.NotNil(t, d.Stages[i].Pooled, d.Stages[i].Level)
			assert.Nil(t, d.Stages[i].Answer)
			assert.Len(t, d.Stages[i].Pooled.Chunks, n)
		}
		assert.Equal(t, []string{"alpha", "beta"}, scoreIDs(d.Stages[0].Pooled.Scores), "every criterion, in order")
		assert.Equal(t, "beta", d.Stages[0].Pooled.Top[0].ID)
		calls := client.AskCalls()
		require.Len(t, calls, 2*n)
		perLevel := map[string][]int{}
		for _, call := range calls {
			id, q, _ := stageAsked(call.Req)
			assert.Equal(t, jev.TypeNoul, call.Req.Questions[questionRelevance].Type, "relevance asked at every level")
			perLevel[id] = append(perLevel[id], call.Req.State.(prompt.ChunkState).Chunk.Index)
			if id == levelEffort {
				assert.Equal(t, []string{"low", "high"}, q.Criteria.Names(), "every chunk asked within the pooled winner")
			}
		}
		for id, idx := range perLevel {
			slices.Sort(idx)
			assert.Len(t, idx, n, id)
			assert.Equal(t, 1, idx[0], id)
			assert.Equal(t, n, idx[n-1], id)
		}
	})
	t.Run("all-zero relevance: a plain mean, so the other chunks outvote the first", func(t *testing.T) {
		d, _, err := route(t, &mocks.JevClientMock{AskFunc: mixed(0, 0)})
		require.NoError(t, err)
		assert.Equal(t, "alpha", d.Stages[0].Choice)
		assert.Equal(t, "strong@low", d.OptionID)
	})
	t.Run("a chunk failing at a later level fails the routing, earlier stages kept", func(t *testing.T) {
		client := &mocks.JevClientMock{AskFunc: func(ctx context.Context, req jev.Request) (map[string]jev.Answer, error) {
			if id, _, _ := stageAsked(req); id == levelEffort && req.State.(prompt.ChunkState).Chunk.Index == 2 {
				return nil, &jev.StatusError{Status: 500}
			}
			return mixed(0.9, 0.01)(ctx, req)
		}}
		d, _, err := route(t, client)
		require.ErrorIs(t, err, ErrCannotDecide)
		assert.Contains(t, err.Error(), "effort stage")
		assert.Equal(t, []Stage{{Level: "cli", Choice: "beta"}, {Level: "model", Choice: "worker", Skipped: true}},
			stageSummary(d.Stages))
		assert.NotNil(t, d.Stages[0].Pooled)
	})
}

func scoreIDs(scores []Score) []string {
	out := make([]string, len(scores))
	for i, s := range scores {
		out[i] = s.ID
	}
	return out
}

func TestLargestStage(t *testing.T) {
	cfg, cat := requestFixture(t)
	size := func(lv level, opts []catalog.Option) int {
		return questionLen(lv.name, stageQuestion(cfg, lv, wholeGuide, groups(opts, lv), ""))
	}
	want := max(
		size(routeLevels[0], cat.Options),
		size(routeLevels[1], catalog.ByCLI(cat.Options, "alpha")),
		size(routeLevels[1], catalog.ByCLI(cat.Options, "beta")),
		size(routeLevels[2], catalog.ByModel(cat.Options, "strong")),
		size(routeLevels[2], catalog.ByModel(cat.Options, "worker")),
	)
	assert.Equal(t, want, largestStage(cfg, cat, wholeGuide))
	assert.Greater(t, largestStage(cfg, cat, chunkGuide), largestStage(cfg, cat, wholeGuide),
		"the chunk guide is the longer one")
}

// pickHigh is the option the stage edge-case tests route to: each of its stages has another choice.
const pickHigh = "strong@high"

// decidedStages are pickHigh's asked stages, in order.
var decidedStages = []Stage{{Level: levelCLI, Choice: "alpha"}, {Level: levelModel, Choice: "strong"},
	{Level: levelEffort, Choice: "high"}}

func TestRouteStageWhole422Resplits(t *testing.T) {
	cfg, cat := requestFixture(t)
	c := captured("HARD: rename the package\n" + filler(40_000))
	for i, lv := range routeLevels {
		t.Run(lv.name, func(t *testing.T) {
			client := &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
				if _, ok := req.State.(prompt.ChunkState); ok {
					return stageAnswers(req, pickHigh, 0.8, 0.5), nil
				}
				if id, _, _ := stageAsked(req); id == lv.name {
					return nil, &jev.StatusError{Status: 422}
				}
				return certain(req, pickHigh), nil
			}}
			r := newRouter(t, cfg, cat, client)
			require.True(t, c.Fits(r.budget))
			req := &args.Request{}
			d, err := r.Route(context.Background(), Eligible(cfg, cat, req), req, c)
			require.NoError(t, err)
			require.NoError(t, d.Undecided)
			assert.Equal(t, pickHigh, d.OptionID)
			assert.Equal(t, decidedStages, stageSummary(d.Stages))
			for j, st := range d.Stages {
				if j < i {
					assert.NotNil(t, st.Answer, "%s asked whole before the 422", st.Level)
					assert.Nil(t, st.Pooled, st.Level)
					continue
				}
				assert.Nil(t, st.Answer, st.Level)
				require.NotNil(t, st.Pooled, "%s pooled from the rejected level on", st.Level)
				assert.Greater(t, len(st.Pooled.Chunks), 1, st.Level)
			}
			whole := 0
			for _, call := range client.AskCalls() {
				st, ok := call.Req.State.(prompt.ChunkState)
				if !ok {
					whole++
					continue
				}
				size, err := json.Marshal(st)
				require.NoError(t, err)
				assert.LessOrEqual(t, prompt.Tokens(len(size)), r.budget.Chunk/2, "split at half the budget")
			}
			assert.Equal(t, i+1, whole, "the whole state is sent until it is rejected, never after")
		})
	}
}

func TestRouteStageChunk422(t *testing.T) {
	cfg, cat := requestFixture(t)
	c := captured("START " + filler(200_000))
	split, err := c.Split(newRouter(t, cfg, cat, &mocks.JevClientMock{}).budget)
	require.NoError(t, err)
	require.NotNil(t, split)
	n := len(split.Chunks)
	pieces := len(prompt.Halve(split.Chunks[0]))
	require.Greater(t, pieces, 1)
	// rejectingAt answers every chunk favoring pickHigh, but rejects with a 422 the model stage's
	// chunks that reject picks
	rejectingAt := func(reject func(prompt.Chunk) bool) *mocks.JevClientMock {
		return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
			st, ok := req.State.(prompt.ChunkState)
			if !ok {
				return nil, &jev.StatusError{Status: 500}
			}
			if id, _, _ := stageAsked(req); id == levelModel && reject(st.Chunk) {
				return nil, &jev.StatusError{Status: 422}
			}
			return stageAnswers(req, pickHigh, 0.8, 0.5), nil
		}}
	}
	req := &args.Request{}

	t.Run("only the rejected chunk re-splits; the pool covers every chunk", func(t *testing.T) {
		client := rejectingAt(func(ch prompt.Chunk) bool { return ch.Index == 1 && ch.Of == n })
		d, err := newRouter(t, cfg, cat, client).Route(context.Background(), Eligible(cfg, cat, req), req, c)
		require.NoError(t, err)
		require.NoError(t, d.Undecided)
		assert.Equal(t, pickHigh, d.OptionID)
		assert.Equal(t, decidedStages, stageSummary(d.Stages))
		want := map[string]int{levelCLI: n, levelModel: n - 1 + pieces, levelEffort: n}
		for _, st := range d.Stages {
			require.NotNil(t, st.Pooled, st.Level)
			assert.Len(t, st.Pooled.Chunks, want[st.Level], "%s: every level starts from the original chunks", st.Level)
		}
		assert.Len(t, stageCalls(client, levelModel), n+pieces, "every chunk, then the rejected one's pieces")
	})
	t.Run("a second 422 fails the routing with earlier stages kept", func(t *testing.T) {
		client := rejectingAt(func(ch prompt.Chunk) bool { return strings.HasPrefix(ch.Text, "START") })
		d, err := newRouter(t, cfg, cat, client).Route(context.Background(), Eligible(cfg, cat, req), req, c)
		require.ErrorIs(t, err, ErrCannotDecide)
		require.ErrorIs(t, err, errUnsplittable)
		assert.Contains(t, err.Error(), "model stage")
		assert.Equal(t, decidedStages[:1], stageSummary(d.Stages))
		assert.Empty(t, stageCalls(client, levelEffort))
	})
}

func TestRouteStageDeadline(t *testing.T) {
	synctest.Test(t, testRouteStageDeadline)
}

func testRouteStageDeadline(t *testing.T) {
	cfg, cat := requestFixture(t)
	client := &mocks.JevClientMock{AskFunc: func(ctx context.Context, req jev.Request) (map[string]jev.Answer, error) {
		if id, _, _ := stageAsked(req); id == levelCLI {
			time.Sleep(2 * time.Second) // the cli stage answers just after the deadline passes
			return certain(req, pickHigh), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := &args.Request{}
	d, err := newRouter(t, cfg, cat, client).Route(ctx, Eligible(cfg, cat, req), req, captured("fix it"))
	require.ErrorIs(t, err, ErrCannotDecide)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "model stage")
	assert.Equal(t, []string{levelCLI, levelModel}, askedLevels(client))
	assert.Equal(t, decidedStages[:1], stageSummary(d.Stages), "the completed cli stage is kept")
	assert.Empty(t, d.CLI)
}

func TestRouteStageMalformedAnswers(t *testing.T) {
	cfg, cat := requestFixture(t)
	// unchosen is a criterion other than pickHigh's at each level; at the effort level, an effort label
	unchosen := map[string]string{levelCLI: "beta", levelModel: "fast", levelEffort: "low"}
	corruptions := []struct {
		name    string
		corrupt func(id string, answers map[string]jev.Answer)
	}{
		{name: "a choice not among the criteria sent", corrupt: func(id string, answers map[string]jev.Answer) {
			a := answers[id]
			a.Choice = "nope"
			answers[id] = a
		}},
		{name: "a missing probability", corrupt: func(id string, answers map[string]jev.Answer) {
			a := answers[id]
			a.Probabilities = maps.Clone(a.Probabilities)
			delete(a.Probabilities, unchosen[id])
			answers[id] = a
		}},
		{name: "a missing answer id", corrupt: func(id string, answers map[string]jev.Answer) {
			delete(answers, id)
		}},
	}
	states := []struct {
		name     string
		captured *prompt.Result
	}{
		{name: "whole", captured: captured("fix it")},
		{name: "split", captured: captured(filler(200_000))},
	}
	for _, state := range states {
		for _, cr := range corruptions {
			for i, lv := range routeLevels {
				t.Run(state.name+"/"+cr.name+"/"+lv.name, func(t *testing.T) {
					client := &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
						st, chunk := req.State.(prompt.ChunkState)
						answers := certain(req, pickHigh)
						if chunk {
							answers = stageAnswers(req, pickHigh, 0.8, 0.5)
						}
						// a split state corrupts one chunk's answers only
						if id, _, _ := stageAsked(req); id == lv.name && (!chunk || st.Chunk.Index == 2) {
							cr.corrupt(id, answers)
						}
						return answers, nil
					}}
					req := &args.Request{}
					d, err := newRouter(t, cfg, cat, client).Route(context.Background(), Eligible(cfg, cat, req), req,
						state.captured)
					require.ErrorIs(t, err, ErrCannotDecide)
					require.ErrorIs(t, err, jev.ErrMalformed)
					assert.Contains(t, err.Error(), lv.name+" stage")
					assert.Equal(t, decidedStages[:i], stageSummary(d.Stages))
				})
			}
		}
	}
}

func TestBudgetLargestStage(t *testing.T) {
	// sizes are each level's largest whole-state question over cat, as budget measures them
	sizes := func(cfg *config.Config, cat *catalog.Catalog) map[string]int {
		size := func(lv level, opts []catalog.Option) int {
			return questionLen(lv.name, stageQuestion(cfg, lv, wholeGuide, groups(opts, lv), ""))
		}
		out := map[string]int{levelCLI: size(routeLevels[0], cat.Options)}
		for _, cli := range catalog.CLIs(cat.Options) {
			out[levelModel] = max(out[levelModel], size(routeLevels[1], catalog.ByCLI(cat.Options, cli)))
		}
		for _, g := range groups(cat.Options, routeLevels[1]) {
			out[levelEffort] = max(out[levelEffort], size(routeLevels[2], g.opts))
		}
		return out
	}
	long := strings.Repeat("x", 3_000)
	tests := []struct {
		name    string
		mutate  func(cfg *config.Config)
		largest string
	}{
		// only the model stage's criteria carry the model name
		{name: "a long model name makes the model stage the largest", largest: levelModel,
			mutate: func(cfg *config.Config) { cfg.Models[2].Name = long }},
		// the effort stage repeats its labels in the criteria names and values
		{name: "a long effort label makes the effort stage the largest", largest: levelEffort,
			mutate: func(cfg *config.Config) {
				cfg.Models[2].Efforts = []string{"low", long}
				cfg.Efforts["beta."+long] = config.Effort{CLI: "beta", Level: long, Description: "Very deep reasoning."}
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := requestFixture(t)
			tc.mutate(cfg)
			cat, err := catalog.Build(cfg)
			require.NoError(t, err)
			s := sizes(cfg, cat)
			for level, n := range s {
				if level != tc.largest {
					assert.Greater(t, s[tc.largest], n, "%s over %s", tc.largest, level)
				}
			}
			assert.Equal(t, s[tc.largest], largestStage(cfg, cat, wholeGuide))

			b, err := budget(cfg, cat)
			require.NoError(t, err)
			cliOnly, err := prompt.NewBudget(prompt.Questions{Route: s[levelCLI], ChunkRoute: 1, Relevance: 1,
				Complexity: 1, Evidence: 1})
			require.NoError(t, err)
			assert.Less(t, b.State, cliOnly.State, "the whole-state budget leaves room for the largest stage")
		})
	}
}

func TestBudgetPolicyOverBudget(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	tests := []struct {
		name   string
		mutate func(ag *config.Agrouter)
		want   []string
		absent string
	}{
		{name: "routing_policy", mutate: func(ag *config.Agrouter) { ag.RoutingPolicy = long },
			want: []string{"routing_policy leaves", "for the state", "beside the anchor"}, absent: "complexity_policy"},
		{name: "complexity_policy", mutate: func(ag *config.Agrouter) { ag.ComplexityPolicy = long },
			want: []string{"complexity_policy leaves", "for the doc state"}, absent: "routing_policy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, cat := requestFixture(t)
			tc.mutate(&cfg.Agrouter)
			_, err := New(cfg, cat, &mocks.JevClientMock{})
			require.ErrorIs(t, err, prompt.ErrQuestionsOverBudget)
			assert.Contains(t, err.Error(), "config: [agrouter]")
			for _, w := range tc.want {
				assert.Contains(t, err.Error(), w)
			}
			assert.NotContains(t, err.Error(), tc.absent)
		})
	}
}
