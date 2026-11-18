package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	request "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestDiscoveryAddOnlyInventoryRoutingAndRestart(t *testing.T) {
	for _, tc := range []struct{ channel, catalog string }{
		{"codex", "gpt-6-chat-thinking"},
		{"xai", "grok-3-mini"},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			const sticky, extra = "fixture-discovered", "fixture-extra"
			now := setDiscoveryTestClock(t, time.Unix(1_700_000_000, 0))
			path := filepath.Join(t.TempDir(), "config.yaml")
			raw := fmt.Sprintf("config-version: 8\nmodels:\n  discovery:\n    %s: {mode: live, retire: false, extra: [%s]}\n", tc.channel, extra)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{cfg: cfg, configPath: path, coreManager: coreauth.NewManager(nil, nil, nil)}
			auth := registerDiscoveryEngineTestAuth(t, s, tc.channel)
			s.coreManager.RegisterExecutor(mockBatchTestExecutor{provider: tc.channel})
			check := func(s *Service, id string, want bool) {
				t.Helper()
				h := management.NewHandler(s.cfg, path, s.coreManager)
				h.SetModelInventorySource(s)
				router := gin.New()
				router.GET("/inventory", h.GetModelInventory)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("inventory failed: %d %s", rec.Code, rec.Body.String())
				}
				on := false
				for _, row := range ojson.ParseBytes(rec.Body.Bytes()).Get("models").Array() {
					if row.Get("channel").String() == tc.channel && row.Get("id").String() == id {
						on = row.Get("on").Bool()
					}
				}
				listed := GlobalModelRegistry().ClientSupportsModel(auth.ID, id)
				_, errExecute := s.coreManager.Execute(t.Context(), []string{tc.channel}, request.Request{Model: id}, request.Options{})
				if listed != want || (errExecute == nil) != want || on != want {
					t.Errorf("%s: listed=%v routable=%v inventoryOn=%v; want all %v", id, listed, errExecute == nil, on, want)
				}
			}
			router := gin.New()
			router.GET("/v1/models", openai.NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, s.coreManager)).OpenAIModels)
			codexRows := func() map[string]string {
				t.Helper()
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.159.0", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("Codex catalog: %d %s", rec.Code, rec.Body.String())
				}
				rows := make(map[string]string)
				for _, row := range ojson.ParseBytes(rec.Body.Bytes()).Get("models").Array() {
					rows[row.Get("slug").String()] = row.Raw
				}
				return rows
			}
			var off map[string]string
			if tc.channel == "codex" {
				offCfg := cfg.CloneForRuntime()
				offCfg.Models.Discovery = nil
				s.cfg = offCfg
				s.completeModelRegistrationForAuth(t.Context(), auth)
				off = codexRows()
				if len(off) == 0 || off[tc.catalog] == "" {
					t.Fatal("off baseline lacks the catalog model")
				}
				s.cfg = cfg
				s.completeModelRegistrationForAuth(t.Context(), auth)
			}
			check(s, sticky, false)
			list := []discoveredModel{{ID: sticky, DisplayName: "Fixture Discovered"}, {ID: extra}}
			tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
				return list, nil
			})
			tick()
			check(s, sticky, true)
			list = nil
			for range 4 {
				*now = now.Add(10 * time.Minute)
				tick()
			}
			for _, id := range []string{tc.catalog, sticky, extra} {
				check(s, id, true)
			}
			if row := s.discoveryRows(cfg, auth, tc.channel)[sticky]; row.Model.DisplayName != "Fixture Discovered" || row.Misses != 0 {
				t.Errorf("sticky discovery lost metadata or counted misses: %+v", row)
			}
			saved, errRead := os.ReadFile(path)
			if errRead != nil || string(saved) != raw {
				t.Errorf("add-only discovery edited the extra out of config: %v", errRead)
			}
			if tc.channel == "codex" {
				live := codexRows()
				for id, row := range off {
					if live[id] != row {
						t.Errorf("shared Codex row %s changed or disappeared", id)
					}
				}
				if live[sticky] == "" || live[extra] == "" {
					t.Error("Codex catalog lacks the extra or newly discovered ID")
				}
			}
			restarted := &Service{cfg: cfg, configPath: path, coreManager: s.coreManager}
			restarted.completeModelRegistrationForAuth(t.Context(), auth)
			check(restarted, sticky, true)
			restarted.applyCoreAuthRemoval(t.Context(), auth.ID)
			check(restarted, sticky, false)
			if len(loadDiscoveryCache(restarted.discoveryCachePath())) != 0 {
				t.Error("removal left the add-only account in the on-disk cache")
			}
		})
	}
}

func TestDiscoveryAddOnlyShadowLog(t *testing.T) {
	out := captureDiscoveryFileLog(t)
	cfg, err := config.ParseConfigBytes([]byte("models: {discovery: {codex: {mode: shadow, retire: false}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	s.initModelDiscovery()
	account := &discoveryAccount{}
	s.modelDiscovery.accounts[auth.ID] = account
	s.logShadowDiscoveryDiff(auth.ID, "codex", account, nil)
	if got := out.String(); !strings.Contains(got, "gpt-6-chat-thinking") || !strings.Contains(got, "(add-only)") {
		t.Errorf("shadow did not report missing models and label add-only: %s", got)
	}
}

func TestDiscoveryRetireConfigV8SubtreeRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("config-version: 8\nmodels: {discovery: {codex: {mode: live}}}\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := management.NewHandler(cfg, path, nil)
	router := gin.New()
	router.GET("/config/*path", h.ConfigV8)
	router.PUT("/config/*path", h.ConfigV8)
	const subtree = "/config/models/discovery/codex/retire"
	call := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, subtree, strings.NewReader(body)))
		return rec
	}
	for _, value := range []string{"false", "true"} {
		if rec := call(http.MethodPut, value); rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", value, rec.Code, rec.Body.String())
		}
		if rec := call(http.MethodGet, ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != value {
			t.Fatalf("GET after PUT %s: %d %s", value, rec.Code, rec.Body.String())
		}
		if _, err := config.LoadConfig(path); err != nil {
			t.Fatalf("reload after PUT %s: %v", value, err)
		}
	}
	for _, value := range []string{`"false"`, `null`, `1`, `{}`, `[]`} {
		if rec := call(http.MethodPut, value); rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("invalid PUT %s: %d %s", value, rec.Code, rec.Body.String())
		}
	}
	if rec := call(http.MethodGet, ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "true" {
		t.Fatalf("invalid writes changed retire: %d %s", rec.Code, rec.Body.String())
	}
}
