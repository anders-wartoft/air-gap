package downstream

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"sitia.nu/airgap/src/internal/pinnedtls"
)

func idleTestConnection(t *testing.T, mode string) (net.Conn, <-chan []byte) {
	t.Helper()
	receiver := &TCPAdapter{maxConnections: 10}
	var clientConfig *tls.Config
	switch mode {
	case "mtls":
		server := httptest.NewTLSServer(nil)
		cert := server.TLS.Certificates[0]
		server.Close()
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
		cert.Certificate[0], err = x509.CreateCertificate(rand.Reader, leaf, leaf, leaf.PublicKey, cert.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		cert.Leaf = nil
		leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(leaf)
		receiver.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    roots,
			MinVersion:   tls.VersionTLS13,
		}
		clientConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      roots,
			ServerName:   "example.com",
			MinVersion:   tls.VersionTLS13,
		}
	case "pinned":
		dir := t.TempDir()
		for _, role := range []string{"client", "server"} {
			handled, err := pinnedtls.GenerateCommand([]string{
				"--generate-tls-key", "--tls-key-output-dir=" + dir,
				"--tls-key-name=" + role, "--tls-key-role=" + role,
				"--json-file-dir=" + filepath.Join(dir, role+"-trust"),
			}, role, io.Discard)
			if err != nil || !handled {
				t.Fatalf("generate %s: handled=%v error=%v", role, handled, err)
			}
		}
		config := defaultConfiguration()
		config.tcpTLSAuthMode = "pinned"
		config.tcpTLSCertFile = filepath.Join(dir, "server-1.crt")
		config.tcpTLSKeyFile = filepath.Join(dir, "server-1.key")
		config.tcpTLSTrustedKeysDir = filepath.Join(dir, "client-trust")
		receiver = NewTCPAdapter(config)
		snapshot, err := pinnedtls.Prepare(filepath.Join(dir, "client-1.crt"),
			filepath.Join(dir, "client-1.key"), filepath.Join(dir, "server-trust"), false)
		if err != nil {
			t.Fatal(err)
		}
		manager, err := pinnedtls.NewManager(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		clientConfig = manager.Config()
	}
	received := make(chan []byte, 2)
	receiver.Listen("127.0.0.1", 0, 0, func(msg []byte) { received <- msg }, 1500, make(chan struct{}), 1)
	t.Cleanup(func() { receiver.Close() })
	var conn net.Conn
	var err error
	if clientConfig == nil {
		conn, err = net.DialTimeout("tcp", receiver.listener.Addr().String(), 2*time.Second)
	} else {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second},
			"tcp", receiver.listener.Addr().String(), clientConfig)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, received
}

func TestTCPConnectionSurvivesIdleBetweenMessages(t *testing.T) {
	for _, mode := range []string{"plain", "mtls", "pinned"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			conn, received := idleTestConnection(t, mode)
			frame := []byte{0, 0, 1, 0, 1, 0, 1, 'x', 0, 0, 0, 0, 0, 1, 'y'}
			send := func() {
				t.Helper()
				if _, err := conn.Write(frame); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-received:
					if !bytes.Equal(got, frame) {
						t.Fatalf("received %x, want %x", got, frame)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("message not delivered on the original connection")
				}
			}
			send()
			time.Sleep(6 * time.Second)
			send()
		})
	}
}

func TestTCPIncompleteMessageStillTimesOut(t *testing.T) {
	for _, mode := range []string{"plain", "mtls", "pinned"} {
		for _, part := range []string{"header", "payload"} {
			t.Run(mode+"/"+part, func(t *testing.T) {
				t.Parallel()
				conn, received := idleTestConnection(t, mode)
				frame := []byte{0}
				if part == "payload" {
					frame = []byte{0, 0, 1, 0, 1, 0, 1, 'x', 0, 0, 0, 0, 0, 1}
				}
				start := time.Now()
				if _, err := conn.Write(frame); err != nil {
					t.Fatal(err)
				}
				if err := conn.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
					t.Fatal(err)
				}
				var b [1]byte
				_, err := conn.Read(b[:])
				if err == nil {
					t.Fatal("incomplete frame unexpectedly produced a response")
				}
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Fatalf("downstream did not close the incomplete frame: %v", err)
				}
				if time.Since(start) < 4*time.Second {
					t.Fatalf("connection closed before the frame deadline: %v", err)
				}
				select {
				case <-received:
					t.Fatal("incomplete frame delivered")
				default:
				}
			})
		}
	}
}

func TestTCPMessageDeadlineStartsAfterIdle(t *testing.T) {
	for _, mode := range []string{"plain", "mtls", "pinned"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			conn, received := idleTestConnection(t, mode)
			frame := []byte{0, 0, 1, 0, 1, 0, 1, 'x', 0, 0, 0, 0, 0, 1, 'y'}
			time.Sleep(4 * time.Second)
			if _, err := conn.Write(frame[:1]); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second)
			if _, err := conn.Write(frame[1:]); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-received:
				if !bytes.Equal(got, frame) {
					t.Fatalf("received %x, want %x", got, frame)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("message inherited the idle polling deadline")
			}
		})
	}
}
