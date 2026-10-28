package router

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/classifier"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router/mocks"
)

func TestStageTokensWholeState(t *testing.T) {
	cfg, cat := requestFixture(t)
	reported := map[string]int{LevelCLI: 1100, LevelModel: 2200, LevelEffort: 3300}
	route := func(t *testing.T, client *mocks.JevClientMock) (*Router, Decision) {
		t.Helper()
		r := newRouter(t, cfg, cat, client)
		req := &args.Request{}
		d, err := r.Route(context.Background(), Eligible(cat, req), req, captured("fix it"))
		require.NoError(t, err)
		require.Len(t, d.Stages, len(routeLevels))
		return r, d
	}

	t.Run("each stage carries its own request's estimate and reported count", func(t *testing.T) {
		client := &mocks.JevClientMock{AskFunc: reporting(func(_ context.Context, req classifier.Request) (map[string]classifier.Answer, error) {
			return certain(req, "strong@high"), nil
		}, func(req classifier.Request) *int {
			id, _, _ := stageAsked(req)
			return new(reported[id])
		})}
		r, d := route(t, client)

		calls := client.AskCalls()
		require.Len(t, calls, len(d.Stages))
		for i, st := range d.Stages {
			id, _, _ := stageAsked(calls[i].Req)
			require.Equal(t, st.Level, id, "calls follow the stage order")
			assert.Equal(t, r.estimate(calls[i].Req.State, calls[i].Req.Questions), st.Estimated, st.Level)
			assert.Positive(t, st.Estimated, st.Level)
			require.NotNil(t, st.Reported, st.Level)
			assert.Equal(t, reported[st.Level], *st.Reported, st.Level)
		}
	})
	t.Run("a provider that reports no count leaves it unknown", func(t *testing.T) {
		_, d := route(t, choosing("strong@high"))
		for _, st := range d.Stages {
			assert.Nil(t, st.Reported, st.Level)
			assert.Positive(t, st.Estimated, st.Level)
		}
	})
	t.Run("a skipped stage has no request, so no tokens", func(t *testing.T) {
		client := choosing("strong@high")
		r := newRouter(t, cfg, cat, client)
		req := &args.Request{CLI: "alpha"}
		d, err := r.Route(context.Background(), Eligible(cat, req), req, captured("fix it"))
		require.NoError(t, err)
		require.True(t, d.Stages[0].Skipped)
		assert.Zero(t, d.Stages[0].Estimated)
		assert.Nil(t, d.Stages[0].Reported)
	})
}

func TestStageTokensPooledChunks(t *testing.T) {
	cfg, cat := requestFixture(t)
	c := captured(filler(200_000))
	r := newRouter(t, cfg, cat, &mocks.JevClientMock{})
	split, err := c.Split(r.budget)
	require.NoError(t, err)
	require.NotNil(t, split)
	n := len(split.Chunks)
	require.Greater(t, n, 2)

	// the provider reports the chunk's position, and later chunks answer first, so a count that
	// followed the answer order instead of its own request would land on the wrong chunk
	var mu sync.Mutex
	sent := map[string]map[int]classifier.Request{}
	client := &mocks.JevClientMock{AskFunc: reporting(func(_ context.Context, req classifier.Request) (map[string]classifier.Answer, error) {
		st := req.State.(prompt.ChunkState)
		time.Sleep(time.Duration(n-st.Chunk.Index) * 5 * time.Millisecond)
		id, _, _ := stageAsked(req)
		mu.Lock()
		if sent[id] == nil {
			sent[id] = map[int]classifier.Request{}
		}
		sent[id][st.Chunk.Index] = req
		mu.Unlock()
		return stageAnswers(req, "strong@high", 0.8, 0.5), nil
	}, func(req classifier.Request) *int {
		id, _, _ := stageAsked(req)
		if id == LevelEffort {
			return nil // the provider reported nothing for this stage's requests
		}
		return new(1000 + req.State.(prompt.ChunkState).Chunk.Index)
	})}
	r = newRouter(t, cfg, cat, client)

	req := &args.Request{CLI: "alpha"}
	d, err := r.Route(context.Background(), Eligible(cat, req), req, c)
	require.NoError(t, err)

	pooled := 0
	for _, st := range d.Stages {
		if st.Pooled == nil {
			continue
		}
		pooled++
		require.Len(t, st.Pooled.Chunks, n, st.Level)
		for i, ch := range st.Pooled.Chunks {
			assert.Equal(t, i+1, ch.Index, st.Level)
			want := sent[st.Level][ch.Index]
			assert.Equal(t, r.estimate(want.State, want.Questions), ch.Estimated, "%s chunk %d", st.Level, ch.Index)
			assert.Positive(t, ch.Estimated, "%s chunk %d", st.Level, ch.Index)
			if st.Level == LevelEffort {
				assert.Nil(t, ch.Reported, "%s chunk %d", st.Level, ch.Index)
				continue
			}
			require.NotNil(t, ch.Reported, "%s chunk %d", st.Level, ch.Index)
			assert.Equal(t, 1000+ch.Index, *ch.Reported, "%s chunk %d", st.Level, ch.Index)
		}
		assert.Zero(t, st.Estimated, "a pooled stage has no single request")
		assert.Nil(t, st.Reported, "a pooled stage has no single request")
	}
	assert.Equal(t, 2, pooled)
}

