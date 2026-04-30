package udp

import (
	"fmt"
	"net"
	"strings"
	"syscall"
)

// UDPConn wraps a reusable UDP connection
type UDPConn struct {
	conn net.Conn
}

// NewUDPConn creates a new UDP connection to the given address.
func NewUDPConn(address string) (*UDPConn, error) {
	return NewUDPConnWithNIC(address, "")
}

// NewUDPConnWithNIC creates a new UDP connection to the given address, optionally bound to a
// specific network interface. When the target is a broadcast address (255.255.255.255) or a
// NIC name is supplied, a custom dialer is used that:
//   - Binds the socket to the IPv4 address of the named interface (if nic != "")
//   - Sets SO_BROADCAST on the socket (if the target is 255.255.255.255)
func NewUDPConnWithNIC(address, nic string) (*UDPConn, error) {
	host, _, _ := net.SplitHostPort(address)
	isBroadcast := host == "255.255.255.255"

	// Resolve the NIC's IPv4 address for local binding when a NIC is specified.
	var localIP net.IP
	if nic != "" {
		iface, err := net.InterfaceByName(nic)
		if err != nil {
			return nil, fmt.Errorf("interface %q not found: %w", nic, err)
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("failed to get addresses for interface %q: %w", nic, err)
		}
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok {
				if v4 := ipNet.IP.To4(); v4 != nil {
					localIP = v4
					break
				}
			}
		}
		if localIP == nil {
			return nil, fmt.Errorf("no IPv4 address found on interface %q", nic)
		}
	}

	// Fast path: no broadcast, no NIC binding needed.
	if !isBroadcast && localIP == nil {
		conn, err := net.Dial("udp", address)
		if err != nil {
			return nil, err
		}
		if udpConn, ok := conn.(*net.UDPConn); ok {
			_ = udpConn.SetWriteBuffer(1 << 20) // 1 MiB
		}
		return &UDPConn{conn: conn}, nil
	}

	// Slow path: need SO_BROADCAST and/or interface binding.
	var localAddr *net.UDPAddr
	if localIP != nil {
		localAddr = &net.UDPAddr{IP: localIP}
	}

	dialer := net.Dialer{
		LocalAddr: localAddr,
		Control: func(network, address string, c syscall.RawConn) error {
			if !isBroadcast {
				return nil
			}
			var setSockOptErr error
			err := c.Control(func(fd uintptr) {
				setSockOptErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			})
			if err != nil {
				return err
			}
			return setSockOptErr
		},
	}

	conn, err := dialer.Dial("udp4", address)
	if err != nil {
		return nil, err
	}
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
