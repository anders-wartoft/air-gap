package create

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"time"

	"sitia.nu/airgap/src/internal/properties"
	"sitia.nu/airgap/src/logging"
)

type TransferConfiguration struct {
	bootstrapServers string // Kafka bootstrap servers
	topic            string // Kafka topic
	groupID          string // Kafka group ID
	logFileName      string // log file name, redirect logging to a file from the console
	certFile         string // Certificate to use to communicate with Kafka with TLS
	keyFile          string // Key file to use for TLS
	caFile           string // CA file to use for TLS
	keyPasswordFile  string // File containing the password to decrypt the key file
	logLevel         string // log level: DEBUG, INFO, WARN, ERROR, FATAL
	limit            string // limit the exported messages: all, first (all means no limit)
	resendFileName   string // output file name
}

var config TransferConfiguration
var nextKeyGeneration time.Time
var keepRunning bool = true

// Log counters
var receivedEvents int64
var sentEvents int64
var totalReceived int64
var totalSent int64
var timeStart int64

// Start by logging to the console. If a log file is specified in the configuration file, use that
// instead. The log file will be created if it doesn't exist, and appended to if it does.
// Any errors creating the log file will be reported to the console.
var Logger = logging.Logger

// BuildNumber is set at build time via -ldflags
var BuildNumber = "dev"

type Stats struct {
	mu         sync.Mutex
	count      int64
	lastOffset int64
	lastTS     time.Time
}

func defaultConfiguration() TransferConfiguration {
	config := TransferConfiguration{}
	config.logLevel = "INFO"
	config.logFileName = ""
	config.limit = "all" // default: no limit
	return config
}

// configApplied describes a setting that has been read and applied, so a
// consumer can see which setting changed without parsing the body.
func configApplied(setting, value string) logging.Attrs {
	return logging.Event(logging.EventConfigApplied,
		logging.Str("airgap.config.setting", setting),
		logging.Str("airgap.config.value", value),
	)
}

// configInvalid describes a setting that failed validation.
func configInvalid(setting, value string) logging.Attrs {
	return logging.Event(logging.EventConfigInvalid,
		logging.Failure(),
		logging.Str("airgap.config.setting", setting),
		logging.Str("airgap.config.value", value),
	)
}

// configOverride describes a setting replaced by the environment or the
// command line. The source says which of the two did it.
func configOverride(source, setting, value string) logging.Attrs {
	return logging.Event(logging.EventConfigOverride,
		logging.Str("airgap.config.source", source),
		logging.Str("airgap.config.setting", setting),
		logging.Str("airgap.config.value", value),
	)
}

// errAttr carries the failure detail of an error onto an existing list.
func errAttr(err error) logging.Attr {
	return logging.ErrorType(err)
}

// Parse the configuration file and return a TransferConfiguration struct.
func readParameters(fileName string, result TransferConfiguration) (TransferConfiguration, error) {
	if fileName == "" {
		// No file, return default configuration
		return result, nil
	}
	Logger.Info(logging.Event(logging.EventConfigLoad,
		logging.Str("file.path", fileName),
	), "Reading configuration from file "+fileName)
	file, err := os.Open(fileName)
	if err != nil {
		// No file, but that's ok. Maybe the user only uses environment variables
		Logger.Fatalf(logging.Event(logging.EventConfigLoad,
			logging.Failure(),
			logging.Str("file.path", fileName),
			errAttr(err),
		), "File: %s not found.", fileName)
		return result, nil
	}
	defer file.Close()

	Logger.Info(logging.Event(logging.EventConfigLoad,
		logging.Str("file.path", fileName),
	), "Reading configuration from file "+fileName)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := properties.StripComment(scanner.Text())
		if line == "" {
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
		case "logFileName":
			result.logFileName = value
			Logger.Infof(configApplied("logFileName", value), "logFileName: %s", value)
		case "bootstrapServers":
			result.bootstrapServers = value
			Logger.Infof(configApplied("bootstrapServers", value), "bootstrapServers: %s", value)
		case "topic":
			result.topic = value
			Logger.Infof(configApplied("topic", value), "topic: %s", value)
		case "groupID":
			result.groupID = value
		case "logLevel":
			result.logLevel = strings.ToUpper(value)
			Logger.Infof(configApplied("logLevel", value), "logLevel: %s", result.logLevel)
		case "certFile":
			result.certFile = value
			Logger.Infof(configApplied("certFile", value), "certFile: %s", value)
		case "keyFile":
			result.keyFile = value
			Logger.Infof(configApplied("keyFile", value), "keyFile: %s", value)
		case "caFile":
			result.caFile = value
			Logger.Infof(configApplied("caFile", value), "caFile: %s", value)
		case "keyPasswordFile":
			result.keyPasswordFile = value
		case "limit":
			result.limit = strings.ToLower(value)
		case "resendFileName":
			result.resendFileName = value
		}
	}

	if err := scanner.Err(); err != nil {
		return result, err
	}

	return result, nil
}

