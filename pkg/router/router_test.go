package router

import (
	"context"
	"encoding/json"
	goflag "flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

var update = goflag.Bool("update", false, "rewrite golden files")

// requestFixture keeps request-format goldens independent of the shipped catalog and defaults.
func requestFixture(t *testing.T) (*config.Config, *catalog.Catalog) {
	t.Helper()
	cfg := &config.Config{
		Agrouter: config.Agrouter{JevModel: "test-jev", Timeout: time.Second,
			Question: "Choose an option for state.", ChunkQuestion: "Choose an option for anchor and chunk.",
			Relevance:          "Does chunk add requirements beyond anchor?",
			ComplexityQuestion: "Rate the project the docs describe.",
			ComplexityEvidence: "Does the text describe the project?"},
		CLIs: []config.CLI{
			{Name: "alpha", Command: "alpha", Description: "Alpha agent."},
			{Name: "beta", Command: "beta", Description: "Beta agent."},
		},
		Models: []config.Model{
			{Section: "strong", CLI: "alpha", Name: "strong-model", Efforts: []string{"low", "high"}, Description: "Strong model."},
			{Section: "fast", CLI: "alpha", Name: "fast-model", Description: "Fast model."},
			{Section: "worker", CLI: "beta", Name: "worker-model", Efforts: []string{"low", "high"}, Description: "Worker model."},
		},
		Efforts: map[string]config.Effort{
			"alpha.low":  {CLI: "alpha", Level: "low", Description: "Quick reasoning."},
			"alpha.high": {CLI: "alpha", Level: "high", Description: "Deep reasoning."},
			"beta.low":   {CLI: "beta", Level: "low", Description: "Quick reasoning."},
			"beta.high":  {CLI: "beta", Level: "high", Description: "Deep reasoning."},
		},
	}
	cat, err := catalog.Build(cfg)
	require.NoError(t, err)
	return cfg, cat
}

func newRouter(t *testing.T, cfg *config.Config, cat *catalog.Catalog, client JevClient) *Router {
	t.Helper()
	r, err := New(cfg, cat, client)
	require.NoError(t, err)
	return r
}

// answering returns a mock that chooses id with certainty.
func answering(id string) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: func(context.Context, jev.Request) (map[string]jev.Answer, error) {
		return map[string]jev.Answer{questionRoute: {Type: jev.TypeChoice, Choice: id,
			Probabilities: map[string]float64{id: 1}, Confidence: 0.9}}, nil
	}}
}

func failing(err error) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: func(context.Context, jev.Request) (map[string]jev.Answer, error) {
		return nil, err
	}}
}

func captured(text string) *prompt.Result {
	return &prompt.Result{Prompt: text}
}

func TestRouteSingleOptionSkipsJev(t *testing.T) {
	cfg, cat := embedded(t)
	client := &mocks.JevClientMock{} // Ask panics if called
	r := newRouter(t, cfg, cat, client)

	req := &args.Request{Model: "claude-opus-5-5", ModelSource: args.SourceFlag, Effort: "high", EffortSource: args.SourceFlag}
	el := Eligible(cfg, cat, req)
	require.Len(t, el.Options, 1)

	d, err := r.Route(context.Background(), el, req, captured("fix the flaky test"))
	require.NoError(t, err)
	assert.Equal(t, Decision{CLI: "claude", Model: "claude-opus-5-5", Effort: "high", OptionID: "claude-opus-5-5@high"}, d)
	assert.Empty(t, client.AskCalls())
}

func TestRouteSingleOptionPassthrough(t *testing.T) {
	cfg, cat := embedded(t)
	r := newRouter(t, cfg, cat, &mocks.JevClientMock{})

	req := &args.Request{CLI: "codex", Model: "gpt-9", ModelSource: args.SourceFlag, Effort: "turbo", EffortSource: args.SourceFlag}
	el := Eligible(cfg, cat, req)
	d, err := r.Route(context.Background(), el, req, captured("x"))
	require.NoError(t, err)
	assert.Equal(t, Decision{CLI: "codex", Model: "gpt-9", Effort: "turbo", OptionID: "codex", Pinned: true}, d)
	assert.Equal(t, []string{"codex", "exec", "--model", "gpt-9", "-c", `model_reasoning_effort="turbo"`},
		args.Build(mustCLI(t, cfg, d.CLI), req, d.Choice()).Argv)
}

