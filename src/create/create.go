package create

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
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

// writeResults formats and outputs the results as JSON, either to a file or the console
func writeResults(lastEntries map[string]json.RawMessage, config TransferConfiguration) {
	var results []json.RawMessage
	for _, v := range lastEntries {
		var entry map[string]interface{}
		if err := json.Unmarshal(v, &entry); err != nil {
			Logger.Errorf("Failed to parse last entry for output: %v", err)
			continue
		}
		// If limit is 'first', only keep the minimum gap offset
		if config.limit == "first" {
			if gaps, ok := entry["gaps"].([]interface{}); ok && len(gaps) > 0 {
				minGap := gaps[0]
				entry["gaps"] = []interface{}{minGap}
			}
		}
		b, err := json.Marshal(entry)
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