func overrideConfiguration(config TransferConfiguration) TransferConfiguration {

	prefix := "AIRGAP_CREATE_"
	Logger.Info(logging.Event(logging.EventConfigOverride,
		logging.Str("airgap.config.source", "environment"),
		logging.Str("airgap.config.prefix", prefix),
	), "Checking configuration from environment variables...")
	if bootstrapServers := os.Getenv(prefix + "BOOTSTRAP_SERVERS"); bootstrapServers != "" {
		Logger.Info(configOverride("environment", "bootstrapServers", bootstrapServers), "Overriding bootstrapServers with environment variable: "+prefix+"BOOTSTRAP_SERVERS"+" with value: "+bootstrapServers)
		config.bootstrapServers = bootstrapServers
	}
	if topic := os.Getenv(prefix + "TOPIC"); topic != "" {
		Logger.Info(configOverride("environment", "topic", topic), "Overriding topic with environment variable: "+prefix+"TOPIC"+" with value: "+topic)
		config.topic = topic
	}
	if groupID := os.Getenv(prefix + "GROUP_ID"); groupID != "" {
		Logger.Info(configOverride("environment", "groupID", groupID), "Overriding groupID with environment variable: "+prefix+"GROUP_ID"+" with value: "+groupID)
		config.groupID = groupID
	}
	if logLevel := os.Getenv(prefix + "LOG_LEVEL"); logLevel != "" {
		Logger.Info(configOverride("environment", "logLevel", logLevel), "Overriding logLevel with environment variable: "+prefix+"LOG_LEVEL"+" with value: "+logLevel)
		config.logLevel = logLevel
	}
	if logFileName := os.Getenv(prefix + "LOG_FILE_NAME"); logFileName != "" {
		Logger.Info(configOverride("environment", "logFileName", logFileName), "Overriding logFileName with environment variable: "+prefix+"LOG_FILE_NAME"+" with value: "+logFileName)
		config.logFileName = logFileName
	}
	if err := logging.ApplyEnvironmentOptions(prefix); err != nil {
		Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
			logging.Failure(),
			logging.Str("airgap.config.source", "environment"),
			errAttr(err),
		), "%v", err)
	}
	if certFile := os.Getenv(prefix + "CERT_FILE"); certFile != "" {
		Logger.Info(configOverride("environment", "certFile", certFile), "Overriding certFile with environment variable: "+prefix+"CERT_FILE"+" with value: "+certFile)
		config.certFile = certFile
	}
	if keyFile := os.Getenv(prefix + "KEY_FILE"); keyFile != "" {
		Logger.Info(configOverride("environment", "keyFile", keyFile), "Overriding keyFile with environment variable: "+prefix+"KEY_FILE"+" with value: "+keyFile)
		config.keyFile = keyFile
	}
	if caFile := os.Getenv(prefix + "CA_FILE"); caFile != "" {
		Logger.Info(configOverride("environment", "caFile", caFile), "Overriding caFile with environment variable: "+prefix+"CA_FILE"+" with value: "+caFile)
		config.caFile = caFile
	}
	if limit := os.Getenv(prefix + "LIMIT"); limit != "" {
		Logger.Info(configOverride("environment", "limit", limit), "Overriding limit with environment variable: "+prefix+"LIMIT"+" with value: "+limit)
		config.limit = strings.ToLower(limit)
	}
	if resendFileName := os.Getenv(prefix + "RESEND_FILE_NAME"); resendFileName != "" {
		Logger.Info(configOverride("environment", "resendFileName", resendFileName), "Overriding resendFileName with environment variable: "+prefix+"RESEND_FILE_NAME"+" with value: "+resendFileName)
		config.resendFileName = resendFileName
	}

	return config
}

