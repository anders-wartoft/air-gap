package resend

import (
	"bufio"
	"crypto/rsa"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/protocol"
)

type TransferConfiguration struct {
	id                           string         // unique id for this resend
	nic                          string         // network interface card
	targetIP                     string         // target IP address
	targetPort                   int            // target port
	payloadSize                  uint16         // Maximum Transmission Unit - protocol headers
	from                         string         // Start time for reading from Kafka
	to                           string         // End time for reading from Kafka
	encryption                   bool           // encryption on/off
	key                          []byte         // symmetric key in use
	newkey                       []byte         // a new symmetric key, when successfully sent, copy the value to key
	publicKey                    *rsa.PublicKey // public key for encrypting the symmetric key
	publicKeyFile                string         // file with the public key
	generateNewSymmetricKeyEvery int            // seconds between key generation
	eps                          float64        // events per second, -1 means no limit
	bootstrapServers             string         // Kafka bootstrap servers
	topic                        string         // Kafka topic
	groupID                      string         // Kafka group ID
	logFileName                  string         // log file name, redirect logging to a file from the console
	certFile                     string         // Certificate to use to communicate with Kafka with TLS
	keyFile                      string         // Key file to use for TLS
	caFile                       string         // CA file to use for TLS
	keyPasswordFile              string         // File containing the password to decrypt the key file
	logLevel                     string         // log level: DEBUG, INFO, WARN, ERROR, FATAL
	limit                        string         // limit the exported messages: all, first (all means no limit)
	resendFileName               string         // output file name
	logStatistics                int            // log statistics interval in seconds, 0 means no logging
	compressWhenLengthExceeds    int            // compress messages when length exceeds this value, 0 means no compression
	partition                    int            // Kafka partition to read from. Can only read from one partition at a time when manually configured
	offsetFrom                   int64          // offset to start reading from
	offsetTo                     int64          // offset to stop reading at
}

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
	config.id = "Resend"
	config.logLevel = "INFO"
	config.logFileName = ""
	config.limit = "all" // default: no limit
	config.logStatistics = 0
	config.payloadSize = 1400            // default MTU (1500-protocol.HEADER_SIZE and some)
	config.from = "1970-01-01T00:00:00Z" // default: read from the beginning of time
	config.to = ""                       // default: read until the time this application started
	config.encryption = false
	config.key = nil
	config.newkey = nil
	config.publicKey = nil
	config.publicKeyFile = ""
	config.generateNewSymmetricKeyEvery = 0 // default: do not generate new keys
	config.eps = -1.0
	config.compressWhenLengthExceeds = 0 // default: no compression
	// Command line overrides
	config.topic = ""
	config.partition = -1
	config.offsetFrom = -1
	config.offsetTo = -1

	return config
}

