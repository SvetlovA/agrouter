package router

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

// errUnsplittable wraps a 422 that re-splitting cannot help.
var errUnsplittable = errors.New("rejected again after a re-split, or already under the minimum chunk size")

// piece is one chunk of a fanned-out sequence and, once asked, its answer.
type piece[C, A any] struct {
	chunk C
	// resplit marks a chunk that already comes from a re-split: another 422 cannot decide
	resplit bool
	done    bool
	answer  A
	err     error // a 422 to re-split; other errors end the fan-out at once
}

// fanout is the request lifecycle both stages share: every pending chunk asked at once, fail-fast
// cancel, and one re-split of a chunk Jev rejects with a 422. Asking and scoring are the stage's.
type fanout[C, A any] struct {
	name   string                                    // what a chunk is called in errors
	ask    func(ctx context.Context, c C) (A, error) // one validated request
	halve  func(c C) []C                             // the pieces of a rejected chunk
	text   func(c C) string                          // the chunk text, against the minimum state size
	number func(c C, index, of int) C
}

// run asks every chunk, re-splitting the ones rejected with a 422 until all are answered. chunks
// are numbered 1 to len(chunks) in order, so errors name a chunk by its position. resplit marks
// chunks that already come from a re-split. A failure other than a 422, a second 422, or a
// 422 on a chunk under the minimum state size ends it with an error.
func (f fanout[C, A]) run(ctx context.Context, chunks []C, resplit bool) ([]*piece[C, A], error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	seq := make([]*piece[C, A], len(chunks))
	for i, c := range chunks {
		seq[i] = &piece[C, A]{chunk: c, resplit: resplit}
	}
	for {
		if err := f.round(ctx, cancel, seq); err != nil {
			return nil, err
		}
		next, again, err := f.resplitRejected(seq)
		if err != nil {
			return nil, err
		}
		if !again {
			return seq, nil
		}
		seq = next
	}
}

// round asks every pending chunk of seq at once: Jev runs remotely, and 429s are retried within the
// deadline. The first failure other than a 422 cancels the rest and is returned; 422s are left on
// their pieces.
func (f fanout[C, A]) round(ctx context.Context, cancel context.CancelFunc, seq []*piece[C, A]) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for i, p := range seq {
		if p.done {
			continue
		}
		wg.Go(func() {
			answer, err := f.ask(ctx, p.chunk)
			if err == nil {
				p.done, p.answer, p.err = true, answer, nil
				return
			}
			if errors.Is(err, jev.ErrUnprocessable) {
				p.err = err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if firstErr == nil {
				firstErr = fmt.Errorf("%s %d of %d: %w", f.name, i+1, len(seq), err)
				cancel()
			}
		})
	}
	wg.Wait()
	// the deadline only matters when it left a chunk unasked; answers that all came in stand
	unasked := slices.ContainsFunc(seq, func(p *piece[C, A]) bool { return !p.done && p.err == nil })
	if firstErr == nil && ctx.Err() != nil && unasked {
		return fmt.Errorf("%s requests: %w", f.name, ctx.Err())
	}
	return firstErr
}

// resplitRejected replaces every chunk rejected with a 422 by its halves and renumbers the
// sequence. again is false when nothing was rejected. A chunk that cannot be re-split (a second
// 422, or text under the minimum state size) is an error.
func (f fanout[C, A]) resplitRejected(seq []*piece[C, A]) (next []*piece[C, A], again bool, err error) {
	for i, p := range seq {
		if p.err == nil {
			next = append(next, p)
			continue
		}
		if p.resplit || prompt.Tokens(len(f.text(p.chunk))) < prompt.MinStateTokens {
			return nil, false, fmt.Errorf("%s %d of %d: %w: %w", f.name, i+1, len(seq), errUnsplittable, p.err)
		}
		again = true
		for _, c := range f.halve(p.chunk) {
			next = append(next, &piece[C, A]{chunk: c, resplit: true})
		}
	}
	for i, p := range next {
		p.chunk = f.number(p.chunk, i+1, len(next))
	}
	return next, again, nil
}
