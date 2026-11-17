package openai

import (
	"slices"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
)

func (h *OpenAIAPIHandler) codexClientModelsResponse(clientVersion ...string) map[string]any {
	version := ""
	if len(clientVersion) > 0 {
		version = clientVersion[0]
	}
	optimizeMultiAgentV2 := false
	var applyPatchCapabilityForModel codexmodels.ApplyPatchCapabilityForModelFunc
	if h != nil {
		if cfg := h.CurrentConfig(); cfg != nil {
			optimizeMultiAgentV2 = cfg.Client.Codex.OptimizeMultiAgentV2
			if cfg.Client.Codex.EnableApplyPatch {
				applyPatchCapabilityForModel = h.SupportsApplyPatchModel
			}
		}
	}
	var toolSearchCapabilityForModel codexmodels.ToolSearchCapabilityForModelFunc
	if h != nil && responsesclaude.ClientToolSearchEnabled() {
		toolSearchCapabilityForModel = h.SupportsNativeToolSearchModel
	}
	models := h.Models()
	if h != nil && h.Cfg != nil && h.Cfg.Client.Codex.OpenAIModelsOnly {
		openAIModels := make([]map[string]any, 0, len(models))
		for _, model := range models {
			if model["owned_by"] == "openai" {
				openAIModels = append(openAIModels, model)
			}
		}
		models = openAIModels
	}
	modelRegistry := registry.GetGlobalRegistry()
	response := codexmodels.BuildResponseForClientWithToolCapabilities(models, modelRegistry.GetModelProviders, modelRegistry.GetResponsesWebSearchCapability, applyPatchCapabilityForModel, toolSearchCapabilityForModel, optimizeMultiAgentV2, version)
	if rows, ok := response["models"].([]map[string]any); ok {
		for _, row := range rows {
			id, _ := row["slug"].(string)
			info := registry.LookupModelInfo(id)
			if info == nil || !info.Discovery {
				continue
			}
			// The Codex protocol expects numeric context windows. For a
			// wholly unknown discovered model, do not claim the default
			// template's window; zero preserves the unknown sentinel.
			providers := modelRegistry.GetModelProviders(id)
			templateID := info.MetadataModelID
			if templateID == "" {
				templateID = id
			}
			if info.ContextLength <= 0 && len(providers) == 1 && providers[0] == "codex" && !registry.HasCodexClientModelTemplate(templateID) {
				row["context_window"], row["max_context_window"] = 0, 0
			}
			if info.DiscoveryServiceTiers != nil {
				applyDiscoveredCodexTiers(row, info.DiscoveryServiceTiers)
			}
		}
	}
	return response
}

func applyDiscoveredCodexTiers(row map[string]any, discovered []string) {
	tiers := make(map[string]map[string]any)
	switch existing := row["service_tiers"].(type) {
	case []any:
		for _, tier := range existing {
			if fields, ok := tier.(map[string]any); ok {
				if id, ok := fields["id"].(string); ok {
					tiers[id] = fields
				}
			}
		}
	}
	out := make([]any, 0, len(discovered))
	speed := make([]string, 0, 1)
	for _, id := range discovered {
		if id == "fast" {
			speed = append(speed, id)
			continue
		}
		if fields := tiers[id]; fields != nil {
			out = append(out, fields)
		} else {
			out = append(out, map[string]any{"id": id, "name": id, "description": ""})
		}
	}
	row["service_tiers"] = out
	row["additional_speed_tiers"] = speed
	if tier, _ := row["default_service_tier"].(string); tier != "" && !slices.Contains(discovered, tier) {
		row["default_service_tier"] = nil
	}
}

// CodexClientModelsResponse builds a Codex client model response.
func CodexClientModelsResponse(models []map[string]any) map[string]any {
	return codexmodels.BuildResponse(models, nil, false)
}

// CodexClientModelsResponseWithMultiAgentV2 builds a Codex client model response
// and advertises multi-agent v2 for synthesized models when enabled.
func CodexClientModelsResponseWithMultiAgentV2(models []map[string]any, enabled bool) map[string]any {
	return codexmodels.BuildResponse(models, nil, enabled)
}

// CodexClientModelsResponseForClient builds a Codex client model response
// tailored for a specific client version.
func CodexClientModelsResponseForClient(models []map[string]any, clientVersion string, enabled bool) map[string]any {
	return codexmodels.BuildResponseForClient(models, nil, enabled, clientVersion)
}
