package upstream

import (
	"bufio"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sitia.nu/airgap/src/filter"
	"sitia.nu/airgap/src/inputfilter"
	"sitia.nu/airgap/src/internal/pinnedtls"
	"sitia.nu/airgap/src/logging"
)

type TransferConfiguration struct {
	id                           string                   // unique id for this upstream
	nic                          string                   // network interface card
	targetIP                     string                   // target IP address
	targetPort                   int                      // target port
	transport                    string                   // transport protocol: udp or tcp (default: udp)
	bootstrapServers             string                   // Kafka bootstrap servers
	topic                        string                   // Kafka topic
	groupID                      string                   // Kafka group ID
	payloadSize                  uint16                   // Maximum Length of payload before fragmentations starts
	from                         string                   // Start time for reading from Kafka
	encryption                   bool                     // encryption on/off
	key                          []byte                   // symmetric key in use
	newkey                       []byte                   // a new symmetric key, when successfully sent, copy the value to key
	publicKey                    *rsa.PublicKey           // public key for encrypting the symmetric key
	publicKeyFile                string                   // file with the public key
	source                       string                   // source of the messages, kafka or random
	generateNewSymmetricKeyEvery int                      // seconds between key generation
	logFileName                  string                   // log file name, redirect logging to a file from the console
	sendingThreads               []map[string]int         // array of objects with thread names and offsets
	certFile                     string                   // Certificate to use to communicate with Kafka with TLS
	keyFile                      string                   // Key file to use for TLS
	caFile                       string                   // CA file to use for TLS
	keyPasswordFile              string                   // File containing the password to decrypt the key file
	deliverFilter                string                   // filter configuration
	topicTranslations            string                   // topic translations in JSON format
	translations                 map[string]string        // map from input topic to output topic (derived from topicTranslations)
	filter                       *filter.Filter           // filter instance
	inputFilterRules             string                   // input filter rules (file path or inline)
	inputFilterDefaultAction     string                   // default action when no rules match (allow or deny)
	inputFilterTimeout           int                      // regex match timeout in milliseconds (default 100)
	inputFilter                  *inputfilter.InputFilter // input filter instance
	partitionStartValue          int                      // value added to source partition number before sending
	eps                          float64                  // events per second
	logLevel                     string                   // log level: DEBUG, INFO, WARN, ERROR, FATAL
	logStatistics                int32                    // log statistics every n seconds, 0 means no logging
	compressWhenLengthExceeds    int                      // compress messages when length exceeds this value, 0 means no compression
	tcpRetryInterval             int                      // milliseconds between TCP send retries (default 1000)
	tcpRetryTimes                int                      // max TCP send retries, 0 = infinite (default 0)
	tcpRetryErrorLevel           string                   // log level for TCP retry messages (default "WARN")
	tcpTLSEnabled                bool                     // enable TLS for TCP transport (default false)
	tcpTLSCertFile               string                   // client certificate file for TCP mTLS
	tcpTLSKeyFile                string                   // client private key file for TCP mTLS
	tcpTLSKeyPasswordFile        string                   // password file for encrypted TCP TLS key
	tcpTLSCAFile                 string                   // CA certificate file to verify downstream server cert
	tcpTLSServerCNRegex          string                   // regex matched against server certificate CN; allows IP-based connections and load-balanced setups
	tcpTLSCipherSuites           string                   // TLS protocol policy: empty or "TLS1.3" = TLS 1.3 only; comma-separated TLS 1.2 cipher names = TLS 1.2 with those suites
	tcpTLSAuthMode               string
	tcpTLSTrustedKeysDir         string
	tcpTLSRotationEnabled        bool
	tcpTLSPinIdleSeconds         int
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
	config.inputFilterTimeout = 100      // default: 100ms regex timeout
	config.partitionStartValue = 0
	config.tcpRetryInterval = 1000     // default: 1 second between retries
	config.tcpRetryTimes = 0           // default: infinite retries
	config.tcpRetryErrorLevel = "WARN" // default: warn level for retry errors
	config.tcpTLSEnabled = false       // default: plain TCP
	config.tcpTLSCipherSuites = ""     // default: TLS 1.3 enforced
	config.tcpTLSAuthMode = "ca"
	return config
}

// configAttrs builds the semantic attributes shared by all configuration
// records: the mandatory eventName, the setting name and, when known, the
// value the setting was given.
func configAttrs(event string, setting string, value interface{}) logging.Attrs {
	attrs := logging.Event(event, logging.Str("airgap.config.setting", setting))
	if value != nil {
		attrs = attrs.With(logging.Str("airgap.config.value", fmt.Sprintf("%v", value)))
	}
	return attrs
}

// configApplied describes a setting that was read and accepted.
func configApplied(setting string, value interface{}) logging.Attrs {
	return configAttrs(logging.EventConfigApplied, setting, value)
}

// configOverride describes a setting replaced by an environment variable or
// a command line flag, named by source.
func configOverride(setting string, source string, value interface{}) logging.Attrs {
	return configAttrs(logging.EventConfigOverride, setting, value).With(logging.Str("airgap.config.source", source))
}

// configInvalid describes a setting that was rejected.
func configInvalid(setting string, value interface{}) logging.Attrs {
	return configAttrs(logging.EventConfigInvalid, setting, value).With(logging.Failure())
}

