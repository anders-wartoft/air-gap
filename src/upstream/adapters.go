package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
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

func NewUDPAdapter(address, nic string) (*UDPAdapter, error) {
	c, err := udp.NewUDPConnWithNIC(address, nic)
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
	conn      net.Conn
	addr      string
	mu        sync.Mutex
	err       error
	tlsConfig *tls.Config // nil for plain TCP
	// TLS cert hot-reload: original params stored so ReloadTLSConfig can rebuild on SIGHUP
	tlsCAFile          string
	tlsCertFile        string
	tlsKeyFile         string
	tlsKeyPasswordFile string
	tlsCipherSuites    string
	tlsServerCNRegex   string
}

// buildUpstreamTLSConfig constructs a *tls.Config for the upstream TCP client.
// It loads the CA cert (required), and optionally the client cert+key for mTLS.
//
// cipherSuites controls the TLS protocol version policy:
//   - empty string or "TLS1.3" → enforce TLS 1.3 only (recommended, NIST-approved suites only)
//   - comma-separated cipher suite names → use TLS 1.2 with those specific NIST-approved suites;
//     only names from crypto/tls.CipherSuites() are accepted
func buildUpstreamTLSConfig(caFile, certFile, keyFile, keyPasswordFile, cipherSuites, serverCNRegex string) (*tls.Config, error) {
	// Load CA cert to verify the server
	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("tcpTLS: failed to read CA file %q: %w", caFile, err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("tcpTLS: no valid CA certificates found in %q", caFile)
	}

	// Compile CN regex (optional)
	var cnRegex *regexp.Regexp
	if serverCNRegex != "" {
		cnRegex, err = regexp.Compile(serverCNRegex)
		if err != nil {
			return nil, fmt.Errorf("tcpTLS: invalid tcpTLSServerCNRegex %q: %w", serverCNRegex, err)
		}
	}

	cfg := &tls.Config{
		// Skip Go's built-in hostname verification so we can connect by IP or through
		// a load balancer. Chain validation and CN regex matching are done manually
		// inside VerifyConnection, so security is not reduced.
		InsecureSkipVerify: true, //nolint:gosec // lgtm[go/disabled-certificate-check]
		VerifyConnection: func(cs tls.ConnectionState) error {
			Logger.Debugf("[TLS upstream] Handshake complete, peer certs: %d, %s / %s", len(cs.PeerCertificates), tlsVersionName(cs.Version), tls.CipherSuiteName(cs.CipherSuite))
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("tcpTLS: server sent no certificate")
			}
			leaf := cs.PeerCertificates[0]
			Logger.Debugf("[TLS upstream] Server cert subject: %s", leaf.Subject.String())
			Logger.Debugf("[TLS upstream] Server cert issuer:  %s", leaf.Issuer.String())
			Logger.Debugf("[TLS upstream] Server cert CN:      %s", leaf.Subject.CommonName)
			Logger.Debugf("[TLS upstream] Server cert not-before: %s, not-after: %s", leaf.NotBefore, leaf.NotAfter)
			switch pub := leaf.PublicKey.(type) {
			case interface{ Size() int }: // *rsa.PublicKey
				bits := pub.Size() * 8
				Logger.Debugf("[TLS upstream] Server cert public key: RSA %d-bit", bits)
				if bits > 8192 {
					Logger.Errorf("[TLS upstream] Server cert RSA key is %d bits — Go rejects RSA keys > 8192 bits. Regenerate the server certificate with a 2048- or 4096-bit key.", bits)
				}
			default:
				Logger.Debugf("[TLS upstream] Server cert public key type: %T", leaf.PublicKey)
			}
			if len(leaf.DNSNames) > 0 {
				Logger.Debugf("[TLS upstream] Server cert SAN DNS: %v", leaf.DNSNames)
			}
			if len(leaf.IPAddresses) > 0 {
				Logger.Debugf("[TLS upstream] Server cert SAN IPs: %v", leaf.IPAddresses)
			}
			// Manually verify the certificate chain against our CA pool
			opts := x509.VerifyOptions{
				Roots:         caPool,
				CurrentTime:   time.Now(),
				Intermediates: x509.NewCertPool(),
				// Use ExtKeyUsageAny so infrastructure certs without an EKU extension pass
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			}
			for _, c := range cs.PeerCertificates[1:] {
				Logger.Debugf("[TLS upstream] Adding intermediate cert: %s", c.Subject.String())
				opts.Intermediates.AddCert(c)
			}
			if _, err := leaf.Verify(opts); err != nil {
				Logger.Debugf("[TLS upstream] Chain verification failed: %v", err)
				return fmt.Errorf("tcpTLS: server certificate chain verification failed: %w", err)
			}
			Logger.Debugf("[TLS upstream] Chain verification OK")
			// Apply CN regex if configured
			if cnRegex != nil {
				cn := leaf.Subject.CommonName
				if !cnRegex.MatchString(cn) {
					Logger.Errorf("[TLS upstream] Server CN %q rejected by pattern %q — update tcpTLSServerCNRegex or regenerate the server certificate", cn, cnRegex.String())
					return fmt.Errorf("tcpTLS: server certificate CN %q does not match pattern %q", cn, cnRegex.String())
				}
				Logger.Infof("[TLS upstream] Authenticated server CN %q (pattern %q)", cn, cnRegex.String())
			} else {
				Logger.Infof("[TLS upstream] Authenticated server CN %q by CA chain (no CN regex)", leaf.Subject.CommonName)
			}
			return nil
		},
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
		ids, err := parseCipherSuites(cipherSuites)
		if err != nil {
			return nil, fmt.Errorf("tcpTLS: %w", err)
		}
		cfg.CipherSuites = ids
	}

	// Optional mTLS: load client cert + key
	if certFile != "" {
		cert, err := kafka.LoadTLSCertificate(certFile, keyFile, keyPasswordFile)
		if err != nil {
			return nil, fmt.Errorf("tcpTLS: failed to load client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// tlsVersionName returns a human-readable TLS version string.
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

// parseCipherSuites converts a comma-separated list of Go TLS cipher suite names into their IDs.
// Only cipher suites from tls.CipherSuites() (Go's secure list) are accepted.
func parseCipherSuites(csv string) ([]uint16, error) {
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

// NewTCPAdapter creates a new TCP adapter that connects to the specified address.
// Pass a non-nil tlsConfig to enable TLS (including mTLS).
// It doesn't fail if connection is not immediately available - it will attempt
// to connect lazily on first message send.
func NewTCPAdapter(address string, tlsConfig *tls.Config) (*TCPAdapter, error) {
	adapter := &TCPAdapter{
		addr:      address,
		tlsConfig: tlsConfig,
	}

	// Try to connect immediately, but don't fail if it's not available yet
	conn, err := adapter.dial()
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

// SetTLSReloadParams stores the TLS configuration parameters so the certificate
// can be reloaded on SIGHUP without restarting the process.
func (t *TCPAdapter) SetTLSReloadParams(caFile, certFile, keyFile, keyPasswordFile, cipherSuites, serverCNRegex string) {
	t.tlsCAFile = caFile
	t.tlsCertFile = certFile
	t.tlsKeyFile = keyFile
	t.tlsKeyPasswordFile = keyPasswordFile
	t.tlsCipherSuites = cipherSuites
	t.tlsServerCNRegex = serverCNRegex
}

// ReloadTLSConfig rebuilds the TLS configuration from disk, swaps it in, and closes
// the current connection so the retry loop reconnects with the new certificate.
// Intended to be called from the SIGHUP handler.
func (t *TCPAdapter) ReloadTLSConfig() error {
	if t.tlsConfig == nil {
		Logger.Debugf("[TLS upstream] ReloadTLSConfig: TLS not enabled, nothing to do")
		return nil
	}
	Logger.Debugf("[TLS upstream] ReloadTLSConfig: reloading TLS configuration")
	Logger.Debugf("[TLS upstream]   caFile:          %s", t.tlsCAFile)
	Logger.Debugf("[TLS upstream]   certFile:        %s", t.tlsCertFile)
	Logger.Debugf("[TLS upstream]   keyFile:         %s", t.tlsKeyFile)
	Logger.Debugf("[TLS upstream]   keyPasswordFile: %s", t.tlsKeyPasswordFile)
	Logger.Debugf("[TLS upstream]   cipherSuites:    %s", t.tlsCipherSuites)
	Logger.Debugf("[TLS upstream]   serverCNRegex:   %s", t.tlsServerCNRegex)

	// Log file modification times so it's easy to confirm fresh certs were picked up
	for label, path := range map[string]string{
		"CA cert":     t.tlsCAFile,
		"client cert": t.tlsCertFile,
		"client key":  t.tlsKeyFile,
	} {
		if path == "" {
			continue
		}
		if fi, err := os.Stat(path); err == nil {
			Logger.Debugf("[TLS upstream]   %s mtime: %s (size %d bytes)", label, fi.ModTime().Format(time.RFC3339), fi.Size())
		} else {
			Logger.Warnf("[TLS upstream]   %s: cannot stat %q: %v", label, path, err)
		}
	}

	newCfg, err := buildUpstreamTLSConfig(
		t.tlsCAFile, t.tlsCertFile, t.tlsKeyFile, t.tlsKeyPasswordFile,
		t.tlsCipherSuites, t.tlsServerCNRegex,
	)
	if err != nil {
		Logger.Debugf("[TLS upstream] ReloadTLSConfig: buildUpstreamTLSConfig failed: %v", err)
		return fmt.Errorf("[TLS upstream] certificate reload failed: %w", err)
	}
	Logger.Debugf("[TLS upstream] ReloadTLSConfig: new TLS config built successfully")

	t.mu.Lock()
	t.tlsConfig = newCfg
	hadConn := t.conn != nil
	if t.conn != nil {
		t.conn.Close()
		t.conn = nil
	}
	t.mu.Unlock()

	if hadConn {
		Logger.Debugf("[TLS upstream] ReloadTLSConfig: existing connection closed; retry loop will reconnect with new cert")
	} else {
		Logger.Debugf("[TLS upstream] ReloadTLSConfig: no active connection at reload time")
	}
	Logger.Infof("[TLS upstream] TLS certificates reloaded from %s; reconnecting with new cert", t.tlsCertFile)
	return nil
}

// dialTimeout is the per-attempt TCP connect timeout. Long enough to traverse a WAN
// but short enough to keep the signal-handling loop responsive.
const dialTimeout = 10 * time.Second

// dial opens a new connection to t.addr, using TLS if configured.
func (t *TCPAdapter) dial() (net.Conn, error) {
	if t.tlsConfig != nil {
		dialer := &net.Dialer{Timeout: dialTimeout}
		conn, err := tls.DialWithDialer(dialer, "tcp", t.addr, t.tlsConfig)
		if err != nil {
			errStr := err.Error()
			switch {
			case strings.Contains(errStr, "RSA key larger than"):
				Logger.Errorf("[TLS upstream] Dial %s failed: server certificate has an oversized RSA key (%s). Regenerate the server certificate with a 2048- or 4096-bit key.", t.addr, errStr)
			case strings.Contains(errStr, "certificate has expired") || strings.Contains(errStr, "certificate is not yet valid"):
				Logger.Errorf("[TLS upstream] Dial %s failed: server certificate validity period error — check system clock and certificate dates (%s)", t.addr, errStr)
			case strings.Contains(errStr, "certificate signed by unknown authority") || strings.Contains(errStr, "x509: "):
				Logger.Errorf("[TLS upstream] Dial %s failed: certificate chain error — check tcpTLSCAFile matches the server's issuing CA (%s)", t.addr, errStr)
			case strings.Contains(errStr, "bad certificate") || strings.Contains(errStr, "unknown certificate"):
				Logger.Errorf("[TLS upstream] Dial %s failed: server rejected our client certificate — check tcpTLSCertFile/tcpTLSKeyFile and that the CA matches (%s)", t.addr, errStr)
			case strings.Contains(errStr, "no such file") || strings.Contains(errStr, "connection refused"):
				// low-level network error — let caller log at appropriate level
			default:
				Logger.Errorf("[TLS upstream] Dial %s TLS error: %s", t.addr, errStr)
			}
		}
		return conn, err
	}
	return net.DialTimeout("tcp", t.addr, dialTimeout)
}

// ensureConnected ensures we have a valid connection, attempting to reconnect if needed.
// The mutex is held only when reading/writing t.conn — it is released before the
// blocking dial() call so the SIGHUP handler (ReloadTLSConfig) is never starved.
func (t *TCPAdapter) ensureConnected() bool {
	t.mu.Lock()

	// Test existing connection with a short deadline.
	if t.conn != nil {
		t.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		b := make([]byte, 1)
		_, err := t.conn.Read(b)
		t.conn.SetReadDeadline(time.Time{})
		if err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				t.conn.Close()
				t.conn = nil
			}
		}
		if t.conn != nil {
			t.mu.Unlock()
			return true
		}
	}

	// Release the mutex before the potentially-slow dial so other goroutines
	// (e.g. the SIGHUP handler calling ReloadTLSConfig) are not blocked.
	t.mu.Unlock()

	conn, err := t.dial()
	if err != nil {
		t.mu.Lock()
		t.err = err
		t.mu.Unlock()
		return false
	}

	t.mu.Lock()
	// A concurrent goroutine (e.g. ReloadTLSConfig) may have replaced t.conn while
	// we were dialing — discard our new conn and use theirs.
	if t.conn != nil {
		t.mu.Unlock()
		conn.Close()
		return true
	}
	t.conn = conn
	t.err = nil
	t.mu.Unlock()
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
	conn, err := t.dial()
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
