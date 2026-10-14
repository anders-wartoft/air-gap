package downstream

import (
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
	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/mtu"
	"sitia.nu/airgap/src/protocol"
	"sitia.nu/airgap/src/udp"
	"sitia.nu/airgap/src/version"
)

// Global state
var Logger = logging.Logger
var cache = protocol.CreateMessageCache()
var receivedEvents int64
var sentEvents int64
var totalReceived int64
var totalSent int64
var timeStart int64
var config TransferConfiguration
var BuildNumber string
var kafkaWriter KafkaWriter

// lastKeyExchangeNano records the Unix nanosecond of the most recently processed
// KEY_EXCHANGE message. Used to rate-limit RSA decryption: any attacker who can
// reach the downstream port could otherwise flood TYPE_KEY_EXCHANGE packets and
// pin a CPU core with repeated RSA-OAEP decrypt calls.
var lastKeyExchangeNano int64

// Transport status tracking
var transportStatusMu sync.Mutex
var transportStatus string = "running"         // "running" or error message
var previousTransportStatus string = "running" // Track previous status for change detection

// Kafka status tracking
var kafkaStatusMu sync.Mutex
var kafkaStatus string = "running"         // "running" or error message
var previousKafkaStatus string = "running" // Track previous status for change detection

func SetConfig(conf TransferConfiguration) {
	config = conf
	cache.SetLimits(conf.maxCacheEntries, uint16(conf.maxNrMessages))
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
			Logger.Infof(logging.Event(logging.EventKafkaConnectionRestored,
				logging.Str("airgap.kafka.status", newStatus),
				logging.Str("airgap.kafka.status.previous", previousKafkaStatus),
			), "Kafka status restored to running (was: %s)", previousKafkaStatus)
		} else {
			Logger.Errorf(logging.Event(logging.EventKafkaConnect,
				logging.Failure(),
				logging.Str("airgap.kafka.status", newStatus),
				logging.Str("airgap.kafka.status.previous", previousKafkaStatus),
			), "Kafka status changed to error: %s (was: %s)", newStatus, previousKafkaStatus)
		}
	}
	kafkaStatusMu.Unlock()
}

