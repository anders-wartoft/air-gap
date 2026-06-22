package udp

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"sitia.nu/airgap/src/logging"
)

// SO_RXQ_OVFL enables receiving packet drop counts in ancillary data
// Value is 40 on Linux (see linux/socket.h)
const SO_RXQ_OVFL = 40

type UDPAdapterLinux struct {
	addr                 string
	conns                []*net.UDPConn
	closed               atomic.Bool
	closeOnce            sync.Once
	wg                   sync.WaitGroup
	numReceivers         int
	channelBufferSize    int
	mtu                  uint16
	readBufferMultiplier uint16
	enableRxqOvfl        bool
}

var Logger = logging.Logger

// Global atomic counters for SO_RXQ_OVFL statistics
var (
	RxqOvflDrops      atomic.Uint64 // Drops since last statistics reset
	RxqOvflDropsTotal atomic.Uint64 // Total drops since start
)

func (u *UDPAdapterLinux) Setup(
	mtu uint16,
	numReceivers int,
	channelBufferSize int,
	readBufferMultiplier uint16,
	enableRxqOvfl bool,
) {
	u.mtu = mtu
	u.numReceivers = numReceivers
	u.channelBufferSize = channelBufferSize
	u.readBufferMultiplier = readBufferMultiplier
	u.enableRxqOvfl = enableRxqOvfl
}

func NewUDPAdapterLinux(addr string) *UDPAdapterLinux {
	return &UDPAdapterLinux{
		addr:                 addr,
		numReceivers:         8,
		channelBufferSize:    8192,
		mtu:                  1500,
		readBufferMultiplier: 16,
	}
}

// Listen starts multiple sockets for SO_REUSEPORT
func (u *UDPAdapterLinux) Listen(ip string, port int, rcvBufSize int, handler func([]byte), stopChan <-chan struct{}) {
	addrStr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
	Logger.Printf("UDP listener starting on %s with %d workers (MTU=%d)", addrStr, u.numReceivers, u.mtu)

	packetChan := make(chan []byte, u.channelBufferSize)

	// --- start worker goroutines ---
	for i := 0; i < u.numReceivers; i++ {
		u.wg.Add(1)
		go func(id int) {
			defer u.wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			for {
				select {
				case pkt := <-packetChan:
					if pkt == nil {
						return
					}
					handler(pkt)
				case <-stopChan:
					return
				}
			}
		}(i)
	}

	// --- create one socket per worker using SO_REUSEPORT ---
	for i := 0; i < u.numReceivers; i++ {
		lc := net.ListenConfig{
			Control: func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
					_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
					_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, rcvBufSize)
					if u.enableRxqOvfl {
						// Enable SO_RXQ_OVFL to track socket-level packet drops
						_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, SO_RXQ_OVFL, 1)
					}
				})
			},
		}

		conn, err := lc.ListenPacket(context.Background(), "udp", addrStr)
		if err != nil {
			Logger.Fatalf("Failed to listen on %s: %v", addrStr, err)
		}
		udpConn := conn.(*net.UDPConn)
		u.conns = append(u.conns, udpConn)

		u.wg.Add(1)
		// Worker thread for reading from this socket
		go func(c *net.UDPConn) {
			defer u.wg.Done()
			readBuf := make([]byte, int(u.mtu)*int(u.readBufferMultiplier))
			var oob []byte
			if u.enableRxqOvfl {
				oob = make([]byte, 1024) // Buffer for ancillary data
			}
			var lastDropCount uint32

			for {
				select {
				case <-stopChan:
					return
				default:
					c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

					var n int
					var err error

					if u.enableRxqOvfl {
						// Use ReadMsgUDP to get ancillary data with drop counts
						var oobn int
						n, oobn, _, _, err = c.ReadMsgUDP(readBuf, oob)
						if err == nil && oobn > 0 {
							// Parse ancillary data to extract SO_RXQ_OVFL drop count
							msgs, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
							if parseErr == nil {
								for _, msg := range msgs {
									if msg.Header.Level == unix.SOL_SOCKET && msg.Header.Type == SO_RXQ_OVFL {
										if len(msg.Data) >= 4 {
											// Extract uint32 drop count
											dropCount := uint32(msg.Data[0]) | uint32(msg.Data[1])<<8 |
												uint32(msg.Data[2])<<16 | uint32(msg.Data[3])<<24

											// Calculate delta if this isn't the first read
											if lastDropCount > 0 && dropCount > lastDropCount {
												delta := dropCount - lastDropCount
												RxqOvflDrops.Add(uint64(delta))
												RxqOvflDropsTotal.Add(uint64(delta))
											}
											lastDropCount = dropCount
										}
									}
								}
							}
						}
					} else {
						// Standard read without ancillary data
						n, _, err = c.ReadFromUDP(readBuf)
					}

					if err != nil {
						if ne, ok := err.(net.Error); ok && ne.Timeout() {
							continue
						}
						if u.closed.Load() {
							return
						}
						Logger.Errorf("UDP read error: %v", err)
						continue
					}
					buf := make([]byte, n)
					copy(buf, readBuf[:n])

					select {
					case packetChan <- buf:
					case <-stopChan:
						return
					}
				}
			}
		}(udpConn)
	}

	// Wait for stop signal
	<-stopChan

	// --- shutdown ---
	u.Close()
	close(packetChan)

	// Give workers a small timeout to finish processing remaining messages
	timeout := time.After(2 * time.Second)
	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-timeout:
		Logger.Warnf("UDPAdapter shutdown timeout reached, some messages may be lost")
	}
}

// Close closes all sockets
func (u *UDPAdapterLinux) Close() error {
	u.closeOnce.Do(func() {
		u.closed.Store(true)
		for _, c := range u.conns {
			c.Close()
		}
	})
	return nil
}
