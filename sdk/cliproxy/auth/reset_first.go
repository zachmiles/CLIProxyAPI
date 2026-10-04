package auth

import (
	"context"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetFirstSelector spends the credential whose weekly quota window resets
// soonest, so quota that is about to expire is used before quota that carries
// over. Credentials without an observed future reset are picked first so their
// window gets observed; ties keep fill-first order.
type ResetFirstSelector struct{}

// Pick selects the available auth with the earliest weekly reset.
func (s *ResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	picked, pickedReset := available[0], weeklyResetAt(available[0], now)
	for _, candidate := range available[1:] {
		if reset := weeklyResetAt(candidate, now); reset.Before(pickedReset) {
			picked, pickedReset = candidate, reset
		}
	}
	return picked, nil
}

// weeklyResetAt returns the earliest observed future weekly reset for a
// credential, or the zero time when none is known.
func weeklyResetAt(auth *Auth, now time.Time) time.Time {
	if auth == nil {
		return time.Time{}
	}
	signals := auth.Quota.Signals
	var earliest time.Time
	consider := func(reset time.Time) {
		if reset.After(now) && (earliest.IsZero() || reset.Before(earliest)) {
			earliest = reset
		}
	}
	for _, name := range []string{
		"Anthropic-Ratelimit-Unified-7d-Reset",
		"Anthropic-Ratelimit-Unified-7d_oi-Reset",
		"X-Codex-Secondary-Reset-At",
	} {
		if seconds, err := strconv.ParseInt(strings.TrimSpace(signals[name]), 10, 64); err == nil && seconds > 0 {
			consider(time.Unix(seconds, 0))
		}
	}
	if seconds, err := strconv.ParseInt(strings.TrimSpace(signals["X-Codex-Secondary-Reset-After-Seconds"]), 10, 64); err == nil && seconds >= 0 {
		consider(auth.Quota.ObservedAt.Add(time.Duration(seconds) * time.Second))
	}
	return earliest
}
