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
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKey = "sk-test-secret-key"

func routeQuestion() Question {
	return Question{
		Type:         TypeChoice,
		Instructions: map[string]any{"question": "which option?"},
		Criteria: Criteria{
			{Name: "b@high", Value: map[string]string{"cli": "x", "model": "b", "effort": "high"}},
			{Name: "a", Value: "plain description"},
		},
	}
}

func relevanceQuestion() Question {
	return Question{
		Type:         TypeNoul,
		Instructions: "does the chunk matter?",
		Criteria:     Criteria{{Name: "true", Value: "yes"}, {Name: "false", Value: "no"}},
	}
}

func testRequest() Request {
	return Request{
		Model:     "jev-latest",
		State:     map[string]string{"prompt": "fix it"},
		Questions: map[string]Question{"route": routeQuestion()},
	}
}

const okBody = `{"model":"jev-1.13.0","answers":{"route":{"type":"choice","choice":"a",` +
	`"probabilities":{"a":0.7,"b@high":0.3},"confidence":0.6}},"usage":{"input_tokens":10,"output_tokens":1}}`

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(testKey)
	c.URL = srv.URL
	c.MinBackoff = time.Millisecond
	c.MaxBackoff = 5 * time.Millisecond
	return c
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestCriteriaMarshalKeepsOrder(t *testing.T) {
	data, err := json.Marshal(routeQuestion())
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"choice","instructions":{"question":"which option?"},
		"criteria":{"b@high":{"cli":"x","effort":"high","model":"b"},"a":"plain description"}}`, string(data))
	assert.Less(t, bytes.Index(data, []byte(`"b@high"`)), bytes.Index(data, []byte(`"a"`)))

	empty, err := json.Marshal(Criteria{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(empty))

	_, err = json.Marshal(Criteria{{Name: "bad", Value: math.NaN()}})
	require.Error(t, err)
}

func TestAskOK(t *testing.T) {
	var got struct {
		auth, contentType string
		body              map[string]any
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		got.auth = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got.body))
		_, _ = io.WriteString(w, okBody)
	})

	resp, err := c.Ask(t.Context(), testRequest())
	require.NoError(t, err)
	assert.Equal(t, map[string]Answer{"route": {
		Type: TypeChoice, Choice: "a", Probabilities: map[string]float64{"a": 0.7, "b@high": 0.3}, Confidence: 0.6,
	}}, resp.Answers)
	assert.Equal(t, "Bearer "+testKey, got.auth)
	assert.Equal(t, "application/json", got.contentType)
	assert.Equal(t, "jev-latest", got.body["model"])
	assert.Equal(t, map[string]any{"prompt": "fix it"}, got.body["state"])
	assert.NotContains(t, got.body, "key")
}

func TestAskKeyOnlyInHeader(t *testing.T) {
	var body []byte
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		body, err = io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NotContains(t, r.URL.String(), testKey)
		_, _ = io.WriteString(w, okBody)
	})
	_, err := c.Ask(t.Context(), testRequest())
	require.NoError(t, err)
	assert.NotContains(t, string(body), testKey)
}

func TestAskNoKey(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) })
	c.key = ""
	_, err := c.Ask(t.Context(), testRequest())
	require.ErrorIs(t, err, ErrNoKey)
	assert.Zero(t, calls.Load())
}

func TestAskChunkRequest(t *testing.T) {
	c := newTestClient(t, reply(http.StatusOK, `{"answers":{
		"route":{"type":"choice","choice":"b@high","probabilities":{"a":0.2,"b@high":0.8},"confidence":0.9},
		"relevance":{"type":"noul","noul":0.25}}}`))
	req := testRequest()
	req.Questions["relevance"] = relevanceQuestion()
	resp, err := c.Ask(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, "b@high", resp.Answers["route"].Choice)
	assert.Equal(t, Answer{Type: TypeNoul, Noul: 0.25}, resp.Answers["relevance"])
}

func TestAskStatusErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		sentinel error
	}{
		{"401", http.StatusUnauthorized, ErrUnauthorized},
		{"422", http.StatusUnprocessableEntity, ErrUnprocessable},
		{"500", http.StatusInternalServerError, nil},
		{"400", http.StatusBadRequest, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"error":"bad\nthing"}`)
			})
			_, err := c.Ask(t.Context(), testRequest())
			var serr *StatusError
			require.ErrorAs(t, err, &serr)
			assert.Equal(t, tt.status, serr.Status)
			assert.JSONEq(t, `{"error":"bad\nthing"}`, serr.Body)
			assert.Equal(t, int32(1), calls.Load(), "not retried")
			for _, s := range []error{ErrUnauthorized, ErrUnprocessable, ErrTooLong, ErrOverloaded, ErrMalformed} {
				assert.Equal(t, errors.Is(s, tt.sentinel), errors.Is(err, s), s.Error())
			}
		})
	}
}

