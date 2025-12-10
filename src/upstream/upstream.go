package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/mtu"
	"sitia.nu/airgap/src/protocol"
	"sitia.nu/airgap/src/version"
)

// TokenBucket implements a simple token bucket rate limiter
type TokenBucket struct {
	capacity   int
	tokens     float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

func NewTokenBucket(rate int) *TokenBucket {
	return &TokenBucket{
		capacity:   rate,
		tokens:     float64(rate),
		refillRate: float64(rate),
		lastRefill: time.Now(),
	}
}

func (tb *TokenBucket) Take() {
	for {
		now := time.Now()
		elapsed := now.Sub(tb.lastRefill).Seconds()
		refill := elapsed * tb.refillRate
		if refill > 0 {
			tb.tokens = min(float64(tb.capacity), tb.tokens+refill)
			tb.lastRefill = now
		}
		if tb.tokens >= 1 {
			tb.tokens -= 1
			return
		}
		time.Sleep(time.Millisecond * 1)
	}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

var config TransferConfiguration
var nextKeyGeneration time.Time
var keepRunning = true

var receivedEvents int64
var sentEvents int64
var totalReceived int64
var totalSent int64
var timeStart int64
var lastUdpErrorLog time.Time

var Logger = logging.Logger
var BuildNumber = "dev"

func SetConfig(conf TransferConfiguration) {
	config = conf
}

func translateTopic(input string) string {
	if len(config.topicTranslations) == 0 {
		return input
	}
	if output, ok := config.translations[input]; ok {
		return output
	}
	return input
}

func RunUpstream(kafkaClient KafkaClient, udpClient UDPClient) {
	var ctx context.Context

	// Start time for statistics
	timeStart = time.Now().Unix()

	// Setup logging to file
	if config.logFileName != "" {
		Logger.Print("Configuring log to: " + config.logFileName)
		if err := Logger.SetLogFile(config.logFileName); err != nil {
			Logger.Fatal(err)
		}
		Logger.Printf("Upstream version: %s, build number: %s", version.GitVersion, BuildNumber)
		Logger.Print("Log to file started up")
	}

	// Stats logger
	if config.logStatistics > 0 {
		go func() {
			interval := time.Duration(config.logStatistics) * time.Second
			for {
				time.Sleep(interval)
				recv := atomic.SwapInt64(&receivedEvents, 0)
				sent := atomic.SwapInt64(&sentEvents, 0)
				totalRecv := atomic.LoadInt64(&totalReceived)
				totalSnt := atomic.LoadInt64(&totalSent)
				stats := map[string]any{
					"id":             config.id,
					"time":           time.Now().Unix(),
					"time_start":     timeStart,
					"interval":       config.logStatistics,
					"received":       recv,
					"sent":           sent,
					"eps":            recv / int64(config.logStatistics),
					"total_received": totalRecv,
					"total_sent":     totalSnt,
				}
				if Logger.CanLog(logging.INFO) {
					b, _ := json.Marshal(stats)
					Logger.Info("STATISTICS: " + string(b))
				}
			}
		}()
	}

	// MTU
	if config.payloadSize == 0 {
		mtuValue, err := mtu.GetMTU(config.nic, fmt.Sprintf("%s:%d", config.targetIP, config.targetPort))
		if err != nil {
			Logger.Fatal(err)
		}
		config.payloadSize = uint16(mtuValue) - protocol.HEADER_SIZE
		if config.payloadSize < 256 {
			Logger.Fatalf("Calculated payloadSize is too small: %d", config.payloadSize)
		}
		if config.payloadSize > 65507 {
			config.payloadSize = 65507 - protocol.HEADER_SIZE
			Logger.Printf("Calculated payloadSize is too large, setting to max UDP size: %d", config.payloadSize)
		}
	}
	Logger.Printf("payloadSize: %d\n", config.payloadSize)

	// Send startup status message
	messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
		fmt.Appendf(nil, "%s Upstream %s starting up...", protocol.GetTimestamp(), config.id), config.payloadSize)
	udpErr := udpClient.SendMessage(messages[0])
	if udpErr != nil {
		Logger.Errorf("Failed sending startup message (will continue): %v", udpErr)
	}

	// Encryption
	if config.encryption {
		Logger.Printf("Creating initial key with public key file: %s", config.publicKeyFile)
		if adapter, ok := udpClient.(*UDPAdapter); ok {
			sendNewKey(adapter.conn)
		} else {
			Logger.Error("udpClient is not of type *UDPAdapter")
		}
	} else {
		Logger.Printf("No encryption will be used.")
	}

	ctx = context.Background()

	var timeFrom time.Time
	if config.from == "" {
		// set timeFrom to beginning of time
		timeFrom = time.Unix(0, 0)
	} else {
		var err error
		timeFrom, err = time.Parse(time.RFC3339, config.from)
		if err != nil {
			Logger.Fatalf("Invalid from time format: %v", err)
		}
	}

	// ----- Kafka handler -----
	Logger.Debug("Setting up Kafka handler")
	kafkaHandler := func(id string, _ []byte, t time.Time, received []byte) bool {
		atomic.AddInt64(&receivedEvents, 1)
		atomic.AddInt64(&totalReceived, 1)

		if Logger.CanLog(logging.DEBUG) {
			Logger.Debugf("kafkaHandler called for id=%s, time=%s", id, t.Format(time.RFC3339))
		}
		// Discard old messages, before "from"
		if t.Before(timeFrom) {
			if Logger.CanLog(logging.DEBUG) {
				Logger.Debugf("Discarding old message: %s time %s before from %s", id, t.Format(time.RFC3339), timeFrom.Format(time.RFC3339))
			}
			return keepRunning
		}

		// Filtering and topic name translation
		if config.filter != nil {
			parts := strings.Split(id, "_")
			if len(parts) == 3 {
				if offset, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
					if !config.filter.Check(offset) {
						if Logger.CanLog(logging.DEBUG) {
							Logger.Debugf("Filtered out message: %s", id)
						}
						return keepRunning
					}
				}
				// We might change the topic name here as well
				parts[0] = translateTopic(parts[0])
				id = strings.Join(parts, "_")
			}
		}
		// Compress or not
		var isCompressed = false
		if config.compressWhenLengthExceeds > 0 && len(received) > config.compressWhenLengthExceeds {
			compressed, err := protocol.CompressGzip(received)
			if err != nil {
				Logger.Errorf("Error compressing data: %s", err)
			} else {
				isCompressed = true
				received = compressed
			}
		}

		// Encrypt or not
		var messages [][]byte
		var err error
		var messageType uint8
		if config.encryption {
			var ciphertext []byte
			ciphertext, err = protocol.Encrypt(received, config.key)
			if isCompressed {
				messageType = protocol.Merge(protocol.TYPE_MESSAGE, protocol.TYPE_COMPRESSED_GZIP)
			} else {
				messageType = protocol.Merge(protocol.TYPE_MESSAGE, 0)
			}
			messages = protocol.FormatMessage(messageType, id, ciphertext, config.payloadSize)
		} else {
			if isCompressed {
				messageType = protocol.Merge(protocol.TYPE_CLEARTEXT, protocol.TYPE_COMPRESSED_GZIP)
			} else {
				messageType = protocol.Merge(protocol.TYPE_CLEARTEXT, 0)
			}
			messages = protocol.FormatMessage(messageType, id, received, config.payloadSize)
		}
		if err != nil {
			Logger.Error(err)
		}

		if Logger.CanLog(logging.DEBUG) {
			Logger.Debugf("sending %d UDP messages for id=%s", len(messages), id)
		}

		udpErr := udpClient.SendMessages(messages)
		if udpErr == nil {
			atomic.AddInt64(&sentEvents, int64(len(messages)))
			atomic.AddInt64(&totalSent, int64(len(messages)))
		} else {
			// Rate limit UDP error logging to once per minute to avoid log spam
			// when downstream is not available, but continue sending
			now := time.Now()
			if now.Sub(lastUdpErrorLog) >= time.Minute {
				Logger.Errorf("UDP send error (will retry, logging once per minute): %v", udpErr)
				lastUdpErrorLog = now
			}
			// Note: We don't stop sending, UDP will continue attempting delivery
		}

		// Key rotation
		if config.encryption && config.generateNewSymmetricKeyEvery > 0 &&
			time.Now().After(nextKeyGeneration) && config.publicKeyFile != "" {
			if adapter, ok := udpClient.(*UDPAdapter); ok {
				sendNewKey(adapter.conn)
			} else {
				Logger.Error("udpClient is not of type *UDPAdapter")
			}
		}
		return keepRunning
	}

	// ----- Source: Kafka or Random or a mock object for unit testing -----
	Logger.Printf("Reading from %s %s", config.source, config.bootstrapServers)
	if config.certFile != "" || config.keyFile != "" || config.caFile != "" {
		Logger.Print("Using TLS for Kafka")
		err := kafkaClient.SetTLS(config.certFile, config.keyFile, config.caFile, config.keyPasswordFile)
		if err != nil {
			Logger.Panicf("Failed to configure TLS: %v", err)
		}
	}

	for _, thread := range config.sendingThreads {
		go func(thread map[string]int) {
			// Use a token bucket per thread for EPS limiting
			var bucket *TokenBucket
			if config.eps > 0 {
				bucket = NewTokenBucket(int(config.eps))
			}
			for name, offset := range thread {
				group := fmt.Sprintf("%s-%s", config.groupID, name)
				Logger.Printf("Kafka thread: %s offset %d", name, offset)
				callbackHandler := func(id string, key []byte, t time.Time, received []byte) bool {
					if bucket != nil {
						bucket.Take()
					}
					return kafkaHandler(id, key, t, received)
				}
				Logger.Debugf("Starting Kafka read: %s offset %d", name, offset)
				kafkaClient.Read(ctx, name, offset,
					config.bootstrapServers, config.topic, group, config.from,
					callbackHandler)
			}
		}(thread)
	}
}

