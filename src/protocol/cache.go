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

// Default security limits for MessageCache (configurable via downstream properties).
const (
	// DefaultMaxCacheEntries caps the number of in-flight fragment-reassembly entries.
	// One entry is held per unique message ID until all fragments arrive or the 30 s
	// TTL expires; without a cap a flood of unique IDs exhausts memory.
	DefaultMaxCacheEntries = 100_000

	// DefaultMaxNrMessages caps the nrMessages field declared in a single packet.
	// Legitimate fragmentation tops out at ~730 parts (1 MiB payload / MTU 1500 bytes);
	// higher values signal a malformed or adversarial packet and are rejected early.
	DefaultMaxNrMessages uint16 = 4096
)

type MessageCache struct {
	mu            sync.RWMutex
	entries       map[string]*MessageCacheEntry
	maxEntries    int    // configurable via downstream maxCacheEntries
	maxNrMessages uint16 // configurable via downstream maxNrMessages
}

// AddPartAndSnapshot adds/updates one multipart payload fragment and returns a
// detached snapshot of the message entry together with a completion flag.
// The returned snapshot can be safely read without holding cache locks.
func (m *MessageCache) AddPartAndSnapshot(id string, messageId uint16, maxMessageId uint16, payload []byte) (MessageCacheEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[id]
	if !ok {
		if len(m.entries) >= m.maxEntries {
			// Cache is full; drop the fragment rather than growing without bound.
			// This protects against memory exhaustion from flooding with unique IDs.
			Logger.Warnf("Fragment cache full (%d entries), dropping fragment for id: %s", m.maxEntries, id)
			return MessageCacheEntry{}, false
		}
		entry = createMessageCacheEntry()
		entry.len = maxMessageId
		m.entries[id] = entry
	} else if entry.len == 0 {
		entry.len = maxMessageId
	}

	entry.val[messageId] = append([]byte(nil), payload...)

	snapshot := MessageCacheEntry{
		end: entry.end,
		len: entry.len,
		val: make(map[uint16][]byte, len(entry.val)),
	}
	for k, v := range entry.val {
		snapshot.val[k] = append([]byte(nil), v...)
	}

	isComplete := snapshot.len > 0 && uint16(len(snapshot.val)) >= snapshot.len
	return snapshot, isComplete
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

// SetLimits updates the security limits for this cache at runtime.
// Call this once, before any messages arrive, after the application config is loaded.
func (m *MessageCache) SetLimits(maxEntries int, maxNrMessages uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxEntries = maxEntries
	m.maxNrMessages = maxNrMessages
}

func CreateMessageCache() *MessageCache {
	cache := &MessageCache{
		entries:       make(map[string]*MessageCacheEntry),
		mu:            sync.RWMutex{},
		maxEntries:    DefaultMaxCacheEntries,
		maxNrMessages: DefaultMaxNrMessages,
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
	Logger.Debugf("Cache cleanup finished after %i seconds. %i items was cleaned", time.Now().UTC().Second()-start_time.UTC().Second(), cleaned_up_times)

}
