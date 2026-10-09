package pinnedtest

import (
	"bytes"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedTLSIdleAgingDownstream(t *testing.T) {
	h := newApplicationHarness(t)
	server, client, unused := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	dir := t.TempDir()
	entry(t, dir, "client", "client", client)
	entry(t, dir, "overlap", "client", unused)
	path := filepath.Join(dir, "overlap.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\ntcpTLSPinIdleSeconds=2\n")
	down.wait(t, "TLS TCP listener started", 0)
	cached := pinnedClient(unused, server, tls.NewLRUClientSessionCache(4))
	overlap := primeSession(t, down, h.address, cached)
	if err := overlap.Close(); err != nil {
		t.Fatal(err)
	}
	active := requireDial(t, h.address, pinnedClient(client, server, nil))
	requireProbe(t, down, active, "active-key-before-aging")
	down.wait(t, "purged=["+unused.fingerprint+"]", 0)
	requireProbe(t, down, active, "active-key-after-aging")
	requireRejected(t, h.address, cached)
	requireRejected(t, h.address, pinnedClient(unused, server, nil))
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("aging changed disk: %v", err)
	}
	originalConfig, err := os.ReadFile(down.configPath)
	if err != nil {
		t.Fatal(err)
	}
	write(t, down.configPath, bytes.ReplaceAll(originalConfig, []byte("tcpTLSPinIdleSeconds=2"), []byte("tcpTLSPinIdleSeconds=3")))
	if log := down.reload(t); !strings.Contains(log, "setting tcpTLSPinIdleSeconds changed; restart required") {
		t.Fatalf("live aging-policy change was not rejected: %s", log)
	}
	requireRejected(t, h.address, cached)
	requireProbe(t, down, active, "active-key-after-failed-reload")
	write(t, down.configPath, originalConfig)
	requireReloadSuccess(t, down.reload(t))
	requireProbe(t, down, requireDial(t, h.address, cached), "aged-key-restored-by-SIGHUP")
}

func TestPinnedTLSIdleAgingUpstream(t *testing.T) {
	h := newApplicationHarness(t)
	client, server, overlap := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	dir := t.TempDir()
	entry(t, dir, "server", "server", server)
	entry(t, dir, "overlap", "server", overlap)
	props := properties(client, dir, false) + "source=random\npayloadSize=1400\neps=10\ntcpTLSPinIdleSeconds=2\n"
	up := h.start(t, "upstream", props)
	t.Run("original_server_kept_while_connected", func(t *testing.T) {
		frames := runPeerServer(t, h.address, server, client)
		waitClientKey(t, frames, client)
		up.wait(t, "purged=["+overlap.fingerprint+"]", 0)
		waitClientKey(t, frames, client)
	})
	t.Run("aged_server_rejected_then_restored", func(t *testing.T) {
		frames := runPeerServer(t, h.address, overlap, client)
		up.wait(t, "untrusted server key "+overlap.fingerprint, 0)
		requireReloadSuccess(t, up.reload(t))
		waitClientKey(t, frames, client)
		data, err := os.ReadFile(filepath.Join(dir, "overlap.json"))
		if err != nil || !strings.Contains(string(data), overlap.fingerprint) {
			t.Fatalf("server aging changed disk trust: %v", err)
		}
	})
}
