package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestDiscoveryFetchListings(t *testing.T) {
	for _, tc := range []struct {
		channel string
		path    string
		query   url.Values
		headers map[string]string
		body    string
		want    []discoveredModel
	}{
		{
			channel: "codex", path: "/backend-api/codex/models",
			query: url.Values{"client_version": {"0.159.0"}},
			headers: map[string]string{
				"Originator": "codex_cli_rs", "User-Agent": "codex_cli_rs/0.159.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9",
				"ChatGPT-Account-Id": "account-1", "X-Test-Header": "custom",
			},
			body: `{"models":[{"slug":"Acme/model.1","display_name":"New Model","visibility":"list",
				"context_window":272000,"max_context_window":872000,
				"supported_reasoning_levels":[{"effort":"low","description":"Fast"},{"effort":"high"}],
				"service_tiers":[{"id":"priority","name":"Fast"}],"additional_speed_tiers":["fast"]},
				{"slug":"acme-fallback-window","context_window":128000}]}`,
			want: []discoveredModel{
				{ID: "Acme/model.1", DisplayName: "New Model", Kind: "chat", ContextTokens: 272000, MaxContextTokens: 872000, Efforts: []string{"low", "high"}, ServiceTiers: []string{"priority", "fast"}},
				{ID: "acme-fallback-window", Kind: "chat", ContextTokens: 128000},
			},
		},
		{
			channel: "opencode-go", path: "/inference/go/v1/models",
			headers: map[string]string{"X-Opencode-Org-Id": "org-1"},
			body: `{"object":"list","data":[{"id":"glm-5","name":"GLM 5","kind":"chat","family":"glm",
				"limits":{"context":202752,"input":170000,"output":32768},"capabilities":{"reasoning":true,"tool_call":true}},
				{"id":"image-gen-1","kind":"image","limits":{"output":1}}]}`,
			want: []discoveredModel{
				{ID: "glm-5", DisplayName: "GLM 5", Kind: "chat", Family: "glm", ContextTokens: 202752, InputTokens: 170000, OutputTokens: 32768},
				{ID: "image-gen-1", Kind: "image", OutputTokens: 1},
			},
		},
		{
			channel: "claude", path: "/v1/models", query: url.Values{"limit": {"100"}},
			headers: map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "oauth-2025-04-20", "X-API-Key": ""},
			body: `{"data":[{"type":"model","id":"claude-opus-4-8","display_name":"Claude Opus 4.8",
				"created_at":"2026-05-28T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000}],
				"first_id":"claude-opus-4-8","last_id":"claude-opus-4-8","has_more":false}`,
			want: []discoveredModel{{ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8", Kind: "chat",
				InputTokens: 1000000, OutputTokens: 128000, Created: 1779926400}},
		},
		{
			channel: "xai", path: "/v1/models",
			headers: map[string]string{
				"X-XAI-Token-Auth": "xai-grok-cli", "X-Grok-Client-Identifier": "grok-shell",
				"X-AuthenticateResponse": "authenticate-response",
			},
			body: `{"object":"list","data":[{"id":"grok-build-0.1","object":"model","created":1779926400,"owned_by":"xai"}]}`,
			want: []discoveredModel{{ID: "grok-build-0.1", Kind: "chat", Created: 1779926400}},
		},
		{
			channel: "zai", path: "/api/coding/paas/v4/models",
			body: `{"object":"list","data":[{"id":"glm-5-turbo","object":"model","created":1779926400,"owned_by":"z.ai"}]}`,
			want: []discoveredModel{{ID: "glm-5-turbo", Created: 1779926400}},
		},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			for _, status := range []int{http.StatusOK, http.StatusPartialContent, http.StatusUnauthorized, http.StatusForbidden} {
				t.Run(http.StatusText(status), func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						assertDiscoveryRequest(t, r, tc.path, tc.query)
						for key, want := range tc.headers {
							if got := r.Header.Get(key); got != want {
								t.Errorf("%s = %q, want %q", key, got, want)
							}
						}
						switch tc.channel {
						case "claude":
							if !strings.HasPrefix(r.UserAgent(), "claude-cli/") {
								t.Errorf("User-Agent = %q, want Claude CLI", r.UserAgent())
							}
						case "xai":
							if ver := r.Header.Get("X-Grok-Client-Version"); ver == "" || r.UserAgent() != "xai-grok-workspace/"+ver {
								t.Errorf("Grok client version = %q, User-Agent = %q", ver, r.UserAgent())
							}
						}
						w.WriteHeader(status)
						_, _ = fmt.Fprint(w, tc.body)
					}))
					defer srv.Close()
					auth := discoveryTestAuth(t, tc.channel, srv.URL, tc.path)
					if got := discoveryChannelForAuth(auth); got != tc.channel {
						t.Fatalf("discoveryChannelForAuth = %q, want %q", got, tc.channel)
					}
					got, err := modelDiscoveryFetchers[tc.channel](t.Context(), nil, auth)
					if status != http.StatusOK {
						if err == nil || errors.Is(err, errDiscoveryNotEligible) || got != nil {
							t.Fatalf("incomplete response: got (%+v, %v), want a fetch error and no models", got, err)
						}
						return
					}
					if err != nil || !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("got (%+v, %v), want (%+v, nil)", got, err, tc.want)
					}
				})
			}
		})
	}
}

