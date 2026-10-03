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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultURL is the Jev endpoint.
const DefaultURL = "https://api.typesafe.ai/v1/systemone"

// Limits on what is read back, so a misbehaving server cannot exhaust memory.
const (
	maxResponseBytes = 4 << 20
	maxErrorBody     = 512
	redacted         = "<redacted>"
	// scannedErrorBody bounds the part of an error body checked for excerpts of the state; the rest
	// is cut, since at most maxErrorBody of it is shown.
	scannedErrorBody = 8 * maxErrorBody
	// minEcho is the shortest run of bytes shared with a state string that is redacted as an excerpt.
	minEcho = 12
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

// StatusError is a non-200 response. Body is truncated and has the key and the state's text redacted.
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
	var last *StatusError
	for attempt := 0; ; attempt++ {
		status, data, header, postErr := c.post(ctx, body)
		if postErr != nil {
			// a retry cut off by the deadline is still the overload that forced it
			if last != nil && ctx.Err() != nil {
				return nil, last
			}
			return nil, c.redact(fmt.Errorf("jev: %w", postErr))
		}
		if status == http.StatusOK {
			answers, decodeErr := decode(data, req.Questions)
			return answers, c.redact(decodeErr)
		}
		serr := &StatusError{Status: status, Body: c.errorBody(data, req.State)}
		if !errors.Is(serr, ErrOverloaded) {
			return nil, serr
		}
		if !c.wait(ctx, c.delay(attempt, header)) {
			return nil, serr
		}
		last = serr
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
// from MinBackoff; never below MinBackoff, so Retry-After: 0 cannot spin, nor above MaxBackoff.
// The seconds are clamped before conversion, so a huge Retry-After cannot overflow time.Duration;
// a digit string past uint64 is still valid delay-seconds and waits MaxBackoff. ParseUint reports
// ErrRange before reading past the overflow, so the all-digits check keeps "<huge>x" malformed.
func (c *Client) delay(attempt int, header http.Header) time.Duration {
	value := header.Get("Retry-After")
	secs, err := strconv.ParseUint(value, 10, 64)
	if errors.Is(err, strconv.ErrRange) && allDigits(value) {
		return c.MaxBackoff
	}
	if err == nil {
		if secs > uint64(c.MaxBackoff/time.Second) {
			return c.MaxBackoff
		}
		return min(max(time.Duration(secs)*time.Second, c.MinBackoff), c.MaxBackoff)
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

// allDigits reports whether s is a non-empty run of ASCII digits, the delay-seconds grammar.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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

// errorBody is the start of an error response on one line, with the key, every string of state and
// every excerpt of one redacted: a validation error may echo the request whole or in part, and the
// prompt must not reach stderr.
func (c *Client) errorBody(data []byte, state any) string {
	s := c.redactString(string(data))
	values := stateStrings(state)
	for _, v := range values {
		s = strings.ReplaceAll(s, v, redacted)
	}
	if len(s) > scannedErrorBody {
		s = s[:scannedErrorBody]
	}
	s = redactExcerpts(s, values)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "..."
	}
	return s
}

// redactExcerpts replaces every run of s that shares at least minEcho consecutive bytes with one of
// values, such as a prefix of the prompt or a truncated excerpt of a file. Runs are widened to whole
// UTF-8 characters.
func redactExcerpts(s string, values []string) string {
	if len(s) < minEcho {
		return s
	}
	windows := make(map[string][]int, len(s)-minEcho+1)
	for i := 0; i+minEcho <= len(s); i++ {
		windows[s[i:i+minEcho]] = append(windows[s[i:i+minEcho]], i)
	}
	hit := make([]bool, len(s))
	found := false
	for _, v := range values {
		for j := 0; j+minEcho <= len(v); j++ {
			starts, ok := windows[v[j:j+minEcho]]
			if !ok {
				continue
			}
			for _, i := range starts {
				for k := i; k < i+minEcho; k++ {
					hit[k] = true
				}
			}
			found = true
			delete(windows, v[j:j+minEcho])
		}
	}
	if !found {
		return s
	}
	// widen each run to whole characters: backwards to its first character's start byte, forwards
	// over the continuation bytes of its last
	for i := len(s) - 1; i > 0; i-- {
		if hit[i] && !utf8.RuneStart(s[i]) {
			hit[i-1] = true
		}
	}
	for i := 1; i < len(s); i++ {
		if hit[i-1] && !utf8.RuneStart(s[i]) {
			hit[i] = true
		}
	}
	var b strings.Builder
	for i := range len(s) {
		switch {
		case !hit[i]:
			b.WriteByte(s[i])
		case i == 0 || !hit[i-1]:
			b.WriteString(redacted)
		}
	}
	return b.String()
}

func (c *Client) redactString(s string) string {
	if c.key == "" {
		return s
	}
	return strings.ReplaceAll(s, c.key, redacted)
}

// stateStrings returns every string value in state, as is and JSON-escaped, longest first so a
// string is redacted before any shorter one inside it.
func stateStrings(state any) []string {
	data, err := json.Marshal(state)
	if err != nil {
		return nil
	}
	var tree any
	if json.Unmarshal(data, &tree) != nil {
		return nil
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if v == "" {
				return
			}
			out = append(out, v)
			escaped := jsonEscaped(v)
			for i, e := range escaped {
				if e != v && (i == 0 || e != escaped[0]) {
					out = append(out, e)
				}
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(tree)
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// jsonEscaped returns v as a JSON string body with and without HTML escaping: Go escapes <, > and &,
// while other servers echo them as is.
func jsonEscaped(v string) []string {
	out := make([]string, 0, 2)
	for _, html := range []bool{true, false} {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		_ = enc.Encode(v) // a string always encodes
		q := strings.TrimSuffix(b.String(), "\n")
		out = append(out, q[1:len(q)-1])
	}
	return out
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
