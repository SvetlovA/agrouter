package defaults_test

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
)

func TestEmbeddedLoadsAndValidates(t *testing.T) {
	_, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
}

// TestNoInlineComments guards against a trailing "; ..." or "# ..." on a value line: with
// IgnoreInlineComment it would silently become part of the value.
func TestNoInlineComments(t *testing.T) {
	inline := regexp.MustCompile(`\s[;#]`)
	sc := bufio.NewScanner(bytes.NewReader(defaults.Config))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		require.True(t, ok, "line %d is neither a comment, a section nor a key: %q", n, line)
		assert.False(t, inline.MatchString(value), "line %d carries an inline comment: %q", n, line)
	}
	require.NoError(t, sc.Err())
}
