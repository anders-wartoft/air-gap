package pinnedtls

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const MaxRotationRecords = 2000

type rotationRecord struct {
	ID, Peer, Old, New, State string
}

func pemPublic(spki []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = file.Sync()
	return errors.Join(err, file.Close())
}

func atomicJSON(path string, value any) (err error) {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if info, e := os.Lstat(path); e == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("unsafe persistent state: %s", path)
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".rotation-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		if err != nil {
			if e := os.Remove(temporary); e != nil && !os.IsNotExist(e) {
				err = errors.Join(err, e)
			}
		}
	}()
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if e := errors.Join(writeErr, file.Close()); e != nil {
		return e
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func installPublicEntry(path string, data []byte) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".rotation-public-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		if cleanupErr := os.Remove(temporary); cleanupErr != nil && !os.IsNotExist(cleanupErr) {
			err = errors.Join(err, cleanupErr)
		}
	}()
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	// Link publishes complete content without replacing an occupied filename.
	if err := os.Link(temporary, path); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readRecords(dir string) ([]rotationRecord, error) {
	path := filepath.Join(dir, ".pinned-rotations.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 ||
		info.Size() > 2*1024*1024 {
		return nil, fmt.Errorf("unsafe/oversized rotation journal")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, fmt.Errorf("rotation journal changed during load")
	}
	data, err := io.ReadAll(io.LimitReader(file, 2*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2*1024*1024 {
		return nil, fmt.Errorf("rotation journal exceeds byte limit")
	}
	var records []rotationRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&records); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing rotation journal content")
	}
	if len(records) > MaxRotationRecords {
		return nil, fmt.Errorf("rotation journal exceeds %d records", MaxRotationRecords)
	}
	seen := make(map[string]bool)
	for _, record := range records {
		if seen[record.ID] || record.ID != rotationID(record.Old, record.New) ||
			record.Peer == "" || (record.State != "intent" && record.State != "committed") {
			return nil, fmt.Errorf("invalid rotation journal record")
		}
		seen[record.ID] = true
	}
	return records, nil
}

func ValidateRotationStore(dir string) error {
	_, err := readRecords(dir)
	return err
}

func (m *Manager) persistRotationJSON(path string, value any) error {
	if m.persistRotation != nil {
		return m.persistRotation(path, value)
	}
	return atomicJSON(path, value)
}

func compatibleRotationPins(active, disk map[string]Entry, replacement string) error {
	for fingerprint, entry := range active {
		if fingerprint == replacement {
			continue
		}
		if current, ok := disk[fingerprint]; !ok || current.Peer != entry.Peer {
			return fmt.Errorf("trusted directory changed; reload before rotation")
		}
	}
	for fingerprint := range disk {
		if fingerprint != replacement {
			if _, ok := active[fingerprint]; !ok {
				return fmt.Errorf("trusted directory changed; reload before rotation")
			}
		}
	}
	return nil
}

// CommitRotation holds the same authorization gate as reload/admission through
// durable installation and activation. Retries never reconstruct deleted pins.
func (m *Manager) CommitRotation(message RotationMessage, dir string, automatic bool) error {
	entry, err := verifyRotation(message)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.active.pins[message.Old]
	if !ok || old.Peer != message.Peer {
		return fmt.Errorf("old key no longer authorized")
	}
	records, err := readRecords(dir)
	if err != nil {
		return err
	}
	index := -1
	for i, record := range records {
		if record.ID == message.ID {
			if record.Peer != message.Peer || record.Old != message.Old || record.New != message.New {
				return fmt.Errorf("conflicting rotation ID")
			}
			index = i
		} else if record.Peer == message.Peer && record.State == "intent" {
			return fmt.Errorf("another replacement is pending for peer")
		} else if record.Old == message.Old && record.New != message.New {
			return fmt.Errorf("old key already used for another replacement")
		}
	}
	pins, err := loadTrust(dir, "client")
	if err != nil {
		return err
	}
	// Disk removal is authoritative even if a concurrent request precedes SIGHUP.
	if current, ok := pins[message.Old]; !ok || current.Peer != message.Peer {
		return fmt.Errorf("old key removed from trusted directory")
	}
	next, exists := pins[message.New]
	if _, aged := m.aged[message.New]; aged {
		return fmt.Errorf("replacement key aged out; SIGHUP required to restore")
	}
	if exists && next.Peer != message.Peer {
		return fmt.Errorf("new key belongs to another peer")
	}
	if index >= 0 && !exists {
		return fmt.Errorf("recorded replacement absent; administrator reconciliation required")
	}
	_, alreadyActive := m.active.pins[message.New]
	if !alreadyActive && !automatic && index < 0 {
		return fmt.Errorf("automatic rotation disabled; replacement not preauthorized")
	}
	diskCount := len(pins)
	m.excludeAged(pins)
	if err := compatibleRotationPins(m.active.pins, pins, message.New); err != nil {
		return err
	}
	if !exists && diskCount >= MaxPins {
		return fmt.Errorf("trusted key capacity reached")
	}
	if index < 0 {
		if len(records) >= MaxRotationRecords {
			return fmt.Errorf("rotation journal capacity reached")
		}
		records = append(records, rotationRecord{message.ID, message.Peer, message.Old, message.New, "intent"})
		index = len(records) - 1
		if err := m.persistRotationJSON(filepath.Join(dir, ".pinned-rotations.json"), records); err != nil {
			return fmt.Errorf("persist rotation intent: %w", err)
		}
	}
	if !exists {
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "rotation-"+message.ID+".json")
		if err := installPublicEntry(path, data); err != nil {
			return fmt.Errorf("install replacement: %w", err)
		}
	}
	pins, err = loadTrust(dir, "client")
	if err != nil {
		return err
	}
	m.excludeAged(pins)
	if err := compatibleRotationPins(m.active.pins, pins, message.New); err != nil {
		return err
	}
	// Validate and durably mark the exact replacement before making it active.
	if next, ok := pins[message.New]; !ok || next.Peer != message.Peer {
		return fmt.Errorf("replacement not installed")
	}
	records[index].State = "committed"
	if err := m.persistRotationJSON(filepath.Join(dir, ".pinned-rotations.json"), records); err != nil {
		return fmt.Errorf("persist rotation commit: %w", err)
	}
	copy := *m.active
	copy.pins = pins
	m.active = m.configure(&copy)
	if _, present := m.lastUse[message.New]; !present {
		m.lastUse[message.New] = time.Now()
	}
	return nil
}
