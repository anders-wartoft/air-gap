package udp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func TestPacketBatchOwnsPayloads(t *testing.T) {
	first := []byte("first")
	second := []byte("second")
	messages := []ipv4.Message{
		{Buffers: [][]byte{first}, N: len(first)},
		{Buffers: [][]byte{second}, N: len(second)},
		{Buffers: [][]byte{[]byte("truncated")}, N: 100, Flags: unix.MSG_TRUNC},
		{Buffers: [][]byte{{}}, N: 0},
	}
	var batch packetBatch
	batch.copyMessages(messages)
	clear(first)
	clear(second)
	if batch.count != 3 || string(batch.packets[0]) != "first" || string(batch.packets[1]) != "second" || len(batch.packets[2]) != 0 {
		t.Fatalf("copied batch: count=%d packets=%q", batch.count, batch.packets[:batch.count])
	}
	if cap(batch.packets[0]) != len(batch.packets[0]) {
		t.Fatal("packet can overwrite the next packet via append")
	}
	batch.reset()
	if batch.count != 0 || batch.packets[0] != nil || batch.packets[1] != nil {
		t.Fatal("returned batch retains packet references")
	}
	large := make([]ipv4.Message, receiveBatchSize)
	for i := range large {
		large[i] = ipv4.Message{Buffers: [][]byte{make([]byte, 1500)}, N: 1500}
	}
	batch.copyMessages(large)
	batch.reset()
	batch.copyMessages(messages[1:2])
	if cap(batch.data) > 2*len(second) || cap(batch.packets) > 2 {
		t.Fatalf("short queued batch retained oversized storage: bytes=%d packet slots=%d", cap(batch.data), cap(batch.packets))
	}
}

func startTestReceiver(t *testing.T, queueSize int, handler func([]byte)) (*UDPAdapterLinux, *net.UDPConn, <-chan struct{}) {
	t.Helper()
	return startTestReceiverOn(t, "127.0.0.1", queueSize, handler)
}

func startTestReceiverOn(t *testing.T, ip string, queueSize int, handler func([]byte)) (*UDPAdapterLinux, *net.UDPConn, <-chan struct{}) {
	t.Helper()
	receiver := NewUDPAdapterLinux("")
	receiver.Setup(1500, 1, queueSize, 1, false)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		receiver.Listen(ip, 0, 256*1024, handler, stop)
	}()
	t.Cleanup(func() {
		close(stop)
		receiver.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("receiver did not stop")
		}
	})
	var address *net.UDPAddr
	deadline := time.Now().Add(5 * time.Second)
	for address == nil && time.Now().Before(deadline) {
		receiver.connsMu.Lock()
		if len(receiver.conns) > 0 {
			address = receiver.conns[0].LocalAddr().(*net.UDPAddr)
		}
		receiver.connsMu.Unlock()
		if address == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if address == nil {
		t.Fatal("receiver did not bind")
	}
	sender, err := net.DialUDP("udp", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sender.Close() })
	return receiver, sender, done
}

func TestUDPBatchIPv6(t *testing.T) {
	probe, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	probe.Close()
	received := make(chan string, 1)
	_, sender, _ := startTestReceiverOn(t, "::1", 64, func(packet []byte) {
		received <- string(packet)
	})
	if _, err := sender.Write([]byte("IPv6")); err != nil {
		t.Fatal(err)
	}
	select {
	case packet := <-received:
		if packet != "IPv6" {
			t.Fatalf("received %q", packet)
		}
	case <-time.After(time.Second):
		t.Fatal("IPv6 packet was not delivered")
	}
}

func TestUDPBatchSparseAndIdleClose(t *testing.T) {
	received := make(chan []byte, 1)
	receiver, sender, done := startTestReceiver(t, 65, func(packet []byte) {
		received <- bytes.Clone(packet)
	})
	for _, packet := range [][]byte{[]byte("one sparse datagram"), {}} {
		if _, err := sender.Write(packet); err != nil {
			t.Fatal(err)
		}
		select {
		case actual := <-received:
			if !bytes.Equal(actual, packet) {
				t.Fatalf("received %q, want %q", actual, packet)
			}
		case <-time.After(time.Second):
			t.Fatal("partial batch was not delivered promptly")
		}
	}
	receiver.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not wake an idle batch read")
	}
}

