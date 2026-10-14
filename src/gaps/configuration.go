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

// configApplied describes a setting that has been read and applied, so a
// consumer can see which setting changed without parsing the body.
func configApplied(setting, value string) logging.Attrs {
	return logging.Event(logging.EventConfigApplied,
		logging.Str("airgap.config.setting", setting),
		logging.Str("airgap.config.value", value),
	)
}

// readParameters parses the property file and returns the updated configuration.
func readParameters(fileName string, result GapsConfiguration) (GapsConfiguration, error) {
	if fileName == "" {
		return result, nil
	}
	Logger.Info(logging.Event(logging.EventConfigLoad,
		logging.Str("file.path", fileName),
	), "Reading configuration from file "+fileName)
	file, err := os.Open(fileName)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventConfigLoad,
			logging.Str("file.path", fileName),
		).Merge(logging.Err(err)), "File: %s not found.", fileName)
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
		case logging.OptionFormat, logging.OptionHash, logging.OptionHashChain,
			logging.OptionSigningCertificate, logging.OptionSigningKey,
			logging.OptionSignerID, logging.OptionSigningEKU:
			if _, err := logging.ApplyOption(key, value); err != nil {
				return result, err
			}
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
	if err := logging.ApplyEnvironmentOptions(prefix); err != nil {
		Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
			logging.Str("airgap.config.source", "environment"),
		).Merge(logging.Err(err)), "%v", err)
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
			Logger.Warnf(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.config.source", "command-line"),
				logging.Str("airgap.config.value", arg),
			), "Ignoring malformed command line override: --%s", arg)
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
		Logger.Fatal(logging.Event(logging.EventConfigInvalid,
			logging.Failure(),
			logging.Str("airgap.config.setting", "bootstrapServers"),
			logging.Str("error.type", "missing.required"),
		), "bootstrapServers is required")
	}
	if config.topic == "" {
		Logger.Fatal(logging.Event(logging.EventConfigInvalid,
			logging.Failure(),
			logging.Str("airgap.config.setting", "topic"),
			logging.Str("error.type", "missing.required"),
		), "topic is required")
	}
	if config.groupID == "" {
		config.groupID = "gaps-group"
	}
	return config
}

// logConfiguration prints the active configuration.
func logConfiguration(config GapsConfiguration) {
	Logger.Infof(configApplied("bootstrapServers", config.bootstrapServers),
		"bootstrapServers: %s", config.bootstrapServers)
	Logger.Infof(configApplied("topic", config.topic), "topic:            %s", config.topic)
	Logger.Infof(configApplied("groupID", config.groupID), "groupID:          %s", config.groupID)
	Logger.Infof(configApplied("mode", config.mode), "mode:             %s", config.mode)
	Logger.Infof(configApplied("logLevel", config.logLevel), "logLevel:         %s", config.logLevel)
}