func Main(build string) {
	BuildNumber = build
	Logger.Printf("Upstream version: %s starting up...", version.GitVersion)
	Logger.Printf("Build number: %s", BuildNumber)

	var fileName string
	if len(os.Args) > 2 {
		Logger.Fatal("Too many command line parameters. Only one allowed.")
	}
	if len(os.Args) == 2 {
		fileName = os.Args[1]
	}

	// Signals
	hup := make(chan os.Signal, 1)
	sigterm := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	// Initial config load
	conf := DefaultConfiguration()
	conf, err := ReadParameters(fileName, conf)
	if err != nil {
		absPath, _ := os.Getwd()
		fullPath := fileName
		if !strings.HasPrefix(fileName, "/") {
			fullPath = absPath + "/" + fileName
		}
		Logger.Fatalf("Error reading configuration file %s: %v", fullPath, err)
	}
	conf = overrideConfiguration(conf)
	conf = checkConfiguration(conf)
	config = conf

	// Setup logging to file
	if config.logFileName != "" {
		Logger.Print("Configuring log to: " + config.logFileName)
		if err := Logger.SetLogFile(config.logFileName); err != nil {
			Logger.Fatal(err)
		}
		Logger.Printf("Downstream version: %s", BuildNumber)
		Logger.Print("Log to file started up")
	}

	// Log config
	logConfiguration(config)

	// Create UDP
	address := fmt.Sprintf("%s:%d", config.targetIP, config.targetPort)
	udpAdapter, err := NewUDPAdapter(address)
	if err != nil {
		Logger.Fatalf("Error creating UDP: %v", err)
	}

	// Choose Kafka adapter
	var kafkaAdapter KafkaClient
	switch config.source {
	case "kafka":
		kafkaAdapter = &KafkaAdapter{}
	case "random":
		kafkaAdapter = &RandomKafkaAdapter{}
	default:
		Logger.Fatalf("Unknown source: %s", config.source)
	}

	// Run upstream
	go RunUpstream(kafkaAdapter, udpAdapter)

	// Wait for exit signals
	for {
		select {
		case <-sigterm:
			Logger.Printf("Received SIGTERM, shutting down")
			messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
				fmt.Appendf(nil, "%s Upstream %s terminating by signal...", protocol.GetTimestamp(), config.id), config.payloadSize)
			udpAdapter.SendMessage(messages[0])
			udpAdapter.Close()
			return
		case <-hup:
			Logger.Printf("SIGHUP received: reopening logs for logrotate. New name: %s", config.logFileName)
			if config.logFileName != "" {
				if err := Logger.SetLogFile(config.logFileName); err != nil {
					Logger.Errorf("Failed reopening log file: %v", err)
				}
			}
			Logger.Printf("Logrotate completed")
		}
	}
}
