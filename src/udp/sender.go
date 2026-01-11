package udp

import (
	"fmt"
	"net"
	"strings"
)

// UDPConn wraps a reusable UDP connection
type UDPConn struct {
	conn net.Conn
}

// NewUDPConn creates a new UDP connection to the given address.
func NewUDPConn(address string) (*UDPConn, error) {
	conn, err := net.Dial("udp", address)
	if err != nil {
		return nil, err
	}
	// Pre-set large buffers to reduce syscall overhead
	if udpConn, ok := conn.(*net.UDPConn); ok {
		_ = udpConn.SetWriteBuffer(1 << 20) // 1 MiB
	}
	return &UDPConn{conn: conn}, nil
}

// Close closes the UDP connection.
func (u *UDPConn) Close() error {
	if u.conn != nil {
		return u.conn.Close()
	}
	return nil
}

// SendMessage sends a single UDP message.
func (u *UDPConn) SendMessage(message []byte) error {
	if u.conn == nil {
		return net.ErrClosed
	}

	// Remove all per-packet logging for performance.
	_, err := u.conn.Write(message)
	if err != nil {
		if opErr, ok := err.(*net.OpError); ok && opErr.Err != nil {
			if strings.Contains(opErr.Err.Error(), "connection refused") {
				return fmt.Errorf("udp-connection-refused: %w", err)
			}
		}
		if err == net.ErrClosed {
			return fmt.Errorf("udp-connection-closed: %w", err)
		}
	}
	return err
}

// SendMessages sends multiple messages using one connection.
func (u *UDPConn) SendMessages(messages [][]byte) error {
	if u.conn == nil {
		return net.ErrClosed
	}

	for _, msg := range messages {
		_, err := u.conn.Write(msg)
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Err != nil {
				if strings.Contains(opErr.Err.Error(), "connection refused") {
					return fmt.Errorf("udp-connection-refused: %w", err)
				}
			}
			if err == net.ErrClosed {
				return fmt.Errorf("udp-connection-closed: %w", err)
			}
			return err
		}
	}
	return nil
}
