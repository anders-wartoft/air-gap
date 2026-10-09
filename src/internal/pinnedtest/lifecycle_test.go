package pinnedtest

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sitia.nu/airgap/src/protocol"
)

func pinnedClient(id, server identity, cache tls.ClientSessionCache) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{id.certificate}, ClientSessionCache: cache,
		InsecureSkipVerify: true, // Fixture authorization is exact SPKI, never CA/name/date.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 ||
				!server.key.PublicKey.Equal(state.PeerCertificates[0].PublicKey) {
				return fmt.Errorf("fixture rejected unexpected server key")
			}
			return nil
		},
	}
}

func dialPeer(address string, config *tls.Config) (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", address, config)
}

func sendProbe(conn *tls.Conn, marker string) error {
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	for _, frame := range protocol.FormatMessage(protocol.TYPE_CLEARTEXT, marker, []byte(marker), 1400) {
		if _, err := conn.Write(frame); err != nil {
			return err
		}
	}
	return nil
}

func requireProbe(t *testing.T, p *applicationProcess, conn *tls.Conn, marker string) {
	t.Helper()
	offset := len(p.read(t))
	if err := sendProbe(conn, marker); err != nil {
		t.Fatalf("authorized application write: %v", err)
	}
	p.wait(t, marker, offset)
}

func requireClosed(t *testing.T, conn *tls.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var buffer [1]byte
	_, err := conn.Read(buffer[:])
	if err == nil {
		t.Fatal("revoked connection unexpectedly delivered data")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("revoked connection remained open after completed reload")
	}
}

func requireRejected(t *testing.T, address string, config *tls.Config) {
	t.Helper()
	conn, err := dialPeer(address, config)
	if err != nil {
		if !strings.Contains(err.Error(), "remote error: tls:") &&
			!strings.Contains(err.Error(), "fixture rejected unexpected server key") {
			t.Fatalf("authentication rejection cannot be inferred from network failure: %v", err)
		}
		return
	}
	defer conn.Close()
	// A TLS 1.3 client can finish its handshake before receiving the server's
	// client-auth rejection. Read the alert instead of treating Dial as acceptance.
	requireClosed(t, conn)
}

func requireDial(t *testing.T, address string, config *tls.Config) *tls.Conn {
	t.Helper()
	conn, err := dialPeer(address, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func removePin(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name+".json")); err != nil {
		t.Fatal(err)
	}
}

func primeSession(t *testing.T, down *applicationProcess, address string, config *tls.Config) *tls.Conn {
	t.Helper()
	first := requireDial(t, address, config)
	requireProbe(t, down, first, "ticket-primer")
	// Read processes post-handshake tickets; application transport is one-way.
	if err := first.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var buffer [1]byte
	if _, err := first.Read(buffer[:]); err == nil {
		t.Fatal("unexpected application response")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("ticket primer connection failed: %v", err)
	}
	first.Close()
	resumed := requireDial(t, address, config)
	requireProbe(t, down, resumed, "resumed-before-revocation")
	if !resumed.ConnectionState().DidResume {
		t.Fatal("resumption control did not resume: revocation test would be a proxy")
	}
	return resumed
}

func TestTLSLifecycleFixtureControls(t *testing.T) {
	h := newApplicationHarness(t)
	server, client, unknown := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	t.Run("downstream_application_data_resumption_and_rejection", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "client", "client", client)
		down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\n")
		down.wait(t, "TLS TCP listener started", 0)
		config := pinnedClient(client, server, tls.NewLRUClientSessionCache(4))
		primeSession(t, down, h.address, config)
		requireRejected(t, h.address, pinnedClient(unknown, server, nil))
	})
	t.Run("upstream_authenticated_application_frames", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "server", "server", server)
		frames := runPeerServer(t, h.address, server, client)
		h.start(t, "upstream", properties(client, dir, false)+"source=random\npayloadSize=1400\neps=10\n")
		waitClientKey(t, frames, client)
	})
}

