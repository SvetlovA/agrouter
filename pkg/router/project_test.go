package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

// staged answers doc requests with docs and every routing request (single or chunk) with route.
func staged(docs func() (map[string]jev.Answer, error), route func(req jev.Request) (map[string]jev.Answer, error)) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		switch req.State.(type) {
		case prompt.DocsState, prompt.DocChunkState:
			return docs()
		}
		return route(req)
	}}
}

// scored answers every doc request with an expected complexity of 6.9 and full evidence.
func scored() (map[string]jev.Answer, error) {
	return map[string]jev.Answer{
		questionComplexity: complexityAnswer(6.9),
		questionEvidence:   {Type: jev.TypeNoul, Noul: 1},
	}, nil
}

// routeFast answers a whole-state stage request towards the fixture's "fast" option.
func routeFast(req jev.Request) (map[string]jev.Answer, error) {
	return certain(req, "fast"), nil
}

func projectCapture() *prompt.Result {
	return &prompt.Result{Prompt: "fix the flaky test in pkg/foo/foo_test.go",
		Files:       []string{"package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tt.Skip(\"flaky\")\n}\n"},
		Attachments: []prompt.Attachment{{Source: "mentioned", Type: "image/png", Bytes: 48213}},
		Docs:        []string{"# Project\nA payments platform of 40 services.\n"}}
}

// routeRequests are the requests that are not doc requests, in call order.
func routeRequests(client *mocks.JevClientMock) []jev.Request {
	calls := client.AskCalls()
	out := make([]jev.Request, 0, len(calls))
	for _, c := range calls {
		switch c.Req.State.(type) {
		case prompt.DocsState, prompt.DocChunkState:
			continue
		}
		out = append(out, c.Req)
	}
	return out
}

func assertGolden(t *testing.T, name string, req jev.Request) {
	t.Helper()
	got, err := json.MarshalIndent(req, "", "  ")
	require.NoError(t, err)
	golden := filepath.Join("testdata", "request_"+name+".json")
	if *update {
		require.NoError(t, os.WriteFile(golden, append(got, '\n'), 0o600))
	}
	want, err := os.ReadFile(golden) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got)+"\n")
}

func TestRouteProjectGoldenRequest(t *testing.T) {
	cfg, cat := requestFixture(t)
	req := &args.Request{CLI: "alpha"}
	el := Eligible(cfg, cat, req)
	client := staged(scored, routeFast)
	c := projectCapture()

	d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, c)
	require.NoError(t, err)
	assert.Equal(t, "fast", d.OptionID)
	require.NotNil(t, d.Complexity)
	assert.InDelta(t, 6.9, d.Complexity.Complexity, 1e-9)
	assert.Nil(t, c.Project, "the caller's capture is not changed")

	calls := client.AskCalls()
	require.Len(t, calls, 2)
	assert.IsType(t, prompt.DocsState{}, calls[0].Req.State, "the complexity stage runs first")
	st, ok := calls[1].Req.State.(prompt.State)
	require.True(t, ok)
	assert.Equal(t, &prompt.Project{Complexity: 6.9}, st.Project)
	assertGolden(t, "project", calls[1].Req)
}

func TestRouteProjectChunkGoldenRequest(t *testing.T) {
	cfg, cat := requestFixture(t)
	req := &args.Request{CLI: "alpha"}
	el := Eligible(cfg, cat, req)
	client := staged(scored, func(req jev.Request) (map[string]jev.Answer, error) {
		return stageAnswers(req, "fast", 0.9, 0.5), nil
	})
	r := newRouter(t, cfg, cat, client)
	// force a small state into chunks, keeping the doc budget whole
	r.budget = prompt.Budget{State: 1, Chunk: 150, Doc: r.budget.Doc}

	d, err := r.Route(context.Background(), el, req, projectCapture())
	require.NoError(t, err)
	require.Len(t, d.Stages, 3)
	require.NotNil(t, d.Stages[1].Pooled, "the model stage")
	require.NotNil(t, d.Complexity)

	routed := routeRequests(client)
	require.GreaterOrEqual(t, len(routed), 2)
	var first jev.Request
	for _, rq := range routed {
		st, ok := rq.State.(prompt.ChunkState)
		require.True(t, ok)
		assert.Equal(t, &prompt.Project{Complexity: 6.9}, st.Anchor.Project, "every chunk's anchor")
		assert.LessOrEqual(t, prompt.Tokens(mustJSONLen(t, st)), r.budget.Chunk, "the anchor size includes the project")
		if st.Chunk.Index == 1 {
			first = rq
		}
	}
	assertGolden(t, "chunk_project", first)
}

func mustJSONLen(t *testing.T, v any) int {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return len(data)
}

func TestRouteProjectSkippedWithoutDocText(t *testing.T) {
	cfg, cat := requestFixture(t)
	req := &args.Request{}
	el := Eligible(cfg, cat, req)
	for _, docs := range [][]string{nil, {""}, {"", ""}} {
		client := staged(func() (map[string]jev.Answer, error) {
			return nil, errors.New("doc request sent")
		}, routeFast)
		d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req,
			&prompt.Result{Prompt: "fix it", Docs: docs})
		require.NoError(t, err)
		assert.Nil(t, d.Complexity)
		require.Len(t, client.AskCalls(), 2, "the cli and model stages")
		for _, c := range client.AskCalls() {
			assert.Equal(t, prompt.State{Prompt: "fix it"}, c.Req.State)
		}
	}
}

