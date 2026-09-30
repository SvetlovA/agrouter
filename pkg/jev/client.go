package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is the Jev endpoint.
const DefaultURL = "https://api.typesafe.ai/v1/systemone"

// Limits on what is read back, so a misbehaving server cannot exhaust memory.
const (
	maxResponseBytes = 4 << 20
	maxErrorBody     = 512
	redacted         = "<redacted>"
)

var (
	// ErrNoKey means no API key is set, so no request is made.
	ErrNoKey = errors.New("no Jev API key")
	// ErrUnauthorized is a 401: the key was rejected.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrUnprocessable is a 422, possibly an oversized request; the router may re-split once.
	ErrUnprocessable = errors.New("request rejected as invalid")
	// ErrOverloaded is a 429 or 529 that retries could not get past within the deadline.
	ErrOverloaded = errors.New("rate limited or overloaded")
	// ErrMalformed is a 200 whose body is not a valid set of answers to the questions sent.
	ErrMalformed = errors.New("malformed response")
)

// StatusError is a non-200 response. Body is truncated and has the key redacted.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	msg := "jev: HTTP " + strconv.Itoa(e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Is matches the sentinel for the status: ErrUnauthorized, ErrUnprocessable or ErrOverloaded.
func (e *StatusError) Is(target error) bool {
	switch e.Status {
	case http.StatusUnauthorized:
		return target == ErrUnauthorized
	case http.StatusUnprocessableEntity:
		return target == ErrUnprocessable
	case http.StatusTooManyRequests, statusOverloaded:
		return target == ErrOverloaded
	default:
		return false
	}
}

// statusOverloaded is TypeSafe's (and Anthropic-style) "overloaded" status.
const statusOverloaded = 529

// Client calls Jev. The zero value is not usable; use New.
type Client struct {
	URL        string
	HTTPClient *http.Client
	// MinBackoff and MaxBackoff bound the wait between retries of a 429 or 529.
	MinBackoff time.Duration
	MaxBackoff time.Duration

	key string
}

// New returns a client for key against DefaultURL.
func New(key string) *Client {
	return &Client{
		URL:        DefaultURL,
		HTTPClient: &http.Client{},
		MinBackoff: 200 * time.Millisecond,
		MaxBackoff: 2 * time.Second,
		key:        key,
	}
}

// Ask sends req and returns the validated answer to every question, keyed as in req.Questions.
// ctx carries the routing deadline; 429 and 529 are retried with backoff until it would pass.
// Every error has the key redacted.
func (c *Client) Ask(ctx context.Context, req Request) (map[string]Answer, error) {
	if c.key == "" {
		return nil, ErrNoKey
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}
	for attempt := 0; ; attempt++ {
		status, data, header, postErr := c.post(ctx, body)
		if postErr != nil {
			return nil, c.redact(fmt.Errorf("jev: %w", postErr))
		}
		if status == http.StatusOK {
			answers, decodeErr := decode(data, req.Questions)
			return answers, c.redact(decodeErr)
		}
		serr := &StatusError{Status: status, Body: c.errorBody(data)}
		if !errors.Is(serr, ErrOverloaded) {
			return nil, serr
		}
		if !c.wait(ctx, c.delay(attempt, header)) {
			return nil, serr
		}
	}
}

func (c *Client) post(ctx context.Context, body []byte) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, data, resp.Header, nil
}

// delay is the wait before retry attempt+1: Retry-After in seconds if given, else exponential
// from MinBackoff; never above MaxBackoff.
func (c *Client) delay(attempt int, header http.Header) time.Duration {
	if secs, err := strconv.Atoi(header.Get("Retry-After")); err == nil && secs >= 0 {
		return min(time.Duration(secs)*time.Second, c.MaxBackoff)
	}
	d := c.MinBackoff
	for range attempt {
		d *= 2
		if d >= c.MaxBackoff {
			return c.MaxBackoff
		}
	}
	return min(d, c.MaxBackoff)
}

// wait sleeps for d and reports whether a retry can still run: false when the deadline would pass
// during the wait, or ctx ends first.
func (c *Client) wait(ctx context.Context, d time.Duration) bool {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= d {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// errorBody is the start of an error response, key redacted, on one line.
func (c *Client) errorBody(data []byte) string {
	s := strings.Join(strings.Fields(c.redactString(string(data))), " ")
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "..."
	}
	return s
}

func (c *Client) redactString(s string) string {
	if c.key == "" {
		return s
	}
	return strings.ReplaceAll(s, c.key, redacted)
}

// redact hides the key in err's message and keeps err in the chain for errors.Is.
func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: c.redactString(err.Error()), err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// decode parses a 200 body and validates one answer per question sent.
func decode(data []byte, questions map[string]Question) (map[string]Answer, error) {
	var resp response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("jev: %w: %w", ErrMalformed, err)
	}
	answers := make(map[string]Answer, len(questions))
	for id, q := range questions {
		w, ok := resp.Answers[id]
		if !ok {
			return nil, fmt.Errorf("jev: %w: no %q answer", ErrMalformed, id)
		}
		a, err := validate(q, w)
		if err != nil {
			return nil, fmt.Errorf("jev: %w: %q: %w", ErrMalformed, id, err)
		}
		answers[id] = a
	}
	return answers, nil
}

// probabilitySlack is how far the probabilities may sum from 1.
const probabilitySlack = 0.01

func validate(q Question, w wireAnswer) (Answer, error) {
	if w.Type != "" && w.Type != q.Type {
		return Answer{}, fmt.Errorf("answer type %q for a %s question", w.Type, q.Type)
	}
	switch q.Type {
	case TypeChoice:
		return validateChoice(q.Criteria.Names(), w)
	case TypeNoul:
		if w.Noul == nil || !unit(*w.Noul) {
			return Answer{}, errors.New("noul missing or outside [0, 1]")
		}
		return Answer{Type: TypeNoul, Noul: *w.Noul}, nil
	default:
		return Answer{}, fmt.Errorf("unsupported question type %q", q.Type)
	}
}

func validateChoice(options []string, w wireAnswer) (Answer, error) {
	known := make(map[string]bool, len(options))
	for _, o := range options {
		known[o] = true
	}
	if !known[w.Choice] {
		return Answer{}, fmt.Errorf("choice %q is not an option sent", w.Choice)
	}
	if w.Confidence == nil || !unit(*w.Confidence) {
		return Answer{}, errors.New("confidence missing or outside [0, 1]")
	}
	if len(w.Probabilities) != len(known) {
		return Answer{}, fmt.Errorf("%d probabilities for %d options", len(w.Probabilities), len(known))
	}
	var sum float64
	for o, p := range w.Probabilities {
		if !known[o] {
			return Answer{}, fmt.Errorf("probability for unknown option %q", o)
		}
		if !unit(p) {
			return Answer{}, fmt.Errorf("probability of %q outside [0, 1]", o)
		}
		sum += p
	}
	if math.Abs(sum-1) > probabilitySlack {
		return Answer{}, fmt.Errorf("probabilities sum to %g", sum)
	}
	return Answer{Type: TypeChoice, Choice: w.Choice, Probabilities: w.Probabilities, Confidence: *w.Confidence}, nil
}

// unit reports whether v is finite and in [0, 1].
func unit(v float64) bool {
	return !math.IsNaN(v) && v >= 0 && v <= 1
}