// parseCommandLineOverrides parses --name=value arguments and applies them to the config struct
func parseCommandLineOverrides(args []string, config TransferConfiguration) TransferConfiguration {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			kv := strings.SplitN(arg[2:], "=", 2)
			if len(kv) != 2 {
				Logger.Warnf(configInvalid("", arg).With(logging.Str("airgap.config.source", "command-line")),
					"Ignoring malformed command line override: %s", arg)
				continue
			}
			key := kv[0]
			value := kv[1]
			found := true
			switch key {
			case "bootstrapServers":
				config.bootstrapServers = value
			case "topic":
				config.topic = value
			case "groupID":
				config.groupID = value
			case "logFileName":
				config.logFileName = value
			case "certFile":
				config.certFile = value
			case "keyFile":
				config.keyFile = value
			case "caFile":
				config.caFile = value
			case "keyPasswordFile":
				config.keyPasswordFile = value
				Logger.Info(configOverride("command-line", key, "[set]"),
					"Overriding keyPasswordFile with command line argument: --keyPasswordFile=[set]")
				continue
			case "logLevel":
				config.logLevel = strings.ToUpper(value)
			case "limit":
				config.limit = strings.ToLower(value)
			case "resendFileName":
				config.resendFileName = value
			default:
				found = false
				Logger.Warnf(configInvalid(key, value).With(logging.Str("airgap.config.source", "command-line")),
					"Unknown command line override: %s", key)
			}
			if found {
				Logger.Infof(configOverride("command-line", key, value),
					"Overriding %s with command line argument: --%s=%s", key, key, value)
			}
		}
	}
	return config
}

