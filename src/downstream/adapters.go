package downstream

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/udp"
)

// UDPAdapter is a thin wrapper around the optimized UDP receiver.
type UDPAdapter struct {
	inner *udp.UDPAdapterLinux
}

// NewUDPAdapter creates a new instance with parameters from TransferConfiguration.
func NewUDPAdapter(config TransferConfiguration) *UDPAdapter {
	addr := fmt.Sprintf("%s:%d", config.targetIP, config.targetPort)
	a := &UDPAdapter{inner: udp.NewUDPAdapterLinux(addr)}

	a.inner.Setup(
		config.mtu,
		config.numReceivers,
		config.channelBufferSize,
		config.readBufferMultiplier,
	)

	return a
}

// Setup allows overriding tuning parameters at runtime (e.g., from CLI or config reload).
func (a *UDPAdapter) Setup(
	mtu uint16,
	numReceivers int,
	channelBufferSize int,
	readBufferMultiplier uint16,
) {
	a.inner.Setup(mtu, numReceivers, channelBufferSize, readBufferMultiplier)
}

// Listen delegates to UDPAdapterLinux.Listen, converting stopChan to a proper channel.
func (a *UDPAdapter) Listen(
	address string,
	port int,
	rcvBufSize int,
	callback func([]byte),
	payloadSize uint16,
	stopChan <-chan struct{},
	_ int, // numReceivers unused (already configured in Setup)
) {
	realStop := make(chan struct{})
	if stopChan != nil {
		go func() {
			<-stopChan
			close(realStop)
		}()
	}
	a.inner.Listen(address, port, rcvBufSize, callback, realStop)
}

// Close gracefully shuts down the UDP listener.
func (a *UDPAdapter) Close() error {
	return a.inner.Close()
}

//
// ────────────────────────────── TCP Adapter ───────────────────────────────
//

// TCPAdapter implements TransportReceiver for TCP connections.
// It listens for incoming TCP connections and parses messages from the same protocol format as UDP.
type TCPAdapter struct {
	listener   net.Listener
	stopChan   <-chan struct{}
	wg         sync.WaitGroup
	closed     bool
	closeMutex sync.Mutex
}

// NewTCPAdapter creates a new TCP receiver adapter
func NewTCPAdapter(config TransferConfiguration) *TCPAdapter {
	return &TCPAdapter{}
}

// Setup initializes the TCP adapter (no-op for TCP as it doesn't need tuning like UDP)
func (t *TCPAdapter) Setup(
	_ uint16,
	_ int,
	_ int,
	_ uint16,
) {
	// TCP doesn't require setup tuning like UDP does
}

// Listen starts listening for incoming TCP connections on the specified address and port
// It accepts connections and reads messages using the same message parsing as UDP
func (t *TCPAdapter) Listen(
	ip string,
	port int,
	_ int, // rcvBufSize unused for TCP
	callback func([]byte),
	_ uint16, // mtu unused for TCP
	stopChan <-chan struct{},
	_ int, // numReceivers unused for TCP
) {
	addr := fmt.Sprintf("%s:%d", ip, port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		Logger.Fatalf("Failed to listen on TCP %s: %v", addr, err)
	}

	t.listener = listener
	t.stopChan = stopChan
	Logger.Infof("TCP listener started on %s", addr)

	// Accept connections in a goroutine
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		for {
			select {
			case <-stopChan:
				return
			default:
			}

			// Set a timeout for Accept so we can check stopChan periodically
			listener.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))

			conn, err := listener.Accept()
			if err != nil {
				// Check if this is a timeout (expected during normal shutdown)
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				// If we're stopping, don't log the error
				if t.isClosed() {
					return
				}
				Logger.Warnf("Error accepting TCP connection: %v", err)
				continue
			}

			// Handle each connection in its own goroutine
			t.wg.Add(1)
			go t.handleConnection(conn, callback, stopChan)
		}
	}()
}

// handleConnection reads messages from a TCP connection and invokes the callback
func (t *TCPAdapter) handleConnection(conn net.Conn, callback func([]byte), stopChan <-chan struct{}) {
	defer t.wg.Done()
	defer conn.Close()

	reader := bufio.NewReader(conn)
	remoteAddr := conn.RemoteAddr().String()
	Logger.Infof("New TCP connection from %s", remoteAddr)

	for {
		select {
		case <-stopChan:
			return
		default:
		}

		// Set read deadline to avoid blocking indefinitely
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))

		// Read a message from the connection
		// The protocol message format includes a length prefix (first 4 bytes)
		msg, err := t.readMessage(reader)
		if err != nil {
			if t.isClosed() {
				return
			}
			Logger.Debugf("TCP read error from %s: %v", remoteAddr, err)
			return
		}

		if len(msg) > 0 {
			callback(msg)
		}
	}
}

