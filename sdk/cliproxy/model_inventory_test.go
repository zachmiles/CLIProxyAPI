package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	request "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestModelInventoryAgreesWithListingAndRouting(t *testing.T) {
	const (
		base    = "gpt-6-astra"
		variant = base + "-fast"
		opus    = "claude-opus-4-7"
	)
	for _, mode := range []string{"off", "shadow", "live"} {
		for _, tc := range []struct {
			name     string
			rules    map[string][]string
			override bool
		}{
			{"exact-upstream", map[string][]string{"codex": {base}, "claude": {opus}, "opencode-go": {"go-upstream"}, "zai": {"glm-upstream"}}, false},
			{"exact-route", map[string][]string{"codex": {variant}, "claude": {"opus"}, "opencode-go": {"go-alias"}, "zai": {"workspace/glm-alias"}}, false},
			{"wildcard", map[string][]string{"codex": {"gpt-6-as*"}, "claude": {"Claude-opus-*"}, "opencode-go": {"go-*"}, "zai": {"workspace/glm-*"}}, false},
			{"credential-override", map[string][]string{"codex": {base}, "claude": {opus}, "opencode-go": {"go-*"}, "zai": {"glm-*"}, "gemini": {"gemini-inventory-allow"}}, true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				cfg := &config.Config{
					OAuthExcludedModels: tc.rules,
					OAuthModelAlias: map[string][]config.OAuthModelAlias{
						"codex":  {{Name: base, Alias: variant, Fork: true, ForceMapping: true}},
						"claude": {{Name: opus, Alias: "opus"}},
					},
					GeminiKey: []config.GeminiKey{{APIKey: "fixture-gemini", Models: []config.GeminiModel{
						{Name: "gemini-allow-upstream", Alias: "gemini-inventory-allow"},
						{Name: "gemini-block-upstream", Alias: "gemini-inventory-block"},
					}, ExcludedModels: []string{"gemini-inventory-block"}}},
					OpenAICompatibility: []config.OpenAICompatibility{
						{Name: "opencode-go", BaseURL: "https://opencode.ai/inference/go/v1", Models: []config.OpenAICompatibilityModel{{Name: "go-upstream", Alias: "go-alias"}}},
						{Name: "zai", BaseURL: "https://api.z.ai/api/coding/paas/v4", Models: []config.OpenAICompatibilityModel{{Name: "glm-upstream", Alias: "glm-alias"}}},
					},
				}
				cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: mode}}
				s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
				s.coreManager.SetConfig(cfg)
				s.coreManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
				auths := map[string][]*coreauth.Auth{}
				for _, channel := range []string{"codex", "claude", "opencode-go", "zai", "gemini"} {
					a := &coreauth.Auth{
						ID: t.Name() + "/" + channel, Provider: channel, Status: coreauth.StatusActive,
						Attributes: map[string]string{"auth_kind": "oauth"}, Metadata: map[string]any{"access_token": "fixture-token"},
					}
					switch channel {
					case "opencode-go", "zai":
						a.Provider = util.OpenAICompatibleProviderKey(channel)
						a.Attributes["compat_name"] = channel
						a.Attributes["provider_key"] = a.Provider
						if channel == "zai" {
							a.Attributes["auth_kind"] = "apikey"
							a.Attributes["api_key"] = "fixture-zai"
							a.Attributes["base_url"] = cfg.OpenAICompatibility[1].BaseURL
							a.Prefix = "workspace"
						}
					case "gemini":
						a.Attributes["auth_kind"] = "apikey"
						a.Attributes["api_key"] = "fixture-gemini"
						a.Attributes["excluded_models"] = "gemini-inventory-allow" // config entry still wins
					}
					if tc.override && channel == "opencode-go" {
						a.Attributes["excluded_models"] = "other-*"
					}
					current, errRegister := s.coreManager.Register(t.Context(), a)
					if errRegister != nil {
						t.Fatal(errRegister)
					}
					t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(a.ID) })
					s.coreManager.RegisterExecutor(mockBatchTestExecutor{provider: a.Provider})
					s.completeModelRegistrationForAuth(t.Context(), current)
					auths[channel] = append(auths[channel], current)
					if mode == "live" && channel == "codex" {
						discoveryTestRunner(t, s, current, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
							return []discoveredModel{{ID: base}, {ID: "gpt-inventory-discovered"}}, nil
						})()
					}
				}
				handler := management.NewHandler(cfg, "", s.coreManager)
				handler.SetModelInventorySource(s)
				router := gin.New()
				const path = "/v8/management/models/inventory"
				router.GET(path, handler.GetModelInventory)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusOK {
					t.Fatalf("inventory HTTP %d: %s", response.Code, response.Body.String())
				}
				seen := make(map[string]bool)
				rowsOn := make(map[string]bool)
				for _, row := range ojson.ParseBytes(response.Body.Bytes()).Get("models").Array() {
					channel, id := row.Get("channel").String(), row.Get("id").String()
					seen[channel+"/"+id] = true
					routes := false
					for _, a := range auths[channel] {
						if !GlobalModelRegistry().ClientSupportsModel(a.ID, id) {
							continue
						}
						opts := request.Options{Metadata: map[string]any{request.PinnedAuthMetadataKey: a.ID}}
						_, errExecute := s.coreManager.Execute(t.Context(), []string{a.Provider}, request.Request{Model: id}, opts)
						routes = routes || errExecute == nil
					}
					on := row.Get("on").Bool()
					rowsOn[channel+"/"+id] = on
					if on != routes {
						t.Errorf("%s/%s: inventory on=%v, listed and routable=%v; row=%s", channel, id, on, routes, row.Raw)
					}
				}
				for _, key := range []string{"codex/" + base, "codex/" + variant, "claude/opus", "opencode-go/go-alias", "zai/workspace/glm-alias", "gemini/gemini-inventory-allow", "gemini/gemini-inventory-block"} {
					if !seen[key] {
						t.Errorf("missing inventory row %s", key)
					}
				}
				if got, want := rowsOn["codex/"+variant], mode != "live" && tc.name == "exact-route"; got != want {
					t.Errorf("Codex variant on=%v, want %v", got, want)
				}
				for channel, creds := range auths {
					for _, auth := range creds {
						for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
							if channel == "claude" && !isOpenCodeClaudeAlias(model.ID) {
								continue // Raw Claude routes aren't part of OpenCode's contract.
							}
							if !seen[channel+"/"+model.ID] {
								t.Errorf("listed %s/%s missing from inventory", channel, model.ID)
							}
						}
					}
				}
			})
		}
	}
}

