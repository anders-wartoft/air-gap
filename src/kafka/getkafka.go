package kafka

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"golang.org/x/crypto/pbkdf2"
	"sitia.nu/airgap/src/logging"
)

// Sarama configuration options
var (
	version  = sarama.DefaultVersion.String()
	assignor = "roundrobin"
	oldest   = true
	verbose  = true
	// TLS configuration
	tlsConfigParameters *TLSConfiguration = nil
)

// Logging
var Logger = logging.Logger

type TLSConfiguration struct {
	CertFile        string
	KeyFile         string
	CAFile          string
	KeyPasswordFile string
}

// ConfigureSaramaLogger sets up the Sarama library logger with a [SARAMA] prefix
// This should be called once at the start of any function that uses Sarama
// Also provides a hook to report errors via callback if provided
func ConfigureSaramaLogger(errorCallback ...func(string)) {
	saramaLogger := log.New(logging.StdLogger.Writer(), "[SARAMA] ", log.LstdFlags)

	// If an error callback is provided, wrap it to capture certain error patterns
	if len(errorCallback) > 0 && errorCallback[0] != nil {
		// Create a custom writer that captures error-like messages
		originalWriter := logging.StdLogger.Writer()
		sarama.Logger = log.New(&errorCapturingWriter{
			originalWriter: originalWriter,
			errorCallback:  errorCallback[0],
		}, "[SARAMA] ", log.LstdFlags)
	} else {
		sarama.Logger = saramaLogger
	}
}

// errorCapturingWriter wraps a writer and captures error messages
type errorCapturingWriter struct {
	originalWriter io.Writer
	errorCallback  func(string)
}

func (w *errorCapturingWriter) Write(p []byte) (n int, err error) {
	msg := string(p)

	// Check for error patterns in Sarama logs
	if strings.Contains(msg, "leaderless") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "failed") ||
		strings.Contains(msg, "error") {
		// Report the error
		w.errorCallback("Kafka cluster issue: " + strings.TrimSpace(msg))
	}

	// Always write to original writer
	return w.originalWriter.Write(p)
}

func SetVerbose(enabled bool) {
	verbose = enabled
}

func SetTLSConfigParameters(certFile, keyFile, caFile, keyPasswordFile string) (*tls.Config, error) {
	tlsConfigParameters = &TLSConfiguration{
		CertFile:        certFile,
		KeyFile:         keyFile,
		CAFile:          caFile,
		KeyPasswordFile: keyPasswordFile,
	}
	return createTLSConfig()
}

// LoadTLSCertificate loads a TLS certificate and private key, decrypting the key if a
// password file is provided. This is exported so that non-Kafka TLS clients (e.g. TCP
// transport) can reuse the same key-decryption logic.
func LoadTLSCertificate(certFile, keyFile, keyPasswordFile string) (tls.Certificate, error) {
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to read key file: %w", err)
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return tls.Certificate{}, fmt.Errorf("failed to decode PEM block from key file")
	}

	isEncrypted := false
	if block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED" {
		isEncrypted = true
	} else if block.Type == "ENCRYPTED PRIVATE KEY" {
		isEncrypted = true
	}

	if isEncrypted && keyPasswordFile == "" {
		return tls.Certificate{}, fmt.Errorf("key file %q is encrypted but no keyPasswordFile is configured", keyFile)
	}

	if keyPasswordFile != "" {
		passphrase, err := os.ReadFile(keyPasswordFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to read key password file: %w", err)
		}
		passphraseBytes := []byte(strings.TrimSpace(string(passphrase)))

		var decryptedPEM []byte
		if block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED" {
			decryptedDER, derr := decryptLegacyPEM(block, passphraseBytes)
			if derr != nil {
				return tls.Certificate{}, fmt.Errorf("failed to decrypt key file: %w", derr)
			}
			decryptedPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: decryptedDER})
		} else if block.Type == "ENCRYPTED PRIVATE KEY" {
			decryptedDER, derr := decryptPKCS8EncryptedKey(block, passphraseBytes)
			if derr != nil {
				return tls.Certificate{}, fmt.Errorf("failed to decrypt PKCS#8 key file: %w", derr)
			}
			decryptedPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: decryptedDER})
		} else {
			return tls.Certificate{}, fmt.Errorf("unsupported encrypted key format: %s", block.Type)
		}

		certPEM, err := os.ReadFile(certFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to read certificate file: %w", err)
		}
		return tls.X509KeyPair(certPEM, decryptedPEM)
	}

	return tls.LoadX509KeyPair(certFile, keyFile)
}

