package readiness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// the test binary doubles as the probe: with GO_WANT_HELPER_PROCESS=1 it runs helper instead of the tests.
const (
	helperEnv     = "GO_WANT_HELPER_PROCESS"
	helperMode    = "READINESS_HELPER_MODE"
	helperStdout  = "READINESS_HELPER_STDOUT"
	helperExit    = "READINESS_HELPER_EXIT"
	helperWantDir = "READINESS_HELPER_WANT_DIR"
	helperFixture = "READINESS_HELPER_FIXTURE"
	helperRecord  = "READINESS_HELPER_RECORD"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(helper())
	}
	os.Exit(m.Run())
}

// helper is the fake probe. Modes:
//   - print: writes $READINESS_HELPER_STDOUT to stdout and exits $READINESS_HELPER_EXIT;
//   - sleep: sleeps for a minute;
//   - isolation: reports auth available only when stdin is empty, the Jev key is absent from the
//     environment and the working directory is $READINESS_HELPER_WANT_DIR; otherwise unavailable with
//     the reason that failed;
//   - control: a stream-json control peer, see controlHelper;
//   - rpc: a stdio JSON-RPC peer, see rpcHelper.
func helper() int {
	switch os.Getenv(helperMode) {
	case "print":
		_, _ = os.Stdout.WriteString(os.Getenv(helperStdout))
		code, _ := strconv.Atoi(os.Getenv(helperExit))
		return code
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	case "control":
		return controlHelper()
	case "rpc":
		return rpcHelper()
	case "isolation":
		reason := ""
		stdin, _ := io.ReadAll(os.Stdin)
		wd, _ := os.Getwd()
		wd, _ = filepath.EvalSymlinks(wd)
		want, _ := filepath.EvalSymlinks(os.Getenv(helperWantDir))
		switch {
		case len(stdin) > 0:
			reason = "stdin_leaked"
		case os.Getenv(config.EnvAPIKey) != "":
			reason = "key_leaked"
		case wd != want:
			reason = "wrong_dir"
		}
		status := `"available"`
		if reason != "" {
			status = `"unavailable","reason":"` + reason + `"`
		}
		_, _ = os.Stdout.WriteString(`{"schema":"agrouter.readiness/v1","auth":{"status":` + status + `}}`)
		return 0
	}
	return 98
}

var observed = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// checker returns a Checker whose probes run the helper in mode with extra environment.
func checker(mode string, env ...string) Checker {
	return Checker{
		Env: append(append(os.Environ(), helperEnv+"=1", helperMode+"="+mode), env...),
		Now: func() time.Time { return observed },
	}
}

// helperCLI is a synthetic CLI whose command and json-helper check both run the test binary.
func helperCLI(checks ...config.Check) config.CLI {
	if len(checks) == 0 {
		checks = []config.Check{{Name: "status", Transport: config.TransportJSONHelper}}
	}
	return config.CLI{Name: "tool-a", Command: os.Args[0], Availability: &config.Availability{
		Enabled: true, Checks: checks, Timeout: 10 * time.Second, Unknown: config.UnknownAllow,
	}}
}

func evidenceOf(r Report, d Dimension) []Evidence {
	var out []Evidence
	for _, e := range r.Evidence {
		if e.Dimension == d {
			out = append(out, e)
		}
	}
	return out
}

func TestCheck_ExecutableMissing(t *testing.T) {
	cli := helperCLI()
	cli.Command = filepath.Join(t.TempDir(), "no-such-cli")
	r := checker("print").Check(context.Background(), cli)
	assert.Equal(t, "tool-a", r.CLI)
	assert.Equal(t, []Evidence{{Dimension: DimensionExecutable, Status: StatusUnavailable,
		Reason: ReasonExecutableMissing, ObservedAt: observed}}, r.Evidence, "no probe runs without the executable")
}

func TestCheck_ExecutableNotExecutable(t *testing.T) {
	// no execute bit on Unix and no PATHEXT extension on Windows: present, but not runnable
	path := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(path, []byte("text"), 0o600))
	cli := helperCLI()
	cli.Command = path
	r := checker("print").Check(context.Background(), cli)
	require.Len(t, r.Evidence, 1)
	assert.Equal(t, StatusUnavailable, r.Evidence[0].Status)
}

