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
	parts := make([]string, 0, len(r.Argv))
	for i := 0; i < len(r.Argv); i++ {
		switch {
		case i == r.rawAt && r.rawLen > 0:
			parts = append(parts, fmt.Sprintf("<%d raw argument(s)>", r.rawLen))
			i += r.rawLen - 1
		case i == r.promptAt:
			parts = append(parts, "<prompt>")
		default:
			parts = append(parts, quote(r.Argv[i]))
		}
	}
	return strings.Join(parts, " ")
}

func quote(tok string) string {
	if tok == "" || strings.ContainsAny(tok, " \t\r\n\"'\\") || strconv.Quote(tok) != `"`+tok+`"` {
		return strconv.Quote(tok)
	}
	return tok
}
