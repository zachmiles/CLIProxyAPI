package cliproxy

import (
	"maps"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func modelUpstreamID(model *ModelInfo) string {
	if model.MetadataModelID != "" {
		return model.MetadataModelID
	}
	return model.ID
}

func withDiscoveryCompatPools(models []*ModelInfo) []*ModelInfo {
	pools := make(map[string][]string)
	for _, model := range models {
		if model != nil {
			pools[model.ID] = append(pools[model.ID], modelUpstreamID(model))
		}
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model != nil {
			clone := *model
			clone.DiscoveryUpstreams = pools[model.ID]
			out = append(out, &clone)
		}
	}
	return out
}

// Resolve membership before exclusions, aliases, settings, and prefixes. A
// non-nil empty snapshot is a complete listing and must not restore the catalog.
func (s *Service) discoveryModels(cfg *config.Config, auth *coreauth.Auth, base []*ModelInfo, compat bool) []*ModelInfo {
	channel := discoveryChannelForAuth(auth)
	settings := discoverySettings(cfg, channel)
	if !discoveryEnabled(settings.Mode) {
		return base
	}
	var rows map[string]discoveryRow
	if settings.Mode == "live" {
		rows = s.discoveryRows(cfg, auth, channel)
	}
	extra := make(map[string]bool, len(settings.Extra))
	for _, id := range settings.Extra {
		if id = strings.TrimSpace(id); id != "" {
			extra[id] = true
		}
	}
	var out []*ModelInfo
	seen := make(map[string]bool)
	add := func(model *ModelInfo, row discoveredModel, configuredCompat bool) {
		out = append(out, discoveryModelInfo(channel, model, row, configuredCompat))
		seen[modelUpstreamID(model)] = true
		seen[model.ID] = true
	}
	for _, model := range base {
		if model == nil {
			continue
		}
		id := modelUpstreamID(model)
		row, found := rows[id]
		if rows == nil || found || extra[id] {
			add(model, row.Model, compat)
		}
	}
	// Claude discovery only gates models already in the existing pipeline.
	// A raw extra that does not match a catalog row can only surface through
	// an existing non-fork alias, never as a new raw Claude ID.
	if channel == "claude" {
		aliases := oauthModelAliasesForAuth(cfg, channel, auth.Attributes)
		for _, alias := range aliases {
			id := strings.TrimSpace(alias.Name)
			row, found := rows[id]
			if id != "" && !alias.Fork && !seen[id] && (found || extra[id]) {
				add(discoveryCatalogModel(channel, id), row.Model, false)
			}
		}
		return out
	}
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		if !seen[id] {
			add(discoveryCatalogModel(channel, id), rows[id].Model, false)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(extra)) {
		if !seen[id] {
			add(discoveryCatalogModel(channel, id), discoveredModel{}, false)
		}
	}
	return out
}

func discoveryCatalogModel(channel, id string) *ModelInfo {
	if model := registry.LookupStaticModelInfoByChannel(id, channel); model != nil {
		model.ID = id
		return model
	}
	var owner, typ string
	switch channel {
	case "codex":
		owner, typ = "openai", "openai"
	case "claude":
		owner, typ = "anthropic", "claude"
	case "xai":
		owner, typ = "xai", "xai"
	default:
		owner, typ = channel, "openai-compatibility"
	}
	model := &ModelInfo{ID: id, Object: "model", OwnedBy: owner, Type: typ, DisplayName: id, Discovery: true}
	if typ == "openai-compatibility" {
		if known := registry.LookupStaticModelInfo(id); known != nil {
			model.ContextLength, model.InputTokenLimit = known.ContextLength, known.InputTokenLimit
			model.OutputTokenLimit, model.MaxCompletionTokens = known.OutputTokenLimit, known.MaxCompletionTokens
			model.Thinking = known.Thinking
		}
	}
	return model
}

func discoveryModelInfo(channel string, model *ModelInfo, row discoveredModel, configuredCompat bool) *ModelInfo {
	if row.ID == "" {
		return model
	}
	out := cloneModelInfoForCatalogRoute(model)
	out.Discovery = true
	if row.DisplayName != "" && !configuredCompat {
		out.DisplayName = row.DisplayName
	}
	if row.Kind != "" {
		out.DiscoveryKind = row.Kind
		if out.Type == "openai-compatibility" || out.Type == registry.OpenAIImageModelType {
			if row.Kind == "image" {
				out.Type = registry.OpenAIImageModelType
			} else {
				out.Type = "openai-compatibility"
			}
		}
	}
	if row.Family != "" {
		out.DiscoveryFamily = row.Family
	}
	if row.ContextTokens > 0 {
		out.ContextLength = int(row.ContextTokens)
		if channel == "codex" || out.MaxContextLength > 0 {
			out.MaxContextLength = out.ContextLength
		}
	}
	if row.InputTokens > 0 {
		out.InputTokenLimit = int(row.InputTokens)
	}
	if row.OutputTokens > 0 {
		out.OutputTokenLimit = int(row.OutputTokens)
		out.MaxCompletionTokens = out.OutputTokenLimit
	}
	if len(row.Efforts) != 0 {
		thinking := registry.ThinkingSupport{}
		if out.Thinking != nil {
			thinking = *out.Thinking
		}
		thinking.Levels = slices.Clone(row.Efforts)
		out.Thinking = &thinking
		out.ExplicitThinking = true
	}
	if row.ServiceTiers != nil {
		out.DiscoveryServiceTiers = slices.Clone(row.ServiceTiers)
	}
	if row.Created > 0 {
		out.Created = row.Created
	}
	return &out
}

// A fast alias is an existing routed variant, not a separate upstream slug.
// Unrelated native IDs ending in -fast are not treated as variants.
func codexVariantBase(id, upstream string) string {
	if id == upstream || !strings.HasSuffix(id, "-fast") {
		return ""
	}
	namespace, _, prefixed := strings.Cut(id, "/")
	if prefixed {
		upstream = namespace + "/" + strings.TrimPrefix(upstream, namespace+"/")
	}
	return upstream
}

func excludeCodexVariants(models []*ModelInfo, excluded []string) []*ModelInfo {
	if len(excluded) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if base := codexVariantBase(model.ID, modelUpstreamID(model)); base != "" &&
			(coreauth.ModelMatchesExclusions(model.ID, excluded) || coreauth.ModelMatchesExclusions(base, excluded)) {
			continue
		}
		out = append(out, model)
	}
	return out
}
