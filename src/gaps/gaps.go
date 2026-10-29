package gaps

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// relGaps contains relative gap ranges: each entry is [from] or [from, to].
	relGaps [][]int64
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
		relGaps:   bitmapToRanges(rb),
	}, nil
}

// bitmapToRanges converts a bitmap of relative positions to compact [from] / [from, to] ranges.
func bitmapToRanges(rb *roaring.Bitmap) [][]int64 {
	var ranges [][]int64
	it := rb.Iterator()
	rangeStart, prev := int64(-1), int64(-1)
	for it.HasNext() {
		v := int64(it.Next())
		if rangeStart < 0 {
			rangeStart, prev = v, v
		} else if v == prev+1 {
			prev = v
		} else {
			if rangeStart == prev {
				ranges = append(ranges, []int64{rangeStart})
			} else {
				ranges = append(ranges, []int64{rangeStart, prev})
			}
			rangeStart, prev = v, v
		}
	}
	if rangeStart >= 0 {
		if rangeStart == prev {
			ranges = append(ranges, []int64{rangeStart})
		} else {
			ranges = append(ranges, []int64{rangeStart, prev})
		}
	}
	return ranges
}

// formatRange renders a single relative gap as an absolute offset or range string.
func formatRange(gap []int64, windowMin int64) string {
	if len(gap) == 1 {
		return fmt.Sprintf("%d", windowMin+gap[0])
	}
	return fmt.Sprintf("%d-%d", windowMin+gap[0], windowMin+gap[1])
}

// countMissing returns the total number of missing offsets in the relative gap list.
func countMissing(relGaps [][]int64) int64 {
	var n int64
	for _, g := range relGaps {
		if len(g) == 1 {
			n++
		} else if len(g) == 2 {
			n += g[1] - g[0] + 1
		}
	}
	return n
}

// printShort prints a one-line summary per window entry followed by a per-topic total.
// Format: topic/partition [windowMin..windowMax]: N missing  first: a, b-c, d
func printShort(windows []parsedWindow) {
	const previewCount = 5
	var topicTotal int64
	var currentTopic string
	for _, pw := range windows {
		if pw.topic != currentTopic {
			if currentTopic != "" {
				fmt.Printf("%s/* Total: %d missing\n", currentTopic, topicTotal)
			}
			currentTopic = pw.topic
			topicTotal = 0
		}
		missing := countMissing(pw.relGaps)
		topicTotal += missing
		preview := pw.relGaps
		suffix := ""
		if len(preview) > previewCount {
			preview = preview[:previewCount]
			suffix = ", ..."
		}
		parts := make([]string, 0, len(preview))
		for _, g := range preview {
			parts = append(parts, formatRange(g, pw.windowMin))
		}
		gapStr := strings.Join(parts, ", ") + suffix
		if missing > 0 {
			fmt.Printf("%s/%d [%d..%d]: %d missing  first: %s\n",
				pw.topic, pw.partition, pw.windowMin, pw.windowMax, missing, gapStr)
		}
	}
	if currentTopic != "" {
		fmt.Printf("%s/* Total: %d missing\n", currentTopic, topicTotal)
	}
}

// printFull prints full details for each window entry followed by a per-topic total.
func printFull(windows []parsedWindow) {
	var topicTotal int64
	var currentTopic string
	for _, pw := range windows {
		if pw.topic != currentTopic {
			if currentTopic != "" {
				fmt.Printf("%s/* Total: %d missing\n", currentTopic, topicTotal)
			}
			currentTopic = pw.topic
			topicTotal = 0
		}
		topicTotal += countMissing(pw.relGaps)
		if len(pw.relGaps) > 0 {
			fmt.Printf("Topic: %s  Partition: %d  Window: [%d..%d]\n",
				pw.topic, pw.partition, pw.windowMin, pw.windowMax)
			fmt.Print("  Missing offsets:")
			for j, g := range pw.relGaps {
				if j > 0 {
					fmt.Print(",")
				}
				fmt.Printf(" %s", formatRange(g, pw.windowMin))
			}
			fmt.Println()
		}
	}
	if currentTopic != "" {
		fmt.Printf("%s/* Total: %d missing\n", currentTopic, topicTotal)
	}
}

