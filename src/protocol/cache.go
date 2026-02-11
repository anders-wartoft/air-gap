package protocol

import (
	"errors"
	"sync"
	"time"
)

// Time each cache entry should live
// Also, how often the background thread
// removing old entries should run
const TTL = 30 * time.Second

type MessageCacheEntry struct {
	end time.Time
	len uint16
	val map[uint16][]byte
}

type MessageCache struct {
	mu      sync.RWMutex
	entries map[string]*MessageCacheEntry
}

func (m *MessageCache) GetEntry(id string) (*MessageCacheEntry, error) {
	m.mu.Lock()
	cachedItem, ok := m.entries[id]
	m.mu.Unlock()
	if ok {
		return cachedItem, nil
	}
	return createMessageCacheEntry(), errors.New("no such key")
}

func (m *MessageCache) GetCacheSize() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func (m *MessageCache) AddEntry(id string, messageId uint16, maxMessageId uint16, payload []byte) {
	m.mu.Lock()
	entry, ok := m.entries[id]
	if !ok {
		entry = createMessageCacheEntry()
		entry.len = maxMessageId
		m.entries[id] = entry
	}
	entry.val[messageId] = append([]byte(nil), payload...)
	m.mu.Unlock()
}

func (m *MessageCache) RemoveEntry(id string) {
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
}

func (m *MessageCache) AddEntryValue(id string, messageId uint16, maxMessageId uint16, payload []byte) {
	entry, _ := m.GetEntry(id)
	m.mu.Lock()
	entry.val[messageId] = append([]byte(nil), payload...)
	m.entries[id] = entry
	m.mu.Unlock()
}

func createMessageCacheEntry() *MessageCacheEntry {
	return &MessageCacheEntry{
		end: time.Now().Add(time.Millisecond + TTL),
		len: 0,
		val: make(map[uint16][]byte),
	}
}

func CreateMessageCache() *MessageCache {
	cache := &MessageCache{
		entries: make(map[string]*MessageCacheEntry),
		mu:      sync.RWMutex{},
	}
	// Start empty old items from the cache
	cache.StartCleaning()
	return cache
}

func (m *MessageCache) StartCleaning() {
	ticker := time.NewTicker(TTL)
	go func() {
		for range ticker.C {
			m.CleanList()
		}
	}()
}

// Remove old entries. Should be run every 30 second or so
func (m *MessageCache) CleanList() {
	m.mu.Lock()
	defer m.mu.Unlock()

	start_time := time.Now()
	Logger.Debugf("Starting cache cleanup task at %v", GetTimestamp())
	cleaned_up_times := 0
	for key, entry := range m.entries {
		if entry.end.Before(time.Now()) {
			delete(m.entries, key)
			cleaned_up_times++
		}
	}
	Logger.Debugf("Cache cleanup finished after %i seconds. %i items was cleaned", time.Now().UTC().Second() - start_time.UTC().Second(), cleaned_up_times)

}
