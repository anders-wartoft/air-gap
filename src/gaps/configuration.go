package gaps

import (
	"bufio"
	"os"
	"strings"

	"sitia.nu/airgap/src/logging"
)

// GapsConfiguration holds configuration for the gaps display tool.
type GapsConfiguration struct {
	bootstrapServers string // Kafka bootstrap servers
	topic            string // Kafka topic to read gap data from
	groupID          string // Kafka consumer group ID
	logFileName      string // optional file to redirect logs to
	certFile         string // TLS certificate file
	keyFile          string // TLS key file
	caFile           string // TLS CA file
	keyPasswordFile  string // file containing the key password
	logLevel         string // log level: DEBUG, INFO, WARN, ERROR, FATAL
	mode             string // display mode: "short" (default) or "full"
}

var config GapsConfiguration

// Log counters
var receivedEvents int64

// Logger is the package-level logger.
var Logger = logging.Logger

func defaultConfiguration() GapsConfiguration {
	return GapsConfiguration{
		logLevel: "INFO",
		mode:     "short",
	}
}

// readParameters parses the property file and returns the updated configuration.
func readParameters(fileName string, result GapsConfiguration) (GapsConfiguration, error) {
	if fileName == "" {
		return result, nil
	}
	Logger.Print("Reading configuration from file " + fileName)
	file, err := os.Open(fileName)
	if err != nil {
		Logger.Fatalf("File: %s not found.", fileName)
		return result, nil
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "bootstrapServers":
			result.bootstrapServers = value
		case "topic":
			result.topic = value
		case "groupID":
			result.groupID = value
		case "logFileName":
			result.logFileName = value
		case "logLevel":
			result.logLevel = strings.ToUpper(value)
		case "certFile":
			result.certFile = value
		case "keyFile":
			result.keyFile = value
		case "caFile":
			result.caFile = value
		case "keyPasswordFile":
			result.keyPasswordFile = value
		case "mode":
			result.mode = strings.ToLower(value)
		}
	}
	return result, scanner.Err()
}

// overrideConfiguration applies environment variable overrides (AIRGAP_GAPS_* prefix).
func overrideConfiguration(config GapsConfiguration) GapsConfiguration {
	prefix := "AIRGAP_GAPS_"
	if v := os.Getenv(prefix + "BOOTSTRAP_SERVERS"); v != "" {
		config.bootstrapServers = v
	}
	if v := os.Getenv(prefix + "GAP_TOPIC"); v != "" {
		config.topic = v
	}
	if v := os.Getenv(prefix + "GROUP_ID"); v != "" {
		config.groupID = v
	}
	if v := os.Getenv(prefix + "LOG_LEVEL"); v != "" {
		config.logLevel = strings.ToUpper(v)
	}
	if v := os.Getenv(prefix + "LOG_FILE_NAME"); v != "" {
		config.logFileName = v
	}
	if v := os.Getenv(prefix + "CERT_FILE"); v != "" {
		config.certFile = v
	}
	if v := os.Getenv(prefix + "KEY_FILE"); v != "" {
		config.keyFile = v
	}
	if v := os.Getenv(prefix + "CA_FILE"); v != "" {
		config.caFile = v
	}
	if v := os.Getenv(prefix + "MODE"); v != "" {
		config.mode = strings.ToLower(v)
	}
	return config
}

// parseCommandLineOverrides applies --key=value arguments to the configuration.
func parseCommandLineOverrides(args []string, config GapsConfiguration) GapsConfiguration {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		arg = arg[2:]
		// Handle bare flags
		if arg == "full" {
			config.mode = "full"
			continue
		}
		if arg == "short" {
			config.mode = "short"
			continue
		}
		kv := strings.SplitN(arg, "=", 2)
		if len(kv) != 2 {
			Logger.Warnf("Ignoring malformed command line override: --%s", arg)
			continue
		}
		switch kv[0] {
		case "bootstrapServers":
			config.bootstrapServers = kv[1]
		case "topic":
			config.topic = kv[1]
		case "groupID":
			config.groupID = kv[1]
		case "mode":
			config.mode = strings.ToLower(kv[1])
		}
	}
	return config
}

// checkConfiguration validates required fields and sets defaults.
func checkConfiguration(config GapsConfiguration) GapsConfiguration {
	if config.bootstrapServers == "" {
		Logger.Fatal("bootstrapServers is required")
	}
	if config.topic == "" {
		Logger.Fatal("topic is required")
	}
	if config.groupID == "" {
		config.groupID = "gaps-group"
	}
	return config
}

// logConfiguration prints the active configuration.
func logConfiguration(config GapsConfiguration) {
	Logger.Printf("bootstrapServers: %s", config.bootstrapServers)
	Logger.Printf("topic:            %s", config.topic)
	Logger.Printf("groupID:          %s", config.groupID)
	Logger.Printf("mode:             %s", config.mode)
	Logger.Printf("logLevel:         %s", config.logLevel)
}