// sortWindows returns a sorted slice of parsedWindow values from the map.
func sortWindows(lastEntries map[string]parsedWindow) []parsedWindow {
	windows := make([]parsedWindow, 0, len(lastEntries))
	for _, pw := range lastEntries {
		windows = append(windows, pw)
	}
	sort.Slice(windows, func(i, j int) bool {
		a, b := windows[i], windows[j]
		if a.topic != b.topic {
			return a.topic < b.topic
		}
		if a.partition != b.partition {
			return a.partition < b.partition
		}
		return a.windowMin < b.windowMin
	})
	return windows
}

// Main is the entry point for the gaps display tool.
func Main(BuildNumber string) {
	Logger.Infof(logging.Event(logging.EventAppStart),
		"Gaps version: %s starting up...", version.GitVersion)
	Logger.Infof(logging.Event(logging.EventAppStart,
		logging.Str("airgap.build.number", BuildNumber),
	), "Build number: %s", BuildNumber)

	fileName := ""
	var overrideArgs []string
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

	configuration := defaultConfiguration()
	var err error
	configuration, err = readParameters(fileName, configuration)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventConfigLoad,
			logging.Str("file.path", fileName),
		).Merge(logging.Err(err)), "Error reading configuration file %s: %v", fileName, err)
	}
	configuration = overrideConfiguration(configuration)
	configuration = parseCommandLineOverrides(overrideArgs, configuration)
	configuration = checkConfiguration(configuration)
	config = configuration

	if config.logFileName != "" {
		if err := Logger.SetLogFile(config.logFileName); err != nil {
			Logger.Fatal(logging.Event(logging.EventConfigInvalid,
				logging.Str("airgap.config.setting", "logFileName"),
				logging.Str("file.path", config.logFileName),
			).Merge(logging.Err(err)), err)
		}
	}
	Logger.SetLogLevel(config.logLevel)
	logConfiguration(config)

	if config.certFile != "" || config.keyFile != "" || config.caFile != "" {
		Logger.Info(logging.Event(logging.EventTLSHandshake,
			logging.Str("server.address", config.bootstrapServers),
			logging.Str("file.path", config.certFile),
		), "Using TLS for Kafka")
		if _, err := kafka.SetTLSConfigParameters(config.certFile, config.keyFile, config.caFile, config.keyPasswordFile); err != nil {
			Logger.Panicf(logging.Event(logging.EventTLSHandshake,
				logging.Str("server.address", config.bootstrapServers),
				logging.Str("file.path", config.certFile),
			).Merge(logging.Err(err)), "Failed to configure TLS: %v", err)
		}
	}

	// Shared state (updated from multiple goroutines after bootstrap).
	var mu sync.Mutex
	state := newGapState()

	// Bootstrap: read all existing messages and print initial state.
	Logger.Infof(logging.Event(logging.EventKafkaConsume,
		logging.Str("server.address", config.bootstrapServers),
		logging.Str("messaging.destination.name", config.topic),
		logging.Str("messaging.consumer.group.name", config.groupID),
	), "Reading from Kafka %s topic %s", config.bootstrapServers, config.topic)
	kafkaReader := NewKafkaReader()
	bootstrapHandler := func(id string, key []byte, t time.Time, received []byte) bool {
		atomic.AddInt64(&receivedEvents, 1)
		if err := state.apply(key, received); err != nil {
			Logger.Debugf(logging.Event(logging.EventGapReport,
				logging.Str("airgap.message.id", id),
				logging.Str("messaging.destination.name", config.topic),
				logging.Str("airgap.gap.phase", "bootstrap"),
			).Merge(logging.Err(err)), "Failed to parse gap message: %v", err)
			return true
		}
		return true
	}
	if err := kafkaReader.ReadToEnd(context.Background(), config.bootstrapServers, config.topic, config.groupID, bootstrapHandler); err != nil {
		Logger.Fatalf(logging.Event(logging.EventKafkaConsume,
			logging.Str("server.address", config.bootstrapServers),
			logging.Str("messaging.destination.name", config.topic),
			logging.Str("messaging.consumer.group.name", config.groupID),
		).Merge(logging.Err(err)), "Error reading gap topic: %v", err)
	}
	Logger.Infof(logging.Event(logging.EventGapReport,
		logging.Str("messaging.destination.name", config.topic),
		logging.Int("airgap.message.count", receivedEvents),
		logging.Int("airgap.gap.window.count", int64(len(state.windows()))),
		logging.Int("airgap.gap.tombstone.count", state.stats.tombstones),
	), "Read %d gap messages (%d tombstones).", receivedEvents, state.stats.tombstones)
	parseStats := state.stats
	if parseStats.legacy > 0 {
		superseded := state.superseded()
		Logger.Infof(logging.Event(logging.EventGapReport,
			logging.Str("messaging.destination.name", config.topic),
			logging.Int("airgap.gap.legacy.count", parseStats.legacy),
			logging.Int("airgap.gap.legacy.superseded", superseded),
		), "Read %d gap records in the legacy JSON format (dedup 0.1.10 and earlier); %d were superseded by binary records or tombstones for the same window, the rest are included",
			parseStats.legacy, superseded)
	}
	if parseStats.failed > 0 {
		Logger.Errorf(logging.Event(logging.EventGapReport,
			logging.Str("messaging.destination.name", config.topic),
			logging.Int("airgap.gap.skipped.count", parseStats.failed),
		).Merge(logging.Err(parseStats.firstErr)), "Skipped %d gap records that could not be parsed (enable debug logging for details); first error: %v",
			parseStats.failed, parseStats.firstErr)
	}
	if config.mode == "full" {
		printFull(sortWindows(state.windows()))
	} else {
		printShort(sortWindows(state.windows()))
	}

	// Set up signal handling and a cancelable context for the tail phase.
	ctx, cancel := context.WithCancel(context.Background())
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigterm
		cancel()
	}()

	// Reprint trigger: non-blocking send so fast bursts collapse into one redraw.
	reprint := make(chan struct{}, 1)

	tailHandler := func(id string, key []byte, t time.Time, received []byte) bool {
		mu.Lock()
		err := state.apply(key, received)
		mu.Unlock()
		if err != nil {
			Logger.Errorf(logging.Event(logging.EventGapReport,
				logging.Str("airgap.message.id", id),
				logging.Str("messaging.destination.name", config.topic),
				logging.Str("airgap.gap.phase", "tail"),
			).Merge(logging.Err(err)), "Failed to parse gap message: %v", err)
			return true
		}
		atomic.AddInt64(&receivedEvents, 1)
		select {
		case reprint <- struct{}{}:
		default:
		}
		return true
	}

	// Start tailing in the background.
	go func() {
		if err := kafkaReader.Tail(ctx, config.bootstrapServers, config.topic, tailHandler); err != nil && err != context.Canceled {
			Logger.Errorf(logging.Event(logging.EventKafkaConsume,
				logging.Str("server.address", config.bootstrapServers),
				logging.Str("messaging.destination.name", config.topic),
			).Merge(logging.Err(err)), "Tail error: %v", err)
		}
	}()

	// Display loop: reprint whenever a new message arrives, stop on signal.
	for {
		select {
		case <-ctx.Done():
			return
		case <-reprint:
			mu.Lock()
			windows := sortWindows(state.windows())
			mu.Unlock()
			fmt.Printf("\n--- %s ---\n", time.Now().Format("2006-01-02 15:04:05"))
			if config.mode == "full" {
				printFull(windows)
			} else {
				printShort(windows)
			}
		}
	}
}
