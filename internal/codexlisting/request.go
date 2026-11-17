// Package codexlisting builds requests for the authenticated Codex model catalog.
package codexlisting

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	Endpoint      = "https://chatgpt.com/backend-api/codex/models"
	ClientVersion = "0.159.0"
	originator    = "codex_cli_rs"
	userAgent     = originator + "/" + ClientVersion + " (Mac OS 26.3.1; arm64) iTerm.app/3.6.9"
)

// URL adds the requested client version to the listing endpoint.
func URL(endpoint, clientVersion string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if clientVersion = strings.TrimSpace(clientVersion); clientVersion != "" {
		q := u.Query()
		q.Set("client_version", clientVersion)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// NewRequest prepares a Codex listing request without refreshing credentials.
func NewRequest(ctx context.Context, endpoint, clientVersion, token, accountID string, attrs map[string]string) (*http.Request, error) {
	target, err := URL(endpoint, clientVersion)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Close = true
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Originator", originator)
	req.Header.Set("User-Agent", userAgent)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return req, nil
}
