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
	questionRoute     = "route"
	questionRelevance = "relevance"
)

// Encoding selects how the catalog goes into the route question.
type Encoding int

// Encodings of the route question.
const (
	// EncodingCompact puts the descriptions once in a structured instructions object; each option's
	// criterion names its cli, model and effort.
	EncodingCompact Encoding = iota
)

// Criterion values for an option without an effort.
const (
	effortNone       = "none (not supported)"
	descModelPassed  = "not in the catalog: passed through as given"
	descEffortPassed = "not in the catalog for this CLI: passed through as given"
)

// Relevance criteria of a chunk request (see the design's "Splitting large state").
const (
	relevanceTrue  = "`chunk.text` adds to what the task is, what it requires, or what makes it hard"
	relevanceFalse = "`chunk.text` is only material the task works on, or repeats `anchor`"
)

// object is a JSON object that keeps its keys in slice order, so requests are reproducible.
type object = jev.Criteria

// routeCriterion is one option's criterion in the compact encoding.
type routeCriterion struct {
	CLI    string `json:"cli"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// routeInstructions is the compact encoding's instructions: the question and the catalog, once.
type routeInstructions struct {
	Question string `json:"question"`
	CLIs     object `json:"clis"`
	Models   object `json:"models"`
	Efforts  object `json:"efforts"`
}

// routeQuestion is the joint Choice over opts, which must all come from cfg. effort is the
// caller's passed-through effort, used for options without one of their own.
func routeQuestion(cfg *config.Config, text string, opts []catalog.Option, effort string, _ Encoding) jev.Question {
	models := make(map[string]config.Model, len(cfg.Models))
	for _, m := range cfg.Models {
		models[m.Name] = m
	}
	in := routeInstructions{Question: text}
	criteria := make(jev.Criteria, 0, len(opts))
	var efforts []string          // CLIs with efforts, in order
	levels := map[string]object{} // cli -> its efforts among opts
	for _, o := range opts {
		if !has(in.CLIs, o.CLI) {
			cli, _ := cfg.CLIByName(o.CLI)
			in.CLIs = append(in.CLIs, jev.Criterion{Name: o.CLI, Value: cli.Description})
		}
		if !has(in.Models, o.Name) {
			in.Models = append(in.Models, jev.Criterion{Name: o.Name, Value: modelDescription(models, o)})
		}
		eff := o.Effort
		if eff == "" {
			eff = effort
		}
		label := eff
		if eff == "" {
			label = effortNone
		} else if !has(levels[o.CLI], eff) {
			if _, ok := levels[o.CLI]; !ok {
				efforts = append(efforts, o.CLI)
			}
			desc := descEffortPassed
			if e, ok := cfg.Efforts[o.CLI+"."+eff]; ok {
				desc = e.Description
			}
			levels[o.CLI] = append(levels[o.CLI], jev.Criterion{Name: eff, Value: desc})
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

func modelDescription(models map[string]config.Model, o catalog.Option) string {
	if m, ok := models[o.Name]; ok {
		return m.Description
	}
	return descModelPassed
}

func has(obj object, name string) bool {
	for _, c := range obj {
		if c.Name == name {
			return true
		}
	}
	return false
}

// budget derives the state budgets from the questions over the whole catalog, the largest they can
// be, so eligibility only ever shrinks them. Questions leaving too little room are a config error.
func budget(cfg *config.Config, cat *catalog.Catalog, enc Encoding) (prompt.Budget, error) {
	ag := cfg.Agrouter
	sizes := prompt.Questions{
		Route:      questionLen(questionRoute, routeQuestion(cfg, ag.Question, cat.Options, "", enc)),
		ChunkRoute: questionLen(questionRoute, routeQuestion(cfg, ag.ChunkQuestion, cat.Options, "", enc)),
		Relevance:  questionLen(questionRelevance, relevanceQuestion(ag.Relevance)),
	}
	b, err := prompt.NewBudget(sizes)
	if err != nil {
		return b, fmt.Errorf("config: [agrouter] question, chunk_question or relevance: %w", err)
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
