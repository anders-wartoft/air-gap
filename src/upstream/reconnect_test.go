package upstream

import (
	"crypto/tls"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

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
	time.Sleep(interval)
	_ = adapter.SendMessage([]byte("test"))
	select {
	case next := <-attempts:
		checkAttempt(next)
	case <-time.After(time.Second):
		t.Fatal("adapter never retried after the cooldown")
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