func TestLimits(t *testing.T) {
	tests := []struct {
		name                    string
		longContext             bool
		perQuestion, perRequest int
	}{
		{name: "default", perQuestion: 32_000, perRequest: 64_000},
		{name: "long context", longContext: true, perQuestion: 262_000, perRequest: 262_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perQuestion, perRequest := Limits(tt.longContext)
			assert.Equal(t, tt.perQuestion, perQuestion)
			assert.Equal(t, tt.perRequest, perRequest)
		})
	}
}

func TestAskLongContextHeader(t *testing.T) {
	tests := []struct {
		name        string
		longContext bool
		want        []string
	}{
		{name: "default sends no header"},
		{name: "long context sends the header", longContext: true, want: []string{"true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Values("TypeSafe-Danger-Zone-Long-Context")
				_, _ = io.WriteString(w, okBody)
			})
			c.LongContext = tt.longContext
			_, err := c.Ask(t.Context(), testRequest())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAskTooLong(t *testing.T) {
	const tooLong = `{"detail":{"error_type":"max_tokens_exceeded"}}`
	tests := []struct {
		name      string
		status    int
		body      string
		tooLong   bool
		errorType string
	}{
		{name: "structured 400", status: http.StatusBadRequest, body: tooLong, tooLong: true, errorType: "max_tokens_exceeded"},
		{name: "structured 400 with other detail fields", status: http.StatusBadRequest, tooLong: true,
			body: `{"detail":{"message":"too long","error_type":"max_tokens_exceeded","limit":64000}}`, errorType: "max_tokens_exceeded"},
		{name: "other error type", status: http.StatusBadRequest, body: `{"detail":{"error_type":"invalid_request"}}`,
			errorType: "invalid_request"},
		{name: "400 without a body", status: http.StatusBadRequest},
		{name: "400 with a text body", status: http.StatusBadRequest, body: "max_tokens_exceeded"},
		{name: "400 with a string detail", status: http.StatusBadRequest, body: `{"detail":"max_tokens_exceeded"}`},
		{name: "400 with a top-level error type", status: http.StatusBadRequest, body: `{"error_type":"max_tokens_exceeded"}`},
		{name: "400 with a malformed body", status: http.StatusBadRequest, body: `{"detail":{"error_type":"max_tokens_exceeded"`},
		{name: "400 with a truncated body", status: http.StatusBadRequest, body: `{"detail":{"error_type":"max_tokens_`},
		{name: "400 with a non-string type", status: http.StatusBadRequest, body: `{"detail":{"error_type":7}}`},
		{name: "400 with an overlong type", status: http.StatusBadRequest,
			body: `{"detail":{"error_type":"` + strings.Repeat("x", 100) + `"}}`},
		{name: "422 with the structured body", status: http.StatusUnprocessableEntity, body: tooLong, errorType: "max_tokens_exceeded"},
		{name: "500 with the structured body", status: http.StatusInternalServerError, body: tooLong, errorType: "max_tokens_exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			_, err := c.Ask(t.Context(), testRequest())
			var serr *StatusError
			require.ErrorAs(t, err, &serr)
			assert.Equal(t, tt.status, serr.Status)
			assert.Equal(t, tt.errorType, serr.ErrorType)
			assert.Equal(t, tt.tooLong, errors.Is(err, ErrTooLong))
			assert.Equal(t, tt.status == http.StatusUnprocessableEntity, errors.Is(err, ErrUnprocessable))
			require.NotErrorIs(t, err, ErrOverloaded)
			assert.Equal(t, int32(1), calls.Load(), "not retried")
		})
	}
}

func TestAskTooLongErrorBody(t *testing.T) {
	t.Run("the type is read before the body is truncated", func(t *testing.T) {
		body := `{"detail":{"padding":"` + strings.Repeat("x", 5000) + `","error_type":"max_tokens_exceeded"}}`
		c := newTestClient(t, reply(http.StatusBadRequest, body))
		_, err := c.Ask(t.Context(), testRequest())
		require.ErrorIs(t, err, ErrTooLong)
		var serr *StatusError
		require.ErrorAs(t, err, &serr)
		assert.Len(t, serr.Body, maxErrorBody+3)
		assert.NotContains(t, serr.Body, "max_tokens_exceeded")
	})
	t.Run("the body still shows the type and redacts the state", func(t *testing.T) {
		body := `{"detail":{"error_type":"max_tokens_exceeded","echo":"fix it"}}`
		c := newTestClient(t, reply(http.StatusBadRequest, body))
		_, err := c.Ask(t.Context(), testRequest())
		require.ErrorIs(t, err, ErrTooLong)
		assert.Contains(t, err.Error(), "max_tokens_exceeded")
		assert.NotContains(t, err.Error(), "fix it")
	})
}

func TestAskInputTokens(t *testing.T) {
	const answer = `"answers":{"route":{"type":"choice","choice":"a","probabilities":{"a":0.7,"b@high":0.3},"confidence":0.6}}`
	ptr := func(n int) *int { return &n }
	tests := []struct {
		name string
		body string
		want *int
	}{
		{name: "reported", body: `{` + answer + `,"usage":{"input_tokens":1234,"output_tokens":1}}`, want: ptr(1234)},
		{name: "zero", body: `{` + answer + `,"usage":{"input_tokens":0}}`, want: ptr(0)},
		{name: "usage absent", body: `{` + answer + `}`},
		{name: "usage null", body: `{` + answer + `,"usage":null}`},
		{name: "usage empty", body: `{` + answer + `,"usage":{}}`},
		{name: "usage a number", body: `{` + answer + `,"usage":5}`},
		{name: "usage a list", body: `{` + answer + `,"usage":[12]}`},
		{name: "input tokens null", body: `{` + answer + `,"usage":{"input_tokens":null}}`},
		{name: "input tokens a string", body: `{` + answer + `,"usage":{"input_tokens":"12"}}`},
		{name: "input tokens a boolean", body: `{` + answer + `,"usage":{"input_tokens":true}}`},
		{name: "input tokens an object", body: `{` + answer + `,"usage":{"input_tokens":{"n":12}}}`},
		{name: "input tokens fractional", body: `{` + answer + `,"usage":{"input_tokens":12.5}}`},
		{name: "input tokens negative", body: `{` + answer + `,"usage":{"input_tokens":-3}}`},
		{name: "input tokens beyond int64", body: `{` + answer + `,"usage":{"input_tokens":99999999999999999999}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, reply(http.StatusOK, tt.body))
			resp, err := c.Ask(t.Context(), testRequest())
			require.NoError(t, err, "usage never fails a valid answer")
			assert.Equal(t, "a", resp.Answers["route"].Choice)
			assert.Equal(t, tt.want, resp.InputTokens)
		})
	}
}

func TestAskInputTokensOnFailure(t *testing.T) {
	c := newTestClient(t, reply(http.StatusOK, `{"answers":{},"usage":{"input_tokens":7}}`))
	resp, err := c.Ask(t.Context(), testRequest())
	require.ErrorIs(t, err, ErrMalformed)
	assert.Equal(t, Response{}, resp, "no answers or tokens beside an error")
}

func TestAskRetriesThenSucceeds(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, 529} {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) <= 2 {
				w.WriteHeader(status)
				return
			}
			_, _ = io.WriteString(w, okBody)
		})
		resp, err := c.Ask(t.Context(), testRequest())
		require.NoError(t, err)
		assert.Equal(t, "a", resp.Answers["route"].Choice)
		assert.Equal(t, int32(3), calls.Load())
	}
}

func TestAskOverloadedExhaustedWithinDeadline(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, 529} {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
		})
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		start := time.Now()
		_, err := c.Ask(ctx, testRequest())
		cancel()
		require.ErrorIs(t, err, ErrOverloaded)
		assert.Less(t, time.Since(start), time.Second)
		assert.Greater(t, calls.Load(), int32(1), "retried")
	}
}

func TestAskRetryAfterPastDeadlineStops(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(529)
	})
	c.MaxBackoff = 10 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Ask(ctx, testRequest())
	require.ErrorIs(t, err, ErrOverloaded)
	assert.Equal(t, int32(1), calls.Load())
	assert.Less(t, time.Since(start), 150*time.Millisecond, "does not sleep past the deadline")
}

func TestAskSlowResponseHitsDeadline(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) }) // runs before the server closes
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Ask(ctx, testRequest())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrTransport, "cut off in the transport")
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestAskRetryCutByDeadlineReportsOverload(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(529)
			return
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) }) // runs before the server closes
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Ask(ctx, testRequest())
	require.ErrorIs(t, err, ErrOverloaded, "the deadline ended the retry, not the overload")
	assert.Equal(t, int32(2), calls.Load())
}

func TestAskNetworkError(t *testing.T) {
	c := New(testKey)
	c.URL = "http://127.0.0.1:1/" + testKey
	_, err := c.Ask(t.Context(), testRequest())
	require.ErrorIs(t, err, ErrTransport)
	require.NotErrorIs(t, err, ErrMalformed)
	assert.NotContains(t, err.Error(), testKey)
	assert.Contains(t, err.Error(), redacted)
}

func TestAskRedactsEchoedKey(t *testing.T) {
	c := newTestClient(t, reply(http.StatusUnauthorized, `{"error":"invalid key `+testKey+`"}`))
	_, err := c.Ask(t.Context(), testRequest())
	require.ErrorIs(t, err, ErrUnauthorized)
	assert.NotContains(t, err.Error(), testKey)
	assert.Contains(t, err.Error(), redacted)

	c = newTestClient(t, reply(http.StatusOK, `{"answers":{"route":{"choice":"`+testKey+`"}}}`))
	_, err = c.Ask(t.Context(), testRequest())
	require.ErrorIs(t, err, ErrMalformed)
	assert.NotContains(t, err.Error(), testKey)
}

func TestAskRedactsEchoedState(t *testing.T) {
	req := testRequest()
	req.State = map[string]any{"prompt": "fix the \"secret\" bug\nnow", "files": []string{"token=abc"}}
	echo := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"detail":[{"loc":["body","state"],"input":`+string(body)+`}]}`)
	}
	_, err := newTestClient(t, echo).Ask(t.Context(), req)
	require.ErrorIs(t, err, ErrUnprocessable)
	assert.NotContains(t, err.Error(), "secret")
	assert.NotContains(t, err.Error(), "token=abc")
	assert.Contains(t, err.Error(), `"loc":["body","state"]`)
}

func TestAskRedactsStateExcerpts(t *testing.T) {
	req := testRequest()
	req.State = map[string]any{"prompt": "rotate the staging credentials in vault/prod.hcl before Friday",
		"files": []string{"password = hunter2-correct-horse"}}
	// a validation error quoting only a prefix and a truncated middle of the inputs
	body := `{"detail":"input too long: 'rotate the staging cred...' and 'word = hunter2-corr'"}`
	_, err := newTestClient(t, reply(http.StatusUnprocessableEntity, body)).Ask(t.Context(), req)
	require.ErrorIs(t, err, ErrUnprocessable)
	assert.NotContains(t, err.Error(), "staging")
	assert.NotContains(t, err.Error(), "hunter2")
	assert.Contains(t, err.Error(), `{"detail":"input too long: '<redacted>...' and '<redacted>'"}`)
}

func TestAskRedactsStateEchoedWithoutHTMLEscaping(t *testing.T) {
	req := testRequest()
	req.State = map[string]any{"prompt": "if a < b && c > d {\n\treturn\n}"}
	// a server that escapes the newlines and tab but writes <, > and & as is
	body := `{"detail":[{"input":"if a < b && c > d {\n\treturn\n}"}]}`
	_, err := newTestClient(t, reply(http.StatusUnprocessableEntity, body)).Ask(t.Context(), req)
	require.ErrorIs(t, err, ErrUnprocessable)
	assert.Contains(t, err.Error(), `{"detail":[{"input":"<redacted>"}]}`)
}

func TestAskRedactsMultilineExcerpt(t *testing.T) {
	req := testRequest()
	req.State = map[string]any{"prompt": "line one of the plan\nline two of the plan"}
	body := `{"detail":"too long: 'line one of the plan\nline two...'"}`
	_, err := newTestClient(t, reply(http.StatusUnprocessableEntity, body)).Ask(t.Context(), req)
	require.ErrorIs(t, err, ErrUnprocessable)
	assert.NotContains(t, err.Error(), "line one")
	assert.NotContains(t, err.Error(), "line two")
}

func TestRedactExcerpts(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		values []string
		want   string
	}{
		{name: "short body", s: "short", values: []string{"short"}, want: "short"},
		{name: "no overlap", s: "nothing in common here", values: []string{"completely different text"},
			want: "nothing in common here"},
		{name: "shorter than minEcho kept", s: "error: the cat sat", values: []string{"the cat sat on"},
			want: "error: the cat sat"},
		{name: "two runs", s: "a=0123456789abcdef b=0123456789abcdef!", values: []string{"0123456789abcdef"},
			want: "a=<redacted> b=<redacted>!"},
		{name: "whole characters", s: "x:ééééééé;", values: []string{"aéééééé"}, want: "x:<redacted>;"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, redactExcerpts(tc.s, tc.values))
		})
	}
}

