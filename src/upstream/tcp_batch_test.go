package upstream

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type batchTestConn struct {
	net.Conn
	mu      sync.Mutex
	writes  [][]byte
	onWrite func([]byte) (int, error)
}

func (c *batchTestConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), data...))
	c.mu.Unlock()
	if c.onWrite != nil {
		return c.onWrite(data)
	}
	return len(data), nil
}

func (c *batchTestConn) Close() error { return nil }

func waitPendingSends(t *testing.T, adapter *TCPAdapter, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		adapter.sendMu.Lock()
		pending := len(adapter.pendingSends)
		adapter.sendMu.Unlock()
		if pending == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending=%d, expected=%d", pending, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTCPConcurrentSendsCoalesceAndCompleteSynchronously(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var first, released sync.Once
	conn := &batchTestConn{onWrite: func(data []byte) (int, error) {
		first.Do(func() { close(started); <-release })
		return len(data), nil
	}}
	adapter := &TCPAdapter{conn: conn}
	var writers sync.WaitGroup
	results := make(chan error, 17)
	send := func() {
		writers.Add(1)
		go func() {
			defer writers.Done()
			results <- adapter.SendMessages([][]byte{[]byte("a"), []byte("b")})
		}()
	}
	defer func() {
		released.Do(func() { close(release) })
		writers.Wait()
	}()
	send()
	<-started
	for range 16 {
		send()
	}
	waitPendingSends(t, adapter, 16)
	if len(results) != 0 {
		t.Fatal("enqueue was reported as a successful socket write")
	}
	released.Do(func() { close(release) })
	writers.Wait()
	for range 17 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	// The initial multipart request writes two frames, then sixteen waiting
	// multipart requests share a single write without dropping any frames.
	if len(conn.writes) != 3 || !bytes.Equal(conn.writes[2], bytes.Repeat([]byte("ab"), 16)) {
		t.Fatalf("unexpected coalescing: %q", conn.writes)
	}
}

func TestTCPBatchRetriesOnlyIncompleteFrames(t *testing.T) {
	for _, writeErr := range []error{io.ErrUnexpectedEOF, nil} {
		t.Run("partial-"+func() string {
			if writeErr == nil {
				return "short-write"
			}
			return "error"
		}(), func(t *testing.T) {
			replacement := &batchTestConn{}
			adapter := &TCPAdapter{}
			failed := &batchTestConn{onWrite: func([]byte) (int, error) {
				adapter.mu.Lock()
				adapter.conn = replacement
				adapter.mu.Unlock()
				return 3, writeErr
			}}
			adapter.conn = failed
			batch := []*tcpSendRequest{
				{frames: [][]byte{[]byte("aa")}},
				{frames: [][]byte{[]byte("bb"), []byte("cc")}},
				{frames: [][]byte{[]byte("dd")}},
			}
			adapter.writeSendBatch(batch)
			for _, request := range batch {
				if request.err != nil {
					t.Fatal(request.err)
				}
			}
			if got := bytes.Join(replacement.writes, nil); string(got) != "bbccdd" {
				t.Fatalf("replayed a fully written frame or lost a suffix: %s", got)
			}
		})
	}
}

func TestTCPCloseReleasesQueuedSends(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	adapter := &TCPAdapter{conn: conn}
	results := make(chan error, 17)
	for range 17 {
		go func() { results <- adapter.SendMessage([]byte("message")) }()
	}
	waitPendingSends(t, adapter, 16)
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	for range 17 {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("closed, unwritten request succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("queued send remained blocked after Close")
		}
	}
}