// ApplyTLSConfig enables TLS on the provided Sarama config if TLS parameters are set.
func ApplyTLSConfig(config *sarama.Config) error {
	if tlsConfigParameters == nil {
		return nil
	}
	Logger.Println("Enabling TLS configuration for Kafka client")
	tlsConfig, err := createTLSConfig()
	if err != nil {
		return err
	}
	config.Net.TLS.Enable = true
	config.Net.TLS.Config = tlsConfig
	return nil
}

// decryptLegacyPEM decrypts a legacy PEM encrypted private key (the old format with DEK-Info header)
func decryptLegacyPEM(block *pem.Block, password []byte) ([]byte, error) {
	// Get encryption info from headers
	dekInfo := block.Headers["DEK-Info"]
	if dekInfo == "" {
		return nil, fmt.Errorf("missing DEK-Info header")
	}

	// Parse DEK-Info: algorithm,iv
	parts := strings.Split(dekInfo, ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid DEK-Info format")
	}
	algorithm := parts[0]
	ivHex := parts[1]

	// Decode IV from hex
	iv := make([]byte, len(ivHex)/2)
	for i := 0; i < len(iv); i++ {
		fmt.Sscanf(ivHex[i*2:i*2+2], "%02x", &iv[i])
	}

	var key []byte
	var blockCipher cipher.Block
	var err error

	// Derive key based on algorithm
	switch algorithm {
	case "AES-256-CBC":
		key = deriveKey(password, iv[:8], 32)
		blockCipher, err = aes.NewCipher(key)
	case "AES-192-CBC":
		key = deriveKey(password, iv[:8], 24)
		blockCipher, err = aes.NewCipher(key)
	case "AES-128-CBC":
		key = deriveKey(password, iv[:8], 16)
		blockCipher, err = aes.NewCipher(key)
	case "DES-EDE3-CBC":
		key = deriveKey(password, iv[:8], 24)
		blockCipher, err = des.NewTripleDESCipher(key)
	default:
		return nil, fmt.Errorf("unsupported encryption algorithm: %s", algorithm)
	}

	if err != nil {
		return nil, err
	}

	// Decrypt using CBC mode
	mode := cipher.NewCBCDecrypter(blockCipher, iv)
	decrypted := make([]byte, len(block.Bytes))
	mode.CryptBlocks(decrypted, block.Bytes)

	// Remove PKCS#7 padding
	if len(decrypted) == 0 {
		return nil, fmt.Errorf("decrypted data is empty")
	}
	paddingLen := int(decrypted[len(decrypted)-1])
	if paddingLen > len(decrypted) || paddingLen == 0 {
		return nil, fmt.Errorf("invalid padding")
	}
	decrypted = decrypted[:len(decrypted)-paddingLen]

	return decrypted, nil
}

// deriveKey derives a key from password and salt using MD5 (OpenSSL EVP_BytesToKey)
//
// This use of MD5 is for compatibility with legacy OpenSSL-encrypted PEM files only.
// It is not used for new password hashing or general cryptography.
// codeql[weak-sensitive-data-hashing]: Required for legacy OpenSSL PEM decryption compatibility, not for new password hashing
func deriveKey(password, salt []byte, keyLen int) []byte {
	var derived []byte
	var previous []byte

	for len(derived) < keyLen {
		h := md5.New()
		h.Write(previous)
		h.Write(password)
		h.Write(salt)
		hash := h.Sum(nil)
		derived = append(derived, hash...)
		previous = hash
	}

	return derived[:keyLen]
}

