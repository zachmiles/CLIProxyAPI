package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestResetFirstSelectorPick(t *testing.T) {
	t.Parallel()

	now := time.Now()
	unix := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	withSignals := func(id string, signals map[string]string) *Auth {
		return &Auth{ID: id, Quota: QuotaState{ObservedAt: now, Signals: signals}}
	}
	claudeIn4d := withSignals("a-claude-4d", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unix(96 * time.Hour)})
	claudeIn1d := withSignals("b-claude-1d", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unix(24 * time.Hour)})
	fableIn12h := withSignals("c-fable-12h", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":    unix(72 * time.Hour),
		"Anthropic-Ratelimit-Unified-7d_oi-Reset": unix(12 * time.Hour),
	})
	codexIn2d := withSignals("d-codex-2d", map[string]string{"X-Codex-Secondary-Reset-At": unix(48 * time.Hour)})
	codexIn6h := withSignals("e-codex-6h", map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "21600"})
	expired := withSignals("f-expired", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unix(-time.Hour)})
	unknown := &Auth{ID: "g-unknown"}
	cooling := withSignals("0-cooling", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unix(time.Hour)})
	cooling.Unavailable = true
	cooling.NextRetryAfter = now.Add(time.Hour)

	tests := []struct {
		name  string
		auths []*Auth
		want  string
	}{
		{"soonest weekly reset wins", []*Auth{claudeIn4d, claudeIn1d}, "b-claude-1d"},
		{"fable window counts", []*Auth{claudeIn1d, fableIn12h}, "c-fable-12h"},
		{"codex relative reset", []*Auth{codexIn2d, codexIn6h}, "e-codex-6h"},
		{"unknown is probed first", []*Auth{claudeIn1d, unknown}, "g-unknown"},
		{"past reset is unknown", []*Auth{expired, claudeIn1d}, "f-expired"},
		{"cooling auth skipped", []*Auth{cooling, claudeIn4d}, "a-claude-4d"},
	}
	for _, tt := range tests {
		got, err := (&ResetFirstSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, tt.auths)
		if err != nil {
			t.Fatalf("%s: Pick() error = %v", tt.name, err)
		}
		if got.ID != tt.want {
			t.Fatalf("%s: Pick() = %q, want %q", tt.name, got.ID, tt.want)
		}
	}
}
