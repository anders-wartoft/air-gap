package pinnedtest

import (
	"crypto/tls"
	"testing"
	"time"

	"sitia.nu/airgap/src/internal/pinnedtls"
)

func TestPinnedTLSAdmissionAtRevocationBoundary(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "retained"
		if revoke {
			name = "revoked"
		}
		t.Run(name, func(t *testing.T) {
			server, client := newIdentity(t, nil, false), newIdentity(t, nil, false)
			dir := t.TempDir()
			entry(t, dir, "client", "client", client)
			snapshot, err := pinnedtls.Prepare(server.certPath, server.keyPath, dir, true)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := pinnedtls.NewManager(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			staleConfig := manager.Config()
			listener, err := tls.Listen("tcp", "127.0.0.1:0", manager.ServerConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			handshake := make(chan *tls.Conn, 1)
			failures := make(chan error, 1)
			go func() {
				raw, err := listener.Accept()
				if err != nil {
					failures <- err
					return
				}
				conn := raw.(*tls.Conn)
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				if err := conn.Handshake(); err != nil {
					conn.Close()
					failures <- err
					return
				}
				handshake <- conn
			}()
			peer := requireDial(t, listener.Addr().String(), pinnedClient(client, server, nil))
			var serverConn *tls.Conn
			select {
			case serverConn = <-handshake:
			case err := <-failures:
				t.Fatal(err)
			case <-time.After(3 * time.Second):
				t.Fatal("handshake did not reach admission boundary")
			}
			defer serverConn.Close()
			if revoke {
				removePin(t, dir, "client")
			}
			next, err := pinnedtls.Prepare(server.certPath, server.keyPath, dir, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Activate(next); err != nil {
				t.Fatal(err)
			}
			_, err = manager.Admit(serverConn)
			if (err != nil) != revoke {
				t.Fatalf("post-reload admission: revoke=%t error=%v", revoke, err)
			}
			if err := staleConfig.VerifyConnection(serverConn.ConnectionState()); (err != nil) != revoke {
				t.Fatalf("old verifier retained stale authorization: %v", err)
			}
			if revoke {
				serverConn.Close()
				requireClosed(t, peer)
				return
			}
			// Once admitted, later revocation must close this exact session.
			removePin(t, dir, "client")
			next, err = pinnedtls.Prepare(server.certPath, server.keyPath, dir, true)
			if err != nil {
				t.Fatal(err)
			}
			result, err := manager.Activate(next)
			if err != nil || result.Disconnected != 1 {
				t.Fatalf("active-session revocation: disconnected=%d error=%v", result.Disconnected, err)
			}
			requireClosed(t, peer)
		})
	}
}
