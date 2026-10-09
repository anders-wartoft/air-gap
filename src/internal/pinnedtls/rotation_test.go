package pinnedtls

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sitia.nu/airgap/src/protocol"
)

func rotationFixture(t *testing.T) (*Manager, string, RotationMessage, *Snapshot, *Snapshot) {
	t.Helper()
	identities := t.TempDir()
	if err := generate(identities, "identity", "client", 3, io.Discard); err != nil {
		t.Fatal(err)
	}
	trust := t.TempDir()
	get := func(number string) *Snapshot {
		s, err := Prepare(filepath.Join(identities, "identity-"+number+".crt"),
			filepath.Join(identities, "identity-"+number+".key"), trust, true)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	old, next := get("1"), get("2")
	spki := old.config.Certificates[0].Leaf.RawSubjectPublicKeyInfo
	install(t, trust, "old", Entry{1, "peer-a", "client", string(pemPublic(spki)), old.Identity()})
	old = get("1")
	manager, err := NewManager(old)
	if err != nil {
		t.Fatal(err)
	}
	message := RotationMessage{Version: 1, Kind: "request", Old: old.Identity(), New: next.Identity(),
		Peer: "peer-a", Nonce: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		Public: base64.StdEncoding.EncodeToString(next.config.Certificates[0].Leaf.RawSubjectPublicKeyInfo)}
	message.ID = rotationID(message.Old, message.New)
	if err := SignRotation(&message, next.config.Certificates[0].PrivateKey.(*ecdsa.PrivateKey)); err != nil {
		t.Fatal(err)
	}
	return manager, trust, message, old, next
}

func TestRotationWireAndProof(t *testing.T) {
	_, _, message, _, _ := rotationFixture(t)
	var wire bytes.Buffer
	if err := WriteRotation(&wire, message); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadRotation(bytes.NewReader(wire.Bytes()))
	if err != nil || decoded != message {
		t.Fatalf("wire round trip: %v", err)
	}
	if _, err := verifyRotation(decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"kind", "id", "old", "new", "peer", "nonce", "public", "reason", "proof", "version"} {
		t.Run(field, func(t *testing.T) {
			modified := message
			switch field {
			case "kind":
				modified.Kind = "begin"
			case "id":
				modified.ID = strings.Repeat("a", 64)
			case "old":
				modified.Old += "x"
			case "new":
				modified.New += "x"
			case "peer":
				modified.Peer = "peer-b"
			case "nonce":
				modified.Nonce = "different"
			case "public":
				modified.Public = "AAAA"
			case "reason":
				modified.Reason = "untrusted"
			case "proof":
				modified.Proof = "AAAA"
			case "version":
				modified.Version++
			}
			if _, err := verifyRotation(modified); err == nil {
				t.Fatal("altered signed request accepted")
			}
		})
	}
	for _, payload := range [][]byte{
		[]byte(`{"version":1,"version":1}`),
		[]byte(`{"unexpected":"value"}`),
		[]byte(`{} {}`),
		bytes.Repeat([]byte(" "), MaxRotationPayload+1),
	} {
		frame := protocol.FormatMessage(protocol.TYPE_KEY_EXCHANGE, RotationMessageID, payload, 16384)[0]
		if _, err := DecodeRotationFrame(frame); err == nil {
			t.Fatal("malformed/oversized payload accepted")
		}
	}
	frame := append([]byte{}, wire.Bytes()...)
	frame[4] = 2
	if _, err := DecodeRotationFrame(frame); err == nil {
		t.Fatal("multipart control accepted")
	}
}

func TestRotationPolicyDurabilityAndReplay(t *testing.T) {
	manager, dir, request, old, _ := rotationFixture(t)
	if err := manager.CommitRotation(request, dir, false); err == nil {
		t.Fatal("disabled automatic policy accepted unprovisioned replacement")
	}
	if _, err := os.Stat(filepath.Join(dir, ".pinned-rotations.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected request changed disk: %v", err)
	}
	if err := manager.CommitRotation(request, dir, true); err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitRotation(request, dir, false); err != nil {
		t.Fatalf("identical durable retry failed: %v", err)
	}
	// Reconstruct service state only from original persistent files.
	pins, err := loadTrust(dir, "client")
	if err != nil || len(pins) != 2 {
		t.Fatalf("durable installation: %v %d", err, len(pins))
	}
	snapshot := *old
	snapshot.pins = pins
	restarted, err := NewManager(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.CommitRotation(request, dir, false); err != nil {
		t.Fatalf("restart retry failed: %v", err)
	}
	remove := filepath.Join(dir, "rotation-"+request.ID+".json")
	if err := os.Remove(remove); err != nil {
		t.Fatal(err)
	}
	if err := restarted.CommitRotation(request, dir, true); err == nil {
		t.Fatal("retry resurrected removed pin")
	}
	if _, err := os.Stat(remove); !os.IsNotExist(err) {
		t.Fatal("removed public entry reconstructed")
	}
}

func TestRotationConcurrentRetriesAndInvalidStorage(t *testing.T) {
	manager, dir, request, _, _ := rotationFixture(t)
	var workers sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < cap(errors); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errors <- manager.CommitRotation(request, dir, true)
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	records, err := readRecords(dir)
	if err != nil || len(records) != 1 {
		t.Fatalf("duplicate durable records: %v %d", err, len(records))
	}
	if err := os.Chmod(filepath.Join(dir, ".pinned-rotations.json"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitRotation(request, dir, true); err == nil {
		t.Fatal("unsafe replay storage accepted")
	}
}

func TestClientRecoveryRetainsOldPrivateIdentity(t *testing.T) {
	_, _, _, old, next := rotationFixture(t)
	dir := t.TempDir()
	// Client trust uses server role; recovery stores a local identity, not a pin.
	spki := old.config.Certificates[0].Leaf.RawSubjectPublicKeyInfo
	install(t, dir, "server", Entry{1, "server", "server", string(pemPublic(spki)), old.Identity()})
	if err := SavePendingClient(dir, old.config, next); err != nil {
		t.Fatal(err)
	}
	restored, pending, err := RestorePendingClient(dir, next)
	if err != nil || !pending || restored.Identity() != old.Identity() {
		t.Fatalf("restart did not preserve old identity: pending=%t error=%v", pending, err)
	}
	if _, _, err := RestorePendingClient(dir, old); err == nil {
		t.Fatal("different configured replacement accepted during recovery")
	}
	if err := ClearPendingClient(dir); err != nil {
		t.Fatal(err)
	}
	restored, pending, err = RestorePendingClient(dir, next)
	if err != nil || pending || restored.Identity() != next.Identity() {
		t.Fatal("accepted replacement not restored after completion")
	}
}

func TestRotationLegacyFrameIsolation(t *testing.T) {
	payload := []byte("KEY_UPDATE#legacy-opaque-RSA-ciphertext")
	frames := protocol.FormatMessage(protocol.TYPE_KEY_EXCHANGE, "KEY_UPDATE#", payload, 1500)
	if IsRotationFrame(frames[0]) {
		t.Fatal("legacy symmetric exchange mistaken for pinned rotation")
	}

	kind, id, data, err := protocol.ParseMessage(frames[0], protocol.CreateMessageCache())
	if err != nil || kind != protocol.TYPE_KEY_EXCHANGE || id != "KEY_UPDATE#" || !bytes.Equal(data, payload) {
		t.Fatalf("legacy framing changed: %v", err)
	}
}

func TestRotationPersistenceFailureRecovery(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		name := "intent"
		if failAt == 2 {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			manager, dir, request, old, _ := rotationFixture(t)
			calls := 0
			manager.persistRotation = func(path string, value any) error {
				calls++
				if calls == failAt {
					return errors.New("injected fsync failure")
				}
				return atomicJSON(path, value)
			}
			if err := manager.CommitRotation(request, dir, true); err == nil {
				t.Fatal("persistence failure reported acceptance")
			}
			if _, exists := manager.active.pins[request.New]; exists {
				t.Fatal("failed commit activated new trust")
			}
			pins, err := loadTrust(dir, "client")
			if err != nil {
				t.Fatal(err)
			}
			snapshot := *old
			snapshot.pins = pins
			restarted, err := NewManager(&snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.CommitRotation(request, dir, true); err != nil {
				t.Fatalf("restart failed to reconcile persistence interruption: %v", err)
			}
			records, err := readRecords(dir)
			if err != nil || len(records) != 1 || records[0].State != "committed" {
				t.Fatalf("recovery did not converge: %+v %v", records, err)
			}
		})
	}
}

func TestRotationConflictingReplacementAndRevokedOld(t *testing.T) {
	manager, dir, request, old, _ := rotationFixture(t)
	if err := manager.CommitRotation(request, dir, true); err != nil {
		t.Fatal(err)
	}
	// Independently generate a competing replacement and sign its bound request.
	keys := t.TempDir()
	if err := generate(keys, "third", "client", 1, io.Discard); err != nil {
		t.Fatal(err)
	}
	third, err := Prepare(filepath.Join(keys, "third-1.crt"), filepath.Join(keys, "third-1.key"), dir, true)
	if err != nil {
		t.Fatal(err)
	}
	conflict := request
	conflict.New = third.Identity()
	conflict.ID = rotationID(conflict.Old, conflict.New)
	conflict.Public = base64.StdEncoding.EncodeToString(third.config.Certificates[0].Leaf.RawSubjectPublicKeyInfo)
	if err := SignRotation(&conflict, third.config.Certificates[0].PrivateKey.(*ecdsa.PrivateKey)); err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitRotation(conflict, dir, true); err == nil {
		t.Fatal("competing replacement accepted")
	}
	if err := os.Remove(filepath.Join(dir, "old.json")); err != nil {
		t.Fatal(err)
	}
	snapshot := *old
	snapshot.pins, err = loadTrust(dir, "client")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(&snapshot); err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitRotation(request, dir, true); err == nil {
		t.Fatal("revoked old key authorized retry")
	}
}