func TestRouteJevChoice(t *testing.T) {
	cfg, cat := embedded(t)
	client := answering("gpt-6.1-sol@medium")
	r := newRouter(t, cfg, cat, client)

	req := &args.Request{}
	el := Eligible(cfg, cat, req)
	d, err := r.Route(context.Background(), el, req, captured("fix it"))
	require.NoError(t, err)
	assert.Equal(t, "codex", d.CLI)
	assert.Equal(t, "gpt-6.1-sol", d.Model)
	assert.Equal(t, "medium", d.Effort)
	assert.Equal(t, "gpt-6.1-sol@medium", d.OptionID)
	require.NotNil(t, d.Answer)
	assert.InDelta(t, 0.9, d.Answer.Confidence, 1e-9)
	require.NoError(t, d.Undecided)

	require.Len(t, client.AskCalls(), 1)
	sent := client.AskCalls()[0].Req
	assert.Equal(t, cfg.Agrouter.JevModel, sent.Model)
	assert.Equal(t, prompt.State{Prompt: "fix it"}, sent.State)
	assert.Equal(t, ids(el.Options), sent.Questions[questionRoute].Criteria.Names())
}

func TestRouteGoldenRequest(t *testing.T) {
	cfg, cat := requestFixture(t)
	tests := []struct {
		name   string
		req    *args.Request
		choice string
	}{
		{name: "all", req: &args.Request{}, choice: "fast"},
		{name: "cli_alpha", req: &args.Request{CLI: "alpha"}, choice: "fast"},
		{name: "effort_passthrough", req: &args.Request{CLI: "alpha", Effort: "turbo", EffortSource: args.SourceFlag},
			choice: "fast"},
		{name: "model_passthrough", req: &args.Request{Model: "unknown-model", ModelSource: args.SourceFlag}, choice: "beta"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := answering(tc.choice)
			r, err := New(cfg, cat, client)
			require.NoError(t, err)
			el := Eligible(cfg, cat, tc.req)
			captured := &prompt.Result{Prompt: "fix the flaky test in pkg/foo/foo_test.go", Files: []string{"package foo\n"},
				Attachments: []prompt.Attachment{{Source: "mentioned", Type: "image/png", Bytes: 48213}}}
			_, err = r.Route(context.Background(), el, tc.req, captured)
			require.NoError(t, err)
			require.Len(t, client.AskCalls(), 1)

			got, err := json.MarshalIndent(client.AskCalls()[0].Req, "", "  ")
			require.NoError(t, err)
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

func TestRouteQuestionContents(t *testing.T) {
	cfg, cat := embedded(t)
	req := &args.Request{CLI: "claude"}
	el := Eligible(cfg, cat, req)
	q := routeQuestion(cfg, "Q?", el.Options, "")

	in, ok := q.Instructions.(routeInstructions)
	require.True(t, ok)
	assert.Equal(t, "Q?", in.Question)
	assert.Equal(t, []string{"claude"}, in.CLIs.Names(), "only the remaining CLIs")
	assert.NotContains(t, in.Models.Names(), "gpt-6.1-sol")
	assert.Equal(t, []string{"claude"}, in.Efforts.Names())
	for _, c := range q.Criteria {
		if c.Name == "claude-haiku-4-5" {
			assert.Equal(t, routeCriterion{CLI: "claude", Model: "claude-haiku-4-5", Effort: effortNone}, c.Value)
		}
	}
}

func TestNewQuestionOverBudget(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	for _, key := range []string{"question", "chunk_question", "relevance", "complexity_question", "complexity_evidence"} {
		t.Run(key, func(t *testing.T) {
			local := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(local, fmt.Appendf(nil, "[agrouter]\n%s = %s\n", key, long), 0o600))
			cfg, err := config.Load(config.Sources{Embedded: defaults.Config, LocalPath: local})
			require.NoError(t, err)
			cat, err := catalog.Build(cfg)
			require.NoError(t, err)

			_, err = New(cfg, cat, &mocks.JevClientMock{})
			require.ErrorIs(t, err, prompt.ErrQuestionsOverBudget)
			assert.Contains(t, err.Error(), "config:")
		})
	}
}

func TestNewBudgetFromWholeCatalog(t *testing.T) {
	cfg, cat := embedded(t)
	r := newRouter(t, cfg, cat, &mocks.JevClientMock{})
	b := r.budget
	assert.Positive(t, b.State)
	assert.Less(t, b.State, 30_000)
	assert.Positive(t, b.Chunk)
	assert.GreaterOrEqual(t, b.Doc, prompt.MinStateTokens)
	assert.Less(t, b.Doc, 30_000)
}

func TestRouteCannotDecide(t *testing.T) {
	cfg, cat := embedded(t)
	causes := []struct {
		name string
		err  error
	}{
		{"no key", jev.ErrNoKey},
		{"timeout", context.DeadlineExceeded},
		{"401", &jev.StatusError{Status: 401}},
		{"429 exhausted", &jev.StatusError{Status: 429}},
		{"529 exhausted", &jev.StatusError{Status: 529}},
		{"malformed", jev.ErrMalformed},
	}
	cases := []struct {
		name   string
		req    *args.Request
		want   Decision // without Undecided
		argv   []string
		noCall bool
	}{
		{name: "cli known by --cli", req: &args.Request{CLI: "claude"},
			want: Decision{CLI: "claude", Pinned: true}, argv: []string{"claude", "-p"}},
		{name: "cli known by --model",
			req:  &args.Request{Model: "opus", ModelSource: args.SourceFlag},
			want: Decision{CLI: "claude", Model: "claude-opus-5-5"},
			argv: []string{"claude", "-p", "--model", "claude-opus-5-5"}},
		{name: "cli known as the only one left (ralphex codex argv)", req: ralphexCodex(),
			want: Decision{CLI: "codex"},
			argv: []string{"codex", "exec", "-c", "features.multi_agent=true", "-c", `agents.reviewer.description="Reviews code"`,
				"--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "--sandbox", "danger-full-access",
				"-c", "stream_idle_timeout_ms=3600000", "-c", `project_doc_fallback_filenames=["CLAUDE.md"]`}},
		{name: "--cli=claude --effort high keeps the effort",
			req:  &args.Request{CLI: "claude", Effort: "high", EffortSource: args.SourceFlag},
			want: Decision{CLI: "claude", Effort: "high", Pinned: true},
			argv: []string{"claude", "-p", "--effort", "high"}},
	}
	for _, cause := range causes {
		for _, tc := range cases {
			t.Run(cause.name+"/"+tc.name, func(t *testing.T) {
				client := failing(cause.err)
				r := newRouter(t, cfg, cat, client)
				el := Eligible(cfg, cat, tc.req)
				require.Greater(t, len(el.Options), 1)

				d, err := r.Route(context.Background(), el, tc.req, captured("fix it"))
				require.NoError(t, err)
				require.ErrorIs(t, d.Undecided, cause.err)
				d.Undecided = nil
				assert.Equal(t, tc.want, d)
				assert.Equal(t, tc.argv, args.Build(mustCLI(t, cfg, d.CLI), tc.req, d.Choice()).Argv)
				assert.Len(t, client.AskCalls(), 1)
			})
		}
		t.Run(cause.name+"/cli unknown", func(t *testing.T) {
			r := newRouter(t, cfg, cat, failing(cause.err))
			req := &args.Request{}
			_, err := r.Route(context.Background(), Eligible(cfg, cat, req), req, captured("fix it"))
			require.ErrorIs(t, err, ErrCannotDecide)
			require.ErrorIs(t, err, cause.err)
			assert.Contains(t, err.Error(), "claude, codex")
		})
	}
}

func TestRouteCannotDecideWithoutRequest(t *testing.T) {
	cfg, cat := embedded(t)
	tests := []struct {
		name     string
		captured *prompt.Result
		want     error
	}{
		{name: "capture past the deadline", captured: &prompt.Result{Undecidable: context.DeadlineExceeded},
			want: context.DeadlineExceeded},
		{name: "attachments over the anchor", captured: overAnchor(), want: prompt.ErrAttachmentsOverAnchor},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &mocks.JevClientMock{}
			r := newRouter(t, cfg, cat, client)
			req := &args.Request{CLI: "codex"}
			d, err := r.Route(context.Background(), Eligible(cfg, cat, req), req, tc.captured)
			require.NoError(t, err)
			require.ErrorIs(t, d.Undecided, tc.want)
			assert.Equal(t, "codex", d.CLI)
			assert.Empty(t, d.Model)
			assert.Empty(t, client.AskCalls())
		})
	}
}

