package create

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/version"
)

// main is the entry point of the program.
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

		// Map to store last entry for each topic-partition-window_min
		lastEntries := make(map[string]json.RawMessage)

		// Kafka message handler: parse JSON, store last entry
		kafkaHandler := func(id string, key []byte, t time.Time, received []byte) bool {
			atomic.AddInt64(&receivedEvents, 1)
			atomic.AddInt64(&totalReceived, 1)
			// Parse message as JSON
			var msg map[string]interface{}
			if err := json.Unmarshal(received, &msg); err != nil {
				Logger.Errorf("Failed to parse message: %v", err)
				return keepRunning
			}
			topic, _ := msg["topic"].(string)
			partition := fmt.Sprintf("%v", msg["partition"])
			windowMin := fmt.Sprintf("%v", msg["window_min"])
			keyStr := fmt.Sprintf("%s-%s-%s", topic, partition, windowMin)
			lastEntries[keyStr] = json.RawMessage(received)
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
func writeResults(lastEntries map[string]json.RawMessage, config TransferConfiguration) {
	// windowEntry holds the parsed fields needed for merging.
	type windowEntry struct {
		windowMin float64
		windowMax float64
		gaps      []interface{} // each element is []interface{} of float64
		raw       map[string]interface{}
	}

	// Group window entries by "topic-partition".
	grouped := make(map[string][]windowEntry) // key: "topic-partition"
	groupOrder := []string{}                  // preserve insertion order for determinism

	for _, v := range lastEntries {
		var entry map[string]interface{}
		if err := json.Unmarshal(v, &entry); err != nil {
			Logger.Errorf("Failed to parse last entry for output: %v", err)
			continue
		}
		topic, _ := entry["topic"].(string)
		partition := fmt.Sprintf("%v", entry["partition"])
		windowMin, _ := entry["window_min"].(float64)
		windowMax, _ := entry["window_max"].(float64)
		rawGaps, _ := entry["gaps"].([]interface{})

		groupKey := fmt.Sprintf("%s-%s", topic, partition)
		if _, exists := grouped[groupKey]; !exists {
			groupOrder = append(groupOrder, groupKey)
		}
		grouped[groupKey] = append(grouped[groupKey], windowEntry{
			windowMin: windowMin,
			windowMax: windowMax,
			gaps:      rawGaps,
			raw:       entry,
		})
	}

	var results []json.RawMessage

	for _, groupKey := range groupOrder {
		windows := grouped[groupKey]

		// Sort windows by windowMin so gaps are merged in ascending offset order.
		sort.Slice(windows, func(i, j int) bool {
			return windows[i].windowMin < windows[j].windowMin
		})

		// Use metadata from first window (topic, partition) and largest windowMax.
		first := windows[0]
		topic, _ := first.raw["topic"].(string)
		partitionVal := first.raw["partition"]
		maxWindowMax := windows[len(windows)-1].windowMax

		// Merge gaps from all windows, converting relative offsets to absolute.
		var absoluteGaps []interface{}
		for _, w := range windows {
			for _, rawGap := range w.gaps {
				gap, ok := rawGap.([]interface{})
				if !ok {
					continue
				}
				if len(gap) == 1 {
					rel, _ := gap[0].(float64)
					absoluteGaps = append(absoluteGaps, []interface{}{rel + w.windowMin})
				} else if len(gap) == 2 {
					relFrom, _ := gap[0].(float64)
					relTo, _ := gap[1].(float64)
					absoluteGaps = append(absoluteGaps, []interface{}{relFrom + w.windowMin, relTo + w.windowMin})
				}
			}
		}

		// For "first" mode, keep only the globally first (smallest absolute offset) gap.
		if config.limit == "first" && len(absoluteGaps) > 0 {
			absoluteGaps = absoluteGaps[:1]
		}

		outputEntry := map[string]interface{}{
			"topic":      topic,
			"partition":  partitionVal,
			"window_min": float64(0),
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