func TestAskTruncatesErrorBody(t *testing.T) {
	c := newTestClient(t, reply(http.StatusInternalServerError, strings.Repeat("x", 5000)))
	_, err := c.Ask(t.Context(), testRequest())
	var serr *StatusError
	require.ErrorAs(t, err, &serr)
	assert.Len(t, serr.Body, maxErrorBody+3)
	assert.Equal(t, "jev: HTTP 500: "+serr.Body, serr.Error())
	assert.Equal(t, "jev: HTTP 502", (&StatusError{Status: 502}).Error())
}

func TestAskMalformed(t *testing.T) {
	answer := func(route string) string { return `{"answers":{"route":` + route + `}}` }
	tests := []struct {
		name string
		body string
		want error // ErrEmptyAnswer or ErrInvalidAnswer
	}{
		{"invalid JSON", `{"answers":`, ErrInvalidAnswer},
		{"NaN literal", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":NaN}`), ErrInvalidAnswer},
		{"missing route answer", `{"answers":{"other":{}}}`, ErrEmptyAnswer},
		{"no answers", `{}`, ErrEmptyAnswer},
		{"unknown choice", answer(`{"choice":"z","probabilities":{"a":1,"b@high":0},"confidence":1}`), ErrInvalidAnswer},
		{"empty choice", answer(`{"probabilities":{"a":1,"b@high":0},"confidence":1}`), ErrEmptyAnswer},
		{"missing confidence", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0}}`), ErrInvalidAnswer},
		{"confidence above 1", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":1.5}`), ErrInvalidAnswer},
		{"confidence below 0", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":-0.1}`), ErrInvalidAnswer},
		{"confidence overflow", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":1e999}`), ErrInvalidAnswer},
		{"missing probability", answer(`{"choice":"a","probabilities":{"a":1},"confidence":1}`), ErrInvalidAnswer},
		{"extra probability", answer(`{"choice":"a","probabilities":{"a":0.5,"b@high":0.5,"c":0},"confidence":1}`), ErrInvalidAnswer},
		{"wrong probability key", answer(`{"choice":"a","probabilities":{"a":0.5,"c":0.5},"confidence":1}`), ErrInvalidAnswer},
		{"probability above 1", answer(`{"choice":"a","probabilities":{"a":1.2,"b@high":-0.2},"confidence":1}`), ErrInvalidAnswer},
		{"sum too low", answer(`{"choice":"a","probabilities":{"a":0.5,"b@high":0.48},"confidence":1}`), ErrInvalidAnswer},
		{"sum too high", answer(`{"choice":"a","probabilities":{"a":0.6,"b@high":0.42},"confidence":1}`), ErrInvalidAnswer},
		{"wrong type", answer(`{"type":"noul","noul":0.5}`), ErrInvalidAnswer},
		{"choice question without a choice", answer(`{"type":"choice","confidence":1}`), ErrEmptyAnswer},
		{"null answer", answer(`null`), ErrEmptyAnswer},
		{"empty answer", answer(`{}`), ErrEmptyAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, reply(http.StatusOK, tt.body))
			_, err := c.Ask(t.Context(), testRequest())
			require.ErrorIs(t, err, ErrMalformed)
			require.ErrorIs(t, err, tt.want)
			other := map[error]error{ErrEmptyAnswer: ErrInvalidAnswer, ErrInvalidAnswer: ErrEmptyAnswer}[tt.want]
			require.NotErrorIs(t, err, other)
		})
	}
}

