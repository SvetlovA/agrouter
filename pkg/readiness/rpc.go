package readiness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

// JSON-RPC methods. These reads and the initialized notification are the only messages the
// jsonrpc-ratelimits-v1 transport sends: it never starts a thread or a turn, logs in or out, or
// writes configuration.
const (
	rpcInitialize  = "initialize"
	rpcInitialized = "initialized"
	rpcAccount     = "account/read"
	rpcRateLimits  = "account/rateLimits/read"
	rpcConfig      = "config/read"
	rpcModels      = "model/list"
)

var rpcReads = []string{rpcInitialize, rpcAccount, rpcRateLimits, rpcConfig, rpcModels}

const (
	// rpcModelPages bounds the model list pagination.
	rpcModelPages = 20
	// rpcShutdownGrace is how long the server may take to exit once its stdin closes.
	rpcShutdownGrace = time.Second
	// rpcSubscriptionAccount is the account type whose usage is metered by subscription windows.
	rpcSubscriptionAccount = "chatgpt"
	// rpcAPIKeyAccount is the API-key account type, which has no subscription windows.
	rpcAPIKeyAccount = "apiKey"
	// throttleErrorInfo is the error info that says a read was itself rate limited.
	throttleErrorInfo = "rateLimitExceeded"
)

// Window durations, in minutes, the server reports for the subscription windows.
var rpcWindows = map[int64]string{300: WindowFiveHour, 10080: WindowWeekly}

// rpcMessage is one stdout line. Lines with a method are server notifications or requests and are
// skipped; only responses to this session's ids are read.
type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code int64           `json:"code"`
	Data json.RawMessage `json:"data"` // never printed
}

// throttled reports whether the error says the read itself was rate limited: the error info, or an
// HTTP 429 status anywhere in the error data.
func (e *rpcError) throttled() bool {
	if e.Code == 429 {
		return true
	}
	var data any
	if len(e.Data) == 0 || json.Unmarshal(e.Data, &data) != nil {
		return false
	}
	return throttleIn(data)
}

func throttleIn(v any) bool {
	switch v := v.(type) {
	case string:
		return v == throttleErrorInfo
	case []any:
		return slices.ContainsFunc(v, throttleIn)
	case map[string]any:
		for k, x := range v {
			if n, ok := x.(float64); ok && n == 429 && strings.HasSuffix(k, "StatusCode") {
				return true
			}
			if throttleIn(x) {
				return true
			}
		}
	}
	return false
}

// rpcCall is one request: a read method and its params.
type rpcCall struct {
	method string
	params any
}

// rpcReply is one response: its result, or its error.
type rpcReply struct {
	result json.RawMessage
	err    *rpcError
}

// decode decodes a successful result into v, reporting whether it could.
func (r rpcReply) decode(v any) bool {
	return r.err == nil && json.Unmarshal(r.result, v) == nil
}

// rpcSession is one stdio JSON-RPC connection.
type rpcSession struct {
	probe *runner.Probe
	out   *bufio.Reader
	next  int
}

func (s *rpcSession) write(msg map[string]any) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode rpc message: %w", err)
	}
	if _, err := s.probe.Stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write rpc message: %w", err)
	}
	return nil
}

// notify sends the initialized notification, the only notification the transport sends.
func (s *rpcSession) notify(method string) error {
	if method != rpcInitialized {
		return fmt.Errorf("rpc notification %q is not allowed", method)
	}
	return s.write(map[string]any{"method": method})
}