func TestDiscoveryFetchClaudePagination(t *testing.T) {
	for _, tc := range []struct {
		name, secondBody string
		secondStatus     int
		missingCursor    bool
	}{
		{"complete", `{"data":[{"id":"page-2"}],"has_more":false}`, http.StatusOK, false},
		{"forbidden-page-2", `{}`, http.StatusForbidden, false},
		{"broken-page-2", `{"data":[`, http.StatusOK, false},
		{"repeated-cursor", `{"data":[{"id":"page-2"}],"has_more":true,"last_id":"cursor / 1"}`, http.StatusOK, false},
		{"missing-cursor", "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := url.Values{"limit": {"100"}}
				switch requests.Add(1) {
				case 1:
					assertDiscoveryRequest(t, r, "/v1/models", query)
					if tc.missingCursor {
						_, _ = fmt.Fprint(w, `{"data":[{"id":"page-1"}],"has_more":true,"last_id":null}`)
						return
					}
					_, _ = fmt.Fprint(w, `{"data":[{"id":"page-1"}],"has_more":true,"last_id":"cursor / 1"}`)
				case 2:
					query.Set("after_id", "cursor / 1")
					assertDiscoveryRequest(t, r, "/v1/models", query)
					w.WriteHeader(tc.secondStatus)
					_, _ = fmt.Fprint(w, tc.secondBody)
				default:
					t.Error("unexpected additional page")
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer srv.Close()
			auth := discoveryTestAuth(t, "claude", srv.URL, "/v1/models")
			got, err := modelDiscoveryFetchers["claude"](t.Context(), nil, auth)
			if tc.name == "complete" {
				want := []discoveredModel{{ID: "page-1", Kind: "chat"}, {ID: "page-2", Kind: "chat"}}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("got (%+v, %v), want (%+v, nil)", got, err, want)
				}
			} else if err == nil || got != nil {
				t.Fatalf("partial or malformed page: got (%+v, %v), want error and no models", got, err)
			}
			wantCalls := int32(2)
			if tc.missingCursor {
				wantCalls = 1
			}
			if n := requests.Load(); n != wantCalls {
				t.Errorf("requests = %d, want %d", n, wantCalls)
			}
		})
	}
}

func TestDiscoveryFetchRejectsMalformedListings(t *testing.T) {
	for _, tc := range []struct {
		channel, body string
	}{
		{"codex", `{"models":[{"slug":"one"}]`},
		{"codex", `{"models":[{"slug":"one"},{"slug":2}]}`},
		{"opencode-go", `{"object":"list"}`},
		{"claude", `{"data":[],"has_more":"true"}`},
		{"xai", `{"object":"list","data":null}`},
		{"zai", `{"object":"list","data":[{"id":"one"}]} []`},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			auth := discoveryTestAuth(t, tc.channel, srv.URL, "/models")
			if got, err := modelDiscoveryFetchers[tc.channel](t.Context(), nil, auth); err == nil || got != nil {
				t.Fatalf("got (%+v, %v), want error and no models", got, err)
			}
		})
	}
}

