package router

import (
	"encoding/json"
	"fmt"

	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

// Question ids in a Jev request.
const (
	questionRelevance  = "relevance"
	questionComplexity = "complexity"
	questionEvidence   = "complexity_evidence"
)

// Criterion values for an option without an effort.
const (
	effortNone       = "none (not supported)"
	effortDefault    = "the CLI's default"
	descModelPassed  = "not in the catalog: passed through as given"
	descEffortPassed = "not in the catalog for this CLI: passed through as given"
)

// Question texts and state guides. The router owns the questions' structure: what is asked and
// what each state field holds. The config owns only the cost/quality preference, sent as policy.
const (
	// cliText asks the cli stage: which CLI, judged by the models and efforts it offers.
	cliText = "Which coding-agent CLI should run the task described in `state`? Choose following `policy`. " +
		"Each CLI in `clis` lists the models it can run, with their reasoning efforts, described under that CLI " +
		"in `efforts`; judge a CLI by the best of its models and efforts for the task."

	// modelText asks the model stage, within the CLI already chosen or fixed.
	modelText = "Which model should run the task described in `state`? Choose following `policy`. " +
		"Each option is described under its name in `models`, with its reasoning efforts described under its " +
		"`cli` in `efforts`; judge a model by the best of its efforts for the task."

	// effortText asks the effort stage, for the model already chosen or fixed.
	effortText = "Which reasoning effort should the model in `models` use for the task described in `state`? " +
		"Choose following `policy`. Each option's `effort` is described under its CLI in `efforts`."

	// wholeGuide describes a state sent whole: state.prompt is the joined -p, positional and
	// --prompt-file text followed by text stdin; state.files holds the readable text files the
	// prompt mentions; state.attachments lists binary stdin and non-text files by source, media
	// type and byte size, without contents or names; state.project.complexity, when present, is
	// the --doc stage's score from 0 to 10.
	wholeGuide = "`state.prompt` is the task. `state.files` are the contents of files the prompt mentions. " +
		"`state.attachments` lists images, PDFs and other non-text inputs the task includes, by type and size only. " +
		"`state.project.complexity`, when present, rates the codebase the task runs in from 0 (a tiny script) " +
		"to 10 (a large, constrained enterprise system)."

	// chunkGuide describes one chunk of a split state. anchor is shared context repeated in every
	// chunk request: prompt and file excerpts (whole strings or head/tail objects), attachment
	// metadata, where entries of one source and type may merge into a count and a byte total, and
	// the same project.complexity a whole state carries. chunk.text is one part of the prompt or of
	// one mentioned file, chunk.field names its source ("prompt" or "files"), chunk.index is its
	// 1-based position and chunk.of the number of chunks. Jev sees only this chunk and the anchor.
	chunkGuide = "`state.anchor` is context shared by every part of a longer task: excerpts of the prompt " +
		"and the files it mentions, and `attachments`, the images, PDFs and other non-text inputs the task " +
		"includes, by type and size only. `state.chunk.text` is one part of the longer input; " +
		"`state.chunk.field` says whether it comes from the prompt or a file, and `state.chunk.index` " +
		"and `state.chunk.of` give its position. `state.anchor.project.complexity`, when present, rates " +
		"the codebase the task runs in from 0 (a tiny script) to 10 (a large, constrained enterprise system)."

	// relevanceText is asked beside the stage question of a chunk request. Its 0-to-1 answer
	// weights the chunk's probabilities when the chunk answers are pooled.
	relevanceText = "Does the text in `chunk.text` state the task to perform, its requirements, " +
		"or what makes it hard, beyond what `anchor` already says?"

	// complexityText is asked of the --doc text before routing. Its score, scaled from levels
	// 0-9 to 0-10, becomes project.complexity in the routing state.
	complexityText = "How complex is the project that the docs in `state` describe? " +
		"Judge it following `policy` and rate it against the ordered complexity descriptions."

	// docGuide describes a doc request's state, which carries only --doc text, never the routing
	// prompt, files, attachments or catalog: state.docs holds every non-empty doc whole, in the
	// order supplied, when they fit; otherwise state.doc is one chunk of one doc, with its 1-based
	// index across all doc chunks and their count.
	docGuide = "`state.docs` are the project's docs, whole. When the docs are too long, `state.doc.text` " +
		"is instead one part of them, and `state.doc.index` and `state.doc.of` give its position."

	// evidenceText is asked beside the complexity question. Its 0-to-1 answer weights the doc
	// chunk's score when the answers are reduced to project.complexity.
	evidenceText = "Does the text in `state` (`docs` or `doc.text`) say anything about the project's size, " +
		"architecture, dependencies or constraints?"
)

// Relevance criteria of a chunk request.
const (
	relevanceTrue  = "`chunk.text` adds to what the task is, what it requires, or what makes it hard"
	relevanceFalse = "`chunk.text` is only material the task works on, or repeats `anchor`"
)

// complexityLevels is the Score rubric, ordered from 0 to 9 and normalized to 0 to 10 after asking.
var complexityLevels = [...]string{
	"a snippet, a single file or a small one-off script: no architecture, dependencies or constraints to speak of",
	"a small single-purpose utility or library of a few files",
	"a small application with a handful of modules and common dependencies",
	"a moderate codebase: several packages, a build and test setup, conventions to follow",
	"a medium application with several components, integrations and documented conventions",
	"a larger codebase with a layered architecture, many dependencies and cross-cutting rules",
	"a multi-service or multi-platform system with integration points and compatibility constraints",
	"a large production system with strict invariants and performance, security or concurrency constraints",
	"a large enterprise system: many modules and teams, legacy and compliance constraints, a wide blast radius",
	"a very large, critical system where almost any change needs deep cross-system understanding",
}

// Evidence criteria of a doc request.
const (
	evidenceTrue  = "the text describes the project's size, architecture, dependencies or constraints"
	evidenceFalse = "the text is only style rules, workflow instructions or other content that says nothing about the project itself"
)

// stageCriterion identifies one group of a stage: its cli, its model and cli, or its effort. What
// lies below it is described once in the instructions.
type stageCriterion struct {
	Model  string `json:"model,omitempty"`
	CLI    string `json:"cli,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// instructions is a question's instructions: the question, its state guide and the policy from config.
type instructions struct {
	Question string `json:"question"`
	State    string `json:"state"`
	Policy   string `json:"policy"`
}

// stageInstructions is a stage question's instructions: the question, its state guide, the routing
// policy and the options below the criteria, once: by CLI for the cli stage, by model otherwise.
type stageInstructions struct {
	Question string       `json:"question"`
	State    string       `json:"state"`
	Policy   string       `json:"policy"`
	CLIs     jev.Criteria `json:"clis,omitempty"`
	Models   jev.Criteria `json:"models,omitempty"`
	Efforts  jev.Criteria `json:"efforts"`
}

// cliEntry describes one CLI of the cli stage and the models it can run.
type cliEntry struct {
	Description string       `json:"description"`
	Models      jev.Criteria `json:"models"`
}

// modelEntry describes one model, under its model stage criterion name, and the effort labels it runs at.
type modelEntry struct {
	Description string   `json:"description"`
	Efforts     []string `json:"efforts"`
}

// stageQuestion is lv's Choice over gs, whose options must all come from cfg, for a state described
// by guide. effort is the caller's passed-through effort, used for options without one of their own.
func stageQuestion(cfg *config.Config, lv level, guide string, gs []group, effort string) jev.Question {
	in := stageInstructions{Question: lv.question, State: guide, Policy: cfg.Agrouter.RoutingPolicy,
		Efforts: jev.Criteria{}}
	criteria := make(jev.Criteria, 0, len(gs))
	var opts []catalog.Option
	for _, g := range gs {
		o := g.opts[0]
		var c stageCriterion
		switch lv.name {
		case levelCLI:
			c.CLI = o.CLI
		case levelModel:
			c.Model, c.CLI = o.Name, o.CLI
		default:
			c.Effort = optionEffort(o, effort)
		}
		criteria = append(criteria, jev.Criterion{Name: g.label, Value: c})
		opts = append(opts, g.opts...)
	}
	t := newTree(cfg, opts, effort)
	for _, cli := range t.clis {
		if lv.name == levelCLI {
			c, _ := cfg.CLIByName(cli)
			in.CLIs = append(in.CLIs, jev.Criterion{Name: cli, Value: cliEntry{Description: c.Description,
				Models: t.modelCriteria(cli)}})
		} else {
			in.Models = append(in.Models, t.modelCriteria(cli)...)
		}
		if len(t.efforts[cli]) > 0 {
			in.Efforts = append(in.Efforts, jev.Criterion{Name: cli, Value: t.efforts[cli]})
		}
	}
	return jev.Question{Type: jev.TypeChoice, Instructions: in, Criteria: criteria}
}

// tree is the options below a stage's criteria, in catalog order: each CLI's models with their
// effort labels, and each CLI's effort descriptions.
type tree struct {
	clis    []string
	models  map[string][]string     // cli -> model stage criterion names (modelLabel)
	entries map[string]*modelEntry  // cli + "/" + criterion name -> its entry
	efforts map[string]jev.Criteria // cli -> its effort descriptions
}

func newTree(cfg *config.Config, opts []catalog.Option, effort string) *tree {
	models := make(map[string]config.Model, len(cfg.Models))
	for _, m := range cfg.Models {
		models[m.Name] = m
	}
	t := &tree{models: map[string][]string{}, entries: map[string]*modelEntry{}, efforts: map[string]jev.Criteria{}}
	for _, o := range opts {
		if _, ok := t.models[o.CLI]; !ok {
			t.clis = append(t.clis, o.CLI)
		}
		name := modelLabel(o)
		e, ok := t.entries[o.CLI+"/"+name]
		if !ok {
			e = &modelEntry{Description: modelDescription(models, o), Efforts: []string{}}
			t.entries[o.CLI+"/"+name] = e
			t.models[o.CLI] = append(t.models[o.CLI], name)
		}
		label := optionEffort(o, effort)
		if label == "" {
			e.Efforts = append(e.Efforts, noEffortLabel(models, o))
			continue
		}
		e.Efforts = append(e.Efforts, label)
		if !has(t.efforts[o.CLI], label) {
			t.efforts[o.CLI] = append(t.efforts[o.CLI], jev.Criterion{Name: label, Value: effortDescription(cfg, o.CLI, label)})
		}
	}
	return t
}

// modelCriteria lists cli's models with their entries.
func (t *tree) modelCriteria(cli string) jev.Criteria {
	out := make(jev.Criteria, 0, len(t.models[cli]))
	for _, name := range t.models[cli] {
		out = append(out, jev.Criterion{Name: name, Value: *t.entries[cli+"/"+name]})
	}
	return out
}

// relevanceQuestion is the Noul asked beside the stage question in a chunk request.
func relevanceQuestion() jev.Question {
	return jev.Question{Type: jev.TypeNoul, Instructions: relevanceText, Criteria: jev.Criteria{
		{Name: "true", Value: relevanceTrue},
		{Name: "false", Value: relevanceFalse},
	}}
}

// complexityQuestion is the Score over the ordered levels asked in every doc request.
func complexityQuestion(policy string) jev.Question {
	return jev.Question{
		Type:         jev.TypeScore,
		Instructions: instructions{Question: complexityText, State: docGuide, Policy: policy},
		Levels:       append([]string(nil), complexityLevels[:]...),
	}
}

// evidenceQuestion is the Noul asked beside the complexity question: whether the doc text says
// anything about the project, so the reduce weighs chunks by it.
func evidenceQuestion() jev.Question {
	return jev.Question{Type: jev.TypeNoul, Instructions: evidenceText, Criteria: jev.Criteria{
		{Name: "true", Value: evidenceTrue},
		{Name: "false", Value: evidenceFalse},
	}}
}

// complexityQuestions are the questions of every doc request.
func complexityQuestions(ag config.Agrouter) map[string]jev.Question {
	return map[string]jev.Question{
		questionComplexity: complexityQuestion(ag.ComplexityPolicy),
		questionEvidence:   evidenceQuestion(),
	}
}

func modelDescription(models map[string]config.Model, o catalog.Option) string {
	if m, ok := models[o.Name]; ok {
		return m.Description
	}
	return descModelPassed
}

// noEffortLabel is the effort of an option without one: a catalog model has no efforts, while a
// model outside the catalog runs at its CLI's default.
func noEffortLabel(models map[string]config.Model, o catalog.Option) string {
	if _, ok := models[o.Name]; ok {
		return effortNone
	}
	return effortDefault
}

func effortDescription(cfg *config.Config, cli, effort string) string {
	if e, ok := cfg.Efforts[cli+"."+effort]; ok {
		return e.Description
	}
	return descEffortPassed
}

func has(obj jev.Criteria, name string) bool {
	for _, c := range obj {
		if c.Name == name {
			return true
		}
	}
	return false
}

// budget derives the state budgets from the questions over the whole catalog, the largest they can
// be, so eligibility only ever shrinks them. Questions leaving too little room are a config error.
func budget(cfg *config.Config, cat *catalog.Catalog) (prompt.Budget, error) {
	sizes := prompt.Questions{
		Route:      largestStage(cfg, cat, wholeGuide),
		ChunkRoute: largestStage(cfg, cat, chunkGuide),
		Relevance:  questionLen(questionRelevance, relevanceQuestion()),
		Complexity: questionLen(questionComplexity, complexityQuestion(cfg.Agrouter.ComplexityPolicy)),
		Evidence:   questionLen(questionEvidence, evidenceQuestion()),
	}
	b, err := prompt.NewBudget(sizes)
	if err != nil {
		return b, fmt.Errorf("config: [agrouter]: %w", err)
	}
	return b, nil
}

// largestStage is the largest stage question a state described by guide can travel with: the cli
// stage over the whole catalog, the model stage over each CLI's models, and the effort stage over
// each model's efforts (effort labels repeat across models, so they are never asked together).
func largestStage(cfg *config.Config, cat *catalog.Catalog, guide string) int {
	size := func(lv level, opts []catalog.Option) int {
		return questionLen(lv.name, stageQuestion(cfg, lv, guide, groups(opts, lv), ""))
	}
	n := size(routeLevels[0], cat.Options)
	for _, cli := range catalog.CLIs(cat.Options) {
		n = max(n, size(routeLevels[1], catalog.ByCLI(cat.Options, cli)))
	}
	for _, g := range groups(cat.Options, routeLevels[1]) {
		n = max(n, size(routeLevels[2], g.opts))
	}
	return n
}

// questionLen is the serialized size of one "id": question entry in the questions object.
func questionLen(id string, q jev.Question) int {
	data, err := json.Marshal(map[string]jev.Question{id: q})
	if err != nil {
		panic(fmt.Sprintf("router: encode question %q: %v", id, err)) // plain data, cannot fail
	}
	return len(data)
}
