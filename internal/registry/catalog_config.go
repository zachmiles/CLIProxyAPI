package registry

import (
	"fmt"
	"net/url"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CatalogSources overrides the source of each model catalog independently.
// Empty values retain the process default (embedded or official remote catalogs).
type CatalogSources struct {
	Catalog      string                    `yaml:"catalog" json:"catalog,omitempty"`
	CodexCatalog string                    `yaml:"codex-catalog" json:"codex-catalog,omitempty"`
	DevinCatalog string                    `yaml:"devin-catalog" json:"devin-catalog,omitempty"`
	Discovery    map[string]ModelDiscovery `yaml:"discovery,omitempty" json:"discovery,omitempty"`
}

type ModelDiscovery struct {
	Mode   string   `yaml:"mode,omitempty" json:"mode,omitempty"`
	Extra  []string `yaml:"extra,omitempty" json:"extra,omitempty"`
	Retire *bool    `yaml:"retire,omitempty" json:"retire,omitempty"`
}

// RetirementEnabled defaults to true when the option is unspecified.
func (m ModelDiscovery) RetirementEnabled() bool {
	return m.Retire == nil || *m.Retire
}

func (m *ModelDiscovery) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value != "retire" {
				continue
			}
			value := node.Content[i+1]
			if value.Kind == yaml.AliasNode {
				value = value.Alias
			}
			if value == nil || value.ShortTag() != "!!bool" {
				return fmt.Errorf("model discovery retire must be a boolean")
			}
		}
	}
	type plain ModelDiscovery
	return node.Decode((*plain)(m))
}

// Validate checks catalog sources and per-channel discovery settings.
func (m CatalogSources) Validate() error {
	for channel, discovery := range m.Discovery {
		switch channel {
		case "codex", "opencode-go", "claude", "xai", "zai":
		default:
			return fmt.Errorf("models.discovery has an unsupported channel %q", channel)
		}
		switch discovery.Mode {
		case "", "off", "shadow", "live":
		default:
			return fmt.Errorf("models.discovery.%s.mode must be off, shadow, or live", channel)
		}
	}
	for name, source := range map[string]string{"catalog": m.Catalog, "codex-catalog": m.CodexCatalog, "devin-catalog": m.DevinCatalog} {
		if source == "" || filepath.IsAbs(source) {
			continue
		}
		parsed, errParse := url.Parse(source)
		if errParse != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return fmt.Errorf("models.%s must be an http(s) URL or an absolute local path", name)
		}
	}
	return nil
}
