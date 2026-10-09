package pinnedtls

import (
	"crypto/tls"
	"testing"
	"time"
)

func agingFixture(t *testing.T) (*Manager, string, string) {
	t.Helper()
	manager, _, request, old, next := rotationFixture(t)
	snapshot := *old
	snapshot.pins = map[string]Entry{}
	snapshot.pins[old.Identity()] = old.pins[old.Identity()]
	snapshot.pins[next.Identity()] = Entry{Peer: "peer-a", Role: "client", Fingerprint: next.Identity()}
	if _, err := manager.Activate(&snapshot); err != nil {
		t.Fatal(err)
	}
	manager.idleTimeout = 10 * time.Second
	start := time.Unix(1800000000, 0)
	manager.lastUse[request.Old] = start
	manager.lastUse[request.New] = start.Add(time.Second)
	manager.lastUsed[request.Old] = start
	manager.lastUsed[request.New] = start.Add(time.Second)
	return manager, request.Old, request.New
}

func TestPinAgingRetainsLastUsedKeyAndHonorsBoundary(t *testing.T) {
	manager, old, next := agingFixture(t)
	start := time.Unix(1800000000, 0)
	if purged := manager.purgeIdle(start.Add(10*time.Second - time.Nanosecond)); len(purged) != 0 {
		t.Fatal("pin aged out before threshold")
	}
	purged := manager.purgeIdle(start.Add(10 * time.Second))
	if len(purged) != 1 || purged[0] != old {
		t.Fatalf("expected older unused key purge, got %v", purged)
	}
	if purged := manager.purgeIdle(start.Add(100 * time.Second)); len(purged) != 0 {
		t.Fatal("last-used peer key aged out")
	}
	if _, ok := manager.active.pins[next]; !ok {
		t.Fatal("most recently used key not retained")
	}
}

func TestPinAgingDisabledAndConnectedPins(t *testing.T) {
	manager, old, next := agingFixture(t)
	now := time.Unix(1800000100, 0)
	manager.idleTimeout = 0
	if got := manager.purgeIdle(now); len(got) != 0 {
		t.Fatal("disabled aging purged trust")
	}
	manager.idleTimeout = 10 * time.Second
	conn := &tls.Conn{}
	other := &tls.Conn{}
	manager.sessions[conn] = authorization{old, "peer-a"}
	manager.sessions[other] = authorization{old, "peer-a"}
	if got := manager.purgeIdle(now); len(got) != 0 {
		t.Fatalf("connected key or last-used key purged: %v", got)
	}
	manager.Forget(conn)
	if !manager.lastUse[old].Equal(time.Unix(1800000000, 0)) {
		t.Fatal("idle timer reset before the final connected session closed")
	}
	manager.Forget(other)
	manager.lastUse[next] = time.Now().Add(-100 * time.Second)
	if got := manager.purgeIdle(time.Now()); len(got) != 0 {
		t.Fatalf("disconnect should start fresh idle timer while last-authenticated key remains protected: %v", got)
	}
	if got := manager.purgeIdle(time.Now().Add(11 * time.Second)); len(got) != 1 || got[0] != old {
		t.Fatalf("disconnected older-authenticated key did not age out: %v", got)
	}
}

func TestPinAgingReloadRestoresPinsAndResetsTimers(t *testing.T) {
	manager, old, _ := agingFixture(t)
	original := manager.active
	if got := manager.purgeIdle(time.Unix(1800000100, 0)); len(got) != 1 {
		t.Fatalf("control did not purge pin: %v", got)
	}
	if _, err := manager.Activate(original); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.active.pins[old]; !ok || len(manager.aged) != 0 {
		t.Fatal("reload failed to restore disk-backed snapshot")
	}
	if got := manager.purgeIdle(time.Now()); len(got) != 0 {
		t.Fatal("reload did not reset idle timers")
	}
}
