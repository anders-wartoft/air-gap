package create

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RoaringBitmap/roaring"
	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/version"
)

// parsedWindow holds a decoded gap-topic binary message.
type parsedWindow struct {
	topic     string
	partition int
	windowMin int64
	windowMax int64
	// Legacy JSON uses ranges; binary records retain their compressed bitmap.
	relGaps [][]int64
	missing *roaring.Bitmap
	// legacy is true when the window was decoded from the JSON format written by
	// dedup 0.1.10 and earlier (key "topic_partition:windowMin").
	legacy bool
}

// legacyGapPayload is the JSON gap-topic value written by dedup 0.1.10 and earlier.
type legacyGapPayload struct {
	Topic     *string   `json:"topic"`
	Partition *int      `json:"partition"`
	WindowMin *int64    `json:"window_min"`
	WindowMax *int64    `json:"window_max"`
	Gaps      [][]int64 `json:"gaps"`
}

// gapParseStats aggregates per-record outcomes so a run logs one summary line
// instead of one line per record.
type gapParseStats struct {
	legacy     int64
	tombstones int64
	failed     int64
	firstErr   error
}

// gapState holds the latest record per gap-topic key. Binary and legacy keys
// for the same window are tracked separately so the result does not depend on
// read order across gap-topic partitions. A nil entry is a tombstone.
type gapState struct {
	binary map[string]*parsedWindow
	legacy map[string]*parsedWindow
	stats  gapParseStats
}

func newGapState() *gapState {
	return &gapState{
		binary: make(map[string]*parsedWindow),
		legacy: make(map[string]*parsedWindow),
	}
}

// windowKey identifies a gap window independently of the record format.
func windowKey(topic string, partition int, windowMin int64) string {
	return fmt.Sprintf("%s-%d-%d", topic, partition, windowMin)
}

// parseGapKey extracts the window identity from a gap-topic key. Kafka topic
// names cannot contain ':', so the current key "topic:partition:windowMin" has
// two colons and the legacy key "topic_partition:windowMin" has one.
func parseGapKey(key []byte) (topic string, partition int, windowMin int64, legacy bool, err error) {
	k := string(key)
	var partitionStr, windowStr string
	switch strings.Count(k, ":") {
	case 2:
		parts := strings.SplitN(k, ":", 3)
		topic, partitionStr, windowStr = parts[0], parts[1], parts[2]
	case 1:
		legacy = true
		colon := strings.LastIndex(k, ":")
		underscore := strings.LastIndex(k[:colon], "_")
		if underscore <= 0 {
			return "", 0, 0, false, fmt.Errorf("invalid legacy gap key %q: expected topic_partition:windowMin", k)
		}
		topic, partitionStr, windowStr = k[:underscore], k[underscore+1:colon], k[colon+1:]
	default:
		return "", 0, 0, false, fmt.Errorf("invalid gap key %q: expected topic:partition:windowMin", k)
	}
	if partition, err = strconv.Atoi(partitionStr); err != nil {
		return "", 0, 0, false, fmt.Errorf("invalid partition in gap key %q: %w", k, err)
	}
	if windowMin, err = strconv.ParseInt(windowStr, 10, 64); err != nil {
		return "", 0, 0, false, fmt.Errorf("invalid windowMin in gap key %q: %w", k, err)
	}
	return topic, partition, windowMin, legacy, nil
}

// apply records one gap-topic message. An empty value is a tombstone: dedup
// writes one when a window leaves its detection range.
func (s *gapState) apply(key, value []byte) error {
	if len(value) == 0 {
		topic, partition, windowMin, legacy, err := parseGapKey(key)
		if err != nil {
			s.fail(err)
			return err
		}
		s.stats.tombstones++
		if legacy {
			s.legacy[windowKey(topic, partition, windowMin)] = nil
		} else {
			s.binary[windowKey(topic, partition, windowMin)] = nil
		}
		return nil
	}
	pw, err := parseGapPayload(key, value)
	if err != nil {
		s.fail(err)
		return err
	}
	k := windowKey(pw.topic, pw.partition, pw.windowMin)
	if pw.legacy {
		s.stats.legacy++
		s.legacy[k] = &pw
	} else {
		s.binary[k] = &pw
	}
	return nil
}

