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

// Relevance criteria of a chunk request, asked beside chunk_question.
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

// routeInstructions is the route question's instructions: the question and the catalog, once.
type routeInstructions struct {
	Question string       `json:"question"`
	CLIs     jev.Criteria `json:"clis"`
	Models   jev.Criteria `json:"models"`
	Efforts  jev.Criteria `json:"efforts"`
}

// routeQuestion is the joint Choice over opts, which must all come from cfg. effort is the
// caller's passed-through effort, used for options without one of their own.
func routeQuestion(cfg *config.Config, text string, opts []catalog.Option, effort string) jev.Question {
	models := make(map[string]config.Model, len(cfg.Models))
	for _, m := range cfg.Models {
		models[m.Name] = m
	}
	in := routeInstructions{Question: text}
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
func relevanceQuestion(text string) jev.Question {
	return jev.Question{Type: jev.TypeNoul, Instructions: text, Criteria: jev.Criteria{
		{Name: "true", Value: relevanceTrue},
		{Name: "false", Value: relevanceFalse},
	}}
}

// complexityQuestion is the Score over the ordered levels asked in every doc request.
func complexityQuestion(text string) jev.Question {
	return jev.Question{Type: jev.TypeScore, Instructions: text, Levels: append([]string(nil), complexityLevels[:]...)}
}

// evidenceQuestion is the Noul asked beside the complexity question: whether the doc text says
// anything about the project, so the reduce weighs chunks by it.
func evidenceQuestion(text string) jev.Question {
	return jev.Question{Type: jev.TypeNoul, Instructions: text, Criteria: jev.Criteria{
		{Name: "true", Value: evidenceTrue},
		{Name: "false", Value: evidenceFalse},
	}}
}

// complexityQuestions are the questions of every doc request.
func complexityQuestions(ag config.Agrouter) map[string]jev.Question {
	return map[string]jev.Question{
		questionComplexity: complexityQuestion(ag.ComplexityQuestion),
		questionEvidence:   evidenceQuestion(ag.ComplexityEvidence),
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
	ag := cfg.Agrouter
	sizes := prompt.Questions{
		Route:      questionLen(questionRoute, routeQuestion(cfg, ag.Question, cat.Options, "")),
		ChunkRoute: questionLen(questionRoute, routeQuestion(cfg, ag.ChunkQuestion, cat.Options, "")),
		Relevance:  questionLen(questionRelevance, relevanceQuestion(ag.Relevance)),
		Complexity: questionLen(questionComplexity, complexityQuestion(ag.ComplexityQuestion)),
		Evidence:   questionLen(questionEvidence, evidenceQuestion(ag.ComplexityEvidence)),
	}
	b, err := prompt.NewBudget(sizes)
	if err != nil {
		return b, fmt.Errorf("config: [agrouter] question, chunk_question, relevance, complexity_question or complexity_evidence: %w", err)
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
