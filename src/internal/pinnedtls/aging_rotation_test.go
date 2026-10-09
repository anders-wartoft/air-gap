package pinnedtls

import (
	"testing"
	"time"
)

func TestPinAgingRotationDoesNotRestoreOtherPins(t *testing.T) {
	manager, dir, request, old, _ := rotationFixture(t)
	unused := fixtureEntry(t)
	unused.Peer = "peer-a"
	install(t, dir, "unused", unused)
	snapshot := *old
	var err error
	snapshot.pins, err = loadTrust(dir, "client")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(&snapshot); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	manager.idleTimeout = time.Second
	manager.lastUse[unused.Fingerprint] = now.Add(-2 * time.Second)
	manager.lastUsed[old.Identity()] = now
	if purged := manager.purgeIdle(now); len(purged) != 1 || purged[0] != unused.Fingerprint {
		t.Fatalf("control pin did not age out: %v", purged)
	}
	if err := manager.CommitRotation(request, dir, true); err != nil {
		t.Fatal(err)
	}
	if _, present := manager.active.pins[unused.Fingerprint]; present {
		t.Fatal("rotation implicitly restored an aged pin")
	}
	if _, present := manager.active.pins[request.New]; !present {
		t.Fatal("rotation failed to activate replacement")
	}
	disk, err := loadTrust(dir, "client")
	if err != nil || len(disk) != 3 {
		t.Fatalf("aging/rotation changed unrelated disk trust: %v", err)
	}
	// A reload may explicitly restore all disk entries, including the aged one.
	snapshot.pins = disk
	if _, err := manager.Activate(&snapshot); err != nil {
		t.Fatal(err)
	}
	if _, present := manager.active.pins[unused.Fingerprint]; !present {
		t.Fatal("explicit reload did not restore aged pin")
	}
}
