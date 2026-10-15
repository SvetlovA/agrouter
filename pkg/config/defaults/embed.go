// Package defaults embeds agrouter's default config: the v1 catalog, invocation descriptors and availability checks.
package defaults

import _ "embed"

// Config is the embedded defaults file, the lowest config layer.
//
//go:embed config
var Config []byte
