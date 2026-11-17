package cliproxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	request "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestDiscoveryRetiresOnlyAfterThreeConsecutiveCompleteMisses(t *testing.T) {
	var account discoveryAccount
	now := time.Unix(1_700_000_000, 0)
	check := func(want bool) {
		t.Helper()
		_, have := account.rows["new"]
		if have != want {
			t.Fatalf("present = %v, want %v", have, want)
		}
	}
	account.record([]discoveredModel{{ID: "new"}}, nil, now)
	now = now.Add(30 * time.Minute)
	account.record(nil, nil, now)
	account.record(nil, nil, now)
	account.record([]discoveredModel{{ID: "partial"}}, errors.New("incomplete"), now)
	check(true)
	if _, ok := account.rows["partial"]; ok {
		t.Fatal("incomplete listing changed membership")
	}
	account.record(nil, nil, now)
	account.record(nil, nil, now)
	check(true)
	account.record(nil, nil, now)
	check(false)
	if account.rows == nil {
		t.Fatal("complete empty listing treated as unseen")
	}
}

func TestDiscoveryRetirementFloor(t *testing.T) {
	const id = "retirement-floor-model"
	type step struct {
		at     time.Duration
		seen   bool
		served bool
		misses int
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{
			name: "quick misses",
			steps: []step{
				{0, true, true, 0},
				{time.Minute, false, true, 1},
				{2 * time.Minute, false, true, 2},
				{30*time.Minute - time.Second, false, true, 3},
			},
		},
		{
			name: "exact floor",
			steps: []step{
				{0, true, true, 0},
				{time.Minute, false, true, 1},
				{2 * time.Minute, false, true, 2},
				{3 * time.Minute, false, true, 3},
				{30 * time.Minute, false, false, 0},
			},
		},
		{
			name: "sighting resets streak and age",
			steps: []step{
				{0, true, true, 0},
				{time.Minute, false, true, 1},
				{2 * time.Minute, false, true, 2},
				{10 * time.Minute, true, true, 0},
				{30 * time.Minute, false, true, 1},
				{31 * time.Minute, false, true, 2},
				{32 * time.Minute, false, true, 3},
				{40 * time.Minute, false, false, 0},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1_700_000_000, 0)
			now := setDiscoveryTestClock(t, start)
			cfg := &config.Config{}
			cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: "live"}}
			s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
			auth := registerDiscoveryEngineTestAuth(t, s, "codex")
			var list []discoveredModel
			tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
				return list, nil
			})
			for _, step := range tc.steps {
				*now = start.Add(step.at)
				list = nil
				if step.seen {
					list = []discoveredModel{{ID: id}}
				}
				tick()
				if got := GlobalModelRegistry().ClientSupportsModel(auth.ID, id); got != step.served {
					t.Fatalf("at %v: served = %v, want %v", step.at, got, step.served)
				}
				if step.served {
					if got := s.discoveryRows(cfg, auth, "codex")[id].Misses; got != step.misses {
						t.Fatalf("at %v: misses = %d, want %d", step.at, got, step.misses)
					}
				}
			}
		})
	}
}

func TestDiscoveryRetirementFloorSurvivesRestart(t *testing.T) {
	const id = "cached-retirement-floor-model"
	start := time.Unix(1_700_000_000, 0)
	now := setDiscoveryTestClock(t, start)
	cfg := &config.Config{}
	cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: "live"}}
	s := &Service{cfg: cfg, configPath: filepath.Join(t.TempDir(), "config.yaml"), coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	list := []discoveredModel{{ID: id}}
	tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
		return list, nil
	})
	tick()
	list = nil
	for minute := 1; minute <= 3; minute++ {
		*now = start.Add(time.Duration(minute) * time.Minute)
		tick()
	}

	*now = start.Add(20 * time.Minute)
	restarted := &Service{cfg: cfg, configPath: s.configPath, coreManager: s.coreManager}
	restarted.completeModelRegistrationForAuth(t.Context(), auth)
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, id) {
		t.Fatal("restart did not restore the model with three misses before the floor")
	}
	tick = discoveryTestRunner(t, restarted, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
		return nil, nil
	})
	tick()
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, id) {
		t.Fatal("first miss after restart retired the model before the floor")
	}
	*now = start.Add(30 * time.Minute)
	tick()
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, id) {
		t.Fatal("restart reset the last-seen time instead of restoring it")
	}
}

