package downstream

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/mtu"
	"sitia.nu/airgap/src/protocol"
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

// RunDownstream runs the downstream process
func RunDownstream(kafkaWriter KafkaWriter, udpReceiver UDPReceiver, stopChan <-chan struct{}) {
	timeStart = time.Now().Unix()

	if config.mtu == 0 {
		mtuValue, err := mtu.GetMTU(config.nic, fmt.Sprintf("%s:%d", config.targetIP, config.targetPort))
		if err != nil {
			Logger.Fatal(err)
		}
		config.mtu = uint16(mtuValue)
	}

	if config.logStatistics > 0 {
		go logStatistics(stopChan)
	}

	Logger.Infof("Downstream version: %s", BuildNumber)
	sendMessage(protocol.TYPE_STATUS, "", config.topic,
		fmt.Appendf(nil, "Downstream starting UDP server on port %d", config.targetPort))

	handleUdpMessage := func(msg []byte) {
		messageType, messageID, payload, err := protocol.ParseMessage(msg, cache)
		if err != nil {
			Logger.Errorf("Failed to parse UDP message: %v", err)
			return
		}

		topic, partitionStr, _, err := protocol.ParseMessageId(messageID)
		if err != nil {
			topic = config.topic
			partitionStr = "0"
		}
		topic = translateTopic(topic)
		partition, _ := strconv.Atoi(partitionStr)

		switch {
		case protocol.IsMessageType(messageType, protocol.TYPE_CLEARTEXT):
			if protocol.IsMessageType(messageType, protocol.TYPE_COMPRESSED_GZIP) {
				payload, err = protocol.DecompressGzip(payload)
				if err != nil {
					Logger.Errorf("Decompress error: %v", err)
					sendMessage(protocol.TYPE_ERROR, messageID, config.topic, []byte(err.Error()))
					return
				}
			}
			kafkaWriter.Write(messageID, topic, int32(partition), payload)
			atomic.AddInt64(&receivedEvents, 1)
			atomic.AddInt64(&sentEvents, 1)
			atomic.AddInt64(&totalReceived, 1)
			atomic.AddInt64(&totalSent, 1)

		case protocol.IsMessageType(messageType, protocol.TYPE_MESSAGE):
			decrypted, err := protocol.Decrypt(payload, config.key)
			if err != nil {
				Logger.Errorf("Decrypt error: %v", err)
				sendMessage(protocol.TYPE_ERROR, messageID, config.topic, []byte(err.Error()))
				return
			}
			if protocol.IsMessageType(messageType, protocol.TYPE_COMPRESSED_GZIP) {
				decrypted, err = protocol.DecompressGzip(decrypted)
				if err != nil {
					Logger.Errorf("Decompress after decrypt error: %v", err)
					sendMessage(protocol.TYPE_ERROR, messageID, config.topic, []byte(err.Error()))
					return
				}
			}
			kafkaWriter.Write(messageID, topic, int32(partition), decrypted)
			atomic.AddInt64(&receivedEvents, 1)
			atomic.AddInt64(&sentEvents, 1)
			atomic.AddInt64(&totalReceived, 1)
			atomic.AddInt64(&totalSent, 1)

		case protocol.IsMessageType(messageType, protocol.TYPE_KEY_EXCHANGE):
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

	// Setup UDP receiver
	udpReceiver.Setup(
		config.mtu,
		config.numReceivers,
		config.channelBufferSize,
		config.readBufferMultiplier,
	)
	go udpReceiver.Listen(
		config.targetIP,
		config.targetPort,
		config.rcvBufSize,
		handleUdpMessage,
		config.mtu,
		stopChan,
		config.numReceivers,
	)
	// Wait for stop signal
	<-stopChan

	// Give a small timeout to flush remaining messages
	time.Sleep(2 * time.Second)
}

// logStatistics logs periodic stats about received/sent messages
func logStatistics(stopChan <-chan struct{}) {
	interval := time.Duration(config.logStatistics) * time.Second
	Logger.Printf("Starting statistics logger with interval %v", interval)
	for {
		select {
		case <-stopChan:
			return
		case <-time.After(interval):
			recv := atomic.SwapInt64(&receivedEvents, 0)
			sent := atomic.SwapInt64(&sentEvents, 0)
			stats := map[string]any{
				"id":             config.id,
				"time":           time.Now().Unix(),
				"time_start":     timeStart,
				"interval":       config.logStatistics,
				"received":       recv,
				"sent":           sent,
				"eps":            recv / int64(config.logStatistics),
				"total_received": atomic.LoadInt64(&totalReceived),
				"total_sent":     atomic.LoadInt64(&totalSent),
			}
			if Logger.CanLog(logging.INFO) {
				b, _ := json.Marshal(stats)
				Logger.Info("STATISTICS: " + string(b))
			}
		}
	}
}

// Main launches the downstream application
func Main(build string) {
	BuildNumber = build
	Logger.Printf("Downstream version: %s starting up...", version.GitVersion)
	Logger.Printf("Build number: %s", BuildNumber)

	var fileName string
	if len(os.Args) > 2 {
		Logger.Fatal("Too many command line parameters. Only one allowed.")
	}
	if len(os.Args) == 2 {
		fileName = os.Args[1]
	}

	configuration := defaultConfiguration()
	configuration, err := readConfiguration(fileName, configuration)
	if err != nil {
		Logger.Fatalf("Failed to read configuration: %v", err)
	}
	configuration = overrideConfiguration(configuration)
	configuration = checkConfiguration(configuration)
	config = configuration

	if config.logFileName != "" {
		Logger.Print("Configuring log to: " + config.logFileName)
		if err := Logger.SetLogFile(config.logFileName); err != nil {
			Logger.Fatal(err)
		}
		Logger.Print("Log to file started up")
	}

	if config.mtu == 0 {
		mtuValue, err := mtu.GetMTU(config.nic, fmt.Sprintf("%s:%d", config.targetIP, config.targetPort))
		if err != nil {
			Logger.Fatal(err)
		}
		config.mtu = uint16(mtuValue)
	}

	if config.logStatistics > 0 {
		// stats logger will use stopChan below
	}

	// Setup adapters
	udpReceiver := NewUDPAdapter(config)
	defer udpReceiver.Close()

	var kafkaWriter KafkaWriter
	switch config.target {
	case "cmd":
		Logger.Print("Using command line Kafka adapter")
		kafkaWriter = NewCmdAdapter()
	case "null":
		Logger.Print("Using null adapter for performance testing")
		kafkaWriter = NewNullAdapter()
	default:
		Logger.Print("Using Kafka adapter")
		kafkaWriter = NewKafkaAdapter()
		kafka.StopBackgroundThread()
		connectToKafka(config)
		kafka.StartBackgroundThread()
		defer kafka.StopBackgroundThread()
	}
	defer kafkaWriter.Flush()

	// Signal handling
	hup := make(chan os.Signal, 1)
	sigterm := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	stopChan := make(chan struct{})
	done := make(chan struct{})

	go func() {
		RunDownstream(kafkaWriter, udpReceiver, stopChan)
		close(done)
	}()

	for {
		select {
		case <-sigterm:
			Logger.Printf("Received SIGTERM, shutting down")
			sendMessage(protocol.TYPE_STATUS, "", config.topic,
				fmt.Appendf(nil, "Downstream %s terminating by signal...", config.id))
			kafka.FlushCache()
			close(stopChan)
			udpReceiver.Close()
			kafkaWriter.Close()
			return
		case <-hup:
			Logger.Printf("SIGHUP received: reopening logs for logrotate. New name: %s", config.logFileName)
			if config.logFileName != "" {
				if err := Logger.SetLogFile(config.logFileName); err != nil {
					Logger.Errorf("Failed reopening log file: %v", err)
				}
			}
			Logger.Printf("Logrotate completed")
		case <-done:
			Logger.Printf("RunDownstream finished, exiting")
			sendMessage(protocol.TYPE_STATUS, "", config.topic,
				fmt.Appendf(nil, "Downstream %s shutting down", config.id))
			kafka.FlushCache()
			close(stopChan)
			udpReceiver.Close()
			kafkaWriter.Close()
			return
		}
	}
}