// decryptPKCS8EncryptedKey decrypts an "ENCRYPTED PRIVATE KEY" PEM block
// (PKCS#8 EncryptedPrivateKeyInfo, produced by OpenSSL 3.x "openssl genrsa -aes256").
// Supports PBES2 with PBKDF2 (HMAC-SHA1/SHA-256/SHA-384/SHA-512) and AES-{128,192,256}-CBC.
// The returned bytes are the inner unencrypted PrivateKeyInfo DER, suitable for
// pem.Block{Type: "PRIVATE KEY", Bytes: …}.
func decryptPKCS8EncryptedKey(block *pem.Block, password []byte) ([]byte, error) {
	// Local ASN.1 structure definitions
	type encKeyInfo struct {
		Algorithm pkix.AlgorithmIdentifier
		Encrypted []byte
	}
	type pbes2Def struct {
		KDF     pkix.AlgorithmIdentifier
		Encrypt pkix.AlgorithmIdentifier
	}
	type kdfParamsDef struct {
		Salt   []byte
		Iters  int
		KeyLen int                      `asn1:"optional"`
		PRF    pkix.AlgorithmIdentifier `asn1:"optional"`
	}

	// OIDs
	oidPBES2 := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2 := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACsha1 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACsha256 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACsha384 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 10}
	oidHMACsha512 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}

	var info encKeyInfo
	if _, err := asn1.Unmarshal(block.Bytes, &info); err != nil {
		return nil, fmt.Errorf("failed to parse EncryptedPrivateKeyInfo: %w", err)
	}
	if !info.Algorithm.Algorithm.Equal(oidPBES2) {
		return nil, fmt.Errorf("unsupported PKCS#8 encryption scheme %v (only PBES2 is supported)", info.Algorithm.Algorithm)
	}

	var p2 pbes2Def
	if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &p2); err != nil {
		return nil, fmt.Errorf("failed to parse PBES2 params: %w", err)
	}
	if !p2.KDF.Algorithm.Equal(oidPBKDF2) {
		return nil, fmt.Errorf("unsupported PBES2 KDF %v (only PBKDF2 is supported)", p2.KDF.Algorithm)
	}

	var kdf kdfParamsDef
	if _, err := asn1.Unmarshal(p2.KDF.Parameters.FullBytes, &kdf); err != nil {
		return nil, fmt.Errorf("failed to parse PBKDF2 params: %w", err)
	}

	// Select PRF hash function
	var newHash func() hash.Hash
	var defaultKeyLen int
	prfOID := kdf.PRF.Algorithm
	switch {
	case len(prfOID) == 0 || prfOID.Equal(oidHMACsha1):
		newHash, defaultKeyLen = sha1.New, 20
	case prfOID.Equal(oidHMACsha256):
		newHash, defaultKeyLen = sha256.New, 32
	case prfOID.Equal(oidHMACsha384):
		newHash, defaultKeyLen = sha512.New384, 48
	case prfOID.Equal(oidHMACsha512):
		newHash, defaultKeyLen = sha512.New, 64
	default:
		return nil, fmt.Errorf("unsupported PBKDF2 PRF %v", prfOID)
	}

	// Determine AES key length from KDF params or cipher OID
	keyLen := kdf.KeyLen
	if keyLen == 0 {
		encOID := p2.Encrypt.Algorithm
		switch {
		case encOID.Equal(oidAES128CBC):
			keyLen = 16
		case encOID.Equal(oidAES192CBC):
			keyLen = 24
		case encOID.Equal(oidAES256CBC):
			keyLen = 32
		default:
			keyLen = defaultKeyLen
		}
	}

	// Derive key via PBKDF2
	key := pbkdf2.Key(password, kdf.Salt, kdf.Iters, keyLen, newHash)

	// Extract IV (OCTET STRING in Encrypt parameters)
	var iv []byte
	if _, err := asn1.Unmarshal(p2.Encrypt.Parameters.FullBytes, &iv); err != nil {
		return nil, fmt.Errorf("failed to parse encryption IV: %w", err)
	}

	// Verify cipher is an AES-CBC variant
	encOID := p2.Encrypt.Algorithm
	if !encOID.Equal(oidAES128CBC) && !encOID.Equal(oidAES192CBC) && !encOID.Equal(oidAES256CBC) {
		return nil, fmt.Errorf("unsupported encryption algorithm %v (only AES-CBC variants are supported)", encOID)
	}

	blockCipher, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	if len(info.Encrypted)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted data length %d is not a multiple of AES block size", len(info.Encrypted))
	}

	decrypted := make([]byte, len(info.Encrypted))
	cipher.NewCBCDecrypter(blockCipher, iv).CryptBlocks(decrypted, info.Encrypted)

	// Remove PKCS#7 padding
	if len(decrypted) == 0 {
		return nil, fmt.Errorf("decrypted data is empty")
	}
	pad := int(decrypted[len(decrypted)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(decrypted) {
		return nil, fmt.Errorf("invalid PKCS#7 padding (wrong password?)")
	}
	for i := len(decrypted) - pad; i < len(decrypted); i++ {
		if int(decrypted[i]) != pad {
			return nil, fmt.Errorf("invalid PKCS#7 padding byte at position %d (wrong password?)", i)
		}
	}
	return decrypted[:len(decrypted)-pad], nil
}

// Creates a new TLS configuration for the Kafka client
func createTLSConfig() (*tls.Config, error) {
	var cert tls.Certificate
	var err error

	// Read the key file to check if it's encrypted
	keyPEM, err := os.ReadFile(tlsConfigParameters.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}

	// Decode the PEM block to check if it's encrypted
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from key file")
	}

	Logger.Debugf("DEBUG: PEM block type: '%s'", block.Type)
	Logger.Debugf("DEBUG: PEM Proc-Type header: '%s'", block.Headers["Proc-Type"])
	Logger.Debugf("DEBUG: keyPasswordFile: [set]")

	// Check if the key file is encrypted
	isEncrypted := false
	if block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED" {
		isEncrypted = true
	} else if block.Type == "ENCRYPTED PRIVATE KEY" {
		isEncrypted = true
	}

	// If key is encrypted but no password file provided, fail
	if isEncrypted && tlsConfigParameters.KeyPasswordFile == "" {
		return nil, fmt.Errorf("key file '%s' is encrypted but no keyPasswordFile is configured. Please provide keyPasswordFile in configuration", tlsConfigParameters.KeyFile)
	}

	// Check if we need to decrypt the key file
	if tlsConfigParameters.KeyPasswordFile != "" {
		Logger.Printf("Attempting to decrypt encrypted key file: %s", tlsConfigParameters.KeyFile)

		// Read the passphrase from the password file
		passphrase, err := os.ReadFile(tlsConfigParameters.KeyPasswordFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read key password file: %w", err)
		}
		passphraseBytes := []byte(strings.TrimSpace(string(passphrase)))

		var decryptedPEM []byte
		switch {
		case block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED":
			Logger.Print("Detected legacy PEM encrypted format (DEK-Info), attempting decryption...")
			decryptedDER, derr := decryptLegacyPEM(block, passphraseBytes)
			if derr != nil {
				return nil, fmt.Errorf("failed to decrypt legacy PEM key: %w", derr)
			}
			Logger.Print("Successfully decrypted key using legacy PEM format")
			decryptedPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: decryptedDER})
		case block.Type == "ENCRYPTED PRIVATE KEY":
			Logger.Print("Detected PKCS#8 encrypted format, attempting decryption...")
			decryptedDER, derr := decryptPKCS8EncryptedKey(block, passphraseBytes)
			if derr != nil {
				return nil, fmt.Errorf("failed to decrypt PKCS#8 key: %w", derr)
			}
			Logger.Print("Successfully decrypted key using PKCS#8 format")
			decryptedPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: decryptedDER})
		default:
			return nil, fmt.Errorf("unsupported encrypted key format: %s", block.Type)
		}

		// Read the certificate file
		certPEM, err := os.ReadFile(tlsConfigParameters.CertFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read certificate file: %w", err)
		}

		// Load certificate and decrypted key
		cert, err = tls.X509KeyPair(certPEM, decryptedPEM)
		if err != nil {
			return nil, fmt.Errorf("failed to load X509 key pair: %w", err)
		}
	} else {
		// No decryption needed - use files directly
		cert, err = tls.LoadX509KeyPair(tlsConfigParameters.CertFile, tlsConfigParameters.KeyFile)
		if err != nil {
			return nil, err
		}
	}

	// Load CA cert
	caCert, err := os.ReadFile(tlsConfigParameters.CAFile)
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
	}
	return tlsConfig, nil
}

