// Package opencode signs in to an OpenCode Console workspace and serves its Go
// subscription through the OpenAI-compatible executor.
//
// Console speaks RFC 8628 device authorization. Approving the code binds the
// login to one workspace; its bearer token works for inference when the
// workspace is named in X-Opencode-Org-Id. Refresh tokens rotate.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	// Provider matches the openai-compatibility group that lists the Go models.
	Provider = "opencode-go"
	// BaseURL is Go's OpenAI Chat Completions inference endpoint.
	BaseURL = "https://opencode.ai/inference/go/openai/v1"
	// Console accepts any client id.
	clientID    = "cli-proxy-api"
	httpTimeout = 30 * time.Second
)

// ConsoleURL is the OpenCode Console root; tests replace it.
var ConsoleURL = "https://opencode.ai/console"

// DeviceCode is a pending device authorization.
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// Tokens is a Console token grant.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	OrgID        string `json:"org_id"`
	Error        string `json:"error"`
	Interval     int    `json:"interval"`
}

// Client talks to the Console auth endpoints.
type Client struct{ http *http.Client }

// NewClient uses the configured outbound proxy, if any.
func NewClient(cfg *config.Config) *Client {
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
	}
	return &Client{http: util.SetProxy(&sdkCfg, &http.Client{Timeout: httpTimeout})}
}

// StartDeviceFlow requests a user code. Console answers with relative
// verification URLs; they are returned absolute, preferring the one that
// already carries the code.
func (c *Client) StartDeviceFlow(ctx context.Context) (*DeviceCode, error) {
	var code DeviceCode
	status, err := c.post(ctx, "/auth/device/code", url.Values{"client_id": {clientID}, "supports_org_scope": {"true"}}, &code)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || code.DeviceCode == "" || code.UserCode == "" || code.VerificationURI == "" {
		return nil, fmt.Errorf("opencode console: device login could not start (HTTP %d)", status)
	}
	if code.VerificationURIComplete != "" {
		code.VerificationURI = code.VerificationURIComplete
	}
	base, _ := url.Parse(ConsoleURL)
	verification, errParse := base.Parse(code.VerificationURI)
	if errParse != nil {
		return nil, fmt.Errorf("opencode console: invalid verification URL: %w", errParse)
	}
	code.VerificationURI = verification.String()
	return &code, nil
}

// PollForTokens waits until the user approves, denies, or the code expires.
func (c *Client) PollForTokens(ctx context.Context, code *DeviceCode) (*Tokens, error) {
	interval := time.Duration(max(code.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(code.ExpiresIn, 60)) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		var tokens Tokens
		status, err := c.post(ctx, "/auth/device/token", url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {clientID},
			"device_code": {code.DeviceCode},
		}, &tokens)
		if err != nil {
			return nil, err
		}
		switch {
		case tokens.AccessToken != "":
			return &tokens, nil
		case tokens.Error == "authorization_pending":
		case tokens.Error == "slow_down":
			interval = max(interval+5*time.Second, time.Duration(tokens.Interval)*time.Second)
		case tokens.Error == "expired_token":
			return nil, fmt.Errorf("opencode console: the code expired before it was approved")
		case tokens.Error == "access_denied" || tokens.Error == "authorization_denied":
			return nil, fmt.Errorf("opencode console: the login was denied")
		default:
			return nil, fmt.Errorf("opencode console: device login failed (HTTP %d %s)", status, tokens.Error)
		}
	}
	return nil, fmt.Errorf("opencode console: the code expired before it was approved")
}

// Refresh exchanges a refresh token for new, rotated tokens.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	var tokens Tokens
	status, err := c.post(ctx, "/auth/device/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}, &tokens)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || tokens.AccessToken == "" {
		return nil, fmt.Errorf("opencode console: token refresh failed (HTTP %d %s)", status, tokens.Error)
	}
	return &tokens, nil
}

func (c *Client) post(ctx context.Context, path string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ConsoleURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("opencode console: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_ = json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode, nil
}

// ApplyTokens writes a grant into credential metadata, keeping the workspace
// and refresh token when a refresh omits them. The org and session headers are
// sent with every inference request.
func ApplyTokens(metadata map[string]any, tokens *Tokens, now time.Time) map[string]any {
	if metadata == nil {
		metadata = map[string]any{}
	}
	orgID := tokens.OrgID
	if orgID == "" {
		orgID, _ = metadata["org_id"].(string)
	}
	refreshToken := tokens.RefreshToken
	if refreshToken == "" {
		refreshToken, _ = metadata["refresh_token"].(string)
	}
	expiresIn := tokens.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	metadata["type"] = Provider
	metadata["access_token"] = tokens.AccessToken
	metadata["refresh_token"] = refreshToken
	metadata["expired"] = now.Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	metadata["last_refresh"] = now.Format(time.RFC3339)
	metadata["org_id"] = orgID
	metadata["base_url"] = BaseURL
	metadata["headers"] = map[string]any{"X-Opencode-Org-Id": orgID, "X-Opencode-Session": "$CPA-SESSION-ID"}
	return metadata
}

// IsConsoleAuth reports whether an auth is an OpenCode Console sign-in.
func IsConsoleAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || !strings.EqualFold(auth.Provider, Provider) {
		return false
	}
	token, _ := auth.Metadata["refresh_token"].(string)
	return token != ""
}

// RefreshAuth returns a copy of auth with refreshed tokens.
func RefreshAuth(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	refreshToken, _ := auth.Metadata["refresh_token"].(string)
	tokens, err := NewClient(cfg).Refresh(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	refreshed := auth.Clone()
	refreshed.Metadata = ApplyTokens(refreshed.Metadata, tokens, time.Now())
	return refreshed, nil
}
