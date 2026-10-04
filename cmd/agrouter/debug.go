package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/router"
)

// envDebug turns on the routing diagnostics on stderr.
const envDebug = "AGROUTER_DEBUG"

// debugTop is how many options a Jev answer lists.
const debugTop = 3

// debugLog writes AGROUTER_DEBUG=1 lines to stderr. A nil *debugLog prints nothing. It never prints
// the prompt text, raw passthrough tokens or the API key: the argv is shown redacted, and any line
// that would still hold the key has it replaced.
type debugLog struct {
	w   io.Writer
	key string
}

// newDebugLog returns a log writing to w when value is "1", and nil otherwise.
func newDebugLog(value string, w io.Writer) *debugLog {
	if value != "1" {
		return nil
	}
	return &debugLog{w: w}
}

func (l *debugLog) printf(format string, a ...any) {
	if l == nil {
		return
	}
	line := fmt.Sprintf(format, a...)
	if l.key != "" {
		line = strings.ReplaceAll(line, l.key, "<redacted>")
	}
	fmt.Fprintln(l.w, "agrouter debug: "+line)
}

// apiKey notes where the key came from, never its value, and remembers it for redaction.
func (l *debugLog) apiKey(key, source string) {
	if l == nil {
		return
	}
	l.key = key
	if key == "" {
		l.printf("api key: none (%s)", source)
		return
	}
	l.printf("api key: from %s", source)
}

// eligibility prints the options left, the CLIs they belong to, and why the others were dropped.
func (l *debugLog) eligibility(el *router.Eligibility) {
	if l == nil {
		return
	}
	l.printf("eligible: %d option(s) on %s", len(el.Options), strings.Join(el.CLIs(), ", "))
	for _, d := range el.Dropped {
		l.printf("dropped %s: %s", d.CLI, d.Reason)
	}
	if el.ModelPassthrough {
		l.printf("model: passed through")
	}
	if el.EffortPassthrough {
		l.printf("effort: passed through")
	}
}

// decision prints how the option was chosen: the doc scores and project complexity when the docs
// were scored, then without Jev, Jev's answer, the pooled chunks, or why Jev could not decide.
func (l *debugLog) decision(d router.Decision) {
	if l == nil {
		return
	}
	if cx := d.Complexity; cx != nil {
		for _, c := range cx.Chunks {
			l.printf("doc %d/%d: score %.3f, evidence %.3f", c.Index, c.Of, c.Score, c.Evidence)
		}
		l.printf("project complexity: %.1f", cx.Complexity)
	}
	switch {
	case d.Undecided != nil:
		l.printf("jev failed: %v; running %s with the caller's fixed values", d.Undecided, d.CLI)
	case d.Answer != nil:
		l.printf("jev: choice %s, confidence %.3f, top %s", d.Answer.Choice, d.Answer.Confidence,
			topProbabilities(d.Answer.Probabilities))
	case d.Pooled != nil:
		for _, c := range d.Pooled.Chunks {
			l.printf("chunk %s %d/%d: relevance %.3f, top %s", c.Field, c.Index, c.Of, c.Relevance, scores(c.Top))
		}
		l.printf("pooled: choice %s, top %s", d.OptionID, scores(d.Pooled.Top))
	default:
		l.printf("one option, jev not asked: %s", d.OptionID)
	}
}

// failed prints the reason routing stopped, for a failure that exits without a decision.
func (l *debugLog) failed(err error) {
	l.printf("no decision: %v", err)
}

// command prints the final argv with the prompt token as <prompt> and raw tokens as a count.
func (l *debugLog) command(res args.Result) {
	if l == nil {
		return
	}
	l.printf("command: %s", res.Redacted())
}

// topProbabilities lists the highest probabilities, ties by option id.
func topProbabilities(probs map[string]float64) string {
	top := make([]router.Score, 0, len(probs))
	for id, p := range probs {
		top = append(top, router.Score{ID: id, Score: p})
	}
	slices.SortFunc(top, func(a, b router.Score) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return scores(top[:min(len(top), debugTop)])
}

func scores(top []router.Score) string {
	parts := make([]string, len(top))
	for i, s := range top {
		parts[i] = fmt.Sprintf("%s %.3f", s.ID, s.Score)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
