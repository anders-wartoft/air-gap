package upstream

import (
	"context"
	"crypto/tls"
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

	"sitia.nu/airgap/src/internal/pinnedtls"
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

var receivedEvents int64  // Kafka messages received
var sentEvents int64      // UDP fragments sent (one message can be multiple fragments)
var messagesSent int64    // Actual Kafka messages successfully sent (not fragments)
var messagesDropped int64 // Messages that failed to send after all retries
var lostEvents int64      // Messages replaced by an Airgap Lost Event guard
var filteredEvents int64
var unfilteredEvents int64
var filterTimeouts int64
var totalReceived int64
var totalSent int64
var totalMessagesSent int64    // Total messages sent since start
var totalMessagesDropped int64 // Total messages dropped since start
var totalLostEvents int64      // Total Airgap Lost Event guards emitted since start
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

func transformMessageID(id string) string {
	parts := strings.Split(id, "_")
	if len(parts) != 3 {
		return id
	}

	parts[0] = translateTopic(parts[0])

	partition, err := strconv.Atoi(parts[1])
	if err != nil {
		return strings.Join(parts, "_")
	}

	partition += config.partitionStartValue
	if partition < 0 || partition > 2147483647 {
		Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
			logging.Failure(),
			logging.Str("airgap.config.setting", "partitionStartValue"),
			logging.Decimal("messaging.destination.partition.id", int64(partition)),
		), "Partition out of int32 range after partitionStartValue is applied: %d", partition)
		os.Exit(1)
	}
	parts[1] = strconv.Itoa(partition)

	return strings.Join(parts, "_")
}

// UpdateKafkaStatus updates the Kafka connection status and logs changes
func UpdateKafkaStatus(newStatus string) {
	kafkaStatusMu.Lock()
	if kafkaStatus != newStatus {
		previousKafkaStatus = kafkaStatus
		kafkaStatus = newStatus
		if newStatus == "running" {
			Logger.Infof(logging.Event(logging.EventKafkaConnectionRestored,
				logging.Str("server.address", config.bootstrapServers),
				logging.Str("airgap.health.status", newStatus),
				logging.Str("airgap.health.status.previous", previousKafkaStatus),
				logging.Str("airgap.health.transition", "failed.to.healthy"),
			), "Kafka status restored to running (was: %s)", previousKafkaStatus)
		} else {
			Logger.Errorf(logging.Event(logging.EventKafkaConnect,
				logging.Failure(),
				logging.Str("server.address", config.bootstrapServers),
				logging.Str("airgap.health.status", newStatus),
				logging.Str("airgap.health.status.previous", previousKafkaStatus),
				logging.Str("airgap.health.transition", "healthy.to.failed"),
				logging.Error("_OTHER"),
			), "Kafka status changed to error: %s (was: %s)", newStatus, previousKafkaStatus)
		}
	}
	kafkaStatusMu.Unlock()
}

func RunUpstream(kafkaClient KafkaClient, udpClient UDPClient) {
	runUpstream(context.Background(), kafkaClient, udpClient, false)
}

