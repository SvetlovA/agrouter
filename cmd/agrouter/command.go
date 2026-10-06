package main

import (
	"strconv"
	"strings"
	"unicode"
)

// commandText renders argv for inspection and copying; the caller adapts it to their terminal.
func commandText(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = arg
		if arg == "" || strings.ContainsFunc(arg, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("\"'`$&|;<>*?!()[]{}", r)
		}) {
			parts[i] = strconv.Quote(arg)
		}
	}
	return strings.Join(parts, " ")
}
