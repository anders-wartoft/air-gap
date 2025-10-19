package upstream

import (
	"bufio"
	"crypto/rsa"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"sitia.nu/airgap/src/filter"
)

type TransferConfiguration struct {
	id                           string            // unique id for this upstream
	nic                          string            // network interface card
	targetIP                     string            // target IP address
	targetPort                   int               // target port
	bootstrapServers             string            // Kafka bootstrap servers
	topic                        string            // Kafka topic
	groupID                      string            // Kafka group ID
	payloadSize                  uint16            // Maximum Length of payload before fragmentations starts
	from                         string            // Start time for reading from Kafka
	encryption                   bool              // encryption on/off
	key                          []byte            // symmetric key in use
	newkey                       []byte            // a new symmetric key, when successfully sent, copy the value to key
	publicKey                    *rsa.PublicKey    // public key for encrypting the symmetric key
	publicKeyFile                string            // file with the public key
	source                       string            // source of the messages, kafka or random
	generateNewSymmetricKeyEvery int               // seconds between key generation
	logFileName                  string            // log file name, redirect logging to a file from the console
	sendingThreads               []map[string]int  // array of objects with thread names and offsets
	certFile                     string            // Certificate to use to communicate with Kafka with TLS
	keyFile                      string            // Key file to use for TLS
	caFile                       string            // CA file to use for TLS
	deliverFilter                string            // filter configuration
	topicTranslations            string            // topic translations in JSON format
	translations                 map[string]string // map from input topic to output topic (derived from topicTranslations)
	filter                       *filter.Filter    // filter instance
	eps                          float64           // events per second
	logLevel                     string            // log level: DEBUG, INFO, WARN, ERROR, FATAL
	logStatistics                int32             // log statistics every n seconds, 0 means no logging
	compressWhenLengthExceeds    int               // compress messages when length exceeds this value, 0 means no compression
}

func DefaultConfiguration() TransferConfiguration {
	config := TransferConfiguration{}
	config.source = "random"
	config.logLevel = "INFO"
	config.encryption = false
	config.id = "default_upstream"
	config.logFileName = ""
	config.translations = make(map[string]string)
	config.payloadSize = 0 // default auto
	config.sendingThreads = []map[string]int{
		{"now": 0},
	}
	config.eps = -1                      // default: no throttle
	config.logStatistics = 0             // default: no statistics
	config.compressWhenLengthExceeds = 0 // default: no compression
	return config
}