func runUpstream(ctx context.Context, kafkaClient KafkaClient, udpClient UDPClient, wait bool) {
	var workers sync.WaitGroup
	if wait {
		defer workers.Wait()
	}

	// Start time for statistics
	timeStart = time.Now().Unix()

	// Setup logging to file
	if config.logFileName != "" {
		Logger.Info(logging.Event(logging.EventAppStart,
			logging.Str("airgap.config.setting", "logFileName"),
			logging.Str("file.path", config.logFileName),
		), "Configuring log to: "+config.logFileName)
		if !wait {
			if err := Logger.SetLogFile(config.logFileName); err != nil {
				Logger.Fatal(logging.Event(logging.EventAppFailure,
					logging.Failure(),
					logging.Str("file.path", config.logFileName),
					logging.ErrorType(err),
				), err)
			}
		}
		Logger.Infof(logging.Event(logging.EventAppStart,
			logging.Str("airgap.build.number", BuildNumber),
		), "Upstream version: %s, build number: %s", version.GitVersion, BuildNumber)
		Logger.Info(logging.Event(logging.EventAppStart,
			logging.Str("file.path", config.logFileName),
		), "Log to file started up")
	}

	// Stats logger
	if config.logStatistics > 0 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			interval := time.Duration(config.logStatistics) * time.Second
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				recv := atomic.SwapInt64(&receivedEvents, 0)
				sent := atomic.SwapInt64(&sentEvents, 0)
				msgsSent := atomic.SwapInt64(&messagesSent, 0)
				msgsDropped := atomic.SwapInt64(&messagesDropped, 0)
				lost := atomic.SwapInt64(&lostEvents, 0)
				filtered := atomic.SwapInt64(&filteredEvents, 0)
				unfiltered := atomic.SwapInt64(&unfilteredEvents, 0)
				filterTimeoutsInterval := atomic.SwapInt64(&filterTimeouts, 0)
				totalRecv := atomic.LoadInt64(&totalReceived)
				totalSnt := atomic.LoadInt64(&totalSent)
				totalMsgsSent := atomic.LoadInt64(&totalMessagesSent)
				totalMsgsDropped := atomic.LoadInt64(&totalMessagesDropped)
				totalLost := atomic.LoadInt64(&totalLostEvents)
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
					"id":                     config.id,
					"time":                   time.Now().Unix(),
					"time_start":             timeStart,
					"interval":               config.logStatistics,
					"received":               recv,
					"sent":                   sent,
					"messages_sent":          msgsSent,
					"messages_dropped":       msgsDropped,
					"lost_events":            lost,
					"filtered":               filtered,
					"unfiltered":             unfiltered,
					"filter_timeouts":        filterTimeoutsInterval,
					"eps":                    recv / int64(config.logStatistics),
					"total_received":         totalRecv,
					"total_sent":             totalSnt,
					"total_messages_sent":    totalMsgsSent,
					"total_messages_dropped": totalMsgsDropped,
					"total_lost_events":      totalLost,
					"total_filtered":         totalFilt,
					"total_unfiltered":       totalUnfilt,
					"total_filter_timeouts":  totalFiltTimeouts,
					"transport_status":       transportStatus,
					"kafka_status":           kafkaStatus,
				}
				if Logger.CanLog(logging.INFO) {
					b, _ := json.Marshal(stats)
					Logger.Info(logging.Event(logging.EventStatsReport,
						logging.Str("airgap.instance.id", config.id),
						logging.Int("airgap.stats.interval_seconds", int64(config.logStatistics)),
						logging.Int("airgap.message.received", recv),
						logging.Int("airgap.message.sent", msgsSent),
						logging.Int("airgap.message.dropped", msgsDropped),
						logging.Int("airgap.message.lost", lost),
						logging.Int("airgap.message.filtered", filtered),
						logging.Int("airgap.fragment.sent", sent),
						logging.Str("airgap.transport.status", transportStatus),
						logging.Str("airgap.kafka.status", kafkaStatus),
					), "STATISTICS: "+string(b))
				}
			}
		}()
	}

	// MTU
	if config.payloadSize == 0 {
		mtuValue, err := mtu.GetMTU(config.nic, fmt.Sprintf("%s:%d", config.targetIP, config.targetPort))
		if err != nil {
			Logger.Fatal(logging.Event(logging.EventAppFailure,
				logging.Failure(),
				logging.Str("airgap.network.interface", config.nic),
				logging.Str("server.address", config.targetIP),
				logging.Int("server.port", int64(config.targetPort)),
				logging.ErrorType(err),
			), err)
		}
		config.payloadSize = uint16(mtuValue) - protocol.HEADER_SIZE
		if config.payloadSize < 256 {
			Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.config.setting", "payloadSize"),
				logging.Int("airgap.config.value", int64(config.payloadSize)),
			), "Calculated payloadSize is too small: %d", config.payloadSize)
		}
		if config.payloadSize > 65507 {
			config.payloadSize = 65507 - protocol.HEADER_SIZE
			Logger.Infof(logging.Event(logging.EventConfigApplied,
				logging.Str("airgap.config.setting", "payloadSize"),
				logging.Int("airgap.config.value", int64(config.payloadSize)),
			), "Calculated payloadSize is too large, setting to max UDP size: %d", config.payloadSize)
		}
	}
	Logger.Infof(logging.Event(logging.EventConfigApplied,
		logging.Str("airgap.config.setting", "payloadSize"),
		logging.Int("airgap.config.value", int64(config.payloadSize)),
	), "payloadSize: %d\n", config.payloadSize)

	// Send startup status message
	messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
		fmt.Appendf(nil, "%s Upstream %s starting up...", protocol.GetTimestamp(), config.id), config.payloadSize)
	transportErr := udpClient.SendMessage(messages[0])
	if transportErr != nil {
		Logger.Errorf(logging.Event(logging.EventTransportSend,
			logging.Failure(),
			logging.Str("airgap.transport.kind", config.transport),
			logging.Str("airgap.message.id", "STATUS"),
			logging.ErrorType(transportErr),
		), "Failed sending startup message (will continue): %v", transportErr)
	}

	// Encryption
	if config.encryption {
		Logger.Infof(logging.Event(logging.EventCryptoKeyExchange,
			logging.Str("file.path", config.publicKeyFile),
		), "Creating initial key with public key file: %s", config.publicKeyFile)
		if adapter, ok := udpClient.(*UDPAdapter); ok {
			sendNewKey(adapter.conn)
		} else {
			Logger.Error(logging.Event(logging.EventCryptoKeyExchange,
				logging.Failure(),
				logging.Str("airgap.transport.kind", config.transport),
				logging.Str("error.type", "transport.not.udp"),
			), "udpClient is not of type *UDPAdapter")
		}
	} else {
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "encryption"),
			logging.Bool("airgap.config.value", false),
		), "No encryption will be used.")
	}

	var timeFrom time.Time
	if config.from == "" {
		// set timeFrom to beginning of time
		timeFrom = time.Unix(0, 0)
	} else {
		var err error
		timeFrom, err = time.Parse(time.RFC3339, config.from)
		if err != nil {
			Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.config.setting", "from"),
				logging.Str("airgap.config.value", config.from),
				logging.ErrorType(err),
			), "Invalid from time format: %v", err)
		}
	}

	// ----- Kafka handler -----
	Logger.Debug(logging.Event(logging.EventAppStart,
		logging.Str("messaging.destination.name", config.topic),
	), "Setting up Kafka handler")

	// ----- Source: Kafka or Random or a mock object for unit testing -----
	Logger.Infof(logging.Event(logging.EventKafkaConnect,
		logging.Str("airgap.kafka.source", config.source),
		logging.Str("server.address", config.bootstrapServers),
		logging.Str("messaging.destination.name", config.topic),
	), "Reading from %s %s", config.source, config.bootstrapServers)
	if config.certFile != "" || config.keyFile != "" || config.caFile != "" {
		Logger.Info(logging.Event(logging.EventTLSHandshake,
			logging.Str("airgap.tls.role", "kafka.client"),
			logging.Str("file.path", config.caFile),
		), "Using TLS for Kafka")
		err := kafkaClient.SetTLS(config.certFile, config.keyFile, config.caFile, config.keyPasswordFile)
		if err != nil {
			Logger.Panicf(logging.Event(logging.EventTLSHandshake,
				logging.Failure(),
				logging.Str("airgap.tls.role", "kafka.client"),
				logging.Str("file.path", config.caFile),
				logging.ErrorType(err),
			), "Failed to configure TLS: %v", err)
		}
	}

	for _, thread := range config.sendingThreads {
		workers.Add(1)
		go func(thread map[string]int) {
			defer workers.Done()
			// Use a token bucket per thread for EPS limiting
			var bucket *TokenBucket
			if config.eps > 0 {
				bucket = NewTokenBucket(int(config.eps))
			}
			for name, offset := range thread {
				group := fmt.Sprintf("%s-%s", config.groupID, name)
				Logger.Infof(logging.Event(logging.EventKafkaConsume,
					logging.Str("airgap.kafka.consumer.name", name),
					logging.Int("messaging.kafka.offset", int64(offset)),
					logging.Str("messaging.consumer.group.name", group),
					logging.Str("messaging.destination.name", config.topic),
				), "Kafka thread: %s offset %d", name, offset)
				callbackHandler := func(id string, key []byte, t time.Time, received []byte) bool {
					if bucket != nil {
						bucket.Take()
					}
					return kafkaHandlerWithContext(ctx, timeFrom, udpClient, id, key, t, received)
				}
				Logger.Debugf(logging.Event(logging.EventKafkaConsume,
					logging.Str("airgap.kafka.consumer.name", name),
					logging.Int("messaging.kafka.offset", int64(offset)),
					logging.Str("messaging.consumer.group.name", group),
					logging.Str("server.address", config.bootstrapServers),
				), "Starting Kafka read: %s offset %d", name, offset)
				kafkaClient.Read(ctx, name, offset,
					config.bootstrapServers, config.topic, group, config.from,
					callbackHandler)
			}
		}(thread)
	}
}
func kafkaHandler(timeFrom time.Time, udpClient UDPClient, id string, _ []byte, t time.Time, received []byte) bool {
	return kafkaHandlerWithContext(context.Background(), timeFrom, udpClient, id, nil, t, received)
}

