package cliproxy

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelinventory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type modelInventoryState struct {
	mu          sync.Mutex
	accounts    map[string]inventoryAccount
	complete    map[string]inventoryAccount
	completeCfg *config.Config
}

type inventoryAccount struct {
	epoch      uint64
	cfg        *config.Config
	settled    bool
	candidates []modelinventory.Candidate
}

func (s *Service) InventoryCandidates() []modelinventory.Candidate {
	return s.InventorySnapshot().Candidates
}

func (s *Service) InventorySnapshot() modelinventory.Snapshot {
	if s == nil || s.coreManager == nil {
		return modelinventory.Snapshot{}
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	d := &s.modelInventory
	d.mu.Lock()
	defer d.mu.Unlock()
	auths := s.coreManager.List()
	d.promoteComplete(s.cfg, auths)
	var snapshot modelinventory.Snapshot
	if cfg := d.completeCfg; cfg != nil {
		snapshot.SourceHash = cfg.SourceHash
		rulesByChannel := cfg.SourceOAuthExcludedModels
		if rulesByChannel == nil {
			rulesByChannel = cfg.OAuthExcludedModels
		}
		snapshot.Rules = make(map[string][]string, len(rulesByChannel))
		for channel, rules := range rulesByChannel {
			snapshot.Rules[channel] = slices.Clone(rules)
		}
	}
	for _, auth := range auths {
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		account := d.complete[auth.ID]
		if account.epoch != auth.RegistrationEpoch {
			continue
		}
		for _, candidate := range account.candidates {
			candidate.Efforts = slices.Clone(candidate.Efforts)
			candidate.CredentialRules = slices.Clone(candidate.CredentialRules)
			candidate.RuleIDs = slices.Clone(candidate.RuleIDs)
			snapshot.Candidates = append(snapshot.Candidates, candidate)
		}
	}
	snapshot.Candidates = projectClaudeInventoryAliases(snapshot.Candidates)
	return snapshot
}

// promoteComplete runs under mu. Only credentials seen by registration are
// participants; a finished registration with no candidates still counts.
func (d *modelInventoryState) promoteComplete(cfg *config.Config, auths []*coreauth.Auth) {
	complete := make(map[string]inventoryAccount, len(d.accounts))
	for _, auth := range auths {
		account, tracked := d.accounts[auth.ID]
		if !tracked || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if !account.settled || account.cfg != cfg || account.epoch != auth.RegistrationEpoch {
			return
		}
		complete[auth.ID] = account
	}
	d.complete, d.completeCfg = complete, cfg
}

// sealInventory preserves a completed generation even if no inventory request
// arrived before the next reload. The caller holds cfgMu.
func (s *Service) sealInventory() {
	if s.coreManager == nil {
		return
	}
	d := &s.modelInventory
	d.mu.Lock()
	defer d.mu.Unlock()
	d.promoteComplete(s.cfg, s.coreManager.List())
}

func (s *Service) trackInventoryRegistration(cfg *config.Config, auth *coreauth.Auth) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg != cfg {
		return
	}
	d := &s.modelInventory
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.accounts == nil {
		d.accounts = make(map[string]inventoryAccount)
	}
	if _, tracked := d.accounts[auth.ID]; !tracked {
		d.accounts[auth.ID] = inventoryAccount{}
	}
}

func (s *Service) removeInventory(id string) {
	d := &s.modelInventory
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.accounts, id)
	delete(d.complete, id)
}

func (s *Service) setInventory(id string, epoch uint64, cfg *config.Config, candidates []modelinventory.Candidate, models []*ModelInfo) {
	d := &s.modelInventory
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.accounts == nil {
		d.accounts = make(map[string]inventoryAccount)
	}
	published := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		if model != nil {
			if _, exists := published[model.ID]; !exists {
				published[model.ID] = model
			}
		}
	}
	out := make([]modelinventory.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if model := published[candidate.ID]; model != nil {
			candidate.Published = true
			// Exclusions precede aliases. If an alias's source was removed,
			// the native route underneath it can be the actual publication.
			if model.MetadataModelID == "" && candidate.BaseID == "" && candidate.RuleID != "" && candidate.RuleID != model.ID {
				candidate.RuleID = model.ID
				candidate.RuleIDs = nil
			}
			candidate.Name = model.DisplayName
		}
		out = append(out, candidate)
	}
	d.accounts[id] = inventoryAccount{epoch: epoch, cfg: cfg, settled: true, candidates: out}
}

// These are the model IDs understood by OpenCode's claude-agent provider.
func isOpenCodeClaudeAlias(id string) bool {
	switch id {
	case "opus", "fable", "sonnet", "haiku":
		return true
	}
	return false
}

var claudeInventoryUpstreamID = regexp.MustCompile(`^claude-(opus|fable|sonnet|haiku)-([0-9]+)-([0-9]+)(?:-([0-9]{8}))?$`)