func TestCheck_Unknown(t *testing.T) {
	const valid = `{"schema":"agrouter.readiness/v1","quota":[{"window":"weekly","status":"available"}]}`
	tests := []struct {
		name   string
		mutate func(*config.CLI)
		mode   string
		env    []string
		limit  int64
		reason string
	}{
		{name: "no availability section", mutate: func(c *config.CLI) { c.Availability = nil }, reason: ReasonNotConfigured},
		{name: "no checks", mutate: func(c *config.CLI) { c.Availability.Checks = nil }, reason: ReasonNotConfigured},
		{name: "disabled", mutate: func(c *config.CLI) { c.Availability.Enabled = false }, reason: ReasonDisabled},
		{name: "transport without a decoder",
			mutate: func(c *config.CLI) { c.Availability.Checks[0].Transport = "unknown-v9" },
			reason: ReasonTransportUnsupported},
		{name: "missing probe command",
			mutate: func(c *config.CLI) { c.Availability.Checks[0].Command = filepath.Join(os.TempDir(), "no-such-probe-x") },
			reason: ReasonProbeMissing},
		{name: "nonzero exit", mode: "print", env: []string{helperStdout + "=" + valid, helperExit + "=1"}, reason: ReasonProbeFailed},
		{name: "timeout", mode: "sleep", mutate: func(c *config.CLI) { c.Availability.Timeout = 200 * time.Millisecond },
			reason: ReasonProbeTimeout},
		{name: "response too large", mode: "print", env: []string{helperStdout + "=" + valid}, limit: 16,
			reason: ReasonProbeResponseTooLarge},
		{name: "malformed output", mode: "print", env: []string{helperStdout + "=quota: ok"}, reason: ReasonProbeMalformed},
		{name: "empty output", mode: "print", reason: ReasonProbeMalformed},
		{name: "no quota reported", mode: "print",
			env: []string{helperStdout + `={"schema":"agrouter.readiness/v1","auth":{"status":"available"}}`}, reason: ReasonNotReported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := helperCLI()
			if tt.mutate != nil {
				tt.mutate(&cli)
			}
			c := checker(tt.mode, tt.env...)
			c.MaxResponse = tt.limit
			r := c.Check(context.Background(), cli)
			quota := evidenceOf(r, DimensionQuota)
			require.Len(t, quota, 1)
			assert.Equal(t, StatusUnknown, quota[0].Status, "an unverified check is never ready")
			assert.Equal(t, tt.reason, quota[0].Reason)
			assert.Equal(t, observed, quota[0].ObservedAt)
			assert.Equal(t, Defaults{}, r.Defaults)
		})
	}
}

func TestCheck_Helper(t *testing.T) {
	out := `{"schema":"agrouter.readiness/v1","auth":{"status":"available"},` +
		`"quota":[{"window":"five_hour","status":"available"},` +
		`{"window":"weekly","status":"unavailable","models":["m-1"],"reset_at":"2026-10-13T15:00:00Z"}],` +
		`"capability":{"unsupported_models":["m-3"]},"defaults":{"model":"m-2","effort":null}}`
	r := checker("print", helperStdout+"="+out).Check(context.Background(), helperCLI())
	assert.Equal(t, []Evidence{
		{Dimension: DimensionExecutable, Status: StatusAvailable, ObservedAt: observed},
		{Dimension: DimensionAuth, Status: StatusAvailable, Check: "status", ObservedAt: observed},
		{Dimension: DimensionQuota, Status: StatusAvailable, Check: "status", Window: WindowFiveHour, ObservedAt: observed},
		{Dimension: DimensionQuota, Status: StatusUnavailable, Reason: ReasonQuotaExhausted, Check: "status",
			Scope: Scope{Models: []string{"m-1"}}, Window: WindowWeekly, ObservedAt: observed,
			ResetAt: time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)},
		{Dimension: DimensionCapability, Status: StatusUnavailable, Reason: ReasonUnsupportedModel, Check: "status",
			Scope: Scope{Models: []string{"m-3"}}, ObservedAt: observed},
	}, r.Evidence)
	require.NotNil(t, r.Defaults.Model)
	assert.Equal(t, "m-2", *r.Defaults.Model)
	assert.Nil(t, r.Defaults.Effort)
}

func TestCheck_ProbeIsolation(t *testing.T) {
	dir := t.TempDir()
	c := checker("isolation", config.EnvAPIKey+"=sk-secret", helperWantDir+"="+dir)
	c.Dir = dir
	r := c.Check(context.Background(), helperCLI())
	auth := evidenceOf(r, DimensionAuth)
	require.Len(t, auth, 1)
	assert.Equal(t, StatusAvailable, auth[0].Status, "probe saw %s", auth[0].Reason)
}

