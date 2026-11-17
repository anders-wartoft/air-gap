package udp

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sys/unix"
	"sitia.nu/airgap/src/logging"
)

// SO_RXQ_OVFL enables receiving packet drop counts in ancillary data
// Value is 40 on Linux (see linux/socket.h)
const SO_RXQ_OVFL = 40

const receiveBatchSize = 64

type UDPAdapterLinux struct {
	addr                 string
	connsMu              sync.Mutex
	conns                []*net.UDPConn
	closed               atomic.Bool
	closeOnce            sync.Once
	done                 chan struct{}
	numReceivers         int
	channelBufferSize    int
	mtu                  uint16
	readBufferMultiplier uint16
	enableRxqOvfl        bool
}

var Logger = logging.Logger

// Global atomic counters for SO_RXQ_OVFL statistics
var (
	RxqOvflDrops          atomic.Uint64 // Drops since last statistics reset
	RxqOvflDropsTotal     atomic.Uint64 // Total drops since start
	ReadBatchesTotal      atomic.Uint64
	ReadDatagramsTotal    atomic.Uint64
	QueueBatchesHighWater atomic.Uint64
)

type packetBatch struct {
	data    []byte
	packets [][]byte
	count   int
}

// Keep one compact owned allocation per batch, not one full-size buffer per
// queued datagram. The read buffers remain private to each socket reader.
func (b *packetBatch) copyMessages(messages []ipv4.Message) {
	total := 0
	for i := range messages {
		if messages[i].Flags&unix.MSG_TRUNC == 0 {
			total += messages[i].N
		}
	}
	if cap(b.data) < total || cap(b.data) > 2*total {
		b.data = make([]byte, total)
	} else {
		b.data = b.data[:total]
	}
	if cap(b.packets) < len(messages) || cap(b.packets) > 2*len(messages) {
		b.packets = make([][]byte, len(messages))
	} else {
		b.packets = b.packets[:len(messages)]
	}
	position := 0
	for i := range messages {
		message := &messages[i]
		if message.Flags&unix.MSG_TRUNC != 0 {
			continue
		}
		end := position + message.N
		copy(b.data[position:end], message.Buffers[0][:message.N])
		b.packets[b.count] = b.data[position:end:end]
		b.count++
		position = end
	}
}

func (b *packetBatch) reset() {
	clear(b.packets[:b.count])
	b.count = 0
}

func recordRxqOverflow(last *uint32, count uint32) {
	delta := count - *last
	*last = count
	if delta != 0 {
		RxqOvflDrops.Add(uint64(delta))
		RxqOvflDropsTotal.Add(uint64(delta))
	}
}

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
		done:                 make(chan struct{}),
	}
}

func (u *UDPAdapterLinux) listenSocket(address string, rcvBufSize int) (*net.UDPConn, int, error) {
	var effectiveBuffer int
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var optionErr error
			err := c.Control(func(fd uintptr) {
				options := []struct{ name, value int }{
					{unix.SO_REUSEPORT, 1},
					{unix.SO_REUSEADDR, 1},
				}
				if rcvBufSize > 0 {
					options = append(options, struct{ name, value int }{unix.SO_RCVBUF, rcvBufSize})
				}
				if u.enableRxqOvfl {
					options = append(options, struct{ name, value int }{SO_RXQ_OVFL, 1})
				}
				for _, option := range options {
					if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, option.name, option.value); err != nil {
						optionErr = fmt.Errorf("set socket option %d: %w", option.name, err)
						return
					}
				}
				effectiveBuffer, optionErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
			})
			if err != nil {
				return err
			}
			return optionErr
		},
	}
	conn, err := lc.ListenPacket(context.Background(), "udp", address)
	if err != nil {
		return nil, 0, err
	}
	udpConn := conn.(*net.UDPConn)
	u.connsMu.Lock()
	defer u.connsMu.Unlock()
	if u.closed.Load() {
		udpConn.Close()
		return nil, 0, net.ErrClosed
	}
	u.conns = append(u.conns, udpConn)
	return udpConn, effectiveBuffer, nil
}