// encodeForTransport encrypts the payload when encryption is enabled and splits
// the result into wire messages. Returning an error instead of silently
// formatting a nil ciphertext lets the caller substitute a lost-event guard.
func encodeForTransport(id string, payload []byte, isCompressed bool) ([][]byte, error) {
	var messageType uint8
	if config.encryption {
		ciphertext, err := protocol.Encrypt(payload, config.key)
		if err != nil {
			return nil, err
		}
		if isCompressed {
			messageType = protocol.Merge(protocol.TYPE_MESSAGE, protocol.TYPE_COMPRESSED_GZIP)
		} else {
			messageType = protocol.Merge(protocol.TYPE_MESSAGE, 0)
		}
		return protocol.FormatMessage(messageType, id, ciphertext, config.payloadSize), nil
	}
	if isCompressed {
		messageType = protocol.Merge(protocol.TYPE_CLEARTEXT, protocol.TYPE_COMPRESSED_GZIP)
	} else {
		messageType = protocol.Merge(protocol.TYPE_CLEARTEXT, 0)
	}
	return protocol.FormatMessage(messageType, id, payload, config.payloadSize), nil
}

func kafkaHandlerWithContext(ctx context.Context, timeFrom time.Time, udpClient UDPClient, id string, _ []byte, t time.Time, received []byte) bool {
	atomic.AddInt64(&receivedEvents, 1)
	atomic.AddInt64(&totalReceived, 1)

	if Logger.CanLog(logging.DEBUG) {
		Logger.Debugf(logging.Event(logging.EventKafkaConsume,
			logging.Str("airgap.message.id", id),
			logging.Str("airgap.message.timestamp", t.Format(time.RFC3339)),
			logging.Int("airgap.message.size", int64(len(received))),
		), "kafkaHandler called for id=%s, time=%s", id, t.Format(time.RFC3339))
	}
	// Discard old messages, before "from"
	if t.Before(timeFrom) {
		if Logger.CanLog(logging.DEBUG) {
			Logger.Debugf(logging.Event(logging.EventMessageFilter,
				logging.Str("airgap.message.id", id),
				logging.Str("airgap.filter.reason", "before.from"),
				logging.Str("airgap.message.timestamp", t.Format(time.RFC3339)),
				logging.Str("airgap.filter.from", timeFrom.Format(time.RFC3339)),
			), "Discarding old message: %s time %s before from %s", id, t.Format(time.RFC3339), timeFrom.Format(time.RFC3339))
		}
		return keepRunning
	}

	// Input filtering based on payload content (before topic translation and deliver filter)
	if config.inputFilter != nil {
		shouldFilter, err := config.inputFilter.ShouldFilterOut(received)
		if err != nil {
			Logger.Errorf(logging.Event(logging.EventMessageFilter,
				logging.Failure(),
				logging.Str("airgap.filter.kind", "input"),
				logging.Str("airgap.message.id", id),
				logging.ErrorType(err),
			), "Error applying input filter: %v", err)
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
				Logger.Debugf(logging.Event(logging.EventMessageFilter,
					logging.Str("airgap.filter.kind", "input"),
					logging.Str("airgap.filter.action", "clear.payload"),
					logging.Str("airgap.message.id", id),
					logging.Int("airgap.message.size", int64(len(received))),
				), "Input filter cleared payload for message: %s (sending with empty payload to preserve sequence)", id)
			}
			received = []byte{} // Clear payload but still send so gap-detector sees the offset
		} else {
			// Message passed the input filter
			atomic.AddInt64(&unfilteredEvents, 1)
			atomic.AddInt64(&totalUnfiltered, 1)
		}
	}

	if config.filter != nil {
		parts := strings.Split(id, "_")
		if len(parts) == 3 {
			if offset, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				if !config.filter.Check(offset) {
					if Logger.CanLog(logging.DEBUG) {
						Logger.Debugf(logging.Event(logging.EventMessageFilter,
							logging.Str("airgap.filter.kind", "deliver"),
							logging.Str("airgap.filter.action", "drop"),
							logging.Str("airgap.message.id", id),
							logging.Int("messaging.kafka.offset", offset),
						), "Filtered out message: %s", id)
					}
					return keepRunning
				}
			}
		}
	}

	id = transformMessageID(id)

	// Records that are too large to process safely are replaced by an Airgap
	// Lost Event guard. The guard carries the same message id, so downstream
	// still sees the offset and the gap detector does not pull it into resend
	// bundles, while the loss stays visible in the delivered data.
	if config.maxEventBytes > 0 && len(received) > config.maxEventBytes {
		reason := fmt.Sprintf("message size %d bytes exceeds maxEventBytes %d bytes", len(received), config.maxEventBytes)
		guard := recordLostEvent(udpClient, id, reason, received)
		if guard == nil {
			return keepRunning
		}
		received = guard
	}

	// Compress or not
	var isCompressed = false
	if config.compressWhenLengthExceeds > 0 && len(received) > config.compressWhenLengthExceeds {
		compressed, err := protocol.CompressGzip(received)
		if err != nil {
			Logger.Errorf(logging.Event(logging.EventProtocolEncode,
				logging.Failure(),
				logging.Str("airgap.compression.algorithm", "gzip"),
				logging.Str("airgap.message.id", id),
				logging.Int("airgap.message.size", int64(len(received))),
				logging.ErrorType(err),
			), "Error compressing data: %s", err)
		} else {
			isCompressed = true
			received = compressed
		}
	}

	// Encrypt or not
	messages, err := encodeForTransport(id, received, isCompressed)
	if err != nil {
		Logger.Error(logging.Event(logging.EventCryptoEncrypt,
			logging.Failure(),
			logging.Str("airgap.message.id", id),
			logging.Int("airgap.message.size", int64(len(received))),
			logging.ErrorType(err),
		), err)
		// The payload cannot be encrypted, so it can never be delivered. Send
		// an Airgap Lost Event guard in its place rather than an unencrypted or
		// empty message.
		guard := recordLostEvent(udpClient, id, fmt.Sprintf("encryption failed: %v", err), received)
		if guard == nil {
			return keepRunning
		}
		messages, err = encodeForTransport(id, guard, false)
		if err != nil {
			Logger.Errorf(logging.Event(logging.EventCryptoEncrypt,
				logging.Failure(),
				logging.Str("airgap.message.id", id),
				logging.ErrorType(err),
			), "Could not encrypt the lost-event guard for id=%s: %v. Message will be retried.", id, err)
			return false
		}
	}

	if Logger.CanLog(logging.DEBUG) {
		Logger.Debugf(logging.Event(logging.EventProtocolEncode,
			logging.Str("airgap.message.id", id),
			logging.Int("airgap.message.count", int64(len(messages))),
			logging.Str("airgap.transport.kind", config.transport),
		), "sending %d %s messages for id=%s", len(messages), config.transport, id)
	}

	// Send with retry logic - prevents message loss if receiver is temporarily unavailable
	// For TCP: uses configurable tcpRetryTimes (0=infinite) and tcpRetryInterval.
	// For UDP: hardcoded 30 retries at 100ms (3s total), then let Kafka reprocess.
	var maxRetries int
	var retryIntervalMs int
	if config.transport == "tcp" {
		maxRetries = config.tcpRetryTimes // 0 = infinite
		retryIntervalMs = config.tcpRetryInterval
	} else {
		maxRetries = 30
		retryIntervalMs = 100
	}
	var transportErr error
	var hadFailure bool

	for attempt := 1; maxRetries == 0 || attempt <= maxRetries; attempt++ {
		transportErr = udpClient.SendMessages(messages)
		if transportErr == nil {
			// Success - update counters and status
			atomic.AddInt64(&sentEvents, int64(len(messages))) // Count UDP fragments
			atomic.AddInt64(&totalSent, int64(len(messages)))
			atomic.AddInt64(&messagesSent, 1) // Count original Kafka message
			atomic.AddInt64(&totalMessagesSent, 1)
			transportStatusMu.Lock()
			if transportStatus != "running" {
				previousTransportStatus = transportStatus
				transportStatus = "running"
				Logger.Infof(logging.Event(logging.EventTransportConnectionRestored,
					logging.Str("airgap.transport.kind", config.transport),
					logging.Str("airgap.health.status", transportStatus),
					logging.Str("airgap.health.status.previous", previousTransportStatus),
					logging.Str("airgap.health.transition", "failed.to.healthy"),
				), "Transport status restored to running (was: %s)", previousTransportStatus)
			}
			transportStatusMu.Unlock()

			// Log if we had failures but eventually succeeded
			if hadFailure {
				// For UDP, transient errors don't guarantee delivery - log with caution
				if config.transport == "udp" {
					Logger.Warnf(logging.Event(logging.EventTransportSendRestored,
						logging.Str("airgap.transport.kind", config.transport),
						logging.Str("airgap.message.id", id),
						logging.Int("airgap.retry.attempt", int64(attempt)),
					), "Message id=%s sent after %d attempt(s) - UDP delivery is not guaranteed (receiver may be down)", id, attempt)
				} else {
					Logger.Infof(logging.Event(logging.EventTransportSendRestored,
						logging.Str("airgap.transport.kind", config.transport),
						logging.Str("airgap.message.id", id),
						logging.Int("airgap.retry.attempt", int64(attempt)),
					), "Message id=%s successfully sent after %d attempt(s)", id, attempt)
				}
			}
			return keepRunning
		}

		// Send failed - prepare for retry with backoff
		hadFailure = true
		retryLimitStr := fmt.Sprintf("%d", maxRetries)
		if maxRetries == 0 {
			retryLimitStr = "∞"
		}
		retryAttrs := logging.Event(logging.EventTransportSend,
			logging.Failure(),
			logging.Str("airgap.transport.kind", config.transport),
			logging.Str("airgap.message.id", id),
			logging.Int("airgap.retry.attempt", int64(attempt)),
			logging.Str("airgap.retry.limit", retryLimitStr),
			logging.Int("airgap.retry.interval_ms", int64(retryIntervalMs)),
			logging.ErrorType(transportErr),
		)
		if config.transport == "tcp" {
			// Use configurable error level for TCP retry messages
			switch config.tcpRetryErrorLevel {
			case "DEBUG":
				Logger.Debugf(retryAttrs, "Send attempt %s/%s failed for id=%s: %v, retrying in %dms",
					fmt.Sprintf("%d", attempt), retryLimitStr, id, transportErr, retryIntervalMs)
			case "INFO":
				Logger.Infof(retryAttrs, "Send attempt %s/%s failed for id=%s: %v, retrying in %dms",
					fmt.Sprintf("%d", attempt), retryLimitStr, id, transportErr, retryIntervalMs)
			case "ERROR":
				Logger.Errorf(retryAttrs, "Send attempt %s/%s failed for id=%s: %v, retrying in %dms",
					fmt.Sprintf("%d", attempt), retryLimitStr, id, transportErr, retryIntervalMs)
			default: // WARN
				Logger.Warnf(retryAttrs, "Send attempt %s/%s failed for id=%s: %v, retrying in %dms",
					fmt.Sprintf("%d", attempt), retryLimitStr, id, transportErr, retryIntervalMs)
			}
		} else {
			if Logger.CanLog(logging.DEBUG) && attempt <= 3 {
				Logger.Debugf(retryAttrs, "Send attempt %d/%s failed for id=%s: %v, retrying in %dms",
					attempt, retryLimitStr, id, transportErr, retryIntervalMs)
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Duration(retryIntervalMs) * time.Millisecond):
		}
	}

	// All retries failed - update status and don't consume the message
	transportStatusMu.Lock()
	newStatus := transportErr.Error()
	if transportStatus != newStatus {
		previousTransportStatus = transportStatus
		transportStatus = newStatus
		Logger.Errorf(logging.Event(logging.EventTransportConnect,
			logging.Failure(),
			logging.Str("airgap.transport.kind", config.transport),
			logging.Str("airgap.health.status", newStatus),
			logging.Str("airgap.health.status.previous", previousTransportStatus),
			logging.Str("airgap.health.transition", "healthy.to.failed"),
			logging.Error("_OTHER"),
		), "Transport status changed to error: %s (was: %s)", newStatus, previousTransportStatus)
	} else if transportStatus != "running" {
		transportStatus = newStatus
	}
	transportStatusMu.Unlock()

	// Track dropped message
	atomic.AddInt64(&messagesDropped, 1)
	atomic.AddInt64(&totalMessagesDropped, 1)

	// Return false to prevent this message from being marked as consumed
	// The message will be reprocessed when the receiver comes back up
	Logger.Errorf(logging.Event(logging.EventTransportSend,
		logging.Failure(),
		logging.Str("airgap.transport.kind", config.transport),
		logging.Str("airgap.message.id", id),
		logging.Int("airgap.retry.count", int64(maxRetries)),
		logging.Int("airgap.retry.elapsed_ms", int64(maxRetries*retryIntervalMs)),
		logging.ErrorType(transportErr),
	), "Failed to send message id=%s after %d retries (%dms): %v. Message will be retried.",
		id, maxRetries, maxRetries*retryIntervalMs, transportErr)
	return false
}

