package pinnedtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedTLSClientRotationRestartRecovery(t *testing.T) {
	h := newApplicationHarness(t)
	old, next, server := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	clients, servers := t.TempDir(), t.TempDir()
	entry(t, clients, "old", "client", old)
	entry(t, servers, "server", "server", server)
	downProps := properties(server, clients, true) + "target=cmd\nmtu=1500\n"
	down := h.start(t, "downstream", downProps)
	down.wait(t, "TLS TCP listener started", 0)
	source := "source=random\npayloadSize=1400\neps=10\ntcpTLSRotationEnabled=true\n"
	up := h.start(t, "upstream", properties(old, servers, false)+source)
	down.wait(t, "Random message 2", 0)
	write(t, up.configPath, []byte(h.network+properties(next, servers, false)+source))
	if log := up.reload(t); !strings.Contains(log, "rotation rejected") {
		t.Fatalf("unprovisioned default policy was not rejected:\n%s", log)
	}
	state := filepath.Join(servers, ".pinned-client-rotation.json")
	info, err := os.Stat(state)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private recovery identity not persisted owner-only: %v", err)
	}
	stopApplication(t, up)
	stopApplication(t, down)
	down = h.start(t, "downstream", downProps+"tcpTLSAutomaticRotation=true\n")
	down.wait(t, "TLS TCP listener started", 0)
	up = h.start(t, "upstream", properties(next, servers, false)+source)
	up.wait(t, "Pinned rotation recovery: retained old identity", 0)
	down.wait(t, "fingerprint="+old.fingerprint, 0)
	requireReloadSuccess(t, up.reload(t))
	down.wait(t, "fingerprint="+next.fingerprint, 0)
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("accepted rotation retained pending state: %v", err)
	}
	stopApplication(t, up)
	up = h.start(t, "upstream", properties(next, servers, false)+source)
	up.wait(t, "Connected to TCP server", 0)
	if strings.Contains(up.read(t), "retained old identity") {
		t.Fatal("completed client restart incorrectly restored old key")
	}
}
