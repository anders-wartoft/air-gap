// src/resend/resend.go
package resend

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

type GapResult struct {
	Gaps       [][]int64 `json:"gaps"`
	Partition  int       `json:"partition"`
	Topic      string    `json:"topic"`
	WindowMax  int64     `json:"window_max"`
	WindowMin  int64     `json:"window_min"`
	LastOffset int64     `json:"last_offset"`
}

type JsonFilter struct {
	Type    string      `json:"type"`
	Results []GapResult `json:"results"`
}

func parseFile(fileName string, config TransferConfiguration) (JsonFilter, error) {
	if config.topic != "" && config.partition >= 0 && config.offsetFrom >= 0 && config.offsetTo >= -1 {
		jsonFilter := JsonFilter{
			Type: "manual",
			Results: []GapResult{
				{
					Partition: config.partition,
					Topic:     config.topic,
					Gaps:      [][]int64{{config.offsetFrom, config.offsetTo}},
				},
			},
		}
		Logger.Infof("Using manual resend configuration: topic=%s, partition=%d, from_offset=%d, to_offset=%d",
			config.topic, config.partition, config.offsetFrom, config.offsetTo)
		return jsonFilter, nil
	}

	if config.topic != "" && config.from != "" {
		jsonFilter := JsonFilter{
			Type: "time-range",
			Results: []GapResult{
				{
					Partition: config.partition,
					Topic:     config.topic,
					Gaps:      [][]int64{{0, -1}},
				},
			},
		}
		Logger.Infof("Using manual resend configuration: topic=%s, partition=%d, from_offset=%d, to_offset=%d",
			config.topic, config.partition, config.offsetFrom, config.offsetTo)
		return jsonFilter, nil
	}

	file, err := os.Open(fileName)
	if err != nil {
		return JsonFilter{}, err
	}
	defer file.Close()

	var result JsonFilter
	if err := json.NewDecoder(file).Decode(&result); err != nil {
		return JsonFilter{}, err
	}
	return result, nil
}

// getLatestOffsets updates each GapResult in a JsonFilter with the latest offset
// from Kafka for its topic and partition.
func getLatestOffsets(kafkaClient KafkaClient, jsonFilter JsonFilter, bootstrapServers string) (JsonFilter, error) {
	for i, result := range jsonFilter.Results {
		lastOffset, err := kafkaClient.GetLastOffset(bootstrapServers, result.Topic, result.Partition)
		if err != nil {
			if result.Partition != -1 {
				Logger.Fatalf("Failed to get latest offset for topic %s partition %d: %v. If partition is not found, it may be due to the topic not existing or no messages being produced.",
					result.Topic, result.Partition, err)
			}
			continue
		}
		jsonFilter.Results[i].LastOffset = lastOffset
		Logger.Debugf("Topic %s partition %d latest offset: %d",
			result.Topic, result.Partition, lastOffset)
	}
	return jsonFilter, nil
}

func isLast(jsonFilter JsonFilter, topic string, partition int, offset int64) bool {
	// Is this the last message to be sent for the given topic and partition?
	for _, g := range jsonFilter.Results {
		if g.Topic == topic && g.Partition == partition {
			Logger.Tracef("Last offset for topic %s partition %d is %d, current offset %d",
				topic, partition, g.LastOffset, offset)
			return offset >= g.LastOffset
		}
	}
	Logger.Trace("Did not find topic/partition in filter when checking for last message")
	return false
}

