package router

import (
	"context"
	"fmt"
	"sync"
)

// piece is one chunk of a fanned-out sequence and, once asked, its answer.
type piece[C, A any] struct {
	chunk  C
	answer A
}

// fanout is the request lifecycle both stages share: every chunk asked at once, and fail-fast
// cancel. A rejected chunk is never resent. Asking and scoring are the stage's.
type fanout[C, A any] struct {
	name string                                    // what a chunk is called in errors
	ask  func(ctx context.Context, c C) (A, error) // one validated request
}

// run asks every chunk at once: the provider runs remotely, and 429s are retried within the
// deadline. chunks are numbered 1 to len(chunks) in order, so errors name a chunk by its position.
// The first failure cancels the rest and ends it with an error; answers that all came in stand,
// whatever the deadline says afterward.
func (f fanout[C, A]) run(ctx context.Context, chunks []C) ([]*piece[C, A], error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	seq := make([]*piece[C, A], len(chunks))
	for i, c := range chunks {
		p := &piece[C, A]{chunk: c}
		seq[i] = p
		wg.Go(func() {
			answer, err := f.ask(ctx, p.chunk)
			if err == nil {
				p.answer = answer
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if firstErr == nil {
				firstErr = fmt.Errorf("%s %d of %d: %w", f.name, i+1, len(chunks), err)
				cancel()
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return seq, nil
}
