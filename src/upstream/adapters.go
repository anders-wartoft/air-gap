package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"sitia.nu/airgap/src/internal/pinnedtls"
	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/udp"
)

type KafkaAdapter struct {
	settings kafka.ConsumerSettings
}

func kafkaConsumerSettings(config TransferConfiguration) (kafka.ConsumerSettings, error) {
	if config.kafkaFetchDefaultBytes <= 0 || config.kafkaFetchDefaultBytes > math.MaxInt32 {
		return kafka.ConsumerSettings{}, fmt.Errorf("kafkaFetchDefaultBytes must be between 1 and %d", math.MaxInt32)
	}
	if config.kafkaMaxMessageBytes < config.kafkaFetchDefaultBytes || config.kafkaMaxMessageBytes > math.MaxInt32 {
		return kafka.ConsumerSettings{}, fmt.Errorf("kafkaMaxMessageBytes must be between kafkaFetchDefaultBytes and %d", math.MaxInt32)
	}
	maxProcessingTimeMs := int64(math.MaxInt64 / int64(time.Millisecond))
	if config.kafkaMaxProcessingTimeMs <= 0 || int64(config.kafkaMaxProcessingTimeMs) > maxProcessingTimeMs {
		return kafka.ConsumerSettings{}, fmt.Errorf("kafkaMaxProcessingTimeMs must be between 1 and %d", maxProcessingTimeMs)
	}
	return kafka.ConsumerSettings{
		FetchDefaultBytes: int32(config.kafkaFetchDefaultBytes),
		FetchMaxBytes:     int32(config.kafkaMaxMessageBytes),
		ChannelBufferSize: config.kafkaChannelBufferSize,
		MaxProcessingTime: time.Duration(int64(config.kafkaMaxProcessingTimeMs)) * time.Millisecond,
	}, nil
}

func NewKafkaAdapter(config TransferConfiguration) (*KafkaAdapter, error) {
	settings, err := kafkaConsumerSettings(config)
	if err != nil {
		return nil, err
	}
	sampler := newSystemMemorySampler(config.memoryLimitBytes)
	sample, err := sampler.Sample()
	if err != nil {
		Logger.Warnf(logging.Event(logging.EventMemoryLimit,
			logging.Failure(),
			logging.Error("memory.limit.unavailable"),
		), "Memory-pressure throttling disabled because no usable memory limit could be detected: %v", err)
		return &KafkaAdapter{settings: settings}, nil
	}

	if config.goMemoryLimitPercent > 0 {
		goLimit, limitErr := applyGoMemoryLimit(sampler, config.goMemoryLimitPercent)
		if limitErr != nil {
			Logger.Errorf(logging.Event(logging.EventMemoryLimit,
				logging.Failure(),
				logging.ErrorType(limitErr),
			), "Failed to apply Go memory limit: %v", limitErr)
		} else {
			Logger.Infof(logging.Event(logging.EventMemoryLimit,
				logging.Int("airgap.memory.limit_bytes", sample.limit),
				logging.Int("airgap.memory.go_limit_bytes", goLimit),
				logging.Str("airgap.memory.source", sample.source),
			), "Applied Go memory limit %d bytes from %s limit %d bytes", goLimit, sample.source, sample.limit)
		}
	}

	controller := newMemoryController(
		sampler,
		config.memoryHighWatermarkPercent,
		config.memoryLowWatermarkPercent,
		time.Duration(config.memoryCheckIntervalMs)*time.Millisecond,
		func(throttled bool, current memorySample) {
			attrs := logging.Event(logging.EventMemoryThrottle,
				logging.Bool("airgap.memory.throttled", throttled),
				logging.Int("airgap.memory.current_bytes", current.current),
				logging.Int("airgap.memory.limit_bytes", current.limit),
				logging.Str("airgap.memory.source", current.source),
				logging.Int("airgap.memory.high_watermark_percent", int64(config.memoryHighWatermarkPercent)),
				logging.Int("airgap.memory.low_watermark_percent", int64(config.memoryLowWatermarkPercent)),
			)
			context := memoryWatermarkContext(current)
			if throttled {
				emitConfiguredMessage(parseMessageSpec(config.memoryHighWatermarkLog, logging.WARN), attrs, context)
			} else {
				emitConfiguredMessage(parseMessageSpec(config.memoryLowWatermarkLog, logging.INFO), attrs, context)
			}
		},
	)
	settings.BeforeMessage = controller.Wait
	return &KafkaAdapter{settings: settings}, nil
}