func filter(jsonFilter JsonFilter, topic string, partition int, offset int64) bool {
	// Should the message with given topic, partition and offset be sent?
	switch jsonFilter.Type {
	case "first":
		// There is only one gap provided, the first. Send everything from the start of that gap and onwards.
		gaps := jsonFilter.Results
		for _, g := range gaps {
			if g.Topic == topic && g.Partition == partition {
				// Just look at the first gap
				first := g.Gaps[0][0] + g.WindowMin
				// Send everything from first and onwards
				return offset >= first
			}
		}
	case "all":
		// All gaps are provided. Send messages that fall within any of the gaps.
		gaps := jsonFilter.Results
		for _, g := range gaps {
			if g.Topic == topic && g.Partition == partition {
				for _, gap := range g.Gaps {
					if len(gap) > 1 { // [min, max]
						if offset >= (gap[0]+g.WindowMin) && offset <= (gap[1]+g.WindowMin) {
							Logger.Debugf("Offset %d is within gap [%d,%d] (windowMin %d)", offset, gap[0], gap[1], g.WindowMin)
							return true
						}
					} else if len(gap) == 1 { // [exact]
						if offset == (gap[0] + g.WindowMin) {
							Logger.Debugf("Offset %d is equal to gap [%d] (windowMin %d)", offset, gap[0], g.WindowMin)
							return true
						}
					}
				}
			}
		}
	case "manual":
		// Manual mode: only one result with one gap, specifying from_offset and to_offset
		// We have no gaps, but from_offset and to_offset, partition and topic. Use those for resend:
		if jsonFilter.Results[0].Topic == topic && jsonFilter.Results[0].Partition == partition {
			Logger.Debugf("Manual mode: checking if offset %d is between from_offset %d and to_offset %d", offset, jsonFilter.Results[0].Gaps[0][0], jsonFilter.Results[0].Gaps[0][1])
			return offset >= jsonFilter.Results[0].Gaps[0][0] && (offset <= jsonFilter.Results[0].Gaps[0][1] || jsonFilter.Results[0].Gaps[0][1] == -1)
		}
	case "time-range":
		// Time-range mode: only one result with one gap, specifying from and to time, partition and topic. From and to are handled above in kafkaHandler.
		if jsonFilter.Results[0].Topic == topic && (jsonFilter.Results[0].Partition == partition || jsonFilter.Results[0].Partition == -1) {
			// We don't have offsets to compare here, so we just return true to send everything in this partition.
			return true
		}
	default:
		// jsonFilter is empty since no resendFileName was provided
		// Send everything
		return true
	}
	return false
}

