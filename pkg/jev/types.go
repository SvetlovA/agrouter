// Package jev is a client for TypeSafe's Jev "System One" API: one endpoint that answers typed
// questions about a state.
package jev

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Question types.
const (
	TypeChoice = "choice"
	TypeScore  = "score"
	TypeNoul   = "noul"
)

// Request is the body of POST /v1/systemone.
type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Question is one typed question. Choice and Noul use named Criteria; Score uses ordered Levels.
type Question struct {
	Type         string   `json:"type"`
	Instructions any      `json:"instructions"`
	Criteria     Criteria `json:"criteria"`
	Levels       []string `json:"-"`
}

// MarshalJSON encodes Score criteria as an array and Choice/Noul criteria as named objects.
func (q Question) MarshalJSON() ([]byte, error) {
	var criteria any = q.Criteria
	if q.Type == TypeScore {
		criteria = q.Levels
	}
	data, err := json.Marshal(struct {
		Type         string `json:"type"`
		Instructions any    `json:"instructions"`
		Criteria     any    `json:"criteria"`
	}{q.Type, q.Instructions, criteria})
	if err != nil {
		return nil, fmt.Errorf("question: %w", err)
	}
	return data, nil
}

// Criterion is one named criterion; Value is a string, an object or an array.
type Criterion struct {
	Name  string
	Value any
}

// Criteria keeps its order when encoded as a JSON object, so requests are reproducible and read
// in catalog order.
type Criteria []Criterion

// Names returns the criterion names in order.
func (c Criteria) Names() []string {
	names := make([]string, len(c))
	for i, cr := range c {
		names[i] = cr.Name
	}
	return names
}

// MarshalJSON encodes the criteria as an object with keys in slice order.
func (c Criteria) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, cr := range c {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := json.Marshal(cr.Name)
		if err != nil {
			return nil, fmt.Errorf("criterion name: %w", err)
		}
		value, err := json.Marshal(cr.Value)
		if err != nil {
			return nil, fmt.Errorf("criterion %q: %w", cr.Name, err)
		}
		buf.Write(name)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Answer is a validated answer: Choice or Score with probabilities and confidence, or Noul.
type Answer struct {
	Type          string
	Choice        string
	Score         float64
	Legend        map[string]string
	Probabilities map[string]float64
	Confidence    float64
	Noul          float64
}

// response is the wire form of a 200 response; pointers tell a missing number from zero.
type response struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
}

type wireAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Legend        map[string]string  `json:"legend"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
	Noul          *float64           `json:"noul"`
}
