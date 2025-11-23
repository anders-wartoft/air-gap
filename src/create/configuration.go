package create

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

// Parse the configuration file and return a TransferConfiguration struct.
func readParameters(fileName string, result TransferConfiguration) (TransferConfiguration, error) {
	if fileName == "" {
		// No file, return default configuration
		return result, nil
	}
	Logger.Print("Reading configuration from file " + fileName)
	file, err := os.Open(fileName)
	if err != nil {
		// No file, but that's ok. Maybe the user only uses environment variables
		Logger.Fatalf("File: %s not found.", fileName)
		return result, nil
	}
	defer file.Close()

	Logger.Print("Reading configuration from file " + fileName)

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
		case "logFileName":
			result.logFileName = value
			Logger.Printf("logFileName: %s", value)
		case "bootstrapServers":
			result.bootstrapServers = value
			Logger.Printf("bootstrapServers: %s", value)
		case "topic":
			result.topic = value
			Logger.Printf("topic: %s", value)
		case "groupID":
			result.groupID = value
		case "logLevel":
			result.logLevel = strings.ToUpper(value)
			Logger.Printf("logLevel: %s", result.logLevel)
		case "certFile":
			result.certFile = value
			Logger.Printf("certFile: %s", value)
		case "keyFile":
			result.keyFile = value
			Logger.Printf("keyFile: %s", value)
		case "caFile":
			result.caFile = value
			Logger.Printf("caFile: %s", value)
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

	Logger.Print("Checking configuration from environment variables...")
	prefix := "AIRGAP_CREATE_"
	if bootstrapServers := os.Getenv(prefix + "BOOTSTRAP_SERVERS"); bootstrapServers != "" {
		Logger.Print("Overriding bootstrapServers with environment variable: " + prefix + "BOOTSTRAP_SERVERS" + " with value: " + bootstrapServers)
		config.bootstrapServers = bootstrapServers
	}
	if topic := os.Getenv(prefix + "TOPIC"); topic != "" {
		Logger.Print("Overriding topic with environment variable: " + prefix + "TOPIC" + " with value: " + topic)
		config.topic = topic
	}
	if groupID := os.Getenv(prefix + "GROUP_ID"); groupID != "" {
		Logger.Print("Overriding groupID with environment variable: " + prefix + "GROUP_ID" + " with value: " + groupID)
		config.groupID = groupID
	}
	if logLevel := os.Getenv(prefix + "LOG_LEVEL"); logLevel != "" {
		Logger.Print("Overriding logLevel with environment variable: " + prefix + "LOG_LEVEL" + " with value: " + logLevel)
		config.logLevel = logLevel
	}
	if logFileName := os.Getenv(prefix + "LOG_FILE_NAME"); logFileName != "" {
		Logger.Print("Overriding logFileName with environment variable: " + prefix + "LOG_FILE_NAME" + " with value: " + logFileName)
		config.logFileName = logFileName
	}
	if certFile := os.Getenv(prefix + "CERT_FILE"); certFile != "" {
		Logger.Print("Overriding certFile with environment variable: " + prefix + "CERT_FILE" + " with value: " + certFile)
		config.certFile = certFile
	}
	if keyFile := os.Getenv(prefix + "KEY_FILE"); keyFile != "" {
		Logger.Print("Overriding keyFile with environment variable: " + prefix + "KEY_FILE" + " with value: " + keyFile)
		config.keyFile = keyFile
	}
	if caFile := os.Getenv(prefix + "CA_FILE"); caFile != "" {
		Logger.Print("Overriding caFile with environment variable: " + prefix + "CA_FILE" + " with value: " + caFile)
		config.caFile = caFile
	}
	if limit := os.Getenv(prefix + "LIMIT"); limit != "" {
		Logger.Print("Overriding limit with environment variable: " + prefix + "LIMIT" + " with value: " + limit)
		config.limit = strings.ToLower(limit)
	}
	if resendFileName := os.Getenv(prefix + "RESEND_FILE_NAME"); resendFileName != "" {
		Logger.Print("Overriding resendFileName with environment variable: " + prefix + "RESEND_FILE_NAME" + " with value: " + resendFileName)
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
				Logger.Warnf("Ignoring malformed command line override: %s", arg)
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
			case "logLevel":
				config.logLevel = strings.ToUpper(value)
			case "limit":
				config.limit = strings.ToLower(value)
			case "resendFileName":
				config.resendFileName = value
			default:
				found = false
				Logger.Warnf("Unknown command line override: %s", key)
			}
			if found {
				Logger.Printf("Overriding %s with command line argument: --%s=%s", key, key, value)
			}
		}
	}
	return config
}