// Parse the configuration file and return a TransferConfiguration struct.
func ReadParameters(fileName string, result TransferConfiguration) (TransferConfiguration, error) {
	if fileName == "" {
		// No file, return default configuration
		return result, nil
	}
	Logger.Info(logging.Event(logging.EventConfigLoad,
		logging.Str("file.path", fileName),
	), "Reading configuration from file "+fileName)
	file, err := os.Open(fileName)
	if err != nil {
		return result, fmt.Errorf("read configuration %s: %w", fileName, err)
	}
	defer file.Close()

	Logger.Info(logging.Event(logging.EventConfigLoad,
		logging.Str("file.path", fileName),
	), "Reading configuration from file "+fileName)

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
				return result, fmt.Errorf("Error in config eps. Illegal value: %s. Legal values are float >= -1", value)
			} else {
				result.eps = tmp
				Logger.Infof(configApplied("eps", tmp), "eps: %f", tmp)
			}
		case "id":
			result.id = value
			logging.Logger.SetInstanceID(value)
			Logger.Infof(configApplied("id", value), "id: %s", value)
		case logging.OptionFormat, logging.OptionHash, logging.OptionHashChain,
			logging.OptionSigningCertificate, logging.OptionSigningKey,
			logging.OptionSignerID, logging.OptionSigningEKU:
			if _, err := logging.ApplyOption(key, value); err != nil {
				return result, err
			}
			Logger.Infof(configApplied(key, value), "%s: %s", key, value)
		case "payloadSize":
			if value == "auto" {
				result.payloadSize = 0
			} else {
				tmp, err := strconv.Atoi(value)
				if err != nil {
					return result, fmt.Errorf("Error in config payloadSize. Illegal value: %s. Legal values are 'auto' or a two byte integer", value)
				} else if tmp < 0 || tmp > 65535 {
					return result, fmt.Errorf("Error in config payloadSize. Illegal value: %s. Legal values are 'auto' or 0-65535", value)
				} else {
					result.payloadSize = uint16(tmp)
				}
			}
			Logger.Infof(configApplied("payloadSize", result.payloadSize), "payloadSize: %d", result.payloadSize)
		case "nic":
			result.nic = value
			Logger.Infof(configApplied("nic", value), "nic: %s", value)
		case "topicTranslations":
			// Json like {"inputTopic1":"outputTopic1","inputTopic2":"outputTopic2"}
			result.topicTranslations = value
		case "logFileName":
			result.logFileName = value
			Logger.Infof(configApplied("logFileName", value), "logFileName: %s", value)
		case "targetIP":
			result.targetIP = value
			Logger.Infof(configApplied("targetIP", value), "targetIP: %s", value)
		case "targetPort":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config targetPort. Illegal value: %s. Legal values are 0-65535", value)
			} else {
				if tmp < 0 || tmp > 65535 {
					return result, fmt.Errorf("Error in config targetPort. Illegal value: %s. Legal values are 0-65535", value)
				} else {
					result.targetPort = tmp
				}
			}
			Logger.Infof(configApplied("targetPort", result.targetPort), "targetPort: %d", result.targetPort)
		case "transport":
			transport := strings.ToLower(value)
			if transport != "udp" && transport != "tcp" {
				return result, fmt.Errorf("Error in config transport. Illegal value: %s. Legal values are 'udp' or 'tcp'", value)
			}
			result.transport = transport
			Logger.Infof(configApplied("transport", result.transport), "transport: %s", result.transport)
		case "bootstrapServers":
			result.bootstrapServers = value
			Logger.Infof(configApplied("bootstrapServers", value), "bootstrapServers: %s", value)
		case "topic":
			result.topic = value
			Logger.Infof(configApplied("topic", value), "topic: %s", value)
		case "groupID":
			result.groupID = value
		case "from":
			result.from = value
			Logger.Infof(configApplied("from", value), "from: %s", value)
		case "publicKeyFile":
			result.publicKeyFile = value
			Logger.Infof(configApplied("publicKeyFile", value), "publicKeyFile: %s", value)
		case "source":
			if value == "kafka" || value == "random" {
				result.source = value
			} else {
				return result, fmt.Errorf("Unknown source %s. Legal values are 'kafka' or 'random'.", value)
			}
			Logger.Infof(configApplied("source", value), "source: %s", value)
		case "logLevel":
			result.logLevel = strings.ToUpper(value)
			Logger.Infof(configApplied("logLevel", result.logLevel), "logLevel: %s", result.logLevel)
		case "encryption":
			tmp, err := strconv.ParseBool(value)
			if err != nil {
				return result, fmt.Errorf("Error in config encryption. Illegal value: %s. Legal values are true or false", value)
			} else {
				result.encryption = tmp
			}
			var encryptionStr string
			if result.encryption {
				encryptionStr = "true"
			} else {
				encryptionStr = "false"
			}
			Logger.Infof(configApplied("encryption", encryptionStr), "encryption: %s", encryptionStr)
		case "generateNewSymmetricKeyEvery": // second
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config generateNewSymmetricKeyEvery. Illegal value: %s. Legal values are a four byte integer", value)
			} else {
				if tmp < 2 {
					return result, fmt.Errorf("Error in config generateNewSymmetricKeyEvery. Illegal value: %s. Legal values are integer >= 2", value)
				} else {
					result.generateNewSymmetricKeyEvery = tmp
					Logger.Infof(configApplied("generateNewSymmetricKeyEvery", tmp), "generateNewSymmetricKeyEvery: %d", tmp)
				}
			}
		case "sendingThreads":
			// Remove the old value for result.sendingThreads
			result.sendingThreads = nil
			// Read the new ones
			if err := json.Unmarshal([]byte(value), &result.sendingThreads); err != nil {
				return result, fmt.Errorf("Error in config sendingThreads. Illegal value: %s. Legal values are an array of objects", value)
			}
			Logger.Infof(configApplied("sendingThreads", result.sendingThreads), "sendingThreads: %v", result.sendingThreads)

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
		case "deliverFilter":
			result.deliverFilter = value
			Logger.Infof(configApplied("deliverFilter", value), "deliverFilter: %s", value)
		case "logStatistics":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config logStatistics. Illegal value: %s. Legal values are a non-negative integer", value)
			} else {
				if tmp < 0 {
					return result, fmt.Errorf("Error in config logStatistics. Illegal value: %s. Legal values are a non-negative integer", value)
				} else if tmp > math.MaxInt32 {
					return result, fmt.Errorf("Error in config logStatistics. Illegal value: %s. Legal values are a non-negative integer below %d", value, math.MaxInt32)
				} else {
					// Safe: tmp is checked to fit in int32 above, and Logger.Fatalf terminates execution if not.
					// codeql[incorrect-integer-conversion]: value is checked and fatal error terminates on overflow
					result.logStatistics = int32(tmp)
					Logger.Infof(configApplied("logStatistics", result.logStatistics), "logStatistics: %d", result.logStatistics)
				}
			}
		case "compressWhenLengthExceeds":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config compressWhenLengthExceeds. Illegal value: %s. Legal values are a non-negative integer", value)
			} else {
				if tmp < 0 {
					return result, fmt.Errorf("Error in config compressWhenLengthExceeds. Illegal value: %s. Legal values are a non-negative integer", value)
				} else {
					result.compressWhenLengthExceeds = tmp
					Logger.Infof(configApplied("compressWhenLengthExceeds", result.compressWhenLengthExceeds), "compressWhenLengthExceeds: %d", result.compressWhenLengthExceeds)
				}
			}
		case "inputFilterRules":
			result.inputFilterRules = value
			Logger.Infof(configApplied("inputFilterRules", value), "inputFilterRules: %s", value)
		case "inputFilterDefaultAction":
			result.inputFilterDefaultAction = strings.ToLower(value)
			Logger.Infof(configApplied("inputFilterDefaultAction", result.inputFilterDefaultAction), "inputFilterDefaultAction: %s", result.inputFilterDefaultAction)
		case "inputFilterTimeout":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config inputFilterTimeout. Illegal value: %s. Legal values are a positive integer (milliseconds)", value)
			} else {
				if tmp <= 0 {
					return result, fmt.Errorf("Error in config inputFilterTimeout. Illegal value: %s. Legal values are a positive integer (milliseconds)", value)
				} else {
					result.inputFilterTimeout = tmp
					Logger.Infof(configApplied("inputFilterTimeout", result.inputFilterTimeout), "inputFilterTimeout: %d", result.inputFilterTimeout)
				}
			}
		case "partitionStartValue":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config partitionStartValue. Illegal value: %s. Legal values are a non-negative integer", value)
			} else {
				if tmp < 0 {
					return result, fmt.Errorf("Error in config partitionStartValue. Illegal value: %s. Legal values are a non-negative integer", value)
				} else {
					result.partitionStartValue = tmp
					Logger.Infof(configApplied("partitionStartValue", result.partitionStartValue), "partitionStartValue: %d", result.partitionStartValue)
				}
			}
		case "tcpRetryInterval":
			tmp, err := strconv.Atoi(value)
			if err != nil || tmp <= 0 {
				return result, fmt.Errorf("Error in config tcpRetryInterval. Illegal value: %s. Legal values are a positive integer (milliseconds)", value)
			} else {
				result.tcpRetryInterval = tmp
				Logger.Infof(configApplied("tcpRetryInterval", tmp), "tcpRetryInterval: %d", tmp)
			}
		case "tcpRetryTimes":
			tmp, err := strconv.Atoi(value)
			if err != nil || tmp < 0 {
				return result, fmt.Errorf("Error in config tcpRetryTimes. Illegal value: %s. Legal values are a non-negative integer (0 = infinite)", value)
			} else {
				result.tcpRetryTimes = tmp
				Logger.Infof(configApplied("tcpRetryTimes", tmp), "tcpRetryTimes: %d", tmp)
			}
		case "tcpRetryErrorLevel":
			level := strings.ToUpper(value)
			switch level {
			case "DEBUG", "INFO", "WARN", "ERROR":
				result.tcpRetryErrorLevel = level
				Logger.Infof(configApplied("tcpRetryErrorLevel", level), "tcpRetryErrorLevel: %s", level)
			default:
				return result, fmt.Errorf("Error in config tcpRetryErrorLevel. Illegal value: %s. Legal values are DEBUG, INFO, WARN, ERROR", value)
			}
		case "tcpTLSEnabled":
			tmp, err := strconv.ParseBool(value)
			if err != nil {
				return result, fmt.Errorf("Error in config tcpTLSEnabled. Illegal value: %s. Legal values are true or false", value)
			} else {
				result.tcpTLSEnabled = tmp
				Logger.Infof(configApplied("tcpTLSEnabled", tmp), "tcpTLSEnabled: %t", tmp)
			}
		case "tcpTLSCertFile":
			result.tcpTLSCertFile = value
			Logger.Infof(configApplied("tcpTLSCertFile", value), "tcpTLSCertFile: %s", value)
		case "tcpTLSKeyFile":
			result.tcpTLSKeyFile = value
			Logger.Infof(configApplied("tcpTLSKeyFile", value), "tcpTLSKeyFile: %s", value)
		case "tcpTLSKeyPasswordFile":
			result.tcpTLSKeyPasswordFile = value
		case "tcpTLSCAFile":
			result.tcpTLSCAFile = value
			Logger.Infof(configApplied("tcpTLSCAFile", value), "tcpTLSCAFile: %s", value)
		case "tcpTLSServerName":
			// Legacy name, still accepted
			result.tcpTLSServerCNRegex = value
			Logger.Infof(configApplied("tcpTLSServerCNRegex", value), "tcpTLSServerCNRegex (via tcpTLSServerName): %s", value)
		case "tcpTLSServerCNRegex":
			result.tcpTLSServerCNRegex = value
			Logger.Infof(configApplied("tcpTLSServerCNRegex", value), "tcpTLSServerCNRegex: %s", value)
		case "tcpTLSCipherSuites":
			result.tcpTLSCipherSuites = value
			Logger.Infof(configApplied("tcpTLSCipherSuites", value), "tcpTLSCipherSuites: %s", value)
		case "tcpTLSAuthMode":
			result.tcpTLSAuthMode = value
		case "tcpTLSTrustedKeysDir":
			result.tcpTLSTrustedKeysDir = value
		case "tcpTLSRotationEnabled":
			if value != "true" && value != "false" {
				return result, fmt.Errorf("invalid tcpTLSRotationEnabled; expected true or false")
			}
			result.tcpTLSRotationEnabled = value == "true"
		case "tcpTLSPinIdleSeconds":
			seconds, err := pinnedtls.ParsePinIdleSeconds(value)
			if err != nil {
				return result, err
			}
			result.tcpTLSPinIdleSeconds = seconds
		default:
			return result, fmt.Errorf("Unknown configuration key: %s", key)
		}
	}

	if result.source == "kafka" {
		// Check the sending threads
		if len(result.sendingThreads) == 0 {
			Logger.Info(configApplied("sendingThreads", "[{'now': 0}]"), "No sendingThreads found. Adding default: sendingThreads: [{'now': 0}]")
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

	Logger.Info(logging.Event(logging.EventConfigOverride,
		logging.Str("airgap.config.source", "environment"),
	), "Checking configuration from environment variables...")
	prefix := "AIRGAP_UPSTREAM_"
	if eps := os.Getenv(prefix + "EPS"); eps != "" {
		if epsFloat, err := strconv.ParseFloat(eps, 64); err == nil {
			Logger.Info(configOverride("eps", prefix+"EPS", eps), "Overriding eps with environment variable: "+prefix+"EPS"+" with value: "+eps)
			config.eps = epsFloat
		}
	}

	if id := os.Getenv(prefix + "ID"); id != "" {
		Logger.Info(configOverride("id", prefix+"ID", id), "Overriding id with environment variable: "+prefix+"ID"+" with value: "+id)
		config.id = id
	}

	if nic := os.Getenv(prefix + "NIC"); nic != "" {
		Logger.Info(configOverride("nic", prefix+"NIC", nic), "Overriding nic with environment variable: "+prefix+"NIC"+" with value: "+nic)
		config.nic = nic
	}

	if targetIP := os.Getenv(prefix + "TARGET_IP"); targetIP != "" {
		Logger.Info(configOverride("targetIP", prefix+"TARGET_IP", targetIP), "Overriding targetIP with environment variable: "+prefix+"TARGET_IP"+" with value: "+targetIP)
		config.targetIP = targetIP
	}

	if targetPort := os.Getenv(prefix + "TARGET_PORT"); targetPort != "" {
		if port, err := strconv.Atoi(targetPort); err == nil {
			Logger.Info(configOverride("targetPort", prefix+"TARGET_PORT", targetPort), "Overriding targetPort with environment variable: "+prefix+"TARGET_PORT"+" with value: "+targetPort)
			config.targetPort = port
		}
	}
	if transport := os.Getenv(prefix + "TRANSPORT"); transport != "" {
		transport = strings.ToLower(transport)
		if transport == "udp" || transport == "tcp" {
			Logger.Info(configOverride("transport", prefix+"TRANSPORT", transport), "Overriding transport with environment variable: "+prefix+"TRANSPORT"+" with value: "+transport)
			config.transport = transport
		}
	}
	if bootstrapServers := os.Getenv(prefix + "BOOTSTRAP_SERVERS"); bootstrapServers != "" {
		Logger.Info(configOverride("bootstrapServers", prefix+"BOOTSTRAP_SERVERS", bootstrapServers), "Overriding bootstrapServers with environment variable: "+prefix+"BOOTSTRAP_SERVERS"+" with value: "+bootstrapServers)
		config.bootstrapServers = bootstrapServers
	}
	if topic := os.Getenv(prefix + "TOPIC"); topic != "" {
		Logger.Info(configOverride("topic", prefix+"TOPIC", topic), "Overriding topic with environment variable: "+prefix+"TOPIC"+" with value: "+topic)
		config.topic = topic
	}
	if groupID := os.Getenv(prefix + "GROUP_ID"); groupID != "" {
		Logger.Info(configOverride("groupID", prefix+"GROUP_ID", groupID), "Overriding groupID with environment variable: "+prefix+"GROUP_ID"+" with value: "+groupID)
		config.groupID = groupID
	}
	if payloadSize := os.Getenv(prefix + "PAYLOAD_SIZE"); payloadSize != "" {
		Logger.Info(configOverride("payloadSize", prefix+"PAYLOAD_SIZE", payloadSize), "Overriding payloadSize with environment variable: "+prefix+"PAYLOAD_SIZE"+" with value: "+payloadSize)
		if payloadSize == "auto" {
			config.payloadSize = 0
		} else if payloadSizeInt, err := strconv.Atoi(payloadSize); err == nil {
			if payloadSizeInt < 0 || payloadSizeInt > 65535 {
				Logger.Fatalf(configInvalid("PAYLOAD_SIZE", payloadSize), "Error in config PAYLOAD_SIZE. Illegal value: %s. Legal values are 'auto' or 0-65535", payloadSize)
				os.Exit(1)
			}
			// Safe: payloadSizeInt is checked to fit in uint16 above, and Logger.Fatalf terminates execution if not.
			// codeql[incorrect-integer-conversion]: value is checked and fatal error terminates on overflow
			config.payloadSize = uint16(payloadSizeInt)
		}
	}
	if topicTranslations := os.Getenv(prefix + "TOPIC_TRANSLATIONS"); topicTranslations != "" {
		Logger.Info(configOverride("topicTranslations", prefix+"TOPIC_TRANSLATIONS", topicTranslations), "Overriding topicTranslations with environment variable: "+prefix+"TOPIC_TRANSLATIONS"+" with value: "+topicTranslations)
		config.topicTranslations = topicTranslations
	}
	if from := os.Getenv(prefix + "FROM"); from != "" {
		Logger.Info(configOverride("from", prefix+"FROM", from), "Overriding from with environment variable: "+prefix+"FROM"+" with value: "+from)
		config.from = from
	}
	if encryption := os.Getenv(prefix + "ENCRYPTION"); encryption != "" {
		Logger.Info(configOverride("encryption", prefix+"ENCRYPTION", encryption), "Overriding encryption with environment variable: "+prefix+"ENCRYPTION"+" with value: "+encryption)
		config.encryption = encryption == "true"
	}
	if publicKeyFile := os.Getenv(prefix + "PUBLIC_KEY_FILE"); publicKeyFile != "" {
		Logger.Info(configOverride("publicKeyFile", prefix+"PUBLIC_KEY_FILE", publicKeyFile), "Overriding publicKeyFile with environment variable: "+prefix+"PUBLIC_KEY_FILE"+" with value: "+publicKeyFile)
		config.publicKeyFile = publicKeyFile
	}
	if generateNewSymmetricKeyEvery := os.Getenv(prefix + "GENERATE_NEW_SYMMETRIC_KEY_EVERY"); generateNewSymmetricKeyEvery != "" {
		if generateNewSymmetricKeyEveryInt, err := strconv.Atoi(generateNewSymmetricKeyEvery); err == nil {
			Logger.Info(configOverride("generateNewSymmetricKeyEvery", prefix+"GENERATE_NEW_SYMMETRIC_KEY_EVERY", generateNewSymmetricKeyEvery), "Overriding generateNewSymmetricKeyEvery with environment variable: "+prefix+"GENERATE_NEW_SYMMETRIC_KEY_EVERY"+" with value: "+generateNewSymmetricKeyEvery)
			config.generateNewSymmetricKeyEvery = generateNewSymmetricKeyEveryInt
		}
	}
	if logLevel := os.Getenv(prefix + "LOG_LEVEL"); logLevel != "" {
		Logger.Info(configOverride("logLevel", prefix+"LOG_LEVEL", logLevel), "Overriding logLevel with environment variable: "+prefix+"LOG_LEVEL"+" with value: "+logLevel)
		config.logLevel = logLevel
	}
	if logFileName := os.Getenv(prefix + "LOG_FILE_NAME"); logFileName != "" {
		Logger.Info(configOverride("logFileName", prefix+"LOG_FILE_NAME", logFileName), "Overriding logFileName with environment variable: "+prefix+"LOG_FILE_NAME"+" with value: "+logFileName)
		config.logFileName = logFileName
	}
	if err := logging.ApplyEnvironmentOptions(prefix); err != nil {
		Logger.Fatalf(configInvalid("loggingOptions", prefix).With(logging.ErrorType(err)), "%v", err)
	}
	if sendingThreads := os.Getenv(prefix + "SENDING_THREADS"); sendingThreads != "" {
		Logger.Info(configOverride("sendingThreads", prefix+"SENDING_THREADS", sendingThreads), "Overriding sendingThreads with environment variable: "+prefix+"SENDING_THREADS"+" with value: "+sendingThreads)
		if err := json.Unmarshal([]byte(sendingThreads), &config.sendingThreads); err != nil {
			Logger.Fatalf(configInvalid("sendingThreads", sendingThreads).With(logging.ErrorType(err)), "Error parsing SENDING_THREADS: %v", err)
		}
	}
	if certFile := os.Getenv(prefix + "CERT_FILE"); certFile != "" {
		Logger.Info(configOverride("certFile", prefix+"CERT_FILE", certFile), "Overriding certFile with environment variable: "+prefix+"CERT_FILE"+" with value: "+certFile)
		config.certFile = certFile
	}
	if keyFile := os.Getenv(prefix + "KEY_FILE"); keyFile != "" {
		Logger.Info(configOverride("keyFile", prefix+"KEY_FILE", keyFile), "Overriding keyFile with environment variable: "+prefix+"KEY_FILE"+" with value: "+keyFile)
		config.keyFile = keyFile
	}
	if caFile := os.Getenv(prefix + "CA_FILE"); caFile != "" {
		Logger.Info(configOverride("caFile", prefix+"CA_FILE", caFile), "Overriding caFile with environment variable: "+prefix+"CA_FILE"+" with value: "+caFile)
		config.caFile = caFile
	}
	if keyPasswordFile := os.Getenv(prefix + "KEY_PASSWORD_FILE"); keyPasswordFile != "" {
		config.keyPasswordFile = keyPasswordFile
	}
	if source := os.Getenv(prefix + "SOURCE"); source != "" {
		Logger.Info(configOverride("source", prefix+"SOURCE", source), "Overriding source with environment variable: "+prefix+"SOURCE"+" with value: "+source)
		config.source = source
	}
	if deliverFilter := os.Getenv(prefix + "DELIVER_FILTER"); deliverFilter != "" {
		Logger.Info(configOverride("deliverFilter", prefix+"DELIVER_FILTER", deliverFilter), "Overriding deliverFilter with environment variable: "+prefix+"DELIVER_FILTER"+" with value: "+deliverFilter)
		config.deliverFilter = deliverFilter
	}
	if logStatistics := os.Getenv(prefix + "LOG_STATISTICS"); logStatistics != "" {
		if logStatisticsInt, err := strconv.Atoi(logStatistics); err == nil {
			if logStatisticsInt < 0 || logStatisticsInt > math.MaxInt32 {
				Logger.Fatalf(configInvalid("LOG_STATISTICS", logStatistics), "Error in config LOG_STATISTICS. Illegal value: %s. Legal values are a non-negative integer below %d", logStatistics, math.MaxInt32)
				os.Exit(1)
			}
			Logger.Info(configOverride("logStatistics", prefix+"LOG_STATISTICS", logStatistics), "Overriding logStatistics with environment variable: "+prefix+"LOG_STATISTICS"+" with value: "+logStatistics)
			// Safe: logStatisticsInt is checked to fit in int32 above, and Logger.Fatalf terminates execution if not.
			// codeql[incorrect-integer-conversion]: value is checked and fatal error terminates on overflow
			config.logStatistics = int32(logStatisticsInt)
		}
	}
	if compressWhenLengthExceeds := os.Getenv(prefix + "COMPRESS_WHEN_LENGTH_EXCEEDS"); compressWhenLengthExceeds != "" {
		if compressWhenLengthExceedsInt, err := strconv.Atoi(compressWhenLengthExceeds); err == nil {
			Logger.Info(configOverride("compressWhenLengthExceeds", prefix+"COMPRESS_WHEN_LENGTH_EXCEEDS", compressWhenLengthExceeds), "Overriding compressWhenLengthExceeds with environment variable: "+prefix+"COMPRESS_WHEN_LENGTH_EXCEEDS"+" with value: "+compressWhenLengthExceeds)
			config.compressWhenLengthExceeds = compressWhenLengthExceedsInt
		}
	}
	if inputFilterRules := os.Getenv(prefix + "INPUT_FILTER_RULES"); inputFilterRules != "" {
		Logger.Info(configOverride("inputFilterRules", prefix+"INPUT_FILTER_RULES", inputFilterRules), "Overriding inputFilterRules with environment variable: "+prefix+"INPUT_FILTER_RULES"+" with value: "+inputFilterRules)
		config.inputFilterRules = inputFilterRules
	}
	if inputFilterDefaultAction := os.Getenv(prefix + "INPUT_FILTER_DEFAULT_ACTION"); inputFilterDefaultAction != "" {
		Logger.Info(configOverride("inputFilterDefaultAction", prefix+"INPUT_FILTER_DEFAULT_ACTION", inputFilterDefaultAction), "Overriding inputFilterDefaultAction with environment variable: "+prefix+"INPUT_FILTER_DEFAULT_ACTION"+" with value: "+inputFilterDefaultAction)
		config.inputFilterDefaultAction = strings.ToLower(inputFilterDefaultAction)
	}
	if inputFilterTimeout := os.Getenv(prefix + "INPUT_FILTER_TIMEOUT"); inputFilterTimeout != "" {
		if inputFilterTimeoutInt, err := strconv.Atoi(inputFilterTimeout); err == nil {
			if inputFilterTimeoutInt > 0 {
				Logger.Info(configOverride("inputFilterTimeout", prefix+"INPUT_FILTER_TIMEOUT", inputFilterTimeout), "Overriding inputFilterTimeout with environment variable: "+prefix+"INPUT_FILTER_TIMEOUT"+" with value: "+inputFilterTimeout)
				config.inputFilterTimeout = inputFilterTimeoutInt
			}
		}
	}
	if partitionStartValue := os.Getenv(prefix + "PARTITION_START_VALUE"); partitionStartValue != "" {
		if partitionStartValueInt, err := strconv.Atoi(partitionStartValue); err == nil {
			if partitionStartValueInt < 0 {
				Logger.Fatalf(configInvalid("partitionStartValue", partitionStartValue).With(logging.Str("airgap.config.source", prefix+"PARTITION_START_VALUE")), "Error in environment variable AIRGAP_UPSTREAM_PARTITION_START_VALUE. Illegal value: %s. Legal values are a non-negative integer", partitionStartValue)
			}
			Logger.Info(configOverride("partitionStartValue", prefix+"PARTITION_START_VALUE", partitionStartValue), "Overriding partitionStartValue with environment variable: "+prefix+"PARTITION_START_VALUE"+" with value: "+partitionStartValue)
			config.partitionStartValue = partitionStartValueInt
		}
	}
	if tcpRetryInterval := os.Getenv(prefix + "TCP_RETRY_INTERVAL"); tcpRetryInterval != "" {
		if v, err := strconv.Atoi(tcpRetryInterval); err == nil && v > 0 {
			Logger.Info(configOverride("tcpRetryInterval", prefix+"TCP_RETRY_INTERVAL", tcpRetryInterval), "Overriding tcpRetryInterval with environment variable: "+prefix+"TCP_RETRY_INTERVAL"+" with value: "+tcpRetryInterval)
			config.tcpRetryInterval = v
		}
	}
	if tcpRetryTimes := os.Getenv(prefix + "TCP_RETRY_TIMES"); tcpRetryTimes != "" {
		if v, err := strconv.Atoi(tcpRetryTimes); err == nil && v >= 0 {
			Logger.Info(configOverride("tcpRetryTimes", prefix+"TCP_RETRY_TIMES", tcpRetryTimes), "Overriding tcpRetryTimes with environment variable: "+prefix+"TCP_RETRY_TIMES"+" with value: "+tcpRetryTimes)
			config.tcpRetryTimes = v
		}
	}
	if tcpRetryErrorLevel := os.Getenv(prefix + "TCP_RETRY_ERROR_LEVEL"); tcpRetryErrorLevel != "" {
		level := strings.ToUpper(tcpRetryErrorLevel)
		switch level {
		case "DEBUG", "INFO", "WARN", "ERROR":
			Logger.Info(configOverride("tcpRetryErrorLevel", prefix+"TCP_RETRY_ERROR_LEVEL", tcpRetryErrorLevel), "Overriding tcpRetryErrorLevel with environment variable: "+prefix+"TCP_RETRY_ERROR_LEVEL"+" with value: "+tcpRetryErrorLevel)
			config.tcpRetryErrorLevel = level
		default:
			Logger.Fatalf(configInvalid("tcpRetryErrorLevel", tcpRetryErrorLevel).With(logging.Str("airgap.config.source", prefix+"TCP_RETRY_ERROR_LEVEL")), "Error in environment variable AIRGAP_UPSTREAM_TCP_RETRY_ERROR_LEVEL. Illegal value: %s. Legal values are DEBUG, INFO, WARN, ERROR", tcpRetryErrorLevel)
		}
	}
	if v := os.Getenv(prefix + "TCP_TLS_ENABLED"); v != "" {
		tmp, err := strconv.ParseBool(v)
		if err != nil {
			Logger.Fatalf(configInvalid("tcpTLSEnabled", v).With(logging.Str("airgap.config.source", prefix+"TCP_TLS_ENABLED")), "Error in environment variable %sTCP_TLS_ENABLED. Illegal value: %s. Legal values are true or false", prefix, v)
		}
		Logger.Info(configOverride("tcpTLSEnabled", prefix+"TCP_TLS_ENABLED", v), "Overriding tcpTLSEnabled with environment variable: "+prefix+"TCP_TLS_ENABLED with value: "+v)
		config.tcpTLSEnabled = tmp
	}
	if v := os.Getenv(prefix + "TCP_TLS_CERT_FILE"); v != "" {
		Logger.Info(configOverride("tcpTLSCertFile", prefix+"TCP_TLS_CERT_FILE", v), "Overriding tcpTLSCertFile with environment variable: "+prefix+"TCP_TLS_CERT_FILE with value: "+v)
		config.tcpTLSCertFile = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_KEY_FILE"); v != "" {
		Logger.Info(configOverride("tcpTLSKeyFile", prefix+"TCP_TLS_KEY_FILE", v), "Overriding tcpTLSKeyFile with environment variable: "+prefix+"TCP_TLS_KEY_FILE with value: "+v)
		config.tcpTLSKeyFile = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_KEY_PASSWORD_FILE"); v != "" {
		Logger.Info(configOverride("tcpTLSKeyPasswordFile", prefix+"TCP_TLS_KEY_PASSWORD_FILE", nil), "Overriding tcpTLSKeyPasswordFile with environment variable: "+prefix+"TCP_TLS_KEY_PASSWORD_FILE")
		config.tcpTLSKeyPasswordFile = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_CA_FILE"); v != "" {
		Logger.Info(configOverride("tcpTLSCAFile", prefix+"TCP_TLS_CA_FILE", v), "Overriding tcpTLSCAFile with environment variable: "+prefix+"TCP_TLS_CA_FILE with value: "+v)
		config.tcpTLSCAFile = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_SERVER_NAME"); v != "" {
		Logger.Info(configOverride("tcpTLSServerCNRegex", prefix+"TCP_TLS_SERVER_NAME", v), "Overriding tcpTLSServerCNRegex with environment variable: "+prefix+"TCP_TLS_SERVER_NAME with value: "+v)
		config.tcpTLSServerCNRegex = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_SERVER_CN_REGEX"); v != "" {
		Logger.Info(configOverride("tcpTLSServerCNRegex", prefix+"TCP_TLS_SERVER_CN_REGEX", v), "Overriding tcpTLSServerCNRegex with environment variable: "+prefix+"TCP_TLS_SERVER_CN_REGEX with value: "+v)
		config.tcpTLSServerCNRegex = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_CIPHER_SUITES"); v != "" {
		Logger.Info(configOverride("tcpTLSCipherSuites", prefix+"TCP_TLS_CIPHER_SUITES", v), "Overriding tcpTLSCipherSuites with environment variable: "+prefix+"TCP_TLS_CIPHER_SUITES with value: "+v)
		config.tcpTLSCipherSuites = v
	}
	if v, present := os.LookupEnv(prefix + "TCP_TLS_AUTH_MODE"); present {
		config.tcpTLSAuthMode = v
	}
	if v, present := os.LookupEnv(prefix + "TCP_TLS_PIN_IDLE_SECONDS"); present {
		seconds, err := pinnedtls.ParsePinIdleSeconds(v)
		if err != nil {
			Logger.Fatal(configInvalid("tcpTLSPinIdleSeconds", v).With(logging.Str("airgap.config.source", prefix+"TCP_TLS_PIN_IDLE_SECONDS")), err)
		}
		config.tcpTLSPinIdleSeconds = seconds
	}
	if v, present := os.LookupEnv(prefix + "TCP_TLS_ROTATION_ENABLED"); present {
		if v != "true" && v != "false" {
			Logger.Fatal(configInvalid("tcpTLSRotationEnabled", v).With(logging.Str("airgap.config.source", prefix+"TCP_TLS_ROTATION_ENABLED")), "invalid TCP_TLS_ROTATION_ENABLED; expected true or false")
		}
		config.tcpTLSRotationEnabled = v == "true"
	}
	if v, present := os.LookupEnv(prefix + "TCP_TLS_TRUSTED_KEYS_DIR"); present {
		config.tcpTLSTrustedKeysDir = v
	}

	return config
}

// parseCommandLineOverrides parses --name=value arguments and applies them to the config struct.
func parseCommandLineOverrides(args []string, config TransferConfiguration) TransferConfiguration {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		kv := strings.SplitN(arg[2:], "=", 2)
		if len(kv) != 2 {
			Logger.Warnf(configInvalid("commandLineOverride", arg).With(logging.Str("airgap.config.source", "command-line")), "Ignoring malformed command line override: %s", arg)
			continue
		}
		key := kv[0]
		value := kv[1]
		switch key {
		case "id":
			config.id = value
			logging.Logger.SetInstanceID(value)
		case logging.OptionFormat, logging.OptionHash, logging.OptionHashChain,
			logging.OptionSigningCertificate, logging.OptionSigningKey,
			logging.OptionSignerID, logging.OptionSigningEKU:
			if _, err := logging.ApplyOption(key, value); err != nil {
				Logger.Fatal(configInvalid(key, value).With(logging.Str("airgap.config.source", "command-line")), err)
			}
		case "nic":
			config.nic = value
		case "targetIP":
			config.targetIP = value
		case "targetPort":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 || v > 65535 {
				Logger.Fatalf(configInvalid("targetPort", value), "Error parsing --targetPort: %s", value)
			}
			config.targetPort = v
		case "transport":
			t := strings.ToLower(value)
			if t != "udp" && t != "tcp" {
				Logger.Fatalf(configInvalid("transport", value).With(logging.Str("airgap.config.source", "command-line")), "Unknown --transport %s. Legal values are: udp, tcp", value)
			}
			config.transport = t
		case "bootstrapServers":
			config.bootstrapServers = value
		case "topic":
			config.topic = value
		case "groupID":
			config.groupID = value
		case "payloadSize":
			if value == "auto" {
				config.payloadSize = 0
			} else {
				v, err := strconv.Atoi(value)
				if err != nil || v < 0 || v > 65535 {
					Logger.Fatalf(configInvalid("payloadSize", value), "Error parsing --payloadSize: %s", value)
				}
				config.payloadSize = uint16(v) // Safe: bounds checked above [0, 65535]
			}
		case "topicTranslations":
			config.topicTranslations = value
		case "from":
			config.from = value
		case "encryption":
			config.encryption = strings.ToLower(value) == "true"
		case "publicKeyFile":
			config.publicKeyFile = value
		case "generateNewSymmetricKeyEvery":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				Logger.Fatalf(configInvalid("generateNewSymmetricKeyEvery", value), "Error parsing --generateNewSymmetricKeyEvery: %s", value)
			}
			config.generateNewSymmetricKeyEvery = v
		case "logLevel":
			config.logLevel = strings.ToUpper(value)
		case "logFileName":
			config.logFileName = value
		case "sendingThreads":
			if err := json.Unmarshal([]byte(value), &config.sendingThreads); err != nil {
				Logger.Fatalf(configInvalid("sendingThreads", err), "Error parsing --sendingThreads: %v", err)
			}
		case "certFile":
			config.certFile = value
		case "keyFile":
			config.keyFile = value
		case "caFile":
			config.caFile = value
		case "keyPasswordFile":
			config.keyPasswordFile = value
		case "source":
			config.source = value
		case "deliverFilter":
			config.deliverFilter = value
		case "logStatistics":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				Logger.Fatalf(configInvalid("logStatistics", value), "Error parsing --logStatistics: %s", value)
			}
			if v > math.MaxInt32 {
				Logger.Fatalf(configInvalid("logStatistics", v), "Error parsing --logStatistics: value %d exceeds max int32", v)
			}
			config.logStatistics = int32(v) // Safe: bounds checked above
		case "compressWhenLengthExceeds":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				Logger.Fatalf(configInvalid("compressWhenLengthExceeds", value), "Error parsing --compressWhenLengthExceeds: %s", value)
			}
			config.compressWhenLengthExceeds = v
		case "inputFilterRules":
			config.inputFilterRules = value
		case "inputFilterDefaultAction":
			config.inputFilterDefaultAction = strings.ToLower(value)
		case "inputFilterTimeout":
			v, err := strconv.Atoi(value)
			if err != nil || v <= 0 {
				Logger.Fatalf(configInvalid("inputFilterTimeout", value), "Error parsing --inputFilterTimeout: %s", value)
			}
			config.inputFilterTimeout = v
		case "partitionStartValue":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				Logger.Fatalf(configInvalid("partitionStartValue", value), "Error parsing --partitionStartValue: %s", value)
			}
			config.partitionStartValue = v
		case "eps":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || v < -1 {
				Logger.Fatalf(configInvalid("eps", value), "Error parsing --eps: %s. Legal values are float >= -1", value)
			}
			config.eps = v
		case "tcpRetryInterval":
			v, err := strconv.Atoi(value)
			if err != nil || v <= 0 {
				Logger.Fatalf(configInvalid("tcpRetryInterval", value), "Error parsing --tcpRetryInterval: %s", value)
			}
			config.tcpRetryInterval = v
		case "tcpRetryTimes":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				Logger.Fatalf(configInvalid("tcpRetryTimes", value), "Error parsing --tcpRetryTimes: %s", value)
			}
			config.tcpRetryTimes = v
		case "tcpRetryErrorLevel":
			level := strings.ToUpper(value)
			switch level {
			case "DEBUG", "INFO", "WARN", "ERROR":
				config.tcpRetryErrorLevel = level
			default:
				Logger.Fatalf(configInvalid("tcpRetryErrorLevel", value).With(logging.Str("airgap.config.source", "command-line")), "Error in --tcpRetryErrorLevel. Illegal value: %s. Legal values are DEBUG, INFO, WARN, ERROR", value)
			}
		case "tcpTLSEnabled":
			v, err := strconv.ParseBool(value)
			if err != nil {
				Logger.Fatalf(configInvalid("tcpTLSEnabled", value), "Error parsing --tcpTLSEnabled: %s. Legal values are true or false", value)
			}
			config.tcpTLSEnabled = v
		case "tcpTLSCertFile":
			config.tcpTLSCertFile = value
		case "tcpTLSKeyFile":
			config.tcpTLSKeyFile = value
		case "tcpTLSKeyPasswordFile":
			config.tcpTLSKeyPasswordFile = value
		case "tcpTLSCAFile":
			config.tcpTLSCAFile = value
		case "tcpTLSServerName", "tcpTLSServerCNRegex":
			config.tcpTLSServerCNRegex = value
		case "tcpTLSCipherSuites":
			config.tcpTLSCipherSuites = value
		case "tcpTLSAuthMode":
			config.tcpTLSAuthMode = value
		case "tcpTLSTrustedKeysDir":
			config.tcpTLSTrustedKeysDir = value
		case "tcpTLSRotationEnabled":
			if value != "true" && value != "false" {
				Logger.Fatal(configInvalid("tcpTLSRotationEnabled", value).With(logging.Str("airgap.config.source", "command-line")), "invalid --tcpTLSRotationEnabled; expected true or false")
			}
			config.tcpTLSRotationEnabled = value == "true"
		case "tcpTLSPinIdleSeconds":
			seconds, err := pinnedtls.ParsePinIdleSeconds(value)
			if err != nil {
				Logger.Fatal(configInvalid("tcpTLSPinIdleSeconds", value).With(logging.Str("airgap.config.source", "command-line")), err)
			}
			config.tcpTLSPinIdleSeconds = seconds
		default:
			Logger.Warnf(configInvalid(key, value).With(logging.Str("airgap.config.source", "command-line")), "Unknown command line override: --%s", key)
		}
	}
	return config
}

