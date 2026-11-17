package cliproxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexlisting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	modelDiscoveryInterval        = time.Minute
	modelDiscoveryRetirementFloor = 30 * time.Minute
)

var modelDiscoveryNow = time.Now

type discoveryRow struct {
	Model    discoveredModel
	Misses   int
	LastSeen time.Time `yaml:"last-seen,omitempty"`
}

type discoveryAccount struct {
	key           string
	fence         string
	mode          string
	rows          map[string]discoveryRow // nil means no complete listing
	ctx           context.Context
	cancel        context.CancelFunc // account lifetime, including any active request
	busy          bool
	next          time.Time
	failure       antigravityFailureState
	failureClass  string
	warnAfter     time.Time
	shadowDiff    string
	pendingExtras map[string]bool
}

func (a *discoveryAccount) record(list []discoveredModel, err error, now time.Time, covered func(discoveredModel) bool) (retired []string) {
	if err != nil {
		for id, row := range a.rows {
			row.Misses = 0
			a.rows[id] = row
		}
		return nil
	}
	if a.rows == nil {
		a.rows = make(map[string]discoveryRow)
	}
	seen := make(map[string]bool, len(list))
	for _, row := range list {
		row.ID = strings.TrimSpace(row.ID)
		if row.ID != "" {
			seen[row.ID] = true
			a.rows[row.ID] = discoveryRow{Model: row, LastSeen: now.UTC()}
		}
	}
	for id, row := range a.rows {
		if !seen[id] && covered(row.Model) {
			row.Misses = min(row.Misses+1, 3)
			if row.Misses == 3 && now.Sub(row.LastSeen) >= modelDiscoveryRetirementFloor {
				delete(a.rows, id)
				retired = append(retired, id)
			} else {
				a.rows[id] = row
			}
		}
	}
	return retired
}

type modelDiscoveryState struct {
	mu       sync.Mutex
	start    sync.Once
	wake     chan struct{}
	accounts map[string]*discoveryAccount
	cache    map[string]map[string]discoveryRow
	dirty    bool
}

func (s *Service) initModelDiscovery() {
	d := &s.modelDiscovery
	d.start.Do(func() {
		d.wake = make(chan struct{}, 1)
		d.accounts = make(map[string]*discoveryAccount)
		d.cache = loadDiscoveryCache(s.discoveryCachePath())
	})
}

func (s *Service) wakeModelDiscovery() {
	s.initModelDiscovery()
	select {
	case s.modelDiscovery.wake <- struct{}{}:
	default:
	}
}

func discoverySettings(cfg *config.Config, channel string) registry.ModelDiscovery {
	if cfg == nil || cfg.Home.Enabled {
		return registry.ModelDiscovery{}
	}
	return cfg.Models.Discovery[channel]
}

func discoveryEnabled(mode string) bool { return mode == "shadow" || mode == "live" }

// The persistent key identifies the account and listing shape, independent
// of rotating bearer tokens.
func discoveryKey(cfg *config.Config, auth *coreauth.Auth, channel string) string {
	shape := "1"
	switch channel {
	case "codex":
		shape = codexlisting.ClientVersion
	case "xai":
		shape = helps.GetXAIClientVersion()
	}
	proxy := ""
	if cfg != nil {
		proxy = cfg.ProxyURL
	}
	parts := []string{
		channel, shape, auth.ID, auth.ProxyURL, proxy,
		auth.Attributes["base_url"], modelDiscoveryEndpoints[channel],
	}
	switch channel {
	case "codex":
		parts = append(parts, discoveryMetadataString(auth, "account_id"), auth.Attributes["account_id"])
		for _, name := range slices.Sorted(maps.Keys(auth.Attributes)) {
			if strings.HasPrefix(name, "header:") {
				parts = append(parts, name, auth.Attributes[name])
			}
		}
	case "opencode-go":
		parts = append(parts, discoveryOpenCodeOrgID(auth))
	}
	digest := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(digest, "%d:%s", len(part), part)
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func discoveryCredentialFence(auth *coreauth.Auth) string {
	token := claudeauth.ReadMetadataString(&auth.Metadata, "access_token")
	return fmt.Sprintf("%d:%d:%x", auth.RegistrationEpoch, auth.CredentialVersion,
		sha256.Sum256([]byte(auth.Attributes["api_key"]+"\x00"+token)))
}

func (s *Service) discoveryRows(cfg *config.Config, auth *coreauth.Auth, channel string) map[string]discoveryRow {
	s.initModelDiscovery()
	key := discoveryKey(cfg, auth, channel)
	d := &s.modelDiscovery
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.cache[key])
}

