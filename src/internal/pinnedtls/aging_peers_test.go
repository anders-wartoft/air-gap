package pinnedtls

import (
	"testing"
	"time"
)

func TestPinAgingPreservesEachPeerAndDeterministicUnusedFallback(t *testing.T) {
	manager, old, next := agingFixture(t)
	now := time.Now()
	peerB := fixtureEntry(t)
	peerB.Peer = "peer-b"
	snapshot := *manager.active
	snapshot.pins = map[string]Entry{}
	for fingerprint, entry := range manager.active.pins {
		snapshot.pins[fingerprint] = entry
	}
	snapshot.pins[peerB.Fingerprint] = peerB
	if _, err := manager.Activate(&snapshot); err != nil {
		t.Fatal(err)
	}
	// Simulate neither overlap key having ever been authenticated.
	manager.lastUsed[old], manager.lastUsed[next] = time.Time{}, time.Time{}
	for fingerprint := range manager.lastUse {
		manager.lastUse[fingerprint] = now.Add(-time.Minute)
	}
	if purged := manager.purgeIdle(now); len(purged) != 1 {
		t.Fatalf("expected exactly one unused overlap key purged: %v", purged)
	}
	first := old
	if next < first {
		first = next
	}
	if _, kept := manager.active.pins[first]; !kept {
		t.Fatal("never-used fallback was not deterministic fingerprint order")
	}
	if _, kept := manager.active.pins[peerB.Fingerprint]; !kept {
		t.Fatal("another peer's single key was purged")
	}
}

func TestPinAgingWorkerStopsIdempotently(t *testing.T) {
	manager, _, _ := agingFixture(t)
	manager.StartAging(1, nil)
	manager.StopAging()
	manager.StopAging()
}
