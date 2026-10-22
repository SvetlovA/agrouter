package router

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFanoutKeepsSequenceOrder(t *testing.T) {
	const n = 8
	chunks := make([]int, n)
	for i := range chunks {
		chunks[i] = i + 1
	}
	g := newGate(n)
	f := fanout[int, string]{name: "item", ask: func(_ context.Context, c int) (string, error) {
		if !g.wait() { // every request in flight, so the answers arrive in no fixed order
			return "", errors.New("not every item in flight")
		}
		return fmt.Sprintf("answer %d", c), nil
	}}

	seq, err := f.run(context.Background(), chunks)
	require.NoError(t, err)
	require.Len(t, seq, n)
	for i, p := range seq {
		assert.Equal(t, i+1, p.chunk)
		assert.Equal(t, fmt.Sprintf("answer %d", i+1), p.answer)
	}
}

func TestFanoutNoChunks(t *testing.T) {
	f := fanout[int, string]{name: "item", ask: func(context.Context, int) (string, error) {
		return "", errors.New("asked with nothing to ask")
	}}
	seq, err := f.run(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, seq)
}

func TestFanoutFirstFailureCancelsTheRest(t *testing.T) {
	const n = 4
	boom := errors.New("boom")
	g := newGate(n)
	var asked atomic.Int32
	f := fanout[int, string]{name: "item", ask: func(ctx context.Context, c int) (string, error) {
		asked.Add(1)
		if !g.wait() {
			return "", errors.New("not every item in flight")
		}
		if c == 2 {
			return "", boom
		}
		<-ctx.Done() // canceled by the failure, not by the caller
		return "", ctx.Err()
	}}

	seq, err := f.run(context.Background(), []int{1, 2, 3, 4})
	assert.Nil(t, seq)
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, context.Canceled, "a canceled sibling does not replace the first failure")
	require.EqualError(t, err, "item 2 of 4: boom")
	assert.Equal(t, int32(n), asked.Load(), "each item asked once")
}

func TestFanoutCallerDeadlineEndsIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := newGate(2)
	f := fanout[int, string]{name: "item", ask: func(ctx context.Context, _ int) (string, error) {
		if !g.wait() {
			return "", errors.New("not every item in flight")
		}
		cancel()
		<-ctx.Done()
		return "", ctx.Err()
	}}
	seq, err := f.run(ctx, []int{1, 2})
	assert.Nil(t, seq)
	require.ErrorIs(t, err, context.Canceled)
}

func TestFanoutCompleteAnswersStandAfterTheDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := fanout[int, string]{name: "item", ask: func(_ context.Context, c int) (string, error) {
		return fmt.Sprintf("answer %d", c), nil
	}}
	seq, err := f.run(ctx, []int{1, 2})
	cancel() // the deadline passes after every answer came in
	require.NoError(t, err)
	require.Len(t, seq, 2)
	assert.Equal(t, "answer 2", seq[1].answer)
}