func Main(build string) {
	if handled, err := pinnedtls.GenerateCommand(os.Args[1:], "client", os.Stdout); handled {
		if err != nil {
			Logger.Fatal(logging.Event(logging.EventAppFailure,
				logging.Failure(),
				logging.Str("airgap.command", "pinned-tls-generate"),
				logging.ErrorType(err),
			), err)
		}
		return
	}
	BuildNumber = build
	Logger.Infof(logging.Event(logging.EventAppStart), "Upstream version: %s starting up...", version.GitVersion)
	Logger.Infof(logging.Event(logging.EventAppStart,
		logging.Str("airgap.build.number", BuildNumber),
	), "Build number: %s", BuildNumber)

	var fileName string
	var overrideArgs []string
	// Parse command line: first non-dashed argument is the property file, --key=value args are overrides.
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--") {
			overrideArgs = append(overrideArgs, arg)
		} else if fileName == "" {
			fileName = arg
		} else {
			Logger.Fatal(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.config.setting", "propertyFile"),
				logging.Str("airgap.config.value", arg),
			), "Too many non-dashed command line parameters. Only one property file allowed.")
		}
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
		Logger.Fatalf(logging.Event(logging.EventConfigLoad,
			logging.Failure(),
			logging.Str("file.path", fullPath),
			logging.ErrorType(err),
		), "Error reading configuration file %s: %v", fullPath, err)
	}
	conf = overrideConfiguration(conf)
	conf = parseCommandLineOverrides(overrideArgs, conf)
	resolved := conf
	if conf.tcpTLSAuthMode == "pinned" {
		if err := pinnedtls.ValidateReloadFile(fileName); err != nil {
			Logger.Fatal(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.tls.auth_mode", "pinned"),
				logging.Str("file.path", fileName),
				logging.ErrorType(err),
			), err)
		}
	}
	conf = checkConfiguration(conf)
	config = conf

	// Setup logging to file
	if config.logFileName != "" {
		Logger.Info(logging.Event(logging.EventAppStart,
			logging.Str("airgap.config.setting", "logFileName"),
			logging.Str("file.path", config.logFileName),
		), "Configuring log to: "+config.logFileName)
		Logger.Infof(logging.Event(logging.EventAppStart,
			logging.Str("airgap.build.number", BuildNumber),
		), "Downstream version: %s", BuildNumber)
		Logger.Info(logging.Event(logging.EventAppStart,
			logging.Str("file.path", config.logFileName),
		), "Log to file started up")
	}

	// Log config
	logConfiguration(config)

	// Create transport adapter (UDP or TCP)
	address := fmt.Sprintf("%s:%d", config.targetIP, config.targetPort)
	var transportAdapter TransportClient

	switch config.transport {
	case "tcp":
		Logger.Infof(logging.Event(logging.EventTransportConnect,
			logging.Str("airgap.transport.kind", "tcp"),
			logging.Str("server.address", config.targetIP),
			logging.Int("server.port", int64(config.targetPort)),
		), "Creating TCP transport to %s", address)
		var tcpTLSConfig *tls.Config
		if config.tcpTLSEnabled {
			Logger.Info(logging.Event(logging.EventTLSHandshake,
				logging.Str("airgap.tls.role", "client"),
				logging.Str("server.address", address),
			), "Building TLS configuration for TCP transport")
			var err error
			tcpTLSConfig, err = config.BuildTLSConfig()
			if err != nil {
				Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
					logging.Failure(),
					logging.Str("airgap.config.setting", "tcpTLS"),
					logging.Str("server.address", address),
					logging.ErrorType(err),
				), "Error building TCP TLS configuration: %v", err)
			}
			Logger.Infof(logging.Event(logging.EventTLSHandshake,
				logging.Str("airgap.tls.role", "client"),
				logging.Str("server.address", address),
				logging.Bool("airgap.tls.mutual", config.tcpTLSCertFile != ""),
				logging.Str("airgap.tls.cipher_suites", config.tcpTLSCipherSuites),
			), "TCP TLS configured (mTLS=%t, cipherSuites=%q)", config.tcpTLSCertFile != "", config.tcpTLSCipherSuites)
		}
		var manager *pinnedtls.Manager
		if config.tcpTLSAuthMode == "pinned" {
			pinAttrs := logging.Event(logging.EventTLSPinReload,
				logging.Str("airgap.tls.role", "client"),
				logging.Str("file.path", config.tcpTLSTrustedKeysDir),
			)
			snapshot, err := pinnedtls.Prepare(config.tcpTLSCertFile, config.tcpTLSKeyFile, config.tcpTLSTrustedKeysDir, false)
			if err != nil {
				Logger.Fatal(pinAttrs.Merge(logging.Err(err)), err)
			}
			var pending bool
			snapshot, pending, err = pinnedtls.RestorePendingClient(config.tcpTLSTrustedKeysDir, snapshot)
			if err != nil {
				Logger.Fatal(pinAttrs.Merge(logging.Err(err)), err)
			}
			if pending {
				if !config.tcpTLSRotationEnabled {
					Logger.Fatal(logging.Event(logging.EventConfigInvalid,
						logging.Failure(),
						logging.Str("airgap.config.setting", "tcpTLSRotationEnabled"),
						logging.Bool("airgap.config.value", config.tcpTLSRotationEnabled),
						logging.Str("file.path", config.tcpTLSTrustedKeysDir),
					), "Pending client rotation requires tcpTLSRotationEnabled=true; reconcile recovery state before disabling exchange")
				}
				Logger.Warn(logging.Event(logging.EventTLSPinRotate,
					logging.Str("airgap.tls.role", "client"),
					logging.Str("airgap.tls.rotation.state", "pending"),
					logging.Str("file.path", config.tcpTLSTrustedKeysDir),
				), "Pinned rotation recovery: retained old identity; SIGHUP retries configured replacement")
			}
			manager, err = pinnedtls.NewManager(snapshot)
			if err != nil {
				Logger.Fatal(pinAttrs.Merge(logging.Err(err)), err)
			}
			tcpTLSConfig = manager.Config()
			manager.StartAging(config.tcpTLSPinIdleSeconds, func(fingerprints []string) {
				Logger.Infof(logging.Event(logging.EventTLSPinAging,
					logging.Str("airgap.tls.role", "server"),
					logging.Int("airgap.tls.pin.purged", int64(len(fingerprints))),
					logging.Int("airgap.tls.pin.idle_seconds", int64(config.tcpTLSPinIdleSeconds)),
				), "Pinned TLS idle aging: role=server purged=%v; disk unchanged, SIGHUP restores pins", fingerprints)
			})
		}
		adapter, errTCP := newTCPAdapter(address, tcpTLSConfig, manager)
		if errTCP != nil {
			if manager != nil {
				manager.StopAging()
			}
			Logger.Fatalf(logging.Event(logging.EventTCPConnection,
				logging.Failure(),
				logging.Str("server.address", address),
				logging.ErrorType(errTCP),
			), "Error creating TCP adapter: %v", errTCP)
		}
		if config.tcpTLSEnabled {
			adapter.pinned = config.tcpTLSAuthMode == "pinned"
			adapter.SetTLSReloadParams(
				config.tcpTLSCAFile,
				config.tcpTLSCertFile,
				config.tcpTLSKeyFile,
				config.tcpTLSKeyPasswordFile,
				config.tcpTLSCipherSuites,
				config.tcpTLSServerCNRegex,
			)
		}
		transportAdapter = adapter
	case "udp":
		Logger.Infof(logging.Event(logging.EventTransportConnect,
			logging.Str("airgap.transport.kind", "udp"),
			logging.Str("server.address", config.targetIP),
			logging.Int("server.port", int64(config.targetPort)),
			logging.Str("airgap.network.interface", config.nic),
		), "Creating UDP transport to %s", address)
		adapter, errUDP := NewUDPAdapter(address, config.nic)
		if errUDP != nil {
			Logger.Fatalf(logging.Event(logging.EventUDPBind,
				logging.Failure(),
				logging.Str("server.address", address),
				logging.Str("airgap.network.interface", config.nic),
				logging.ErrorType(errUDP),
			), "Error creating UDP adapter: %v", errUDP)
		}
		transportAdapter = adapter
	default:
		Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
			logging.Failure(),
			logging.Str("airgap.config.setting", "transport"),
			logging.Str("airgap.config.value", config.transport),
		), "Unknown transport: %s", config.transport)
	}

	// Choose Kafka adapter
	var kafkaAdapter KafkaClient
	switch config.source {
	case "kafka":
		adapter, err := NewKafkaAdapter(config)
		if err != nil {
			Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.ErrorType(err),
			), "Invalid Kafka consumer settings: %v", err)
		}
		kafkaAdapter = adapter
	case "random":
		kafkaAdapter = &RandomKafkaAdapter{}
	default:
		Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
			logging.Failure(),
			logging.Str("airgap.config.setting", "source"),
			logging.Str("airgap.config.value", config.source),
		), "Unknown source: %s", config.source)
	}

	// Run upstream
	runContext, cancelRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	var reloadWorkers sync.WaitGroup
	var reloadMu sync.Mutex
	go func() {
		runUpstream(runContext, kafkaAdapter, transportAdapter, true)
		close(done)
	}()

	// Wait for exit signals
	for {
		select {
		case <-sigterm:
			Logger.Info(logging.Event(logging.EventAppShutdown,
				logging.Str("airgap.signal", "SIGTERM"),
				logging.Str("airgap.instance.id", config.id),
			), "Received SIGTERM, shutting down")
			cancelRun()
			<-done
			reloadWorkers.Wait()
			messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
				fmt.Appendf(nil, "%s Upstream %s terminating by signal...", protocol.GetTimestamp(), config.id), config.payloadSize)
			transportAdapter.SendMessage(messages[0])
			transportAdapter.Close()
			config.WarnProductionConfiguration("shutdown")
			return
		case <-hup:
			// Run SIGHUP work in a separate goroutine so the signal loop stays
			// responsive — ReloadTLSConfig may need to acquire t.mu which could be
			// briefly held by a send goroutine, and SetLogFile does I/O.
			reloadWorkers.Add(1)
			go func() {
				defer reloadWorkers.Done()
				reloadMu.Lock()
				defer reloadMu.Unlock()
				Logger.Infof(logging.Event(logging.EventAppReload,
					logging.Str("airgap.signal", "SIGHUP"),
					logging.Str("file.path", config.logFileName),
				), "SIGHUP received: reopening logs with name %s and reloading TLS certificates", config.logFileName)
				if config.logFileName != "" {
					Logger.Infof(logging.Event(logging.EventAppReload,
						logging.Str("airgap.signal", "SIGHUP"),
						logging.Str("file.path", config.logFileName),
					), "Reopening logs for logrotate. New name: %s", config.logFileName)
					if err := Logger.SetLogFile(config.logFileName); err != nil {
						Logger.Errorf(logging.Event(logging.EventAppReload,
							logging.Failure(),
							logging.Str("file.path", config.logFileName),
							logging.ErrorType(err),
						), "Failed reopening log file: %v", err)
					}
				}
				if fileName != "" {
					rotation, err := Logger.ReloadSigningCertificate(fileName)
					if err != nil {
						Logger.Errorf(logging.Event(
							logging.EventLogSigningCertificateRotate,
							logging.Failure(),
							logging.Str("file.path", fileName),
							logging.ErrorType(err),
						), "Log-signing certificate reload failed: %v", err)
					} else if rotation.Authenticated {
						messages := protocol.FormatMessage(
							protocol.TYPE_STATUS,
							"STATUS",
							rotation.Status,
							config.payloadSize,
						)
						for _, message := range messages {
							if err := transportAdapter.SendMessage(message); err != nil {
								Logger.Errorf(logging.Event(
									logging.EventLogSigningCertificateRotate,
									logging.Failure(),
									logging.ErrorType(err),
								), "Could not send log-signing certificate rotation STATUS: %v", err)
								break
							}
						}
					}
				}
				if tcpAdapter, ok := transportAdapter.(*TCPAdapter); ok {
					var err error
					if tcpAdapter.pinned {
						err = reloadPinnedConfiguration(fileName, overrideArgs, &resolved, tcpAdapter)
					} else {
						err = tcpAdapter.ReloadTLSConfig()
					}
					if err != nil {
						Logger.Errorf(logging.Event(logging.EventTLSCertReload,
							logging.Failure(),
							logging.Bool("airgap.tls.pinned", tcpAdapter.pinned),
							logging.Str("file.path", fileName),
							logging.ErrorType(err),
						), "TLS certificate reload failed: %v", err)
					}
				}
				Logger.Info(logging.Event(logging.EventAppReload,
					logging.Str("airgap.signal", "SIGHUP"),
				), "SIGHUP handling completed")
			}()
		}
	}
}
