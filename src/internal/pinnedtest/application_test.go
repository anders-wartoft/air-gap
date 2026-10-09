package pinnedtest

import "testing"

func TestPinnedTLSApplicationTransport(t *testing.T) {
	h := newApplicationHarness(t)
	client, server, unknown := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	clientTrust, serverTrust := t.TempDir(), t.TempDir()
	entry(t, clientTrust, "server", "server", server)
	entry(t, serverTrust, "client", "client", client)
	down := h.start(t, "downstream", properties(server, serverTrust, true)+"target=cmd\nmtu=1500\n")
	down.wait(t, "TLS TCP listener started", 0)
	up := h.start(t, "upstream", properties(client, clientTrust, false)+"source=random\npayloadSize=1400\neps=10\n")
	down.wait(t, "Random message 2", 0)
	requireReloadSuccess(t, up.reload(t))
	requireReloadSuccess(t, down.reload(t))
	down.wait(t, "Random message 5", 0)

	h.start(t, "upstream", properties(unknown, clientTrust, false)+"source=random\npayloadSize=1400\neps=10\n")
	down.wait(t, "untrusted client key "+unknown.fingerprint, 0)
}