// projectClaudeInventoryAliases selects across the complete snapshot, before
// consulting Published. OpenCode chooses the newest upstream even if excluded.
// Keep every credential for the winning ID so availability and rule flags
// still reflect credential overrides. This changes inventory, not routing.
func projectClaudeInventoryAliases(candidates []modelinventory.Candidate) []modelinventory.Candidate {
	type upstream struct {
		id, date     string
		major, minor int
	}
	configured := make(map[string]bool)
	newest := make(map[string]upstream)
	for _, candidate := range candidates {
		if candidate.Channel != "claude" {
			continue
		}
		id := candidate.ID
		if isOpenCodeClaudeAlias(id) {
			configured[id] = true
			continue
		}
		match := claudeInventoryUpstreamID.FindStringSubmatch(id)
		if match == nil {
			continue
		}
		family, date := match[1], match[4]
		major, errMajor := strconv.Atoi(match[2])
		minor, errMinor := strconv.Atoi(match[3])
		if errMajor != nil || errMinor != nil {
			continue
		}
		current := newest[family]
		// For matching versions, an undated ID beats any dated twin.
		dateOrder := cmp.Compare(date, current.date)
		if date == "" || current.date == "" {
			dateOrder = -dateOrder
		}
		if current.id == "" || cmp.Or(cmp.Compare(major, current.major), cmp.Compare(minor, current.minor), dateOrder, cmp.Compare(id, current.id)) > 0 {
			newest[family] = upstream{id: id, date: date, major: major, minor: minor}
		}
	}
	aliases := make(map[string]string, len(newest))
	for family, upstream := range newest {
		if !configured[family] {
			aliases[upstream.id] = family
		}
	}
	out := candidates[:0]
	for _, candidate := range candidates {
		if candidate.Channel == "claude" && !isOpenCodeClaudeAlias(candidate.ID) {
			family := aliases[candidate.ID]
			if family == "" {
				continue
			}
			candidate.RuleID, candidate.ID = candidate.ID, family
			candidate.RuleIDs = nil
		}
		out = append(out, candidate)
	}
	return out
}

func inventoryCredentialRules(auth *coreauth.Auth, inheritsChannel bool) []string {
	if rules := strings.TrimSpace(auth.Attributes["excluded_models"]); rules != "" {
		return strings.Split(rules, ",")
	}
	if !inheritsChannel {
		return []string{}
	}
	return nil
}

type inventoryModelRules struct {
	credential                               []string
	applyAliases, namedCompat, codexVariants bool
}

func inventoryCandidates(cfg *config.Config, auth *coreauth.Auth, channel string, models []*ModelInfo, rules inventoryModelRules) []modelinventory.Candidate {
	if strings.TrimSpace(channel) == "" || len(models) == 0 {
		return nil
	}
	rawIDs := make(map[string]string, len(models))
	for _, model := range models {
		if model != nil {
			rawIDs[model.ID] = model.ID
		}
	}
	if rules.applyAliases {
		if aliasChannel := coreauth.OAuthModelAliasChannel(channel, auth.AuthKind()); aliasChannel != "" {
			models = applyOAuthModelAliasEntries(oauthModelAliasesForAuth(cfg, aliasChannel, auth.Attributes), models, func(route, original string) {
				rawIDs[route] = original
			})
		}
	}
	if rules.applyAliases {
		models = applyOAuthSettingsForAuth(cfg, channel, auth.AuthKind(), models)
	}
	prefix := strings.TrimSpace(auth.Prefix)
	prefixed := make(map[string]string, len(models)*2)
	forcePrefix := cfg != nil && cfg.ForceModelPrefix
	models = applyModelPrefixesWithSource(models, prefix, forcePrefix, func(route, original string) {
		prefixed[route] = original
	})
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(auth.ID)))
	out := make([]modelinventory.Candidate, 0, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			continue
		}
		kind := inventoryModelKind(model)
		unprefixed := model.ID
		if original, ok := prefixed[model.ID]; ok {
			unprefixed = original
		}
		candidate := modelinventory.Candidate{
			Channel: channel, ID: model.ID, RuleID: rawIDs[unprefixed],
			Name: model.DisplayName, Kind: kind, Family: model.DiscoveryFamily,
			Limits:        modelinventory.Limits{Context: int64(model.ContextLength), Input: int64(model.InputTokenLimit), Output: int64(model.OutputTokenLimit)},
			CredentialKey: key, CredentialRules: rules.credential,
		}
		if candidate.Limits.Output == 0 {
			candidate.Limits.Output = int64(model.MaxCompletionTokens)
		}
		if model.Thinking != nil {
			candidate.Efforts = slices.Clone(model.Thinking.Levels)
		}
		switch {
		case rules.namedCompat:
			candidate.RuleID = modelUpstreamID(model)
			for _, id := range []string{candidate.RuleID, unprefixed, model.ID} {
				for _, matchID := range []string{id, thinking.ParseSuffix(id).ModelName} {
					if matchID != "" && !slices.Contains(candidate.RuleIDs, matchID) {
						candidate.RuleIDs = append(candidate.RuleIDs, matchID)
					}
				}
			}
		case rules.codexVariants:
			if base := codexVariantBase(unprefixed, modelUpstreamID(model)); base != "" {
				candidate.BaseID = base
				if unprefixed != model.ID {
					candidate.BaseID = prefix + "/" + base
				}
				candidate.RuleIDs = []string{candidate.RuleID, unprefixed, base}
				candidate.RuleID = unprefixed
			}
		}
		out = append(out, candidate)
	}
	return out
}
