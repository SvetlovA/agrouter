package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// InstallDefaults creates or refreshes a commented global config template. A file with any
// active settings is preserved so only embedded defaults supply values until users customize them.
func InstallDefaults(path string, embedded []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create global config directory: %w", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // the user-selected global config path
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read global config: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if !isCommentOrBlank(line) {
			return nil
		}
	}

	lines := strings.Split(strings.ReplaceAll(string(embedded), "\r\n", "\n"), "\n")
	for i, line := range lines {
		if !isCommentOrBlank(line) {
			lines[i] = "# " + line
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return fmt.Errorf("write global config template: %w", err)
	}
	return nil
}

// isCommentOrBlank recognizes the comment markers accepted by the INI loader.
func isCommentOrBlank(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";")
}
