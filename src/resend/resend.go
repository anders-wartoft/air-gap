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

func hasUsableGaps(filterType string, result GapResult) bool {
	switch filterType {
	case "first", "manual":
		return len(result.Gaps) > 0 && len(result.Gaps[0]) > 0
	case "all":
		for _, gap := range result.Gaps {
			if len(gap) > 0 {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func isPartitionInConfiguredRange(partition int, partitionStartValue int, partitionStopValue int) bool {
	if partition == -1 {
		return partitionStopValue == 0
	}
	if partition < partitionStartValue {
		return false
	}
	if partitionStopValue == 0 {
		return true
	}
	return partition < partitionStartValue+partitionStopValue
}

func filterJsonFilterByPartitionRange(jsonFilter JsonFilter, partitionStartValue int, partitionStopValue int) JsonFilter {
	if partitionStopValue == 0 {
		return jsonFilter
	}

	filtered := make([]GapResult, 0, len(jsonFilter.Results))
	for _, result := range jsonFilter.Results {
		if isPartitionInConfiguredRange(result.Partition, partitionStartValue, partitionStopValue) {
			if !hasUsableGaps(jsonFilter.Type, result) {
				Logger.Debugf(logging.Event(logging.EventResendRequest,
					logging.Decimal("messaging.destination.partition.id", int64(result.Partition)),
					logging.Str("airgap.resend.filter.type", jsonFilter.Type),
				), "Skipping partition %d due to empty gaps for filter type %s", result.Partition, jsonFilter.Type)
				continue
			}
			filtered = append(filtered, result)
		} else {
			Logger.Debugf(logging.Event(logging.EventResendRequest,
				logging.Decimal("messaging.destination.partition.id", int64(result.Partition)),
				logging.Int("airgap.partition.start", int64(partitionStartValue)),
				logging.Int("airgap.partition.stop", int64(partitionStopValue)),
			), "Skipping partition %d due to configured range start=%d stop=%d",
				result.Partition, partitionStartValue, partitionStopValue)
		}
	}

	jsonFilter.Results = filtered
	return jsonFilter
}

func getStartOffsetForResult(filterType string, result GapResult) int64 {
	switch filterType {
	case "first":
		if len(result.Gaps) > 0 && len(result.Gaps[0]) > 0 {
			return result.Gaps[0][0]
		}
	case "all":
		// Optimization: start from the first gap instead of offset 0
		if len(result.Gaps) > 0 && len(result.Gaps[0]) > 0 {
			return result.Gaps[0][0]
		}
		return 0
	case "manual":
		if len(result.Gaps) > 0 && len(result.Gaps[0]) > 0 {
			return result.Gaps[0][0]
		}
	}

	return 0
}

func mapDownstreamToUpstreamPartition(downstreamPartition int, partitionStartValue int) (int, error) {
	if downstreamPartition == -1 {
		return -1, nil
	}
	upstreamPartition := downstreamPartition - partitionStartValue
	if upstreamPartition < 0 {
		return 0, fmt.Errorf("downstream partition %d maps to negative upstream partition with partitionStartValue %d", downstreamPartition, partitionStartValue)
	}
	return upstreamPartition, nil
}

func mapUpstreamToDownstreamPartition(upstreamPartition int, partitionStartValue int) int {
	if upstreamPartition == -1 {
		return -1
	}
	return upstreamPartition + partitionStartValue
}

func parseFile(fileName string, config TransferConfiguration) (JsonFilter, error) {
	if fileName != "" {
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
		Logger.Infof(logging.Event(logging.EventResendRequest,
			logging.Str("airgap.resend.filter.type", "manual"),
			logging.Str("messaging.destination.name", config.topic),
			logging.Decimal("messaging.destination.partition.id", int64(config.partition)),
			logging.Int("airgap.resend.offset.from", config.offsetFrom),
			logging.Int("airgap.resend.offset.to", config.offsetTo),
		), "Using manual resend configuration: topic=%s, partition=%d, from_offset=%d, to_offset=%d",
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
		Logger.Infof(logging.Event(logging.EventResendRequest,
			logging.Str("airgap.resend.filter.type", "time-range"),
			logging.Str("messaging.destination.name", config.topic),
			logging.Decimal("messaging.destination.partition.id", int64(config.partition)),
			logging.Str("airgap.resend.time.from", config.from),
			logging.Str("airgap.resend.time.to", config.to),
		), "Using manual resend configuration: topic=%s, partition=%d, from_offset=%d, to_offset=%d",
			config.topic, config.partition, config.offsetFrom, config.offsetTo)
		return jsonFilter, nil
	}

	return JsonFilter{}, fmt.Errorf("no resend input specified: provide resendFileName or manual topic/partition/from settings")
}

// getLatestOffsets updates each GapResult in a JsonFilter with the latest offset
// from Kafka for its topic and partition.
func getLatestOffsets(kafkaClient KafkaClient, jsonFilter JsonFilter, bootstrapServers string, partitionStartValue int) (JsonFilter, error) {
	for i, result := range jsonFilter.Results {
		upstreamPartition, mapErr := mapDownstreamToUpstreamPartition(result.Partition, partitionStartValue)
		if mapErr != nil {
			return jsonFilter, mapErr
		}
		lastOffset, err := kafkaClient.GetLastOffset(bootstrapServers, result.Topic, upstreamPartition)
		if err != nil {
			if result.Partition != -1 {
				Logger.Fatalf(logging.Event(logging.EventKafkaOffset,
					logging.Str("messaging.destination.name", result.Topic),
					logging.Decimal("messaging.destination.partition.id", int64(upstreamPartition)),
				).Merge(logging.Err(err)),
					"Failed to get latest offset for topic %s partition %d: %v. If partition is not found, it may be due to the topic not existing or no messages being produced.",
					result.Topic, upstreamPartition, err)
			}
			continue
		}
		jsonFilter.Results[i].LastOffset = lastOffset
		Logger.Debugf(logging.Event(logging.EventKafkaOffset,
			logging.Str("messaging.destination.name", result.Topic),
			logging.Decimal("messaging.destination.partition.id", int64(upstreamPartition)),
			logging.Int("airgap.partition.downstream", int64(result.Partition)),
			logging.Int("messaging.kafka.offset", lastOffset),
		), "Topic %s downstream partition %d (upstream partition %d) latest offset: %d",
			result.Topic, result.Partition, upstreamPartition, lastOffset)
	}
	return jsonFilter, nil
}

func isLast(jsonFilter JsonFilter, topic string, partition int, offset int64) bool {
	// Is this the last message to be sent for the given topic and partition?
	for _, g := range jsonFilter.Results {
		if g.Topic == topic && g.Partition == partition {
			Logger.Tracef(logging.Event(logging.EventKafkaOffset,
				logging.Str("messaging.destination.name", topic),
				logging.Decimal("messaging.destination.partition.id", int64(partition)),
				logging.Int("airgap.resend.offset.last", g.LastOffset),
				logging.Int("messaging.kafka.offset", offset),
			), "Last offset for topic %s partition %d is %d, current offset %d",
				topic, partition, g.LastOffset, offset)
			return offset >= g.LastOffset
		}
	}
	Logger.Trace(logging.Event(logging.EventMessageFilter,
		logging.Str("messaging.destination.name", topic),
		logging.Decimal("messaging.destination.partition.id", int64(partition)),
	), "Did not find topic/partition in filter when checking for last message")
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
				if len(g.Gaps) == 0 || len(g.Gaps[0]) == 0 {
					return false
				}
				// Just look at the first gap; offsets are absolute
				first := g.Gaps[0][0]
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
						if offset >= gap[0] && offset <= gap[1] {
							Logger.Debugf(logging.Event(logging.EventMessageFilter,
								logging.Str("messaging.destination.name", topic),
								logging.Decimal("messaging.destination.partition.id", int64(partition)),
								logging.Int("messaging.kafka.offset", offset),
								logging.Int("airgap.gap.from", gap[0]),
								logging.Int("airgap.gap.to", gap[1]),
							), "Offset %d is within gap [%d,%d]", offset, gap[0], gap[1])
							return true
						}
					} else if len(gap) == 1 { // [exact]
						if offset == gap[0] {
							Logger.Debugf(logging.Event(logging.EventMessageFilter,
								logging.Str("messaging.destination.name", topic),
								logging.Decimal("messaging.destination.partition.id", int64(partition)),
								logging.Int("messaging.kafka.offset", offset),
								logging.Int("airgap.gap.from", gap[0]),
							), "Offset %d is equal to gap [%d]", offset, gap[0])
							return true
						}
					}
				}
			}
		}
	case "manual":
		// Manual mode: only one result with one gap, specifying from_offset and to_offset
		// We have no gaps, but from_offset and to_offset, partition and topic. Use those for resend:
		if len(jsonFilter.Results) == 0 || len(jsonFilter.Results[0].Gaps) == 0 || len(jsonFilter.Results[0].Gaps[0]) == 0 {
			return false
		}
		if jsonFilter.Results[0].Topic == topic && jsonFilter.Results[0].Partition == partition {
			Logger.Debugf(logging.Event(logging.EventMessageFilter,
				logging.Str("airgap.resend.filter.type", "manual"),
				logging.Str("messaging.destination.name", topic),
				logging.Decimal("messaging.destination.partition.id", int64(partition)),
				logging.Int("messaging.kafka.offset", offset),
				logging.Int("airgap.resend.offset.from", jsonFilter.Results[0].Gaps[0][0]),
				logging.Int("airgap.resend.offset.to", jsonFilter.Results[0].Gaps[0][1]),
			), "Manual mode: checking if offset %d is between from_offset %d and to_offset %d", offset, jsonFilter.Results[0].Gaps[0][0], jsonFilter.Results[0].Gaps[0][1])
			return offset >= jsonFilter.Results[0].Gaps[0][0] && (offset <= jsonFilter.Results[0].Gaps[0][1] || jsonFilter.Results[0].Gaps[0][1] == -1)
		}
	case "time-range":
		// Time-range mode: only one result with one gap, specifying from and to time, partition and topic. From and to are handled above in kafkaHandler.
		if len(jsonFilter.Results) == 0 {
			return false
		}
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
	runResend(context.Background(), kafkaClient, udpClient, config, done)
}

func runResend(ctx context.Context, kafkaClient KafkaClient, udpClient UDPClient, config TransferConfiguration, done chan<- struct{}) {
	// Start time for statistics
	timeStart = time.Now().Unix()
	Logger.Debugf(logging.Event(logging.EventResendReplay),
		"RunResend starting at: %s", time.Now().Format(time.RFC3339))

	// Read from the beginning of time (this parameter is used by the kafka adapter)
	// Format: "1970-01-01T00:00:00Z"
	fromTime, err := time.Parse(time.RFC3339, config.from)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
			logging.Str("airgap.config.setting", "from"),
			logging.Str("airgap.config.value", config.from),
		).Merge(logging.Err(err)), "Invalid from time format: %v", err)
	}
	Logger.Debugf(logging.Event(logging.EventConfigApplied,
		logging.Str("airgap.config.setting", "from"),
		logging.Str("airgap.config.value", fromTime.Format(time.RFC3339)),
	), "From time is set to: %s", fromTime.Format(time.RFC3339))
	// If no config.to is set, use now:
	toTime := time.Now()
	if config.to != "" {
		toTime, err = time.Parse(time.RFC3339, config.to)
		if err != nil {
			Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
				logging.Str("airgap.config.setting", "to"),
				logging.Str("airgap.config.value", config.to),
			).Merge(logging.Err(err)), "Invalid to time format: %v", err)
		}
		if toTime.Before(fromTime) {
			Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.config.setting", "to"),
				logging.Str("airgap.config.value", config.to),
			), "The 'to' time must be after the 'from' time.")
		}
		Logger.Debugf(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "to"),
			logging.Str("airgap.config.value", toTime.Format(time.RFC3339)),
		), "To time is set to: %s", toTime.Format(time.RFC3339))
	}

	// Send startup status message
	Logger.Debug(logging.Event(logging.EventAppStart),
		"Sending startup status message")
	messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
		fmt.Appendf(nil, "%s %s starting up with resendFileName: %s", protocol.GetTimestamp(), config.id, config.resendFileName), config.payloadSize)
	if len(messages) > 0 {
		if err := udpClient.SendMessage(messages[0]); err != nil {
			Logger.Errorf(logging.Event(logging.EventMessageDeliver).Merge(logging.Err(err)),
				"Failed sending startup message: %v", err)
		}
	}

	var jsonFilter JsonFilter

	// Read filter file and enrich with latest offsets
	jsonFilter, err = parseFile(config.resendFileName, config)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventResendFileRead,
			logging.Str("file.path", config.resendFileName),
		).Merge(logging.Err(err)), "Failed parsing filter file: %v", err)
		return
	}

	// TLS for kafka if needed
	if config.certFile != "" || config.keyFile != "" || config.caFile != "" {
		Logger.Info(logging.Event(logging.EventTLSHandshake,
			logging.Str("server.address", config.bootstrapServers),
			logging.Str("file.path", config.certFile),
		), "Using TLS for Kafka")
		err := kafkaClient.SetTLS(config.certFile, config.keyFile, config.caFile, config.keyPasswordFile)
		if err != nil {
			Logger.Panicf(logging.Event(logging.EventTLSHandshake,
				logging.Str("server.address", config.bootstrapServers),
				logging.Str("file.path", config.certFile),
			).Merge(logging.Err(err)), "Failed to configure TLS: %v", err)
		}
	}

	jsonFilter = filterJsonFilterByPartitionRange(jsonFilter, config.partitionStartValue, config.partitionStopValue)
	if len(jsonFilter.Results) == 0 {
		Logger.Warnf(logging.Event(logging.EventResendRequest,
			logging.Int("airgap.partition.start", int64(config.partitionStartValue)),
			logging.Int("airgap.partition.stop", int64(config.partitionStopValue)),
		), "No resend bundle partitions match configured range start=%d stop=%d",
			config.partitionStartValue, config.partitionStopValue)
	}
	jsonFilter, err = getLatestOffsets(kafkaClient, jsonFilter, config.bootstrapServers, config.partitionStartValue)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventResendRequest,
			logging.Int("airgap.partition.start", int64(config.partitionStartValue)),
		).Merge(logging.Err(err)), "Failed applying partitionStartValue mapping for resend: %v", err)
	}
	Logger.Tracef(logging.Event(logging.EventResendFileRead,
		logging.Str("file.path", config.resendFileName),
		logging.Int("airgap.resend.result.count", int64(len(jsonFilter.Results))),
	), "Resend filter content length: %d", len(jsonFilter.Results))

	// Encryption setup (unchanged)
	if config.encryption {
		Logger.Infof(logging.Event(logging.EventCryptoKeyLoad,
			logging.Str("file.path", config.publicKeyFile),
		), "Creating initial key with public key file: %s", config.publicKeyFile)
		if adapter, ok := udpClient.(*UDPAdapter); ok {
			sendNewKey(adapter.conn, &config)
		} else {
			Logger.Error(logging.Event(logging.EventCryptoKeyExchange,
				logging.Failure(),
				logging.Str("error.type", "unexpected.adapter.type"),
			), "udpClient is not of type *UDPAdapter")
		}
	} else {
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "encryption"),
			logging.Str("airgap.config.value", "false"),
		), "No encryption will be used.")
	}
	// Context that will be cancelled when all partitions are done
	// ctx, cancel := context.WithCancel(context.Background())
	// defer cancel()
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
			Logger.Info(logging.Event(logging.EventResendReplay,
				logging.Int("airgap.resend.result.count", int64(len(jsonFilter.Results))),
			), "All partitions reached their last offsets — cancelling Kafka readers.")
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
		var upstreamPartition int
		var offset int64
		if len(parts) == 3 {
			topic = parts[0]
			p, err := strconv.Atoi(parts[1])
			if err == nil {
				upstreamPartition = p
			}
			if off, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				offset = off
			}
		}
		downstreamPartition := mapUpstreamToDownstreamPartition(upstreamPartition, config.partitionStartValue)
		sendID := id
		if len(parts) == 3 {
			parts[1] = strconv.Itoa(downstreamPartition)
			sendID = strings.Join(parts, "_")
		}
		Logger.Tracef(logging.Event(logging.EventKafkaConsume,
			logging.Str("airgap.message.id", id),
			logging.Str("messaging.destination.name", topic),
			logging.Decimal("messaging.destination.partition.id", int64(upstreamPartition)),
			logging.Int("airgap.partition.downstream", int64(downstreamPartition)),
			logging.Int("messaging.kafka.offset", offset),
		), "kafkaHandler called for id=%s, topic=%s, upstreamPartition=%d, downstreamPartition=%d, offset=%d, time=%s", id, topic, upstreamPartition, downstreamPartition, offset, t.Format(time.RFC3339))

		// Discard old messages, before "from"
		if t.Before(fromTime) {
			Logger.Tracef(logging.Event(logging.EventMessageFilter,
				logging.Str("airgap.message.id", id),
				logging.Str("airgap.filter.reason", "before.from"),
			), "Discarding old message: %s time %s before from %s", id, t.Format(time.RFC3339), fromTime.Format(time.RFC3339))
			return keepRunning
		}

		// Discard messages after "to"
		if t.After(toTime) {
			Logger.Tracef(logging.Event(logging.EventMessageFilter,
				logging.Str("airgap.message.id", id),
				logging.Str("airgap.filter.reason", "after.to"),
			), "Discarding message: %s time %s after to %s", id, t.Format(time.RFC3339), toTime.Format(time.RFC3339))
			markFinished(downstreamPartition)
			// return false to indicate consumer can stop this claim (adapter may honor it)
			return false
		}

		// Filtering: skip messages that are not in the resend filter
		shouldSend := filter(jsonFilter, topic, downstreamPartition, offset)
		if !shouldSend {
			// If message is not to be sent but it's equal-or-after lastOffset, we still consider partition finished.
			if isLast(jsonFilter, topic, downstreamPartition, offset) {
				markFinished(downstreamPartition)
				// returning false signals the consumer to stop processing this message stream if the adapter honors it.
				return false
			}
			// keep running
			return keepRunning
		}

		// If this is the last message that we will send for this partition, mark finished after sending.
		last := isLast(jsonFilter, topic, downstreamPartition, offset)

		// Input filtering based on payload content: clear payload instead of dropping,
		// so the downstream sequence ID is still filled and no gap is reported.
		if config.inputFilter != nil {
			shouldFilter, err := config.inputFilter.ShouldFilterOut(received)
			if err != nil {
				Logger.Errorf(logging.Event(logging.EventMessageFilter,
					logging.Str("airgap.message.id", sendID),
				).Merge(logging.Err(err)), "Error applying input filter: %v", err)
				if strings.Contains(err.Error(), "timeout") {
					Logger.Warnf(logging.Event(logging.EventMessageFilter,
						logging.Failure(),
						logging.Str("airgap.message.id", sendID),
						logging.Str("error.type", "timeout"),
					), "Input filter timeout for id=%s", sendID)
				}
				// On error, fail open (don't filter)
			} else if shouldFilter {
				Logger.Debugf(logging.Event(logging.EventMessageFilter,
					logging.Str("airgap.message.id", sendID),
					logging.Str("airgap.filter.action", "clear.payload"),
				), "Input filter cleared payload for message: %s (sending with empty payload to preserve sequence)", sendID)
				received = []byte{}
			}
		}

		// Compress or not
		var isCompressed = false
		if config.compressWhenLengthExceeds > 0 && len(received) > config.compressWhenLengthExceeds {
			compressed, err := protocol.CompressGzip(received)
			if err != nil {
				Logger.Errorf(logging.Event(logging.EventProtocolEncode,
					logging.Str("airgap.message.id", sendID),
					logging.Str("airgap.compression.algorithm", "gzip"),
				).Merge(logging.Err(err)), "Error compressing data: %s", err)
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
			messages = protocol.FormatMessage(messageType, sendID, ciphertext, config.payloadSize)
		} else {
			if isCompressed {
				messageType = protocol.Merge(protocol.TYPE_CLEARTEXT, protocol.TYPE_COMPRESSED_GZIP)
			} else {
				messageType = protocol.Merge(protocol.TYPE_CLEARTEXT, 0)
			}
			messages = protocol.FormatMessage(messageType, sendID, received, config.payloadSize)
		}
		if err != nil {
			Logger.Error(logging.Event(logging.EventCryptoEncrypt,
				logging.Str("airgap.message.id", sendID),
			).Merge(logging.Err(err)), err)
		}

		// EPS limiter: if config.eps > 0, limit sending rate
		if config.eps > 0 {
			limiterMu.Lock()
			now := time.Now().UnixNano()
			minInterval := int64(float64(time.Second) / config.eps)
			if lastSendTime > 0 {
				nextAllowed := lastSendTime + minInterval
				if now < nextAllowed {
					timer := time.NewTimer(time.Duration(nextAllowed - now))
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						limiterMu.Unlock()
						return false
					}
					now = nextAllowed
				}
			}
			lastSendTime = now
			limiterMu.Unlock()
		}
		if uerr := udpClient.SendMessages(messages); uerr != nil {
			Logger.Errorf(logging.Event(logging.EventMessageDeliver,
				logging.Str("airgap.message.id", sendID),
				logging.Int("airgap.message.count", int64(len(messages))),
			).Merge(logging.Err(uerr)), "UDP send error: %v", uerr)
		} else {
			atomic.AddInt64(&sentEvents, int64(len(messages)))
			atomic.AddInt64(&totalSent, int64(len(messages)))
		}

		// If this was last, mark it finished and possibly cancel
		if last {
			markFinished(downstreamPartition)
			// return false to indicate consumer can stop this claim (adapter may honor it)
			return false
		}

		return keepRunning
	}

	Logger.Debugf(logging.Event(logging.EventResendFileRead,
		logging.Str("file.path", config.resendFileName),
		logging.Int("airgap.resend.result.count", int64(len(jsonFilter.Results))),
	), "Got %d results from resend filter", len(jsonFilter.Results))
	Logger.Debugf(logging.Event(logging.EventResendFileRead,
		logging.Str("file.path", config.resendFileName),
		logging.Str("airgap.resend.filter.type", jsonFilter.Type),
	), "Resend filter content: %+v", jsonFilter)

	// Start one kafka reader goroutine per GapResult (partition)
	for _, r := range jsonFilter.Results {
		wg.Add(1)
		go func(res GapResult) {
			defer wg.Done()
			upstreamPartition, mapErr := mapDownstreamToUpstreamPartition(res.Partition, config.partitionStartValue)
			if mapErr != nil {
				Logger.Errorf(logging.Event(logging.EventResendReplay,
					logging.Str("messaging.destination.name", res.Topic),
					logging.Int("airgap.partition.downstream", int64(res.Partition)),
				).Merge(logging.Err(mapErr)), "Invalid partition mapping for topic %s downstream partition %d: %v", res.Topic, res.Partition, mapErr)
				return
			}
			// name per partition + timestamp
			timestamp := time.Now().Format("20060102150405")
			name := fmt.Sprintf("%s-%d-%s", config.groupID, res.Partition, timestamp)

			// The adapter's Read must accept ctx and return eventually (ReadFromKafkaWithContext does).
			// We call Read with "from" as configured earlier; adapters should internally honor ctx.
			Logger.Debugf(logging.Event(logging.EventKafkaConsume,
				logging.Str("server.address", config.bootstrapServers),
				logging.Str("messaging.destination.name", res.Topic),
				logging.Decimal("messaging.destination.partition.id", int64(upstreamPartition)),
				logging.Int("airgap.partition.downstream", int64(res.Partition)),
				logging.Str("messaging.consumer.group.name", name),
			), "Starting Kafka read for downstream partition %d (upstream partition %d) with name %s with bootstrapServers: %s", res.Partition, upstreamPartition, name, config.bootstrapServers)
			fromOffset := getStartOffsetForResult(jsonFilter.Type, res)
			Logger.Debugf(logging.Event(logging.EventKafkaOffset,
				logging.Str("messaging.destination.name", res.Topic),
				logging.Int("airgap.partition.downstream", int64(res.Partition)),
				logging.Int("messaging.kafka.offset", fromOffset),
			), "Using start offset %d for downstream partition %d", fromOffset, res.Partition)
			kafkaClient.ReadToEndPartition(upstreamPartition, ctx, config.bootstrapServers, res.Topic, name, fromOffset,
				kafkaHandler)
			// // If we exit the read loop without having marked finished, do it now.
			// if _, ok := finished.Load(res.Partition); !ok {
			// 	markFinished(res.Partition)
			// }
			// When Read returns, it means the consumer was stopped (ctx cancelled or adapter finished).
			Logger.Debugf(logging.Event(logging.EventKafkaConsume,
				logging.Str("messaging.destination.name", res.Topic),
				logging.Decimal("messaging.destination.partition.id", int64(upstreamPartition)),
				logging.Int("airgap.partition.downstream", int64(res.Partition)),
			), "Kafka reader for downstream partition %d (upstream partition %d) returned", res.Partition, upstreamPartition)
		}(r)
	}

	// Wait for all readers to finish (cancel() will cause them to return)
	wg.Wait()
	Logger.Info(logging.Event(logging.EventResendReplay,
		logging.Int("airgap.message.count", atomic.LoadInt64(&totalSent)),
	), "RunResend completed: all kafka readers finished")

	// Signal completion
	if done != nil {
		close(done)
	}
}

