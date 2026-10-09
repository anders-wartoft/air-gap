package pinnedtls

import (
	"encoding/json"
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
	if err := generate(dir, "identity", "client", 1, io.Discard); err != nil {
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
		{"--generate-tls-keysets=0"},
		{"--generate-tls-keysets=2001"},
		{"--generate-tls-keysets=1", "--tls-key-output-dir=" + t.TempDir(),
			"--tls-key-name=../escape", "--tls-key-role=client"},
		{"--generate-tls-keysets=1", "--tls-key-output-dir=" + t.TempDir(),
			"--tls-key-name=identity", "--tls-key-role=server"},
	} {
		if handled, err := GenerateCommand(args, "client", io.Discard); !handled || err == nil {
			t.Fatalf("invalid generation input accepted: %v", args)
		}
	}
}
