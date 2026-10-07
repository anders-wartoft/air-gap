package downstream

import (
	"net"
	"testing"
	"time"
)

type readyTCPReceiver struct {
	*TCPAdapter
	ready chan struct{}
}

func (r *readyTCPReceiver) Listen(ip string, port, rcvBufSize int, callback func([]byte),
	mtu uint16, stop <-chan struct{}, numReceivers int) {
	r.TCPAdapter.Listen(ip, port, rcvBufSize, callback, mtu, stop, numReceivers)
	close(r.ready)
}

func TestTCPShutdownClosesListenerBeforeFlushDelay(t *testing.T) {
	previousConfig := config
	t.Cleanup(func() { config = previousConfig })
	config = defaultConfiguration()
	config.transport, config.target, config.targetIP = "tcp", "null", "127.0.0.1"
	config.targetPort, config.mtu = 0, 1500
	receiver := &readyTCPReceiver{
		TCPAdapter: NewTCPAdapter(config),
		ready:      make(chan struct{}),
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		RunDownstream(receiver, stop)
		close(done)
	}()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			close(stop)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("downstream shutdown did not finish")
		}
	})
	select {
	case <-receiver.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("TCP listener startup timed out")
	}
	address := receiver.listener.Addr().String()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	close(stop)
	stopped = true

	deadline := time.Now().Add(time.Second)
	for {
		probe, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			break
		}
		probe.Close()
		if time.Now().After(deadline) {
			t.Fatal("TCP listener still accepts connections during the flush delay")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("shutdown skipped the existing flush delay")
	default:
	}
}