// RunDownstream runs the downstream process
func RunDownstream(transportReceiver TransportReceiver, stopChan <-chan struct{}) {
	var workers sync.WaitGroup
	defer workers.Wait()
	timeStart = time.Now().Unix()

	if config.mtu == 0 {
		mtuValue, err := mtu.GetMTU(config.nic, fmt.Sprintf("%s:%d", config.targetIP, config.targetPort))
		if err != nil {
			Logger.Fatal(logging.Event(logging.EventConfigInvalid,
				logging.Str("airgap.config.setting", "mtu"),
				logging.Str("airgap.network.interface", config.nic),
			).Merge(logging.Err(err)), err)
		}
		config.mtu = uint16(mtuValue)
	}

	if config.logStatistics > 0 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			logStatistics(stopChan)
		}()
	}

	Logger.Infof(logging.Event(logging.EventAppStart,
		logging.Str("airgap.build.number", BuildNumber),
	), "Downstream version: %s", BuildNumber)
	sendMessage(protocol.TYPE_STATUS, "", config.topic,
		fmt.Appendf(nil, "Downstream starting %s server on port %d", config.transport, config.targetPort))

	// Setup transport receiver
	transportReceiver.Setup(
		config.mtu,
		config.numReceivers,
		config.channelBufferSize,
		config.readBufferMultiplier,
		config.enableRxqOvfl,
	)
	listen := func() {
		transportReceiver.Listen(
			config.targetIP,
			config.targetPort,
			config.rcvBufSize,
			handleUdpMessage,
			config.mtu,
			stopChan,
			config.numReceivers,
		)
	}
	if config.transport == "tcp" {
		// TCP Listen installs the listener and launches its connection handlers.
		listen()
	} else {
		workers.Add(1)
		go func() {
			defer workers.Done()
			listen()
		}()
	}
	// Wait for stop signal
	<-stopChan
	if config.transport == "tcp" {
		if err := transportReceiver.Close(); err != nil {
			Logger.Errorf(logging.Event(logging.EventTCPClose,
				logging.Failure(),
				logging.ErrorType(err),
			), "Failed to close TCP receiver: %v", err)
		}
	}

	// Give a small timeout to flush remaining messages
	time.Sleep(2 * time.Second)
}
func handleUdpMessage(msg []byte) {
	if pinnedtls.IsRotationFrame(msg) {
		Logger.Error(logging.Event(logging.EventTLSPinRotate,
			logging.Failure(),
			logging.Str("error.type", "rotation.unsupported.transport"),
			logging.Str("network.transport", config.transport),
		), "Pinned rotation rejected on non-control transport")
		return
	}
	messageType, messageID, payload, err := protocol.ParseMessage(msg, cache)
	if err != nil {
		Logger.Errorf(logging.Event(logging.EventProtocolDecode,
			logging.Int("airgap.message.size", int64(len(msg))),
		).Merge(logging.Err(err)), "Failed to parse message: %v", err)
		// Update transport status on parse errors
		transportStatusMu.Lock()
		newStatus := fmt.Sprintf("parse error: %v", err)
		if transportStatus != newStatus {
			previousTransportStatus = transportStatus
			transportStatus = newStatus
			Logger.Errorf(logging.Event(logging.EventProtocolDecode,
				logging.Failure(),
				logging.Str("airgap.transport.status", newStatus),
				logging.Str("airgap.transport.status.previous", previousTransportStatus),
			), "Transport status changed to error: %s (was: %s)", newStatus, previousTransportStatus)
		}
		transportStatusMu.Unlock()
		return
	}

	// Message received successfully - restore running status if it was in error
	transportStatusMu.Lock()
	if transportStatus != "running" {
		previousTransportStatus = transportStatus
		transportStatus = "running"
		Logger.Infof(logging.Event(logging.EventTransportConnectionRestored,
			logging.Str("airgap.transport.status", transportStatus),
			logging.Str("airgap.transport.status.previous", previousTransportStatus),
		), "Transport status restored to running (was: %s)", previousTransportStatus)
	}
	transportStatusMu.Unlock()

	topic, partitionStr, _, err := protocol.ParseMessageId(messageID)
	if err != nil {
		topic = config.topic
		partitionStr = "0"
	}
	topic = translateTopic(topic)
	// Safe: partitionStr is parsed from Kafka message ID and checked above; conversion to int32 is safe.
	// codeql[integer-type-incorrect-conversion]: partition is always a valid int from Kafka, and fatal error terminates on parse failure
	partition, _ := strconv.Atoi(partitionStr)

	switch {
	case protocol.IsMessageType(messageType, protocol.TYPE_CLEARTEXT):
		msgAttrs := logging.Event(logging.EventMessageDeliver,
			logging.Str("airgap.message.id", messageID),
			logging.Str("messaging.destination.name", topic),
			logging.Decimal("messaging.destination.partition.id", int64(partition)),
		)
		if protocol.IsMessageType(messageType, protocol.TYPE_COMPRESSED_GZIP) {
			if Logger.CanLog(logging.DEBUG) {
				Logger.Debugf(msgAttrs.With(logging.Int("airgap.message.size", int64(len(payload)))),
					"Gzip, payload length %d", len(payload))
			}
			payload, err = protocol.DecompressGzip(payload, config.maximumDecompressSize)
			if err != nil {
				Logger.Errorf(msgAttrs.Merge(logging.Err(err)), "Decompress error: %v", err)
				sendMessage(protocol.TYPE_ERROR, messageID, config.topic, []byte(err.Error()))
				return
			}
			if Logger.CanLog(logging.DEBUG) {
				Logger.Debugf(msgAttrs.With(logging.Int("airgap.message.size", int64(len(payload)))),
					"unzipped, payload length %d", len(payload))
			}
		}
		if partition < 0 || partition > 2147483647 {
			Logger.Fatalf(msgAttrs.With(logging.Failure()),
				"Partition out of int32 range: %d", partition)
			os.Exit(1)
		}
		if len(payload) == 0 {
			Logger.Debugf(msgAttrs.With(logging.Int("airgap.message.size", 0)),
				"Writing empty payload for messageID=%s to topic=%s partition=%d, messageType=%d", messageID, topic, partition, messageType)
		}
		kafkaWriter.Write(messageID, topic, int32(partition), payload)
		atomic.AddInt64(&receivedEvents, 1)
		atomic.AddInt64(&sentEvents, 1)
		atomic.AddInt64(&totalReceived, 1)
		atomic.AddInt64(&totalSent, 1)

	case protocol.IsMessageType(messageType, protocol.TYPE_MESSAGE):
		msgAttrs := logging.Event(logging.EventCryptoDecrypt,
			logging.Str("airgap.message.id", messageID),
			logging.Str("messaging.destination.name", topic),
			logging.Decimal("messaging.destination.partition.id", int64(partition)),
		)
		decrypted, err := protocol.Decrypt(payload, config.key)
		if err != nil {
			Logger.Errorf(msgAttrs.Merge(logging.Err(err)), "Decrypt error: %v", err)
			sendMessage(protocol.TYPE_ERROR, messageID, config.topic, []byte(err.Error()))
			return
		}
		if protocol.IsMessageType(messageType, protocol.TYPE_COMPRESSED_GZIP) {
			decrypted, err = protocol.DecompressGzip(decrypted, config.maximumDecompressSize)
			if err != nil {
				Logger.Errorf(msgAttrs.Merge(logging.Err(err)), "Decompress after decrypt error: %v", err)
				sendMessage(protocol.TYPE_ERROR, messageID, config.topic, []byte(err.Error()))
				return
			}
		}
		if partition < 0 || partition > 2147483647 {
			Logger.Fatalf(msgAttrs.With(logging.Failure()),
				"Partition out of int32 range: %d", partition)
			os.Exit(1)
		}
		if len(decrypted) == 0 {
			Logger.Debugf(msgAttrs.With(logging.Int("airgap.message.size", 0)),
				"Writing empty decrypted payload for messageID=%s to topic=%s partition=%d", messageID, topic, partition)
		}
		kafkaWriter.Write(messageID, topic, int32(partition), decrypted)
		atomic.AddInt64(&receivedEvents, 1)
		atomic.AddInt64(&sentEvents, 1)
		atomic.AddInt64(&totalReceived, 1)
		atomic.AddInt64(&totalSent, 1)

	case protocol.IsMessageType(messageType, protocol.TYPE_KEY_EXCHANGE):
		now := time.Now().UnixNano()
		last := atomic.LoadInt64(&lastKeyExchangeNano)
		minInterval := int64(config.keyExchangeMinIntervalSecs) * int64(time.Second)
		if minInterval > 0 && now-last < minInterval {
			Logger.Warnf(logging.Event(logging.EventCryptoKeyLoad,
				logging.Failure(),
				logging.Str("error.type", "rate.limited"),
				logging.Int("airgap.crypto.key.exchange.min.interval.seconds", int64(config.keyExchangeMinIntervalSecs)),
			), "KEY_EXCHANGE rate limit: dropping duplicate key exchange request (< %ds since last)", config.keyExchangeMinIntervalSecs)
			return
		}
		atomic.StoreInt64(&lastKeyExchangeNano, now)
		keyFile := readNewKey(payload)
		sendMessage(protocol.TYPE_STATUS, "", config.topic, []byte("Updating symmetric key with: "+keyFile))

	case protocol.IsMessageType(messageType, protocol.TYPE_ERROR):
		sendMessage(protocol.TYPE_ERROR, messageID, config.topic, payload)

	case protocol.IsMessageType(messageType, protocol.TYPE_MULTIPART):
		return

	default:
		sendMessage(protocol.TYPE_MESSAGE, "", config.topic, payload)
	}
}