// Listen starts multiple sockets for SO_REUSEPORT. The handler must copy any
// packet data it needs to retain after returning.
func (u *UDPAdapterLinux) Listen(ip string, port int, rcvBufSize int, handler func([]byte), stopChan <-chan struct{}) {
	addrStr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
	Logger.Infof(logging.Event(logging.EventUDPBind,
		logging.Str("server.address", ip),
		logging.Int("server.port", int64(port)),
		logging.Int("airgap.udp.workers", int64(u.numReceivers)),
		logging.Int("airgap.udp.mtu", int64(u.mtu)),
	), "UDP listener starting on %s with %d workers (MTU=%d)", addrStr, u.numReceivers, u.mtu)

	batchSize := min(receiveBatchSize, u.channelBufferSize)
	if runtime.GOOS != "linux" {
		batchSize = 1
	}
	queueSize := u.channelBufferSize
	packetChan := make(chan *packetBatch, queueSize)
	queueSpace := semaphore.NewWeighted(int64(u.channelBufferSize))
	free := make(chan *packetBatch, max(1, u.channelBufferSize/batchSize)+2*u.numReceivers)
	recycle := func(batch *packetBatch) {
		batch.reset()
		select {
		case free <- batch:
		default:
			// Only the storage cache is full; the batch was already handled.
		}
	}
	var workers, readers sync.WaitGroup
	for i := 0; i < u.numReceivers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for batch := range packetChan {
				queueSpace.Release(int64(batch.count))
				for _, pkt := range batch.packets[:batch.count] {
					handler(pkt)
				}
				recycle(batch)
			}
		}()
	}

	for i := 0; i < u.numReceivers; i++ {
		conn, effectiveBuffer, err := u.listenSocket(addrStr, rcvBufSize)
		if err != nil {
			if u.closed.Load() {
				break
			}
			Logger.Fatalf(logging.Event(logging.EventUDPBind,
				logging.Failure(),
				logging.Str("server.address", ip),
				logging.Int("server.port", int64(port)),
				logging.ErrorType(err),
			), "Failed to listen on %s: %v", addrStr, err)
		}
		Logger.Infof(logging.Event(logging.EventUDPBind,
			logging.Int("airgap.udp.socket.index", int64(i)),
			logging.Int("airgap.udp.receive_buffer.requested", int64(rcvBufSize)),
			logging.Int("airgap.udp.receive_buffer.effective", int64(effectiveBuffer)),
			logging.Int("airgap.udp.batch.max_datagrams", int64(batchSize)),
			logging.Int("airgap.udp.queue.max_datagrams", int64(u.channelBufferSize)),
		), "UDP socket %d: SO_RCVBUF requested=%d effective=%d, batch=%d, queue=%d datagrams",
			i, rcvBufSize, effectiveBuffer, batchSize, queueSize)
		readers.Add(1)
		go func(c *net.UDPConn) {
			defer readers.Done()
			type batchReader interface {
				ReadBatch([]ipv4.Message, int) (int, error)
			}
			var reader batchReader = ipv4.NewPacketConn(c)
			if c.LocalAddr().(*net.UDPAddr).IP.To4() == nil {
				reader = ipv6.NewPacketConn(c)
			}
			messages := make([]ipv4.Message, batchSize)
			readSize := int(min(uint64(u.mtu)*uint64(u.readBufferMultiplier), uint64(65535)))
			for i := range messages {
				messages[i].Buffers = [][]byte{make([]byte, readSize)}
				if u.enableRxqOvfl {
					messages[i].OOB = make([]byte, unix.CmsgSpace(4))
				}
			}
			var lastDropCount uint32
			for {
				var batch *packetBatch
				select {
				case <-u.done:
					return
				default:
				}
				select {
				case batch = <-free:
				default:
					batch = &packetBatch{}
				}
				n, err := reader.ReadBatch(messages, 0)
				if err != nil {
					recycle(batch)
					if u.closed.Load() {
						return
					}
					Logger.Errorf(logging.Event(logging.EventUDPReceive,
						logging.Failure(),
						logging.Str("server.address", addrStr),
						logging.ErrorType(err),
					), "UDP batch read error (%d datagrams, incomplete metadata): %v", n, err)
					continue
				}
				if n > 0 {
					ReadBatchesTotal.Add(1)
					ReadDatagramsTotal.Add(uint64(n))
					for i := range messages[:n] {
						message := &messages[i]
						if message.Flags&unix.MSG_TRUNC != 0 {
							Logger.Errorf(logging.Event(logging.EventUDPReceive, logging.Failure()),
								"UDP datagram exceeds receive buffer (%d bytes); discarded", len(message.Buffers[0]))
						}
						if message.Flags&unix.MSG_CTRUNC != 0 {
							Logger.Error(logging.Event(logging.EventUDPReceive, logging.Failure()),
								"UDP ancillary data truncated; socket drop statistics may be incomplete")
						} else if u.enableRxqOvfl && message.NN > 0 {
							u.recordControl(message.OOB[:message.NN], &lastDropCount)
						}
					}
					batch.copyMessages(messages[:n])
					if batch.count == 0 {
						recycle(batch)
						continue
					}
					if err := queueSpace.Acquire(context.Background(), int64(batch.count)); err != nil {
						Logger.Errorf(logging.Event(logging.EventUDPReceive, logging.Failure()).Merge(logging.Err(err)),
							"Cannot reserve UDP queue space: %v", err)
						recycle(batch)
						return
					}
					// Drain successfully read batches even when Close races
					// with enqueueing; workers exit only after readers finish.
					packetChan <- batch
					depth := uint64(len(packetChan))
					for old := QueueBatchesHighWater.Load(); depth > old; old = QueueBatchesHighWater.Load() {
						if QueueBatchesHighWater.CompareAndSwap(old, depth) {
							break
						}
					}
					batch = nil
				}
				if batch != nil {
					recycle(batch)
				}
			}
		}(conn)
	}

	select {
	case <-stopChan:
	case <-u.done:
	}
	u.Close()

	timeout := time.After(2 * time.Second)
	done := make(chan struct{})
	go func() {
		readers.Wait()
		close(packetChan)
		workers.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-timeout:
		Logger.Warn(logging.Event(logging.EventAppShutdown,
			logging.Failure(),
			logging.Str("server.address", addrStr),
			logging.Str("airgap.shutdown.reason", "worker.timeout"),
		), "UDPAdapter shutdown timeout reached, some messages may be lost")
		<-done
	}
}

func (u *UDPAdapterLinux) recordControl(oob []byte, last *uint32) {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		Logger.Errorf(logging.Event(logging.EventUDPReceive, logging.Failure()).Merge(logging.Err(err)),
			"Cannot parse UDP ancillary data: %v", err)
		return
	}
	for _, message := range messages {
		if message.Header.Level == unix.SOL_SOCKET && message.Header.Type == SO_RXQ_OVFL {
			if len(message.Data) < 4 {
				Logger.Error(logging.Event(logging.EventUDPReceive, logging.Failure()), "UDP socket drop counter is shorter than four bytes")
				continue
			}
			recordRxqOverflow(last, binary.NativeEndian.Uint32(message.Data[:4]))
		}
	}
}

// Close closes all sockets
func (u *UDPAdapterLinux) Close() error {
	u.closeOnce.Do(func() {
		u.connsMu.Lock()
		defer u.connsMu.Unlock()
		u.closed.Store(true)
		close(u.done)
		for _, c := range u.conns {
			c.Close()
		}
	})
	return nil
}
