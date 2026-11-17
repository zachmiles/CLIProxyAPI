package management

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelinventory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type inventoryRow struct {
	Channel      string           `json:"channel"`
	ProviderID   string           `json:"providerID"`
	ID           string           `json:"id"`
	RuleID       string           `json:"ruleID,omitempty"`
	BaseID       string           `json:"baseID,omitempty"`
	Name         string           `json:"name"`
	Kind         string           `json:"kind"`
	Family       string           `json:"family,omitempty"`
	Limits       *inventoryLimits `json:"limits,omitempty"`
	Efforts      []string         `json:"efforts,omitempty"`
	On           bool             `json:"on"`
	BlockedBy    []inventoryBlock `json:"blockedBy"`
	FollowsRules bool             `json:"followsRules"`
	OverrideOn   bool             `json:"overrideOn"`
}

type inventoryLimits struct {
	Context int64 `json:"context,omitempty"`
	Input   int64 `json:"input,omitempty"`
	Output  int64 `json:"output,omitempty"`
}

type inventoryBlock struct {
	Pattern string `json:"pattern"`
	Source  string `json:"source"`
}

// SetModelInventorySource sets the source of eligible pre-exclusion candidates.
func (h *Handler) SetModelInventorySource(src modelinventory.Source) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.modelInventorySource = src
}

// GetModelInventory reports model availability and the config write validator.
func (h *Handler) GetModelInventory(c *gin.Context) {
	snapshot := h.inventoryConfigSnapshot()
	configETag := ""
	if snapshot.SourceHash != "" {
		configETag = configV8ETag(snapshot.SourceHash)
	}
	if snapshot.Rules == nil {
		snapshot.Rules = map[string][]string{}
	}
	doc := ojson.NewDocString(`{}`)
	for _, field := range []struct {
		name  string
		value any
	}{
		{"configETag", configETag},
		{"rules", snapshot.Rules},
		{"models", inventoryRows(snapshot.Candidates, snapshot.Rules)},
	} {
		if errSet := doc.Set(field.name, field.value); errSet != nil {
			c.Data(http.StatusInternalServerError, "application/json; charset=utf-8", []byte(`{"error":"encode_failed"}`))
			return
		}
	}
	body := doc.Bytes()
	etag := configV8ETag(config.ComputeSourceHash(body))
	c.Header("ETag", etag)
	c.Header("Cache-Control", "private, no-cache, no-transform")
	if inventoryNotModified(c.Request.Header.Values("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func (h *Handler) inventoryConfigSnapshot() modelinventory.Snapshot {
	h.mu.Lock()
	source, cfg := h.modelInventorySource, h.cfg
	h.mu.Unlock()
	if source != nil {
		return source.InventorySnapshot()
	}
	if cfg != nil {
		return modelinventory.Snapshot{Rules: maps.Clone(cfg.OAuthExcludedModels)}
	}
	return modelinventory.Snapshot{}
}

func inventoryNotModified(values []string, etag string) bool {
	field := strings.TrimSpace(strings.Join(values, ","))
	if field == "*" {
		return true
	}
	for _, tag := range splitEntityTagList(field) {
		// If-None-Match uses weak comparison, even though our ETag is strong.
		if strings.TrimPrefix(strings.TrimSpace(tag), "W/") == etag {
			return true
		}
	}
	return false
}

type inventoryKey struct{ channel, id string }

type inventoryGroup struct {
	model      modelinventory.Candidate
	candidates []modelinventory.Candidate
}

func inventoryRows(candidates []modelinventory.Candidate, rules map[string][]string) []inventoryRow {
	groups := make(map[inventoryKey]*inventoryGroup)
	for _, candidate := range candidates {
		key := inventoryKey{candidate.Channel, candidate.ID}
		group := groups[key]
		if group == nil {
			group = &inventoryGroup{model: candidate}
			groups[key] = group
		}
		// Prefer variant metadata, then pick a stable representative for the row.
		if (candidate.Published && !group.model.Published) ||
			(candidate.Published == group.model.Published && ((candidate.BaseID != "" && group.model.BaseID == "") ||
				((candidate.BaseID == "") == (group.model.BaseID == "") && candidate.CredentialKey < group.model.CredentialKey))) {
			group.model = candidate
		}
		group.candidates = append(group.candidates, candidate)
	}

	rows := make([]inventoryRow, 0, len(groups))
	for _, group := range groups {
		model := group.model
		ruleID := cmp.Or(model.RuleID, model.ID)
		row := inventoryRow{
			Channel:    model.Channel,
			ProviderID: inventoryProviderID(model.Channel),
			ID:         model.ID,
			BaseID:     model.BaseID,
			Name:       cmp.Or(model.Name, model.ID),
			Kind:       cmp.Or(model.Kind, "chat"),
			Family:     model.Family,
			Efforts:    model.Efforts,
			BlockedBy:  []inventoryBlock{},
		}
		if model.Limits != (modelinventory.Limits{}) {
			row.Limits = &inventoryLimits{Context: model.Limits.Context, Input: model.Limits.Input, Output: model.Limits.Output}
		}
		if ruleID != model.ID {
			row.RuleID = ruleID
		}

		blocks := make(map[inventoryBlock]struct{})
		addMatches := func(candidate modelinventory.Candidate, patterns []string, source string) bool {
			ids := candidate.RuleIDs
			if len(ids) == 0 {
				ids = []string{cmp.Or(candidate.RuleID, candidate.ID)}
				if candidate.BaseID != "" {
					baseRuleID := candidate.BaseID
					if base := groups[inventoryKey{candidate.Channel, candidate.BaseID}]; base != nil {
						baseRuleID = cmp.Or(base.model.RuleID, base.model.ID)
					}
					ids = append(ids, baseRuleID)
				}
			}
			matched := false
			for i, pattern := range patterns {
				for _, id := range ids {
					if coreauth.ModelMatchesExclusions(id, patterns[i:i+1]) {
						matched = true
						blocks[inventoryBlock{Pattern: pattern, Source: source}] = struct{}{}
						break
					}
				}
			}
			return matched
		}
		for _, candidate := range group.candidates {
			addMatches(candidate, rules[model.Channel], "rules")
			if candidate.CredentialRules == nil {
				row.FollowsRules = true
			} else if !addMatches(candidate, candidate.CredentialRules, "credential") {
				row.OverrideOn = true
			}
			row.On = row.On || candidate.Published
		}
		for block := range blocks {
			row.BlockedBy = append(row.BlockedBy, block)
		}
		slices.SortFunc(row.BlockedBy, func(a, b inventoryBlock) int {
			return cmp.Or(cmp.Compare(b.Source, a.Source), cmp.Compare(a.Pattern, b.Pattern))
		})
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b inventoryRow) int {
		return cmp.Or(cmp.Compare(a.Channel, b.Channel), cmp.Compare(a.ID, b.ID))
	})
	return rows
}

func inventoryProviderID(channel string) string {
	switch channel {
	case "codex":
		return "openai"
	case "claude":
		return "claude-agent"
	case "zai":
		return "zai-coding-plan"
	default:
		return channel
	}
}