func setDiscoveryTestClock(t *testing.T, now time.Time) *time.Time {
	t.Helper()
	oldNow := modelDiscoveryNow
	modelDiscoveryNow = func() time.Time { return now }
	t.Cleanup(func() { modelDiscoveryNow = oldNow })
	return &now
}

func TestDiscoveryExtraCodexParityAndConfigV8Precondition(t *testing.T) {
	now := setDiscoveryTestClock(t, time.Unix(1_700_000_000, 0))
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("# preserved\nconfig-version: 8\nmodels:\n  discovery:\n    codex:\n      mode: live # still live\n      extra: [gpt-6.2-sol, retain-me]\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.OAuthExcludedModels = map[string][]string{"codex": {"blocked-*"}}
	s := &Service{cfg: cfg, configPath: path, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	s.coreManager.RegisterExecutor(discoveryTestExecutor{})
	testFetch := []discoveredModel{{ID: "gpt-test-discovered", DisplayName: "Provider display", ContextTokens: 112233}, {ID: "blocked-discovery"}}
	tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
		return testFetch, nil
	})
	tick()
	for _, on := range []bool{false, true} {
		apiCfg := cfg.SDKConfig
		apiCfg.Client.Codex.OpenAIModelsOnly = on
		router := gin.New()
		router.GET("/v1/models", openai.NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&apiCfg, s.coreManager)).OpenAIModels)
		registry.GetGlobalRegistry().RegisterClient("discovery-parity-other", "claude", []*ModelInfo{{ID: "discovery-other", OwnedBy: "anthropic"}})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.159.0", nil))
		registry.GetGlobalRegistry().UnregisterClient("discovery-parity-other")
		listed := make(map[string]ojson.Result)
		for _, row := range ojson.Get(recorder.Body.String(), "models").Array() {
			listed[row.Get("slug").String()] = row
		}
		if recorder.Code != 200 || listed["gpt-6.2-sol"].Get("display_name").String() != "gpt-6.2-sol" ||
			listed["gpt-test-discovered"].Get("context_window").Int() != 112233 ||
			listed["blocked-discovery"].Exists() || listed["discovery-other"].Exists() == on {
			t.Fatalf("Codex list parity failed; status=%d, OpenAI-only=%v", recorder.Code, on)
		}
	}
	for _, id := range []string{"gpt-6.2-sol", "gpt-test-discovered", "blocked-discovery"} {
		resp, errExecute := s.coreManager.Execute(t.Context(), []string{"codex"}, request.Request{Model: id}, request.Options{})
		if id == "blocked-discovery" {
			if errExecute == nil {
				t.Fatal("excluded model executed")
			}
		} else if errExecute != nil || string(resp.Payload) != id {
			t.Fatalf("normal routing of %s: %q, %v", id, resp.Payload, errExecute)
		}
	}

	router := gin.New()
	handler := management.NewHandler(cfg, path, s.coreManager)
	router.GET("/config/*path", handler.ConfigV8)
	router.PUT("/config/*path", handler.ConfigV8)
	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/config/models", nil))
	etag := get.Header().Get("ETag")
	if get.Code != 200 || etag == "" {
		t.Fatalf("config GET status=%d etag=%q", get.Code, etag)
	}
	testFetch = []discoveredModel{{ID: "gpt-6.2-sol"}}
	tick()
	next, errRead := os.ReadFile(path)
	if errRead != nil || bytes.Contains(next, []byte("gpt-6.2-sol")) ||
		!bytes.Contains(next, []byte("# preserved")) || !bytes.Contains(next, []byte("# still live")) ||
		!bytes.Contains(next, []byte("retain-me")) {
		t.Fatalf("extra edit: %s (%v)", next, errRead)
	}
	req := httptest.NewRequest(http.MethodPut, "/config/observability/logs/debug", strings.NewReader("true"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", etag)
	put := httptest.NewRecorder()
	router.ServeHTTP(put, req)
	if put.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale PUT: got %d, want 412", put.Code)
	}
	s.cfg, err = config.ParseConfigBytes(next)
	if err != nil {
		t.Fatal(err)
	}
	testFetch = nil
	for i := 1; i <= 3; i++ {
		*now = now.Add(10 * time.Minute)
		tick()
		if got := GlobalModelRegistry().ClientSupportsModel(auth.ID, "gpt-6.2-sol"); got != (i < 3) {
			t.Fatalf("extra after miss %d is present=%v", i, got)
		}
	}
}

