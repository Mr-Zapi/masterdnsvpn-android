// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
// Package client provides the core logic for the MasterDnsVPN client.
// This file (mtu_cache.go) persists the last discovered per-resolver MTU so a
// reconnect can bring the tunnel up immediately and refresh the values in the
// background instead of blocking startup on a full MTU scan.
// ==============================================================================
package client

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type mtuCacheEntry struct {
	Upload   int   `json:"up"`
	Download int   `json:"down"`
	Updated  int64 `json:"ts"`
}

type mtuCache struct {
	mu      sync.Mutex
	path    string
	ttl     time.Duration
	entries map[string]mtuCacheEntry
}

// loadMTUCache reads the on-disk cache. A missing or unreadable file yields an
// empty cache; callers can still discover MTU normally.
func loadMTUCache(path string, ttl time.Duration) *mtuCache {
	mc := &mtuCache{
		path:    path,
		ttl:     ttl,
		entries: make(map[string]mtuCacheEntry),
	}
	if path == "" {
		return mc
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return mc
	}

	var entries map[string]mtuCacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return mc
	}

	for key, entry := range entries {
		if key == "" || entry.Upload <= 0 || entry.Download <= 0 {
			continue
		}
		mc.entries[key] = entry
	}
	return mc
}

// get returns a fresh cache entry for the connection key, if any.
func (mc *mtuCache) get(key string, now time.Time) (mtuCacheEntry, bool) {
	if mc == nil || key == "" {
		return mtuCacheEntry{}, false
	}

	mc.mu.Lock()
	entry, ok := mc.entries[key]
	mc.mu.Unlock()

	if !ok || entry.Upload <= 0 || entry.Download <= 0 {
		return mtuCacheEntry{}, false
	}
	if mc.ttl > 0 && entry.Updated > 0 && now.Sub(time.Unix(entry.Updated, 0)) > mc.ttl {
		return mtuCacheEntry{}, false
	}
	return entry, true
}

// put stores a discovered MTU. Persisting is explicit via persist().
func (mc *mtuCache) put(key string, upload, download int, now time.Time) {
	if mc == nil || key == "" || upload <= 0 || download <= 0 {
		return
	}

	mc.mu.Lock()
	if mc.entries == nil {
		mc.entries = make(map[string]mtuCacheEntry)
	}
	mc.entries[key] = mtuCacheEntry{
		Upload:   upload,
		Download: download,
		Updated:  now.Unix(),
	}
	mc.mu.Unlock()
}

// persist atomically writes the cache to disk.
func (mc *mtuCache) persist() error {
	if mc == nil || mc.path == "" {
		return nil
	}

	mc.mu.Lock()
	entries := make(map[string]mtuCacheEntry, len(mc.entries))
	for key, entry := range mc.entries {
		entries[key] = entry
	}
	mc.mu.Unlock()

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}

	tmp := mc.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, mc.path)
}