// Parse the configuration file and return a TransferConfiguration struct.
func ReadParameters(fileName string, result TransferConfiguration) (TransferConfiguration, error) {
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
		case "eps":
			tmp, err := strconv.ParseFloat(value, 64)
			if err != nil {
				Logger.Fatalf("Error in config eps. Illegal value: %s. Legal values are float >= -1", value)
			} else {
				result.eps = tmp
				Logger.Printf("eps: %f", tmp)
			}
		case "id":
			result.id = value
			Logger.Printf("id: %s", value)
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
			Logger.Printf("payloadSize: %d", result.payloadSize)
		case "nic":
			result.nic = value
			Logger.Printf("nic: %s", value)
		case "topicTranslations":
			// Json like {"inputTopic1":"outputTopic1","inputTopic2":"outputTopic2"}
			result.topicTranslations = value
		case "logFileName":
			result.logFileName = value
			Logger.Printf("logFileName: %s", value)
		case "targetIP":
			result.targetIP = value
			Logger.Printf("targetIP: %s", value)
		case "targetPort":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config targetPort. Illegal value: %s. Legal values are 0-65535", value)
			} else {
				if tmp < 0 || tmp > 65535 {
					Logger.Fatalf("Error in config targetPort. Illegal value: %s. Legal values are 0-65535", value)
				} else {
					result.targetPort = tmp
				}
			}
			Logger.Printf("targetPort: %d", result.targetPort)
		case "bootstrapServers":
			result.bootstrapServers = value
			Logger.Printf("bootstrapServers: %s", value)
		case "topic":
			result.topic = value
			Logger.Printf("topic: %s", value)
		case "groupID":
			result.groupID = value
		case "from":
			result.from = value
			Logger.Printf("from: %s", value)
		case "publicKeyFile":
			result.publicKeyFile = value
			Logger.Printf("publicKeyFile: %s", value)
		case "source":
			if value == "kafka" || value == "random" {
				result.source = value
			} else {
				Logger.Fatalf("Unknown source %s. Legal values are 'kafka' or 'random'.", value)
			}
			Logger.Printf("source: %s", value)
		case "logLevel":
			result.logLevel = strings.ToUpper(value)
			Logger.Printf("logLevel: %s", result.logLevel)
		case "encryption":
			tmp, err := strconv.ParseBool(value)
			if err != nil {
				Logger.Fatalf("Error in config encryption. Illegal value: %s. Legal values are true or false", value)
			} else {
				result.encryption = tmp
			}
			var encryptionStr string
			if result.encryption {
				encryptionStr = "true"
			} else {
				encryptionStr = "false"
			}
			Logger.Printf("encryption: %s", encryptionStr)
		case "generateNewSymmetricKeyEvery": // second
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config generateNewSymmetricKeyEvery. Illegal value: %s. Legal values are a four byte integer", value)
			} else {
				if tmp < 2 {
					Logger.Fatalf("Error in config generateNewSymmetricKeyEvery. Illegal value: %s. Legal values are integer >= 2", value)
				} else {
					result.generateNewSymmetricKeyEvery = tmp
					Logger.Printf("generateNewSymmetricKeyEvery: %d", tmp)
				}
			}
		case "sendingThreads":
			// Remove the old value for result.sendingThreads
			result.sendingThreads = nil
			// Read the new ones
			if err := json.Unmarshal([]byte(value), &result.sendingThreads); err != nil {
				Logger.Fatalf("Error in config sendingThreads. Illegal value: %s. Legal values are an array of objects", value)
			}
			Logger.Printf("sendingThreads: %v", result.sendingThreads)

		case "certFile":
			result.certFile = value
			Logger.Printf("certFile: %s", value)
		case "keyFile":
			result.keyFile = value
			Logger.Printf("keyFile: %s", value)
		case "caFile":
			result.caFile = value
			Logger.Printf("caFile: %s", value)
		case "deliverFilter":
			result.deliverFilter = value
			Logger.Printf("deliverFilter: %s", value)
		case "logStatistics":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config logStatistics. Illegal value: %s. Legal values are a non-negative integer", value)
			} else {
				if tmp < 0 {
					Logger.Fatalf("Error in config logStatistics. Illegal value: %s. Legal values are a non-negative integer", value)
				} else {
					result.logStatistics = int32(tmp)
					Logger.Printf("logStatistics: %d", result.logStatistics)
				}
			}
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
			Logger.Fatalf("Unknown configuration key: %s", key)
		}
	}

	if result.source == "kafka" {
		// Check the sending threads
		if len(result.sendingThreads) == 0 {
			Logger.Print("No sendingThreads found. Adding default: sendingThreads: [{'now': 0}]")
			result.sendingThreads = []map[string]int{
				{"now": 0},
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return result, err
	}

	return result, nil
}

func overrideConfiguration(config TransferConfiguration) TransferConfiguration {

	Logger.Print("Checking configuration from environment variables...")
	prefix := "AIRGAP_UPSTREAM_"
	if eps := os.Getenv(prefix + "EPS"); eps != "" {
		if epsFloat, err := strconv.ParseFloat(eps, 64); err == nil {
			Logger.Print("Overriding eps with environment variable: " + prefix + "EPS" + " with value: " + eps)
			config.eps = epsFloat
		}
	}

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
		if port, err := strconv.Atoi(targetPort); err == nil {
			Logger.Print("Overriding targetPort with environment variable: " + prefix + "TARGET_PORT" + " with value: " + targetPort)
			config.targetPort = port
		}
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
	if payloadSize := os.Getenv(prefix + "PAYLOAD_SIZE"); payloadSize != "" {
		Logger.Print("Overriding payloadSize with environment variable: " + prefix + "PAYLOAD_SIZE" + " with value: " + payloadSize)
		if payloadSize == "auto" {
			config.payloadSize = 0
		} else if payloadSizeInt, err := strconv.Atoi(payloadSize); err == nil {
			config.payloadSize = uint16(payloadSizeInt)
		}
	}
	if topicTranslations := os.Getenv(prefix + "TOPIC_TRANSLATIONS"); topicTranslations != "" {
		Logger.Print("Overriding topicTranslations with environment variable: " + prefix + "TOPIC_TRANSLATIONS" + " with value: " + topicTranslations)
		config.topicTranslations = topicTranslations
	}
	if from := os.Getenv(prefix + "FROM"); from != "" {
		Logger.Print("Overriding from with environment variable: " + prefix + "FROM" + " with value: " + from)
		config.from = from
	}
	if encryption := os.Getenv(prefix + "ENCRYPTION"); encryption != "" {
		Logger.Print("Overriding encryption with environment variable: " + prefix + "ENCRYPTION" + " with value: " + encryption)
		config.encryption = encryption == "true"
	}
	if publicKeyFile := os.Getenv(prefix + "PUBLIC_KEY_FILE"); publicKeyFile != "" {
		Logger.Print("Overriding publicKeyFile with environment variable: " + prefix + "PUBLIC_KEY_FILE" + " with value: " + publicKeyFile)
		config.publicKeyFile = publicKeyFile
	}
	if generateNewSymmetricKeyEvery := os.Getenv(prefix + "GENERATE_NEW_SYMMETRIC_KEY_EVERY"); generateNewSymmetricKeyEvery != "" {
		if generateNewSymmetricKeyEveryInt, err := strconv.Atoi(generateNewSymmetricKeyEvery); err == nil {
			Logger.Print("Overriding generateNewSymmetricKeyEvery with environment variable: " + prefix + "GENERATE_NEW_SYMMETRIC_KEY_EVERY" + " with value: " + generateNewSymmetricKeyEvery)
			config.generateNewSymmetricKeyEvery = generateNewSymmetricKeyEveryInt
		}
	}
	if logLevel := os.Getenv(prefix + "LOG_LEVEL"); logLevel != "" {
		Logger.Print("Overriding logLevel with environment variable: " + prefix + "LOG_LEVEL" + " with value: " + logLevel)
		config.logLevel = logLevel
	}
	if logFileName := os.Getenv(prefix + "LOG_FILE_NAME"); logFileName != "" {
		Logger.Print("Overriding logFileName with environment variable: " + prefix + "LOG_FILE_NAME" + " with value: " + logFileName)
		config.logFileName = logFileName
	}
	if sendingThreads := os.Getenv(prefix + "SENDING_THREADS"); sendingThreads != "" {
		Logger.Print("Overriding sendingThreads with environment variable: " + prefix + "SENDING_THREADS" + " with value: " + sendingThreads)
		if err := json.Unmarshal([]byte(sendingThreads), &config.sendingThreads); err != nil {
			Logger.Fatalf("Error parsing SENDING_THREADS:", err)
		}
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
	if source := os.Getenv(prefix + "SOURCE"); source != "" {
		Logger.Print("Overriding source with environment variable: " + prefix + "SOURCE" + " with value: " + source)
		config.source = source
	}
	if deliverFilter := os.Getenv(prefix + "DELIVER_FILTER"); deliverFilter != "" {
		Logger.Print("Overriding deliverFilter with environment variable: " + prefix + "DELIVER_FILTER" + " with value: " + deliverFilter)
		config.deliverFilter = deliverFilter
	}
	if logStatistics := os.Getenv(prefix + "LOG_STATISTICS"); logStatistics != "" {
		if logStatisticsInt, err := strconv.Atoi(logStatistics); err == nil {
			Logger.Print("Overriding logStatistics with environment variable: " + prefix + "LOG_STATISTICS" + " with value: " + logStatistics)
			config.logStatistics = int32(logStatisticsInt)
		}
	}
	if compressWhenLengthExceeds := os.Getenv(prefix + "COMPRESS_WHEN_LENGTH_EXCEEDS"); compressWhenLengthExceeds != "" {
		if compressWhenLengthExceedsInt, err := strconv.Atoi(compressWhenLengthExceeds); err == nil {
			Logger.Print("Overriding compressWhenLengthExceeds with environment variable: " + prefix + "COMPRESS_WHEN_LENGTH_EXCEEDS" + " with value: " + compressWhenLengthExceeds)
			config.compressWhenLengthExceeds = compressWhenLengthExceedsInt
		}
	}

	return config
}

// Check the configuration. On fail, will terminate the application
func checkConfiguration(result TransferConfiguration) TransferConfiguration {
	Logger.Print("Validating the configuration...")

	// Must have an id
	if result.id == "" {
		Logger.Fatal("Missing required configuration: id")
	}
	if result.nic == "" {
		Logger.Fatal("Missing required configuration: nic")
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
	if result.source != "kafka" && result.source != "random" {
		Logger.Fatalf("Unknown source %s. Legal values are 'kafka' or 'random'.", result.source)
	}
	if result.targetIP == "" {
		Logger.Fatal("Missing required configuration: targetIP")
	}
	if result.targetPort < 0 || result.targetPort > 65535 {
		Logger.Fatal("Invalid configuration: targetPort must be between 0 and 65535")
	}
	if result.source == "kafka" {
		if result.bootstrapServers == "" {
			Logger.Fatal("Missing required configuration: bootstrapServers")
		}
		if result.topic == "" {
			Logger.Fatal("Missing required configuration: topic")
		}
		if result.groupID == "" {
			Logger.Fatal("Missing required configuration: groupID")
		}
	}
	if result.publicKeyFile != "" {
		// Check that the name is a valid file name and that we can open and read from that file
		file, err := os.OpenFile(result.publicKeyFile, os.O_RDONLY, 0644)
		if err != nil {
			Logger.Fatalf("Cannot open public key file '%s' for reading: %v", result.publicKeyFile, err)
		}
		defer file.Close()
		// Check that we can read from that file
		if _, err := file.Stat(); err != nil {
			Logger.Fatalf("Cannot read from public key file '%s': %v", result.publicKeyFile, err)
		}
	}
	if result.source != "kafka" && result.source != "random" {
		Logger.Fatalf("Unknown source %s. Legal values are 'kafka' or 'random'.", result.source)
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
	if result.encryption {
		Logger.Printf("encryption: %t", result.encryption)
		if result.publicKeyFile == "" {
			Logger.Fatalf("Missing required configuration: publicKeyFile")
		}
	}
	if result.encryption && result.generateNewSymmetricKeyEvery < 2 {
		Logger.Fatalf("Error in config generateNewSymmetricKeyEvery. Illegal value: %d. Legal values are integer >= 2", result.generateNewSymmetricKeyEvery)
	}
	if len(result.sendingThreads) == 0 {
		Logger.Fatalf("Error in config sendingThreads. Illegal value: %v. Legal values are an array of objects", result.sendingThreads)
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
	if result.topicTranslations != "" {
		if err := json.Unmarshal([]byte(result.topicTranslations), &result.translations); err != nil {
			Logger.Fatalf("Error in config topicTranslations. Illegal value: %s. Legal values are JSON objects 'from': 'to', ...", result.topicTranslations)
		}
	}
	// Set up a filtering scheme, if configured. You can filter every other, third, fifth message etc
	// by setting filterConfig to a string like "2,3,22,23,42,43". This will send messages 2 and 3
	// of every group of 23 messages. The last group (42,43) is just to verify that the user has
	// entered the correct configuration.
	// If deliverFilter is empty, no filtering is done.
	// The deliverFilter can also be set from the environment variable AIRGAP_UPSTREAM_DELIVER_FILTER
	if result.deliverFilter != "" {
		var err error
		result.filter, err = filter.NewFilter(result.deliverFilter)
		if err != nil {
			Logger.Fatalf("Error in deliverFilter: %v", err)
		}
		Logger.Printf("Filtering is enabled with configuration: %s", result.deliverFilter)
		Logger.Printf("config.filter is ", result.filter)
	} else {
		result.filter = nil
		Logger.Printf("No filtering is enabled.")
	}
	return result
}

func logConfiguration(config TransferConfiguration) {
	Logger.Printf("Configuration:")
	Logger.Printf("  id: %s", config.id)
	Logger.Printf("  nic: %s", config.nic)
	Logger.Printf("  logLevel: %s", config.logLevel)
	Logger.Printf("  logFileName: %s", config.logFileName)
	Logger.Printf("  targetIP: %s", config.targetIP)
	Logger.Printf("  targetPort: %d", config.targetPort)
	Logger.Printf("  bootstrapServers: %s", config.bootstrapServers)
	Logger.Printf("  topic: %s", config.topic)
	Logger.Printf("  groupID: %s", config.groupID)
	Logger.Printf("  payloadSize: %d", config.payloadSize)
	Logger.Printf("  topicTranslations: %s", config.topicTranslations)
	Logger.Printf("  from: %s", config.from)
	Logger.Printf("  encryption: %t", config.encryption)
	if config.publicKeyFile != "" {
		Logger.Printf("  publicKeyFile: %s", config.publicKeyFile)
	}
	Logger.Printf("  source: %s", config.source)
	Logger.Printf("  eps: %f", config.eps)

	Logger.Printf("  generateNewSymmetricKeyEvery: %d seconds", config.generateNewSymmetricKeyEvery)
	if len(config.sendingThreads) > 0 {
		for i, thread := range config.sendingThreads {
			for name, offset := range thread {
				Logger.Printf("  sendingThread[%d]: name=%s, offset=%d seconds", i, name, offset)
			}
		}
	}
	Logger.Printf("  certFile: %s", config.certFile)
	Logger.Printf("  keyFile: %s", config.keyFile)
	Logger.Printf("  caFile: %s", config.caFile)
	Logger.Printf("  deliverFilter: %s", config.deliverFilter)
	Logger.Printf("  logStatistics: %d seconds", config.logStatistics)
	Logger.Printf("  compressWhenLengthExceeds: %d bytes", config.compressWhenLengthExceeds)
}
