package pinnedtls

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func fixtureEntry(t *testing.T) Entry {
	t.Helper()
	dir := t.TempDir()
	if err := generate(dir, "identity", "client", 1, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(filepath.Join(dir, "identity-1.pub"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := os.ReadFile(filepath.Join(dir, "identity-1.fingerprint"))
	if err != nil {
		t.Fatal(err)
	}
	return Entry{1, "sender", "client", string(public), strings.TrimSpace(string(fingerprint))}
}

func install(t *testing.T, dir, name string, entry Entry) string {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTrustIgnoresOtherEndpointRole(t *testing.T) {
	entry := fixtureEntry(t) // role "client"
	serverEntry := Entry{
		Version: 1, Peer: "receiver", Role: "server",
		PublicKey: entry.PublicKey, Fingerprint: entry.Fingerprint,
	}

	dir := t.TempDir()
	install(t, dir, "client", entry)
	install(t, dir, "server", serverEntry)

	clientPins, err := loadTrust(dir, "client")
	if err != nil || len(clientPins) != 1 {
		t.Fatalf("client role did not load exactly its own entry: %v, %d", err, len(clientPins))
	}
	if clientPins[entry.Fingerprint].Role != "client" {
		t.Fatal("client load kept a server-role entry")
	}
	serverPins, err := loadTrust(dir, "server")
	if err != nil || len(serverPins) != 1 {
		t.Fatalf("server role did not load exactly its own entry: %v, %d", err, len(serverPins))
	}
	if serverPins[entry.Fingerprint].Role != "server" {
		t.Fatal("server load kept a client-role entry")
	}

	broken := entry
	broken.Role = ""
	install(t, dir, "broken", broken)
	if _, err := loadTrust(dir, "client"); err == nil {
		t.Fatal("entry without role accepted")
	}
}

func TestTrustDuplicatesAndPermissions(t *testing.T) {
	entry := fixtureEntry(t)
	t.Run("duplicates", func(t *testing.T) {
		dir := t.TempDir()
		install(t, dir, "a", entry)
		install(t, dir, "b", entry)
		pins, err := loadTrust(dir, "client")
		if err != nil || len(pins) != 1 {
			t.Fatalf("duplicate key did not yield one authorization: %v, %d", err, len(pins))
		}
		conflict := entry
		conflict.Peer = "another-sender"
		install(t, dir, "b", conflict)
		if _, err := loadTrust(dir, "client"); err == nil {
			t.Fatal("conflicting identity accepted")
		}
	})
	t.Run("writable_entry", func(t *testing.T) {
		dir := t.TempDir()
		path := install(t, dir, "peer", entry)
		if err := os.Chmod(path, 0666); err != nil {
			t.Fatal(err)
		}
		if _, err := loadTrust(dir, "client"); err == nil {
			t.Fatal("insecure entry permissions accepted")
		}
	})
	t.Run("writable_directory", func(t *testing.T) {
		dir := t.TempDir()
		install(t, dir, "peer", entry)
		if err := os.Chmod(dir, 0777); err != nil {
			t.Fatal(err)
		}
		if _, err := loadTrust(dir, "client"); err == nil {
			t.Fatal("insecure directory permissions accepted")
		}
	})
}

func TestTrustEntrySizeBoundary(t *testing.T) {
	entry := fixtureEntry(t)
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{MaxEntrySize - 1, MaxEntrySize, MaxEntrySize + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			dir := t.TempDir()
			content := append(append([]byte{}, data...), []byte(strings.Repeat(" ", size-len(data)))...)
			if err := os.WriteFile(filepath.Join(dir, "peer.json"), content, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadTrust(dir, "client")
			if (err != nil) != (size > MaxEntrySize) {
				t.Fatalf("size=%d error=%v", size, err)
			}
		})
	}
}

func TestTrustCountBoundaries(t *testing.T) {
	entry := fixtureEntry(t)
	t.Run("JSON_entries", func(t *testing.T) {
		dir := t.TempDir()
		for count := 1; count <= MaxPins+1; count++ {
			install(t, dir, strconv.Itoa(count), entry)
			if count < MaxPins-1 {
				continue
			}
			pins, err := loadTrust(dir, "client")
			if count <= MaxPins {
				if err != nil || len(pins) != 1 {
					t.Fatalf("count=%d pins=%d error=%v", count, len(pins), err)
				}
			} else if err == nil {
				t.Fatal("excess JSON entry count accepted")
			}
		}
	})
	t.Run("including_staged_files", func(t *testing.T) {
		dir := t.TempDir()
		install(t, dir, "peer", entry)
		for count := 2; count <= MaxFiles+1; count++ {
			path := filepath.Join(dir, strconv.Itoa(count)+".tmp")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if count < MaxFiles-1 {
				continue
			}
			pins, err := loadTrust(dir, "client")
			if count <= MaxFiles {
				if err != nil || len(pins) != 1 {
					t.Fatalf("count=%d pins=%d error=%v", count, len(pins), err)
				}
			} else if err == nil {
				t.Fatal("excess total file count accepted")
			}
		}
	})
}

func TestGenerationInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		{"--tls-key-role=client"},
		{"--generate-tls-keysets=2"},
		{"--generate-tls-key", "--tls-key-output-dir=" + t.TempDir(),
			"--tls-key-name=../escape", "--tls-key-role=client"},
		{"--generate-tls-key", "--tls-key-output-dir=" + t.TempDir(),
			"--tls-key-name=identity", "--tls-key-role=server"},
	} {
		if handled, err := GenerateCommand(args, "client", io.Discard); !handled || err == nil {
			t.Fatalf("invalid generation input accepted: %v", args)
		}
	}
}

func TestGenerationWritesTrustEntries(t *testing.T) {
	keyDir, jsonDir := filepath.Join(t.TempDir(), "keys"), filepath.Join(t.TempDir(), "trust")
	args := []string{
		"--generate-tls-key",
		"--tls-key-output-dir=" + keyDir,
		"--tls-key-name=sender",
		"--tls-key-role=client",
		"--json-file-dir=" + jsonDir,
	}
	for i := 0; i < 2; i++ {
		handled, err := GenerateCommand(args, "client", io.Discard)
		if !handled || err != nil {
			t.Fatalf("generation failed: handled=%v error=%v", handled, err)
		}
	}

	pins, err := loadTrust(jsonDir, "client")
	if err != nil {
		t.Fatalf("generated trust entries did not load: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("got %d generated pins, want 2", len(pins))
	}
	for i := 1; i <= 2; i++ {
		name := fmt.Sprintf("sender-%d", i)
		data, err := os.ReadFile(filepath.Join(jsonDir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var entry Entry
		if err := json.Unmarshal(data, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Peer != name || entry.Role != "client" {
			t.Errorf("%s has peer=%q role=%q", name, entry.Peer, entry.Role)
		}
	}
}

func TestGenerationContinuesNumbering(t *testing.T) {
	keyDir, jsonDir := filepath.Join(t.TempDir(), "keys"), filepath.Join(t.TempDir(), "trust")
	args := []string{
		"--generate-tls-key",
		"--tls-key-output-dir=" + keyDir,
		"--tls-key-name=sender",
		"--tls-key-role=client",
		"--json-file-dir=" + jsonDir,
	}
	for i := 0; i < 2; i++ {
		handled, err := GenerateCommand(args, "client", io.Discard)
		if !handled || err != nil {
			t.Fatalf("generation failed: handled=%v error=%v", handled, err)
		}
	}
	for _, path := range []string{
		filepath.Join(keyDir, "sender-1.key"),
		filepath.Join(keyDir, "sender-2.key"),
		filepath.Join(jsonDir, "sender-1.json"),
		filepath.Join(jsonDir, "sender-2.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected generated file %s: %v", path, err)
		}
	}
	pins, err := loadTrust(jsonDir, "client")
	if err != nil || len(pins) != 2 {
		t.Fatalf("generated trust entries did not load: pins=%d error=%v", len(pins), err)
	}
}
