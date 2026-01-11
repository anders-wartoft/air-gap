package upstream

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/udp"
)

type KafkaAdapter struct{}

func (k *KafkaAdapter) Read(ctx context.Context, name string, offset int,
	bootstrapServers, topic, group, from string,
	handler kafka.KafkaHandler) {
	// Pass UpdateKafkaStatus as error callback to report Kafka issues
	kafka.ReadFromKafkaWithContext(ctx, name, offset, bootstrapServers, topic, group, from, handler, UpdateKafkaStatus)
}

func (k *KafkaAdapter) SetTLS(certFile, keyFile, caFile, keyPasswordFile string) error {
	_, err := kafka.SetTLSConfigParameters(certFile, keyFile, caFile, keyPasswordFile)
	return err
}

// UDPAdapter implements TransportClient for UDP connections
type UDPAdapter struct {
	conn *udp.UDPConn
}

func NewUDPAdapter(address string) (*UDPAdapter, error) {
	c, err := udp.NewUDPConn(address)
	if err != nil {
		return nil, err
	}
	return &UDPAdapter{conn: c}, nil
}

func (u *UDPAdapter) SendMessage(msg []byte) error {
	return u.conn.SendMessage(msg)
}

func (u *UDPAdapter) SendMessages(msgs [][]byte) error {
	return u.conn.SendMessages(msgs)
}

func (u *UDPAdapter) Close() error {
	return u.conn.Close()
}

// TCPAdapter implements TransportClient for TCP connections
type TCPAdapter struct {
	conn net.Conn
	addr string
	mu   sync.Mutex
	err  error
}

// NewTCPAdapter creates a new TCP adapter that connects to the specified address
// It doesn't fail if connection is not immediately available - it will attempt
// to connect lazily on first message send
func NewTCPAdapter(address string) (*TCPAdapter, error) {
	adapter := &TCPAdapter{
		addr: address,
	}

	// Try to connect immediately, but don't fail if it's not available yet
	conn, err := net.Dial("tcp", address)
	if err != nil {
		// Store the error but don't fail - we'll retry on first message
		adapter.err = err
		Logger.Warnf("Failed to connect to TCP server at %s on startup, will retry: %v", address, err)
	} else {
		adapter.conn = conn
		Logger.Infof("Connected to TCP server at %s", address)
	}

	return adapter, nil
}

// ensureConnected ensures we have a valid connection, attempting to reconnect if needed
// Returns true if connected, false otherwise
func (t *TCPAdapter) ensureConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	// If we have a connection, test it with a deadline to ensure it's actually working
	if t.conn != nil {
		// Set a short read deadline to test if connection is alive
		// This will return immediately with timeout error if there's no pending data
		t.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		b := make([]byte, 1)
		_, err := t.conn.Read(b)
		t.conn.SetReadDeadline(time.Time{}) // Clear the deadline

		// If we get an error that's not a timeout, the connection is dead
		if err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				// Connection is dead or EOF, close it
				t.conn.Close()
				t.conn = nil
			}
		}

		// If connection is still set after checks, it's good
		if t.conn != nil {
			return true
		}
	}

	// Try to connect
	conn, err := net.Dial("tcp", t.addr)
	if err != nil {
		t.err = err
		return false
	}

	t.conn = conn
	t.err = nil
	Logger.Infof("Connected to TCP server at %s", t.addr)
	return true
}

// reconnect attempts to re-establish a TCP connection
func (t *TCPAdapter) reconnect() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Close existing connection if any
	if t.conn != nil {
		t.conn.Close()
	}

	// Attempt to reconnect
	conn, err := net.Dial("tcp", t.addr)
	if err != nil {
		t.err = err
		return fmt.Errorf("failed to reconnect to TCP server at %s: %w", t.addr, err)
	}

	t.conn = conn
	t.err = nil
	return nil
}

// SendMessage sends a single message over TCP, reconnecting if necessary
func (t *TCPAdapter) SendMessage(msg []byte) error {
	// Ensure we have a connection, attempting to connect if needed
	if !t.ensureConnected() {
		return fmt.Errorf("TCP connection unavailable to %s", t.addr)
	}

	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()

	_, err := conn.Write(msg)
	if err != nil {
		// Connection error - try to reconnect and resend
		if reconnectErr := t.reconnect(); reconnectErr != nil {
			return fmt.Errorf("failed to send message over TCP: %w", err)
		}

		// Try to write again with the new connection
		t.mu.Lock()
		_, retryErr := t.conn.Write(msg)
		t.mu.Unlock()

		if retryErr != nil {
			return fmt.Errorf("failed to send message over TCP after reconnect: %w", retryErr)
		}
	}

	return nil
}

// SendMessages sends multiple messages over TCP
func (t *TCPAdapter) SendMessages(msgs [][]byte) error {
	for _, msg := range msgs {
		if err := t.SendMessage(msg); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the TCP connection
func (t *TCPAdapter) Close() error {
	if t.conn != nil {
		return t.conn.Close()
	}
	return nil
}