// RunResend reads messages from Kafka according to the resend file and sends them via UDP.
// Runs one Kafka reader goroutine per "result" entry and cancels all readers
// when every partition has reached its recorded LastOffset.
func RunResend(kafkaClient KafkaClient, udpClient UDPClient, config TransferConfiguration, done chan<- struct{}) {
	// Start time for statistics
	timeStart = time.Now().Unix()
	Logger.Debugf("RunResend starting at: %s", time.Now().Format(time.RFC3339))

	// Read from the beginning of time (this parameter is used by the kafka adapter)
	// Format: "1970-01-01T00:00:00Z"
	fromTime, err := time.Parse(time.RFC3339, config.from)
	if err != nil {
		Logger.Fatalf("Invalid from time format: %v", err)
	}
	Logger.Debugf("From time is set to: %s", fromTime.Format(time.RFC3339))
	// If no config.to is set, use now:
	toTime := time.Now()
	if config.to != "" {
		toTime, err = time.Parse(time.RFC3339, config.to)
		if err != nil {
			Logger.Fatalf("Invalid to time format: %v", err)
		}
		if toTime.Before(fromTime) {
			Logger.Fatalf("The 'to' time must be after the 'from' time.")
		}
		Logger.Debugf("To time is set to: %s", toTime.Format(time.RFC3339))
	}

	// Send startup status message
	Logger.Debug("Sending startup status message")
	messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
		fmt.Appendf(nil, "%s %s starting up with resendFileName: %s", protocol.GetTimestamp(), config.id, config.resendFileName), config.payloadSize)
	if len(messages) > 0 {
		if err := udpClient.SendMessage(messages[0]); err != nil {
			Logger.Errorf("Failed sending startup message: %v", err)
		}
	}

	var jsonFilter JsonFilter

	// Read filter file and enrich with latest offsets
	jsonFilter, err = parseFile(config.resendFileName, config)
	if err != nil {
		Logger.Fatalf("Failed parsing filter file: %v", err)
		return
	}
	jsonFilter, _ = getLatestOffsets(kafkaClient, jsonFilter, config.bootstrapServers)
	Logger.Tracef("Resend filter content length: %d", len(jsonFilter.Results))

	// Encryption setup (unchanged)
	if config.encryption {
		Logger.Printf("Creating initial key with public key file: %s", config.publicKeyFile)
		if adapter, ok := udpClient.(*UDPAdapter); ok {
			sendNewKey(adapter.conn, &config)
		} else {
			Logger.Error("udpClient is not of type *UDPAdapter")
		}
	} else {
		Logger.Printf("No encryption will be used.")
	}
	// Context that will be cancelled when all partitions are done
	// ctx, cancel := context.WithCancel(context.Background())
	// defer cancel()
	ctx := context.Background()

	// Track finished partitions
	var wg sync.WaitGroup
	var finished sync.Map // map[int]bool keyed by partition

	// helper: mark partition finished and check if all done
	markFinished := func(partition int) {
		finished.Store(partition, true)
		// check all partitions
		allDone := true
		for _, r := range jsonFilter.Results {
			if done, ok := finished.Load(r.Partition); !ok || !done.(bool) {
				allDone = false
				break
			}
		}
		if allDone {
			Logger.Info("All partitions reached their last offsets — cancelling Kafka readers.")
			// cancel readers; they'll return and wg will be unblocked
			//			cancel()
		}
	}

	// EPS limiter state
	var lastSendTime int64 = 0
	var limiterMu sync.Mutex

	// ----- Kafka handler (shared) -----
	kafkaHandler := func(id string, key []byte, t time.Time, received []byte) bool {
		atomic.AddInt64(&receivedEvents, 1)
		atomic.AddInt64(&totalReceived, 1)

		// Determine topic/partition/offset from id
		parts := strings.Split(id, "_")
		var topic string
		var partition int
		var offset int64
		if len(parts) == 3 {
			topic = parts[0]
			p, err := strconv.Atoi(parts[1])
			if err == nil {
				partition = p
			}
			if off, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				offset = off
			}
		}
		Logger.Tracef("kafkaHandler called for id=%s, topic=%s, partition=%d, offset=%d, time=%s", id, topic, partition, offset, t.Format(time.RFC3339))

		// Discard old messages, before "from"
		if t.Before(fromTime) {
			Logger.Tracef("Discarding old message: %s time %s before from %s", id, t.Format(time.RFC3339), fromTime.Format(time.RFC3339))
			return keepRunning
		}

		// Discard messages after "to"
		if t.After(toTime) {
			Logger.Tracef("Discarding message: %s time %s after to %s", id, t.Format(time.RFC3339), toTime.Format(time.RFC3339))
			markFinished(partition)
			// return false to indicate consumer can stop this claim (adapter may honor it)
			return false
		}

		// Filtering: skip messages that are not in the resend filter
		shouldSend := filter(jsonFilter, topic, partition, offset)
		if !shouldSend {
			// If message is not to be sent but it's equal-or-after lastOffset, we still consider partition finished.
			if isLast(jsonFilter, topic, partition, offset) {
				markFinished(partition)
				// returning false signals the consumer to stop processing this message stream if the adapter honors it.
				return false
			}
			// keep running
			return keepRunning
		}

		// If this is the last message that we will send for this partition, mark finished after sending.
		last := isLast(jsonFilter, topic, partition, offset)

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

		// EPS limiter: if config.eps > 0, limit sending rate
		if config.eps > 0 {
			limiterMu.Lock()
			now := time.Now().UnixNano()
			minInterval := int64(float64(time.Second) / config.eps)
			if lastSendTime > 0 {
				nextAllowed := lastSendTime + minInterval
				if now < nextAllowed {
					time.Sleep(time.Duration(nextAllowed - now))
					now = nextAllowed
				}
			}
			lastSendTime = now
			limiterMu.Unlock()
		}
		if uerr := udpClient.SendMessages(messages); uerr != nil {
			Logger.Errorf("UDP send error: %v", uerr)
		} else {
			atomic.AddInt64(&sentEvents, int64(len(messages)))
			atomic.AddInt64(&totalSent, int64(len(messages)))
		}

		// If this was last, mark it finished and possibly cancel
		if last {
			markFinished(partition)
			// return false to indicate consumer can stop this claim (adapter may honor it)
			return false
		}

		return keepRunning
	}

	// TLS for kafka if needed
	if config.certFile != "" || config.keyFile != "" || config.caFile != "" {
		Logger.Print("Using TLS for Kafka")
		err := kafkaClient.SetTLS(config.certFile, config.keyFile, config.caFile, config.keyPasswordFile)
		if err != nil {
			Logger.Panicf("Failed to configure TLS: %v", err)
		}
	}

	Logger.Debugf("Got %d results from resend filter", len(jsonFilter.Results))
	Logger.Debugf("Resend filter content: %+v", jsonFilter)

	// Start one kafka reader goroutine per GapResult (partition)
	for _, r := range jsonFilter.Results {
		wg.Add(1)
		go func(res GapResult) {
			defer wg.Done()
			// name per partition + timestamp
			timestamp := time.Now().Format("20060102150405")
			name := fmt.Sprintf("%s-%d-%s", config.groupID, res.Partition, timestamp)

			// The adapter's Read must accept ctx and return eventually (ReadFromKafkaWithContext does).
			// We call Read with "from" as configured earlier; adapters should internally honor ctx.
			Logger.Debugf("Starting Kafka read for partition %d with name %s with bootstrapServers: %s", res.Partition, name, config.bootstrapServers)
			fromOffset := int64(0)
			kafkaClient.ReadToEndPartition(res.Partition, ctx, config.bootstrapServers, res.Topic, name, fromOffset,
				kafkaHandler)
			// // If we exit the read loop without having marked finished, do it now.
			// if _, ok := finished.Load(res.Partition); !ok {
			// 	markFinished(res.Partition)
			// }
			// When Read returns, it means the consumer was stopped (ctx cancelled or adapter finished).
			Logger.Debugf("Kafka reader for partition %d returned", res.Partition)
		}(r)
	}

	// Wait for all readers to finish (cancel() will cause them to return)
	wg.Wait()
	Logger.Info("RunResend completed: all kafka readers finished")

	// Signal completion
	if done != nil {
		close(done)
	}
}