func TestComplexityTokens(t *testing.T) {
	cfg, cat := requestFixture(t)
	r := newRouter(t, cfg, cat, &mocks.JevClientMock{})
	b := r.budget
	docs := &prompt.Result{Docs: []string{docsOf("ENTERPRISE", 2*b.Doc), docsOf("STYLE", 3*b.Doc)}}
	chunks := docs.SplitDocs(b)
	require.Greater(t, len(chunks), 2)

	var mu sync.Mutex
	sent := map[int]classifier.Request{}
	client := &mocks.JevClientMock{AskFunc: reporting(func(_ context.Context, req classifier.Request) (map[string]classifier.Answer, error) {
		st := req.State.(prompt.DocChunkState)
		time.Sleep(time.Duration(len(chunks)-st.Doc.Index) * 5 * time.Millisecond)
		mu.Lock()
		sent[st.Doc.Index] = req
		mu.Unlock()
		return docAnswers(5, 0.5), nil
	}, func(req classifier.Request) *int {
		idx := req.State.(prompt.DocChunkState).Doc.Index
		if idx == 2 {
			return nil
		}
		return new(2000 + idx)
	})}
	r = newRouter(t, cfg, cat, client)

	got, err := r.complexity(context.Background(), docs)
	require.NoError(t, err)
	require.Len(t, got.Chunks, len(chunks))
	for i, ch := range got.Chunks {
		want := sent[i+1]
		assert.Equal(t, r.estimate(want.State, want.Questions), ch.Estimated, "doc chunk %d", i+1)
		assert.Positive(t, ch.Estimated, "doc chunk %d", i+1)
		if i+1 == 2 {
			assert.Nil(t, ch.Reported, "doc chunk 2: the provider reported nothing")
			continue
		}
		require.NotNil(t, ch.Reported, "doc chunk %d", i+1)
		assert.Equal(t, 2000+i+1, *ch.Reported, "doc chunk %d", i+1)
	}

	t.Run("the whole-docs request", func(t *testing.T) {
		client := &mocks.JevClientMock{AskFunc: reporting(func(context.Context, classifier.Request) (map[string]classifier.Answer, error) {
			return docAnswers(6, 0.9), nil
		}, func(classifier.Request) *int { return new(777) })}
		r := newRouter(t, cfg, cat, client)
		got, err := r.complexity(context.Background(), &prompt.Result{Docs: []string{"# Shop\n"}})
		require.NoError(t, err)
		require.Len(t, got.Chunks, 1)
		call := client.AskCalls()[0].Req
		assert.Equal(t, r.estimate(call.State, call.Questions), got.Chunks[0].Estimated)
		require.NotNil(t, got.Chunks[0].Reported)
		assert.Equal(t, 777, *got.Chunks[0].Reported)
	})
}
