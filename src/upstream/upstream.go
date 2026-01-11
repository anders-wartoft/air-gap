package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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
var filteredEvents int64
var unfilteredEvents int64
var filterTimeouts int64
var totalReceived int64
var totalSent int64
var totalFiltered int64
var totalUnfiltered int64
var totalFilterTimeouts int64
var timeStart int64

// Transport status tracking
var transportStatusMu sync.Mutex
var transportStatus string = "running"         // "running" or error message
var previousTransportStatus string = "running" // Track previous status for change detection

// Kafka status tracking
var kafkaStatusMu sync.Mutex
var kafkaStatus string = "running"         // "running" or error message
var previousKafkaStatus string = "running" // Track previous status for change detection

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

// UpdateKafkaStatus updates the Kafka connection status and logs changes
func UpdateKafkaStatus(newStatus string) {
	kafkaStatusMu.Lock()
	if kafkaStatus != newStatus {
		previousKafkaStatus = kafkaStatus
		kafkaStatus = newStatus
		if newStatus == "running" {
			Logger.Infof("Kafka status restored to running (was: %s)", previousKafkaStatus)
		} else {
			Logger.Errorf("Kafka status changed to error: %s (was: %s)", newStatus, previousKafkaStatus)
		}
	}
	kafkaStatusMu.Unlock()
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
				filtered := atomic.SwapInt64(&filteredEvents, 0)
				unfiltered := atomic.SwapInt64(&unfilteredEvents, 0)
				filterTimeoutsInterval := atomic.SwapInt64(&filterTimeouts, 0)
				totalRecv := atomic.LoadInt64(&totalReceived)
				totalSnt := atomic.LoadInt64(&totalSent)
				totalFilt := atomic.LoadInt64(&totalFiltered)
				totalUnfilt := atomic.LoadInt64(&totalUnfiltered)
				totalFiltTimeouts := atomic.LoadInt64(&totalFilterTimeouts)

				// Get current transport and Kafka status
				transportStatusMu.Lock()
				transportStatus := transportStatus
				transportStatusMu.Unlock()

				kafkaStatusMu.Lock()
				kafkaStatus := kafkaStatus
				kafkaStatusMu.Unlock()

				stats := map[string]any{
					"id":                    config.id,
					"time":                  time.Now().Unix(),
					"time_start":            timeStart,
					"interval":              config.logStatistics,
					"received":              recv,
					"sent":                  sent,
					"filtered":              filtered,
					"unfiltered":            unfiltered,
					"filter_timeouts":       filterTimeoutsInterval,
					"eps":                   recv / int64(config.logStatistics),
					"total_received":        totalRecv,
					"total_sent":            totalSnt,
					"total_filtered":        totalFilt,
					"total_unfiltered":      totalUnfilt,
					"total_filter_timeouts": totalFiltTimeouts,
					"transport_status":      transportStatus,
					"kafka_status":          kafkaStatus,
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
	transportErr := udpClient.SendMessage(messages[0])
	if transportErr != nil {
		Logger.Errorf("Failed sending startup message (will continue): %v", transportErr)
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
					return kafkaHandler(timeFrom, udpClient, id, key, t, received)
				}
				Logger.Debugf("Starting Kafka read: %s offset %d", name, offset)
				kafkaClient.Read(ctx, name, offset,
					config.bootstrapServers, config.topic, group, config.from,
					callbackHandler)
			}
		}(thread)
	}
}
func kafkaHandler(timeFrom time.Time, udpClient UDPClient, id string, _ []byte, t time.Time, received []byte) bool {
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

	// Input filtering based on payload content (before topic translation and deliver filter)
	if config.inputFilter != nil {
		shouldFilter, err := config.inputFilter.ShouldFilterOut(received)
		if err != nil {
			Logger.Errorf("Error applying input filter: %v", err)
			// Check if it's a timeout error
			if strings.Contains(err.Error(), "timeout") {
				atomic.AddInt64(&filterTimeouts, 1)
				atomic.AddInt64(&totalFilterTimeouts, 1)
			}
			// On error, fail open (don't filter)
		} else if shouldFilter {
			atomic.AddInt64(&filteredEvents, 1)
			atomic.AddInt64(&totalFiltered, 1)
			if Logger.CanLog(logging.DEBUG) {
				Logger.Debugf("Input filter blocked message: %s", id)
			}
			return keepRunning
		} else {
			// Message passed the input filter
			atomic.AddInt64(&unfilteredEvents, 1)
			atomic.AddInt64(&totalUnfiltered, 1)
		}
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
		Logger.Debugf("sending %d %s messages for id=%s", len(messages), config.transport, id)
	}

	// Send with retry logic - prevents message loss if receiver is temporarily unavailable
	// We retry aggressively to avoid telling Kafka to reprocess (which causes consumer group rebalancing)
	const maxRetries = 30       // 30 attempts
	const retryIntervalMs = 100 // 100ms between attempts = 3 seconds total wait
	var transportErr error
	var hadFailure bool

	for attempt := 1; attempt <= maxRetries; attempt++ {
		transportErr = udpClient.SendMessages(messages)
		if transportErr == nil {
			// Success - update counters and status
			atomic.AddInt64(&sentEvents, int64(len(messages)))
			atomic.AddInt64(&totalSent, int64(len(messages)))

			// Clear error status if send succeeded
			transportStatusMu.Lock()
			if transportStatus != "running" {
				previousTransportStatus = transportStatus
				transportStatus = "running"
				Logger.Infof("Transport status restored to running (was: %s)", previousTransportStatus)
			}
			transportStatusMu.Unlock()

			// Log if we had failures but eventually succeeded
			if hadFailure {
				// For UDP, transient errors don't guarantee delivery - log with caution
				if config.transport == "udp" {
					Logger.Warnf("Message id=%s sent after %d attempt(s) - UDP delivery is not guaranteed (receiver may be down)", id, attempt)
				} else {
					Logger.Infof("Message id=%s successfully sent after %d attempt(s)", id, attempt)
				}
			}
			return keepRunning
		}

		// Send failed - prepare for retry with backoff
		hadFailure = true
		if attempt < maxRetries {
			if Logger.CanLog(logging.DEBUG) && attempt <= 3 {
				// Only log first 3 attempts to avoid log spam
				Logger.Debugf("Send attempt %d/%d failed for id=%s: %v, retrying in %dms",
					attempt, maxRetries, id, transportErr, retryIntervalMs)
			}
			time.Sleep(time.Duration(retryIntervalMs) * time.Millisecond)
		}
	}

	// All retries failed - update status and don't consume the message
	transportStatusMu.Lock()
	newStatus := transportErr.Error()
	if transportStatus != newStatus {
		previousTransportStatus = transportStatus
		transportStatus = newStatus
		Logger.Errorf("Transport status changed to error: %s (was: %s)", newStatus, previousTransportStatus)
	} else if transportStatus != "running" {
		transportStatus = newStatus
	}
	transportStatusMu.Unlock()

	// Return false to prevent this message from being marked as consumed
	// The message will be reprocessed when the receiver comes back up
	// Note: This should be rare as we retry for ~3 seconds before giving up
	Logger.Errorf("Failed to send message id=%s after %d retries (%dms): %v. Message will be retried.",
		id, maxRetries, maxRetries*retryIntervalMs, transportErr)
	return false
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

	// Create transport adapter (UDP or TCP)
	address := fmt.Sprintf("%s:%d", config.targetIP, config.targetPort)
	var transportAdapter TransportClient

	switch config.transport {
	case "tcp":
		Logger.Printf("Creating TCP transport to %s", address)
		adapter, errTCP := NewTCPAdapter(address)
		if errTCP != nil {
			Logger.Fatalf("Error creating TCP adapter: %v", errTCP)
		}
		transportAdapter = adapter
	case "udp":
		Logger.Printf("Creating UDP transport to %s", address)
		adapter, errUDP := NewUDPAdapter(address)
		if errUDP != nil {
			Logger.Fatalf("Error creating UDP adapter: %v", errUDP)
		}
		transportAdapter = adapter
	default:
		Logger.Fatalf("Unknown transport: %s", config.transport)
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
	go RunUpstream(kafkaAdapter, transportAdapter)

	// Wait for exit signals
	for {
		select {
		case <-sigterm:
			Logger.Printf("Received SIGTERM, shutting down")
			messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
				fmt.Appendf(nil, "%s Upstream %s terminating by signal...", protocol.GetTimestamp(), config.id), config.payloadSize)
			transportAdapter.SendMessage(messages[0])
			transportAdapter.Close()
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