func Main(build string) {
	BuildNumber = build
	Logger.Printf("Resend version: %s starting up...", version.GitVersion)
	Logger.Printf("Build number: %s", BuildNumber)

	var fileName string
	var overrideArgs []string
	// Parse command line: first non-dashed argument is fileName, rest are overrides
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--") {
			overrideArgs = append(overrideArgs, arg)
		} else if fileName == "" {
			fileName = arg
		} else {
			Logger.Fatalf("Too many non-dashed command line parameters. Only one property file allowed.")
		}
	}

	// Start with the default parameters
	config := defaultConfiguration()
	// Read configuration from file, if added
	var err error
	config, err = readParameters(fileName, config)
	// May override with environment variables
	config = overrideConfiguration(config)
	// Apply command line overrides
	config = parseCommandLineOverrides(overrideArgs, config)
	// Make sure we have everything we need in the config
	config = checkConfiguration(config)
	if err != nil {
		Logger.Fatalf("Error reading configuration file %s: %v", fileName, err)
	}

	// Set the log file name
	if config.logFileName != "" {
		Logger.Print("Configuring log to: " + config.logFileName)
		err := Logger.SetLogFile(config.logFileName)
		if err != nil {
			Logger.Fatal(err)
		}
		Logger.Printf("resend version: %s", version.GitVersion)
		Logger.Print("Log to file started up")
	}

	// Now log the complete configuration to stdout or file
	logConfiguration(config)

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
	}
	Logger.Printf("payloadSize read from MTU: %d\n", config.payloadSize)

	// Signals
	hup := make(chan os.Signal, 1)
	sigterm := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	// Create UDP
	address := fmt.Sprintf("%s:%d", config.targetIP, config.targetPort)
	udpAdapter, err := NewUDPAdapter(address)
	if err != nil {
		Logger.Fatalf("Error creating UDP: %v", err)
	}

	// Choose Kafka adapter
	kafkaAdapter := NewKafkaAdapter()
	done := make(chan struct{})

	// Run upstream
	go RunResend(kafkaAdapter, udpAdapter, config, done)

	// Wait for exit signals
	for {
		select {
		case <-done:
			// print logStatistics one last time
			recv := atomic.SwapInt64(&receivedEvents, 0)
			sent := atomic.SwapInt64(&sentEvents, 0)
			stats := map[string]any{
				"id":             config.id,
				"time":           time.Now().Unix(),
				"time_start":     timeStart,
				"interval":       config.logStatistics,
				"received":       recv,
				"sent":           sent,
				"total_received": atomic.LoadInt64(&totalReceived),
				"total_sent":     atomic.LoadInt64(&totalSent),
			}
			if Logger.CanLog(logging.INFO) {
				b, _ := json.Marshal(stats)
				Logger.Info("STATISTICS: " + string(b))
			}
			Logger.Info("Resend process finished. Exiting...")
			messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
				fmt.Appendf(nil, "%s %s finished", protocol.GetTimestamp(), config.id), config.payloadSize)
			udpAdapter.SendMessage(messages[0])
			udpAdapter.Close()
			return
		case <-sigterm:
			Logger.Printf("Received SIGTERM, shutting down")
			messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
				fmt.Appendf(nil, "%s %s shutting down", protocol.GetTimestamp(), config.id), config.payloadSize)
			udpAdapter.SendMessage(messages[0])
			udpAdapter.Close()
			return
		case <-hup:
			Logger.Printf("SIGHUP received: reopening logs")
			if config.logFileName != "" {
				if err := Logger.SetLogFile(config.logFileName); err != nil {
					Logger.Errorf("Failed reopening log file: %v", err)
				}
			}
		}
	}
}
