package runner

import (
	"fmt"
	"path/filepath"
	"strings"
)

// isBatchFile reports whether path is a Windows batch file, which CreateProcess runs through cmd.exe.
func isBatchFile(path string) bool {
	ext := filepath.Ext(path)
	return strings.EqualFold(ext, ".cmd") || strings.EqualFold(ext, ".bat")
}

// cmdSpecial are the characters cmd.exe acts on outside its quotes; ( and ) end a block in a shim
// that runs its program inside IF ( ... ).
const cmdSpecial = "&|<>^()"

// batchCommandLine returns the cmd.exe command line that runs script with args, each arriving intact
// in a program the script passes them to with %* (an npm shim's node), which splits its command line
// with the CommandLineToArgvW rules. /d skips AutoRun, /v:off disables !delayed! expansion, and /s /c
// strips only the outer quotes. CR, LF and NUL cannot pass through cmd.exe at all and are an error.
//
// cmd.exe parses the line once for /c and again when the shim expands %*, and both parses see the
// same quotes, so no ^ escaping (which each parse consumes once) is used. Instead two quote states
// are tracked: cmd.exe's, toggled by every ", and the program's, toggled by " but not by \".
//   - a literal " is \" (cmd.exe's state flips, the program's does not);
//   - a space or tab needs the program's quotes and a cmdSpecial character cmd.exe's: when the
//     needed state is off, a bare " is written first, which turns it on (and flips the other);
//   - backslashes are doubled before any ", once more for a literal one;
//   - % becomes %%cd:~,% (a literal % then an empty substring of %cd%), so no %VAR% can form.
//
// No "" pair ever appears inside the program's quotes, where parsers disagree about it.
func batchCommandLine(script string, args []string) (string, error) {
	q := batchQuoter{}
	q.b.WriteString(`cmd.exe /d /v:off /s /c "`)
	for i, a := range append([]string{script}, args...) {
		if strings.ContainsAny(a, "\r\n\x00") {
			return "", fmt.Errorf("argument %d contains a line break or NUL, which a batch file cannot receive", i)
		}
		if i > 0 {
			q.b.WriteByte(' ')
		}
		q.arg(a)
	}
	q.b.WriteByte('"')
	return q.b.String(), nil
}

// batchQuoter writes arguments tracking cmd.exe's quote state across them (see batchCommandLine).
type batchQuoter struct {
	b    strings.Builder
	cmdQ bool // inside cmd.exe's quotes
}

// arg writes one argument, leaving the program's quotes closed so the next space separates.
func (q *batchQuoter) arg(s string) {
	progQ := false
	backslashes := 0
	flush := func(beforeQuote bool) {
		n := backslashes
		if beforeQuote {
			n *= 2
		}
		q.b.WriteString(strings.Repeat(`\`, n))
		backslashes = 0
	}
	toggle := func() {
		flush(true)
		q.b.WriteByte('"')
		progQ, q.cmdQ = !progQ, !q.cmdQ
	}

	toggle() // always opened, so an empty argument survives
	for _, r := range s {
		switch r {
		case '\\':
			backslashes++
		case '"':
			flush(true)
			q.b.WriteString(`\"`)
			q.cmdQ = !q.cmdQ
		case '%':
			flush(false)
			q.b.WriteString(`%%cd:~,%`)
		default:
			if (r == ' ' || r == '\t') && !progQ || strings.ContainsRune(cmdSpecial, r) && !q.cmdQ {
				toggle()
			}
			flush(false)
			q.b.WriteRune(r)
		}
	}
	if progQ {
		toggle()
	}
	flush(false)
}
