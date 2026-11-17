package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// NamedCompatExclusionChannel returns the config channel for compatibility
// groups that use OAuth exclusions regardless of credential kind.
func NamedCompatExclusionChannel(auth *Auth) string {
	if auth == nil {
		return ""
	}
	name := strings.TrimSpace(auth.Attributes["compat_name"])
	if name == "" {
		name = strings.TrimSpace(auth.Provider)
		if strings.EqualFold(name, "openai-compatibility") {
			name = strings.TrimSpace(auth.Label)
		}
	}
	switch strings.TrimPrefix(strings.ToLower(name), "openai-compatible-") {
	case "opencode-go":
		return "opencode-go"
	case "zai":
		return "zai"
	default:
		return ""
	}
}

// NamedCompatExcludedModels returns the effective rules for a named
// compatibility group. A nonempty credential list replaces the channel list.
func NamedCompatExcludedModels(cfg *internalconfig.Config, auth *Auth) []string {
	channel := NamedCompatExclusionChannel(auth)
	if channel == "" {
		return nil
	}
	if raw := strings.TrimSpace(auth.Attributes["excluded_models"]); raw != "" {
		return strings.Split(raw, ",")
	}
	if cfg == nil {
		return nil
	}
	return cfg.OAuthExcludedModels[channel]
}

// ModelMatchesExclusions matches a model ID against case-insensitive patterns,
// where '*' matches any substring. All other characters are literal.
func ModelMatchesExclusions(model string, patterns []string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, pattern := range patterns {
		if matchExclusionWildcard(strings.ToLower(strings.TrimSpace(pattern)), model) {
			return true
		}
	}
	return false
}

// NamedCompatModelExcluded matches a route or upstream model, with or without
// its thinking suffix. Setting a suffix cannot re-enable an excluded base ID.
func NamedCompatModelExcluded(model string, patterns []string) bool {
	if len(patterns) == 0 || strings.TrimSpace(model) == "" {
		return false
	}
	if ModelMatchesExclusions(model, patterns) {
		return true
	}
	suffix := thinking.ParseSuffix(strings.TrimSpace(model))
	return suffix.HasSuffix && ModelMatchesExclusions(suffix.ModelName, patterns)
}

func namedCompatRouteExcluded(auth *Auth, model string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	if NamedCompatModelExcluded(model, patterns) {
		return true
	}
	if prefix := strings.TrimSpace(auth.Prefix); prefix != "" {
		if namespace, base, ok := strings.Cut(strings.TrimSpace(model), "/"); ok && strings.EqualFold(namespace, prefix) {
			return NamedCompatModelExcluded(base, patterns)
		}
	}
	return false
}

func namedCompatExecutionOverrideExcluded(cfg *internalconfig.Config, auth *Auth, executionModel string) bool {
	if executionModel == "" || cfg.Home.Enabled {
		return false
	}
	return namedCompatRouteExcluded(auth, executionModel, NamedCompatExcludedModels(cfg, auth))
}

func matchExclusionWildcard(pattern, value string) bool {
	prefix, rest, wild := strings.Cut(pattern, "*")
	if !wild {
		return pattern != "" && pattern == value
	}
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	value = value[len(prefix):]
	for {
		part, remaining, more := strings.Cut(rest, "*")
		if !more {
			return strings.HasSuffix(value, part)
		}
		at := strings.Index(value, part)
		if at < 0 {
			return false
		}
		value, rest = value[at+len(part):], remaining
	}
}