func Main(build string) {
	mainWithKafka(build, NewKafkaAdapter())
}

func mainWithKafka(build string, kafkaAdapter KafkaClient) {
	BuildNumber = build
	Logger.Infof(logging.Event(logging.EventAppStart),
		"Resend version: %s starting up...", version.GitVersion)
	Logger.Infof(logging.Event(logging.EventAppStart,
		logging.Str("airgap.build.number", BuildNumber),
	), "Build number: %s", BuildNumber)

	var fileName string
	var overrideArgs []string
	// Parse command line: first non-dashed argument is fileName, rest are overrides
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--") {
			overrideArgs = append(overrideArgs, arg)
		} else if fileName == "" {
			fileName = arg
		} else {
			Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.config.source", "command-line"),
				logging.Str("airgap.config.value", arg),
			), "Too many non-dashed command line parameters. Only one property file allowed.")
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
		Logger.Fatalf(logging.Event(logging.EventConfigLoad,
			logging.Str("file.path", fileName),
		).Merge(logging.Err(err)), "Error reading configuration file %s: %v", fileName, err)
	}

	// Set the log file name
	if config.logFileName != "" {
		Logger.Info(logging.Event(logging.EventConfigApplied,
			logging.Str("airgap.config.setting", "logFileName"),
			logging.Str("file.path", config.logFileName),
		), "Configuring log to: "+config.logFileName)
		Logger.Infof(logging.Event(logging.EventAppStart),
			"resend version: %s", version.GitVersion)
		Logger.Info(logging.Event(logging.EventAppStart,
			logging.Str("file.path", config.logFileName),
		), "Log to file started up")
	}

	// Now log the complete configuration to stdout or file
	logConfiguration(config)

	// Stats logger
	statsContext, stopStats := context.WithCancel(context.Background())
	var statsWorkers sync.WaitGroup
	defer func() {
		stopStats()
		statsWorkers.Wait()
		config.WarnProductionConfiguration("shutdown")
	}()
	if config.logStatistics > 0 {
		statsWorkers.Add(1)
		go func() {
			defer statsWorkers.Done()
			interval := time.Duration(config.logStatistics) * time.Second
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-statsContext.Done():
					return
				case <-ticker.C:
				}
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
					Logger.Info(logging.Event(logging.EventStatsReport,
						logging.Int("airgap.message.count", sent),
						logging.Int("airgap.message.received", recv),
						logging.Int("airgap.message.total.sent", totalSnt),
						logging.Int("airgap.message.total.received", totalRecv),
					), "STATISTICS: "+string(b))
				}
			}
		}()
	}

	// MTU
	if config.payloadSize == 0 {
		mtuValue, err := mtu.GetMTU(config.nic, fmt.Sprintf("%s:%d", config.targetIP, config.targetPort))
		if err != nil {
			Logger.Fatal(logging.Event(logging.EventConfigInvalid,
				logging.Str("airgap.config.setting", "payloadSize"),
				logging.Str("airgap.network.interface", config.nic),
			).Merge(logging.Err(err)), err)
		}
		config.payloadSize = uint16(mtuValue) - protocol.HEADER_SIZE
	}
	Logger.Infof(logging.Event(logging.EventConfigApplied,
		logging.Str("airgap.config.setting", "payloadSize"),
		logging.Int("airgap.config.value", int64(config.payloadSize)),
	), "payloadSize read from MTU: %d\n", config.payloadSize)

	// Signals
	hup := make(chan os.Signal, 1)
	sigterm := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	// Create UDP
	address := fmt.Sprintf("%s:%d", config.targetIP, config.targetPort)
	udpAdapter, err := NewUDPAdapter(address, config.nic)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventUDPBind,
			logging.Str("server.address", address),
			logging.Str("airgap.network.interface", config.nic),
		).Merge(logging.Err(err)), "Error creating UDP: %v", err)
	}

	// Choose Kafka adapter
	done := make(chan struct{})

	// Run upstream
	runContext, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	go runResend(runContext, kafkaAdapter, udpAdapter, config, done)

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
				Logger.Info(logging.Event(logging.EventStatsReport,
					logging.Int("airgap.message.count", sent),
					logging.Int("airgap.message.received", recv),
					logging.Int("airgap.message.total.sent", atomic.LoadInt64(&totalSent)),
					logging.Int("airgap.message.total.received", atomic.LoadInt64(&totalReceived)),
				), "STATISTICS: "+string(b))
			}
			Logger.Info(logging.Event(logging.EventAppStop), "Resend process finished. Exiting...")
			messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
				fmt.Appendf(nil, "%s %s finished", protocol.GetTimestamp(), config.id), config.payloadSize)
			udpAdapter.SendMessage(messages[0])
			udpAdapter.Close()
			return
		case <-sigterm:
			Logger.Info(logging.Event(logging.EventAppShutdown,
				logging.Str("airgap.shutdown.reason", "sigterm"),
			), "Received SIGTERM, shutting down")
			cancelRun()
			<-done
			messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS",
				fmt.Appendf(nil, "%s %s shutting down", protocol.GetTimestamp(), config.id), config.payloadSize)
			udpAdapter.SendMessage(messages[0])
			udpAdapter.Close()
			return
		case <-hup:
			Logger.Info(logging.Event(logging.EventConfigApplied,
				logging.Str("airgap.config.setting", "logFileName"),
				logging.Str("file.path", config.logFileName),
			), "SIGHUP received: reopening logs")
			if config.logFileName != "" {
				if err := Logger.SetLogFile(config.logFileName); err != nil {
					Logger.Errorf(logging.Event(logging.EventConfigApplied,
						logging.Str("airgap.config.setting", "logFileName"),
						logging.Str("file.path", config.logFileName),
					).Merge(logging.Err(err)), "Failed reopening log file: %v", err)
				}
			}
		}
	}
}
