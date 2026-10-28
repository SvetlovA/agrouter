package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallDefaults(t *testing.T) {
	const embedded = "# reference\r\n; note\r\n\r\n[agrouter]\r\ntimeout = 10s\r\n"
	const template = "# reference\n; note\n\n# [agrouter]\n# timeout = 10s\n"
	for _, tc := range []struct {
		name, existing, want string
		missing              bool
	}{
		{name: "missing", missing: true, want: template},
		{name: "empty", want: template},
		{name: "whitespace", existing: " \t\r\n", want: template},
		{name: "comments", existing: " # old template\n ; old value\n", want: template},
		{name: "customized", existing: "# custom\n[agrouter]\ntimeout = 20s\n", want: "# custom\n[agrouter]\ntimeout = 20s\n"},
		{name: "active section", existing: "[agrouter]\n# timeout = 20s\n", want: "[agrouter]\n# timeout = 20s\n"},
		{name: "malformed", existing: "api_key: private-key\n", want: "api_key: private-key\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", "config")
			if !tc.missing {
				writeFile(t, filepath.Dir(path), "config", tc.existing)
			}
			require.NoError(t, InstallDefaults(path, []byte(embedded)))
			data, err := os.ReadFile(path) //nolint:gosec // isolated test config
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(data))

			// a second invocation must preserve custom settings and keep the template valid
			require.NoError(t, InstallDefaults(path, []byte(embedded)))
			data, err = os.ReadFile(path) //nolint:gosec // isolated test config
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(data))
		})
	}
}

func TestInstallDefaults_RefreshAndLayering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "global", "config")
	require.NoError(t, InstallDefaults(path, []byte(baseConfig)))
	// an upgrade refreshes the reference without pinning the previous embedded values
	updated := baseConfig + "\n[cli.alpha]\ncommand = upgraded-agent\n"
	require.NoError(t, InstallDefaults(path, []byte(updated)))
	data, err := os.ReadFile(path) //nolint:gosec // isolated test config
	require.NoError(t, err)
	assert.Contains(t, string(data), "# command = upgraded-agent")

	local := writeFile(t, t.TempDir(), ".agrouter/config", "[agrouter]\ntimeout = 25s\n")
	cfg, err := Load(Sources{Embedded: []byte(updated), GlobalPath: path, LocalPath: local})
	require.NoError(t, err)
	assert.Equal(t, "upgraded-agent", cfg.CLIs[0].Command)
	assert.Equal(t, "25s", cfg.Agrouter.Timeout.String())
	assert.Equal(t, LayerEmbedded, cfg.Classifier.APIKeySource)
}

func TestInstallDefaults_Permissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "global")
	path := filepath.Join(dir, "config")
	require.NoError(t, InstallDefaults(path, []byte(baseConfig)))
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0o700}, {path, 0o600}} {
		info, err := os.Stat(tc.path)
		require.NoError(t, err)
		assert.Equal(t, tc.mode, info.Mode().Perm())
	}
}

func TestInstallDefaults_Errors(t *testing.T) {
	t.Run("create directory", func(t *testing.T) {
		parent := writeFile(t, t.TempDir(), "blocked", "file")
		err := InstallDefaults(filepath.Join(parent, "config"), []byte(baseConfig))
		require.ErrorContains(t, err, "create global config directory")
	})
	t.Run("read config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		require.NoError(t, os.Mkdir(path, 0o700))
		err := InstallDefaults(path, []byte(baseConfig))
		require.ErrorContains(t, err, "read global config")
	})
	t.Run("write config", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks require privileges on Windows")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "config")
		require.NoError(t, os.Symlink(filepath.Join(dir, "missing", "target"), path))
		err := InstallDefaults(path, []byte(baseConfig))
		require.ErrorContains(t, err, "write global config template")
	})
}
