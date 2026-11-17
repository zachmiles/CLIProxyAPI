package management_test

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelinventory"
)

const inventoryPath = "/v8/management/models/inventory"

type fakeInventorySource struct{ modelinventory.Snapshot }

func (f *fakeInventorySource) InventorySnapshot() modelinventory.Snapshot { return f.Snapshot }

func inventoryServer(t *testing.T, cfg *config.Config, path string, opts ...api.ServerOption) *gin.Engine {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	var router *gin.Engine
	opts = append(opts,
		api.WithEngineConfigurator(func(engine *gin.Engine) { router = engine }),
		api.WithRequestLoggerFactory(nil),
	)
	api.NewServer(cfg, nil, nil, path, opts...)
	return router
}

func inventoryRequest(t *testing.T, router http.Handler, method, path, body string, headers http.Header, status int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	if headers != nil {
		req.Header = headers.Clone()
	}
	req.Header.Set("X-Management-Key", "test-management-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: status %d, want %d; body: %s", method, path, rec.Code, status, rec.Body.String())
	}
	return rec
}

func TestModelInventoryRows(t *testing.T) {
	cfg := &config.Config{OAuthExcludedModels: map[string][]string{
		"claude": {"CLAUDE-*-V1"},
		"codex":  {"gpt-6.2-sol"},
		"gemini": {"gemini-fast"},
		"xai":    {"grok-off*"},
		"other":  {"some-other-model"},
	}}
	const credentialKey = "test-account-key-do-not-serialize"
	source := &fakeInventorySource{modelinventory.Snapshot{Rules: cfg.OAuthExcludedModels, Candidates: []modelinventory.Candidate{
		{Channel: "xai", ID: "grok-on", CredentialKey: "xai-1", CredentialRules: []string{"grok-*"}},
		{Channel: "xai", ID: "grok-on", CredentialKey: "xai-2", CredentialRules: []string{"grok-*"}},
		{Channel: "xai", ID: "grok-on", CredentialKey: "xai-3", Published: true},
		{Channel: "xai", ID: "grok-off", CredentialKey: "xai-3"},
		{Channel: "zai", ID: "glm", Kind: "other", CredentialKey: "zai-1", Published: true},
		{Channel: "opencode-go", ID: "kimi", CredentialKey: "oc-1", Published: true},
		{Channel: "codex", ID: "gpt-6.2-sol-fast", BaseID: "gpt-6.2-sol", CredentialKey: credentialKey},
		{Channel: "codex", ID: "gpt-6.2-sol", Name: "GPT-6.2 Sol", Family: "gpt-sol", Limits: modelinventory.Limits{Context: 400000}, Efforts: []string{"high"}, CredentialKey: credentialKey},
		{Channel: "claude", ID: "opus", RuleID: "claude-opus-v1", CredentialKey: "claude-1"},
		{Channel: "claude", ID: "opus", RuleID: "claude-opus-v1", CredentialKey: "claude-2", CredentialRules: []string{}, Published: true},
		{Channel: "claude", ID: "opus-fast", RuleID: "claude-opus-v1-fast", BaseID: "opus", CredentialKey: "claude-1"},
		{Channel: "gemini", ID: "gemini", CredentialKey: "gemini-1", Published: true},
		{Channel: "gemini", ID: "gemini-fast", BaseID: "gemini", CredentialKey: "gemini-1"},
	}}}
	router := inventoryServer(t, cfg, "", api.WithModelInventorySource(source))
	rec := inventoryRequest(t, router, http.MethodGet, inventoryPath, "", nil, http.StatusOK)
	if strings.Contains(rec.Body.String(), credentialKey) || !ojson.ValidBytes(rec.Body.Bytes()) {
		t.Fatal("response is invalid JSON or includes the credential key")
	}
	doc := ojson.ParseBytes(rec.Body.Bytes())
	if got := doc.Get("rules.other.0").String(); got != "some-other-model" {
		t.Fatalf("rules missing another channel: %s", doc.Get("rules").Raw)
	}
	rows := doc.Get("models").Array()
	want := []string{
		`{"channel":"claude","providerID":"claude-agent","id":"opus","ruleID":"claude-opus-v1","name":"opus","kind":"chat","on":true,"blockedBy":[{"pattern":"CLAUDE-*-V1","source":"rules"}],"followsRules":true,"overrideOn":true}`,
		`{"channel":"claude","providerID":"claude-agent","id":"opus-fast","ruleID":"claude-opus-v1-fast","baseID":"opus","name":"opus-fast","kind":"chat","on":false,"blockedBy":[{"pattern":"CLAUDE-*-V1","source":"rules"}],"followsRules":true,"overrideOn":false}`,
		`{"channel":"codex","providerID":"openai","id":"gpt-6.2-sol","name":"GPT-6.2 Sol","kind":"chat","family":"gpt-sol","limits":{"context":400000},"efforts":["high"],"on":false,"blockedBy":[{"pattern":"gpt-6.2-sol","source":"rules"}],"followsRules":true,"overrideOn":false}`,
		`{"channel":"codex","providerID":"openai","id":"gpt-6.2-sol-fast","baseID":"gpt-6.2-sol","name":"gpt-6.2-sol-fast","kind":"chat","on":false,"blockedBy":[{"pattern":"gpt-6.2-sol","source":"rules"}],"followsRules":true,"overrideOn":false}`,
		`{"channel":"gemini","providerID":"gemini","id":"gemini","name":"gemini","kind":"chat","on":true,"blockedBy":[],"followsRules":true,"overrideOn":false}`,
		`{"channel":"gemini","providerID":"gemini","id":"gemini-fast","baseID":"gemini","name":"gemini-fast","kind":"chat","on":false,"blockedBy":[{"pattern":"gemini-fast","source":"rules"}],"followsRules":true,"overrideOn":false}`,
		`{"channel":"opencode-go","providerID":"opencode-go","id":"kimi","name":"kimi","kind":"chat","on":true,"blockedBy":[],"followsRules":true,"overrideOn":false}`,
		`{"channel":"xai","providerID":"xai","id":"grok-off","name":"grok-off","kind":"chat","on":false,"blockedBy":[{"pattern":"grok-off*","source":"rules"}],"followsRules":true,"overrideOn":false}`,
		`{"channel":"xai","providerID":"xai","id":"grok-on","name":"grok-on","kind":"chat","on":true,"blockedBy":[{"pattern":"grok-*","source":"credential"}],"followsRules":true,"overrideOn":false}`,
		`{"channel":"zai","providerID":"zai-coding-plan","id":"glm","name":"glm","kind":"other","on":true,"blockedBy":[],"followsRules":true,"overrideOn":false}`,
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %s", len(rows), len(want), doc.Get("models").Raw)
	}
	for i, row := range rows {
		if row.Raw != want[i] {
			t.Errorf("row %d: got %s\nwant %s", i, row.Raw, want[i])
		}
	}
	t.Log(rows[2].Raw)

	// Discovery order must not change the representation or its validator.
	slices.Reverse(source.Candidates)
	reversed := inventoryRequest(t, router, http.MethodGet, inventoryPath, "", nil, http.StatusOK)
	if rec.Body.String() != reversed.Body.String() || rec.Header().Get("ETag") != reversed.Header().Get("ETag") {
		t.Fatal("reversing source order changed the response")
	}
}

func TestModelInventoryETagsAndRulesWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver: {port: 8317, response-compression: {enabled: true, min-bytes: 1}}\noauth: {excluded-models: {other: [legacy-off], xai: [stale-rule]}}\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeInventorySource{modelinventory.Snapshot{Rules: cfg.OAuthExcludedModels, SourceHash: cfg.SourceHash, Candidates: []modelinventory.Candidate{
		{Channel: "codex", ID: "gpt-6.2-sol", Published: true},
	}}}
	router := inventoryServer(t, cfg, path, api.WithModelInventorySource(source))
	first := inventoryRequest(t, router, http.MethodGet, inventoryPath, "", http.Header{"Accept-Encoding": {"gzip"}}, http.StatusOK)
	etag := first.Header().Get("ETag")
	if want := fmt.Sprintf(`"%x"`, sha256.Sum256(first.Body.Bytes())); etag != want || !ojson.ValidBytes(first.Body.Bytes()) {
		t.Fatalf("inventory must hash the transmitted JSON: ETag=%q, want %q, encoding=%q", etag, want, first.Header().Get("Content-Encoding"))
	}
	for _, header := range [][]string{{etag}, {`"stale,tag", ` + etag}, {`"stale"`, "W/" + etag}, {"*"}} {
		notModified := inventoryRequest(t, router, http.MethodGet, inventoryPath, "", http.Header{"If-None-Match": header}, http.StatusNotModified)
		if notModified.Header().Get("ETag") != etag || notModified.Body.Len() != 0 {
			t.Fatalf("304 must carry the ETag and no body: %v", notModified.Header())
		}
	}
	inventoryRequest(t, router, http.MethodGet, inventoryPath, "", http.Header{"If-None-Match": {`"stale,*,tag"`}}, http.StatusOK)

	const configPath = "/v8/management/config"
	firstDoc := ojson.ParseBytes(first.Body.Bytes())
	if firstDoc.Get("rules.xai.0").String() != "stale-rule" || firstDoc.Get("models.0.on").Type != ojson.True {
		t.Fatalf("unexpected starting rules or availability: %s", firstDoc.Raw)
	}
	configETag := firstDoc.Get("configETag").String()
	configResponse := inventoryRequest(t, router, http.MethodGet, configPath, "", http.Header{"Accept-Encoding": {"gzip"}}, http.StatusOK)
	if configResponse.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("expected the server to compress other JSON responses")
	}
	if want := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(raw))); configETag != want || configETag != configResponse.Header().Get("ETag") {
		t.Fatalf("inventory configETag=%q, config ETag=%q, file hash=%q", configETag, configResponse.Header().Get("ETag"), want)
	}

	const rulesPath = configPath + "/oauth/excluded-models"
	update := `{"codex":["gpt-6.2-sol"],"other":["legacy-off"]}`
	oldVersion := http.Header{"If-Match": {configETag}}
	saved := inventoryRequest(t, router, http.MethodPut, rulesPath, update, oldVersion, http.StatusOK)
	inventoryRequest(t, router, http.MethodGet, inventoryPath, "", http.Header{"If-None-Match": {etag}}, http.StatusNotModified)
	reloaded, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	source.Rules = reloaded.OAuthExcludedModels
	source.SourceHash = reloaded.SourceHash
	source.Candidates[0].Published = false
	changed := inventoryRequest(t, router, http.MethodGet, inventoryPath, "", http.Header{"If-None-Match": {etag}}, http.StatusOK)
	doc := ojson.ParseBytes(changed.Body.Bytes())
	newConfigETag := doc.Get("configETag").String()
	if newConfigETag == configETag || newConfigETag != saved.Header().Get("ETag") || changed.Header().Get("ETag") == etag {
		t.Fatal("successful config write did not update the inventory and config validators")
	}
	if doc.Get("rules").Raw != update || doc.Get("models.0.on").Type != ojson.False {
		t.Fatalf("PUT must replace the rules map and take the model off: %s", doc.Raw)
	}
	stale := inventoryRequest(t, router, http.MethodPut, rulesPath, `{"codex":[]}`, oldVersion, http.StatusPreconditionFailed)
	if stale.Header().Get("ETag") != newConfigETag || ojson.GetBytes(stale.Body.Bytes(), "error").String() != "precondition_failed" {
		t.Fatalf("stale PUT did not report current version and precondition error: %s", stale.Body.String())
	}
	inventoryRequest(t, router, http.MethodGet, inventoryPath, "", http.Header{"If-None-Match": {changed.Header().Get("ETag")}}, http.StatusNotModified)
}

