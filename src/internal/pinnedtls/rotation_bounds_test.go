package pinnedtls

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sitia.nu/airgap/src/protocol"
)

func TestRotationPayloadBoundary(t *testing.T) {
	_, _, request, _, _ := rotationFixture(t)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{MaxRotationPayload - 1, MaxRotationPayload, MaxRotationPayload + 1} {
		payload := append(append([]byte{}, data...), bytes.Repeat([]byte(" "), size-len(data))...)
		frame := protocol.FormatMessage(protocol.TYPE_KEY_EXCHANGE, RotationMessageID, payload, 16384)[0]
		_, err := DecodeRotationFrame(frame)
		if (err != nil) != (size > MaxRotationPayload) {
			t.Fatalf("payload size=%d error=%v", size, err)
		}
	}
	for _, fingerprint := range []string{"SHA256:" + strings.Repeat("x", 44), request.New + "\n", "SHA256:AAAA"} {
		if validFingerprint(fingerprint) {
			t.Fatal("noncanonical fingerprint accepted")
		}
	}
}

func TestRotationJournalCountBoundary(t *testing.T) {
	for _, count := range []int{MaxRotationRecords - 1, MaxRotationRecords, MaxRotationRecords + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			manager, dir, request, _, _ := rotationFixture(t)
			records := make([]rotationRecord, count)
			for i := range records {
				hash := sha256.Sum256([]byte(fmt.Sprint(i)))
				old := "SHA256:" + base64.StdEncoding.EncodeToString(hash[:])
				records[i] = rotationRecord{rotationID(old, request.New), fmt.Sprintf("retired-peer-%d", i), old, request.New, "committed"}
			}
			if err := atomicJSON(filepath.Join(dir, ".pinned-rotations.json"), records); err != nil {
				t.Fatal(err)
			}
			err := manager.CommitRotation(request, dir, true)
			if (err == nil) != (count < MaxRotationRecords) {
				t.Fatalf("journal count=%d rotation error=%v", count, err)
			}
			if count >= MaxRotationRecords {
				if _, err := os.Stat(filepath.Join(dir, "rotation-"+request.ID+".json")); !os.IsNotExist(err) {
					t.Fatal("capacity rejection installed a partial authorization")
				}
			}
		})
	}
}

func TestPublicEntryPublicationNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "occupied.json")
	before := []byte("administrator-owned-content")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := installPublicEntry(path, []byte("replacement")); err == nil {
		t.Fatal("occupied public entry overwritten")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("original entry changed: %v", err)
	}
}
