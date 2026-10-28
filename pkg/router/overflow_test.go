package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

// overflowClient rejects the requests reject picks with err, once each, and answers the rest.
func overflowClient(reject func(jev.Request) bool, err error, rejected *jev.Request, calls *atomic.Int32) *mocks.JevClientMock {
	return &mocks.JevClientMock{AskFunc: answersOnly(func(_ context.Context, req jev.Request) (map[string]jev.Answer, error) {
		if reject(req) {
			calls.Add(1)
			*rejected = req
			return nil, err
		}
		switch req.State.(type) {
		case prompt.DocsState, prompt.DocChunkState:
			return scored()
		case prompt.ChunkState:
			return stageAnswers(req, "fast", 0.9, 0.5), nil
		}
		return certain(req, "fast"), nil
	})}
}

// estimateOf is the tokens of the state and questions of req at the allowance's ratio, counted from
// the request as sent.
func estimateOf(t *testing.T, p prompt.Policy, req jev.Request) int {
	t.Helper()
	state, err := json.Marshal(req.State)
	require.NoError(t, err)
	questions, err := json.Marshal(req.Questions)
	require.NoError(t, err)
	return p.Tokens(len(state) + len(questions))
}

func TestOverflowErrorNamesTheRequest(t *testing.T) {
	const marker = "SECRET-PROMPT-MARKER"
	cfg, cat := requestFixture(t)
	cfg.Classifier.RequestTokens = 20_000
	cfg.Classifier.BytesPerToken = 2.5
	probe := newRouter(t, cfg, cat, &mocks.JevClientMock{})

	big := &prompt.Result{Prompt: "START " + marker + "\n" + filler(200_000)}
	split, err := big.Split(probe.budget)
	require.NoError(t, err)
	require.NotNil(t, split)
	docs := &prompt.Result{Prompt: "fix it", Docs: []string{docsOf(marker, 4*probe.budget.Doc)}}
	docChunks := docs.SplitDocs(probe.budget)
	require.Greater(t, len(docChunks), 2)

	tests := []struct {
		name     string
		captured *prompt.Result
		reject   func(jev.Request) bool
		want     string // the stage and the position, up to the request's own wording
	}{
		{name: "whole state", captured: captured("fix it " + marker),
			reject: func(req jev.Request) bool { _, ok := req.State.(prompt.State); return ok },
			want:   "cli stage: route request"},
		{name: "chunk", captured: big,
			reject: func(req jev.Request) bool {
				st, ok := req.State.(prompt.ChunkState)
				return ok && st.Chunk.Index == 2
			},
			want: fmt.Sprintf("cli stage: chunk 2 of %d: chunk request", len(split.Chunks))},
		{name: "whole docs", captured: &prompt.Result{Prompt: "fix it", Docs: []string{"# Project " + marker}},
			reject: func(req jev.Request) bool { _, ok := req.State.(prompt.DocsState); return ok },
			want:   "complexity stage: doc request"},
		{name: "doc chunk", captured: docs,
			reject: func(req jev.Request) bool {
				st, ok := req.State.(prompt.DocChunkState)
				return ok && st.Doc.Index == 2
			},
			want: fmt.Sprintf("complexity stage: doc chunk 2 of %d: doc request", len(docChunks))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rejected jev.Request
			var calls atomic.Int32
			r := newRouter(t, cfg, cat, overflowClient(tc.reject, tooLong(), &rejected, &calls))
			req := &args.Request{}
			_, err := r.Route(context.Background(), Eligible(cat, req), req, tc.captured)

			require.ErrorIs(t, err, ErrCannotDecide)
			require.ErrorIs(t, err, jev.ErrTooLong)
			assert.EqualValues(t, 1, calls.Load(), "the rejected request is never resent")
			want := fmt.Sprintf("%s: estimated %d tokens, request_tokens is 20000: ", tc.want, estimateOf(t, r.budget.Policy, rejected))
			assert.Contains(t, err.Error(), want)
			assert.NotContains(t, err.Error(), marker, "no prompt text in the error")
			se, ok := errors.AsType[*StageError](err)
			require.True(t, ok)
			assert.Contains(t, tc.want, se.Stage)
		})
	}
}

func TestOverflowEstimateFollowsPolicy(t *testing.T) {
	c := captured("fix the flaky test")
	estimates := make([]int, 0, 3)
	for _, ratio := range []float64{1, 2, 4} {
		cfg, cat := requestFixture(t)
		cfg.Classifier.BytesPerToken = ratio
		var rejected jev.Request
		var calls atomic.Int32
		r := newRouter(t, cfg, cat, overflowClient(func(jev.Request) bool { return true }, tooLong(), &rejected, &calls))
		req := &args.Request{}
		_, err := r.Route(context.Background(), Eligible(cat, req), req, c)
		require.ErrorIs(t, err, jev.ErrTooLong)
		n := estimateOf(t, r.budget.Policy, rejected)
		assert.Contains(t, err.Error(), fmt.Sprintf("estimated %d tokens", n), "ratio %g", ratio)
		estimates = append(estimates, n)
	}
	assert.Greater(t, estimates[0], estimates[1], "a smaller ratio estimates more tokens for the same request")
	assert.Greater(t, estimates[1], estimates[2])
}

func TestOtherRejectionsAreNotSized(t *testing.T) {
	cfg, cat := requestFixture(t)
	for _, rj := range rejections {
		t.Run(rj.name, func(t *testing.T) {
			var rejected jev.Request
			var calls atomic.Int32
			r := newRouter(t, cfg, cat, overflowClient(func(jev.Request) bool { return true }, rj.err, &rejected, &calls))
			req := &args.Request{}
			_, err := r.Route(context.Background(), Eligible(cat, req), req, captured("fix it"))
			require.ErrorIs(t, err, ErrCannotDecide)
			requireRejected(t, err, rj)
			assert.Equal(t, rj.tooLong, strings.Contains(err.Error(), "request_tokens"), "only an overflow names the allowance")
		})
	}
}