func TestInventoryReviewUsesPublishedConfigAndAliasOrder(t *testing.T) {
	const a, b = "gpt-6-astra", "gpt-6-sol"
	raw := []byte("config-version: 8\noauth:\n  excluded-models: {codex: [gpt-6-astra]}\n  model-alias:\n    codex: [{name: gpt-6-astra, alias: gpt-6-sol, fork: true}]\n")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: cfg, configPath: path, coreManager: coreauth.NewManager(nil, nil, nil)}
	s.coreManager.SetConfig(cfg)
	s.coreManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	h := management.NewHandler(cfg, path, s.coreManager)
	h.SetModelInventorySource(s)
	router := gin.New()
	router.GET("/inventory", h.GetModelInventory)
	router.PUT("/config/*path", h.ConfigV8)
	check := func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
		doc := ojson.ParseBytes(rec.Body.Bytes())
		if got := doc.Get("configETag").String(); got != `"`+cfg.SourceHash+`"` || doc.Get("rules.codex.0").String() != a {
			t.Errorf("inventory used unpublished rules or ETag: %s / %s", got, doc.Get("rules").Raw)
		}
		for _, row := range doc.Get("models").Array() {
			if row.Get("id").String() == b {
				if !row.Get("on").Bool() || !GlobalModelRegistry().ClientSupportsModel(auth.ID, b) || row.Get("ruleID").String() == a {
					t.Errorf("native B did not survive excluded alias source A: %s", row.Raw)
				}
				return
			}
		}
		t.Error("native B is missing from inventory")
	}
	check()
	if err := os.WriteFile(path, []byte("config-version: 8\noauth: {excluded-models: {codex: [gpt-6-sol]}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check()
	req := httptest.NewRequest(http.MethodPut, "/config/oauth/excluded-models/codex", nil)
	req.Header.Set("If-Match", `"`+cfg.SourceHash+`"`)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale inventory ETag PUT = %d", rec.Code)
	}
}

