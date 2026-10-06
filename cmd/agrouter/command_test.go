package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCommandText(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"ordinary arguments", []string{"agent", "run", "--model", "fast-1", "--effort=low"}, "agent run --model fast-1 --effort=low"},
		{"prompt and config", []string{"agent", "-c", `effort="high"`, "--", "fix the test"}, `agent -c "effort=\"high\"" -- "fix the test"`},
		{"empty and special arguments", []string{"my cli", "", "it's", "$HOME; $(bad) & |", "line\none"}, `"my cli" "" "it's" "$HOME; $(bad) & |" "line\none"`},
		{"no arguments", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, commandText(tc.argv))
		})
	}
}
