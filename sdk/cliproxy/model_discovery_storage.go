package cliproxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func (s *Service) discoveryCachePath() string {
	if s.configPath == "" {
		return ""
	}
	// Only the exact config path and immediate .json files in auth-dir are
	// watched. A sibling .cache is neither, even if both directories coincide.
	return s.configPath + ".model-discovery.cache"
}

func loadDiscoveryCache(path string) map[string]map[string]discoveryRow {
	cache := make(map[string]map[string]discoveryRow)
	if path == "" {
		return cache
	}
	contents, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return cache
	}
	var saved map[string]map[string]discoveryRow
	if errRead != nil || yaml.Unmarshal(contents, &saved) != nil {
		log.Warn("could not load model discovery cache")
		return cache
	}
	now := modelDiscoveryNow().UTC()
	for key, rows := range saved {
		if _, errDecode := hex.DecodeString(key); len(key) != 64 || errDecode != nil || rows == nil {
			continue
		}
		for id, row := range rows {
			if id == "" || id != row.Model.ID || row.Misses < 0 || row.Misses > 3 {
				delete(rows, id)
			} else if row.LastSeen.IsZero() {
				// Older caches cannot prove how long the model has been absent.
				row.LastSeen = now
				rows[id] = row
			}
		}
		cache[key] = rows
	}
	return cache
}

func (s *Service) persistDiscoveryCacheLocked() {
	s.modelDiscovery.dirty = true
	s.flushDiscoveryCacheLocked()
}

func (s *Service) flushDiscoveryCacheLocked() {
	path := s.discoveryCachePath()
	if path == "" {
		return
	}
	data, errMarshal := yaml.Marshal(s.modelDiscovery.cache)
	if errMarshal != nil || writeDiscoveryCache(path, data) != nil {
		log.Warn("could not persist model discovery cache")
		return
	}
	s.modelDiscovery.dirty = false
}

func writeDiscoveryCache(path string, data []byte) error {
	f, errCreate := os.CreateTemp(filepath.Dir(path), ".model-discovery-*")
	if errCreate != nil {
		return errCreate
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	if errClose := f.Close(); errClose != nil {
		return errClose
	}
	return os.Rename(f.Name(), path)
}

func (s *Service) removeDiscoveredExtras(ctx context.Context, authID, channel string, account *discoveryAccount, ids map[string]bool) bool {
	if s.configPath == "" || len(ids) == 0 {
		return true
	}
	applied := false
	var removed []string
	d := &s.modelDiscovery
	var release func()
	locked := false
	defer func() {
		if locked {
			d.mu.Unlock()
		}
		if release != nil {
			release()
		}
	}()
	errUpdate := config.UpdateConfigFile(s.configPath, func(raw []byte) ([]byte, error) {
		auth, unlock, errHold := s.coreManager.HoldAuthSnapshot(ctx, authID)
		if errHold != nil {
			return nil, errHold
		}
		release = unlock
		d.mu.Lock()
		locked = true
		if ctx.Err() != nil || d.accounts[authID] != account || d.dirty || !s.discoveryCredentialCurrent(s.currentConfig(), auth, account) {
			return nil, nil
		}
		for id := range ids {
			if _, ok := d.cache[account.key][id]; !ok {
				return nil, nil
			}
		}
		data, removedIDs, errEdit := removeDiscoveryExtrasFromYAML(raw, channel, ids)
		removed = removedIDs
		applied = errEdit == nil
		return data, errEdit
	})
	if errUpdate == nil {
		slices.Sort(removed)
		for _, id := range slices.Compact(removed) {
			log.Infof("model discovery removed extra channel=%s id=%q", channel, id)
		}
	} else if !errors.Is(errUpdate, config.ErrConfigWriteConflict) {
		log.Warnf("model discovery could not remove extras from config channel=%s", channel)
	}
	return errUpdate == nil && applied
}

func removeDiscoveryExtrasFromYAML(raw []byte, channel string, ids map[string]bool) ([]byte, []string, error) {
	var doc yaml.Node
	if errParse := yaml.Unmarshal(raw, &doc); errParse != nil {
		return nil, nil, errParse
	}
	if len(doc.Content) != 1 {
		return nil, nil, nil
	}
	settings := discoveryYAMLNode(doc.Content[0], "models", "discovery", channel)
	mode := discoveryYAMLNode(settings, "mode")
	list := discoveryYAMLNode(settings, "extra")
	if mode == nil || mode.Value != "live" || list == nil || list.Kind != yaml.SequenceNode {
		return nil, nil, nil
	}
	kept := list.Content[:0]
	var removed []string
	for _, entry := range list.Content {
		if id := strings.TrimSpace(entry.Value); entry.Kind == yaml.ScalarNode && ids[id] {
			removed = append(removed, id)
		} else {
			kept = append(kept, entry)
		}
	}
	if len(removed) == 0 {
		return nil, nil, nil
	}
	list.Content = kept
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if errEncode := encoder.Encode(&doc); errEncode != nil {
		return nil, nil, errEncode
	}
	if errClose := encoder.Close(); errClose != nil {
		return nil, nil, errClose
	}
	return out.Bytes(), removed, nil
}

func discoveryYAMLNode(root *yaml.Node, keys ...string) *yaml.Node {
	for _, key := range keys {
		if root == nil || root.Kind != yaml.MappingNode {
			return nil
		}
		var child *yaml.Node
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				child = root.Content[i+1]
				break
			}
		}
		root = child
	}
	return root
}
