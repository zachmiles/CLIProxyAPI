package cliproxy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const (
	goExcludedUpstream = "go-preview-v1"
	goAllowedUpstream  = "go-stable-v1"
)

func openCodeExclusionsFixture(t *testing.T) (*config.Config, *coreauth.Auth) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		AuthDir: dir,
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "opencode-go", BaseURL: "https://example.invalid/go/v1",
			Models: []config.OpenAICompatibilityModel{
				{Name: goExcludedUpstream},
				{Name: goExcludedUpstream, Alias: "go-config-alias"},
				{Name: goAllowedUpstream},
			},
		}},
		OAuthExcludedModels: map[string][]string{"opencode-go": {" Go-*VIEW-V1 "}},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"opencode-go": {{Name: goExcludedUpstream, Alias: "go-oauth-alias", Fork: true}},
		},
	}
	sctx := &synthesizer.SynthesisContext{Config: cfg, AuthDir: dir}
	auths, err := synthesizer.SynthesizeAuthFile(sctx, filepath.Join(dir, "console.json"), []byte(`{"type":"opencode-go","access_token":"fixture-token"}`))
	if err != nil || len(auths) != 1 {
		t.Fatalf("synthesize OpenCode Go: count=%d err=%v", len(auths), err)
	}
	auths[0].ID = t.Name()
	return cfg, auths[0]
}

func registerCompatExclusionsFixture(t *testing.T, cfg *config.Config, auth *coreauth.Auth, cached bool) *coreauth.Manager {
	t.Helper()
	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	manager.RegisterExecutor(mockBatchTestExecutor{provider: auth.Provider})
	service := &Service{cfg: cfg, coreManager: manager}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	current, err := manager.Register(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	var cache *openAICompatibilityRegistrationCache
	if cached {
		cache = newOpenAICompatibilityRegistrationCacheForConfig(cfg)
		if _, ok := cache.lookup(current, cfg.OpenAICompatibility[0].Name); !ok {
			t.Fatal("missing compatibility cache entry")
		}
	}
	if !service.registerModelsForAuthWithConfigAndCache(ctx, cfg, current, cache) {
		t.Fatal("model registration was superseded")
	}
	manager.RefreshSchedulerEntry(current.ID)
	return manager
}

func TestOpenCodeGoModelExclusionsListing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cached bool
	}{{"normal", false}, {"cached", true}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, auth := openCodeExclusionsFixture(t)
			registerCompatExclusionsFixture(t, cfg, auth, tc.cached)
			reg := GlobalModelRegistry()
			if !reg.ClientSupportsModel(auth.ID, goAllowedUpstream) {
				t.Fatalf("allowed OpenCode Go model %q missing from registry", goAllowedUpstream)
			}
			for _, model := range []string{goExcludedUpstream, "go-config-alias"} {
				if reg.ClientSupportsModel(auth.ID, model) {
					t.Errorf("excluded OpenCode Go model %q still listed", model)
				}
			}
		})
	}
}

func TestZAICodingPlanModelExclusionsListing(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "ZAI", BaseURL: "https://example.invalid/coding/paas/v4",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "fixture-key"}},
			Models: []config.OpenAICompatibilityModel{
				{Name: "glm-fixture-preview", Alias: "coding-preview"},
				{Name: "glm-fixture-stable", Alias: "coding-stable"},
			},
		}},
		OAuthExcludedModels: map[string][]string{"zai": {"GLM-*-PREVIEW"}},
	}
	auths, err := (&synthesizer.ConfigSynthesizer{}).Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil || len(auths) != 1 {
		t.Fatalf("synthesize Z.AI: count=%d err=%v", len(auths), err)
	}
	auth := auths[0]
	auth.ID = t.Name()
	if auth.AuthKind() != coreauth.AuthKindAPIKey || auth.Provider != util.OpenAICompatibleProviderKey("zai") {
		t.Fatalf("unexpected Z.AI credential kind=%q provider=%q", auth.AuthKind(), auth.Provider)
	}
	manager := registerCompatExclusionsFixture(t, cfg, auth, true)
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(auth.ID, "coding-stable") {
		t.Fatal("allowed Z.AI Coding Plan model missing from registry")
	}
	if reg.ClientSupportsModel(auth.ID, "coding-preview") {
		t.Error("excluded Z.AI Coding Plan model \"coding-preview\" still listed")
	}
	// Even if the old registry entry is still visible during reload, a
	// configured compatibility alias must not execute an excluded upstream.
	reg.RegisterClient(auth.ID, auth.Provider, []*ModelInfo{{ID: "coding-preview"}})
	manager.RefreshSchedulerEntry(auth.ID)
	_, err = manager.Execute(context.Background(), []string{auth.Provider}, cliproxyexecutor.Request{Model: "coding-preview"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Error("excluded Z.AI Coding Plan route \"coding-preview\" reached executor")
	}
}

