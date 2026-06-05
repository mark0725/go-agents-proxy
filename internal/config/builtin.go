package config

import (
	_ "embed"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed builtin_providers.yaml
var builtinProvidersYAML []byte

var (
	builtinProvidersOnce sync.Once
	builtinProviders     map[string]ProviderConfig
)

// BuiltinProviders returns the embedded catalog of well-known providers used to
// pre-fill the providers UI. The catalog is parsed once and cached.
func BuiltinProviders() map[string]ProviderConfig {
	builtinProvidersOnce.Do(func() {
		var parsed map[string]ProviderConfig
		if err := yaml.Unmarshal(builtinProvidersYAML, &parsed); err != nil {
			panic("parse builtin providers: " + err.Error())
		}
		builtinProviders = parsed
	})
	return builtinProviders
}