func TestRouteMalformedAnswers(t *testing.T) {
	cfg, cat := embedded(t)
	tests := []struct {
		name    string
		answers map[string]jev.Answer
	}{
		{name: "missing route answer", answers: map[string]jev.Answer{}},
		{name: "choice outside the options", answers: map[string]jev.Answer{questionRoute: {Choice: "gpt-6.1-sol@high"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &mocks.JevClientMock{AskFunc: func(context.Context, jev.Request) (map[string]jev.Answer, error) {
				return tc.answers, nil
			}}
			r := newRouter(t, cfg, cat, client)
			req := &args.Request{CLI: "claude"}
			d, err := r.Route(context.Background(), Eligible(cfg, cat, req), req, captured("x"))
			require.NoError(t, err)
			require.ErrorIs(t, d.Undecided, jev.ErrMalformed)
			assert.Equal(t, Decision{CLI: "claude", Pinned: true, Undecided: d.Undecided}, d)
		})
	}
}

func mustCLI(t *testing.T, cfg *config.Config, name string) config.CLI {
	t.Helper()
	cli, ok := cfg.CLIByName(name)
	require.True(t, ok)
	return cli
}

// overAnchor is a state too large for one request whose attachments exceed their anchor share even
// summarized.
func overAnchor() *prompt.Result {
	c := captured(strings.Repeat("line of text\n", 20_000))
	for i := range 200 {
		c.Attachments = append(c.Attachments, prompt.Attachment{Source: prompt.SourceMentioned,
			Type: fmt.Sprintf("application/x-kind-%d", i), Bytes: 1})
	}
	return c
}