func TestPinnedTLSLifecycleDownstream(t *testing.T) {
	h := newApplicationHarness(t)
	server, a, a2, b := newIdentity(t, nil, false), newIdentity(t, nil, false),
		newIdentity(t, nil, false), newIdentity(t, nil, false)

	t.Run("TC27_05_staged_addition_requires_SIGHUP", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "a", "client", a)
		down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\n")
		down.wait(t, "TLS TCP listener started", 0)
		config := pinnedClient(a2, server, nil)
		entry(t, dir, "a2", "client", a2)
		final := filepath.Join(dir, "a2.json")
		staged := final + ".tmp"
		if err := os.Rename(final, staged); err != nil {
			t.Fatal(err)
		}
		requireRejected(t, h.address, config)
		requireReloadSuccess(t, down.reload(t))
		requireRejected(t, h.address, config)
		if err := os.Rename(staged, final); err != nil {
			t.Fatal(err)
		}
		requireRejected(t, h.address, config)
		requireReloadSuccess(t, down.reload(t))
		requireProbe(t, down, requireDial(t, h.address, config), "staged-key-now-authorized")
	})

	t.Run("TC27_06_invalid_candidates_are_atomic", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "a", "client", a)
		base := h.network + properties(server, dir, true) + "target=cmd\nmtu=1500\n"
		down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\n")
		down.wait(t, "TLS TCP listener started", 0)
		old := requireDial(t, h.address, pinnedClient(a, server, nil))
		requireProbe(t, down, old, "before-invalid-reloads")
		for _, test := range []struct {
			name, config, reason string
		}{
			{"missing_directory", base + "tcpTLSTrustedKeysDir=" + filepath.Join(t.TempDir(), "missing") + "\n", "missing"},
			{"mismatched_identity", base + "tcpTLSKeyFile=" + a.keyPath + "\n", "key"},
			{"mode_change", base + "tcpTLSAuthMode=ca\n", "mode"},
			{"unsupported_setting", base + "mtu=1400\n", "mtu"},
			{"malformed_config", base + "unrecognizedLifecycleSetting=value\n", "unrecognizedLifecycleSetting"},
			{"invalid_value", base + "targetPort=not-a-port\n", "targetPort"},
		} {
			t.Run(test.name, func(t *testing.T) {
				write(t, down.configPath, []byte(test.config))
				log := down.reload(t)
				if !strings.Contains(strings.ToLower(log), "failed") ||
					!strings.Contains(strings.ToLower(log), strings.ToLower(test.reason)) ||
					strings.Contains(log, "not implemented") {
					t.Errorf("candidate-specific reload failure required:\n%s", log)
				}
				requireProbe(t, down, old, "retained-"+test.name)
				requireProbe(t, down, requireDial(t, h.address, pinnedClient(a, server, nil)), "fresh-"+test.name)
			})
		}
		write(t, down.configPath, []byte(base))
		requireReloadSuccess(t, down.reload(t))
	})

	t.Run("TC27_06_12_empty_store_revokes_all", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "a", "client", a)
		down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\n")
		down.wait(t, "TLS TCP listener started", 0)
		config := pinnedClient(a, server, nil)
		old := requireDial(t, h.address, config)
		requireProbe(t, down, old, "before-empty-store")
		removePin(t, dir, "a")
		requireProbe(t, down, old, "removal-not-yet-active")
		requireReloadSuccess(t, down.reload(t))
		requireClosed(t, old)
		requireRejected(t, h.address, config)
	})

	t.Run("TC27_06_malformed_trust_retains_previous_snapshot", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "a", "client", a)
		down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\n")
		down.wait(t, "TLS TCP listener started", 0)
		old := requireDial(t, h.address, pinnedClient(a, server, nil))
		requireProbe(t, down, old, "before-malformed-trust")
		removePin(t, dir, "a")
		write(t, filepath.Join(dir, "broken.json"), []byte("{"))
		log := down.reload(t)
		if !strings.Contains(log, "failed") || !strings.Contains(log, "broken.json") ||
			strings.Contains(log, "not implemented") {
			t.Errorf("malformed-entry failure must identify the entry:\n%s", log)
		}
		requireProbe(t, down, old, "retained-after-malformed-trust")
		requireProbe(t, down, requireDial(t, h.address, pinnedClient(a, server, nil)), "fresh-after-malformed-trust")
	})

	t.Run("TC27_08_server_identity_and_trust_paths_reread", func(t *testing.T) {
		dir, replacementDir := t.TempDir(), t.TempDir()
		server2 := newIdentity(t, nil, false)
		entry(t, dir, "a", "client", a)
		entry(t, replacementDir, "a", "client", a)
		entry(t, replacementDir, "a2", "client", a2)
		down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\n")
		down.wait(t, "TLS TCP listener started", 0)
		requireProbe(t, down, requireDial(t, h.address, pinnedClient(a, server, nil)), "old-server-wrapper")
		write(t, down.configPath, []byte(h.network+properties(server2, replacementDir, true)+"target=cmd\nmtu=1500\n"))
		requireReloadSuccess(t, down.reload(t))
		requireProbe(t, down, requireDial(t, h.address, pinnedClient(a2, server2, nil)), "new-server-and-trust")
		requireRejected(t, h.address, pinnedClient(a, server, nil))
	})

	t.Run("TC27_08_12_overlap_revocation_and_resumption", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "a", "client", a)
		entry(t, dir, "b", "client", b)
		down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\n")
		down.wait(t, "TLS TCP listener started", 0)
		cache := tls.NewLRUClientSessionCache(4)
		config := pinnedClient(a, server, cache)
		resumed := primeSession(t, down, h.address, config)
		retained := requireDial(t, h.address, pinnedClient(b, server, nil))
		requireProbe(t, down, retained, "retained-peer-before")
		entry(t, dir, "a2", "client", a2)
		requireReloadSuccess(t, down.reload(t))
		replacement := requireDial(t, h.address, pinnedClient(a2, server, nil))
		requireProbe(t, down, replacement, "overlap-replacement")
		requireProbe(t, down, resumed, "overlap-old-key")
		removePin(t, dir, "a")
		requireReloadSuccess(t, down.reload(t))
		requireClosed(t, resumed)
		requireRejected(t, h.address, config)
		requireRejected(t, h.address, pinnedClient(a, server, nil))
		requireProbe(t, down, retained, "retained-peer-after")
		requireProbe(t, down, replacement, "replacement-after")
	})

	t.Run("TC27_06_downstream_environment_and_CLI_precedence", func(t *testing.T) {
		dir, unused := t.TempDir(), t.TempDir()
		server2 := newIdentity(t, nil, false)
		entry(t, dir, "a", "client", a)
		down := h.startWithEnvironment(t, "downstream", properties(server2, unused, true)+"target=cmd\nmtu=1500\n",
			[]string{
				"AIRGAP_DOWNSTREAM_TCP_TLS_CERT_FILE=" + server2.certPath,
				"AIRGAP_DOWNSTREAM_TCP_TLS_KEY_FILE=" + server2.keyPath,
				"AIRGAP_DOWNSTREAM_TCP_TLS_TRUSTED_KEYS_DIR=" + dir,
			}, "--tcpTLSCertFile="+server.certPath, "--tcpTLSKeyFile="+server.keyPath)
		down.wait(t, "TLS TCP listener started", 0)
		requireProbe(t, down, requireDial(t, h.address, pinnedClient(a, server, nil)), "downstream-overrides-before")
		write(t, down.configPath, []byte(h.network+properties(server2, unused, true)+"target=cmd\nmtu=1500\n"))
		requireReloadSuccess(t, down.reload(t))
		requireProbe(t, down, requireDial(t, h.address, pinnedClient(a, server, nil)), "downstream-overrides-after")
	})
}