// readMessage reads a single message from the TCP connection
// The protocol message format is self-describing:
// - 1 byte: MessageType
// - 2 bytes: MessageNumber (big-endian)
// - 2 bytes: NrMessages (big-endian)
// - 2 bytes: Length of ID (big-endian)
// - N bytes: ID
// - 4 bytes: Checksum
// - 2 bytes: Length of payload (big-endian)
// - M bytes: Payload
func (t *TCPAdapter) readMessage(reader *bufio.Reader) ([]byte, error) {
	// Read the fixed header (1 + 2 + 2 + 2 = 7 bytes)
	headerBytes := make([]byte, 7)
	_, err := io.ReadFull(reader, headerBytes)
	if err != nil {
		return nil, err
	}

	// Header structure:
	// Byte 0: MessageType
	// Bytes 1-2: MessageNumber (big-endian)
	// Bytes 3-4: NrMessages (big-endian)
	// Bytes 5-6: Length of ID (big-endian)

	// Parse the ID length from bytes 5-6
	idLen := int(headerBytes[5])<<8 | int(headerBytes[6])

	if idLen < 0 || idLen > 1024 {
		return nil, fmt.Errorf("invalid ID length: %d", idLen)
	}

	// Read the ID bytes and checksum (idLen + 4 bytes)
	idAndChecksum := make([]byte, idLen+4)
	_, err = io.ReadFull(reader, idAndChecksum)
	if err != nil {
		return nil, err
	}

	// Read the payload length (2 bytes, big-endian)
	payloadLenBytes := make([]byte, 2)
	_, err = io.ReadFull(reader, payloadLenBytes)
	if err != nil {
		return nil, err
	}

	payloadLen := int(payloadLenBytes[0])<<8 | int(payloadLenBytes[1])

	if payloadLen < 0 || payloadLen > 65535 {
		return nil, fmt.Errorf("invalid payload length: %d", payloadLen)
	}

	// Read the payload
	payload := make([]byte, payloadLen)
	_, err = io.ReadFull(reader, payload)
	if err != nil {
		return nil, err
	}

	// Reconstruct the full message
	fullMsg := make([]byte, 0, 7+idLen+4+2+payloadLen)
	fullMsg = append(fullMsg, headerBytes...)
	fullMsg = append(fullMsg, idAndChecksum...)
	fullMsg = append(fullMsg, payloadLenBytes...)
	fullMsg = append(fullMsg, payload...)

	return fullMsg, nil
}

// Close gracefully shuts down the TCP listener and active connections
func (t *TCPAdapter) Close() error {
	t.closeMutex.Lock()
	defer t.closeMutex.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	if t.listener != nil {
		t.listener.Close()
	}

	// Wait for all goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		Logger.Infof("TCP adapter closed gracefully")
	case <-time.After(5 * time.Second):
		Logger.Warnf("TCP adapter close timeout")
	}

	return nil
}

func (t *TCPAdapter) isClosed() bool {
	t.closeMutex.Lock()
	defer t.closeMutex.Unlock()
	return t.closed
}

//
// ────────────────────────────── Kafka Adapter ───────────────────────────────
//

type KafkaAdapter struct{}

func NewKafkaAdapter() *KafkaAdapter { return &KafkaAdapter{} }

func (a *KafkaAdapter) Write(key string, topic string, partition int32, message []byte) error {
	kafka.WriteToKafka(key, topic, partition, message)
	return nil
}

func (a *KafkaAdapter) Flush() {
	kafka.StopBackgroundThread()
}

func (a *KafkaAdapter) Close() error {
	kafka.StopBackgroundThread()
	kafka.CloseProducer()
	return nil
}

//
// ────────────────────────────── Cmd Adapter ───────────────────────────────
//

// CmdAdapter writes to stdout for debugging or CLI testing.
type CmdAdapter struct{}

func NewCmdAdapter() *CmdAdapter { return &CmdAdapter{} }

func (a *CmdAdapter) Write(_ string, _ string, _ int32, message []byte) error {
	println(string(message))
	return nil
}

func (a *CmdAdapter) Flush()       {}
func (a *CmdAdapter) Close() error { return nil }

//
// ────────────────────────────── Null Adapter ───────────────────────────────
//

// NullAdapter is used for performance benchmarking and profiling.
type NullAdapter struct {
	numMessages int64
	startTime   time.Time
}

func NewNullAdapter() *NullAdapter { return &NullAdapter{} }

func (a *NullAdapter) Write(_ string, _ string, _ int32, _ []byte) error {
	atomic.AddInt64(&a.numMessages, 1)
	if a.startTime.IsZero() {
		a.startTime = time.Now()
	}
	return nil
}

func (a *NullAdapter) Flush() {
	total := atomic.LoadInt64(&a.numMessages)
	Logger.Printf("NullAdapter flush: %d messages total", total)

	if !a.startTime.IsZero() {
		elapsed := time.Since(a.startTime)
		if elapsed > 0 {
			eps := float64(total) / elapsed.Seconds()
			Logger.Printf("Processed %d messages in %s (%.2f EPS)",
				total, elapsed.Truncate(time.Millisecond), eps)
		}
	}
}

func (a *NullAdapter) Close() error { return nil }
