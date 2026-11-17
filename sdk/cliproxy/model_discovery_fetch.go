package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	opencodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/opencode"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexlisting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// discoveredModel is one upstream listing row, normalized. Zero/empty means unknown.
type discoveredModel struct {
	ID            string
	DisplayName   string
	Kind          string
	Family        string
	ContextTokens int64
	InputTokens   int64
	OutputTokens  int64
	Efforts       []string
	ServiceTiers  []string
	Created       int64
}

var errDiscoveryNotEligible = errors.New("model discovery: auth not eligible")

// discoveryFetchFunc returns models only when the entire upstream listing was read.
type discoveryFetchFunc func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]discoveredModel, error)

var modelDiscoveryFetchers = map[string]discoveryFetchFunc{
	"codex":       fetchDiscoveryCodex,
	"opencode-go": fetchDiscoveryOpenCode,
	"claude":      fetchDiscoveryClaude,
	"xai":         fetchDiscoveryXAI,
	"zai":         fetchDiscoveryZAI,
}

var modelDiscoveryEndpoints = map[string]string{
	"codex":       codexlisting.Endpoint,
	"opencode-go": "https://opencode.ai/inference/go/v1/models",
	"claude":      "https://api.anthropic.com/v1/models",
	"xai":         xaiauth.CLIChatProxyBaseURL + "/models",
	// Z.ai uses the Coding Plan group's configured base_url, with no default.
}

func discoveryChannelForAuth(auth *coreauth.Auth) string {
	if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	group := strings.ToLower(strings.TrimSpace(auth.Attributes["compat_name"]))
	key := strings.TrimSpace(auth.Attributes["api_key"])
	base := strings.TrimSpace(auth.Attributes["base_url"])
	if group == "zai" {
		if key != "" && base != "" && !discoveryZAIPayAsYouGo(base) {
			return "zai"
		}
		return ""
	}
	kind := strings.TrimSpace(auth.Attributes["auth_kind"])
	if kind == "" && provider == "claude" {
		kind = strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "auth_kind"))
	} else if kind == "" {
		kind = discoveryMetadataString(auth, "auth_kind")
	}
	if key != "" || (kind != "" && !strings.EqualFold(kind, "oauth")) {
		return ""
	}
	if group == opencodeauth.Provider || provider == opencodeauth.Provider {
		if discoveryMetadataString(auth, "access_token") != "" && discoveryOpenCodeOrgID(auth) != "" {
			return opencodeauth.Provider
		}
		return ""
	}
	switch provider {
	case "codex":
		if discoveryMetadataString(auth, "access_token") != "" {
			return provider
		}
	case "claude":
		if strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "access_token")) != "" {
			return provider
		}
	case "xai":
		if discoveryMetadataString(auth, "access_token") != "" && !discoveryXAIUsingAPI(auth, kind) {
			return provider
		}
	}
	return ""
}

func discoveryMetadataString(auth *coreauth.Auth, key string) string {
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}