type receivedFrame struct {
	conn        *tls.Conn
	closed      <-chan struct{}
	fingerprint string
	payload     string
	err         error
}

func spkiFingerprint(spki []byte) string {
	hash := sha256.Sum256(spki)
	return "SHA256:" + base64.StdEncoding.EncodeToString(hash[:])
}

// runPeerServer receives real application frames and records the authenticated
// client key. It is a fixture, not an implementation of key-exchange packets.
func runPeerServer(t *testing.T, address string, server identity, clients ...identity) <-chan receivedFrame {
	t.Helper()
	allowed := make(map[string]bool, len(clients))
	for _, client := range clients {
		allowed[client.fingerprint] = true
	}
	listener, err := tls.Listen("tcp", address, &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{server.certificate}, ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 ||
				!allowed[spkiFingerprint(state.PeerCertificates[0].RawSubjectPublicKeyInfo)] {
				return fmt.Errorf("fixture rejected unprovisioned client key")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan receivedFrame, 128)
	stop := make(chan struct{})
	var mu sync.Mutex
	connections := make(map[*tls.Conn]struct{})
	var handlers sync.WaitGroup
	accepted := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		listener.Close()
		<-accepted
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		handlers.Wait()
	})
	go func() {
		defer close(accepted)
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			conn := raw.(*tls.Conn)
			mu.Lock()
			connections[conn] = struct{}{}
			mu.Unlock()
			handlers.Add(1)
			closed := make(chan struct{})
			go func() {
				defer handlers.Done()
				defer close(closed)
				defer func() {
					conn.Close()
					mu.Lock()
					delete(connections, conn)
					mu.Unlock()
				}()
				cache := protocol.CreateMessageCache()
				for {
					conn.SetReadDeadline(time.Now().Add(3 * time.Second))
					header := make([]byte, 7)
					if _, err := io.ReadFull(conn, header); err != nil {
						return
					}
					idLength := int(header[5])<<8 | int(header[6])
					if idLength > 1024 {
						return
					}
					middle := make([]byte, idLength+6)
					if _, err := io.ReadFull(conn, middle); err != nil {
						return
					}
					size := int(middle[len(middle)-2])<<8 | int(middle[len(middle)-1])
					payload := make([]byte, size)
					if _, err := io.ReadFull(conn, payload); err != nil {
						return
					}
					message := append(append(header, middle...), payload...)
					kind, _, body, err := protocol.ParseMessage(message, cache)
					if err != nil {
						select {
						case frames <- receivedFrame{err: err}:
						case <-stop:
						}
						return
					}
					if !protocol.IsMessageType(kind, protocol.TYPE_CLEARTEXT) {
						continue
					}
					if protocol.IsMessageType(kind, protocol.TYPE_COMPRESSED_GZIP) {
						body, err = protocol.DecompressGzip(body, 65536)
						if err != nil {
							select {
							case frames <- receivedFrame{err: err}:
							case <-stop:
							}
							return
						}
					}
					state := conn.ConnectionState()
					if len(state.PeerCertificates) == 0 {
						return
					}
					select {
					case frames <- receivedFrame{
						conn: conn, closed: closed, fingerprint: spkiFingerprint(state.PeerCertificates[0].RawSubjectPublicKeyInfo),
						payload: string(body),
					}:
					case <-stop:
						return
					}
				}
			}()
		}
	}()
	return frames
}

