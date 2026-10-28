package router

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/SvetlovA/agrouter/pkg/classifier"
)

// answersOnly adapts a test closure that returns only answers to the JevClient's Ask signature, with
// no reported input tokens.
func answersOnly(fn func(context.Context, classifier.Request) (map[string]classifier.Answer, error)) func(context.Context, classifier.Request) (classifier.Response, error) {
	return func(ctx context.Context, req classifier.Request) (classifier.Response, error) {
		answers, err := fn(ctx, req)
		return classifier.Response{Answers: answers}, err
	}
}

// reporting adapts a test closure like answersOnly, with the input tokens the provider reports for
// each request: tokens returns nil for a request it reports none for.
func reporting(fn func(context.Context, classifier.Request) (map[string]classifier.Answer, error),
	tokens func(classifier.Request) *int) func(context.Context, classifier.Request) (classifier.Response, error) {
	return func(ctx context.Context, req classifier.Request) (classifier.Response, error) {
		answers, err := fn(ctx, req)
		return classifier.Response{Answers: answers, InputTokens: tokens(req)}, err
	}
}

// rejection is a provider rejection a request can fail with, and what the router must make of it.
type rejection struct {
	name    string
	err     error
	status  int
	tooLong bool // matches classifier.ErrTooLong
}

// rejections are the kinds of rejected request: the structured overflow, any other 400, and a 422.
var rejections = []rejection{
	{name: "overflow", err: tooLong(), status: http.StatusBadRequest, tooLong: true},
	{name: "other 400", err: &classifier.StatusError{Status: http.StatusBadRequest, ErrorType: "invalid_request"},
		status: http.StatusBadRequest},
	{name: "422", err: unprocessable(), status: http.StatusUnprocessableEntity},
}

// tooLong is the provider's rejection of a request over its context length.
func tooLong() error {
	return &classifier.StatusError{Status: http.StatusBadRequest, ErrorType: "max_tokens_exceeded"}
}

func unprocessable() error { return &classifier.StatusError{Status: http.StatusUnprocessableEntity} }

// gate holds each request that reaches it until n have, so a test sees every chunk in flight
// before any of them fails; no request waits for another to finish.
type gate struct {
	n       int
	mu      sync.Mutex
	arrived int
	open    chan struct{}
}

func newGate(n int) *gate { return &gate{n: n, open: make(chan struct{})} }

// wait reports whether all n requests arrived before the timeout.
func (g *gate) wait() bool {
	g.mu.Lock()
	g.arrived++
	if g.arrived == g.n {
		close(g.open)
	}
	g.mu.Unlock()
	select {
	case <-g.open:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}