func discoveryOpenCodeOrgID(auth *coreauth.Auth) string {
	const header = "X-Opencode-Org-Id"
	// File auths synthesize header attributes from metadata. Also accept raw
	// metadata so listing works for credentials obtained directly from login.
	for key, value := range auth.Attributes {
		if name, ok := strings.CutPrefix(key, "header:"); ok && strings.EqualFold(name, header) {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	switch headers := auth.Metadata["headers"].(type) {
	case map[string]any:
		for key, raw := range headers {
			if value, ok := raw.(string); ok && strings.EqualFold(key, header) && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	case map[string]string:
		for key, value := range headers {
			if strings.EqualFold(key, header) && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return discoveryMetadataString(auth, "org_id")
}

// Mirror xaiUsingAPI's attribute -> metadata -> auth_kind precedence.
func discoveryXAIUsingAPI(auth *coreauth.Auth, kind string) bool {
	if using, err := strconv.ParseBool(strings.TrimSpace(auth.Attributes["using_api"])); err == nil {
		return using
	}
	switch value := auth.Metadata["using_api"].(type) {
	case bool:
		return value
	case string:
		if using, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
			return using
		}
	}
	return !strings.EqualFold(kind, "oauth")
}

func discoveryZAIPayAsYouGo(base string) bool {
	u, err := url.Parse(base)
	return err == nil && strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "api.z.ai") &&
		strings.TrimRight(u.EscapedPath(), "/") != "/api/coding/paas/v4"
}

func fetchDiscoveryCodex(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]discoveredModel, error) {
	const channel = "codex"
	if discoveryChannelForAuth(auth) != channel {
		return nil, errDiscoveryNotEligible
	}
	req, err := codexlisting.NewRequest(ctx, modelDiscoveryEndpoints[channel], codexlisting.ClientVersion,
		discoveryMetadataString(auth, "access_token"), discoveryMetadataString(auth, "account_id"), auth.Attributes)
	if err != nil {
		return nil, errors.New("codex model discovery: invalid listing request")
	}

	page, err := readDiscoveryPage(discoveryHTTPClient(ctx, cfg, auth), req, channel)
	if err != nil {
		return nil, err
	}
	// Read the upstream catalog shape (see registry's Codex client catalog).
	// The command's stricter offline catalog and baseline injection do not apply.
	return parseDiscoveryRows(page, channel, "models", "slug", func(row ojson.Result, model *discoveredModel) {
		model.Kind = "chat"
		model.ContextTokens = discoveryNumber(row.Get("max_context_window"))
		if model.ContextTokens == 0 {
			model.ContextTokens = discoveryNumber(row.Get("context_window"))
		}
		model.Efforts = discoveryLabels(row.Get("supported_reasoning_levels"), "effort", nil)
		model.ServiceTiers = discoveryLabels(row.Get("service_tiers"), "id", nil)
		model.ServiceTiers = discoveryLabels(row.Get("additional_speed_tiers"), "", model.ServiceTiers)
	})
}

func fetchDiscoveryOpenCode(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]discoveredModel, error) {
	const channel = opencodeauth.Provider
	if discoveryChannelForAuth(auth) != channel {
		return nil, errDiscoveryNotEligible
	}
	endpoint := modelDiscoveryEndpoints[channel]
	base := strings.TrimSpace(auth.Attributes["base_url"])
	if base == "" {
		base = discoveryMetadataString(auth, "base_url")
	}
	if base = strings.TrimRight(base, "/"); base != "" && base != opencodeauth.BaseURL {
		u, err := url.Parse(base)
		if err != nil {
			return nil, errors.New("opencode-go model discovery: invalid inference base URL")
		}
		// Console inference uses /openai/v1, but the catalog is beside it at /v1.
		if strings.HasSuffix(u.Path, "/openai/v1") {
			u.Path = strings.TrimSuffix(u.Path, "/openai/v1") + "/v1"
			u.RawPath = ""
		}
		endpoint = u.JoinPath("models").String()
	}
	req, err := newDiscoveryRequest(ctx, channel, endpoint, discoveryMetadataString(auth, "access_token"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Opencode-Org-Id", discoveryOpenCodeOrgID(auth))
	page, err := readDiscoveryPage(discoveryHTTPClient(ctx, cfg, auth), req, channel)
	if err != nil {
		return nil, err
	}
	return parseDiscoveryRows(page, channel, "data", "id", func(row ojson.Result, model *discoveredModel) {
		if model.DisplayName == "" {
			model.DisplayName = discoveryString(row.Get("name"))
		}
		model.ContextTokens = discoveryNumber(row.Get("limits.context"))
		model.InputTokens = discoveryNumber(row.Get("limits.input"))
		model.OutputTokens = discoveryNumber(row.Get("limits.output"))
	})
}

func fetchDiscoveryClaude(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]discoveredModel, error) {
	const channel = "claude"
	if discoveryChannelForAuth(auth) != channel {
		return nil, errDiscoveryNotEligible
	}
	token := strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "access_token"))
	client := discoveryHTTPClient(ctx, cfg, auth)
	query := url.Values{"limit": {"100"}}
	seen := make(map[string]struct{})
	var models []discoveredModel
	for {
		req, err := newDiscoveryRequest(ctx, channel, modelDiscoveryEndpoints[channel], token, query)
		if err != nil {
			return nil, err
		}
		helps.ApplyClaudeDefaultDeviceProfileHeaders(req, cfg)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")

		page, err := readDiscoveryPage(client, req, channel)
		if err != nil {
			return nil, err
		}
		rows, err := parseDiscoveryRows(page, channel, "data", "id", func(row ojson.Result, model *discoveredModel) {
			model.Kind = "chat"
			model.InputTokens = discoveryNumber(row.Get("max_input_tokens"))
			model.OutputTokens = discoveryNumber(row.Get("max_tokens"))
			if created, errParse := time.Parse(time.RFC3339, discoveryString(row.Get("created_at"))); errParse == nil {
				model.Created = created.Unix()
			}
		})
		if err != nil {
			return nil, err
		}
		models = append(models, rows...)
		switch page.Get("has_more").Type {
		case ojson.False:
			return models, nil
		case ojson.True:
			after := discoveryString(page.Get("last_id"))
			if strings.TrimSpace(after) == "" || len(rows) == 0 {
				return nil, errors.New("claude model discovery: incomplete pagination")
			}
			if _, duplicate := seen[after]; duplicate {
				return nil, errors.New("claude model discovery: repeated pagination cursor")
			}
			seen[after] = struct{}{}
			query.Set("after_id", after)
		default:
			return nil, errors.New("claude model discovery: expected boolean has_more")
		}
	}
}