func (s *Service) runProviderModelDiscovery(ctx context.Context) {
	s.initModelDiscovery()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	slots := make(chan struct{}, modelRegistrationMaxWorkersPerCategory)
	for ctx.Err() == nil {
		s.scheduleModelDiscovery(ctx, slots)
		select {
		case <-ctx.Done():
		case <-tick.C:
		case <-s.modelDiscovery.wake:
		}
	}
}

// Each credential gets one account watcher (also when in backoff) and at
// most one outstanding fetch. Global slots bound upstream work.
func (s *Service) scheduleModelDiscovery(ctx context.Context, slots chan struct{}) {
	if s.coreManager == nil || ctx.Err() != nil {
		return
	}
	s.initModelDiscovery()
	cfg := s.currentConfig()
	d := &s.modelDiscovery
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.accounts) == 0 && len(d.cache) == 0 {
		enabled := false
		if cfg != nil && !cfg.Home.Enabled {
			for _, settings := range cfg.Models.Discovery {
				enabled = enabled || discoveryEnabled(settings.Mode)
			}
		}
		if !enabled {
			return
		}
	}
	now := modelDiscoveryNow()
	keep := make(map[string]bool)
	for _, auth := range s.coreManager.List() {
		channel := discoveryChannelForAuth(auth)
		if channel == "" {
			continue
		}
		key := discoveryKey(cfg, auth, channel)
		keep[key] = true
		mode := discoverySettings(cfg, channel).Mode
		if !discoveryEnabled(mode) || modelDiscoveryFetchers[channel] == nil {
			continue
		}
		id := auth.ID
		account := d.accounts[id]
		fence := discoveryCredentialFence(auth)
		if account != nil && (account.key != key || account.mode != mode || account.fence != fence) {
			account.cancel()
			if account.key != key {
				delete(d.cache, account.key)
				s.persistDiscoveryCacheLocked()
			}
			account = nil
		}
		if account == nil {
			life, cancel := context.WithCancel(ctx)
			changes, unsubscribe := s.coreManager.WatchAuthChanges(id)
			account = &discoveryAccount{key: key, fence: fence, mode: mode, ctx: life, cancel: cancel, rows: maps.Clone(d.cache[key])}
			if !s.discoveryAccountCurrent(id, account) {
				cancel()
				unsubscribe()
				delete(d.accounts, id)
				continue
			}
			d.accounts[id] = account
			go s.watchDiscoveryAccount(ctx, id, account, changes, unsubscribe)
		}
		if account.busy || now.Before(account.next) {
			continue
		}
		select {
		case slots <- struct{}{}:
			account.busy = true
			go func(auth *coreauth.Auth, account *discoveryAccount, channel string) {
				defer func() {
					<-slots
					s.wakeModelDiscovery()
				}()
				s.fetchDiscoveryAccount(account.ctx, cfg, auth, channel, account)
			}(auth, account, channel)
		default:
		}
	}
	for id, account := range d.accounts {
		if !keep[account.key] || !s.discoveryAccountCurrent(id, account) {
			account.cancel()
			delete(d.accounts, id)
		}
	}
	changed := false
	for key := range d.cache {
		if !keep[key] {
			delete(d.cache, key)
			changed = true
		}
	}
	if changed {
		s.persistDiscoveryCacheLocked()
	}
}

