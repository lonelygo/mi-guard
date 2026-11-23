// Package share embeds configuration data distributed with mi-guard.
package share

import _ "embed"

//go:embed mi-guard/mihomo-guard-default.yaml
var defaultConfig string

// DefaultConfig returns the YAML template used by mihomo-guard init.
func DefaultConfig() string { return defaultConfig }