// Check the configuration. On fail, will terminate the application
func checkConfiguration(result TransferConfiguration) TransferConfiguration {
	Logger.Info(logging.Event(logging.EventConfigParse), "Validating the configuration...")

	// Must have an id
	if result.id == "" {
		Logger.Fatal(configInvalid("id", nil), "Missing required configuration: id")
	}
	if result.nic == "" {
		Logger.Fatal(configInvalid("nic", nil), "Missing required configuration: nic")
	}
	if result.logFileName != "" {
		// Check that the logFileName is a valid file name
		file, err := os.OpenFile(result.logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			Logger.Fatalf(configInvalid("logFileName", result.logFileName).With(logging.Str("file.path", result.logFileName), logging.ErrorType(err)), "Cannot open log file '%s' for writing: %v", result.logFileName, err)
		}
		defer file.Close()
		// Check that we can write to that file
		if _, err := file.WriteString(""); err != nil {
			Logger.Fatalf(configInvalid("logFileName", result.logFileName).With(logging.Str("file.path", result.logFileName), logging.ErrorType(err)), "Cannot write to log file '%s': %v", result.logFileName, err)
		}
	}
	if result.source != "kafka" && result.source != "random" {
		Logger.Fatalf(configInvalid("source", result.source), "Unknown source %s. Legal values are 'kafka' or 'random'.", result.source)
	}
	if result.targetIP == "" {
		Logger.Fatal(configInvalid("targetIP", nil), "Missing required configuration: targetIP")
	}
	if result.targetPort < 0 || result.targetPort > 65535 {
		Logger.Fatalf(configInvalid("targetPort", result.targetPort), "Invalid targetPort: %d. Legal values are 0-65535", result.targetPort)
	}
	// Set default transport if not specified
	if result.transport == "" {
		result.transport = "udp"
		Logger.Info(configApplied("transport", "udp"), "transport not specified, using default: udp")
	}
	if result.source == "kafka" {
		if result.bootstrapServers == "" {
			Logger.Fatal(configInvalid("bootstrapServers", nil), "Missing required configuration: bootstrapServers")
		}
		if result.topic == "" {
			Logger.Fatal(configInvalid("topic", nil), "Missing required configuration: topic")
		}
		if result.groupID == "" {
			Logger.Fatal(configInvalid("groupID", nil), "Missing required configuration: groupID")
		}
	}
	if result.publicKeyFile != "" {
		// Check that the name is a valid file name and that we can open and read from that file
		file, err := os.OpenFile(result.publicKeyFile, os.O_RDONLY, 0644)
		if err != nil {
			Logger.Fatalf(configInvalid("publicKeyFile", result.publicKeyFile).With(logging.Str("file.path", result.publicKeyFile), logging.ErrorType(err)), "Cannot open public key file '%s' for reading: %v", result.publicKeyFile, err)
		}
		defer file.Close()
		// Check that we can read from that file
		if _, err := file.Stat(); err != nil {
			Logger.Fatalf(configInvalid("publicKeyFile", result.publicKeyFile).With(logging.Str("file.path", result.publicKeyFile), logging.ErrorType(err)), "Cannot read from public key file '%s': %v", result.publicKeyFile, err)
		}
	}
	if result.source != "kafka" && result.source != "random" {
		Logger.Fatalf(configInvalid("source", result.source), "Unknown source %s. Legal values are 'kafka' or 'random'.", result.source)
	}
	if result.logLevel != "" {
		Logger.SetLogLevel(result.logLevel)
		var tmp = Logger.GetLogLevel()
		if (tmp == result.logLevel) == false {
			Logger.Fatalf(configInvalid("logLevel", result.logLevel), "Error in config logLevel. Illegal value: %s. Legal values are TRACE, DEBUG, INFO, WARN, ERROR, FATAL", result.logLevel)
		} else {
			Logger.Infof(configApplied("logLevel", tmp), "logLevel: %s", tmp)
		}
	}
	if result.encryption {
		Logger.Infof(configApplied("encryption", result.encryption), "encryption: %t", result.encryption)
		if result.publicKeyFile == "" {
			Logger.Fatalf(configInvalid("publicKeyFile", nil), "Missing required configuration: publicKeyFile")
		}
	}
	if result.encryption && result.generateNewSymmetricKeyEvery < 2 {
		Logger.Fatalf(configInvalid("generateNewSymmetricKeyEvery", result.generateNewSymmetricKeyEvery), "Error in config generateNewSymmetricKeyEvery. Illegal value: %d. Legal values are integer >= 2", result.generateNewSymmetricKeyEvery)
	}
	if len(result.sendingThreads) == 0 {
		Logger.Fatalf(configInvalid("sendingThreads", result.sendingThreads), "Error in config sendingThreads. Illegal value: %v. Legal values are an array of objects", result.sendingThreads)
	}

	// if one of certFile, keyFile or caFile is given, they all must be
	if result.certFile != "" || result.keyFile != "" || result.caFile != "" {
		if result.certFile == "" {
			Logger.Fatalf(configInvalid("certFile", nil), "Missing required configuration: certFile")
		}
		if result.keyFile == "" {
			Logger.Fatalf(configInvalid("keyFile", nil), "Missing required configuration: keyFile")
		}
		if result.caFile == "" {
			Logger.Fatalf(configInvalid("caFile", nil), "Missing required configuration: caFile")
		}
	}
	if result.keyPasswordFile != "" && (result.keyFile != "" || result.certFile != "" || result.caFile != "") {
		// Make sure this file exists and is readable. If not, print the complete file path for easier debugging
		absPath, _ := filepath.Abs(result.keyPasswordFile)
		file, err := os.Open(result.keyPasswordFile)
		if err != nil {
			Logger.Fatalf(configInvalid("keyPasswordFile", result.keyPasswordFile).Merge(logging.Err(err)), "Cannot open keyPasswordFile (absolute path: '%s'): %v", absPath, err)
		}
		defer file.Close()
	}
	if result.topicTranslations != "" {
		if err := json.Unmarshal([]byte(result.topicTranslations), &result.translations); err != nil {
			Logger.Fatalf(configInvalid("topicTranslations", result.topicTranslations), "Error in config topicTranslations. Illegal value: %s. Legal values are JSON objects 'from': 'to', ...", result.topicTranslations)
		}
	}
	if result.partitionStartValue < 0 {
		Logger.Fatalf(configInvalid("partitionStartValue", result.partitionStartValue), "Error in config partitionStartValue. Illegal value: %d. Legal values are a non-negative integer", result.partitionStartValue)
	}
	if result.tcpRetryInterval <= 0 {
		Logger.Fatalf(configInvalid("tcpRetryInterval", result.tcpRetryInterval), "Error in config tcpRetryInterval. Illegal value: %d. Legal values are a positive integer (milliseconds)", result.tcpRetryInterval)
	}
	if result.tcpRetryTimes < 0 {
		Logger.Fatalf(configInvalid("tcpRetryTimes", result.tcpRetryTimes), "Error in config tcpRetryTimes. Illegal value: %d. Legal values are a non-negative integer (0 = infinite)", result.tcpRetryTimes)
	}
	switch result.tcpRetryErrorLevel {
	case "DEBUG", "INFO", "WARN", "ERROR":
		// valid
	default:
		Logger.Fatalf(configInvalid("tcpRetryErrorLevel", result.tcpRetryErrorLevel), "Error in config tcpRetryErrorLevel. Illegal value: %s. Legal values are DEBUG, INFO, WARN, ERROR", result.tcpRetryErrorLevel)
	}
	// Validate TCP TLS configuration
	if err := result.validateTLSMode(); err != nil {
		Logger.Fatal(configInvalid("tcpTLSAuthMode", result.tcpTLSAuthMode).With(logging.ErrorType(err)), err)
	}
	if result.tcpTLSAuthMode == "pinned" {
		if _, err := result.BuildTLSConfig(); err != nil {
			Logger.Fatal(configInvalid("tcpTLSAuthMode", result.tcpTLSAuthMode).With(logging.Str("file.path", result.tcpTLSTrustedKeysDir), logging.ErrorType(err)), err)
		}
	}
	if result.tcpTLSEnabled {
		if result.transport != "tcp" {
			Logger.Fatal(configInvalid("tcpTLSEnabled", result.tcpTLSEnabled).With(logging.Str("airgap.config.transport", result.transport)), "Error: tcpTLSEnabled=true requires transport=tcp")
		}
		if result.tcpTLSAuthMode != "pinned" && result.tcpTLSCAFile == "" {
			Logger.Fatal(configInvalid("tcpTLSCAFile", nil), "Missing required configuration: tcpTLSCAFile (required when tcpTLSEnabled=true)")
		}
	}
	if result.tcpTLSCertFile != "" || result.tcpTLSKeyFile != "" {
		if result.tcpTLSCertFile == "" {
			Logger.Fatal(configInvalid("tcpTLSCertFile", nil), "Missing required configuration: tcpTLSCertFile (required when tcpTLSKeyFile is set)")
		}
		if result.tcpTLSKeyFile == "" {
			Logger.Fatal(configInvalid("tcpTLSKeyFile", nil), "Missing required configuration: tcpTLSKeyFile (required when tcpTLSCertFile is set)")
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
			Logger.Fatalf(configInvalid("deliverFilter", result.deliverFilter).With(logging.ErrorType(err)), "Error in deliverFilter: %v", err)
		}
		Logger.Infof(configApplied("deliverFilter", result.deliverFilter).With(logging.Bool("airgap.filter.enabled", true)), "Filtering is enabled with configuration: %s", result.deliverFilter)
		Logger.Infof(logging.Event(logging.EventMessageFilter,
			logging.Str("airgap.config.setting", "filter"),
			logging.Str("airgap.config.value", fmt.Sprintf("%v", result.filter)),
		), "config.filter is %v", result.filter)
	} else {
		result.filter = nil
		Logger.Info(logging.Event(logging.EventMessageFilter,
			logging.Str("airgap.config.setting", "deliverFilter"),
			logging.Bool("airgap.filter.enabled", false),
		), "No filtering is enabled.")
	}

	// Set up input filtering based on payload content
	if result.inputFilterRules != "" {
		var err error
		timeout := time.Duration(result.inputFilterTimeout) * time.Millisecond
		result.inputFilter, err = inputfilter.LoadFilterRules(result.inputFilterRules, result.inputFilterDefaultAction, timeout)
		if err != nil {
			Logger.Fatalf(configInvalid("inputFilterRules", result.inputFilterRules).With(logging.Str("file.path", result.inputFilterRules), logging.ErrorType(err)), "Error loading input filter rules: %v", err)
		}
		Logger.Info(logging.Event(logging.EventMessageFilter,
			logging.Str("airgap.config.setting", "inputFilterRules"),
			logging.Str("airgap.config.value", result.inputFilterRules),
			logging.Bool("airgap.filter.enabled", true),
		), "Input filtering is enabled")
		Logger.Info(logging.Event(logging.EventMessageFilter,
			logging.Str("airgap.config.setting", "inputFilterRules"),
			logging.Str("airgap.filter.default_action", result.inputFilterDefaultAction),
		), result.inputFilter.GetRulesSummary())
	} else {
		result.inputFilter = nil
		Logger.Info(logging.Event(logging.EventMessageFilter,
			logging.Str("airgap.config.setting", "inputFilterRules"),
			logging.Bool("airgap.filter.enabled", false),
		), "No input filtering is enabled.")
	}
	Logger.Infof(logging.Event(logging.EventAppStart,
		logging.Int("airgap.process.pid", int64(os.Getpid())),
	), "pid:%d", os.Getpid())

	if result.logFileName != "" {
		if err := Logger.SetLogFile(result.logFileName); err != nil {
			Logger.Fatal(configInvalid("logFileName", result.logFileName).With(logging.Str("file.path", result.logFileName), logging.ErrorType(err)), err)
		}
	}
	result.WarnProductionConfiguration("startup")
	return result
}