func (k *KafkaAdapter) Read(ctx context.Context, name string, offset int,
	bootstrapServers, topic, group, from string,
	handler kafka.KafkaHandler) {
	// Pass UpdateKafkaStatus as error callback to report Kafka issues
	kafka.ReadFromKafkaWithContextSettings(ctx, name, offset, bootstrapServers, topic, group, from, handler, k.settings, UpdateKafkaStatus)
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
	conn       net.Conn
	addr       string
	mu         sync.Mutex
	err        error
	tlsConfig  *tls.Config // nil for plain TCP
	pinned     bool
	pinManager *pinnedtls.Manager
	generation uint64
	closed     bool
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
		// lgtm[go/disabled-certificate-check]
		InsecureSkipVerify: true, //nolint:gosec
		VerifyConnection: func(cs tls.ConnectionState) error {
			handshake := logging.Event(logging.EventTLSHandshake,
				logging.Str("airgap.tls.role", "client"),
				logging.Str("file.path", caFile),
			)
			Logger.Debugf(handshake.With(
				logging.Int("airgap.tls.peer_certificates", int64(len(cs.PeerCertificates))),
				logging.Str("tls.protocol.version", tlsVersionName(cs.Version)),
				logging.Str("tls.cipher", tls.CipherSuiteName(cs.CipherSuite)),
			), "[TLS upstream] Handshake complete, peer certs: %d, %s / %s", len(cs.PeerCertificates), tlsVersionName(cs.Version), tls.CipherSuiteName(cs.CipherSuite))
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("tcpTLS: server sent no certificate")
			}
			leaf := cs.PeerCertificates[0]
			Logger.Debugf(handshake.With(
				logging.Str("airgap.tls.certificate.subject", leaf.Subject.String()),
			), "[TLS upstream] Server cert subject: %s", leaf.Subject.String())
			Logger.Debugf(handshake.With(
				logging.Str("airgap.tls.certificate.issuer", leaf.Issuer.String()),
			), "[TLS upstream] Server cert issuer:  %s", leaf.Issuer.String())
			Logger.Debugf(handshake.With(
				logging.Str("airgap.tls.certificate.cn", leaf.Subject.CommonName),
			), "[TLS upstream] Server cert CN:      %s", leaf.Subject.CommonName)
			Logger.Debugf(handshake.With(
				logging.Str("airgap.tls.certificate.not_before", leaf.NotBefore.Format(time.RFC3339)),
				logging.Str("airgap.tls.certificate.not_after", leaf.NotAfter.Format(time.RFC3339)),
			), "[TLS upstream] Server cert not-before: %s, not-after: %s", leaf.NotBefore, leaf.NotAfter)
			switch pub := leaf.PublicKey.(type) {
			case interface{ Size() int }: // *rsa.PublicKey
				bits := pub.Size() * 8
				Logger.Debugf(handshake.With(
					logging.Str("airgap.tls.certificate.key_type", "RSA"),
					logging.Int("airgap.tls.certificate.key_bits", int64(bits)),
				), "[TLS upstream] Server cert public key: RSA %d-bit", bits)
				if bits > 8192 {
					Logger.Errorf(handshake.With(
						logging.Failure(),
						logging.Str("airgap.tls.certificate.key_type", "RSA"),
						logging.Int("airgap.tls.certificate.key_bits", int64(bits)),
						logging.Str("error.type", "certificate.key.oversized"),
					), "[TLS upstream] Server cert RSA key is %d bits — Go rejects RSA keys > 8192 bits. Regenerate the server certificate with a 2048- or 4096-bit key.", bits)
				}
			default:
				Logger.Debugf(handshake.With(
					logging.Str("airgap.tls.certificate.key_type", fmt.Sprintf("%T", leaf.PublicKey)),
				), "[TLS upstream] Server cert public key type: %T", leaf.PublicKey)
			}
			if len(leaf.DNSNames) > 0 {
				Logger.Debugf(handshake.With(
					logging.Str("airgap.tls.certificate.san_dns", strings.Join(leaf.DNSNames, ",")),
				), "[TLS upstream] Server cert SAN DNS: %v", leaf.DNSNames)
			}
			if len(leaf.IPAddresses) > 0 {
				Logger.Debugf(handshake.With(
					logging.Int("airgap.tls.certificate.san_ip_count", int64(len(leaf.IPAddresses))),
				), "[TLS upstream] Server cert SAN IPs: %v", leaf.IPAddresses)
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
				Logger.Debugf(handshake.With(
					logging.Str("airgap.tls.certificate.subject", c.Subject.String()),
					logging.Str("airgap.tls.certificate.role", "intermediate"),
				), "[TLS upstream] Adding intermediate cert: %s", c.Subject.String())
				opts.Intermediates.AddCert(c)
			}
			if _, err := leaf.Verify(opts); err != nil {
				Logger.Debugf(handshake.With(
					logging.Failure(),
					logging.Str("error.type", "certificate.chain.invalid"),
					logging.ErrorType(err),
				), "[TLS upstream] Chain verification failed: %v", err)
				return fmt.Errorf("tcpTLS: server certificate chain verification failed: %w", err)
			}
			Logger.Debug(handshake, "[TLS upstream] Chain verification OK")
			// Apply CN regex if configured
			if cnRegex != nil {
				cn := leaf.Subject.CommonName
				if !cnRegex.MatchString(cn) {
					Logger.Errorf(handshake.With(
						logging.Failure(),
						logging.Str("airgap.tls.certificate.cn", cn),
						logging.Str("airgap.tls.server_cn_regex", cnRegex.String()),
						logging.Str("error.type", "certificate.cn.rejected"),
					), "[TLS upstream] Server CN %q rejected by pattern %q — update tcpTLSServerCNRegex or regenerate the server certificate", cn, cnRegex.String())
					return fmt.Errorf("tcpTLS: server certificate CN %q does not match pattern %q", cn, cnRegex.String())
				}
				Logger.Infof(handshake.With(
					logging.Str("airgap.tls.certificate.cn", cn),
					logging.Str("airgap.tls.server_cn_regex", cnRegex.String()),
				), "[TLS upstream] Authenticated server CN %q (pattern %q)", cn, cnRegex.String())
			} else {
				Logger.Infof(handshake.With(
					logging.Str("airgap.tls.certificate.cn", leaf.Subject.CommonName),
				), "[TLS upstream] Authenticated server CN %q by CA chain (no CN regex)", leaf.Subject.CommonName)
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
	return newTCPAdapter(address, tlsConfig, nil)
}

func newTCPAdapter(address string, tlsConfig *tls.Config, manager *pinnedtls.Manager) (*TCPAdapter, error) {
	adapter := &TCPAdapter{
		addr:       address,
		tlsConfig:  tlsConfig,
		pinManager: manager,
		pinned:     manager != nil,
	}

	// Try to connect immediately, but don't fail if it's not available yet
	conn, err := adapter.dial()
	if err != nil {
		// Store the error but don't fail - we'll retry on first message
		adapter.err = err
		Logger.Warnf(logging.Event(logging.EventTCPConnection,
			logging.Failure(),
			logging.Str("server.address", address),
			logging.Bool("airgap.tls.enabled", tlsConfig != nil),
			logging.ErrorType(err),
		), "Failed to connect to TCP server at %s on startup, will retry: %v", address, err)
	} else {
		if err := adapter.admit(conn); err != nil {
			conn.Close()
			return nil, err
		}
		adapter.conn = conn
		Logger.Infof(logging.Event(logging.EventTCPConnection,
			logging.Str("server.address", address),
			logging.Bool("airgap.tls.enabled", tlsConfig != nil),
		), "Connected to TCP server at %s", address)
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
	if t.pinned {
		return fmt.Errorf("pinned TLS requires complete configuration reload, not certificate-only reload")
	}
	reload := logging.Event(logging.EventTLSCertReload,
		logging.Str("airgap.tls.role", "client"),
		logging.Str("server.address", t.addr),
	)
	if t.tlsConfig == nil {
		Logger.Debug(reload.With(
			logging.Bool("airgap.tls.enabled", false),
		), "[TLS upstream] ReloadTLSConfig: TLS not enabled, nothing to do")
		return nil
	}
	Logger.Debug(reload, "[TLS upstream] ReloadTLSConfig: reloading TLS configuration")
	Logger.Debugf(reload.With(
		logging.Str("airgap.config.setting", "tcpTLSCAFile"),
		logging.Str("file.path", t.tlsCAFile),
	), "[TLS upstream]   caFile:          %s", t.tlsCAFile)
	Logger.Debugf(reload.With(
		logging.Str("airgap.config.setting", "tcpTLSCertFile"),
		logging.Str("file.path", t.tlsCertFile),
	), "[TLS upstream]   certFile:        %s", t.tlsCertFile)
	Logger.Debugf(reload.With(
		logging.Str("airgap.config.setting", "tcpTLSKeyFile"),
		logging.Str("file.path", t.tlsKeyFile),
	), "[TLS upstream]   keyFile:         %s", t.tlsKeyFile)
	Logger.Debugf(reload.With(
		logging.Str("airgap.config.setting", "tcpTLSKeyPasswordFile"),
	), "[TLS upstream]   keyPasswordFile: [redacted]")
	Logger.Debugf(reload.With(
		logging.Str("airgap.config.setting", "tcpTLSCipherSuites"),
		logging.Str("airgap.config.value", t.tlsCipherSuites),
	), "[TLS upstream]   cipherSuites:    %s", t.tlsCipherSuites)
	Logger.Debugf(reload.With(
		logging.Str("airgap.config.setting", "tcpTLSServerCNRegex"),
		logging.Str("airgap.config.value", t.tlsServerCNRegex),
	), "[TLS upstream]   serverCNRegex:   %s", t.tlsServerCNRegex)

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
			Logger.Debugf(reload.With(
				logging.Str("airgap.tls.file.kind", label),
				logging.Str("file.path", path),
				logging.Str("file.modified", fi.ModTime().Format(time.RFC3339)),
				logging.Int("file.size", fi.Size()),
			), "[TLS upstream]   %s mtime: %s (size %d bytes)", label, fi.ModTime().Format(time.RFC3339), fi.Size())
		} else {
			Logger.Warnf(reload.With(
				logging.Failure(),
				logging.Str("airgap.tls.file.kind", label),
				logging.Str("file.path", path),
				logging.ErrorType(err),
			), "[TLS upstream]   %s: cannot stat %q: %v", label, path, err)
		}
	}

	newCfg, err := buildUpstreamTLSConfig(
		t.tlsCAFile, t.tlsCertFile, t.tlsKeyFile, t.tlsKeyPasswordFile,
		t.tlsCipherSuites, t.tlsServerCNRegex,
	)
	if err != nil {
		Logger.Debugf(reload.With(
			logging.Failure(),
			logging.ErrorType(err),
		), "[TLS upstream] ReloadTLSConfig: buildUpstreamTLSConfig failed: %v", err)
		return fmt.Errorf("[TLS upstream] certificate reload failed: %w", err)
	}
	Logger.Debug(reload, "[TLS upstream] ReloadTLSConfig: new TLS config built successfully")

	t.mu.Lock()
	t.tlsConfig = newCfg
	t.generation++
	hadConn := t.conn != nil
	if t.conn != nil {
		t.conn.Close()
		t.conn = nil
	}
	t.mu.Unlock()

	if hadConn {
		Logger.Debug(reload.With(
			logging.Bool("airgap.transport.connected", true),
		), "[TLS upstream] ReloadTLSConfig: existing connection closed; retry loop will reconnect with new cert")
	} else {
		Logger.Debug(reload.With(
			logging.Bool("airgap.transport.connected", false),
		), "[TLS upstream] ReloadTLSConfig: no active connection at reload time")
	}
	Logger.Infof(reload.With(
		logging.Str("file.path", t.tlsCertFile),
	), "[TLS upstream] TLS certificates reloaded from %s; reconnecting with new cert", t.tlsCertFile)
	return nil
}

