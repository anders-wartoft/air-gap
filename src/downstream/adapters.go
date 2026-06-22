package downstream

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
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
		config.enableRxqOvfl,
	)

	return a
}

// Setup allows overriding tuning parameters at runtime (e.g., from CLI or config reload).
func (a *UDPAdapter) Setup(
	mtu uint16,
	numReceivers int,
	channelBufferSize int,
	readBufferMultiplier uint16,
	enableRxqOvfl bool,
) {
	a.inner.Setup(mtu, numReceivers, channelBufferSize, readBufferMultiplier, enableRxqOvfl)
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
	listener       net.Listener
	stopChan       <-chan struct{}
	wg             sync.WaitGroup
	closed         bool
	closeMutex     sync.Mutex
	activeConns    atomic.Int64
	maxConnections int            // configured via maxTCPConnections; caps goroutine exhaustion
	tlsConfig      *tls.Config    // nil for plain TCP
	clientCNRegex  *regexp.Regexp // nil if no CN validation required
	// TLS cert hot-reload: file paths stored so ReloadTLSCert can re-read them on SIGHUP
	certFile        string
	keyFile         string
	keyPasswordFile string
	certPtr         atomic.Pointer[tls.Certificate] // serves every new TLS handshake
}

// buildDownstreamTLSConfig constructs a *tls.Config for the downstream TCP server.
// certFile and keyFile are required. caFile is required when clientAuth is "allow" or "require".
//
// cipherSuites controls the TLS protocol version policy:
//   - empty string or "TLS1.3" → enforce TLS 1.3 only (recommended, NIST-approved suites only)
//   - comma-separated cipher suite names → use TLS 1.2 with those specific NIST-approved suites;
//     only names from crypto/tls.CipherSuites() are accepted
func buildDownstreamTLSConfig(certFile, keyFile, keyPasswordFile, caFile, clientAuth, cipherSuites string) (*tls.Config, error) {
	cert, err := kafka.LoadTLSCertificate(certFile, keyFile, keyPasswordFile)
	if err != nil {
		return nil, fmt.Errorf("tcpTLS: failed to load server certificate: %w", err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	// Apply TLS version and cipher suite policy
	upper := strings.ToUpper(strings.TrimSpace(cipherSuites))
	if upper == "" || upper == "TLS1.3" {
		// Enforce TLS 1.3 only. Cipher suites for 1.3 are fixed by the standard
		// (TLS_AES_128_GCM_SHA256, TLS_AES_256_GCM_SHA384, TLS_CHACHA20_POLY1305_SHA256)
		// and cannot be configured — all are NIST-approved.
		cfg.MinVersion = tls.VersionTLS13
	} else {
		// TLS 1.2 with caller-specified NIST-approved cipher suites
		cfg.MinVersion = tls.VersionTLS12
		ids, err := parseDownstreamCipherSuites(cipherSuites)
		if err != nil {
			return nil, fmt.Errorf("tcpTLS: %w", err)
		}
		cfg.CipherSuites = ids
	}

	// Set client auth policy
	switch clientAuth {
	case "require":
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	case "allow":
		cfg.ClientAuth = tls.RequestClientCert
	default: // "none"
		cfg.ClientAuth = tls.NoClientCert
	}

	// Load CA pool for verifying client certificates
	if caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("tcpTLS: failed to read CA file %q: %w", caFile, err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("tcpTLS: no valid CA certificates found in %q", caFile)
		}
		cfg.ClientCAs = caPool
	}

	return cfg, nil
}

// parseDownstreamCipherSuites converts a comma-separated list of cipher suite names to IDs.
// Only cipher suites from tls.CipherSuites() (Go's secure list) are accepted.
func parseDownstreamCipherSuites(csv string) ([]uint16, error) {
	secure := tls.CipherSuites()
	byName := make(map[string]uint16, len(secure))
	for _, cs := range secure {
		byName[cs.Name] = cs.ID
	}
	var ids []uint16
	for _, name := range strings.Split(csv, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		id, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown or insecure cipher suite %q; use a name from crypto/tls.CipherSuites()", name)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("tcpTLSCipherSuites is set but contains no valid entries")
	}
	return ids, nil
}

// NewTCPAdapter creates a new TCP receiver adapter
func NewTCPAdapter(config TransferConfiguration) *TCPAdapter {
	adapter := &TCPAdapter{
		maxConnections: config.maxTCPConnections,
	}

	if config.tcpTLSCertFile != "" {
		tlsCfg, err := buildDownstreamTLSConfig(
			config.tcpTLSCertFile,
			config.tcpTLSKeyFile,
			config.tcpTLSKeyPasswordFile,
			config.tcpTLSCAFile,
			config.tcpTLSClientAuth,
			config.tcpTLSCipherSuites,
		)
		if err != nil {
			Logger.Fatalf("Error building TCP TLS configuration: %v", err)
		}
		// Store cert files for hot-reload on SIGHUP
		adapter.certFile = config.tcpTLSCertFile
		adapter.keyFile = config.tcpTLSKeyFile
		adapter.keyPasswordFile = config.tcpTLSKeyPasswordFile
		// Move the initial cert into the atomic pointer and wire up GetCertificate
		// so every new handshake picks up the latest cert without restarting.
		initialCert := tlsCfg.Certificates[0]
		adapter.certPtr.Store(&initialCert)
		tlsCfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return adapter.certPtr.Load(), nil
		}
		tlsCfg.Certificates = nil
		adapter.tlsConfig = tlsCfg
		Logger.Printf("TCP TLS configured: clientAuth=%s", config.tcpTLSClientAuth)
	}

	if config.tcpTLSClientCNRegex != "" {
		re, err := regexp.Compile(config.tcpTLSClientCNRegex)
		if err != nil {
			Logger.Fatalf("Invalid tcpTLSClientCNRegex %q: %v", config.tcpTLSClientCNRegex, err)
		}
		adapter.clientCNRegex = re
	}

	return adapter
}