func logConfiguration(config TransferConfiguration) {
	Logger.Info(logging.Event(logging.EventConfigLoad), "Configuration:")
	Logger.Infof(configApplied("id", config.id), "  id: %s", config.id)
	Logger.Infof(configApplied("nic", config.nic), "  nic: %s", config.nic)
	Logger.Infof(configApplied("logLevel", config.logLevel), "  logLevel: %s", config.logLevel)
	Logger.Infof(configApplied("logFileName", config.logFileName), "  logFileName: %s", config.logFileName)
	Logger.Infof(configApplied("targetIP", config.targetIP), "  targetIP: %s", config.targetIP)
	Logger.Infof(configApplied("targetPort", config.targetPort), "  targetPort: %d", config.targetPort)
	Logger.Infof(configApplied("transport", config.transport), "  transport: %s", config.transport)
	Logger.Infof(configApplied("bootstrapServers", config.bootstrapServers), "  bootstrapServers: %s", config.bootstrapServers)
	Logger.Infof(configApplied("topic", config.topic), "  topic: %s", config.topic)
	Logger.Infof(configApplied("groupID", config.groupID), "  groupID: %s", config.groupID)
	Logger.Infof(configApplied("payloadSize", config.payloadSize), "  payloadSize: %d", config.payloadSize)
	Logger.Infof(configApplied("topicTranslations", config.topicTranslations), "  topicTranslations: %s", config.topicTranslations)
	Logger.Infof(configApplied("from", config.from), "  from: %s", config.from)
	Logger.Infof(configApplied("encryption", config.encryption), "  encryption: %t", config.encryption)
	if config.publicKeyFile != "" {
		Logger.Infof(configApplied("publicKeyFile", config.publicKeyFile), "  publicKeyFile: %s", config.publicKeyFile)
	}
	Logger.Infof(configApplied("source", config.source), "  source: %s", config.source)
	Logger.Infof(configApplied("eps", config.eps), "  eps: %f", config.eps)

	Logger.Infof(configApplied("generateNewSymmetricKeyEvery", config.generateNewSymmetricKeyEvery), "  generateNewSymmetricKeyEvery: %d seconds", config.generateNewSymmetricKeyEvery)
	if len(config.sendingThreads) > 0 {
		for i, thread := range config.sendingThreads {
			for name, offset := range thread {
				Logger.Infof(configApplied("sendingThread", name), "  sendingThread[%d]: name=%s, offset=%d seconds", i, name, offset)
			}
		}
	}
	Logger.Infof(configApplied("certFile", config.certFile), "  certFile: %s", config.certFile)
	Logger.Infof(configApplied("keyFile", config.keyFile), "  keyFile: %s", config.keyFile)
	Logger.Info(configApplied("keyPasswordFile", "[set]"), "  keyPasswordFile: [set]")
	Logger.Infof(configApplied("caFile", config.caFile), "  caFile: %s", config.caFile)
	Logger.Infof(configApplied("deliverFilter", config.deliverFilter), "  deliverFilter: %s", config.deliverFilter)
	Logger.Infof(configApplied("logStatistics", config.logStatistics), "  logStatistics: %d seconds", config.logStatistics)
	Logger.Infof(configApplied("compressWhenLengthExceeds", config.compressWhenLengthExceeds), "  compressWhenLengthExceeds: %d bytes", config.compressWhenLengthExceeds)
	Logger.Infof(configApplied("inputFilterRules", config.inputFilterRules), "  inputFilterRules: %s", config.inputFilterRules)
	Logger.Infof(configApplied("inputFilterDefaultAction", config.inputFilterDefaultAction), "  inputFilterDefaultAction: %s", config.inputFilterDefaultAction)
	Logger.Infof(configApplied("inputFilterTimeout", config.inputFilterTimeout), "  inputFilterTimeout: %dms", config.inputFilterTimeout)
	Logger.Infof(configApplied("partitionStartValue", config.partitionStartValue), "  partitionStartValue: %d", config.partitionStartValue)
	if config.transport == "tcp" {
		tcpRetryTimesStr := "infinite"
		if config.tcpRetryTimes > 0 {
			tcpRetryTimesStr = strconv.Itoa(config.tcpRetryTimes)
		}
		Logger.Infof(configApplied("tcpRetryInterval", config.tcpRetryInterval), "  tcpRetryInterval: %dms", config.tcpRetryInterval)
		Logger.Infof(configApplied("tcpRetryTimes", tcpRetryTimesStr), "  tcpRetryTimes: %s", tcpRetryTimesStr)
		Logger.Infof(configApplied("tcpRetryErrorLevel", config.tcpRetryErrorLevel), "  tcpRetryErrorLevel: %s", config.tcpRetryErrorLevel)
		Logger.Infof(configApplied("tcpTLSEnabled", config.tcpTLSEnabled), "  tcpTLSEnabled: %t", config.tcpTLSEnabled)
		Logger.Infof(configApplied("tcpTLSAuthMode", config.tcpTLSAuthMode), "  tcpTLSAuthMode: %s", config.tcpTLSAuthMode)
		Logger.Infof(configApplied("tcpTLSRotationEnabled", config.tcpTLSRotationEnabled), "  tcpTLSRotationEnabled: %t", config.tcpTLSRotationEnabled)
		Logger.Infof(configApplied("tcpTLSPinIdleSeconds", config.tcpTLSPinIdleSeconds), "  tcpTLSPinIdleSeconds: %d", config.tcpTLSPinIdleSeconds)
		Logger.Infof(configApplied("tcpTLSTrustedKeysDir", config.tcpTLSTrustedKeysDir), "  tcpTLSTrustedKeysDir: %s", config.tcpTLSTrustedKeysDir)
		if config.tcpTLSEnabled {
			Logger.Infof(configApplied("tcpTLSCertFile", config.tcpTLSCertFile), "  tcpTLSCertFile: %s", config.tcpTLSCertFile)
			Logger.Infof(configApplied("tcpTLSKeyFile", config.tcpTLSKeyFile), "  tcpTLSKeyFile: %s", config.tcpTLSKeyFile)
			Logger.Infof(configApplied("tcpTLSCAFile", config.tcpTLSCAFile), "  tcpTLSCAFile: %s", config.tcpTLSCAFile)
			if config.tcpTLSServerCNRegex != "" {
				Logger.Infof(configApplied("tcpTLSServerCNRegex", config.tcpTLSServerCNRegex), "  tcpTLSServerCNRegex: %s", config.tcpTLSServerCNRegex)
			}
			upper := strings.ToUpper(strings.TrimSpace(config.tcpTLSCipherSuites))
			if upper == "" || upper == "TLS1.3" {
				Logger.Info(configApplied("tcpTLSCipherSuites", "TLS 1.3 (enforced)"), "  tcpTLS protocol: TLS 1.3 (enforced)")
			} else {
				Logger.Infof(configApplied("tcpTLSCipherSuites", config.tcpTLSCipherSuites), "  tcpTLS protocol: TLS 1.2 with cipher suites: %s", config.tcpTLSCipherSuites)
			}
		}
	}
}