func TestUDPBatchRecyclingPreservesPayloads(t *testing.T) {
	received := make(chan error, 32)
	seen := make([]bool, 1024)
	_, sender, _ := startTestReceiver(t, 64, func(packet []byte) {
		if len(packet) < 8 {
			received <- fmt.Errorf("short payload: %d bytes", len(packet))
			return
		}
		seq := binary.BigEndian.Uint64(packet)
		if seq >= uint64(len(seen)) || seen[seq] {
			received <- fmt.Errorf("duplicate or unexpected sequence %d", seq)
			return
		}
		seen[seq] = true
		if !bytes.Equal(packet[8:], bytes.Repeat([]byte{byte(seq)}, len(packet)-8)) {
			received <- fmt.Errorf("corrupted payload %d", seq)
			return
		}
		received <- nil
	})
	// Recycle a small pool repeatedly, varying both packet and batch lengths.
	for base := 0; base < 1024; base += 32 {
		for i := base; i < base+32; i++ {
			packet := bytes.Repeat([]byte{byte(i)}, 8+i%1400)
			binary.BigEndian.PutUint64(packet, uint64(i))
			if _, err := sender.Write(packet); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 32; i++ {
			select {
			case err := <-received:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing packet")
			}
		}
	}
}

func TestUDPBatchRejectsTruncatedDatagram(t *testing.T) {
	received := make(chan []byte, 2)
	_, sender, _ := startTestReceiver(t, 64, func(packet []byte) {
		received <- bytes.Clone(packet)
	})
	for _, packet := range [][]byte{make([]byte, 2000), []byte("complete")} {
		if _, err := sender.Write(packet); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case actual := <-received:
		if string(actual) != "complete" {
			t.Fatalf("truncated packet reached handler: %d bytes", len(actual))
		}
	case <-time.After(time.Second):
		t.Fatal("complete packet following truncated packet was not delivered")
	}
}

func TestUDPBatchQueuePreservesDatagramCapacityAndDrains(t *testing.T) {
	const queueSize = 65
	const count = queueSize + 2 // One in the handler and one awaiting queue space.
	entered := make(chan struct{})
	release := make(chan struct{})
	received := make(chan struct{}, count)
	var once, releaseOnce sync.Once
	receiver, sender, done := startTestReceiver(t, queueSize, func(packet []byte) {
		once.Do(func() { close(entered) })
		<-release
		received <- struct{}{}
	})
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	before := ReadDatagramsTotal.Load()
	for i := 0; i < count; i++ {
		if _, err := sender.Write([]byte("queued")); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for ReadDatagramsTotal.Load()-before < uint64(i+1) {
			if time.Now().After(deadline) {
				t.Fatalf("short batches exhausted queue space at datagram %d (budget %d)", i+1, queueSize)
			}
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	receiver.Close()
	select {
	case <-done:
		t.Fatal("listener returned before its handler finished")
	default:
	}
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after handler unblocked")
	}
	if len(received) != count {
		t.Fatalf("shutdown discarded a successfully read batch: handled=%d, want %d", len(received), count)
	}
}

func TestRecordRxqOverflow(t *testing.T) {
	RxqOvflDrops.Store(0)
	RxqOvflDropsTotal.Store(0)
	t.Cleanup(func() {
		RxqOvflDrops.Store(0)
		RxqOvflDropsTotal.Store(0)
	})

	var first, second uint32
	recordRxqOverflow(&first, 7)
	recordRxqOverflow(&first, 7)
	recordRxqOverflow(&second, 3)
	recordRxqOverflow(&first, 10)
	if got := RxqOvflDrops.Swap(0); got != 13 {
		t.Fatalf("first interval: got %d drops, want 13", got)
	}

	recordRxqOverflow(&second, 5)
	if got := RxqOvflDrops.Load(); got != 2 {
		t.Errorf("second interval: got %d drops, want 2", got)
	}
	if got := RxqOvflDropsTotal.Load(); got != 15 {
		t.Errorf("total: got %d drops, want 15", got)
	}

	first = math.MaxUint32 - 2
	recordRxqOverflow(&first, 2)
	if got := RxqOvflDrops.Load(); got != 7 {
		t.Errorf("interval after wrap: got %d drops, want 7", got)
	}
	if got := RxqOvflDropsTotal.Load(); got != 20 {
		t.Errorf("total after wrap: got %d drops, want 20", got)
	}
}
