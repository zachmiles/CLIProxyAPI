package opencode

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Failure modes: Go receiving the router's prefixed affinity key instead of the
// client's session, overriding the client's own OpenCode session, inventing a
// header when there is no session, and touching other providers' requests.
func TestApplySessionHeader(t *testing.T) {
	console := &cliproxyauth.Auth{Provider: Provider, Metadata: map[string]any{"refresh_token": "rt"}}
	other := &cliproxyauth.Auth{Provider: "openai-compatible-zai"}
	cases := []struct {
		name    string
		auth    *cliproxyauth.Auth
		current string
		client  http.Header
		want    string
	}{
		{"client session wins", console, "header:ses_child", http.Header{"X-Opencode-Session": {"ses_parent"}}, "ses_parent"},
		{"affinity prefix stripped", console, "header:ses_abc", nil, "ses_abc"},
		{"chained prefix stripped", console, "derived:claude:sess-1", http.Header{}, "sess-1"},
		{"bare id kept", console, "ses_abc", nil, "ses_abc"},
		{"no session stays absent", console, "", nil, ""},
		{"other provider untouched", other, "header:ses_abc", http.Header{"X-Opencode-Session": {"ses_parent"}}, "header:ses_abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/inference/go/openai/v1/chat/completions", nil)
			if tc.current != "" {
				req.Header.Set(SessionHeader, tc.current)
			}
			ApplySessionHeader(req, tc.auth, tc.client)
			if got := req.Header.Get(SessionHeader); got != tc.want {
				t.Fatalf("%s = %q, want %q", SessionHeader, got, tc.want)
			}
		})
	}
}
