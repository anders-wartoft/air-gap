package pinnedtest

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sitia.nu/airgap/src/internal/pinnedtls"
)

func rotationBegin(old, next identity) pinnedtls.RotationMessage {
	hash := sha256.Sum256([]byte("airgap/pinned-rotation/id/v1\n" + old.fingerprint + "\n" + next.fingerprint))
	return pinnedtls.RotationMessage{Version: 1, Kind: "begin", ID: hex.EncodeToString(hash[:]), Old: old.fingerprint, New: next.fingerprint}
}

func rotationRequest(t *testing.T, conn *tls.Conn, old, next identity) pinnedtls.RotationMessage {
	t.Helper()
	begin := rotationBegin(old, next)
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := pinnedtls.WriteRotation(conn, begin); err != nil {
		t.Fatal(err)
	}
	challenge, err := pinnedtls.ReadRotation(conn)
	if err != nil || challenge.Kind != "challenge" {
		t.Fatalf("challenge: %+v %v", challenge, err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&next.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	challenge.Kind, challenge.Public = "request", base64.StdEncoding.EncodeToString(spki)
	if err := pinnedtls.SignRotation(&challenge, next.key); err != nil {
		t.Fatal(err)
	}
	return challenge
}

func stopApplication(t *testing.T, p *applicationProcess) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatal(p.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("application did not stop for restart test")
	}
}

func TestPinnedTLSRotationLostAcknowledgmentAndRevocation(t *testing.T) {
	h := newApplicationHarness(t)
	old, next, server := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	dir := t.TempDir()
	entry(t, dir, "old", "client", old)
	props := properties(server, dir, true) + "target=cmd\nmtu=1500\ntcpTLSAutomaticRotation=true\n"
	down := h.start(t, "downstream", props)
	down.wait(t, "TLS TCP listener started", 0)
	conn := requireDial(t, h.address, pinnedClient(old, server, nil))
	request := rotationRequest(t, conn, old, next)
	if err := pinnedtls.WriteRotation(conn, request); err != nil {
		t.Fatal(err)
	}
	// Observe server commit, but deliberately do not consume its acceptance.
	down.wait(t, "result=accepted role=client peer=peer id="+request.ID, 0)
	conn.Close()
	stopApplication(t, down)
	down = h.start(t, "downstream", props)
	down.wait(t, "TLS TCP listener started", 0)
	conn = requireDial(t, h.address, pinnedClient(old, server, nil))
	retry := rotationRequest(t, conn, old, next)
	if retry.Nonce == request.Nonce {
		t.Fatal("restart did not issue a fresh challenge")
	}
	if err := pinnedtls.WriteRotation(conn, retry); err != nil {
		t.Fatal(err)
	}
	response, err := pinnedtls.ReadRotation(conn)
	if err != nil || response.Kind != "accepted" || response.ID != request.ID {
		t.Fatalf("lost-ack restart retry: %+v %v", response, err)
	}
	requireProbe(t, down, requireDial(t, h.address, pinnedClient(next, server, nil)), "replacement-after-restart")
	if err := os.Remove(filepath.Join(dir, "rotation-"+request.ID+".json")); err != nil {
		t.Fatal(err)
	}
	requireReloadSuccess(t, down.reload(t))
	conn2 := requireDial(t, h.address, pinnedClient(old, server, nil))
	retry = rotationRequest(t, conn2, old, next)
	if err := pinnedtls.WriteRotation(conn2, retry); err != nil {
		t.Fatal(err)
	}
	response, err = pinnedtls.ReadRotation(conn2)
	if err != nil || response.Kind != "rejected" || !strings.Contains(response.Reason, "absent") {
		t.Fatalf("removed pin retry: %+v %v", response, err)
	}
	requireRejected(t, h.address, pinnedClient(next, server, nil))
	stopApplication(t, down)
	down = h.start(t, "downstream", props)
	down.wait(t, "TLS TCP listener started", 0)
	requireRejected(t, h.address, pinnedClient(next, server, nil))
}

func TestPinnedTLSRotationProofAndChallengeRejection(t *testing.T) {
	h := newApplicationHarness(t)
	old, next, server := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	dir := t.TempDir()
	entry(t, dir, "old", "client", old)
	down := h.start(t, "downstream", properties(server, dir, true)+"target=cmd\nmtu=1500\ntcpTLSAutomaticRotation=true\n")
	down.wait(t, "TLS TCP listener started", 0)
	for _, name := range []string{"tampered-proof", "cross-peer", "cross-connection"} {
		t.Run(name, func(t *testing.T) {
			conn := requireDial(t, h.address, pinnedClient(old, server, nil))
			request := rotationRequest(t, conn, old, next)
			switch name {
			case "tampered-proof":
				request.Proof = "AAAA"
			case "cross-peer":
				request.Peer = "another-peer"
				if err := pinnedtls.SignRotation(&request, next.key); err != nil {
					t.Fatal(err)
				}
			case "cross-connection":
				conn = requireDial(t, h.address, pinnedClient(old, server, nil))
			}
			if err := pinnedtls.WriteRotation(conn, request); err != nil {
				t.Fatal(err)
			}
			response, err := pinnedtls.ReadRotation(conn)
			if err != nil || response.Kind != "rejected" {
				t.Fatalf("invalid request accepted: %+v %v", response, err)
			}
			requireRejected(t, h.address, pinnedClient(next, server, nil))
		})
	}
}

func TestPinnedTLSRotationServices(t *testing.T) {
	h := newApplicationHarness(t)
	for _, test := range []struct {
		policy        string
		preauthorized bool
	}{
		{"false", false}, {"true", false}, {"false", true},
	} {
		t.Run("automatic="+test.policy+"/preauthorized="+strings.ToLower(fmt.Sprint(test.preauthorized)), func(t *testing.T) {
			client, replacement, server := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
			clients, servers := t.TempDir(), t.TempDir()
			entry(t, clients, "client", "client", client)
			if test.preauthorized {
				entry(t, clients, "replacement", "client", replacement)
			}
			entry(t, servers, "server", "server", server)
			down := h.start(t, "downstream", properties(server, clients, true)+
				"target=cmd\nmtu=1500\ntcpTLSAutomaticRotation="+test.policy+"\n")
			down.wait(t, "TLS TCP listener started", 0)
			source := "source=random\npayloadSize=1400\neps=10\ntcpTLSRotationEnabled=true\n"
			up := h.start(t, "upstream", properties(client, servers, false)+source)
			down.wait(t, "Random message 2", 0)
			write(t, up.configPath, []byte(h.network+properties(replacement, servers, false)+source))
			log := up.reload(t)
			if test.policy == "false" && !test.preauthorized {
				if !strings.Contains(log, "rotation rejected") {
					t.Fatalf("disabled rotation needs explicit rejection:\n%s", log)
				}
				down.wait(t, "Random message 5", 0)
				files, err := filepath.Glob(filepath.Join(clients, "*.json"))
				if err != nil || len(files) != 1 {
					t.Fatalf("rejected request changed trust: files=%v error=%v", files, err)
				}
				return
			}
			requireReloadSuccess(t, log)
			down.wait(t, "fingerprint="+replacement.fingerprint, 0)
			down.wait(t, "Random message 5", 0)
			files, err := filepath.Glob(filepath.Join(clients, "rotation-*.json"))
			if test.preauthorized {
				if err != nil || len(files) != 0 {
					t.Fatalf("preauthorized confirmation rewrote trust: %v %v", files, err)
				}
				return
			}
			if err != nil || len(files) != 1 {
				t.Fatalf("acceptance without one durable public entry: %v %v", files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil || !strings.Contains(string(data), replacement.fingerprint) ||
				strings.Contains(string(data), "PRIVATE KEY") {
				t.Fatalf("invalid public installation: %v", err)
			}
		})
	}
}