// logStatistics logs periodic stats about received/sent messages
func logStatistics(stopChan <-chan struct{}) {
	interval := time.Duration(config.logStatistics) * time.Second
	Logger.Infof(logging.Event(logging.EventStatsReport,
		logging.Int("airgap.stats.interval.seconds", int64(config.logStatistics)),
	), "Starting statistics logger with interval %v", interval)
	for {
		select {
		case <-stopChan:
			return
		case <-time.After(interval):
			recv := atomic.SwapInt64(&receivedEvents, 0)
			sent := atomic.SwapInt64(&sentEvents, 0)

			// Get current transport and Kafka status
			transportStatusMu.Lock()
			transportStatus := transportStatus
			transportStatusMu.Unlock()

			kafkaStatusMu.Lock()
			kafkaStatus := kafkaStatus
			kafkaStatusMu.Unlock()

			// Get SO_RXQ_OVFL drop counts if enabled
			var rxqOvflDrops, rxqOvflDropsTotal uint64
			if config.enableRxqOvfl {
				rxqOvflDrops = udp.RxqOvflDrops.Swap(0)
				rxqOvflDropsTotal = udp.RxqOvflDropsTotal.Load()
			}

			stats := map[string]any{
				"id":               config.id,
				"time":             time.Now().Unix(),
				"time_start":       timeStart,
				"interval":         config.logStatistics,
				"received":         recv,
				"sent":             sent,
				"eps":              recv / int64(config.logStatistics),
				"total_received":   atomic.LoadInt64(&totalReceived),
				"total_sent":       atomic.LoadInt64(&totalSent),
				"transport_status": transportStatus,
				"kafka_status":     kafkaStatus,
				"cache_entries":    cache.GetCacheSize(),
			}

			// Only include SO_RXQ_OVFL stats if enabled
			if config.enableRxqOvfl {
				stats["SO_RXQ_OVFL"] = rxqOvflDrops
				stats["SO_RXQ_OVFL_TOTAL"] = rxqOvflDropsTotal
			}

			if Logger.CanLog(logging.INFO) {
				b, _ := json.Marshal(stats)
				Logger.Info(logging.Event(logging.EventStatsReport,
					logging.Str("airgap.instance.id", config.id),
					logging.Int("airgap.stats.interval.seconds", int64(config.logStatistics)),
					logging.Int("airgap.message.count", recv),
					logging.Int("airgap.message.sent", sent),
					logging.Int("airgap.stats.eps", recv/int64(config.logStatistics)),
					logging.Int("airgap.message.total.received", atomic.LoadInt64(&totalReceived)),
					logging.Int("airgap.message.total.sent", atomic.LoadInt64(&totalSent)),
					logging.Str("airgap.transport.status", transportStatus),
					logging.Str("airgap.kafka.status", kafkaStatus),
					logging.Int("airgap.cache.entries", int64(cache.GetCacheSize())),
				), "STATISTICS: "+string(b))

			}
		}
	}
}