// Parse the configuration file and return a TransferConfiguration struct.
func readParameters(fileName string, result TransferConfiguration) (TransferConfiguration, error) {
	if fileName == "" {
		// No file, return default configuration
		return result, nil
	}

	file, err := os.Open(fileName)
	if fileName == "" {
		// No file, return default configuration
		return result, nil
	}
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
		case "id":
			result.id = value
		case "nic":
			result.nic = value
		case "targetIP":
			result.targetIP = value
		case "targetPort":
			result.targetPort, err = strconv.Atoi(value)
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
		case "logStatistics":
			result.logStatistics, err = strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error parsing logStatistics: %v", err)
			}
			if result.logStatistics < 0 {
				result.logStatistics = 0
			}
			Logger.Printf("logStatistics: %d", result.logStatistics)
		case "from":
			result.from = value
			Logger.Printf("from: %s", result.from)
		case "to":
			result.to = value
			Logger.Printf("to: %s", result.to)
		case "encryption":
			result.encryption = strings.ToLower(value) == "true"
			Logger.Printf("encryption: %t", result.encryption)
		case "publicKeyFile":
			result.publicKeyFile = value
			Logger.Printf("publicKeyFile: %s", result.publicKeyFile)
		case "generateNewSymmetricKeyEvery":
			result.generateNewSymmetricKeyEvery, err = strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error parsing generateNewSymmetricKeyEvery: %v", err)
			}
			if result.generateNewSymmetricKeyEvery < 0 {
				result.generateNewSymmetricKeyEvery = 0
			}
			Logger.Printf("generateNewSymmetricKeyEvery: %d", result.generateNewSymmetricKeyEvery)
		case "payloadSize":
			if value == "auto" {
				result.payloadSize = 0
			} else {
				tmp, err := strconv.Atoi(value)
				if err != nil {
					Logger.Fatalf("Error in config payloadSize. Illegal value: %s. Legal values are 'auto' or a two byte integer", value)
				} else {
					result.payloadSize = uint16(tmp)
				}
			}
			if result.payloadSize < 500 || result.payloadSize > 65507-protocol.HEADER_SIZE {
				if result.payloadSize != 0 { // auto-detect payloadSize when payloadSize=0
					Logger.Fatalf("Error in config payloadSize. Illegal value: %d. Legal values are between 500 and %d", result.payloadSize, 65507-protocol.HEADER_SIZE)
				}
			}
			Logger.Printf("payloadSize: %d", result.payloadSize)
		case "eps":
			result.eps, err = strconv.ParseFloat(value, 64)
			if err != nil {
				Logger.Fatalf("Error parsing eps: %v", err)
			}
			if result.eps < -1 {
				result.eps = -1
			}
			Logger.Printf("eps: %f", result.eps)
		case "compressWhenLengthExceeds":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config compressWhenLengthExceeds. Illegal value: %s. Legal values are a non-negative integer", value)
			} else {
				if tmp < 0 {
					Logger.Fatalf("Error in config compressWhenLengthExceeds. Illegal value: %s. Legal values are a non-negative integer", value)
				} else {
					result.compressWhenLengthExceeds = tmp
					Logger.Printf("compressWhenLengthExceeds: %d", result.compressWhenLengthExceeds)
				}
			}
		default:
			Logger.Printf("Unknown configuration key: %s", key)
		}
	}

	if err := scanner.Err(); err != nil {
		return result, err
	}

	return result, nil
}

