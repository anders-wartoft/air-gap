package udp

import (
	"net"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func TestUDPBatchOverflowAncillary(t *testing.T) {
	RxqOvflDrops.Store(0)
	RxqOvflDropsTotal.Store(0)
	t.Cleanup(func() {
		RxqOvflDrops.Store(0)
		RxqOvflDropsTotal.Store(0)
	})
	receiver := NewUDPAdapterLinux("")
	receiver.enableRxqOvfl = true
	conn, effective, err := receiver.listenSocket("127.0.0.1:0", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if effective < 1024 {
		t.Fatalf("effective receive buffer %d is smaller than requested", effective)
	}
	sender, err := net.DialUDP("udp4", nil, conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	packet := make([]byte, 512)
	for i := 0; i < 4096; i++ {
		if _, err := sender.Write(packet); err != nil {
			t.Fatal(err)
		}
	}
	reader := ipv4.NewPacketConn(conn)
	messages := make([]ipv4.Message, receiveBatchSize)
	for i := range messages {
		messages[i].Buffers = [][]byte{make([]byte, len(packet))}
		messages[i].OOB = make([]byte, unix.CmsgSpace(4))
	}
	// Drain packets queued before the overflow, then receive a fresh packet
	// carrying the cumulative kernel drop count.
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := reader.ReadBatch(messages, 0)
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sender.Write(packet); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := reader.ReadBatch(messages, 0)
	if err != nil {
		t.Fatal(err)
	}
	var last uint32
	for _, message := range messages[:n] {
		if message.Flags&unix.MSG_CTRUNC != 0 {
			t.Fatal("drop counter control message was truncated")
		}
		receiver.recordControl(message.OOB[:message.NN], &last)
	}
	if last == 0 || RxqOvflDropsTotal.Load() != uint64(last) {
		t.Fatalf("first kernel report was not counted: report=%d total=%d", last, RxqOvflDropsTotal.Load())
	}
}
