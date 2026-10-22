package router

import (
	"context"

	"github.com/SvetlovA/agrouter/pkg/jev"
)

// answersOnly adapts a test closure that returns only answers to the JevClient's Ask signature, with
// no reported input tokens.
func answersOnly(fn func(context.Context, jev.Request) (map[string]jev.Answer, error)) func(context.Context, jev.Request) (jev.Response, error) {
	return func(ctx context.Context, req jev.Request) (jev.Response, error) {
		answers, err := fn(ctx, req)
		return jev.Response{Answers: answers}, err
	}
}
