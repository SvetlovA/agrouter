package main

import (
	"context"
	"errors"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/router"
)

// Routing failure codes in the reason JSON, from the typed errors only: never from messages or
// response bodies.
const (
	reasonNoKey          = "jev_no_key"
	reasonUnauthorized   = "jev_unauthorized"
	reasonTimeout        = "jev_timeout"
	reasonCanceled       = "jev_canceled"
	reasonAPIError       = "jev_api_error"
	reasonTransportError = "jev_transport_error"
	reasonInvalidAnswer  = "jev_invalid_answer"
	reasonEmptyAnswer    = "jev_empty_answer"
	reasonNoDecision     = "no_decision"
)

// Null value codes in null_reasons.
const (
	nullRoutingFailed = "routing_failed"       // routing failed with the CLI known: left to the child
	nullUnresolved    = "unresolved"           // routing failed with more than one CLI: nothing chosen
	nullDelegated     = "delegated"            // routing succeeded and left the value to the child
	nullNoEffort      = "no_applicable_effort" // the chosen model has no effort levels
)

// reasonJSON is why routing failed: a normalized code, the failing stage when known, and a fixed
// message that never carries the prompt, docs, key or a response body.
type reasonJSON struct {
	Code    string `json:"code"`
	Stage   string `json:"stage,omitempty"`
	Message string `json:"message"`
}

// nullReasonJSON is why one selection field is null.
type nullReasonJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// nullReasonsJSON has an entry for every null selection field.
type nullReasonsJSON struct {
	CLI    *nullReasonJSON `json:"cli,omitempty"`
	Model  *nullReasonJSON `json:"model,omitempty"`
	Effort *nullReasonJSON `json:"effort,omitempty"`
}

// failureReason classifies a routing failure; nil for nil.
func failureReason(err error) *reasonJSON {
	if err == nil {
		return nil
	}
	out := &reasonJSON{Code: failureCode(err)}
	if se, ok := errors.AsType[*router.StageError](err); ok {
		out.Stage = se.Stage
	}
	out.Message = failureMessage(out.Code, out.Stage)
	return out
}

// failureCode maps the typed errors in err's chain to a code. The deadline is checked before the
// transport, since a request cut off by it fails in the transport too.
func failureCode(err error) string {
	switch {
	case errors.Is(err, jev.ErrNoKey):
		return reasonNoKey
	case errors.Is(err, jev.ErrUnauthorized):
		return reasonUnauthorized
	case errors.Is(err, context.DeadlineExceeded):
		return reasonTimeout
	case errors.Is(err, context.Canceled):
		return reasonCanceled
	case errors.Is(err, jev.ErrEmptyAnswer):
		return reasonEmptyAnswer
	case errors.Is(err, jev.ErrMalformed):
		return reasonInvalidAnswer
	case errors.Is(err, jev.ErrTransport):
		return reasonTransportError
	}
	if _, ok := errors.AsType[*jev.StatusError](err); ok {
		return reasonAPIError
	}
	return reasonNoDecision
}

// stageSubjects name what Jev was doing at each stage.
var stageSubjects = map[string]string{
	router.LevelCLI:        "Jev CLI selection",
	router.LevelModel:      "Jev model selection",
	router.LevelEffort:     "Jev effort selection",
	router.LevelComplexity: "Jev project complexity scoring",
}

// failureDetails complete "<subject> failed: ..." for the codes that are not about time.
var failureDetails = map[string]string{
	reasonNoKey:          "no Jev API key is set",
	reasonUnauthorized:   "the Jev API key was rejected",
	reasonAPIError:       "Jev returned an error status",
	reasonTransportError: "Jev could not be reached",
	reasonInvalidAnswer:  "Jev returned an invalid answer",
	reasonEmptyAnswer:    "Jev returned no answer",
	reasonNoDecision:     "Jev could not decide",
}

// failureMessage is the fixed message for code at stage.
func failureMessage(code, stage string) string {
	subject, ok := stageSubjects[stage]
	if !ok {
		subject = "Jev routing"
	}
	switch code {
	case reasonTimeout:
		return subject + " timed out"
	case reasonCanceled:
		return subject + " was canceled"
	default:
		return subject + " failed: " + failureDetails[code]
	}
}

// nullReasons explains every null field of s. failed is true when routing failed; noEffort when the
// chosen model has no effort levels. It returns nil when no field is null.
func nullReasons(s selectionJSON, failed, noEffort bool) *nullReasonsJSON {
	out := &nullReasonsJSON{}
	switch {
	case s.CLI == nil:
		// more than one CLI was left: nothing was chosen
		out.CLI = &nullReasonJSON{nullUnresolved, "No CLI chosen: routing failed with more than one CLI left"}
		if s.Model == nil {
			out.Model = &nullReasonJSON{nullUnresolved, "No model chosen: no CLI was chosen"}
		}
		if s.Effort == nil {
			out.Effort = &nullReasonJSON{nullUnresolved, "No effort chosen: no CLI was chosen"}
		}
	case failed:
		if s.Model == nil {
			out.Model = &nullReasonJSON{nullRoutingFailed, "Model delegated to the child after routing failed"}
		}
		if s.Effort == nil {
			out.Effort = &nullReasonJSON{nullRoutingFailed, "Effort delegated to the child after routing failed"}
		}
	default:
		if s.Model == nil {
			out.Model = &nullReasonJSON{nullDelegated, "Model left to the child's default"}
		}
		switch {
		case s.Effort != nil:
		case noEffort:
			out.Effort = &nullReasonJSON{nullNoEffort, "The chosen model has no effort levels"}
		default:
			out.Effort = &nullReasonJSON{nullDelegated, "Effort left to the child's default"}
		}
	}
	if *out == (nullReasonsJSON{}) {
		return nil
	}
	return out
}