func overrideConfiguration(config TransferConfiguration) TransferConfiguration {

	Logger.Print("Checking configuration from environment variables...")
	prefix := "AIRGAP_RESEND_"
	if id := os.Getenv(prefix + "ID"); id != "" {
		Logger.Print("Overriding id with environment variable: " + prefix + "ID" + " with value: " + id)
		config.id = id
	}
	if nic := os.Getenv(prefix + "NIC"); nic != "" {
		Logger.Print("Overriding nic with environment variable: " + prefix + "NIC" + " with value: " + nic)
		config.nic = nic
	}
	if targetIP := os.Getenv(prefix + "TARGET_IP"); targetIP != "" {
		Logger.Print("Overriding targetIP with environment variable: " + prefix + "TARGET_IP" + " with value: " + targetIP)
		config.targetIP = targetIP
	}
	if targetPort := os.Getenv(prefix + "TARGET_PORT"); targetPort != "" {
		value, err := strconv.Atoi(targetPort)
		if err != nil {
			Logger.Fatalf("Error parsing TARGET_PORT environment variable: %v", err)
		}
		Logger.Print("Overriding targetPort with environment variable: " + prefix + "TARGET_PORT" + " with value: " + targetPort)
		config.targetPort = value
	}
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
	if resendFileName := os.Getenv(prefix + "RESULT_FILE_NAME"); resendFileName != "" {
		Logger.Print("Overriding resendFileName with environment variable: " + prefix + "RESULT_FILE_NAME" + " with value: " + resendFileName)
		config.resendFileName = resendFileName
	}
	if logStatistics := os.Getenv(prefix + "LOG_STATISTICS"); logStatistics != "" {
		value, err := strconv.Atoi(logStatistics)
		if err != nil {
			Logger.Fatalf("Error parsing LOG_STATISTICS environment variable: %v", err)
		}
		if value < 0 {
			value = 0
		}
		Logger.Print("Overriding logStatistics with environment variable: " + prefix + "LOG_STATISTICS" + " with value: " + logStatistics)
		config.logStatistics = value
	}
	if from := os.Getenv(prefix + "FROM"); from != "" {
		Logger.Print("Overriding from with environment variable: " + prefix + "FROM" + " with value: " + from)
		config.from = from
	}
	if to := os.Getenv(prefix + "TO"); to != "" {
		Logger.Print("Overriding to with environment vairable: " + prefix + "TO" + " with value: " + to)
		config.to = to
	}
	if encryption := os.Getenv(prefix + "ENCRYPTION"); encryption != "" {
		value := strings.ToLower(encryption) == "true"
		Logger.Print("Overriding encryption with environment variable: " + prefix + "ENCRYPTION" + " with value: " + encryption)
		config.encryption = value
	}
	if publicKeyFile := os.Getenv(prefix + "PUBLIC_KEY_FILE"); publicKeyFile != "" {
		Logger.Print("Overriding publicKeyFile with environment variable: " + prefix + "PUBLIC_KEY_FILE" + " with value: " + publicKeyFile)
		config.publicKeyFile = publicKeyFile
	}
	if generateNewSymmetricKeyEvery := os.Getenv(prefix + "GENERATE_NEW_SYMMETRIC_KEY_EVERY"); generateNewSymmetricKeyEvery != "" {
		value, err := strconv.Atoi(generateNewSymmetricKeyEvery)
		if err != nil {
			Logger.Fatalf("Error parsing GENERATE_NEW_SYMMETRIC_KEY_EVERY environment variable: %v", err)
		}
		if value < 0 {
			value = 0
		}
		Logger.Print("Overriding generateNewSymmetricKeyEvery with environment variable: " + prefix + "GENERATE_NEW_SYMMETRIC_KEY_EVERY" + " with value: " + generateNewSymmetricKeyEvery)
		config.generateNewSymmetricKeyEvery = value
	}
	if payloadSize := os.Getenv(prefix + "PAYLOAD_SIZE"); payloadSize != "" {
		if payloadSize == "auto" {
			config.payloadSize = 0
		} else {
			value, err := strconv.Atoi(payloadSize)
			if err != nil {
				Logger.Fatalf("Error parsing PAYLOAD_SIZE environment variable: %v", err)
			}
			if value < 500 || value > 65507-protocol.HEADER_SIZE {
				if value != 0 { // auto-detect payloadSize when payloadSize=0
					Logger.Fatalf("Error in config payloadSize. Illegal value: %d. Legal values are between 500 and %d", value, 65507-protocol.HEADER_SIZE)
				}
			}
			Logger.Print("Overriding payloadSize with environment variable: " + prefix + "PAYLOAD_SIZE" + " with value: " + payloadSize)
			config.payloadSize = uint16(value)
		}
	}
	if eps := os.Getenv(prefix + "EPS"); eps != "" {
		value, err := strconv.ParseFloat(eps, 64)
		if err != nil {
			Logger.Fatalf("Error parsing EPS environment variable: %v", err)
		}
		if value < -1 {
			value = -1
		}
		Logger.Print("Overriding eps with environment variable: " + prefix + "EPS" + " with value: " + eps)
		config.eps = value
	}
	if compressWhenLengthExceeds := os.Getenv(prefix + "COMPRESS_WHEN_LENGTH_EXCEEDS"); compressWhenLengthExceeds != "" {
		if compressWhenLengthExceedsInt, err := strconv.Atoi(compressWhenLengthExceeds); err == nil {
			Logger.Print("Overriding compressWhenLengthExceeds with environment variable: " + prefix + "COMPRESS_WHEN_LENGTH_EXCEEDS" + " with value: " + compressWhenLengthExceeds)
			config.compressWhenLengthExceeds = compressWhenLengthExceedsInt
		}
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
			case "id":
				config.id = value
			case "nic":
				config.nic = value
			case "targetIP":
				config.targetIP = value
			case "targetPort":
				v, err := strconv.Atoi(value)
				if err != nil {
					Logger.Fatalf("Error parsing targetPort command line argument: %v", err)
				}
				config.targetPort = v
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
			case "logStatistics":
				v, err := strconv.Atoi(value)
				if err != nil {
					Logger.Fatalf("Error parsing logStatistics command line argument: %v", err)
				}
				if v < 0 {
					v = 0
				}
				config.logStatistics = v
			case "from":
				config.from = value
			case "to":
				config.to = value
			case "encryption":
				config.encryption = strings.ToLower(value) == "true"
			case "publicKeyFile":
				config.publicKeyFile = value
			case "generateNewSymmetricKeyEvery":
				v, err := strconv.Atoi(value)
				if err != nil {
					Logger.Fatalf("Error parsing generateNewSymmetricKeyEvery command line argument: %v", err)
				}
				if v < 0 {
					v = 0
				}
				config.generateNewSymmetricKeyEvery = v
			case "payloadSize":
				if value == "auto" {
					config.payloadSize = 0
					break
				}
				payloadSize, err := strconv.Atoi(value)
				if err != nil {
					Logger.Fatalf("Error parsing payloadSize command line argument: %v", err)
				}
				if payloadSize < 500 || payloadSize > 65507-protocol.HEADER_SIZE {
					if payloadSize != 0 { // auto-detect payloadSize when payloadSize=0
						Logger.Fatalf("Error in config payloadSize. Illegal value: %d. Legal values are between 500 and %d", payloadSize, 65507-protocol.HEADER_SIZE)
					}
				}
				config.payloadSize = uint16(payloadSize)
			case "eps":
				v, err := strconv.ParseFloat(value, 64)
				if err != nil {
					Logger.Fatalf("Error parsing eps command line argument: %v", err)
				}
				if v < -1 {
					v = -1
				}
				config.eps = v
			case "compressWhenLengthExceeds":
				compressWhenLengthExceeds, err := strconv.Atoi(value)
				if err != nil {
					Logger.Fatalf("Error parsing compressWhenLengthExceeds value %s. Legal values are positive integers", compressWhenLengthExceeds)
				}
				config.compressWhenLengthExceeds = compressWhenLengthExceeds
			case "partition":
				v, err := strconv.Atoi(value)
				if err != nil {
					Logger.Fatalf("Error parsing partition command line argument: %v", err)
				}
				if v < 0 {
					Logger.Fatalf("Error parsing partition command line argument: %s must be a positive integer", value)
				}
				config.partition = v
			case "offsetFrom":
				v, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					Logger.Fatalf("Error parsing offsetFrom command line argument: %v", err)
				}
				if v < 0 {
					Logger.Fatalf("Error parsing offsetFrom command line argument: %s must be a positive integer", value)
				}
				config.offsetFrom = v
			case "offsetTo":
				v, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					Logger.Fatalf("Error parsing offsetTo command line argument: %v", err)
				}
				if v < -1 {
					Logger.Fatalf("Error parsing offsetTo command line argument: %s must be >= -1", value)
				}
				config.offsetTo = v
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

	if result.id == "" {
		Logger.Fatalf("Missing required configuration: id")
	}
	if result.nic == "" {
		Logger.Fatalf("Missing required configuration: nic")
	}
	if result.targetIP == "" {
		Logger.Fatalf("Missing required configuration: targetIP")
	}
	if result.targetPort == 0 {
		Logger.Fatalf("Missing required configuration: targetPort")
	}
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
		// Check that the resendFileName is a valid file name and that we can read from it
		file, err := os.OpenFile(result.resendFileName, os.O_RDONLY, 0644)
		if err != nil {
			Logger.Fatalf("Cannot open result file '%s' for reading: %v", result.resendFileName, err)
		}
		defer file.Close()
	}
	if result.encryption {
		if result.publicKeyFile == "" {
			Logger.Fatalf("encryption is enabled, but publicKeyFile is not set")
		} else {
			pubKey := readPublicKey(result.publicKeyFile)
			if pubKey == nil {
				Logger.Fatalf("Failed to load public key from file '%s': %v", result.publicKeyFile)
			}
			result.publicKey = pubKey
			Logger.Printf("Loaded public key from file '%s'", result.publicKeyFile)
		}
		if result.generateNewSymmetricKeyEvery < 0 {
			Logger.Fatalf("Error in config generateNewSymmetricKeyEvery. Illegal value: %d. Legal values are 0 or higher", result.generateNewSymmetricKeyEvery)
		}
		if result.generateNewSymmetricKeyEvery > 0 {
			nextKeyGeneration = time.Now().Add(time.Duration(result.generateNewSymmetricKeyEvery) * time.Second)
			Logger.Printf("New symmetric keys will be generated every %d seconds", result.generateNewSymmetricKeyEvery)
		} else {
			Logger.Printf("The same symmetric key will be used for the entire session")
		}
		Logger.Printf("Symmetric key generated")
	} else {
		Logger.Printf("encryption is disabled")
	}
	if result.payloadSize < 500 || result.payloadSize > 65507 {
		if result.payloadSize != 0 { // auto-detect payloadSize when payloadSize=0
			Logger.Fatalf("Error in config payloadSize. Illegal value: %d. Legal values are between 500 and %d", result.payloadSize, 65507-protocol.HEADER_SIZE)
		}
	}
	if result.eps < -1 {
		Logger.Fatalf("Error in config eps. Illegal value: %d. Legal values are -1 or higher", result.eps)
	}

	return result
}

