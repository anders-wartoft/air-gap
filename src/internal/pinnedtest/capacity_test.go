package pinnedtest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"sitia.nu/airgap/src/internal/pinnedtls"
)

func TestPinnedTLSFunctionalCapacity2000DistinctPins(t *testing.T) {
	server := newIdentity(t, nil, false)
	dir := t.TempDir()
	clients := make([]identity, 0, 2000)
	for peer := 0; peer < 1000; peer++ {
		for key := 0; key < 2; key++ {
			id := newIdentity(t, nil, false)
			clients = append(clients, id)
			data, err := json.Marshal(trustEntry{1, fmt.Sprintf("client-%d", peer), "client", id.publicPEM, id.fingerprint})
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, fmt.Sprintf("client-%d-key-%d.json", peer, key)), data)
		}
	}
	config, err := pinnedtls.Build(server.certPath, server.keyPath, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, client := range clients {
		if err := exchange(config, client, true); err != nil {
			t.Fatalf("baseline key %d did not authenticate and exchange data: %v", i, err)
		}
	}
	if err := exchange(config, newIdentity(t, nil, false), true); err == nil {
		t.Fatal("unknown key accepted at baseline capacity")
	}
	t.Log("all 2,000 distinct pins completed real TLS and application round trips; no performance budget claimed")
}
