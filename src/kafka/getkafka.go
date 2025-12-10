package kafka

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/IBM/sarama"
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
func ConfigureSaramaLogger() {
	saramaLogger := log.New(logging.StdLogger.Writer(), "[SARAMA] ", log.LstdFlags)
	sarama.Logger = saramaLogger
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

// decryptPKCS8 attempts to decrypt a PKCS#8 encrypted private key
// func decryptPKCS8(encryptedPEM []byte, _password []byte) ([]byte, error) {
// 	// Try to parse as PKCS#8 encrypted key
// 	_, err := x509.ParsePKCS8PrivateKey(encryptedPEM)
// 	if err == nil {
// 		// It's already decrypted or wasn't encrypted
// 		return encryptedPEM, nil
// 	}

// 	// Try with password - this is a simplified approach
// 	// For full PKCS#8 support, we'd need external library
// 	return nil, fmt.Errorf("PKCS#8 decryption not fully supported in this implementation")
// }

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
	Logger.Debugf("DEBUG: keyPasswordFile: '%s'", tlsConfigParameters.KeyPasswordFile)

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

		var decryptedDER []byte
		var decryptionMethod string

		// Try legacy PEM format first (RSA PRIVATE KEY with Proc-Type: 4,ENCRYPTED)
		if block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED" {
			Logger.Print("Detected legacy PEM encrypted format (DEK-Info), attempting decryption...")
			decryptedDER, err = decryptLegacyPEM(block, passphraseBytes)
			if err == nil {
				decryptionMethod = "legacy PEM"
			}
		}

		// If legacy PEM failed or wasn't detected, try PKCS#8
		// if decryptedDER == nil && block.Type == "ENCRYPTED PRIVATE KEY" {
		// 	Logger.Print("Detected PKCS#8 encrypted format, attempting decryption...")
		// 	decryptedDER, err = decryptPKCS8(keyPEM, passphraseBytes)
		// 	if err == nil {
		// 		decryptionMethod = "PKCS#8"
		// 	}
		// }

		// If both methods failed, panic
		if decryptedDER == nil {
			Logger.Panicf("Failed to decrypt key file. Tried legacy PEM and PKCS#8 formats. Last error: %v", err)
		}

		Logger.Printf("Successfully decrypted key using %s format", decryptionMethod)

		// Encode back to PEM format
		decryptedPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: decryptedDER,
		})

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
func ReadFromKafkaWithContext(ctx context.Context, name string, offsetSeconds int, brokers string, topics string, group string, timestamp string, callbackFunction func(string, []byte, time.Time, []byte) bool) {
	Logger.Print("Starting a new Sarama consumer (WithContext): " + name + " with offset: " + fmt.Sprintf("%d", offsetSeconds))

	ConfigureSaramaLogger()

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
		Logger.Panicf("Error creating consumer group client: %v", err)
	}

	consumptionIsPaused := false
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if err := client.Consume(ctx, strings.Split(topics, ","), &consumer); err != nil {
				if errors.Is(err, sarama.ErrClosedConsumerGroup) {
					return
				}
				Logger.Panicf("Error from consumer: %v", err)
			}
			if ctx.Err() != nil {
				return
			}
			consumer.ready = make(chan bool)
		}
	}()

	<-consumer.ready
	Logger.Println("Sarama consumer (WithContext) " + name + " up and running!...")

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
			session.MarkMessage(message, "")
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
