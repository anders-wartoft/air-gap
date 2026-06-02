package gaps

import (
	"bytes"
	"context"
	"encoding/binary"
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
func parseGapPayload(key, value []byte) (parsedWindow, error) {
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
	Logger.Printf("Gaps version: %s starting up...", version.GitVersion)
	Logger.Printf("Build number: %s", BuildNumber)

	fileName := ""
	var overrideArgs []string
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--") {
			overrideArgs = append(overrideArgs, arg)
		} else if fileName == "" {
			fileName = arg
		} else {
			Logger.Fatalf("Too many non-dashed command line parameters. Only one property file allowed.")
		}
	}

	configuration := defaultConfiguration()
	var err error
	configuration, err = readParameters(fileName, configuration)
	if err != nil {
		Logger.Fatalf("Error reading configuration file %s: %v", fileName, err)
	}
	configuration = overrideConfiguration(configuration)
	configuration = parseCommandLineOverrides(overrideArgs, configuration)
	configuration = checkConfiguration(configuration)
	config = configuration

	if config.logFileName != "" {
		if err := Logger.SetLogFile(config.logFileName); err != nil {
			Logger.Fatal(err)
		}
	}
	Logger.SetLogLevel(config.logLevel)
	logConfiguration(config)

	if config.certFile != "" || config.keyFile != "" || config.caFile != "" {
		Logger.Print("Using TLS for Kafka")
		if _, err := kafka.SetTLSConfigParameters(config.certFile, config.keyFile, config.caFile, config.keyPasswordFile); err != nil {
			Logger.Panicf("Failed to configure TLS: %v", err)
		}
	}

	// Shared state (updated from multiple goroutines after bootstrap).
	var mu sync.Mutex
	lastEntries := make(map[string]parsedWindow)

	// Bootstrap: read all existing messages and print initial state.
	Logger.Printf("Reading from Kafka %s topic %s", config.bootstrapServers, config.topic)
	kafkaReader := NewKafkaReader()
	bootstrapHandler := func(id string, key []byte, t time.Time, received []byte) bool {
		atomic.AddInt64(&receivedEvents, 1)
		pw, err := parseGapPayload(key, received)
		if err != nil {
			Logger.Errorf("Failed to parse gap message: %v", err)
			return true
		}
		keyStr := fmt.Sprintf("%s-%d-%d", pw.topic, pw.partition, pw.windowMin)
		lastEntries[keyStr] = pw
		return true
	}
	if err := kafkaReader.ReadToEnd(context.Background(), config.bootstrapServers, config.topic, config.groupID, bootstrapHandler); err != nil {
		Logger.Fatalf("Error reading gap topic: %v", err)
	}
	Logger.Printf("Read %d gap messages.", receivedEvents)
	if config.mode == "full" {
		printFull(sortWindows(lastEntries))
	} else {
		printShort(sortWindows(lastEntries))
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
		pw, err := parseGapPayload(key, received)
		if err != nil {
			Logger.Errorf("Failed to parse gap message: %v", err)
			return true
		}
		keyStr := fmt.Sprintf("%s-%d-%d", pw.topic, pw.partition, pw.windowMin)
		mu.Lock()
		lastEntries[keyStr] = pw
		mu.Unlock()
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
			Logger.Errorf("Tail error: %v", err)
		}
	}()

	// Display loop: reprint whenever a new message arrives, stop on signal.
	for {
		select {
		case <-ctx.Done():
			return
		case <-reprint:
			mu.Lock()
			windows := sortWindows(lastEntries)
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
