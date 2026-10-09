package pinnedtest

import (
	"bytes"
	"net"
	"testing"
	"time"

	"sitia.nu/airgap/src/internal/pinnedtls"
)

func TestPinnedTLSRotationPlaintextAndUDPReject(t *testing.T) {
	h := newApplicationHarness(t)
	old, next := newIdentity(t, nil, false), newIdentity(t, nil, false)
	hash := rotationBegin(old, next)
	var frame bytes.Buffer
	if err := pinnedtls.WriteRotation(&frame, hash); err != nil {
		t.Fatal(err)
	}
	t.Run("plaintext_TCP", func(t *testing.T) {
		down := h.start(t, "downstream", "transport=tcp\ntarget=cmd\nmtu=1500\n")
		down.wait(t, "TCP listener started", 0)
		conn, err := net.DialTimeout("tcp", h.address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write(frame.Bytes()); err != nil {
			t.Fatal(err)
		}
		down.wait(t, "Pinned rotation rejected: requires pinned TLS TCP", 0)
		conn.SetReadDeadline(time.Now().Add(time.Second))
		var b [1]byte
		if _, err := conn.Read(b[:]); err == nil {
			t.Fatal("unexpected response from unsupported transport")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("plaintext rejection did not close connection")
		}
	})
	t.Run("CA_TLS", func(t *testing.T) {
		server := newIdentity(t, nil, false)
		down := h.start(t, "downstream", properties(server, "", true)+
			"tcpTLSAuthMode=ca\ntcpTLSClientAuth=none\ntarget=cmd\nmtu=1500\n")
		down.wait(t, "TLS TCP listener started", 0)
		conn := requireDial(t, h.address, pinnedClient(old, server, nil))
		if _, err := conn.Write(frame.Bytes()); err != nil {
			t.Fatal(err)
		}
		down.wait(t, "Pinned rotation rejected: requires pinned TLS TCP", 0)
		requireClosed(t, conn)
	})
	t.Run("UDP_callback", func(t *testing.T) {
		// Exercise the existing receiver through an actual UDP datagram.
		down := h.start(t, "downstream", "transport=udp\ntarget=cmd\nmtu=1500\n")
		down.wait(t, "Downstream version:", 0)
		conn, err := net.Dial("udp", h.address)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := conn.Write(frame.Bytes()); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains([]byte(down.read(t)), []byte("Pinned rotation rejected on non-control transport")) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("UDP rejection not observed:\n%s", down.read(t))
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}