// Check the configuration. On fail, will terminate the application
func checkConfiguration(result TransferConfiguration) TransferConfiguration {
	Logger.Info(logging.Event(logging.EventConfigParse), "Validating the configuration...")

	if result.logFileName != "" {
		// Check that the logFileName is a valid file name
		file, err := os.OpenFile(result.logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			Logger.Fatalf(configInvalid("logFileName", result.logFileName).With(logging.Str("file.path", result.logFileName), errAttr(err)),
				"Cannot open log file '%s' for writing: %v", result.logFileName, err)
		}
		defer file.Close()
		// Check that we can write to that file
		if _, err := file.WriteString(""); err != nil {
			Logger.Fatalf(configInvalid("logFileName", result.logFileName).With(logging.Str("file.path", result.logFileName), errAttr(err)),
				"Cannot write to log file '%s': %v", result.logFileName, err)
		}
	}
	if result.logLevel != "" {
		Logger.SetLogLevel(result.logLevel)
		var tmp = Logger.GetLogLevel()
		if (tmp == result.logLevel) == false {
			Logger.Fatalf(configInvalid("logLevel", result.logLevel),
				"Error in config logLevel. Illegal value: %s. Legal values are TRACE, DEBUG, INFO, WARN, ERROR, FATAL", result.logLevel)
		} else {
			Logger.Infof(configApplied("logLevel", tmp), "logLevel: %s", tmp)
		}
	}
	// if one of certFile, keyFile or caFile is given, they all must be
	if result.certFile != "" || result.keyFile != "" || result.caFile != "" {
		if result.certFile == "" {
			Logger.Fatalf(configInvalid("certFile", result.certFile).With(logging.Str("error.type", "missing.required")),
				"Missing required configuration: certFile")
		}
		if result.keyFile == "" {
			Logger.Fatalf(configInvalid("keyFile", result.keyFile).With(logging.Str("error.type", "missing.required")),
				"Missing required configuration: keyFile")
		}
		if result.caFile == "" {
			Logger.Fatalf(configInvalid("caFile", result.caFile).With(logging.Str("error.type", "missing.required")),
				"Missing required configuration: caFile")
		}
	}
	if result.keyPasswordFile != "" && (result.keyFile != "" || result.certFile != "" || result.caFile != "") {
		// Make sure this file exists and is readable without exposing its path.
		file, err := os.Open(result.keyPasswordFile)
		if err != nil {
			Logger.Fatal(configInvalid("keyPasswordFile", "[redacted]").With(logging.Error("file.unreadable")),
				"Cannot open configured keyPasswordFile; verify that it exists and is readable")
		}
		defer file.Close()
	}
	if result.limit != "all" && result.limit != "first" {
		Logger.Fatalf(configInvalid("limit", result.limit),
			"Error in config limit. Illegal value: %s. Legal values are all, first", result.limit)
	}
	if result.resendFileName != "" {
		// Check that the resendFileName is a valid file name and that we can write to it
		file, err := os.OpenFile(result.resendFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			Logger.Fatalf(configInvalid("resendFileName", result.resendFileName).With(logging.Str("file.path", result.resendFileName), errAttr(err)),
				"Cannot open resend file '%s' for writing: %v", result.resendFileName, err)
		}
		defer file.Close()
		// Check that we can write to that file
		if _, err := file.WriteString(""); err != nil {
			Logger.Fatalf(configInvalid("resendFileName", result.resendFileName).With(logging.Str("file.path", result.resendFileName), errAttr(err)),
				"Cannot write to resend file '%s': %v", result.resendFileName, err)
		}
	}
	if result.logFileName != "" {
		if err := Logger.SetLogFile(result.logFileName); err != nil {
			Logger.Fatal(configInvalid("logFileName", result.logFileName).With(logging.Str("file.path", result.logFileName), errAttr(err)), err)
		}
	}
	result.WarnProductionConfiguration("startup")
	return result
}

func logConfiguration(config TransferConfiguration) {
	Logger.Info(logging.Event(logging.EventConfigApplied), "Configuration:")
	Logger.Infof(configApplied("logLevel", config.logLevel), "  logLevel: %s", config.logLevel)
	Logger.Infof(configApplied("logFileName", config.logFileName), "  logFileName: %s", config.logFileName)
	Logger.Infof(configApplied("bootstrapServers", config.bootstrapServers), "  bootstrapServers: %s", config.bootstrapServers)
	Logger.Infof(configApplied("topic", config.topic), "  topic: %s", config.topic)
	Logger.Infof(configApplied("groupID", config.groupID), "  groupID: %s", config.groupID)
	Logger.Infof(configApplied("certFile", config.certFile), "  certFile: %s", config.certFile)
	Logger.Infof(configApplied("keyFile", config.keyFile), "  keyFile: %s", config.keyFile)
	Logger.Info(configApplied("keyPasswordFile", "[set]"), "  keyPasswordFile: [set]")
	Logger.Infof(configApplied("caFile", config.caFile), "  caFile: %s", config.caFile)
	Logger.Infof(configApplied("limit", config.limit), "  limit: %s", config.limit)
	Logger.Infof(configApplied("resendFileName", config.resendFileName), "  resendFileName: %s", config.resendFileName)
}
