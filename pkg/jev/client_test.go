package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	answers, err := c.Ask(t.Context(), testRequest())
	require.NoError(t, err)
	assert.Equal(t, map[string]Answer{"route": {
		Type: TypeChoice, Choice: "a", Probabilities: map[string]float64{"a": 0.7, "b@high": 0.3}, Confidence: 0.6,
	}}, answers)
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
	answers, err := c.Ask(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, "b@high", answers["route"].Choice)
	assert.Equal(t, Answer{Type: TypeNoul, Noul: 0.25}, answers["relevance"])
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
			for _, s := range []error{ErrUnauthorized, ErrUnprocessable, ErrOverloaded, ErrMalformed} {
				assert.Equal(t, errors.Is(s, tt.sentinel), errors.Is(err, s), s.Error())
			}
		})
	}
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
		answers, err := c.Ask(t.Context(), testRequest())
		require.NoError(t, err)
		assert.Equal(t, "a", answers["route"].Choice)
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
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestAskNetworkError(t *testing.T) {
	c := New(testKey)
	c.URL = "http://127.0.0.1:1/" + testKey
	_, err := c.Ask(t.Context(), testRequest())
	require.Error(t, err)
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
	}{
		{"invalid JSON", `{"answers":`},
		{"NaN literal", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":NaN}`)},
		{"missing route answer", `{"answers":{"other":{}}}`},
		{"no answers", `{}`},
		{"unknown choice", answer(`{"choice":"z","probabilities":{"a":1,"b@high":0},"confidence":1}`)},
		{"empty choice", answer(`{"probabilities":{"a":1,"b@high":0},"confidence":1}`)},
		{"missing confidence", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0}}`)},
		{"confidence above 1", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":1.5}`)},
		{"confidence below 0", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":-0.1}`)},
		{"confidence overflow", answer(`{"choice":"a","probabilities":{"a":1,"b@high":0},"confidence":1e999}`)},
		{"missing probability", answer(`{"choice":"a","probabilities":{"a":1},"confidence":1}`)},
		{"extra probability", answer(`{"choice":"a","probabilities":{"a":0.5,"b@high":0.5,"c":0},"confidence":1}`)},
		{"wrong probability key", answer(`{"choice":"a","probabilities":{"a":0.5,"c":0.5},"confidence":1}`)},
		{"probability above 1", answer(`{"choice":"a","probabilities":{"a":1.2,"b@high":-0.2},"confidence":1}`)},
		{"sum too low", answer(`{"choice":"a","probabilities":{"a":0.5,"b@high":0.48},"confidence":1}`)},
		{"sum too high", answer(`{"choice":"a","probabilities":{"a":0.6,"b@high":0.42},"confidence":1}`)},
		{"wrong type", answer(`{"type":"noul","noul":0.5}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, reply(http.StatusOK, tt.body))
			_, err := c.Ask(t.Context(), testRequest())
			require.ErrorIs(t, err, ErrMalformed)
		})
	}
}

func TestAskProbabilitySlack(t *testing.T) {
	c := newTestClient(t, reply(http.StatusOK,
		`{"answers":{"route":{"choice":"a","probabilities":{"a":0.5,"b@high":0.505},"confidence":0}}}`))
	_, err := c.Ask(t.Context(), testRequest())
	require.NoError(t, err)
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
	req := Request{Questions: map[string]Question{"route": {Type: "score"}}}
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
	assert.Equal(t, time.Duration(0), c.delay(3, withRetry))
	withRetry.Set("Retry-After", "30")
	assert.Equal(t, time.Second, c.delay(0, withRetry))
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