func TestDiscoveryFetchIneligible(t *testing.T) {
	for _, tc := range []struct {
		channel, provider string
		attrs             map[string]string
		meta              map[string]any
	}{
		{channel: "codex", provider: "codex", attrs: map[string]string{"api_key": "key"}, meta: map[string]any{"access_token": "token"}},
		{channel: "codex", provider: "codex", meta: map[string]any{"refresh_token": "refresh-only"}},
		{channel: "claude", provider: "claude", attrs: map[string]string{"auth_kind": "apikey"}, meta: map[string]any{"access_token": "token"}},
		{channel: "xai", provider: "xai", attrs: map[string]string{"api_key": "key"}, meta: map[string]any{"auth_kind": "oauth", "access_token": "token"}},
		{channel: "xai", provider: "xai", attrs: map[string]string{"using_api": "true"}, meta: map[string]any{"auth_kind": "oauth", "access_token": "token"}},
		{channel: "xai", provider: "xai", meta: map[string]any{"using_api": true, "auth_kind": "oauth", "access_token": "token"}},
		{channel: "opencode-go", provider: "opencode-go", meta: map[string]any{"access_token": "token"}},
		{channel: "opencode-go", provider: "opencode-go", attrs: map[string]string{"api_key": "key"}},
		{channel: "zai", provider: "openai-compatibility", attrs: map[string]string{"compat_name": "ZAI", "base_url": "https://api.z.ai/api/coding/paas/v4"}},
		{channel: "zai", provider: "openai-compatibility", attrs: map[string]string{"compat_name": "zai", "base_url": "https://api.z.ai/api/paas/v4", "api_key": "key"}},
		{channel: "zai", provider: "zai", attrs: map[string]string{"api_key": "key", "base_url": "https://api.z.ai/api/coding/paas/v4"}},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			auth := &coreauth.Auth{Provider: tc.provider, Attributes: tc.attrs, Metadata: tc.meta}
			ctx, cancel := context.WithCancel(t.Context())
			cancel() // Prevent accidental contact with an upstream if eligibility regresses.
			if got, err := modelDiscoveryFetchers[tc.channel](ctx, nil, auth); !errors.Is(err, errDiscoveryNotEligible) || got != nil {
				t.Fatalf("got (%+v, %v), want errDiscoveryNotEligible and no models", got, err)
			}
		})
	}
}

func TestDiscoveryFetchProxyAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, authProxy, configProxy string
		cancel                       bool
	}{
		{name: "auth-proxy", authProxy: "test-proxy", configProxy: "http://127.0.0.1:1"},
		{name: "config-proxy", configProxy: "test-proxy"},
		{name: "cancel", authProxy: "test-proxy", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertDiscoveryRequest(t, r, "/api/coding/paas/v4/models", nil)
				if r.URL.Host != "listing.invalid" {
					t.Errorf("proxy target = %q, want listing.invalid", r.URL.Host)
				}
				if tc.cancel {
					cancel()
					<-r.Context().Done()
					return
				}
				_, _ = fmt.Fprint(w, `{"data":[]}`)
			}))
			defer srv.Close()
			auth := discoveryTestAuth(t, "zai", "http://listing.invalid", "/api/coding/paas/v4/models")
			auth.ProxyURL = strings.ReplaceAll(tc.authProxy, "test-proxy", srv.URL)
			cfg := &config.Config{}
			cfg.ProxyURL = strings.ReplaceAll(tc.configProxy, "test-proxy", srv.URL)
			got, err := modelDiscoveryFetchers["zai"](ctx, cfg, auth)
			if tc.cancel {
				if !errors.Is(err, context.Canceled) || got != nil {
					t.Fatalf("got (%+v, %v), want cancellation and no models", got, err)
				}
			} else if err != nil || len(got) != 0 {
				t.Fatalf("got (%+v, %v), want a complete empty listing", got, err)
			}
		})
	}
}

func assertDiscoveryRequest(t *testing.T, r *http.Request, path string, query url.Values) {
	t.Helper()
	if r.Method != http.MethodGet || r.URL.Path != path {
		t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, path)
	}
	if got := r.URL.Query(); len(got) != 0 || len(query) != 0 {
		if !reflect.DeepEqual(got, query) {
			t.Errorf("query = %v, want %v", got, query)
		}
	}
	if got := r.Header.Get("Authorization"); got != "Bearer discovery-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func discoveryTestAuth(t *testing.T, channel, base, path string) *coreauth.Auth {
	t.Helper()
	auth := &coreauth.Auth{Provider: channel, Metadata: map[string]any{"type": channel, "access_token": "discovery-token"}}
	switch channel {
	case "codex":
		auth.Metadata["account_id"] = "account-1"
		auth.Attributes = map[string]string{"header:X-Test-Header": "custom"}
	case "opencode-go":
		auth.Provider = "openai-compatibility"
		auth.Attributes = map[string]string{"compat_name": "OpenCode-Go"}
		auth.Metadata["base_url"] = base + "/inference/go/openai/v1"
		auth.Metadata["headers"] = map[string]any{"X-Opencode-Org-Id": "org-1", "X-Opencode-Session": "$CPA-SESSION-ID"}
		return auth
	case "xai":
		auth.Metadata["auth_kind"] = "oauth"
		auth.Metadata["base_url"] = "https://api.x.ai/v1"
	case "zai":
		auth.Provider = "compat-zai"
		auth.Attributes = map[string]string{"compat_name": "ZaI", "base_url": base + strings.TrimSuffix(path, "/models") + "/", "api_key": "discovery-token"}
		auth.Metadata = nil
		return auth
	}
	original := modelDiscoveryEndpoints[channel]
	modelDiscoveryEndpoints[channel] = base + path
	t.Cleanup(func() { modelDiscoveryEndpoints[channel] = original })
	return auth
}
