package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

// evalDir holds the routing evaluation set, shared by make eval-routing and the loader tests.
var evalDir = filepath.Join("..", "..", "testdata", "routing")

// evalCase is one labeled prompt of the routing evaluation set (testdata/routing/*.json).
type evalCase struct {
	Name        string            `json:"-"` // the file name without .json
	Description string            `json:"description"`
	Prompt      string            `json:"prompt"`     // the positional prompt
	Stdin       string            `json:"stdin"`      // stdin text, inline
	StdinFile   string            `json:"stdin_file"` // stdin text from a file beside the case, after Stdin
	Filler      *evalFiller       `json:"filler"`     // generated material around the stdin text
	Files       map[string]string `json:"files"`      // files the prompt mentions, relative path → content
	CLI         string            `json:"cli"`        // --cli, empty to route across every CLI
	Acceptable  []string          `json:"acceptable"` // option ids counted as correct
}

// evalFiller repeats Text up to Bytes, before or after the stdin text, so an oversized prompt is
// generated rather than stored.
type evalFiller struct {
	Text     string `json:"text"`
	Bytes    int    `json:"bytes"`
	Position string `json:"position"` // "before" or "after"
}

// loadEvalCases reads every case in dir, checking that its acceptable options are in cat.
func loadEvalCases(dir string, cat *catalog.Catalog) ([]evalCase, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("glob cases: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no cases in %s", dir)
	}
	ids := map[string]bool{}
	for _, o := range cat.Options {
		ids[o.ID] = true
	}
	cases := make([]evalCase, 0, len(paths))
	for _, p := range paths {
		c, err := loadEvalCase(p, ids)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		cases = append(cases, c)
	}
	return cases, nil
}

func loadEvalCase(path string, ids map[string]bool) (evalCase, error) {
	var c evalCase
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		return c, fmt.Errorf("read: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return c, fmt.Errorf("decode: %w", err)
	}
	c.Name = strings.TrimSuffix(filepath.Base(path), ".json")
	if c.StdinFile != "" {
		text, err := os.ReadFile(filepath.Join(filepath.Dir(path), c.StdinFile))
		if err != nil {
			return c, fmt.Errorf("stdin_file: %w", err)
		}
		c.Stdin += string(text)
	}
	if c.Filler != nil {
		f := c.Filler
		if f.Text == "" || f.Bytes <= 0 || (f.Position != "before" && f.Position != "after") {
			return c, errors.New("filler needs text, bytes > 0 and position before or after")
		}
		fill := strings.Repeat(f.Text, f.Bytes/len(f.Text)+1)[:f.Bytes]
		if f.Position == "before" {
			c.Stdin = fill + c.Stdin
		} else {
			c.Stdin += fill
		}
	}
	if c.Prompt == "" && c.Stdin == "" {
		return c, errors.New("no prompt and no stdin")
	}
	if len(c.Acceptable) == 0 {
		return c, errors.New("no acceptable options")
	}
	for _, id := range c.Acceptable {
		if !ids[id] {
			return c, fmt.Errorf("acceptable option %q is not in the catalog", id)
		}
	}
	return c, nil
}

// evalResult is how one case was routed.
type evalResult struct {
	Case       string
	Chosen     string
	Correct    bool
	Split      bool    // pooled over chunks: no confidence
	Confidence float64 // Jev's, for a state sent whole
	Duration   time.Duration
	Err        error
}

// evalTimeout bounds each case. It is longer than [agrouter] timeout because the evaluation
// measures routing quality; each case's duration is reported beside it.
const evalTimeout = 2 * time.Minute