func logConfiguration(config TransferConfiguration) {
	Logger.Printf("Configuration:")
	Logger.Printf("  id: %s", config.id)
	Logger.Printf("  nic: %s", config.nic)
	Logger.Printf("  targetIP: %s", config.targetIP)
	Logger.Printf("  targetPort: %d", config.targetPort)
	Logger.Printf("  logLevel: %s", config.logLevel)
	Logger.Printf("  logFileName: %s", config.logFileName)
	Logger.Printf("  bootstrapServers: %s", config.bootstrapServers)
	Logger.Printf("  topic: %s", config.topic)
	Logger.Printf("  groupID: %s", config.groupID)
	Logger.Printf("  limit: %s", config.limit)
	Logger.Printf("  certFile: %s", config.certFile)
	Logger.Printf("  keyFile: %s", config.keyFile)
	Logger.Printf("  keyPasswordFile: %s", config.keyPasswordFile)
	Logger.Printf("  caFile: %s", config.caFile)
	Logger.Printf("  resendFileName: %s", config.resendFileName)
	Logger.Printf("  logStatistics: %d", config.logStatistics)
	Logger.Printf("  from: %s", config.from)
	Logger.Printf("  to: %s", config.to)
	Logger.Printf("  encryption: %t", config.encryption)
	Logger.Printf("  eps: %f", config.eps)
	if config.encryption {
		Logger.Printf("  publicKeyFile: %s", config.publicKeyFile)
		Logger.Printf("  generateNewSymmetricKeyEvery: %d", config.generateNewSymmetricKeyEvery)
	}
	Logger.Printf("  payloadSize: %d", config.payloadSize)
	Logger.Printf("  compressWhenLengthExceeds: %d bytes", config.compressWhenLengthExceeds)
	Logger.Printf("  partition: %d", config.partition)
	Logger.Printf("  offsetFrom: %d", config.offsetFrom)
	Logger.Printf("  offsetTo: %d", config.offsetTo)
}