func TestInventoryReviewClaudeAliasJSON(t *testing.T) {
	const upstream = "claude-opus-4-7"
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"claude": {{Name: upstream, Alias: "opus"}, {Name: "claude-opus-4-6", Alias: "custom-opus"}},
	}}
	s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	registerDiscoveryEngineTestAuth(t, s, "claude")
	h := management.NewHandler(cfg, "", s.coreManager)
	h.SetModelInventorySource(s)
	router := gin.New()
	router.GET("/inventory", h.GetModelInventory)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
	rows := ojson.ParseBytes(rec.Body.Bytes()).Get("models").Array()
	if len(rows) != 1 || rows[0].Get("id").String() != "opus" || rows[0].Get("ruleID").String() != upstream || rows[0].Get("providerID").String() != "claude-agent" {
		t.Fatalf("Claude inventory did not expose only the OpenCode alias: got %d rows", len(rows))
	}
	t.Log(rows[0].Raw)
}

func TestInventoryReloadWaitsForCompleteGeneration(t *testing.T) {
	for _, finish := range []string{"publish", "empty", "remove", "disable"} {
		t.Run(finish, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			load := func(first, second, rule string) *config.Config {
				t.Helper()
				raw := "config-version: 8\n" +
					"gemini-api-key:\n" +
					"  - api-key: first\n    models: [{name: " + first + "}]\n" +
					"  - api-key: second\n" + second +
					"\noauth: {excluded-models: {gemini: [" + rule + "]}}\n"
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				return cfg
			}
			old := load("old-first", "    models: [{name: old-second}]\n", "old-rule")
			s := &Service{cfg: old, configPath: path, coreManager: coreauth.NewManager(nil, nil, nil)}
			register := func(id, provider string) *coreauth.Auth {
				t.Helper()
				a, err := s.coreManager.Register(t.Context(), &coreauth.Auth{
					ID: t.Name() + "/" + id, Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{"api_key": id},
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(a.ID) })
				return a
			}
			first := register("first", "gemini")
			second := register("second", "gemini")
			s.completeModelRegistrationForAuth(t.Context(), first)
			s.completeModelRegistrationForAuth(t.Context(), second)
			// An enabled credential that bypasses registration must not
			// become a participant just because it exists in the manager.
			register("unprocessed", "external-provider")
			h := management.NewHandler(old, path, s.coreManager)
			h.SetModelInventorySource(s)
			router := gin.New()
			router.GET("/inventory", h.GetModelInventory)
			router.PUT("/config/*path", h.ConfigV8)
			check := func(cfg *config.Config, rule string, ids ...string) string {
				t.Helper()
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
				doc := ojson.ParseBytes(rec.Body.Bytes())
				var gotIDs []string
				for _, row := range doc.Get("models").Array() {
					gotIDs = append(gotIDs, row.Get("id").String())
				}
				etag := doc.Get("configETag").String()
				if rec.Code != http.StatusOK || etag != `"`+cfg.SourceHash+`"` ||
					doc.Get("rules.gemini.0").String() != rule || strings.Join(ids, ",") != strings.Join(gotIDs, ",") {
					t.Fatalf("inventory = status %d rows %v rules %s configETag %s; want rows %v rule %q hash %s",
						rec.Code, gotIDs, doc.Get("rules").Raw, etag, ids, rule, cfg.SourceHash)
				}
				return etag
			}
			check(old, "old-rule", "old-first", "old-second")
			next := load("new-first", "    models: [{name: new-second}]\n", "new-rule")
			commit := s.commitConfigUpdate(next)
			s.coreManager.SetConfig(commit.cfg)
			s.completeModelRegistrationForAuth(t.Context(), first)
			etag := check(old, "old-rule", "old-first", "old-second")
			req := httptest.NewRequest(http.MethodPut, "/config/oauth/excluded-models", strings.NewReader(`{}`))
			req.Header.Set("If-Match", etag)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusPreconditionFailed {
				t.Fatalf("PUT during reload = %d; want 412", rec.Code)
			}

			switch finish {
			case "publish":
				s.completeModelRegistrationForAuth(t.Context(), second)
				check(next, "new-rule", "new-first", "new-second")
			case "empty":
				second.Provider = "no-model-provider"
				current, err := s.coreManager.Update(t.Context(), second)
				if err != nil {
					t.Fatal(err)
				}
				s.completeModelRegistrationForAuth(t.Context(), current)
				check(next, "new-rule", "new-first")
			case "remove":
				s.applyCoreAuthRemoval(t.Context(), second.ID)
				check(next, "new-rule", "new-first")
			case "disable":
				second.Disabled = true
				if _, err := s.coreManager.Update(t.Context(), second); err != nil {
					t.Fatal(err)
				}
				check(next, "new-rule", "new-first")
			}
		})
	}
}