func (s *Service) discoveryAccountCurrent(id string, account *discoveryAccount) bool {
	auth, _ := s.coreManager.GetByID(id)
	return s.discoveryCredentialCurrent(s.currentConfig(), auth, account)
}

func (s *Service) discoveryCredentialCurrent(cfg *config.Config, auth *coreauth.Auth, account *discoveryAccount) bool {
	channel := discoveryChannelForAuth(auth)
	return channel != "" && discoverySettings(cfg, channel).Mode == account.mode &&
		discoveryKey(cfg, auth, channel) == account.key && discoveryCredentialFence(auth) == account.fence
}

func (s *Service) watchDiscoveryAccount(ctx context.Context, id string, account *discoveryAccount, changes <-chan struct{}, unsubscribe func()) {
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case <-discoveryAccountDone(account):
			return
		case <-changes:
			if s.discoveryAccountCurrent(id, account) {
				continue
			}
			s.invalidateDiscoveryAccount(id)
			if auth, ok := s.coreManager.GetByID(id); ok {
				s.completeModelRegistrationForAuth(ctx, auth)
			}
			return
		}
	}
}

func discoveryAccountDone(account *discoveryAccount) <-chan struct{} {
	if account.ctx != nil {
		return account.ctx.Done()
	}
	return nil
}

func (s *Service) invalidateDiscoveryAccount(id string) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.initModelDiscovery()
	d := &s.modelDiscovery
	d.mu.Lock()
	account := d.accounts[id]
	if account != nil && !s.discoveryAccountCurrent(id, account) {
		account.cancel()
		delete(d.accounts, id)
		auth, _ := s.coreManager.GetByID(id)
		cfg := s.currentConfig()
		if channel := discoveryChannelForAuth(auth); channel == "" || discoveryKey(cfg, auth, channel) != account.key {
			delete(d.cache, account.key)
			s.persistDiscoveryCacheLocked()
		}
	}
	d.mu.Unlock()
	s.wakeModelDiscovery()
}

func (s *Service) fetchDiscoveryAccount(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, channel string, account *discoveryAccount) {
	list, errFetch := modelDiscoveryFetchers[channel](ctx, cfg, auth)
	var catalogKinds map[string]string
	if errFetch == nil {
		catalogKinds = discoveryCatalogKinds(cfg, auth, channel)
	}

	d := &s.modelDiscovery
	defer func() {
		d.mu.Lock()
		account.busy = false
		d.mu.Unlock()
	}()
	d.mu.Lock()
	if ctx.Err() != nil || d.accounts[auth.ID] != account || !s.discoveryAccountCurrent(auth.ID, account) {
		d.mu.Unlock()
		return
	}
	now := modelDiscoveryNow()
	previous := d.cache[account.key]
	retired := account.record(list, errFetch, now, func(row discoveredModel) bool {
		kind := catalogKinds[row.ID]
		if kind == "" {
			kind = inventoryModelKind(&ModelInfo{DiscoveryKind: row.Kind})
		}
		return discoveryListingCovers(channel, kind)
	})
	if account.rows != nil && !reflect.DeepEqual(previous, account.rows) {
		d.cache[account.key] = maps.Clone(account.rows)
		d.dirty = true
	}
	if d.dirty {
		s.flushDiscoveryCacheLocked()
	}
	if errFetch != nil {
		if errors.Is(errFetch, errDiscoveryNotEligible) {
			account.next = now.Add(modelDiscoveryInterval)
			d.mu.Unlock()
			return
		}
		account.failure = nextAntigravityFailure(account.failure, now, rand.Int64N)
		account.next = account.failure.nextRetry
		summary := discoveryFailureSummary(errFetch)
		warn := summary != account.failureClass || !now.Before(account.warnAfter)
		if warn {
			account.failureClass = summary
			account.warnAfter = account.next
		}
		retryIn := account.next.Sub(now).Round(time.Second)
		d.mu.Unlock()
		if warn {
			log.Warnf("model discovery backoff channel=%s retry_in=%s: %s; keeping last good", channel, retryIn, summary)
		}
		return
	}
	account.failure = antigravityFailureState{}
	account.failureClass = ""
	account.warnAfter = time.Time{}
	account.next = now.Add(modelDiscoveryInterval)
	var removeExtras map[string]bool
	var routesChanged bool
	if account.mode == "live" {
		// Persist sightings and misses without republishing identical routes.
		routesChanged = previous == nil || !maps.EqualFunc(previous, account.rows, func(a, b discoveryRow) bool {
			return reflect.DeepEqual(a.Model, b.Model)
		})
		removeExtras = account.pendingExtras
		sighted := make(map[string]bool, len(list))
		for _, row := range list {
			sighted[strings.TrimSpace(row.ID)] = true
		}
		for _, id := range discoverySettings(s.currentConfig(), channel).Extra {
			id = strings.TrimSpace(id)
			if sighted[id] {
				if removeExtras == nil {
					removeExtras = make(map[string]bool)
				}
				removeExtras[id] = true
			}
		}
		for id := range removeExtras {
			if _, stillKnown := account.rows[id]; !stillKnown {
				delete(removeExtras, id)
			}
		}
		account.pendingExtras = removeExtras
	}
	d.mu.Unlock()

	switch account.mode {
	case "live":
		if routesChanged {
			s.completeModelRegistrationForAuth(ctx, auth)
		}
		slices.Sort(retired)
		for _, id := range retired {
			log.Infof("model discovery retired channel=%s id=%q", channel, id)
		}
		if s.removeDiscoveredExtras(ctx, auth.ID, channel, account, removeExtras) {
			d.mu.Lock()
			account.pendingExtras = nil
			d.mu.Unlock()
		}
	case "shadow":
		s.logShadowDiscoveryDiff(auth.ID, channel, account, list)
	}
}