func (s *gapState) fail(err error) {
	s.stats.failed++
	if s.stats.firstErr == nil {
		s.stats.firstErr = err
	}
}

// windows returns the live windows. A binary record or tombstone for a window
// takes precedence over a legacy record for the same window.
func (s *gapState) windows() map[string]parsedWindow {
	result := make(map[string]parsedWindow, len(s.binary)+len(s.legacy))
	for k, pw := range s.legacy {
		if _, shadowed := s.binary[k]; !shadowed && pw != nil {
			result[k] = *pw
		}
	}
	for k, pw := range s.binary {
		if pw != nil {
			result[k] = *pw
		}
	}
	return result
}

// superseded counts live legacy records hidden by a binary record or tombstone.
func (s *gapState) superseded() int64 {
	var n int64
	for k, pw := range s.legacy {
		if _, shadowed := s.binary[k]; shadowed && pw != nil {
			n++
		}
	}
	return n
}

// isLegacyJSONPayload reports whether value is a legacy JSON gap record. The
// binary format starts with version byte 1, so a leading '{' is unambiguous.
func isLegacyJSONPayload(value []byte) bool {
	trimmed := bytes.TrimLeft(value, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// parseLegacyGapPayload decodes the JSON gap format written by dedup 0.1.10 and
// earlier. Topic, partition and window are taken from the value, since the
// legacy key "topic_partition:windowMin" is ambiguous for topics containing '_'.
func parseLegacyGapPayload(value []byte) (parsedWindow, error) {
	var p legacyGapPayload
	if err := json.Unmarshal(value, &p); err != nil {
		return parsedWindow{}, fmt.Errorf("invalid legacy JSON gap payload: %w", err)
	}
	if p.Topic == nil || p.Partition == nil || p.WindowMin == nil {
		return parsedWindow{}, fmt.Errorf("legacy JSON gap payload is missing topic, partition or window_min")
	}
	windowMax := *p.WindowMin
	if p.WindowMax != nil {
		windowMax = *p.WindowMax
	}
	for _, gap := range p.Gaps {
		if len(gap) != 1 && len(gap) != 2 {
			return parsedWindow{}, fmt.Errorf("invalid gap range %v in legacy JSON gap payload", gap)
		}
	}
	return parsedWindow{
		topic:     *p.Topic,
		partition: *p.Partition,
		windowMin: *p.WindowMin,
		windowMax: windowMax,
		relGaps:   p.Gaps,
		legacy:    true,
	}, nil
}

// parseGapPayload decodes the binary gap-topic wire format.
//
// Wire format (big-endian):
//
//	[1 byte]  version = 1
//	[8 bytes] windowMin (int64)
//	[8 bytes] windowMax (int64)
//	[N bytes] 32-bit RoaringBitmap of MISSING relative positions (portable format)
//
// Key format: "topic:partition:windowMin"
//
// Records in the legacy JSON format (dedup 0.1.10 and earlier) are also accepted.
func parseGapPayload(key, value []byte) (parsedWindow, error) {
	if isLegacyJSONPayload(value) {
		return parseLegacyGapPayload(value)
	}
	parts := strings.SplitN(string(key), ":", 3)
	if len(parts) != 3 {
		return parsedWindow{}, fmt.Errorf("invalid gap key %q: expected topic:partition:windowMin", string(key))
	}
	topic := parts[0]
	partition, err := strconv.Atoi(parts[1])
	if err != nil {
		return parsedWindow{}, fmt.Errorf("invalid partition in gap key %q: %w", string(key), err)
	}

	const headerLen = 17
	if len(value) < headerLen {
		return parsedWindow{}, fmt.Errorf("gap payload too short: %d bytes", len(value))
	}
	if value[0] != 1 {
		return parsedWindow{}, fmt.Errorf("unsupported gap payload version: %d", value[0])
	}
	windowMin := int64(binary.BigEndian.Uint64(value[1:9]))
	windowMax := int64(binary.BigEndian.Uint64(value[9:17]))

	rb := roaring.New()
	if len(value) > headerLen {
		if _, err := rb.ReadFrom(bytes.NewReader(value[headerLen:])); err != nil {
			return parsedWindow{}, fmt.Errorf("failed to parse gap bitmap: %w", err)
		}
	}

	return parsedWindow{
		topic:     topic,
		partition: partition,
		windowMin: windowMin,
		windowMax: windowMax,
		missing:   rb,
	}, nil
}

// eachGap visits ranges without expanding a fragmented bitmap into millions of slices.
func (pw parsedWindow) eachGap(visit func(from, to int64) bool) {
	if pw.missing == nil {
		for _, gap := range pw.relGaps {
			to := gap[0]
			if len(gap) == 2 {
				to = gap[1]
			}
			if !visit(gap[0], to) {
				return
			}
		}
		return
	}
	it := pw.missing.Iterator()
	rangeStart, prev := int64(-1), int64(-1)
	for it.HasNext() {
		v := int64(it.Next())
		if rangeStart < 0 {
			rangeStart, prev = v, v
		} else if v == prev+1 {
			prev = v
		} else {
			if !visit(rangeStart, prev) {
				return
			}
			rangeStart, prev = v, v
		}
	}
	if rangeStart >= 0 {
		visit(rangeStart, prev)
	}
}

// Main is the entry point of the program.
// It reads a configuration file from the command line parameter,
// initializes the necessary variables, and starts the upstream process.
func Main(BuildNumber string, kafkaReader KafkaReader) {
	Logger.Infof(logging.Event(logging.EventAppStart),
		"CreateResourceBundle version: %s starting up...", version.GitVersion)
	Logger.Infof(logging.Event(logging.EventAppStart,
		logging.Str("airgap.build.number", BuildNumber),
	), "Build number: %s", BuildNumber)
	fileName := ""
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

	hup := make(chan os.Signal, 1)
	sigterm := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	var cancel context.CancelFunc
	var ctx context.Context
	signalHandlerDone := make(chan struct{})
	signalHandlerStopped := make(chan struct{})

	// Set time_start at app start
	timeStart = time.Now().Unix()
	reload := func() {
		// Start with the default parameters
		configuration := defaultConfiguration()
		// Read configuration from file, if added
		var err error
		configuration, err = readParameters(fileName, configuration)
		// May override with environment variables
		configuration = overrideConfiguration(configuration)
		// Apply command line overrides
		configuration = parseCommandLineOverrides(overrideArgs, configuration)
		// Make sure we have everything we need in the config
		configuration = checkConfiguration(configuration)
		if err != nil {
			Logger.Fatalf(logging.Event(logging.EventConfigLoad,
				logging.Str("file.path", fileName),
			).Merge(logging.Err(err)), "Error reading configuration file %s: %v", fileName, err)
		}
		// Add to package variable
		config = configuration

		// Set the log file name
		if config.logFileName != "" {
			Logger.Info(logging.Event(logging.EventConfigApplied,
				logging.Str("airgap.config.setting", "logFileName"),
				logging.Str("file.path", config.logFileName),
			), "Configuring log to: "+config.logFileName)
			Logger.Infof(logging.Event(logging.EventAppStart),
				"CreateResourceBundle version: %s", version.GitVersion)
			Logger.Info(logging.Event(logging.EventAppStart,
				logging.Str("file.path", config.logFileName),
			), "Log to file started up")
		}

		// Now log the complete configuration to stdout or file
		logConfiguration(config)

		// Cancel previous threads if any
		if cancel != nil {
			cancel()
			// Give threads a moment to shut down
			time.Sleep(200 * time.Millisecond)
		}
		ctx, cancel = context.WithCancel(context.Background())
		go func() {
			defer close(signalHandlerStopped)
			select {
			case <-sigterm:
				Logger.Info(logging.Event(logging.EventAppShutdown,
					logging.Str("airgap.shutdown.reason", "sigterm"),
				), "Received termination signal, stopping create")
				cancel()
			case <-signalHandlerDone:
			}
		}()

		// Latest record (or tombstone) per gap-topic key.
		state := newGapState()

		// Kafka message handler: decode gap payload or tombstone, keep latest per key.
		kafkaHandler := func(id string, key []byte, t time.Time, received []byte) bool {
			atomic.AddInt64(&receivedEvents, 1)
			atomic.AddInt64(&totalReceived, 1)
			if err := state.apply(key, received); err != nil {
				Logger.Debugf(logging.Event(logging.EventCreateScan,
					logging.Str("airgap.message.id", id),
					logging.Str("messaging.destination.name", config.topic),
				).Merge(logging.Err(err)), "Failed to parse gap message: %v", err)
				return keepRunning
			}
			Logger.Debugf(logging.Event(logging.EventCreateScan,
				logging.Str("messaging.destination.name", config.topic),
				logging.Str("airgap.message.id", id),
				logging.Bool("airgap.gap.tombstone", len(received) == 0),
			), "Read gap record key=%s tombstone=%t", key, len(received) == 0)
			return keepRunning
		}

		Logger.Infof(logging.Event(logging.EventKafkaConsume,
			logging.Str("server.address", config.bootstrapServers),
			logging.Str("messaging.destination.name", config.topic),
			logging.Str("messaging.consumer.group.name", config.groupID),
		), "Reading from Kafka %s", config.bootstrapServers)
		if configuration.certFile != "" || configuration.keyFile != "" || configuration.caFile != "" {
			Logger.Info(logging.Event(logging.EventTLSHandshake,
				logging.Str("server.address", config.bootstrapServers),
				logging.Str("file.path", configuration.certFile),
			), "Using TLS for Kafka")
			_, err := kafka.SetTLSConfigParameters(configuration.certFile, configuration.keyFile, configuration.caFile, configuration.keyPasswordFile)
			if err != nil {
				Logger.Panicf(logging.Event(logging.EventTLSHandshake,
					logging.Str("server.address", config.bootstrapServers),
					logging.Str("file.path", configuration.certFile),
				).Merge(logging.Err(err)), "Failed to configure TLS: %v", err)
			}
		}

		// Single thread: read from Kafka and process messages, then exit when done
		group := config.groupID
		// Read all available messages, then exit
		err = kafkaReader.ReadToEnd(ctx, config.bootstrapServers, config.topic, group, kafkaHandler)
		if err != nil && ctx.Err() == nil {
			Logger.Fatalf(logging.Event(logging.EventKafkaConsume,
				logging.Str("server.address", config.bootstrapServers),
				logging.Str("messaging.destination.name", config.topic),
				logging.Str("messaging.consumer.group.name", group),
			).Merge(logging.Err(err)), "Error reading to end of topic: %v", err)
		}

		logParseSummary(state, config.topic)
		if err := writeResults(state.windows(), config); err != nil {
			Logger.Fatalf(logging.Event(logging.EventCreateFileWrite,
				logging.Str("file.path", config.resendFileName),
			).Merge(logging.Err(err)), "Failed to write resend bundle: %v", err)
		}

	}

	reload() // initial start
	close(signalHandlerDone)
	<-signalHandlerStopped
	cancel()
	// Exit after reload finishes (batch mode)
	Logger.Info(logging.Event(logging.EventAppStop), "CreateResourceBundle finished, exiting.")
	config.WarnProductionConfiguration("shutdown")
	os.Exit(0)
}

// logParseSummary logs at most one line about legacy records and one about
// unparseable records for a complete read of the gap topic.
func logParseSummary(state *gapState, topic string) {
	stats := state.stats
	Logger.Debugf(logging.Event(logging.EventCreateScan,
		logging.Str("messaging.destination.name", topic),
		logging.Int("airgap.gap.tombstone.count", stats.tombstones),
	), "Read %d gap tombstones", stats.tombstones)
	if stats.legacy > 0 {
		superseded := state.superseded()
		Logger.Infof(logging.Event(logging.EventCreateScan,
			logging.Str("messaging.destination.name", topic),
			logging.Int("airgap.gap.legacy.count", stats.legacy),
			logging.Int("airgap.gap.legacy.superseded", superseded),
		), "Read %d gap records in the legacy JSON format (dedup 0.1.10 and earlier); %d were superseded by binary records or tombstones for the same window, the rest are included",
			stats.legacy, superseded)
	}
	if stats.failed > 0 {
		Logger.Errorf(logging.Event(logging.EventCreateScan,
			logging.Str("messaging.destination.name", topic),
			logging.Int("airgap.gap.skipped.count", stats.failed),
		).Merge(logging.Err(stats.firstErr)), "Skipped %d gap records that could not be parsed (enable debug logging for details); first error: %v",
			stats.failed, stats.firstErr)
	}
}

// writeResults formats and outputs the results as JSON, either to a file or the console.
// Windows for the same topic+partition are merged into a single entry with absolute gap offsets.
func writeResults(lastEntries map[string]parsedWindow, config TransferConfiguration) error {
	if config.resendFileName == "" {
		if err := writeBundle(os.Stdout, lastEntries, config); err != nil {
			return err
		}
		_, err := fmt.Fprintln(os.Stdout)
		return err
	}
	mode := os.FileMode(0644)
	if info, err := os.Stat(config.resendFileName); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(config.resendFileName), ".resend-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		file.Close()
		if err := os.Remove(file.Name()); err != nil && !os.IsNotExist(err) {
			Logger.Errorf(logging.Event(logging.EventCreateFileWrite,
				logging.Str("file.path", file.Name()),
			).Merge(logging.Err(err)), "Failed to remove temporary resend file: %v", err)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := writeBundle(file, lastEntries, config); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), config.resendFileName); err != nil {
		return err
	}
	Logger.Infof(logging.Event(logging.EventCreateFileWrite,
		logging.Str("file.path", config.resendFileName),
	), "Results written to %s", config.resendFileName)
	return nil
}

func writeBundle(output io.Writer, lastEntries map[string]parsedWindow, config TransferConfiguration) error {
	grouped := make(map[string][]parsedWindow)
	var groupOrder []string
	for _, pw := range lastEntries {
		key := fmt.Sprintf("%s-%d", pw.topic, pw.partition)
		if _, exists := grouped[key]; !exists {
			groupOrder = append(groupOrder, key)
		}
		grouped[key] = append(grouped[key], pw)
	}
	writer := bufio.NewWriter(output)
	limit, err := json.Marshal(config.limit)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "{\n  \"type\": %s,\n  \"results\": [\n", limit); err != nil {
		return err
	}
	for i, key := range groupOrder {
		windows := grouped[key]
		sort.Slice(windows, func(i, j int) bool { return windows[i].windowMin < windows[j].windowMin })
		if i > 0 {
			if _, err := writer.WriteString(",\n"); err != nil {
				return err
			}
		}
		if _, err := writer.WriteString("  {\"gaps\":"); err != nil {
			return err
		}
		haveGap := false
		var writeErr error
		var buffer [48]byte
		for _, window := range windows {
			window.eachGap(func(from, to int64) bool {
				b := buffer[:0]
				if haveGap {
					b = append(b, ',')
				} else {
					b = append(b, '[')
				}
				b = append(b, '[')
				b = strconv.AppendInt(b, from+window.windowMin, 10)
				if from != to {
					b = append(b, ',')
					b = strconv.AppendInt(b, to+window.windowMin, 10)
				}
				b = append(b, ']')
				_, writeErr = writer.Write(b)
				haveGap = true
				return writeErr == nil && config.limit != "first"
			})
			if writeErr != nil {
				return writeErr
			}
			if haveGap && config.limit == "first" {
				break
			}
		}
		ending := "null"
		if haveGap {
			ending = "]"
		}
		topic, err := json.Marshal(windows[0].topic)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "%s,\"partition\":%d,\"topic\":%s,\"window_max\":%d,\"window_min\":0}",
			ending, windows[0].partition, topic, windows[len(windows)-1].windowMax); err != nil {
			return err
		}
	}
	if _, err := writer.WriteString("\n]\n}"); err != nil {
		return err
	}
	return writer.Flush()
}