// dialTimeout is the per-attempt TCP connect timeout. Long enough to traverse a WAN
// but short enough to keep the signal-handling loop responsive.
const dialTimeout = 10 * time.Second

// dial opens a new connection to t.addr, using TLS if configured.
func (t *TCPAdapter) dial() (net.Conn, error) {
	return t.dialConfig(t.tlsConfig)
}

func (t *TCPAdapter) dialConfig(config *tls.Config) (net.Conn, error) {
	if config != nil {
		dialer := &net.Dialer{Timeout: dialTimeout}
		conn, err := tls.DialWithDialer(dialer, "tcp", t.addr, config)
		if err != nil {
			errStr := err.Error()
			dialFailure := logging.Event(logging.EventTLSHandshake,
				logging.Failure(),
				logging.Str("airgap.tls.role", "client"),
				logging.Str("server.address", t.addr),
				logging.Error("_OTHER"),
			)
			switch {
			case strings.Contains(errStr, "RSA key larger than"):
				Logger.Errorf(dialFailure.With(
					logging.Str("error.type", "certificate.key.oversized"),
				), "[TLS upstream] Dial %s failed: server certificate has an oversized RSA key (%s). Regenerate the server certificate with a 2048- or 4096-bit key.", t.addr, errStr)
			case strings.Contains(errStr, "certificate has expired") || strings.Contains(errStr, "certificate is not yet valid"):
				Logger.Errorf(dialFailure.With(
					logging.Str("error.type", "certificate.validity"),
				), "[TLS upstream] Dial %s failed: server certificate validity period error — check system clock and certificate dates (%s)", t.addr, errStr)
			case strings.Contains(errStr, "certificate signed by unknown authority") || strings.Contains(errStr, "x509: "):
				Logger.Errorf(dialFailure.With(
					logging.Str("error.type", "certificate.chain.invalid"),
				), "[TLS upstream] Dial %s failed: certificate chain error — check tcpTLSCAFile matches the server's issuing CA (%s)", t.addr, errStr)
			case strings.Contains(errStr, "bad certificate") || strings.Contains(errStr, "unknown certificate"):
				Logger.Errorf(dialFailure.With(
					logging.Str("error.type", "certificate.rejected"),
				), "[TLS upstream] Dial %s failed: server rejected our client certificate — check tcpTLSCertFile/tcpTLSKeyFile and that the CA matches (%s)", t.addr, errStr)
			case strings.Contains(errStr, "no such file") || strings.Contains(errStr, "connection refused"):
				// low-level network error — let caller log at appropriate level
			default:
				Logger.Errorf(dialFailure.With(
					logging.Str("error.type", "tls.dial"),
				), "[TLS upstream] Dial %s TLS error: %s", t.addr, errStr)
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
				t.forget(t.conn)
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
	config, generation := t.tlsConfig, t.generation
	if t.closed {
		t.mu.Unlock()
		return false
	}
	t.mu.Unlock()

	conn, err := t.dialConfig(config)
	if err != nil {
		t.mu.Lock()
		t.err = err
		t.mu.Unlock()
		return false
	}

	t.mu.Lock()
	if t.closed || t.generation != generation {
		t.mu.Unlock()
		conn.Close()
		return false
	}
	// A concurrent goroutine (e.g. ReloadTLSConfig) may have replaced t.conn while
	// we were dialing — discard our new conn and use theirs.
	if t.conn != nil {
		t.mu.Unlock()
		conn.Close()
		return true
	}
	if err := t.admit(conn); err != nil {
		t.err = err
		t.mu.Unlock()
		conn.Close()
		return false
	}
	recovered := t.err != nil
	t.conn = conn
	t.err = nil
	t.mu.Unlock()
	attrs := logging.Event(logging.EventTCPConnection,
		logging.Str("server.address", t.addr),
		logging.Bool("airgap.tls.enabled", config != nil),
	)
	if recovered {
		attrs[0] = logging.Str(logging.AttrEventName, logging.EventTransportConnectionRestored)
	}
	Logger.Infof(attrs, "Connected to TCP server at %s", t.addr)
	return true
}

// reconnect attempts to re-establish a TCP connection
func (t *TCPAdapter) reconnect() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("TCP adapter closed")
	}

	// Close existing connection if any
	if t.conn != nil {
		t.forget(t.conn)
		if tlsConn, ok := t.conn.(*tls.Conn); ok && t.pinned {
			tlsConn.NetConn().Close()
		} else {
			t.conn.Close()
		}
		t.conn = nil
	}
	t.mu.Unlock()

	if !t.ensureConnected() {
		return fmt.Errorf("failed to reconnect to TCP server at %s", t.addr)
	}
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

	if conn == nil {
		return fmt.Errorf("TCP connection changed during reload; retry message")
	}
	_, err := conn.Write(msg)
	if err != nil {
		// Connection error - try to reconnect and resend
		if reconnectErr := t.reconnect(); reconnectErr != nil {
			return fmt.Errorf("failed to send message over TCP: %w", err)
		}

		// Try to write again with the new connection
		t.mu.Lock()
		conn := t.conn
		t.mu.Unlock()
		if conn == nil {
			return fmt.Errorf("TCP connection changed during reload; retry message")
		}
		_, retryErr := conn.Write(msg)
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
	if t.pinManager != nil {
		t.pinManager.StopAging()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.conn != nil {
		t.forget(t.conn)
		err := t.conn.Close()
		t.conn = nil
		return err
	}
	return nil
}