func (s *Service) logShadowDiscoveryDiff(id, channel string, account *discoveryAccount, list []discoveredModel) {
	served := make(map[string]bool)
	covered := make(map[string]bool)
	for _, model := range GlobalModelRegistry().GetModelsForClient(id) {
		upstream := modelUpstreamID(model)
		served[upstream] = true
		if discoveryListingCovers(channel, inventoryModelKind(model)) {
			covered[upstream] = true
		}
	}
	listed := make(map[string]bool, len(list))
	for _, model := range list {
		if id := strings.TrimSpace(model.ID); id != "" {
			listed[id] = true
		}
	}
	var added []string
	for id := range listed {
		if !served[id] {
			added = append(added, id)
		}
		delete(covered, id)
	}
	missing := slices.Sorted(maps.Keys(covered))
	slices.Sort(added)
	diff := fmt.Sprintf("%d/%d/%q/%q", len(listed), len(served), added, missing)
	d := &s.modelDiscovery
	d.mu.Lock()
	if d.accounts[id] == account && diff != account.shadowDiff {
		log.Infof("model discovery shadow channel=%s discovered=%d served=%d added=%d %s missing=%d %s",
			channel, len(listed), len(served), len(added), formatDiscoveryDiffIDs(added), len(missing), formatDiscoveryDiffIDs(missing))
		account.shadowDiff = diff
	}
	d.mu.Unlock()
}

func formatDiscoveryDiffIDs(ids []string) string {
	const limit = 20
	if len(ids) > limit {
		return fmt.Sprintf("%q …+%d more", ids[:limit], len(ids)-limit)
	}
	return fmt.Sprintf("%q", ids)
}

func discoveryFailureSummary(err error) string {
	var status discoveryListingStatus
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "listing timed out"
	case errors.Is(err, context.Canceled):
		return "listing canceled"
	case errors.As(err, &status):
		return status.Error()
	case errors.Is(err, discoveryInvalidEndpoint):
		return string(discoveryInvalidEndpoint)
	case errors.Is(err, discoveryIncomplete):
		return string(discoveryIncomplete)
	case errors.Is(err, discoveryTransport):
		return string(discoveryTransport)
	default:
		return "listing failed"
	}
}
