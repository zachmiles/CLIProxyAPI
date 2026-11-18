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

// inventoryModelKind classifies output, not input: vision-capable chat models
// still belong to the chat listing. Generic provider types alone imply no kind.
func inventoryModelKind(model *ModelInfo) string {
	switch model.Type {
	case registry.OpenAIImageModelType, "image":
		return "image"
	case "openai-video", "video":
		return "video"
	case "openai-audio", "openai-speech", "audio", "speech":
		return "speech"
	case "embedding", "embeddings":
		return "embedding"
	}
	if slices.Contains(model.SupportedOutputModalities, "text") {
		return "chat"
	}
	for _, kind := range []string{"video", "image", "speech", "embedding"} {
		if slices.Contains(model.SupportedOutputModalities, kind) {
			return kind
		}
	}
	if slices.Contains(model.SupportedOutputModalities, "audio") {
		return "speech"
	}
	if kind := strings.ToLower(strings.TrimSpace(model.DiscoveryKind)); kind != "" {
		if kind == "audio" || kind == "tts" {
			return "speech"
		}
		return kind
	}
	// Some built-ins carry only their provider type and canonical model ID.
	id := modelUpstreamID(model)
	switch model.Type {
	case "openai":
		if strings.HasPrefix(id, "gpt-image-") {
			return "image"
		}
	case "xai":
		switch {
		case strings.HasPrefix(id, "grok-imagine-video"):
			return "video"
		case strings.HasPrefix(id, "grok-imagine-image"):
			return "image"
		case strings.HasPrefix(id, "grok-voice-"), strings.HasSuffix(id, "-tts"), strings.Contains(id, "-tts-"):
			return "speech"
		}
	}
	return "chat"
}

func discoveryListingCovers(channel, kind string) bool {
	return slices.Contains(modelDiscoveryListedKinds[channel], kind)
}

func discoveryCatalogKinds(cfg *config.Config, auth *coreauth.Auth, channel string) map[string]string {
	models := registry.GetStaticModelDefinitionsByChannel(channel)
	if cfg != nil && (channel == "opencode-go" || channel == "zai") {
		entry := configEntryForAuthIndex(auth, cfg.OpenAICompatibility)
		if entry == nil || entry.Disabled {
			for i := range cfg.OpenAICompatibility {
				candidate := &cfg.OpenAICompatibility[i]
				if !candidate.Disabled && strings.EqualFold(strings.TrimSpace(candidate.Name), channel) {
					entry = candidate
					break
				}
			}
		}
		if entry != nil && !entry.Disabled {
			models = buildOpenAICompatibilityConfigModels(entry)
		}
	}
	kinds := make(map[string]string)
	for _, model := range models {
		if model != nil {
			if kind := inventoryModelKind(model); kind != "chat" {
				kinds[modelUpstreamID(model)] = kind
			}
		}
	}
	return kinds
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
	addOnly := settings.Mode == "live" && !settings.RetirementEnabled()
	add := func(model *ModelInfo, row discoveredModel, configuredCompat bool) {
		out = append(out, discoveryModelInfo(channel, model, row, configuredCompat))
		seen[modelUpstreamID(model)] = true
		seen[model.ID] = true
	}
	addNew := func(id string, row discoveredModel) {
		model := discoveryCatalogModel(channel, id)
		model.DiscoveryCatalogAddition = addOnly || channel == "codex" && settings.Mode == "live"
		add(model, row, false)
	}
	for _, model := range base {
		if model == nil {
			continue
		}
		id := modelUpstreamID(model)
		row, found := rows[id]
		if !discoveryListingCovers(channel, inventoryModelKind(model)) {
			// An out-of-scope model follows the catalog, not this listing's
			// membership or metadata (including misclassified chat rows).
			add(model, discoveredModel{}, compat)
		} else if rows == nil || addOnly || found || extra[id] {
			add(model, row.Model, compat)
		}
	}
	// Claude's default policy only gates existing routes. Add-only uses the
	// same union as the other channels; inventory still projects Claude aliases.
	if channel == "claude" && !addOnly {
		aliases := oauthModelAliasesForAuth(cfg, channel, auth.Attributes)
		for _, alias := range aliases {
			id := strings.TrimSpace(alias.Name)
			row, found := rows[id]
			if id != "" && !alias.Fork && !seen[id] && (found || extra[id]) {
				addNew(id, row.Model)
			}
		}
		return out
	}
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		if !seen[id] {
			addNew(id, rows[id].Model)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(extra)) {
		if !seen[id] {
			addNew(id, discoveredModel{})
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
	if channel == "codex" {
		return codexDiscoveryModelInfo(out, row)
	}
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
		if out.MaxContextLength > 0 {
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

func codexDiscoveryModelInfo(model ModelInfo, row discoveredModel) *ModelInfo {
	template, templated := registry.LookupCodexClientModelMetadata(modelUpstreamID(&model))
	// A known catalog title (including an operator's display-name override)
	// stays as-is, matching the existing Codex client presentation.
	if model.DisplayName == "" || model.Discovery && templated {
		model.DisplayName = template.DisplayName
	}
	if model.ContextLength == 0 {
		model.ContextLength = template.ContextWindow
	}
	if model.Thinking == nil && len(template.Efforts) != 0 {
		model.Thinking = &registry.ThinkingSupport{Levels: template.Efforts}
	}
	if templated {
		// Leave template reasoning, tiers, and windows to the client catalog.
		// In particular, don't set ExplicitThinking or MaxContextLength from a listing.
		return &model
	}

	if model.DisplayName == "" || model.Discovery && model.DisplayName == model.ID && row.DisplayName != "" {
		model.DisplayName = row.DisplayName
	}
	if model.ContextLength == 0 {
		model.ContextLength = int(row.ContextTokens)
	}
	if model.InputTokenLimit == 0 {
		model.InputTokenLimit = int(row.InputTokens)
	}
	if model.OutputTokenLimit == 0 && model.MaxCompletionTokens == 0 {
		model.OutputTokenLimit = int(row.OutputTokens)
		model.MaxCompletionTokens = model.OutputTokenLimit
	}
	if model.Thinking == nil && len(row.Efforts) != 0 {
		model.Thinking = &registry.ThinkingSupport{Levels: slices.Clone(row.Efforts)}
		model.DiscoveryDefaultEffort = row.DefaultEffort
	}
	if model.Created == 0 {
		model.Created = row.Created
	}
	model.Discovery = true
	model.DiscoveryMaxContext = int(row.MaxContextTokens)
	model.DiscoveryServiceTiers = slices.Clone(row.ServiceTiers)
	return &model
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
