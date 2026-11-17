package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	request "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func TestDiscoveryInventoryRetirementRespectsListingKinds(t *testing.T) {
	for _, tc := range []struct {
		channel  string
		chat     string
		media    map[string]string
		excluded string
		unlisted string
	}{
		{"codex", "gpt-6-chat-thinking", map[string]string{"gpt-image-2": "image", "gpt-image-2.6": "image", "gpt-image-1.5": "image"}, "gpt-image-2.6", "gpt-image-1.5"},
		{"xai", "grok-3-mini", map[string]string{
			"grok-imagine-image": "image", "grok-imagine-image-2.0": "image", "grok-imagine-image-quality": "image",
			"grok-imagine-video": "video", "grok-imagine-video-1.5": "video", "grok-imagine-video-1.5-lite": "video", "grok-imagine-video-1.5-preview": "video",
			"grok-tts": "speech", "grok-voice-tts-1.0": "speech",
		}, "grok-imagine-video-1.5-lite", "grok-imagine-video"},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			now := setDiscoveryTestClock(t, time.Unix(1_700_000_000, 0))
			cfg := &config.Config{OAuthExcludedModels: map[string][]string{tc.channel: {tc.excluded}}}
			cfg.Models.Discovery = map[string]registry.ModelDiscovery{tc.channel: {Mode: "live"}}
			s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
			auth := registerDiscoveryEngineTestAuth(t, s, tc.channel)
			s.coreManager.RegisterExecutor(mockBatchTestExecutor{provider: tc.channel})
			list := []discoveredModel{{ID: tc.chat}}
			// Even if an upstream sends a media ID once, its omission is not a miss.
			for id, kind := range tc.media {
				if id != tc.unlisted {
					list = append(list, discoveredModel{ID: id, Kind: kind})
				}
			}
			tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) { return list, nil })
			tick()
			list = nil
			*now = now.Add(10 * time.Minute)
			tick()
			for id := range tc.media {
				if row := s.discoveryRows(cfg, auth, tc.channel)[id]; row.Misses != 0 {
					t.Errorf("out-of-scope %s counted a miss: %d", id, row.Misses)
				}
			}
			for range 3 {
				*now = now.Add(10 * time.Minute)
				tick()
			}
			if GlobalModelRegistry().ClientSupportsModel(auth.ID, tc.chat) {
				t.Errorf("missing chat model %s did not retire", tc.chat)
			}
			h := management.NewHandler(cfg, "", s.coreManager)
			h.SetModelInventorySource(s)
			router := gin.New()
			router.GET("/inventory", h.GetModelInventory)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
			rows := map[string]ojson.Result{}
			for _, row := range ojson.ParseBytes(rec.Body.Bytes()).Get("models").Array() {
				rows[row.Get("id").String()] = row
			}
			for id, kind := range tc.media {
				wantOn := id != tc.excluded
				listed := GlobalModelRegistry().ClientSupportsModel(auth.ID, id)
				_, errExecute := s.coreManager.Execute(t.Context(), []string{tc.channel}, request.Request{Model: id}, request.Options{})
				if row := rows[id]; row.Get("kind").String() != kind || row.Get("on").Bool() != wantOn || wantOn != (listed && errExecute == nil) {
					t.Errorf("%s: want kind=%s on=%v; inventory=%s listed=%v routing error=%v", id, kind, wantOn, row.Raw, listed, errExecute)
				}
			}
		})
	}
}

func TestDiscoveryRetiresMediaWhenListingCoversIt(t *testing.T) {
	old := modelDiscoveryListedKinds["codex"]
	t.Cleanup(func() { modelDiscoveryListedKinds["codex"] = old })
	modelDiscoveryListedKinds["codex"] = []string{"chat", "image"}
	now := setDiscoveryTestClock(t, time.Unix(1_700_000_000, 0))
	cfg := &config.Config{}
	cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: "live"}}
	s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	const id = "gpt-image-2"
	list := []discoveredModel{{ID: id, Kind: "image"}}
	tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) { return list, nil })
	tick()
	list = nil
	for miss := 1; miss <= 3; miss++ {
		*now = now.Add(10 * time.Minute)
		tick()
		if got := GlobalModelRegistry().ClientSupportsModel(auth.ID, id); got != (miss < 3) {
			t.Fatalf("listed=%v after %d complete misses of a covered image model", got, miss)
		}
	}
}

func TestDiscoveryShadowFileLogIncludesBoundedDiff(t *testing.T) {
	out := captureDiscoveryFileLog(t)
	s := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	s.initModelDiscovery()
	account := &discoveryAccount{}
	s.modelDiscovery.accounts[auth.ID] = account
	var served []*ModelInfo
	var listed []discoveredModel
	for i := range 21 {
		served = append(served, &ModelInfo{ID: fmt.Sprintf("missing-%02d", i)})
		listed = append(listed, discoveredModel{ID: fmt.Sprintf("added-%02d", i)})
	}
	served = append(served, &ModelInfo{ID: "gpt-image-2", Type: registry.OpenAIImageModelType})
	GlobalModelRegistry().RegisterClient(auth.ID, "codex", served)
	out.Reset()
	s.logShadowDiscoveryDiff(auth.ID, "codex", account, listed)
	s.logShadowDiscoveryDiff(auth.ID, "codex", account, listed)
	line := strings.TrimSpace(out.String())
	t.Log(line)
	for _, want := range []string{"channel=codex", "discovered=21", "served=22", "added=21", "missing=21", "added-19", "missing-19"} {
		if !strings.Contains(line, want) {
			t.Errorf("shadow log missing %q: %s", want, line)
		}
	}
	if strings.Count(line, "…+1 more") != 2 || strings.Contains(line, "-20") || strings.Contains(line, "gpt-image-2") || strings.Contains(line, "\n") {
		t.Errorf("shadow log was not capped, scoped, or deduplicated: %s", line)
	}
}

func TestDiscoveryFileLogBackoffIsSafeAndThrottled(t *testing.T) {
	out := captureDiscoveryFileLog(t)
	now := setDiscoveryTestClock(t, time.Unix(1_700_000_000, 0))
	cfg := &config.Config{}
	cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: "live"}}
	s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	var fetchErr error
	tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) { return nil, fetchErr })
	tick()
	out.Reset()
	fetchErr = errors.New("unsafe fixture token discovery-test-token account " + auth.ID)
	tick()
	tick()
	fetchErr = context.DeadlineExceeded
	tick()
	tick()
	*now = now.Add(30 * time.Minute)
	tick()
	got := out.String()
	t.Log(strings.TrimSpace(got))
	if strings.Count(got, "[warn ") != 3 || strings.Count(got, "channel=codex") != 3 ||
		!strings.Contains(got, "timed out") || strings.Contains(got, "discovery-test-token") || strings.Contains(got, auth.ID) {
		t.Fatalf("unexpected backoff logging: %s", got)
	}
}

func captureDiscoveryFileLog(t *testing.T) *strings.Builder {
	t.Helper()
	logger := log.StandardLogger()
	oldOutput, oldFormatter := logger.Out, logger.Formatter
	oldReportCaller := logger.ReportCaller
	out := new(strings.Builder)
	logger.SetOutput(out)
	logger.SetFormatter(&logging.LogFormatter{})
	logger.SetReportCaller(true)
	t.Cleanup(func() {
		logger.SetOutput(oldOutput)
		logger.SetFormatter(oldFormatter)
		logger.SetReportCaller(oldReportCaller)
	})
	return out
}