// call sends the requests together and returns their replies in order.
func (s *rpcSession) call(calls ...rpcCall) ([]rpcReply, error) {
	pending := make(map[string]int, len(calls)) // request id -> index
	for i, c := range calls {
		if !slices.Contains(rpcReads, c.method) {
			return nil, fmt.Errorf("rpc method %q is not a read", c.method)
		}
		s.next++
		id := "agrouter-" + strconv.Itoa(s.next)
		params := c.params
		if params == nil {
			params = map[string]any{}
		}
		if err := s.write(map[string]any{"id": id, "method": c.method, "params": params}); err != nil {
			return nil, err
		}
		pending[id] = i
	}
	replies := make([]rpcReply, len(calls))
	for len(pending) > 0 {
		line, err := s.out.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var m rpcMessage
			var id string
			if json.Unmarshal(line, &m) == nil && m.Method == "" && json.Unmarshal(m.ID, &id) == nil {
				if i, ok := pending[id]; ok {
					replies[i] = rpcReply{result: m.Result, err: m.Error}
					delete(pending, id)
					continue
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("read rpc response: %w", err)
		}
	}
	return replies, nil
}

// runRPC runs a jsonrpc-ratelimits-v1 check: initialize, the account and rate-limit reads, then the
// effective configuration read, and the model list only when the configured model's default effort is
// needed.
func (c Checker) runRPC(ctx context.Context, cli config.CLI, check config.Check, r *Report) {
	p, err := runner.StartProbe(ctx, c.probeCommand(cli, check))
	if err != nil {
		r.add(unknownQuota(check.Name, ReasonProbeMissing, c.now()))
		return
	}
	s := &rpcSession{probe: p, out: bufio.NewReader(p.Stdout)}
	evidence, defaults, err := c.rpcExchange(s, check)
	if err != nil {
		p.Kill()
	}
	// the server exits when its stdin closes; one that lingers is killed
	stop := time.AfterFunc(rpcShutdownGrace, p.Kill)
	_, _ = p.Wait()
	stop.Stop()
	r.addExchange(ctx, check.Name, evidence, defaults, err, c.now())
}

func (c Checker) rpcExchange(s *rpcSession, check config.Check) ([]Evidence, Defaults, error) {
	res, err := s.call(rpcCall{method: rpcInitialize, params: map[string]any{
		"clientInfo": map[string]any{"name": "agrouter", "version": "0"},
	}})
	if err != nil {
		return nil, Defaults{}, err
	}
	if res[0].err != nil {
		return nil, Defaults{}, &exchangeError{ReasonProbeFailed}
	}
	if check.MinVersion != "" && !userAgentSupported(res[0].result, check.MinVersion) {
		return nil, Defaults{}, &exchangeError{ReasonVersionUnsupported}
	}
	err = s.notify(rpcInitialized)
	if err != nil {
		return nil, Defaults{}, err
	}

	dir, err := filepath.Abs(c.Dir)
	if err != nil {
		return nil, Defaults{}, &exchangeError{ReasonProbeFailed}
	}
	res, err = s.call(
		rpcCall{method: rpcAccount},
		rpcCall{method: rpcRateLimits, params: map[string]any{"excludeResetCreditDetails": true}},
	)
	if err != nil {
		return nil, Defaults{}, err
	}
	account, limits := res[0], res[1]
	if account.err != nil {
		if account.err.throttled() {
			return nil, Defaults{}, &exchangeError{ReasonProbeThrottled}
		}
		return nil, Defaults{}, &exchangeError{ReasonProbeFailed}
	}
	now := c.now()
	evidence, subscription, err := accountEvidence(account.result, check.Name, now)
	if err != nil {
		return nil, Defaults{}, &exchangeError{ReasonProbeMalformed}
	}
	if subscription {
		evidence = append(evidence, limitsEvidence(limits, check.Name, now)...)
	}
	// the defaults are read after the quota, so a connection lost reading them keeps the evidence
	res, err = s.call(rpcCall{method: rpcConfig, params: map[string]any{"cwd": dir}})
	if err != nil {
		return evidence, Defaults{}, err
	}
	// a model list lost on the way keeps the configured model, with its effort unknown
	defaults, err := configDefaults(s, res[0])
	return evidence, defaults, err
}

// userAgentSupported reports whether the initialize result's user agent ("<client>/<version> ...")
// carries a version at or above lowest.
func userAgentSupported(result json.RawMessage, lowest string) bool {
	var init struct {
		UserAgent string `json:"userAgent"`
	}
	if json.Unmarshal(result, &init) != nil {
		return false
	}
	product, _, _ := strings.Cut(init.UserAgent, " ")
	_, version, ok := strings.Cut(product, "/")
	if !ok {
		return false
	}
	cmp, ok := compareVersions(version, lowest)
	return ok && cmp >= 0
}

// accountEvidence normalizes the account read. subscription is true when the account is metered by
// subscription windows, so the rate limits apply.
func accountEvidence(data json.RawMessage, check string, now time.Time) ([]Evidence, bool, error) {
	var a struct {
		Account *struct {
			Type string `json:"type"`
		} `json:"account"`
		RequiresAuth *bool `json:"requiresOpenaiAuth"`
	}
	if err := json.Unmarshal(data, &a); err != nil || a.RequiresAuth == nil {
		return nil, false, errors.New("account reply has no requiresOpenaiAuth")
	}
	notApplicable := Evidence{Dimension: DimensionQuota, Status: StatusNotApplicable, Reason: ReasonNotSubscription,
		Check: check, ObservedAt: now}
	switch {
	case !*a.RequiresAuth:
		// a custom provider: no account, no subscription windows
		return []Evidence{notApplicable}, false, nil
	case a.Account == nil:
		return []Evidence{{Dimension: DimensionAuth, Status: StatusUnavailable, Reason: ReasonAuthFailed,
			Check: check, ObservedAt: now}}, false, nil
	case a.Account.Type == rpcAPIKeyAccount:
		return []Evidence{notApplicable}, false, nil
	case a.Account.Type != rpcSubscriptionAccount:
		// a missing or unrecognized type proves neither a subscription nor its absence
		return nil, false, errors.New("account reply has an unrecognized type")
	}
	return []Evidence{{Dimension: DimensionAuth, Status: StatusAvailable, Check: check, ObservedAt: now}}, true, nil
}

// limitsReply is the rate-limit read: the account bucket and, keyed by limit id, every metered bucket.
type limitsReply struct {
	OrdinaryUsageAllowed *bool                 `json:"ordinaryUsageAllowed"`
	RateLimits           *rateBucket           `json:"rateLimits"`
	ByLimitID            map[string]rateBucket `json:"rateLimitsByLimitId"`
}

type rateBucket struct {
	LimitID         *string     `json:"limitId"`
	LimitName       *string     `json:"limitName"`
	NormalModelSlug *string     `json:"normalModelSlug"` // the model a scoped bucket meters
	Primary         *rateWindow `json:"primary"`
	Secondary       *rateWindow `json:"secondary"`
	Credits         *struct {
		HasCredits bool `json:"hasCredits"`
		Unlimited  bool `json:"unlimited"`
	} `json:"credits"`
	ReachedType         *string `json:"rateLimitReachedType"` // any value is a denial; never printed
	SpendControlReached *bool   `json:"spendControlReached"`
}

type rateWindow struct {
	UsedPercent *float64 `json:"usedPercent"`
	DurationMin *int64   `json:"windowDurationMins"`
	ResetsAt    *int64   `json:"resetsAt"` // unix seconds
}

// limitsEvidence normalizes the rate-limit read. A failed or malformed read is unknown quota.
func limitsEvidence(reply rpcReply, check string, now time.Time) []Evidence {
	quota := func(status Status, reason string) Evidence {
		return Evidence{Dimension: DimensionQuota, Status: status, Reason: reason, Check: check, ObservedAt: now}
	}
	if reply.err != nil {
		if reply.err.throttled() {
			return []Evidence{quota(StatusUnknown, ReasonProbeThrottled)}
		}
		return []Evidence{quota(StatusUnknown, ReasonProbeFailed)}
	}
	var l limitsReply
	if err := json.Unmarshal(reply.result, &l); err != nil || l.RateLimits == nil {
		return []Evidence{quota(StatusUnknown, ReasonProbeMalformed)}
	}
	evidence, err := l.evidence(quota)
	if err != nil {
		return []Evidence{quota(StatusUnknown, ReasonProbeMalformed)}
	}
	return evidence
}

func (l limitsReply) evidence(quota func(Status, string) Evidence) ([]Evidence, error) {
	var evidence []Evidence
	switch {
	case l.OrdinaryUsageAllowed == nil:
		// the percentages alone never establish that usage is allowed
		evidence = append(evidence, quota(StatusUnknown, ReasonPermissionUnknown))
	case !*l.OrdinaryUsageAllowed:
		evidence = append(evidence, quota(StatusUnavailable, ReasonQuotaDenied))
	}
	account, err := l.RateLimits.evidence(quota, Scope{}, true)
	if err != nil {
		return nil, err
	}
	evidence = append(evidence, account...)

	accountID := ""
	if l.RateLimits.LimitID != nil {
		accountID = *l.RateLimits.LimitID
	}
	for _, id := range slices.Sorted(maps.Keys(l.ByLimitID)) {
		if id == accountID {
			continue
		}
		b := l.ByLimitID[id]
		label := id
		for _, v := range []*string{b.NormalModelSlug, b.LimitName} {
			if v != nil && strings.TrimSpace(*v) != "" {
				label = *v
				break
			}
		}
		if strings.TrimSpace(label) == "" {
			return nil, errors.New("scoped bucket without a label")
		}
		scoped, err := b.evidence(quota, Scope{Models: []string{label}}, false)
		if err != nil {
			return nil, fmt.Errorf("bucket %q: %w", id, err)
		}
		evidence = append(evidence, scoped...)
	}
	return evidence, nil
}

// evidence normalizes one bucket's windows, named by their server-provided duration. The account
// bucket always reports both subscription windows, unknown when missing. An explicit denial (limit
// reached, spend control) is unavailable whatever the percentages.
func (b rateBucket) evidence(quota func(Status, string) Evidence, scope Scope, account bool) ([]Evidence, error) {
	overage := b.Credits != nil && (b.Credits.HasCredits || b.Credits.Unlimited)
	var evidence []Evidence
	seen := map[string]bool{}
	for _, w := range []*rateWindow{b.Primary, b.Secondary} {
		if w == nil {
			continue
		}
		e := quota(StatusUnknown, ReasonNotReported)
		e.Scope = scope
		if w.DurationMin != nil {
			e.Window = rpcWindows[*w.DurationMin]
		}
		if err := e.fromWindow(w.UsedPercent, nil, false, overage); err != nil {
			return nil, err
		}
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			e.ResetAt = time.Unix(*w.ResetsAt, 0).UTC()
		}
		seen[e.Window] = true
		evidence = append(evidence, e)
	}
	if account {
		for _, name := range []string{WindowFiveHour, WindowWeekly} {
			if !seen[name] {
				e := quota(StatusUnknown, ReasonNotReported)
				e.Window = name
				evidence = append(evidence, e)
			}
		}
	}
	if b.ReachedType != nil || (b.SpendControlReached != nil && *b.SpendControlReached) {
		e := quota(StatusUnavailable, ReasonQuotaDenied)
		e.Scope = scope
		evidence = append(evidence, e)
	}
	return evidence, nil
}