// This is called for each sending thread
// ReadFromKafkaWithContext allows external context cancellation (for SIGHUP reloads)
// errorCallback is optional and will be called with error messages if provided
func ReadFromKafkaWithContext(ctx context.Context, name string, offsetSeconds int, brokers string, topics string, group string, timestamp string, callbackFunction func(string, []byte, time.Time, []byte) bool, errorCallback ...func(string)) {
	Logger.Print("Starting a new Sarama consumer (WithContext): " + name + " with offset: " + fmt.Sprintf("%d", offsetSeconds))

	// Configure Sarama logger with error callback to capture metadata issues
	ConfigureSaramaLogger(errorCallback...)

	// Helper function to report errors
	reportError := func(msg string) {
		if len(errorCallback) > 0 && errorCallback[0] != nil {
			errorCallback[0](msg)
		}
	}

	version, err := sarama.ParseKafkaVersion(version)
	if err != nil {
		Logger.Panicf("Error parsing Kafka version: %v", err)
	}

	config := sarama.NewConfig()
	config.Version = version
	config.ClientID = name

	if tlsConfigParameters != nil {
		Logger.Println("Enabling TLS configuration for Kafka consumer")
		tlsConfig, err := createTLSConfig()
		config.Net.TLS.Enable = true
		if err != nil {
			Logger.Panicf("Error creating TLS config: %v", err)
		}
		config.Net.TLS.Enable = true
		config.Net.TLS.Config = tlsConfig
	}

	var from time.Time
	if timestamp != "" {
		t, err := time.Parse("2006-01-02T15:04:05-07:00", timestamp)
		if err != nil {
			Logger.Panicf("Error parsing from time: %v", err)
		}
		from = t
	} else {
		from = time.Now().Add(time.Duration(offsetSeconds) * time.Second)
	}
	config.Consumer.Offsets.Initial = sarama.OffsetOldest

	switch assignor {
	case "sticky":
		config.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategySticky()}
	case "roundrobin":
		config.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategyRoundRobin()}
	case "range":
		config.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategyRange()}
	default:
		Logger.Panicf("Unrecognized consumer group partition assignor: %s", assignor)
	}

	if oldest {
		config.Consumer.Offsets.Initial = sarama.OffsetOldest
	}

	consumer := Consumer{
		name:     name,
		ready:    make(chan bool),
		from:     from,
		delay:    time.Duration(offsetSeconds) * time.Second,
		callback: callbackFunction,
	}

	client, err := sarama.NewConsumerGroup(strings.Split(brokers, ","), group, config)
	if err != nil {
		errMsg := fmt.Sprintf("Error creating consumer group client: %v", err)
		Logger.Errorf("%s. Retrying in 5 seconds...", errMsg)
		reportError(errMsg)
		// Sleep and continue - will try again in the consumer loop
		// This allows the consumer to retry if Kafka cluster is temporarily unavailable
		time.Sleep(5 * time.Second)
		// Retry once more
		client, err = sarama.NewConsumerGroup(strings.Split(brokers, ","), group, config)
		if err != nil {
			Logger.Panicf("Error creating consumer group client after retry: %v", err)
		}
	}

	consumptionIsPaused := false
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		retryCount := 0
		for {
			if err := client.Consume(ctx, strings.Split(topics, ","), &consumer); err != nil {
				if errors.Is(err, sarama.ErrClosedConsumerGroup) {
					return
				}
				// Log error and retry instead of panicking
				retryCount++
				errMsg := fmt.Sprintf("Error from consumer (attempt %d): %v", retryCount, err)
				Logger.Errorf("%s. Will retry in 5 seconds...", errMsg)
				reportError(errMsg)

				// Sleep before retrying to avoid busy-wait loop
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
					// Continue to retry
				}
				continue
			}
			if ctx.Err() != nil {
				return
			}
			// Success - reset retry counter
			retryCount = 0
			// Clear error status on successful connect
			reportError("running")
			consumer.ready = make(chan bool)
		}
	}()

	<-consumer.ready
	Logger.Println("Sarama consumer (WithContext) " + name + " up and running!...")

	// Track if we've reported an error recently to avoid spam
	var lastErrorTime time.Time
	var lastErrorMsg string

	// Monitor Kafka health with periodic checks
	go func() {
		healthCheckTicker := time.NewTicker(10 * time.Second)
		defer healthCheckTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-healthCheckTicker.C:
				// If we had an error but it's been more than 30 seconds, try to clear it
				if lastErrorMsg != "" && time.Since(lastErrorTime) > 30*time.Second {
					// Assume error has resolved if no new errors in last 30 seconds
					Logger.Infof("Kafka cluster appears to have recovered")
					reportError("running")
					lastErrorMsg = ""
				}
			case err := <-client.Errors():
				if err != nil {
					errMsg := fmt.Sprintf("Kafka error: %v", err)
					if errMsg != lastErrorMsg {
						Logger.Errorf("%s", errMsg)
						reportError(errMsg)
						lastErrorMsg = errMsg
						lastErrorTime = time.Now()
					}
				}
			}
		}
	}()

	sigusr1 := make(chan os.Signal, 1)
	signal.Notify(sigusr1, syscall.SIGUSR1)

	for {
		select {
		case <-ctx.Done():
			Logger.Println("terminating: context cancelled (WithContext)")
			wg.Wait()
			if err = client.Close(); err != nil {
				Logger.Panicf("Error closing client: %v", err)
			}
			return
		case <-sigusr1:
			toggleConsumptionFlow(client, &consumptionIsPaused, &consumer)
		}
	}
}

