package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Failure modes: an unopenable relative verification URL, giving up while the
// grant is pending, losing the workspace (inference needs it), and dropping
// the rotated refresh token or workspace on refresh.
func TestDeviceLoginAndRefresh(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		reply := map[string]any{}
		switch r.URL.Path + " " + r.Form.Get("grant_type") {
		case "/console/auth/device/code ":
			reply = map[string]any{"device_code": "dev", "user_code": "ABCD-EFGH", "verification_uri": "/console/device",
				"verification_uri_complete": "/console/device?user_code=ABCD-EFGH", "expires_in": 60, "interval": 1}
		case "/console/auth/device/token urn:ietf:params:oauth:grant-type:device_code":
			if polls++; polls == 1 {
				reply = map[string]any{"error": "authorization_pending"}
			} else {
				reply = map[string]any{"access_token": "at1", "refresh_token": "rt1", "expires_in": 3600, "org_id": "org"}
			}
		case "/console/auth/device/token refresh_token":
			if r.Form.Get("refresh_token") != "rt1" {
				w.WriteHeader(http.StatusBadRequest)
			}
			reply = map[string]any{"access_token": "at2", "refresh_token": "rt2", "expires_in": 3600}
		default:
			t.Errorf("unexpected request %s %v", r.URL.Path, r.Form)
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer server.Close()
	ConsoleURL = server.URL + "/console"

	client := NewClient(nil)
	code, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code.VerificationURI != server.URL+"/console/device?user_code=ABCD-EFGH" {
		t.Fatalf("verification URL = %q", code.VerificationURI)
	}
	tokens, err := client.PollForTokens(context.Background(), code)
	if err != nil || polls != 2 {
		t.Fatalf("poll = %v after %d polls", err, polls)
	}

	auth := &cliproxyauth.Auth{Provider: Provider, Metadata: ApplyTokens(nil, tokens, time.Now())}
	if !IsConsoleAuth(auth) {
		t.Fatal("console login not recognized")
	}
	refreshed, err := RefreshAuth(context.Background(), nil, auth)
	if err != nil {
		t.Fatal(err)
	}
	meta := refreshed.Metadata
	if meta["access_token"] != "at2" || meta["refresh_token"] != "rt2" || meta["org_id"] != "org" || meta["base_url"] != BaseURL {
		t.Fatalf("refreshed metadata = %v", meta)
	}
	if headers := meta["headers"].(map[string]any); headers["X-Opencode-Org-Id"] != "org" {
		t.Fatalf("org header = %v", headers)
	}
	if auth.Metadata["access_token"] != "at1" {
		t.Fatal("refresh mutated the original credential")
	}
}