// configDefaults reads the model and effort the effective configuration for the working directory
// sets. A configured model without an effort takes that model's default effort from the model list;
// the list's default model alone is never taken as the child's default. Read failures leave the
// defaults unknown; only a broken connection returns an error.
func configDefaults(s *rpcSession, reply rpcReply) (Defaults, error) {
	var cfg struct {
		Config *struct {
			Model  *string `json:"model"`
			Effort *string `json:"model_reasoning_effort"`
		} `json:"config"`
	}
	if !reply.decode(&cfg) || cfg.Config == nil {
		return Defaults{}, nil
	}
	d := Defaults{Model: safeValue(cfg.Config.Model), Effort: safeValue(cfg.Config.Effort)}
	if d.Model == nil || d.Effort != nil {
		return d, nil
	}
	effort, err := modelDefaultEffort(s, *d.Model)
	d.Effort = safeValue(&effort)
	return d, err
}

// modelDefaultEffort pages through the model list, hidden models included, for model's default
// effort; empty when the list does not report it.
func modelDefaultEffort(s *rpcSession, model string) (string, error) {
	var cursor *string
	for range rpcModelPages {
		params := map[string]any{"includeHidden": true}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		res, err := s.call(rpcCall{method: rpcModels, params: params})
		if err != nil {
			return "", err
		}
		var page struct {
			Data []struct {
				ID            string  `json:"id"`
				Model         string  `json:"model"`
				DefaultEffort *string `json:"defaultReasoningEffort"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if !res[0].decode(&page) {
			return "", nil
		}
		for _, m := range page.Data {
			if m.ID == model || m.Model == model {
				if m.DefaultEffort == nil {
					return "", nil
				}
				return *m.DefaultEffort, nil
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return "", nil
		}
		cursor = page.NextCursor
	}
	return "", nil
}
