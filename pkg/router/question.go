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
	questionRoute      = "route"
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
	// routeText asks the joint Choice over every eligible (cli, model, effort) option.
	routeText = "Which coding-agent CLI, model and reasoning effort should run the task described in `state`? " +
		"Choose following `policy`. Look up each option's `model` in `models`, its `cli` in `clis`, " +
		"and its `effort` under that CLI in `efforts`."

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

	// relevanceText is asked beside the route question of a chunk request. Its 0-to-1 answer
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

// routeCriterion is one option's criterion: its cli, model and effort, described in the instructions.
type routeCriterion struct {
	CLI    string `json:"cli"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// instructions is a question's instructions: the question, its state guide and the policy from config.
type instructions struct {
	Question string `json:"question"`
	State    string `json:"state"`
	Policy   string `json:"policy"`
}

// routeInstructions is the route question's instructions: the question, its state guide, the
// routing policy and the catalog, once.
type routeInstructions struct {
	Question string       `json:"question"`
	State    string       `json:"state"`
	Policy   string       `json:"policy"`
	CLIs     jev.Criteria `json:"clis"`
	Models   jev.Criteria `json:"models"`
	Efforts  jev.Criteria `json:"efforts"`
}

// routeQuestion is the joint Choice over opts, which must all come from cfg, for a state described
// by guide. effort is the caller's passed-through effort, used for options without one of their own.
func routeQuestion(cfg *config.Config, guide string, opts []catalog.Option, effort string) jev.Question {
	models := make(map[string]config.Model, len(cfg.Models))
	for _, m := range cfg.Models {
		models[m.Name] = m
	}
	in := routeInstructions{Question: routeText, State: guide, Policy: cfg.Agrouter.RoutingPolicy}
	criteria := make(jev.Criteria, 0, len(opts))
	var efforts []string                // CLIs with efforts, in order
	levels := map[string]jev.Criteria{} // cli -> its efforts among opts
	for _, o := range opts {
		if !has(in.CLIs, o.CLI) {
			cli, _ := cfg.CLIByName(o.CLI)
			in.CLIs = append(in.CLIs, jev.Criterion{Name: o.CLI, Value: cli.Description})
		}
		if !has(in.Models, o.Name) {
			in.Models = append(in.Models, jev.Criterion{Name: o.Name, Value: modelDescription(models, o)})
		}
		eff := optionEffort(o, effort)
		label := eff
		if eff == "" {
			label = noEffortLabel(models, o)
		} else if !has(levels[o.CLI], eff) {
			if _, ok := levels[o.CLI]; !ok {
				efforts = append(efforts, o.CLI)
			}
			levels[o.CLI] = append(levels[o.CLI], jev.Criterion{Name: eff, Value: effortDescription(cfg, o.CLI, eff)})
		}
		criteria = append(criteria, jev.Criterion{Name: o.ID,
			Value: routeCriterion{CLI: o.CLI, Model: o.Name, Effort: label}})
	}
	for _, cli := range efforts {
		in.Efforts = append(in.Efforts, jev.Criterion{Name: cli, Value: levels[cli]})
	}
	return jev.Question{Type: jev.TypeChoice, Instructions: in, Criteria: criteria}
}

// relevanceQuestion is the Noul asked beside the route question in a chunk request.
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
		Route:      questionLen(questionRoute, routeQuestion(cfg, wholeGuide, cat.Options, "")),
		ChunkRoute: questionLen(questionRoute, routeQuestion(cfg, chunkGuide, cat.Options, "")),
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

// questionLen is the serialized size of one "id": question entry in the questions object.
func questionLen(id string, q jev.Question) int {
	data, err := json.Marshal(map[string]jev.Question{id: q})
	if err != nil {
		panic(fmt.Sprintf("router: encode question %q: %v", id, err)) // plain data, cannot fail
	}
	return len(data)
}
