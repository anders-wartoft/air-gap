package pinnedtls

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

func ParsePinIdleSeconds(value string) (int, error) {
	number, err := strconv.ParseInt(value, 10, 32)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("tcpTLSPinIdleSeconds must be an integer in 0..2147483647")
	}
	return int(number), nil
}

func (m *Manager) resetIdle(now time.Time) {
	lastUsed := make(map[string]time.Time, len(m.active.pins))
	for fingerprint := range m.active.pins {
		lastUsed[fingerprint] = m.lastUsed[fingerprint]
	}
	m.lastUsed = lastUsed
	m.lastUse = make(map[string]time.Time, len(m.active.pins))
	m.aged = make(map[string]string)
	for fingerprint := range m.active.pins {
		m.lastUse[fingerprint] = now
	}
}

// StartAging is called once during adapter construction. Connected keys are
// protected; after their last session closes, a new idle period begins.
func (m *Manager) StartAging(seconds int, report func([]string)) {
	m.mu.Lock()
	m.idleTimeout = time.Duration(seconds) * time.Second
	m.mu.Unlock()
	if seconds == 0 {
		return
	}
	m.agingWorkers.Add(1)
	go func() {
		defer m.agingWorkers.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.agingStop:
				return
			case now := <-ticker.C:
				if removed := m.purgeIdle(now); len(removed) != 0 && report != nil {
					report(removed)
				}
			}
		}
	}()
}

func (m *Manager) StopAging() {
	m.agingOnce.Do(func() { close(m.agingStop) })
	m.agingWorkers.Wait()
}

func (m *Manager) purgeIdle(now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimeout <= 0 {
		return nil
	}
	connected := make(map[string]bool)
	for _, auth := range m.sessions {
		connected[auth.fingerprint] = true
	}
	latest := make(map[string]string)
	for fingerprint, entry := range m.active.pins {
		previous, exists := latest[entry.Peer]
		if !exists || m.lastUsed[fingerprint].After(m.lastUsed[previous]) ||
			(m.lastUsed[fingerprint].Equal(m.lastUsed[previous]) && fingerprint < previous) {
			latest[entry.Peer] = fingerprint
		}
	}
	pins := make(map[string]Entry, len(m.active.pins))
	var removed []string
	for fingerprint, entry := range m.active.pins {
		if latest[entry.Peer] != fingerprint && !connected[fingerprint] &&
			now.Sub(m.lastUse[fingerprint]) >= m.idleTimeout {
			m.aged[fingerprint] = entry.Peer
			removed = append(removed, fingerprint)
			continue
		}
		pins[fingerprint] = entry
	}
	if len(removed) != 0 {
		snapshot := *m.active
		snapshot.pins = pins
		m.active = m.configure(&snapshot)
		sort.Strings(removed)
	}
	return removed
}

// Rotation must not restore aged pins just because it rereads the directory.
func (m *Manager) excludeAged(pins map[string]Entry) {
	for fingerprint, aged := range m.aged {
		if entry, exists := pins[fingerprint]; exists && entry.Peer == aged {
			delete(pins, fingerprint)
		}
	}
}