// Main launches the downstream application
func Main(build string) {
	if handled, err := pinnedtls.GenerateCommand(os.Args[1:], "server", os.Stdout); handled {
		if err != nil {
			Logger.Fatal(logging.Event(logging.EventTLSPinVerify).Merge(logging.Err(err)), err)
		}
		return
	}
	BuildNumber = build
	Logger.Infof(logging.Event(logging.EventAppStart), "Downstream version: %s starting up...", version.GitVersion)
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

	configuration := defaultConfiguration()
	configuration, err := readConfiguration(fileName, configuration)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventConfigLoad,
			logging.Str("file.path", fileName),
		).Merge(logging.Err(err)), "Failed to read configuration: %v", err)
	}
	configuration = overrideConfiguration(configuration)
	configuration = parseCommandLineOverrides(overrideArgs, configuration)
	resolved := configuration
	if configuration.tcpTLSAuthMode == "pinned" {
		if err := pinnedtls.ValidateReloadFile(fileName); err != nil {
			Logger.Fatal(logging.Event(logging.EventConfigInvalid,
				logging.Str("file.path", fileName),
				logging.Str("airgap.config.setting", "tcpTLSAuthMode"),
				logging.Str("airgap.config.value", configuration.tcpTLSAuthMode),
			).Merge(logging.Err(err)), err)
		}
	}
	configuration = checkConfiguration(configuration)
	config = configuration

	if config.logFileName != "" {
		Logger.Infof(logging.Event(logging.EventAppStart,
			logging.Int("process.pid", int64(os.Getpid())),
		), "pid:%d", os.Getpid())
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "logFileName"),
			logging.Str("file.path", config.logFileName),
		), "Configuring log to: "+config.logFileName)
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "logFileName"),
			logging.Str("file.path", config.logFileName),
		), "Log to file started up")
	}

	if config.mtu == 0 {
		mtuValue, err := mtu.GetMTU(config.nic, fmt.Sprintf("%s:%d", config.targetIP, config.targetPort))
		if err != nil {
			Logger.Fatal(logging.Event(logging.EventConfigInvalid,
				logging.Str("airgap.config.setting", "mtu"),
				logging.Str("airgap.network.interface", config.nic),
			).Merge(logging.Err(err)), err)
		}
		config.mtu = uint16(mtuValue)
	}

	if config.logStatistics > 0 {
		// stats logger will use stopChan below
	}

	// Setup adapters
	var receiver TransportReceiver
	switch config.transport {
	case "tcp":
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "transport"),
			logging.Str("airgap.config.value", "tcp"),
		), "Using TCP receiver adapter")
		receiver = NewTCPAdapter(config)
	default:
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "transport"),
			logging.Str("airgap.config.value", "udp"),
		), "Using UDP receiver adapter")
		receiver = NewUDPAdapter(config)
	}
	defer receiver.Close()

	switch config.target {
	case "cmd":
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "target"),
			logging.Str("airgap.config.value", "cmd"),
		), "Using command line Kafka adapter")
		kafkaWriter = NewCmdAdapter()
	case "null":
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "target"),
			logging.Str("airgap.config.value", "null"),
		), "Using null adapter for performance testing")
		kafkaWriter = NewNullAdapter()
	default:
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "target"),
			logging.Str("airgap.config.value", config.target),
		), "Using Kafka adapter")
		kafkaWriter = NewKafkaAdapter()
		kafka.StopBackgroundThread()
		connectToKafka(config)
		kafka.StartBackgroundThread()
		defer kafka.StopBackgroundThread()
	}

	// Signal handling
	hup := make(chan os.Signal, 1)
	sigterm := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	stopChan := make(chan struct{})
	done := make(chan struct{})

	go func() {
		RunDownstream(receiver, stopChan)
		close(done)
	}()

	for {
		select {
		case <-sigterm:
			Logger.Infof(logging.Event(logging.EventAppShutdown,
				logging.Str("airgap.shutdown.reason", "SIGTERM"),
				logging.Str("airgap.instance.id", config.id),
			), "Received SIGTERM, shutting down")
			sendMessage(protocol.TYPE_STATUS, "", config.topic,
				fmt.Appendf(nil, "Downstream %s terminating by signal...", config.id))
			kafka.FlushCache()
			close(stopChan)
			<-done
			receiver.Close()
			kafka.StopBackgroundThread()
			if config.target == "kafka" {
				kafka.CloseProducer()
			}
			config.WarnProductionConfiguration("shutdown")
			return
		case <-hup:
			hupAttrs := logging.Event(logging.EventAppReload,
				logging.Str("airgap.shutdown.reason", "SIGHUP"),
				logging.Str("file.path", config.logFileName),
			)
			Logger.Infof(hupAttrs, "SIGHUP received: reopening logs with name %s and reloading TLS certificates", config.logFileName)
			if config.logFileName != "" {
				Logger.Infof(hupAttrs, "Reopening logs for logrotate. New name: %s", config.logFileName)
				if err := Logger.SetLogFile(config.logFileName); err != nil {
					Logger.Errorf(hupAttrs.Merge(logging.Err(err)), "Failed reopening log file: %v", err)
				}
			}
			if tcpAdapter, ok := receiver.(*TCPAdapter); ok {
				var err error
				if tcpAdapter.pinned {
					err = reloadPinnedConfiguration(fileName, overrideArgs, &resolved, tcpAdapter)
				} else {
					err = tcpAdapter.ReloadTLSCert()
				}
				if err != nil {
					Logger.Errorf(logging.Event(logging.EventTLSCertReload,
						logging.Str("file.path", fileName),
					).Merge(logging.Err(err)), "TLS certificate reload failed: %v", err)
				}
			}
			Logger.Info(hupAttrs, "SIGHUP handling completed")
		case <-done:
			Logger.Info(logging.Event(logging.EventAppStop,
				logging.Str("airgap.instance.id", config.id),
			), "RunDownstream finished, exiting")
			sendMessage(protocol.TYPE_STATUS, "", config.topic,
				fmt.Appendf(nil, "Downstream %s shutting down", config.id))
			kafka.FlushCache()
			close(stopChan)
			receiver.Close()
			kafka.StopBackgroundThread()
			if config.target == "kafka" {
				kafka.CloseProducer()
			}
			config.WarnProductionConfiguration("shutdown")
			return
		}
	}
}