// Check the configuration. On fail, will terminate the application
func checkConfiguration(result TransferConfiguration) TransferConfiguration {
	Logger.Print("Validating the configuration...")

	if result.logFileName != "" {
		// Check that the logFileName is a valid file name
		file, err := os.OpenFile(result.logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			Logger.Fatalf("Cannot open log file '%s' for writing: %v", result.logFileName, err)
		}
		defer file.Close()
		// Check that we can write to that file
		if _, err := file.WriteString(""); err != nil {
			Logger.Fatalf("Cannot write to log file '%s': %v", result.logFileName, err)
		}
	}
	if result.logLevel != "" {
		Logger.SetLogLevel(result.logLevel)
		var tmp = Logger.GetLogLevel()
		if (tmp == result.logLevel) == false {
			Logger.Fatalf("Error in config logLevel. Illegal value: %s. Legal values are TRACE, DEBUG, INFO, WARN, ERROR, FATAL", result.logLevel)
		} else {
			Logger.Printf("logLevel: %s", tmp)
		}
	}
	// if one of certFile, keyFile or caFile is given, they all must be
	if result.certFile != "" || result.keyFile != "" || result.caFile != "" {
		if result.certFile == "" {
			Logger.Fatalf("Missing required configuration: certFile")
		}
		if result.keyFile == "" {
			Logger.Fatalf("Missing required configuration: keyFile")
		}
		if result.caFile == "" {
			Logger.Fatalf("Missing required configuration: caFile")
		}
	}
	if result.keyPasswordFile != "" && (result.keyFile != "" || result.certFile != "" || result.caFile != "") {
		// Make sure this file exists and is readable. If not, print the complete file path for easier debugging
		absPath, _ := filepath.Abs(result.keyPasswordFile)
		file, err := os.Open(result.keyPasswordFile)
		if err != nil {
			Logger.Fatalf("Cannot open keyPasswordFile '%s' (absolute path: '%s'): %v", result.keyPasswordFile, absPath, err)
		}
		defer file.Close()
	}
	if result.limit != "all" && result.limit != "first" {
		Logger.Fatalf("Error in config limit. Illegal value: %s. Legal values are all, first", result.limit)
	}
	if result.resendFileName != "" {
		// Check that the resendFileName is a valid file name and that we can write to it
		file, err := os.OpenFile(result.resendFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			Logger.Fatalf("Cannot open resend file '%s' for writing: %v", result.resendFileName, err)
		}
		defer file.Close()
		// Check that we can write to that file
		if _, err := file.WriteString(""); err != nil {
			Logger.Fatalf("Cannot write to resend file '%s': %v", result.resendFileName, err)
		}
	}
	return result
}

func logConfiguration(config TransferConfiguration) {
	Logger.Printf("Configuration:")
	Logger.Printf("  logLevel: %s", config.logLevel)
	Logger.Printf("  logFileName: %s", config.logFileName)
	Logger.Printf("  bootstrapServers: %s", config.bootstrapServers)
	Logger.Printf("  topic: %s", config.topic)
	Logger.Printf("  groupID: %s", config.groupID)
	Logger.Printf("  certFile: %s", config.certFile)
	Logger.Printf("  keyFile: %s", config.keyFile)
	Logger.Printf("  keyPasswordFile: %s", config.keyPasswordFile)
	Logger.Printf("  caFile: %s", config.caFile)
	Logger.Printf("  limit: %s", config.limit)
	Logger.Printf("  resendFileName: %s", config.resendFileName)
}
