package upstream

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTCPIdlePeerCloseIsDetectedBeforeNextSend(t *testing.T) {
	for _, mode := range []string{"tcp", "tls12", "tls13"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var clientTLS *tls.Config
			if mode != "tcp" {
				certServer := httptest.NewTLSServer(nil)
				cert := certServer.TLS.Certificates[0]
				certServer.Close()
				leaf, err := x509.ParseCertificate(cert.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				roots := x509.NewCertPool()
				roots.AddCert(leaf)
				version := uint16(tls.VersionTLS13)
				if mode == "tls12" {
					version = tls.VersionTLS12
				}
				listener = tls.NewListener(listener, &tls.Config{
					Certificates: []tls.Certificate{cert},
					MinVersion:   version, MaxVersion: version,
				})
				clientTLS = &tls.Config{
					RootCAs: roots, ServerName: "example.com",
					MinVersion: version, MaxVersion: version,
				}
			}
			accepted := make(chan net.Conn, 2)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 2 {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					if tlsConn, ok := conn.(*tls.Conn); ok {
						_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
						if err := tlsConn.Handshake(); err != nil {
							conn.Close()
							t.Error(err)
							return
						}
						_ = conn.SetDeadline(time.Time{})
					}
					accepted <- conn
				}
			}()
			t.Cleanup(func() {
				listener.Close()
				wg.Wait()
				close(accepted)
				for conn := range accepted {
					conn.Close()
				}
			})
			adapter, err := newTCPAdapter(listener.Addr().String(), clientTLS, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			takeConnection := func() net.Conn {
				t.Helper()
				select {
				case conn := <-accepted:
					t.Cleanup(func() { conn.Close() })
					return conn
				case <-time.After(3 * time.Second):
					t.Fatal("server did not accept the connection")
					return nil
				}
			}
			peer := takeConnection()
			if err := peer.Close(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				adapter.mu.Lock()
				disconnected := adapter.conn == nil && adapter.err != nil
				adapter.mu.Unlock()
				if disconnected {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("idle peer shutdown was not detected without sending")
				}
				time.Sleep(time.Millisecond)
			}
			if err := adapter.SendMessage([]byte("after-close")); err != nil {
				t.Fatal(err)
			}
			replacement := takeConnection()
			_ = replacement.SetReadDeadline(time.Now().Add(2 * time.Second))
			data := make([]byte, len("after-close"))
			if _, err := io.ReadFull(replacement, data); err != nil {
				t.Fatal(err)
			}
			if string(data) != "after-close" {
				t.Fatalf("replacement received %q", data)
			}
		})
	}
}

func TestTCPStaleConnectionFailurePreservesReplacement(t *testing.T) {
	oldConn, peer := net.Pipe()
	defer oldConn.Close()
	defer peer.Close()
	adapter := &TCPAdapter{conn: oldConn}
	defer adapter.Close()
	adapter.monitorWG.Add(1)
	go adapter.monitorConnection(oldConn)
	replacement := &writeOnlyTestConn{}
	adapter.mu.Lock()
	adapter.conn = replacement
	adapter.mu.Unlock()
	peer.Close()
	adapter.monitorWG.Wait()
	if err := adapter.reconnect(oldConn); err != nil {
		t.Fatal(err)
	}
	if adapter.conn != replacement {
		t.Fatal("stale failure invalidated the replacement connection")
	}
	if err := adapter.SendMessage([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if replacement.written.String() != "replacement" {
		t.Fatal("message did not reach the replacement connection")
	}
}

func TestTCPMonitorStopsOnAdapterClose(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	adapter := &TCPAdapter{conn: conn}
	adapter.monitorWG.Add(1)
	go adapter.monitorConnection(conn)
	closed := make(chan error, 1)
	go func() { closed <- adapter.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("adapter close did not stop its connection monitor")
	}
}

func TestTLSMissingClientCertificateReconnectsAreRateLimited(t *testing.T) {
	certServer := httptest.NewTLSServer(nil)
	cert := certServer.TLS.Certificates[0]
	certServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	attempts := make(chan time.Time, 100)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			attempts <- time.Now()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_ = conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		wg.Wait()
	})
	interval := 60 * time.Millisecond
	adapter, err := newTCPAdapter(listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true, // Test server uses an ephemeral self-signed certificate.
		MinVersion:         tls.VersionTLS13,
	}, nil, interval)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	first := <-attempts
	previous := first
	checkAttempt := func(next time.Time) {
		t.Helper()
		if next.Sub(previous) < interval-time.Millisecond {
			t.Fatalf("reconnected after %s, want at least %s", next.Sub(previous), interval)
		}
		previous = next
	}
	// TLS 1.3 may return a successful client dial before receiving the server's
	// missing-client-certificate alert. Every redial still needs the cooldown.
	for range 10000 {
		_ = adapter.SendMessage([]byte("test"))
	}
	for len(attempts) > 0 {
		checkAttempt(<-attempts)
	}
	// A successful write is not a peer acknowledgment: the TLS alert/close
	// can take more than one write to become a local socket error.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case next := <-attempts:
			checkAttempt(next)
			return
		case <-tick.C:
			_ = adapter.SendMessage([]byte("test"))
		case <-deadline.C:
			t.Fatal("adapter never retried after the cooldown")
		}
	}
}

func TestFailedDialCooldownAcrossConcurrentSenders(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	adapter, err := newTCPAdapter(address, nil, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	lastDial := adapter.lastDial
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := adapter.SendMessage([]byte("test")); err == nil {
				t.Error("send unexpectedly succeeded")
			}
		}()
	}
	wg.Wait()
	if adapter.lastDial != lastDial {
		t.Fatal("failed startup dial was retried before tcpRetryInterval")
	}
}