// ReadFromKafka starts a Kafka consumer with a background context
func ReadFromKafka(name string, offsetSeconds int, brokers string, topics string, group string, timestamp string, callbackFunction func(string, []byte, time.Time, []byte) bool) {
	ctx := context.Background()
	ReadFromKafkaWithContext(ctx, name, offsetSeconds, brokers, topics, group, timestamp, callbackFunction)
}

func toggleConsumptionFlow(_ sarama.ConsumerGroup, isPaused *bool, _ *Consumer) {
	// Optionally use consumer.from for seeking if needed
	*isPaused = !*isPaused
}

// Consumer represents a Sarama consumer group consumer
type Consumer struct {
	name     string
	ready    chan bool
	from     time.Time
	delay    time.Duration
	callback func(string, []byte, time.Time, []byte) bool
}

// Setup is run at the beginning of a new session, before ConsumeClaim
func (consumer *Consumer) Setup(sarama.ConsumerGroupSession) error {
	// Mark the consumer as ready
	close(consumer.ready)
	return nil
}

func (consumer *Consumer) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

func (consumer *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	delimiter := "_"
	keepRunning := true
	for keepRunning {
		select {
		case message, ok := <-claim.Messages():
			if !ok {
				Logger.Printf("message channel was closed")
				return nil
			}
			sleepTime := time.Until(message.Timestamp.Add(-consumer.delay))
			if sleepTime > 0 {
				Logger.Debug("Consumer.delay: " + consumer.delay.String())
				Logger.Debug("Delaying message delivery on thread: " + consumer.name + " for " + sleepTime.String())
				time.Sleep(sleepTime)
			}
			// Debug output the first 80 characters of the message. If the message is truncated, show ...
			maxLen := 80
			var payload string
			if len(message.Value) < maxLen {
				payload = string(message.Value)
			} else {
				payload = string(message.Value[0:maxLen]) + "..."
			}
			Logger.Debugf("Message on thread %s: topic=%s partition=%d offset=%d timestamp=%s key=%s value=%s",
				consumer.name, message.Topic, message.Partition, message.Offset, message.Timestamp.String(),
				string(message.Key), payload)

			id := message.Topic + delimiter +
				fmt.Sprint(message.Partition) + delimiter +
				fmt.Sprint(message.Offset)
			keepRunning = consumer.callback(id, message.Key, message.Timestamp, message.Value)

			// Only mark message as consumed if callback succeeded (returned true)
			// If callback returns false, the message will be reprocessed on next poll
			if keepRunning {
				session.MarkMessage(message, "")
			} else {
				Logger.Debugf("Message id=%s not marked as consumed, will be retried", id)
			}
		case <-session.Context().Done():
			return nil
		default:
			// Sleep briefly to avoid busy loop
			time.Sleep(10 * time.Millisecond)
		}
	}
	// shutting down
	return nil
}