func fetchDiscoveryXAI(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]discoveredModel, error) {
	const channel = "xai"
	if discoveryChannelForAuth(auth) != channel {
		return nil, errDiscoveryNotEligible
	}
	req, err := newDiscoveryRequest(ctx, channel, modelDiscoveryEndpoints[channel], discoveryMetadataString(auth, "access_token"), nil)
	if err != nil {
		return nil, err
	}
	// Match applyXAIChatHeaders on the CLI chat proxy. The existing version
	// helper is a local cache read; this fetch never starts its network updater.
	version := helps.GetXAIClientVersion()
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("X-Grok-Client-Version", version)
	req.Header.Set("X-Grok-Client-Identifier", "grok-shell")
	req.Header.Set("X-AuthenticateResponse", "authenticate-response")
	req.Header.Set("User-Agent", "xai-grok-workspace/"+version)

	page, err := readDiscoveryPage(discoveryHTTPClient(ctx, cfg, auth), req, channel)
	if err != nil {
		return nil, err
	}
	return parseDiscoveryRows(page, channel, "data", "id", func(_ ojson.Result, model *discoveredModel) {
		if model.Kind == "" {
			model.Kind = "chat"
		}
	})
}

func fetchDiscoveryZAI(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]discoveredModel, error) {
	const channel = "zai"
	if discoveryChannelForAuth(auth) != channel {
		return nil, errDiscoveryNotEligible
	}
	u, err := url.Parse(strings.TrimSpace(auth.Attributes["base_url"]))
	if err != nil {
		return nil, errors.New("zai model discovery: invalid Coding Plan base URL")
	}
	req, err := newDiscoveryRequest(ctx, channel, u.JoinPath("models").String(), strings.TrimSpace(auth.Attributes["api_key"]), nil)
	if err != nil {
		return nil, err
	}
	page, err := readDiscoveryPage(discoveryHTTPClient(ctx, cfg, auth), req, channel)
	if err != nil {
		return nil, err
	}
	return parseDiscoveryRows(page, channel, "data", "id", nil)
}

func newDiscoveryRequest(ctx context.Context, channel, endpoint, token string, query url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil || req.URL.Host == "" || (req.URL.Scheme != "https" && req.URL.Scheme != "http") {
		return nil, fmt.Errorf("%s model discovery: invalid listing URL", channel)
	}
	if len(query) != 0 {
		q := req.URL.Query()
		for key, values := range query {
			q[key] = values
		}
		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func discoveryHTTPClient(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) *http.Client {
	client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	// Only a response from the listing endpoint is authoritative. Redirects
	// must not forward these credentials or turn an error into another catalog.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func readDiscoveryPage(client *http.Client, req *http.Request, channel string) (ojson.Result, error) {
	resp, err := client.Do(req)
	if err != nil {
		return ojson.Result{}, discoveryRequestError(req.Context(), channel, "listing request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || resp.StatusCode == http.StatusPartialContent {
		return ojson.Result{}, fmt.Errorf("%s model discovery: upstream HTTP %d", channel, resp.StatusCode)
	}
	const maxListingBytes = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListingBytes+1))
	if err != nil {
		return ojson.Result{}, discoveryRequestError(req.Context(), channel, "could not read listing response")
	}
	if len(body) > maxListingBytes {
		return ojson.Result{}, fmt.Errorf("%s model discovery: listing response too large", channel)
	}
	if !ojson.ValidBytes(body) {
		return ojson.Result{}, fmt.Errorf("%s model discovery: malformed listing JSON", channel)
	}
	return ojson.ParseBytes(body), nil
}

func discoveryRequestError(ctx context.Context, channel, message string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s model discovery: %w", channel, err)
	}
	// Transport errors can include the configured URL; upstream error bodies
	// can echo submitted credentials. Do not expose either to discovery logs.
	return fmt.Errorf("%s model discovery: %s", channel, message)
}

func parseDiscoveryRows(page ojson.Result, channel, arrayKey, idKey string, parseRow func(ojson.Result, *discoveredModel)) ([]discoveredModel, error) {
	list := page.Get(arrayKey)
	if !page.IsObject() || !list.IsArray() {
		return nil, fmt.Errorf("%s model discovery: expected %s array", channel, arrayKey)
	}
	rows := list.Array()
	models := make([]discoveredModel, 0, len(rows))
	for i, row := range rows {
		id := discoveryString(row.Get(idKey))
		if !row.IsObject() || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%s model discovery: missing or invalid %s in row %d", channel, idKey, i)
		}
		model := discoveredModel{
			ID:          id,
			DisplayName: discoveryString(row.Get("display_name")),
			Kind:        discoveryString(row.Get("kind")),
			Family:      discoveryString(row.Get("family")),
			Created:     discoveryNumber(row.Get("created")),
		}
		if parseRow != nil {
			parseRow(row, &model)
		}
		models = append(models, model)
	}
	return models, nil
}

func discoveryString(value ojson.Result) string {
	if value.Type == ojson.String {
		return value.Str
	}
	return ""
}

func discoveryNumber(value ojson.Result) int64 {
	if value.Type == ojson.Number {
		n, err := strconv.ParseInt(value.Raw, 10, 64)
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func discoveryLabels(result ojson.Result, key string, labels []string) []string {
	for _, value := range result.Array() {
		if key != "" {
			value = value.Get(key)
		}
		if label := discoveryString(value); strings.TrimSpace(label) != "" && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	return labels
}