func TestCheck_MultipleChecks(t *testing.T) {
	cli := helperCLI(
		config.Check{Name: "usage", Transport: "unknown-v9"},
		config.Check{Name: "status", Transport: config.TransportJSONHelper},
	)
	out := `{"schema":"agrouter.readiness/v1","quota":[{"window":"weekly","status":"available"}]}`
	r := checker("print", helperStdout+"="+out).Check(context.Background(), cli)
	quota := evidenceOf(r, DimensionQuota)
	require.Len(t, quota, 2)
	assert.Equal(t, "usage", quota[0].Check)
	assert.Equal(t, ReasonTransportUnsupported, quota[0].Reason)
	assert.Equal(t, "status", quota[1].Check)
	assert.Equal(t, StatusAvailable, quota[1].Status)
}

// a CLI that exists only in a local config layer gets readiness from configuration alone
func TestCheck_ThirdCLIFromLocalConfig(t *testing.T) {
	dir := t.TempDir()
	embedded := "[agrouter]\ntimeout = 10s\nrouting_policy = cheap\ncomplexity_policy = whole\n" +
		"[cli.tool-a]\ncommand = tool-a\nexec_args = []\nmodel_args = [\"-m\", \"{model}\"]\nprompt_args = [\"{prompt}\"]\n"
	local := "[cli.tool-c]\ncommand = " + os.Args[0] + "\nexec_args = []\nmodel_args = [\"-m\", \"{model}\"]\nprompt_args = [\"{prompt}\"]\n" +
		"[cli.tool-c.availability]\nchecks = status\nunknown = block\n" +
		"[cli.tool-c.availability.status]\ntransport = json-helper\nargs = [\"--status\"]\n"
	localPath := filepath.Join(dir, "config")
	require.NoError(t, os.WriteFile(localPath, []byte(local), 0o600))
	cfg, err := config.Load(config.Sources{Embedded: []byte(embedded), LocalPath: localPath})
	require.NoError(t, err)
	cli, ok := cfg.CLIByName("tool-c")
	require.True(t, ok)

	out := `{"schema":"agrouter.readiness/v1","quota":[{"window":"five_hour","status":"unavailable","reason":"limit_reached"}]}`
	r := checker("print", helperStdout+"="+out).Check(context.Background(), cli)
	quota := evidenceOf(r, DimensionQuota)
	require.Len(t, quota, 1)
	assert.Equal(t, StatusUnavailable, quota[0].Status)
	assert.Equal(t, "limit_reached", quota[0].Reason)
	assert.True(t, quota[0].Scope.Account())
}

func TestCheck_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := checker("sleep").Check(ctx, helperCLI())
	quota := evidenceOf(r, DimensionQuota)
	require.Len(t, quota, 1)
	assert.Equal(t, StatusUnknown, quota[0].Status)
	assert.Equal(t, ReasonProbeTimeout, quota[0].Reason)
}

// a later check's reported default overrides an earlier one, an unreported one leaves it alone, and
// an effort never pairs with a model another check reported
func TestReport_SetDefaults(t *testing.T) {
	tests := []struct {
		name   string
		checks []Defaults
		want   Defaults
	}{
		{name: "same model, new effort", checks: []Defaults{{Model: new("m-1"), Effort: new("e-1")},
			{Model: new("m-1"), Effort: new("e-2")}}, want: Defaults{Model: new("m-1"), Effort: new("e-2")}},
		{name: "same model, no effort", checks: []Defaults{{Model: new("m-1"), Effort: new("e-1")},
			{Model: new("m-1")}}, want: Defaults{Model: new("m-1"), Effort: new("e-1")}},
		{name: "other model replaces the pair", checks: []Defaults{{Model: new("m-1"), Effort: new("e-1")},
			{Model: new("m-2")}}, want: Defaults{Model: new("m-2")}},
		{name: "effort alone after a model", checks: []Defaults{{Model: new("m-1"), Effort: new("e-1")},
			{Effort: new("e-2")}}, want: Defaults{Model: new("m-1"), Effort: new("e-1")}},
		{name: "effort alone", checks: []Defaults{{Effort: new("e-1")}, {}}, want: Defaults{Effort: new("e-1")}},
		{name: "model after an effort alone", checks: []Defaults{{Effort: new("e-1")}, {Model: new("m-1")}},
			want: Defaults{Model: new("m-1")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Report
			for _, d := range tt.checks {
				r.setDefaults(d)
			}
			assert.Equal(t, tt.want, r.Defaults)
		})
	}
}

func TestChecker_DefaultClock(t *testing.T) {
	before := time.Now()
	assert.False(t, Checker{}.now().Before(before))
}