func TestDiscoveryConfigWriteConflictDoesNotClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte("config-version: 8\nmodels:\n  discovery:\n    codex: {mode: live, extra: [test-extra]}\n")
	if errWrite := os.WriteFile(path, initial, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	want := append(bytes.Clone(initial), []byte("# concurrent edit\n")...)
	errUpdate := config.UpdateConfigFile(path, func(raw []byte) ([]byte, error) {
		if errWrite := os.WriteFile(path, want, 0o600); errWrite != nil {
			return nil, errWrite
		}
		return removeDiscoveryExtrasFromYAML(raw, "codex", map[string]bool{"test-extra": true})
	})
	got, errRead := os.ReadFile(path)
	if !errors.Is(errUpdate, config.ErrConfigWriteConflict) || errRead != nil || !bytes.Equal(got, want) {
		t.Fatalf("conflict clobbered config: %s (%v, %v)", got, errUpdate, errRead)
	}
}

func TestDiscoveryModesSchedulingAndRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Unix(1000, 0)
		oldNow := modelDiscoveryNow
		modelDiscoveryNow = func() time.Time { return now }
		defer func() { modelDiscoveryNow = oldNow }()
		s := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
		auth := registerDiscoveryEngineTestAuth(t, s, "codex")
		slots := make(chan struct{}, 1)
		oldFetch := modelDiscoveryFetchers["codex"]
		defer func() { modelDiscoveryFetchers["codex"] = oldFetch }()
		calls := 0
		canceled := make(chan struct{})
		modelDiscoveryFetchers["codex"] = func(ctx context.Context, _ *config.Config, _ *coreauth.Auth) ([]discoveredModel, error) {
			calls++
			if calls == 2 {
				<-ctx.Done()
				close(canceled)
				return nil, ctx.Err()
			}
			return []discoveredModel{{ID: "shadow-only"}}, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		setMode := func(mode string) {
			s.cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: mode, Extra: []string{"shadow-extra"}}}
			s.completeModelRegistrationForAuth(ctx, auth)
			s.scheduleModelDiscovery(ctx, slots)
			synctest.Wait()
		}
		setMode("off")
		if calls != 0 || GlobalModelRegistry().ClientSupportsModel(auth.ID, "shadow-extra") {
			t.Fatal("off fetched or served extras")
		}
		setMode("shadow")
		if calls != 1 || !GlobalModelRegistry().ClientSupportsModel(auth.ID, "shadow-extra") ||
			GlobalModelRegistry().ClientSupportsModel(auth.ID, "shadow-only") {
			t.Fatal("shadow did not keep established membership plus extras")
		}
		now = now.Add(59 * time.Second)
		s.scheduleModelDiscovery(ctx, slots)
		synctest.Wait()
		if calls != 1 {
			t.Fatal("probe ran before 60 seconds")
		}
		now = now.Add(time.Second)
		s.scheduleModelDiscovery(ctx, slots)
		synctest.Wait()
		if calls != 2 {
			t.Fatal("60-second probe was not started")
		}
		s.applyCoreAuthRemoval(ctx, auth.ID)
		<-canceled
		synctest.Wait()
		if len(s.modelDiscovery.cache) != 0 || len(s.modelDiscovery.accounts) != 0 || len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 0 {
			t.Fatal("revoked account retained state or registry routes")
		}
	})
}

func TestDiscoveryRestartLoadsLastGood(t *testing.T) {
	now := setDiscoveryTestClock(t, time.Unix(1_700_000_000, 0))
	cfg := &config.Config{}
	cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: "live"}}
	s := &Service{cfg: cfg, configPath: filepath.Join(t.TempDir(), "config.yaml"), coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	list := []discoveredModel{{ID: "cached-discovery-model"}}
	tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) { return list, nil })
	tick()
	list = nil
	tick()
	tick()
	stat, errStat := os.Stat(s.discoveryCachePath())
	raw, errRead := os.ReadFile(s.discoveryCachePath())
	if errStat != nil || errRead != nil || stat.Mode().Perm() != 0o600 || bytes.Contains(raw, []byte(auth.ID)) || bytes.Contains(raw, []byte("discovery-test-token")) {
		t.Fatal("last-good cache is missing, not private, or contains credentials")
	}
	restarted := &Service{cfg: cfg, configPath: s.configPath, coreManager: s.coreManager}
	restarted.completeModelRegistrationForAuth(t.Context(), auth)
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-discovery-model") {
		t.Fatal("restart lost last-good before fetch")
	}
	*now = now.Add(30 * time.Minute)
	discoveryTestRunner(t, restarted, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) { return nil, nil })()
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "cached-discovery-model") {
		t.Fatal("restart lost miss streak")
	}
}

