package create

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

// Main is the entry point of the program.
// It reads a configuration file from the command line parameter,
// initializes the necessary variables, and starts the upstream process.
func Main(BuildNumber string, kafkaReader KafkaReader) {
	Logger.Printf("CreateResourceBundle version: %s starting up...", version.GitVersion)
	Logger.Printf("Build number: %s", BuildNumber)
	fileName := ""
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

	hup := make(chan os.Signal, 1)
	sigterm := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	var cancel context.CancelFunc
	var ctx context.Context

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
			Logger.Fatalf("Error reading configuration file %s: %v", fileName, err)
		}
		// Add to package variable
		config = configuration

		// Set the log file name
		if config.logFileName != "" {
			Logger.Print("Configuring log to: " + config.logFileName)
			err := Logger.SetLogFile(config.logFileName)
			if err != nil {
				Logger.Fatal(err)
			}
			Logger.Printf("CreateResourceBundle version: %s", version.GitVersion)
			Logger.Print("Log to file started up")
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

		// Map to store last entry for each "topic-partition-windowMin".
		lastEntries := make(map[string]parsedWindow)

		// Kafka message handler: decode binary gap payload, keep latest per window.
		kafkaHandler := func(id string, key []byte, t time.Time, received []byte) bool {
			atomic.AddInt64(&receivedEvents, 1)
			atomic.AddInt64(&totalReceived, 1)
			pw, err := parseGapPayload(key, received)
			if err != nil {
				Logger.Errorf("Failed to parse gap message: %v", err)
				return keepRunning
			}
			keyStr := fmt.Sprintf("%s-%d-%d", pw.topic, pw.partition, pw.windowMin)
			lastEntries[keyStr] = pw
			return keepRunning
		}

		Logger.Printf("Reading from Kafka %s", config.bootstrapServers)
		if configuration.certFile != "" || configuration.keyFile != "" || configuration.caFile != "" {
			Logger.Print("Using TLS for Kafka")
			_, err := kafka.SetTLSConfigParameters(configuration.certFile, configuration.keyFile, configuration.caFile, configuration.keyPasswordFile)
			if err != nil {
				Logger.Panicf("Failed to configure TLS: %v", err)
			}
		}

		// Single thread: read from Kafka and process messages, then exit when done
		group := config.groupID
		// Read all available messages, then exit
		err = kafkaReader.ReadToEnd(ctx, config.bootstrapServers, config.topic, group, kafkaHandler)
		if err != nil {
			Logger.Fatalf("Error reading to end of topic: %v", err)
		}

		writeResults(lastEntries, config)

	}

	reload() // initial start
	// Exit after reload finishes (batch mode)
	Logger.Print("CreateResourceBundle finished, exiting.")
	os.Exit(0)
}

// writeResults formats and outputs the results as JSON, either to a file or the console.
// Windows for the same topic+partition are merged into a single entry with absolute gap offsets.
func writeResults(lastEntries map[string]parsedWindow, config TransferConfiguration) {
	type windowEntry struct {
		windowMin int64
		windowMax int64
		topic     string
		partition int
		relGaps   [][]int64
	}

	// Group window entries by "topic-partition".
	grouped := make(map[string][]windowEntry)
	groupOrder := []string{}

	for _, pw := range lastEntries {
		groupKey := fmt.Sprintf("%s-%d", pw.topic, pw.partition)
		if _, exists := grouped[groupKey]; !exists {
			groupOrder = append(groupOrder, groupKey)
		}
		grouped[groupKey] = append(grouped[groupKey], windowEntry{
			windowMin: pw.windowMin,
			windowMax: pw.windowMax,
			topic:     pw.topic,
			partition: pw.partition,
			relGaps:   pw.relGaps,
		})
	}

	var results []json.RawMessage

	for _, groupKey := range groupOrder {
		windows := grouped[groupKey]

		// Sort windows by windowMin so gaps are merged in ascending offset order.
		sort.Slice(windows, func(i, j int) bool {
			return windows[i].windowMin < windows[j].windowMin
		})

		first := windows[0]
		maxWindowMax := windows[len(windows)-1].windowMax

		// Merge gaps from all windows, converting relative offsets to absolute.
		var absoluteGaps []interface{}
		for _, w := range windows {
			for _, gap := range w.relGaps {
				if len(gap) == 1 {
					absoluteGaps = append(absoluteGaps, []interface{}{float64(gap[0] + w.windowMin)})
				} else if len(gap) == 2 {
					absoluteGaps = append(absoluteGaps, []interface{}{
						float64(gap[0] + w.windowMin),
						float64(gap[1] + w.windowMin),
					})
				}
			}
		}

		// For "first" mode, keep only the globally first (smallest absolute offset) gap.
		if config.limit == "first" && len(absoluteGaps) > 0 {
			absoluteGaps = absoluteGaps[:1]
		}

		outputEntry := map[string]interface{}{
			"topic":      first.topic,
			"partition":  first.partition,
			"window_min": 0,
			"window_max": maxWindowMax,
			"gaps":       absoluteGaps,
		}

		b, err := json.Marshal(outputEntry)
		if err != nil {
			Logger.Errorf("Failed to marshal entry: %v", err)
			continue
		}
		results = append(results, json.RawMessage(b))
	}

	// Compose compact results array as a string
	resultsStr := "[\n"
	for i, r := range results {
		resultsStr += "  " + string(r)
		if i < len(results)-1 {
			resultsStr += ","
		}
		resultsStr += "\n"
	}
	resultsStr += "]"
	// Compose top-level JSON manually for compactness
	outputStr := fmt.Sprintf("{\n  \"type\": \"%s\",\n  \"results\": %s\n}", config.limit, resultsStr)
	if config.resendFileName != "" {
		err := os.WriteFile(config.resendFileName, []byte(outputStr), 0644)
		if err != nil {
			Logger.Errorf("Failed to write resend file: %v", err)
		} else {
			Logger.Printf("Results written to %s", config.resendFileName)
		}
	} else {
		fmt.Println(outputStr)
	}
}
