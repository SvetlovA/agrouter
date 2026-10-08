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
	Prompt      string            `json:"prompt"`      // the positional prompt
	PromptFile  string            `json:"prompt_file"` // --prompt-file: a text file beside the case
	Stdin       string            `json:"stdin"`       // stdin text, inline
	StdinFile   string            `json:"stdin_file"`  // stdin text from a file beside the case, after Stdin
	Filler      *evalFiller       `json:"filler"`      // generated material around the stdin text
	Files       map[string]string `json:"files"`       // files the prompt mentions, relative path → content
	Docs        []evalDoc         `json:"docs"`        // --doc, in order
	CLI         string            `json:"cli"`         // --cli, empty to route across every CLI
	Acceptable  []string          `json:"acceptable"`  // option ids counted as correct

	FileText string   `json:"-"` // the prompt_file contents
	DocTexts []string `json:"-"` // the docs' contents, in order
}

// evalDoc is one --doc: inline Text or a File beside the case, exactly one of them.
type evalDoc struct {
	Text string `json:"text"`
	File string `json:"file"`
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
		text, readErr := os.ReadFile(filepath.Join(filepath.Dir(path), c.StdinFile))
		if readErr != nil {
			return c, fmt.Errorf("stdin_file: %w", readErr)
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
	if err := c.readExplicit(filepath.Dir(path)); err != nil {
		return c, err
	}
	if c.Prompt == "" && c.FileText == "" && c.Stdin == "" {
		return c, errors.New("no prompt, no prompt_file and no stdin")
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

// readExplicit reads the prompt file and the doc files from dir strictly, the way agrouter reads
// --prompt-file and --doc.
func (c *evalCase) readExplicit(dir string) error {
	var err error
	if c.PromptFile != "" {
		if c.FileText, err = prompt.ReadTextFile(context.Background(), dir, c.PromptFile); err != nil {
			return fmt.Errorf("prompt_file: %w", err)
		}
	}
	for i, d := range c.Docs {
		text := d.Text
		switch {
		case (d.Text == "") == (d.File == ""):
			return fmt.Errorf("doc %d: needs exactly one of text and file", i)
		case d.File != "":
			if text, err = prompt.ReadTextFile(context.Background(), dir, d.File); err != nil {
				return fmt.Errorf("doc %d: %w", i, err)
			}
		}
		c.DocTexts = append(c.DocTexts, text)
	}
	return nil
}

// evalResult is how one case was routed.
type evalResult struct {
	Case     string
	Chosen   string
	Correct  bool
	Split    bool        // some stage was pooled over chunks
	Stages   []evalStage // the stages asked, in order; skipped ones are left out
	Project  string      // the project complexity, formatted, when the case has docs
	Duration time.Duration
	Err      error
}

// evalStage is one asked routing stage. Its confidence is conditional on the stages before it.
type evalStage struct {
	Level      string
	Confidence float64 // the whole-state answer's, or the mean of the chunk confidences when pooled
	Pooled     bool
	// Correct is whether the choice leads towards an acceptable option, given the stages before it;
	// after a wrong stage none is left, so every later stage is wrong too.
	Correct bool
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
	req := &args.Request{CLI: c.CLI, Prompt: args.Optional{Value: c.Prompt, Set: c.Prompt != ""},
		PromptFile: args.Optional{Value: c.PromptFile, Set: c.PromptFile != ""}, FileText: c.FileText}
	captured, err := prompt.Capture(ctx, req.ArgvPrompt(), stdin)
	if err != nil {
		return fail(fmt.Errorf("capture: %w", err))
	}
	captured.Docs = c.DocTexts
	if err = captured.ReadMentions(ctx, workDir); err != nil {
		return fail(fmt.Errorf("read mentions: %w", err))
	}

	d, err := r.Route(ctx, Eligible(cfg, cat, req), req, captured)
	if err != nil {
		return fail(err)
	}
	if d.Undecided != nil {
		return fail(d.Undecided)
	}
	res.Chosen = d.OptionID
	res.Correct = slices.Contains(c.Acceptable, d.OptionID)
	if d.Complexity != nil {
		res.Project = fmt.Sprintf("%.1f", d.Complexity.Complexity)
	}
	res.Stages = evalStages(d.Stages, acceptableOptions(cat, c.Acceptable))
	res.Split = slices.ContainsFunc(res.Stages, func(st evalStage) bool { return st.Pooled })
	res.Duration = time.Since(start)
	return res
}

// acceptableOptions are the catalog options with the given ids.
func acceptableOptions(cat *catalog.Catalog, ids []string) []catalog.Option {
	var out []catalog.Option
	for _, o := range cat.Options {
		if slices.Contains(ids, o.ID) {
			out = append(out, o)
		}
	}
	return out
}

// evalStages scores the asked stages against the acceptable options, narrowing them at every stage,
// skipped ones included, the way routing narrows the eligible options.
func evalStages(stages []Stage, acceptable []catalog.Option) []evalStage {
	var out []evalStage
	for _, st := range stages {
		i := slices.IndexFunc(routeLevels[:], func(lv level) bool { return lv.name == st.Level })
		if i < 0 {
			continue
		}
		label := routeLevels[i].label
		acceptable = slices.DeleteFunc(slices.Clone(acceptable), func(o catalog.Option) bool { return label(o) != st.Choice })
		if st.Skipped {
			continue
		}
		es := evalStage{Level: st.Level, Pooled: st.Pooled != nil, Correct: len(acceptable) > 0}
		switch {
		case st.Answer != nil:
			es.Confidence = st.Answer.Confidence
		case st.Pooled != nil && len(st.Pooled.Chunks) > 0:
			for _, c := range st.Pooled.Chunks {
				es.Confidence += c.Confidence
			}
			es.Confidence /= float64(len(st.Pooled.Chunks))
		}
		out = append(out, es)
	}
	return out
}

// evalReport summarizes results: accuracy over all cases, latency, and per routing level the
// distribution of the asked stages' confidence, which is conditional on the stages before.
type evalReport struct {
	Total, Correct, Errors int
	Duration               time.Duration // all cases, errors included
	// Levels counts each level's asked stages by confidence decile (0.0-0.1 … 0.9-1.0), correct and
	// wrong; a pooled stage counts with the mean of its chunk confidences.
	Levels map[string]*levelBuckets
}

// levelBuckets counts one level's stages by confidence decile.
type levelBuckets struct {
	Correct, Wrong [10]int
}

func scoreEval(results []evalResult) evalReport {
	rep := evalReport{Levels: map[string]*levelBuckets{}}
	for _, r := range results {
		rep.Total++
		rep.Duration += r.Duration
		switch {
		case r.Err != nil:
			rep.Errors++
			continue
		case r.Correct:
			rep.Correct++
		}
		for _, st := range r.Stages {
			lb := rep.Levels[st.Level]
			if lb == nil {
				lb = &levelBuckets{}
				rep.Levels[st.Level] = lb
			}
			b := min(int(st.Confidence*10), 9)
			if st.Correct {
				lb.Correct[b]++
			} else {
				lb.Wrong[b]++
			}
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
	fmt.Fprintf(&b, "accuracy %d/%d (%.1f%%), %d error(s), latency %s total", r.Correct, r.Total, 100*r.Accuracy(),
		r.Errors, r.Duration.Round(time.Millisecond))
	if r.Total > 0 {
		fmt.Fprintf(&b, ", %s per case", (r.Duration / time.Duration(r.Total)).Round(time.Millisecond))
	}
	b.WriteString("\n")
	for _, lv := range routeLevels {
		lb := r.Levels[lv.name]
		if lb == nil {
			continue
		}
		fmt.Fprintf(&b, "%s confidence (conditional)  correct  wrong\n", lv.name)
		for i := range lb.Correct {
			if lb.Correct[i] == 0 && lb.Wrong[i] == 0 {
				continue
			}
			fmt.Fprintf(&b, "  %.1f-%.1f   %7d  %5d\n", float64(i)/10, float64(i+1)/10, lb.Correct[i], lb.Wrong[i])
		}
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
			line = fmt.Sprintf("ERROR %s: %v in %s", r.Case, r.Err, r.Duration.Round(time.Millisecond))
		default:
			var b strings.Builder
			fmt.Fprintf(&b, "%-5s %s: %s", mark(r.Correct), r.Case, r.Chosen)
			for _, st := range r.Stages {
				fmt.Fprintf(&b, ", %s %.3f", st.Level, st.Confidence)
				if st.Pooled {
					b.WriteString(" (split)")
				}
			}
			fmt.Fprintf(&b, " in %s", r.Duration.Round(time.Millisecond))
			line = b.String()
		}
		if r.Err == nil && r.Project != "" {
			line += ", project " + r.Project
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