func TestDiscoveryCodexVariantExclusionsAndCredentialOverride(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {{Name: "gpt-discovery-base", Alias: "gpt-discovery-base-fast", Fork: true, ForceMapping: true}},
		},
	}
	cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: "live"}}
	s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	s.coreManager.RegisterExecutor(discoveryTestExecutor{})
	s.coreManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
		return []discoveredModel{{ID: "gpt-discovery-base"}, {ID: "blocked-one"}, {ID: "override-one"}}, nil
	})
	check := func(id string, want bool) {
		t.Helper()
		if got := GlobalModelRegistry().ClientSupportsModel(auth.ID, id); got != want {
			t.Fatalf("listing %s = %v, want %v", id, got, want)
		}
		if _, errExecute := s.coreManager.Execute(t.Context(), []string{"codex"}, request.Request{Model: id}, request.Options{}); (errExecute == nil) != want {
			t.Fatalf("execution %s: %v, want allowed=%v", id, errExecute, want)
		}
	}
	cfg.OAuthExcludedModels = map[string][]string{"codex": {"gpt-discovery-base-fast", "blocked-*"}}
	s.coreManager.SetConfig(cfg)
	tick()
	check("gpt-discovery-base", true)
	check("gpt-discovery-base-fast", false)
	check("blocked-one", false)
	found := false
	for _, candidate := range s.InventoryCandidates() {
		if candidate.ID == "gpt-discovery-base-fast" {
			found = candidate.BaseID == "gpt-discovery-base" && candidate.RuleID == "gpt-discovery-base-fast"
		}
	}
	if !found {
		t.Fatal("inventory lost excluded variant or its base")
	}
	cfg.OAuthExcludedModels = map[string][]string{"codex": {"gpt-discovery-base", "blocked-*"}}
	s.coreManager.SetConfig(cfg)
	s.completeModelRegistrationForAuth(t.Context(), auth)
	check("gpt-discovery-base", false)
	check("gpt-discovery-base-fast", false)

	auth.Attributes["excluded_models"] = "override-*"
	updated, errUpdate := s.coreManager.Update(t.Context(), auth)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	s.completeModelRegistrationForAuth(t.Context(), updated)
	check("gpt-discovery-base", true)
	check("gpt-discovery-base-fast", true)
	check("blocked-one", true)
	check("override-one", false)
}

func TestDiscoveryClaudeGatesExistingAliases(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"claude": {{Name: "claude-opus-4-7", Alias: "opus"}, {Name: "claude-fable-test", Alias: "fable"}},
		},
	}
	cfg.Models.Discovery = map[string]registry.ModelDiscovery{"claude": {Mode: "live", Extra: []string{"claude-fable-test"}}}
	s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "claude")
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "fable") {
		t.Fatal("configured Claude extra alias did not serve before discovery")
	}
	discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
		return []discoveredModel{{ID: "claude-opus-4-7"}, {ID: "claude-fable-test"}, {ID: "claude-new-raw-id"}}, nil
	})()
	cfg.Models.Discovery["claude"] = registry.ModelDiscovery{Mode: "live"}
	s.completeModelRegistrationForAuth(t.Context(), auth)
	got := make(map[string]bool)
	for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
		got[model.ID] = true
	}
	if len(got) != 2 || !got["opus"] || !got["fable"] {
		t.Fatalf("Claude discovery changed alias scheme: %v", got)
	}
}