// Setup initializes the TCP adapter (no-op for TCP as it doesn't need tuning like UDP)
func (t *TCPAdapter) Setup(
	_ uint16,
	_ int,
	_ int,
	_ uint16,
	_ bool, // enableRxqOvfl unused for TCP
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
	var listener net.Listener
	var err error
	if t.tlsConfig != nil {
		listener, err = tls.Listen("tcp", addr, t.tlsConfig)
		if err != nil {
			Logger.Fatalf("Failed to listen on TLS TCP %s: %v", addr, err)
		}
		Logger.Infof("TLS TCP listener started on %s", addr)
	} else {
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			Logger.Fatalf("Failed to listen on TCP %s: %v", addr, err)
		}
		Logger.Infof("TCP listener started on %s", addr)
	}

	t.listener = listener
	t.stopChan = stopChan

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

			// Set a timeout for Accept so we can check stopChan periodically.
			// For TLS listeners the underlying type is *tls.listener (not *net.TCPListener),
			// so we use the net.Listener SetDeadline method via a type assertion to net.Conn,
			// or simply call the listener's own deadline setter.
			if dl, ok := listener.(interface{ SetDeadline(time.Time) error }); ok {
				dl.SetDeadline(time.Now().Add(1 * time.Second))
			}

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
			if t.activeConns.Add(1) > int64(t.maxConnections) {
				t.activeConns.Add(-1)
				conn.Close()
				Logger.Warnf("Max TCP connections (%d) reached, rejecting connection from %s", t.maxConnections, conn.RemoteAddr())
				continue
			}
			Logger.Debugf("[TLS downstream] Accepted TCP connection from %s (active: %d)", conn.RemoteAddr(), t.activeConns.Load())
			t.wg.Add(1)
			go t.handleConnection(conn, callback, stopChan)
		}
	}()
}

// tlsVersionName returns a human-readable TLS version string for a ConnectionState.Version value.
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("TLS 0x%04x", v)
	}
}

// ReloadTLSCert re-reads the server certificate and key from disk and atomically
// replaces the live certificate. Existing connections are not affected; the new
// cert is used for every handshake that starts after this call returns.
// Intended to be called from the SIGHUP handler.
func (t *TCPAdapter) ReloadTLSCert() error {
	if t.tlsConfig == nil {
		return nil // TLS not enabled
	}
	cert, err := kafka.LoadTLSCertificate(t.certFile, t.keyFile, t.keyPasswordFile)
	if err != nil {
		return fmt.Errorf("[TLS downstream] certificate reload failed: %w", err)
	}
	t.certPtr.Store(&cert)
	Logger.Infof("[TLS downstream] Server certificate reloaded from %s", t.certFile)
	return nil
}

// logTLSHandshakeError logs a TLS handshake failure with a human-readable hint.
func logTLSHandshakeError(remoteAddr string, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "RSA key larger than"):
		Logger.Errorf("[TLS downstream] Handshake failed from %s: client certificate has an oversized RSA key — regenerate with a 2048- or 4096-bit key (%s)", remoteAddr, msg)
	case strings.Contains(msg, "certificate has expired") || strings.Contains(msg, "certificate is not yet valid"):
		Logger.Errorf("[TLS downstream] Handshake failed from %s: certificate validity period error — check system clock and certificate dates (%s)", remoteAddr, msg)
	case strings.Contains(msg, "certificate signed by unknown authority") || strings.Contains(msg, "x509: "):
		Logger.Errorf("[TLS downstream] Handshake failed from %s: certificate chain error — check tcpTLSCAFile matches the client's issuing CA (%s)", remoteAddr, msg)
	case strings.Contains(msg, "bad certificate") || strings.Contains(msg, "unknown certificate"):
		Logger.Warnf("[TLS downstream] Handshake failed from %s: remote rejected our server certificate — check tcpTLSCertFile/tcpTLSKeyFile on this side (%s)", remoteAddr, msg)
	case strings.Contains(msg, "no certificate"):
		Logger.Warnf("[TLS downstream] Handshake failed from %s: client sent no certificate (clientAuth=require needs a client cert) (%s)", remoteAddr, msg)
	default:
		Logger.Warnf("[TLS downstream] Handshake failed from %s: %s", remoteAddr, msg)
	}
}

