package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestDiscoveryReviewRotationKeepsLastGoodAndStreak(t *testing.T) {
	s := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	s.cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: "live"}}
	auth := registerDiscoveryEngineTestAuth(t, s, "codex")
	list := []discoveredModel{{ID: "rotation-model"}}
	tick := discoveryTestRunner(t, s, auth, func(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) { return list, nil })
	tick()
	list = nil
	tick()
	auth.Metadata["access_token"] = "rotated-token"
	rotated, err := s.coreManager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	s.invalidateDiscoveryAccount(auth.ID)
	s.completeModelRegistrationForAuth(t.Context(), rotated)
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "rotation-model") || s.discoveryRows(s.cfg, rotated, "codex")["rotation-model"].Misses != 1 {
		t.Fatal("token rotation lost last-good membership or its miss streak")
	}
}

func TestDiscoveryReviewExtraCommitRejectsStaleCredential(t *testing.T) {
	s, auth := discoveryReviewExtraFixture(t)
	// A config writer holds the lock until the attempted removal reaches
	// the commit boundary. The account is revoked before releasing it.
	config.ConfigWriteMu.Lock()
	tick := discoveryTestRunner(t, s, auth, discoverReviewExtra)
	done := make(chan struct{})
	go func() { tick(); close(done) }()
	// Sync.Mutex waits are not durable blocks for synctest.Wait.
	stack := make([]byte, 1<<20)
	for {
		blocked := false
		for _, trace := range strings.Split(string(stack[:runtime.Stack(stack, true)]), "\n\n") {
			blocked = blocked || (strings.Contains(trace, "sync.Mutex.Lock") && strings.Contains(trace, "config.UpdateConfigFile"))
		}
		if blocked {
			break
		}
		runtime.Gosched()
	}
	s.coreManager.Remove(t.Context(), auth.ID)
	config.ConfigWriteMu.Unlock()
	<-done
	assertReviewExtra(t, s, true, "revoked discovery removed an extra at config commit")
}

func TestDiscoveryReviewDirtyCacheRetriesIdenticalListing(t *testing.T) {
	s, auth := discoveryReviewExtraFixture(t)
	if err := os.Mkdir(s.discoveryCachePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	tick := discoveryTestRunner(t, s, auth, discoverReviewExtra)
	tick()
	assertReviewExtra(t, s, true, "removed extra although last-good write failed")
	if err := os.Remove(s.discoveryCachePath()); err != nil {
		t.Fatal(err)
	}
	tick()
	assertReviewExtra(t, s, false, "identical fetch did not retry the dirty cache before removing the extra")
	if _, err := os.Stat(s.discoveryCachePath()); err != nil {
		t.Fatal(err)
	}
}

func discoveryReviewExtraFixture(t *testing.T) (*Service, *coreauth.Auth) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("config-version: 8\nmodels:\n  discovery:\n    codex: {mode: live, extra: [review-extra]}\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: cfg, configPath: path, coreManager: coreauth.NewManager(nil, nil, nil)}
	return s, registerDiscoveryEngineTestAuth(t, s, "codex")
}

func discoverReviewExtra(context.Context, *config.Config, *coreauth.Auth) ([]discoveredModel, error) {
	return []discoveredModel{{ID: "review-extra"}}, nil
}

func assertReviewExtra(t *testing.T, s *Service, present bool, message string) {
	t.Helper()
	raw, err := os.ReadFile(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "review-extra") != present {
		t.Fatal(message)
	}
}

func TestDiscoveryReviewWatcherStopsDuringRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, manager, _, auth := newIssue6718PluginService(t, "codex", nil)
		s.pluginHost = pluginhost.New()
		provider := &reviewBlockingProvider{issue6718ModelProvider: issue6718ModelProvider{provider: "codex"}, started: make(chan struct{}), release: make(chan struct{})}
		s.pluginHost.RegisterPluginForTest("codex", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			AuthProvider: provider, ModelProvider: provider, Executor: provider, ExecutorModelScope: pluginapi.ExecutorModelScopeOAuth,
		}})
		auth.Metadata = map[string]any{"access_token": "original"}
		auth, err := manager.Update(t.Context(), auth)
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.Models.Discovery = map[string]registry.ModelDiscovery{"codex": {Mode: "live"}}
		s.initModelDiscovery()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s.runCancel = cancel
		s.modelDiscovery.accounts[auth.ID] = &discoveryAccount{key: "stale", mode: "live", cancel: func() {}}
		changes := make(chan struct{}, 1)
		changes <- struct{}{}
		done := make(chan struct{})
		go s.watchDiscoveryAccount(ctx, auth.ID, s.modelDiscovery.accounts[auth.ID], changes, func() { close(done) })
		synctest.Wait()
		select {
		case <-provider.started:
		default:
			t.Fatal("test plugin did not block watcher registration")
		}
		if errShutdown := s.Shutdown(t.Context()); errShutdown != nil {
			t.Fatal(errShutdown)
		}
		synctest.Wait()
		select {
		case <-done:
		default:
			close(provider.release)
			<-done
			t.Fatal("watcher registration ignored service shutdown")
		}
	})
}

type reviewBlockingProvider struct {
	issue6718ModelProvider
	started, release chan struct{}
}

func (p *reviewBlockingProvider) ModelsForAuth(ctx context.Context, _ pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	close(p.started)
	select {
	case <-ctx.Done():
	case <-p.release:
	}
	return pluginapi.ModelResponse{}, ctx.Err()
}

type discoveryReviewTransport func(*http.Request) (*http.Response, error)

func (f discoveryReviewTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDiscoveryReviewClaudeDeadlinePerPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
		s.cfg.Models.Discovery = map[string]registry.ModelDiscovery{"claude": {Mode: "live"}}
		auth := registerDiscoveryEngineTestAuth(t, s, "claude")
		transport := discoveryReviewTransport(func(req *http.Request) (*http.Response, error) {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(6 * time.Second): // synctest virtual clock
			}
			page := 1
			if req.URL.Query().Get("after_id") != "" {
				page = 2
			}
			body := fmt.Sprintf(`{"data":[{"id":"claude-review-%d"}],"has_more":%t,"last_id":"claude-review-%d"}`, page, page == 1, page)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		var got int
		var fetchErr error
		discoveryTestRunner(t, s, auth, func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]discoveredModel, error) {
			rows, err := fetchDiscoveryClaude(context.WithValue(ctx, "cliproxy.roundtripper", transport), cfg, auth)
			got, fetchErr = len(rows), err
			return rows, err
		})()
		if got != 2 || fetchErr != nil {
			t.Fatalf("two pages under the individual deadline returned %d models: %v", got, fetchErr)
		}
	})
}

func TestDiscoveryReviewExampleDocumentsCadenceAndRetirement(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"startup and every 60 s per account", "back off on errors", "three consecutive complete misses", "errors interrupt"} {
		if !strings.Contains(string(raw), phrase) {
			t.Errorf("example does not document %q", phrase)
		}
	}
}