func ReadToEnd(ctx context.Context, brokers string, topic string, group string,
	callbackFunction func(string, []byte, time.Time, []byte) bool) error {
	partition := -1 // all partitions
	return ReadToEndPartition(partition, ctx, brokers, topic, group, 0, callbackFunction)
}

// ReadToEndPartition reads all available messages from the topic and exits when done.
// If partition >= 0, only reads the specified partition; otherwise, reads all partitions.
func ReadToEndPartition(partition int, ctx context.Context, brokers string, topic string, group string, fromOffset int64,
	callbackFunction func(string, []byte, time.Time, []byte) bool) error {

	Logger.Printf("Starting ReadToEnd for topic %s and partition %d", topic, partition)

	ConfigureSaramaLogger()

	version, err := sarama.ParseKafkaVersion(sarama.DefaultVersion.String())
	if err != nil {
		return err
	}

	config := sarama.NewConfig()
	config.Version = version
	config.ClientID = "readtoend"
	if tlsConfigParameters != nil {
		Logger.Println("Enabling TLS configuration for Kafka consumer")
		tlsConfig, err := createTLSConfig()
		if err != nil {
			return err
		}
		config.Net.TLS.Enable = true
		config.Net.TLS.Config = tlsConfig
	}
	config.Consumer.Offsets.Initial = sarama.OffsetOldest

	client, err := sarama.NewClient(strings.Split(brokers, ","), config)
	if err != nil {
		return err
	}
	defer client.Close()

	consumer, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		return err
	}
	defer consumer.Close()

	partitions, err := consumer.Partitions(topic)
	if err != nil {
		return err
	}

	var targetPartitions []int32
	if partition >= 0 {
		// Only process the specified partition
		found := false
		for _, p := range partitions {
			if int(p) == partition {
				targetPartitions = []int32{p}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("partition %d not found in topic %s", partition, topic)
		}
	} else {
		// Process all partitions
		targetPartitions = partitions
	}

	for _, partition := range targetPartitions {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		newestOffset, err := client.GetOffset(topic, partition, sarama.OffsetNewest)
		oldestOffset, err := client.GetOffset(topic, partition, sarama.OffsetOldest)

		startOffset := fromOffset
		if startOffset < oldestOffset {
			startOffset = oldestOffset
		}
		if startOffset >= newestOffset {
			Logger.Debugf("Partition %d has no new messages (startOffset=%d, latest=%d) — skipping", partition, startOffset, newestOffset)
			continue
		}

		Logger.Debugf("Reading partition %d from offset %d to %d (oldest=%d)",
			partition, startOffset, newestOffset-1, oldestOffset)

		pc, err := consumer.ConsumePartition(topic, partition, startOffset)
		if err != nil {
			return err
		}

		expectedFinalOffset := newestOffset - 1
		stallTimeout := 2 * time.Second
		stallTimer := time.NewTimer(stallTimeout)
		if !stallTimer.Stop() {
			<-stallTimer.C
		}

	partitionLoop: // <---- labeled loop
		for {
			select {
			case <-ctx.Done():
				Logger.Debugf("Context cancelled during ReadToEnd")
				pc.Close()
				return ctx.Err()

			case message, ok := <-pc.Messages():
				if !ok {
					Logger.Debugf("Partition consumer closed for partition %d", partition)
					pc.Close()
					break partitionLoop
				}

				// reset stall timer
				if !stallTimer.Stop() {
					select {
					case <-stallTimer.C:
					default:
					}
				}
				stallTimer.Reset(stallTimeout)

				id := fmt.Sprintf("%s_%d_%d", topic, partition, message.Offset)
				callbackFunction(id, message.Key, message.Timestamp, message.Value)

				if message.Offset >= expectedFinalOffset {
					Logger.Debugf("Reached end of partition %d at offset %d", partition, message.Offset)
					pc.AsyncClose()
					// Drain until closed
					for range pc.Messages() {
					}
					break partitionLoop
				}

			case <-stallTimer.C:
				Logger.Debugf("No progress for %v on partition %d, assuming done",
					stallTimeout, partition)
				pc.AsyncClose()
				for range pc.Messages() {
				}
				break partitionLoop
			}
		} // end partitionLoop

		if !stallTimer.Stop() {
			select {
			case <-stallTimer.C:
			default:
			}
		}
		pc.Close()
	}

	Logger.Debugf("ReadToEnd finished for topic %s", topic)
	return nil
}