func waitClientKey(t *testing.T, frames <-chan receivedFrame, id identity) receivedFrame {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case frame := <-frames:
			if frame.err != nil {
				t.Fatalf("invalid application frame: %v", frame.err)
			}
			if frame.fingerprint == id.fingerprint && strings.HasPrefix(frame.payload, "Random message ") {
				return frame
			}
		case <-timeout.C:
			t.Fatal("no application data authenticated by expected active client key")
		}
	}

}

func TestPinnedTLSLifecycleUpstream(t *testing.T) {
	h := newApplicationHarness(t)
	server, a, a2 := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	source := "source=random\npayloadSize=1400\neps=10\n"
	t.Run("TC27_08_active_identity_switch_reconnects", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "server", "server", server)
		frames := runPeerServer(t, h.address, server, a, a2)
		up := h.start(t, "upstream", properties(a, dir, false)+source)
		before := waitClientKey(t, frames, a)
		write(t, up.configPath, []byte(h.network+properties(a2, dir, false)+source))
		requireReloadSuccess(t, up.reload(t))
		after := waitClientKey(t, frames, a2)
		if before.conn == after.conn {
			t.Fatal("identity switch did not establish a new authenticated connection")
		}
		t.Logf("observed traffic across identity switch: %q -> %q; not a no-loss guarantee", before.payload, after.payload)
	})
	t.Run("TC27_06_12_server_removal_closes_connection", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "server", "server", server)
		frames := runPeerServer(t, h.address, server, a)
		up := h.start(t, "upstream", properties(a, dir, false)+source)
		before := waitClientKey(t, frames, a)
		removePin(t, dir, "server")
		requireReloadSuccess(t, up.reload(t))
		select {
		case <-before.closed:
		case <-time.After(time.Second):
			t.Fatal("upstream kept a revoked-server connection open after reload")
		}
		up.wait(t, "untrusted server key "+server.fingerprint, 0)
	})

	t.Run("TC27_08_identity_replaced_at_same_paths", func(t *testing.T) {
		dir := t.TempDir()
		local := newIdentity(t, nil, false)
		entry(t, dir, "server", "server", server)
		frames := runPeerServer(t, h.address, server, local, a2)
		up := h.start(t, "upstream", properties(local, dir, false)+source)
		waitClientKey(t, frames, local)
		for destination, original := range map[string]string{local.certPath: a2.certPath, local.keyPath: a2.keyPath} {
			data, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			write(t, destination+".tmp", data)
			if err := os.Rename(destination+".tmp", destination); err != nil {
				t.Fatal(err)
			}
		}
		requireReloadSuccess(t, up.reload(t))
		waitClientKey(t, frames, a2)
	})
	t.Run("TC27_06_environment_and_CLI_precedence_survive_reread", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "server", "server", server)
		frames := runPeerServer(t, h.address, server, a)
		up := h.startWithEnvironment(t, "upstream", properties(a2, dir, false)+source, []string{
			"AIRGAP_UPSTREAM_TCP_TLS_CERT_FILE=" + a2.certPath,
			"AIRGAP_UPSTREAM_TCP_TLS_KEY_FILE=" + a2.keyPath,
		},
			"--tcpTLSCertFile="+a.certPath, "--tcpTLSKeyFile="+a.keyPath)
		waitClientKey(t, frames, a)
		write(t, up.configPath, []byte(h.network+properties(a2, dir, false)+source))
		requireReloadSuccess(t, up.reload(t))
		waitClientKey(t, frames, a)
	})
	t.Run("TC27_06_environment_overrides_changed_file", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "server", "server", server)
		frames := runPeerServer(t, h.address, server, a)
		up := h.startWithEnvironment(t, "upstream", properties(a2, dir, false)+source, []string{
			"AIRGAP_UPSTREAM_TCP_TLS_CERT_FILE=" + a.certPath,
			"AIRGAP_UPSTREAM_TCP_TLS_KEY_FILE=" + a.keyPath,
		})
		waitClientKey(t, frames, a)
		write(t, up.configPath, []byte(h.network+properties(a2, dir, false)+source))
		requireReloadSuccess(t, up.reload(t))
		waitClientKey(t, frames, a)
	})

	t.Run("TC27_06_invalid_reload_retains_active_identity", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "server", "server", server)
		frames := runPeerServer(t, h.address, server, a)
		up := h.start(t, "upstream", properties(a, dir, false)+source)
		waitClientKey(t, frames, a)
		base := h.network + properties(a, dir, false) + source
		for _, test := range []struct {
			name, candidate, reason string
		}{
			{"mismatched_pair", base + "tcpTLSKeyFile=" + a2.keyPath + "\n", "key"},
			{"mode_change", base + "tcpTLSAuthMode=ca\n", "mode"},
			{"unsupported_setting", base + "eps=20\n", "eps"},
			{"malformed_config", base + "unrecognizedLifecycleSetting=value\n", "unrecognizedLifecycleSetting"},
			{"invalid_value", base + "eps=not-a-number\n", "eps"},
		} {
			t.Run(test.name, func(t *testing.T) {
				write(t, up.configPath, []byte(test.candidate))
				log := up.reload(t)
				if !strings.Contains(strings.ToLower(log), "failed") ||
					!strings.Contains(strings.ToLower(log), strings.ToLower(test.reason)) ||
					strings.Contains(log, "not implemented") {
					t.Errorf("candidate-specific failure required:\n%s", log)
				}
				waitClientKey(t, frames, a)
			})
		}
		write(t, up.configPath, []byte(base))
		if err := os.Remove(up.configPath); err != nil {
			t.Fatal(err)
		}
		log := up.reload(t)
		if !strings.Contains(log, "failed") || !strings.Contains(log, up.configPath) ||
			strings.Contains(log, "not implemented") {
			t.Errorf("missing configuration failure must identify original path:\n%s", log)
		}
		waitClientKey(t, frames, a)
	})
}