func TestModelInventoryKeepsPublishedRulesBeforeConfigReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const initial = "config-version: 8\noauth: {excluded-models: {xai: [old-rule]}}\n"
	if errWrite := os.WriteFile(path, []byte(initial), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	router := inventoryServer(t, cfg, path, api.WithModelInventorySource(&fakeInventorySource{modelinventory.Snapshot{Rules: cfg.OAuthExcludedModels, SourceHash: cfg.SourceHash, Candidates: []modelinventory.Candidate{
		{Channel: "codex", ID: "gpt-6.2-sol", Published: true},
	}}}))

	// Edit the file without reloading the server. Preserve the authored pattern
	// and empty list because clients use the rules map as the basis of a PUT.
	const edited = "config-version: 8\noauth-excluded-models: {codex: ['GPT-*'], xai: []}\n"
	if errWrite := os.WriteFile(path, []byte(edited), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	rec := inventoryRequest(t, router, http.MethodGet, inventoryPath, "", nil, http.StatusOK)
	doc := ojson.ParseBytes(rec.Body.Bytes())
	configRules := inventoryRequest(t, router, http.MethodGet, "/v8/management/config/oauth/excluded-models", "", nil, http.StatusOK)
	if got, want := doc.Get("rules").Raw, `{"xai":["old-rule"]}`; got != want {
		t.Fatalf("inventory rules=%s, config rules=%s", got, want)
	}
	if got, want := doc.Get("configETag").String(), fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(initial))); got != want || got == configRules.Header().Get("ETag") {
		t.Fatalf("inventory configETag=%q, want %q", got, want)
	}
	if doc.Get("models.0.on").Type != ojson.True || len(doc.Get("models.0.blockedBy").Array()) != 0 {
		t.Fatalf("inventory row did not keep the published state: %s", doc.Get("models.0").Raw)
	}
}

func TestModelInventoryWithoutSource(t *testing.T) {
	router := inventoryServer(t, &config.Config{OAuthExcludedModels: map[string][]string{"xai": {"grok-*"}}}, "")
	rec := inventoryRequest(t, router, http.MethodGet, inventoryPath, "", nil, http.StatusOK)
	if got, want := rec.Body.String(), `{"configETag":"","rules":{"xai":["grok-*"]},"models":[]}`; got != want {
		t.Fatalf("response=%s, want %s", got, want)
	}
}
