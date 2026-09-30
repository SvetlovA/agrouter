package args

import (
	"fmt"
	"strconv"
	"strings"
)

// Redacted renders the argv for debug output: the {prompt} token as <prompt> and raw passthrough as
// a count, so neither the prompt text nor raw tokens are printed. The argv never holds the API key.
// Tokens with spaces, quotes or control characters are shown Go-quoted.
func (r Result) Redacted() string {
	parts := make([]string, 0, r.rawAt+1)
	for i, tok := range r.Argv[:r.rawAt] {
		if i == r.promptAt {
			parts = append(parts, "<prompt>")
			continue
		}
		parts = append(parts, quote(tok))
	}
	if n := len(r.Argv) - r.rawAt; n > 0 {
		parts = append(parts, fmt.Sprintf("<%d raw argument(s)>", n))
	}
	return strings.Join(parts, " ")
}

func quote(tok string) string {
	if tok == "" || strings.ContainsAny(tok, " \t\r\n\"'\\") || strconv.Quote(tok) != `"`+tok+`"` {
		return strconv.Quote(tok)
	}
	return tok
}