func TestAskProbabilitySlack(t *testing.T) {
	tests := []struct {
		name      string
		other     float64
		malformed bool
	}{
		{name: "within slack", other: 0.505},
		{name: "lower boundary", other: 0.49},
		{name: "upper boundary", other: 0.51},
		{name: "lower boundary roundoff", other: math.Nextafter(0.49, 0)},
		{name: "upper boundary roundoff", other: math.Nextafter(0.51, 1)},
		{name: "below lower boundary", other: 0.4899999999, malformed: true},
		{name: "above upper boundary", other: 0.5100000001, malformed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"answers":{"route":{"choice":"a","probabilities":{"a":0.5,"b@high":%g},"confidence":0}}}`, tc.other)
			c := newTestClient(t, reply(http.StatusOK, body))
			resp, err := c.Ask(t.Context(), testRequest())
			if tc.malformed {
				require.ErrorIs(t, err, ErrMalformed)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, map[string]float64{"a": 0.5, "b@high": tc.other}, resp.Answers["route"].Probabilities)
		})
	}
}

func TestAskProbabilitySlackManyOptions(t *testing.T) {
	for _, count := range []int{33, 255} {
		for _, total := range []float64{0.99, 1.01} {
			t.Run(fmt.Sprintf("%d options totaling %g", count, total), func(t *testing.T) {
				req := testRequest()
				q := routeQuestion()
				q.Criteria = nil
				probabilities := make(map[string]float64, count)
				for i := range count {
					name := fmt.Sprintf("option-%d", i)
					q.Criteria = append(q.Criteria, Criterion{Name: name, Value: "test option"})
					probabilities[name] = total / float64(count)
				}
				req.Questions["route"] = q
				body, err := json.Marshal(map[string]any{"answers": map[string]any{"route": map[string]any{
					"choice": "option-0", "probabilities": probabilities, "confidence": 0,
				}}})
				require.NoError(t, err)
				c := newTestClient(t, reply(http.StatusOK, string(body)))
				resp, err := c.Ask(t.Context(), req)
				require.NoError(t, err)
				assert.Equal(t, probabilities, resp.Answers["route"].Probabilities)
			})
		}
	}
}

func TestAskMalformedNoul(t *testing.T) {
	route := `"route":{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":1}`
	for name, relevance := range map[string]string{
		"missing relevance": ``,
		"missing noul":      `,"relevance":{"type":"noul"}`,
		"noul above 1":      `,"relevance":{"noul":1.01}`,
		"noul below 0":      `,"relevance":{"noul":-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, reply(http.StatusOK, `{"answers":{`+route+relevance+`}}`))
			req := testRequest()
			req.Questions["relevance"] = relevanceQuestion()
			_, err := c.Ask(t.Context(), req)
			require.ErrorIs(t, err, ErrMalformed)
		})
	}
}

func TestAskUnsupportedQuestionType(t *testing.T) {
	c := newTestClient(t, reply(http.StatusOK, `{"answers":{"route":{}}}`))
	req := Request{Questions: map[string]Question{"route": {Type: "unsupported"}}}
	_, err := c.Ask(t.Context(), req)
	require.ErrorIs(t, err, ErrMalformed)
}

func TestAskEncodeError(t *testing.T) {
	c := New(testKey)
	_, err := c.Ask(t.Context(), Request{State: math.Inf(1)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encode request")
}

func TestAskBadURL(t *testing.T) {
	c := New(testKey)
	c.URL = "://bad"
	_, err := c.Ask(t.Context(), testRequest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build request")
}

func TestDelay(t *testing.T) {
	c := New(testKey)
	c.MinBackoff = 100 * time.Millisecond
	c.MaxBackoff = time.Second
	none := http.Header{}
	assert.Equal(t, 100*time.Millisecond, c.delay(0, none))
	assert.Equal(t, 200*time.Millisecond, c.delay(1, none))
	assert.Equal(t, 800*time.Millisecond, c.delay(3, none))
	assert.Equal(t, time.Second, c.delay(4, none))
	assert.Equal(t, time.Second, c.delay(40, none))

	withRetry := http.Header{"Retry-After": []string{"0"}}
	assert.Equal(t, 100*time.Millisecond, c.delay(3, withRetry), "Retry-After: 0 still waits MinBackoff")
	withRetry.Set("Retry-After", "30")
	assert.Equal(t, time.Second, c.delay(0, withRetry))
	withRetry.Set("Retry-After", "10000000000")
	assert.Equal(t, time.Second, c.delay(0, withRetry), "a huge Retry-After is clamped, not overflowed")
	withRetry.Set("Retry-After", "99999999999999999999999")
	assert.Equal(t, time.Second, c.delay(0, withRetry), "a Retry-After past uint64 is clamped too")
	withRetry.Set("Retry-After", "184467440737095516160x")
	assert.Equal(t, 100*time.Millisecond, c.delay(0, withRetry), "an overflowing prefix with trailing junk is malformed")
	withRetry.Set("Retry-After", "-5")
	assert.Equal(t, 100*time.Millisecond, c.delay(0, withRetry), "a signed value is not delay-seconds")
	withRetry.Set("Retry-After", "Wed, 21 Oct 2015 07:28:00 GMT")
	assert.Equal(t, 100*time.Millisecond, c.delay(0, withRetry))
}

func TestWaitCanceled(t *testing.T) {
	c := New(testKey)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.False(t, c.wait(ctx, time.Second))
	assert.True(t, c.wait(t.Context(), time.Millisecond))
}