// logClientKeyInfo logs the public key type and size of a peer certificate at DEBUG level.
func logClientKeyInfo(cert *x509.Certificate) {
	switch pub := cert.PublicKey.(type) {
	case interface{ Size() int }: // *rsa.PublicKey
		bits := pub.Size() * 8
		Logger.Debugf("[TLS downstream] Client cert public key: RSA %d-bit", bits)
		if bits > 8192 {
			Logger.Errorf("[TLS downstream] Client cert RSA key is %d bits — Go rejects RSA keys > 8192 bits. Regenerate the client certificate with a 2048- or 4096-bit key.", bits)
		}
	default:
		Logger.Debugf("[TLS downstream] Client cert public key type: %T", cert.PublicKey)
	}
}

// handleConnection reads messages from a TCP connection and invokes the callback
func (t *TCPAdapter) handleConnection(conn net.Conn, callback func([]byte), stopChan <-chan struct{}) {
	defer t.wg.Done()
	defer t.activeConns.Add(-1)
	defer conn.Close()

	remoteAddr := conn.RemoteAddr().String()

	// For TLS connections, verify the client CN if a regex is configured
	if t.clientCNRegex != nil {
		if tlsConn, ok := conn.(*tls.Conn); ok {
			// Complete the TLS handshake so we can inspect peer certificates
			Logger.Debugf("[TLS downstream] Starting handshake with %s", remoteAddr)
			if err := tlsConn.Handshake(); err != nil {
				logTLSHandshakeError(remoteAddr, err)
				return
			}
			state := tlsConn.ConnectionState()
			Logger.Debugf("[TLS downstream] Handshake complete with %s, peer certs: %d, %s / %s", remoteAddr, len(state.PeerCertificates), tlsVersionName(state.Version), tls.CipherSuiteName(state.CipherSuite))
			if len(state.PeerCertificates) == 0 {
				Logger.Warnf("[TLS downstream] No client certificate presented from %s (clientAuth=require) — rejecting", remoteAddr)
				return
			}
			leaf := state.PeerCertificates[0]
			Logger.Debugf("[TLS downstream] Client cert subject: %s", leaf.Subject.String())
			Logger.Debugf("[TLS downstream] Client cert issuer:  %s", leaf.Issuer.String())
			Logger.Debugf("[TLS downstream] Client cert CN:      %s", leaf.Subject.CommonName)
			logClientKeyInfo(leaf)
			cn := leaf.Subject.CommonName
			if !t.clientCNRegex.MatchString(cn) {
				Logger.Warnf("[TLS downstream] Client CN %q from %s rejected by pattern %q — update tcpTLSClientCNRegex or regenerate the client certificate", cn, remoteAddr, t.clientCNRegex.String())
				return
			}
			Logger.Infof("[TLS downstream] Authenticated client CN %q from %s (pattern %q)", cn, remoteAddr, t.clientCNRegex.String())
		}
	} else if t.tlsConfig != nil {
		// TLS without CN regex: still log handshake details at DEBUG
		if tlsConn, ok := conn.(*tls.Conn); ok {
			Logger.Debugf("[TLS downstream] Starting handshake with %s (no CN check)", remoteAddr)
			if err := tlsConn.Handshake(); err != nil {
				logTLSHandshakeError(remoteAddr, err)
				return
			}
			state := tlsConn.ConnectionState()
			Logger.Debugf("[TLS downstream] Handshake complete with %s, peer certs: %d, %s / %s", remoteAddr, len(state.PeerCertificates), tlsVersionName(state.Version), tls.CipherSuiteName(state.CipherSuite))
			if len(state.PeerCertificates) > 0 {
				leaf := state.PeerCertificates[0]
				Logger.Debugf("[TLS downstream] Client cert CN: %s", leaf.Subject.CommonName)
				logClientKeyInfo(leaf)
				Logger.Infof("[TLS downstream] Authenticated client CN %q from %s (CA-chain only)", leaf.Subject.CommonName, remoteAddr)
			} else {
				Logger.Infof("[TLS downstream] TLS connection established with %s (one-way TLS, no client cert)", remoteAddr)
			}
		}
	}

	reader := bufio.NewReader(conn)
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