func TestDiscoveryCompatMergesProviderFieldsAndRetiresConfiguredModels(t *testing.T) {
	now := setDiscoveryTestClock(t, time.Unix(1_700_000_000, 0))
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "zai", BaseURL: "https://api.z.ai/api/coding/paas/v4",
		Models: []config.OpenAICompatibilityModel{
			{Name: "test-glm-retired", Alias: "glm-alias"},
			{Name: "test-glm", Alias: "glm-alias", MaxContextLength: 1234},
		},
	}}}
	cfg.Models.Discovery = map[string]registry.ModelDiscovery{"zai": {Mode: "live", Extra: []string{"zai-extra"}}}
	s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	s.coreManager.SetConfig(cfg)
	auth, err := s.coreManager.Register(t.Context(), &coreauth.Auth{
		ID: "discovery-credential-zai", Provider: util.OpenAICompatibleProviderKey("zai"),
		Attributes: map[string]string{"compat_name": "zai", "provider_key": util.OpenAICompatibleProviderKey("zai"), "api_key": "test-key", "base_url": cfg.OpenAICompatibility[0].BaseURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	registerDiscoveryEngineTestAuth(t, s, "gemini")
	var rows = []discoveredModel{{ID: "test-glm", DisplayName: "Upstream GLM", ContextTokens: 2345, InputTokens: 2000, Efforts: []string{"high"}}, {ID: "zai-unconfigured"}}
	tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) { return rows, nil })
	tick()
	s.coreManager.RegisterExecutor(discoveryCompatTestExecutor{mockBatchTestExecutor{provider: util.OpenAICompatibleProviderKey("zai")}})
	for range 2 {
		response, errExecute := s.coreManager.Execute(t.Context(), []string{util.OpenAICompatibleProviderKey("zai")}, request.Request{Model: "glm-alias"}, request.Options{})
		if errExecute != nil || string(response.Payload) != "test-glm" {
			t.Fatalf("live alias pool executed %q; want only discovered test-glm; err=%v", response.Payload, errExecute)
		}
	}
	foundGemini, foundAlias := false, false
	for _, row := range s.InventoryCandidates() {
		foundGemini = foundGemini || row.Channel == "gemini"
		if row.ID == "glm-alias" {
			foundAlias = row.Channel == "zai" && row.RuleID == "test-glm" && row.Name == "glm-alias" &&
				row.Limits.Context == 2345 && row.Limits.Input == 2000 && row.Limits.Output == 0 && len(row.Efforts) == 1 && row.Efforts[0] == "high"
		}
	}
	if !foundGemini || !foundAlias {
		t.Fatalf("inventory failed metadata/route merge or omitted off channel: gemini=%v alias=%v", foundGemini, foundAlias)
	}
	rows = nil
	for range 3 {
		*now = now.Add(10 * time.Minute)
		tick()
	}
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "glm-alias") ||
		!GlobalModelRegistry().ClientSupportsModel(auth.ID, "zai-extra") ||
		GlobalModelRegistry().ClientSupportsModel(auth.ID, "zai-unconfigured") {
		t.Fatal("compat retirement kept a missed configured/discovered model or dropped the extra")
	}
}

func registerDiscoveryEngineTestAuth(t *testing.T, s *Service, channel string) *coreauth.Auth {
	t.Helper()
	auth, err := s.coreManager.Register(t.Context(), &coreauth.Auth{
		ID: "discovery-credential-" + channel, Provider: channel, Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"access_token": "discovery-test-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	s.completeModelRegistrationForAuth(t.Context(), auth)
	return auth
}

func discoveryTestRunner(t *testing.T, s *Service, auth *coreauth.Auth, fetch discoveryFetchFunc) func() {
	t.Helper()
	s.initModelDiscovery()
	channel := discoveryChannelForAuth(auth)
	oldFetch := modelDiscoveryFetchers[channel]
	t.Cleanup(func() { modelDiscoveryFetchers[channel] = oldFetch })
	modelDiscoveryFetchers[channel] = fetch
	key := discoveryKey(s.cfg, auth, channel)
	account := &discoveryAccount{key: key, fence: discoveryCredentialFence(auth), mode: "live", rows: s.discoveryRows(s.cfg, auth, channel), cancel: func() {}}
	s.modelDiscovery.accounts[auth.ID] = account
	return func() { s.fetchDiscoveryAccount(t.Context(), s.cfg, auth, channel, account) }
}

type discoveryTestExecutor struct{ mockBatchTestExecutor }

type discoveryCompatTestExecutor struct{ mockBatchTestExecutor }

func (discoveryCompatTestExecutor) Execute(_ context.Context, _ *coreauth.Auth, req request.Request, _ request.Options) (request.Response, error) {
	return request.Response{Payload: []byte(req.Model)}, nil
}

func (discoveryTestExecutor) Identifier() string { return "codex" }

func (discoveryTestExecutor) Execute(_ context.Context, _ *coreauth.Auth, req request.Request, _ request.Options) (request.Response, error) {
	return request.Response{Payload: []byte(req.Model)}, nil
}
