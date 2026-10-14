package protocol

import (
	"errors"
	"sync"
	"time"

	"sitia.nu/airgap/src/logging"
)

// Time each cache entry should live
// Also, how often the background thread
// removing old entries should run
const keyTTL = 7200 * time.Second

type KeyCacheEntry struct {
	end time.Time
	len uint16
	val []byte
}

type KeyCache struct {
	mu      sync.RWMutex
	entries map[string]KeyCacheEntry
}

func (m *KeyCache) GetKey(id string) (KeyCacheEntry, error) {
	m.mu.Lock()
	cachedItem, ok := m.entries[id]
	m.mu.Unlock()
	if ok {
		return cachedItem, nil
	}
	return *createKeyCacheEntry(), errors.New("No such key")
}

func (m *KeyCache) AddKey(id string, payload []byte) {
	entry := createKeyCacheEntry()
	entry.len = uint16(len(payload))
	entry.val = payload
	m.mu.Lock()
	m.entries[id] = *entry
	m.mu.Unlock()
}

func (m *MessageCache) RemoveKey(id string) {
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
}

func createKeyCacheEntry() *KeyCacheEntry {
	return &KeyCacheEntry{
		end: time.Now().Add(time.Millisecond + TTL),
		len: 0,
		val: make([]byte, 0),
	}
}

func CreateKeyCache() *KeyCache {
	cache := &KeyCache{
		entries: make(map[string]KeyCacheEntry),
		mu:      sync.RWMutex{},
	}
	// Start empty old items from the cache
	cache.StartCleaning()
	return cache
}

func (m *KeyCache) StartCleaning() {
	ticker := time.NewTicker(TTL)
	go func() {
		for range ticker.C {
			m.CleanList()
		}
	}()
}

// Remove old entries. Should be run every 30 second or so
func (m *KeyCache) CleanList() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, entry := range m.entries {
		if entry.end.Before(time.Now()) {
			Logger.Debugf(logging.Event(logging.EventCryptoKeyLoad,
				logging.Str("airgap.cache.key", key),
				logging.Str("airgap.cache.action", "evict"),
			), "%v Removing cache item: %v", GetTimestamp(), key)
			delete(m.entries, key)
		}
	}
}