func TestRouteProjectOneOptionSkipsJev(t *testing.T) {
	cfg, cat := requestFixture(t)
	client := &mocks.JevClientMock{} // Ask panics if called
	req := &args.Request{Model: "strong-model", ModelSource: args.SourceFlag, Effort: "high", EffortSource: args.SourceFlag}
	el := Eligible(cfg, cat, req)
	require.Len(t, el.Options, 1)

	d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, projectCapture())
	require.NoError(t, err)
	assert.Equal(t, "strong@high", d.OptionID)
	assert.Nil(t, d.Complexity)
	assert.Empty(t, client.AskCalls())
}

func TestRouteProjectStageFailure(t *testing.T) {
	cfg, cat := requestFixture(t)
	failures := map[string]func() (map[string]jev.Answer, error){
		"jev error": func() (map[string]jev.Answer, error) { return nil, &jev.StatusError{Status: 500} },
		"malformed": func() (map[string]jev.Answer, error) { return map[string]jev.Answer{}, nil },
		"deadline":  func() (map[string]jev.Answer, error) { return nil, context.DeadlineExceeded },
	}
	for name, fail := range failures {
		t.Run(name+", one CLI left: runs it with the caller's values", func(t *testing.T) {
			req := &args.Request{CLI: "alpha"}
			el := Eligible(cfg, cat, req)
			require.Greater(t, len(el.Options), 1)
			client := staged(fail, routeFast)

			d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, projectCapture())
			require.NoError(t, err)
			assert.Equal(t, "alpha", d.CLI)
			assert.Empty(t, d.Model)
			assert.Empty(t, d.Effort)
			assert.Empty(t, d.OptionID)
			require.Error(t, d.Undecided)
			assert.Contains(t, d.Undecided.Error(), "complexity stage")
			assert.Nil(t, d.Complexity)
			assert.Empty(t, routeRequests(client), "no routing without the complexity")
		})
		t.Run(name+", several CLIs: cannot decide", func(t *testing.T) {
			req := &args.Request{}
			el := Eligible(cfg, cat, req)
			client := staged(fail, routeFast)

			_, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, projectCapture())
			require.ErrorIs(t, err, ErrCannotDecide)
			assert.Contains(t, err.Error(), "complexity stage")
			assert.Empty(t, routeRequests(client))
		})
	}
}

func TestRouteProjectKeptWhenRoutingFails(t *testing.T) {
	cfg, cat := requestFixture(t)
	req := &args.Request{CLI: "alpha"}
	el := Eligible(cfg, cat, req)
	client := staged(scored, func(jev.Request) (map[string]jev.Answer, error) {
		return nil, &jev.StatusError{Status: 500}
	})

	d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, projectCapture())
	require.NoError(t, err)
	require.Error(t, d.Undecided)
	assert.Equal(t, "alpha", d.CLI)
	require.NotNil(t, d.Complexity, "the doc scores still reach debug output")
	assert.InDelta(t, 6.9, d.Complexity.Complexity, 1e-9)
}

func TestRouteProjectUndecidableCaptureSkipsBothStages(t *testing.T) {
	cfg, cat := requestFixture(t)
	req := &args.Request{CLI: "alpha"}
	el := Eligible(cfg, cat, req)
	client := &mocks.JevClientMock{} // Ask panics if called
	c := &prompt.Result{Docs: []string{"docs"}, Undecidable: context.DeadlineExceeded}

	d, err := newRouter(t, cfg, cat, client).Route(context.Background(), el, req, c)
	require.NoError(t, err)
	require.ErrorIs(t, d.Undecided, context.DeadlineExceeded)
	assert.Empty(t, client.AskCalls())
}

// TestRouteProjectStagesInOrder pins that routing starts only once every doc request is answered.
func TestRouteProjectStagesInOrder(t *testing.T) {
	cfg, cat := requestFixture(t)
	req := &args.Request{CLI: "alpha"}
	el := Eligible(cfg, cat, req)
	var mu sync.Mutex
	var order []string
	client := staged(func() (map[string]jev.Answer, error) {
		mu.Lock()
		order = append(order, "doc")
		mu.Unlock()
		return scored()
	}, func(req jev.Request) (map[string]jev.Answer, error) {
		mu.Lock()
		order = append(order, fmt.Sprintf("%T", req.State))
		mu.Unlock()
		return routeFast(req)
	})
	r := newRouter(t, cfg, cat, client)
	r.budget.Doc = 40 // split the docs into several chunks
	c := projectCapture()
	c.Docs = []string{docsOf("A", 30), docsOf("B", 30)}

	_, err := r.Route(context.Background(), el, req, c)
	require.NoError(t, err)
	require.Greater(t, len(order), 2)
	for _, o := range order[:len(order)-1] {
		assert.Equal(t, "doc", o)
	}
	assert.Equal(t, "prompt.State", order[len(order)-1])
}