func TestOpenCodeGoModelExclusionsExecutionOverride(t *testing.T) {
	cfg, auth := openCodeExclusionsFixture(t)
	manager := registerCompatExclusionsFixture(t, cfg, auth, false)
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.AuthSelectionModelMetadataKey: goAllowedUpstream,
	}}
	_, err := manager.Execute(context.Background(), []string{auth.Provider}, cliproxyexecutor.Request{Model: goExcludedUpstream}, opts)
	if err == nil {
		t.Errorf("excluded OpenCode Go execution override %q reached executor", goExcludedUpstream)
	}
}

func TestOpenCodeGoModelExclusionsCredentialReplacement(t *testing.T) {
	cfg, auth := openCodeExclusionsFixture(t)
	auth.Attributes["excluded_models"] = "GO-STABLE*"
	manager := registerCompatExclusionsFixture(t, cfg, auth, true)
	for _, tc := range []struct {
		model   string
		allowed bool
	}{{goExcludedUpstream, true}, {goAllowedUpstream, false}} {
		if listed := GlobalModelRegistry().ClientSupportsModel(auth.ID, tc.model); listed != tc.allowed {
			t.Errorf("listed %q = %v, want %v", tc.model, listed, tc.allowed)
		}
		_, err := manager.Execute(context.Background(), []string{auth.Provider}, cliproxyexecutor.Request{Model: tc.model}, cliproxyexecutor.Options{})
		if (err == nil) != tc.allowed {
			t.Errorf("request for %q: err=%v, want allowed=%v", tc.model, err, tc.allowed)
		}
	}
}

func TestOpenCodeGoModelExclusionsRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		model       string
		forcePrefix bool
		stale       bool
	}{
		{"direct", goExcludedUpstream, false, false},
		{"oauth-alias", "go-oauth-alias", false, false},
		{"prefixed", "workspace/" + goExcludedUpstream, true, false},
		{"stale-registry-oauth-alias", "workspace/go-oauth-alias(high)", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, auth := openCodeExclusionsFixture(t)
			cfg.ForceModelPrefix = tc.forcePrefix
			auth.Prefix = "workspace"
			registrationConfig := cfg
			if tc.stale {
				beforeReload := *cfg
				beforeReload.OAuthExcludedModels = nil
				registrationConfig = &beforeReload
			}
			manager := registerCompatExclusionsFixture(t, registrationConfig, auth, false)
			if tc.stale {
				manager.SetConfig(cfg)
			}
			allowed := goAllowedUpstream
			if tc.forcePrefix {
				allowed = "workspace/" + allowed
			}
			execute := func(model string) error {
				_, err := manager.Execute(context.Background(), []string{auth.Provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return err
			}
			if err := execute(allowed); err != nil {
				t.Fatalf("allowed route %q failed: %v", allowed, err)
			}
			if err := execute(tc.model); err == nil {
				t.Errorf("excluded OpenCode Go route %q reached executor", tc.model)
			}
		})
	}
}