// runEvalCase captures the case's prompt the way agrouter does (mentioned files from a temporary
// working directory) and routes it within evalTimeout.
func runEvalCase(ctx context.Context, r *Router, cfg *config.Config, cat *catalog.Catalog, c evalCase, workDir string) evalResult {
	res := evalResult{Case: c.Name}
	start := time.Now()
	fail := func(err error) evalResult {
		res.Err = err
		res.Duration = time.Since(start)
		return res
	}

	for rel, content := range c.Files {
		p := filepath.Join(workDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return fail(fmt.Errorf("write %s: %w", rel, err))
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return fail(fmt.Errorf("write %s: %w", rel, err))
		}
	}

	ctx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()
	var stdin io.Reader // nil: no stdin
	if c.Stdin != "" {
		stdin = strings.NewReader(c.Stdin)
	}
	captured, err := prompt.Capture(ctx, []string{c.Prompt}, stdin)
	if err != nil {
		return fail(fmt.Errorf("capture: %w", err))
	}
	if err = captured.ReadMentions(ctx, workDir); err != nil {
		return fail(fmt.Errorf("read mentions: %w", err))
	}

	req := &args.Request{CLI: c.CLI, Prompt: args.Optional{Value: c.Prompt, Set: c.Prompt != ""}}
	d, err := r.Route(ctx, Eligible(cfg, cat, req), req, captured)
	if err != nil {
		return fail(err)
	}
	if d.Undecided != nil {
		return fail(d.Undecided)
	}
	res.Chosen = d.OptionID
	res.Correct = slices.Contains(c.Acceptable, d.OptionID)
	res.Split = d.Pooled != nil
	if d.Answer != nil {
		res.Confidence = d.Answer.Confidence
	}
	res.Duration = time.Since(start)
	return res
}

// evalReport summarizes results: accuracy over all cases, and the confidence distribution of the
// cases sent whole (split cases have no confidence and count towards accuracy only).
type evalReport struct {
	Total, Correct, Errors int
	// Buckets counts whole-state answers by confidence decile (0.0-0.1 … 0.9-1.0), correct and wrong.
	CorrectBuckets, WrongBuckets [10]int
}

func scoreEval(results []evalResult) evalReport {
	var rep evalReport
	for _, r := range results {
		rep.Total++
		switch {
		case r.Err != nil:
			rep.Errors++
			continue
		case r.Correct:
			rep.Correct++
		}
		if r.Split {
			continue
		}
		b := min(int(r.Confidence*10), 9)
		if r.Correct {
			rep.CorrectBuckets[b]++
		} else {
			rep.WrongBuckets[b]++
		}
	}
	return rep
}

// Accuracy is the share of cases routed to an acceptable option; errors count as wrong.
func (r evalReport) Accuracy() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Correct) / float64(r.Total)
}

func (r evalReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "accuracy %d/%d (%.1f%%), %d error(s)\n", r.Correct, r.Total, 100*r.Accuracy(), r.Errors)
	b.WriteString("confidence  correct  wrong\n")
	for i := range r.CorrectBuckets {
		if r.CorrectBuckets[i] == 0 && r.WrongBuckets[i] == 0 {
			continue
		}
		fmt.Fprintf(&b, "%.1f-%.1f   %7d  %5d\n", float64(i)/10, float64(i+1)/10, r.CorrectBuckets[i], r.WrongBuckets[i])
	}
	return b.String()
}

// resultLines lists each case's outcome, sorted by case name.
func resultLines(results []evalResult) []string {
	lines := make([]string, 0, len(results))
	for _, r := range results {
		var line string
		switch {
		case r.Err != nil:
			line = fmt.Sprintf("ERROR %s: %v", r.Case, r.Err)
		case r.Split:
			line = fmt.Sprintf("%-5s %s: %s (split) in %s", mark(r.Correct), r.Case, r.Chosen, r.Duration.Round(time.Millisecond))
		default:
			line = fmt.Sprintf("%-5s %s: %s confidence %.3f in %s", mark(r.Correct), r.Case, r.Chosen, r.Confidence,
				r.Duration.Round(time.Millisecond))
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

func mark(ok bool) string {
	if ok {
		return "ok"
	}
	return "WRONG"
}

// evalQuestions adapts the configured questions to enc: the full encoding has no instructions
// object to look options up in, so the sentence pointing there is dropped.
func evalQuestions(cfg *config.Config, enc Encoding) {
	if enc != EncodingFull {
		return
	}
	for _, q := range []*string{&cfg.Agrouter.Question, &cfg.Agrouter.ChunkQuestion} {
		if i := strings.Index(*q, " Look up each option"); i >= 0 {
			*q = (*q)[:i]
		}
	}
}
