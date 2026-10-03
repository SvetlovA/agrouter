// Package defaults embeds agrouter's default config: the v1 catalog and each CLI's argument mappings.
package defaults

import _ "embed"

// Config is the embedded defaults file, the lowest config layer.
//
//go:embed config
var Config []byte
