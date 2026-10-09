package pinnedtls

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

type authorization struct {
	fingerprint string
	peer        string
}

// Manager serializes activation and post-handshake admission, so a handshake
// completed against an older snapshot cannot register after revocation.
type Manager struct {
	mu              sync.Mutex
	active          *Snapshot
	sessions        map[*tls.Conn]authorization
	ticket          [32]byte
	persistRotation func(string, any) error
	idleTimeout     time.Duration
	lastUse         map[string]time.Time
	lastUsed        map[string]time.Time
	aged            map[string]string
	agingStop       chan struct{}
	agingOnce       sync.Once
	agingWorkers    sync.WaitGroup
}

type ReloadResult struct {
	Peers, Keys, Disconnected int
	Added, Removed            []string
}

func NewManager(snapshot *Snapshot) (*Manager, error) {
	m := &Manager{sessions: make(map[*tls.Conn]authorization), agingStop: make(chan struct{})}
	if _, err := rand.Read(m.ticket[:]); err != nil {
		return nil, err
	}
	m.active = m.configure(snapshot)
	m.resetIdle(time.Now())
	return m, nil
}

func (m *Manager) configure(s *Snapshot) *Snapshot {
	copy := *s
	copy.config = s.config.Clone()
	copy.config.SetSessionTicketKeys([][32]byte{m.ticket})
	copy.config.VerifyConnection = func(state tls.ConnectionState) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.active.verify(state)
	}
	return &copy
}

func (m *Manager) Config() *tls.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active.config
}

func (m *Manager) Identity() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Fingerprint(m.active.config.Certificates[0].Leaf.RawSubjectPublicKeyInfo)
}

func (m *Manager) ServerConfig() *tls.Config {
	base := m.Config().Clone()
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return m.Config(), nil
	}
	return base
}

func (m *Manager) Admit(conn *tls.Conn) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fingerprint, entry, err := m.active.authorize(conn.ConnectionState())
	if err != nil {
		return Entry{}, err
	}
	m.sessions[conn] = authorization{fingerprint, entry.Peer}
	m.lastUse[fingerprint] = time.Now()
	m.lastUsed[fingerprint] = m.lastUse[fingerprint]
	return entry, nil
}

func (m *Manager) Forget(conn *tls.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, exists := m.sessions[conn]
	delete(m.sessions, conn)
	if exists {
		connected := false
		for _, other := range m.sessions {
			if other.fingerprint == auth.fingerprint {
				connected = true
				break
			}
		}
		if _, authorized := m.active.pins[auth.fingerprint]; !connected && authorized {
			m.lastUse[auth.fingerprint] = time.Now()
		}
	}
}

func (m *Manager) Activate(candidate *Snapshot) (ReloadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.configure(candidate)
	result := ReloadResult{Keys: len(next.pins)}
	peers := make(map[string]bool)
	for fingerprint, entry := range next.pins {
		peers[entry.Peer] = true
		if previous, ok := m.active.pins[fingerprint]; !ok || previous.Peer != entry.Peer {
			result.Added = append(result.Added, fingerprint)
		}
	}
	for fingerprint, entry := range m.active.pins {
		if current, ok := next.pins[fingerprint]; !ok || current.Peer != entry.Peer {
			result.Removed = append(result.Removed, fingerprint)
		}
	}
	result.Peers = len(peers)
	sort.Strings(result.Added)
	sort.Strings(result.Removed)
	m.active = next
	m.resetIdle(time.Now())
	var closeErr error
	for conn, auth := range m.sessions {
		if entry, ok := next.pins[auth.fingerprint]; !ok || entry.Peer != auth.peer {
			// Close the socket directly: tls.Conn.Close can wait for a concurrent
			// TLS writer and delay revocation while holding the authorization gate.
			if err := conn.NetConn().Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				closeErr = errors.Join(closeErr, fmt.Errorf("revocation disconnect %s: %w", auth.fingerprint, err))
			}
			delete(m.sessions, conn)
			result.Disconnected++
		}
	}
	return result, closeErr
}
