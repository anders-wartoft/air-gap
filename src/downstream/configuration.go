package downstream

import (
	"bufio"
	"crypto/rsa"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A private key, the filename and the hash of the file
// If a file is removed in the OS, it will be removed from
// the list of keys. If it is changed, the new key will be
// loaded and if it's added in the OS, it will be added to
// the list of keys.
type KeyInfo struct {
	privateKey         *rsa.PrivateKey
	privateKeyFilename string
	privateKeyHash     string
}

// TransferConfiguration struct definition
type TransferConfiguration struct {
	id                   string
	nic                  string
	targetIP             string
	targetPort           int
	bootstrapServers     string
	topic                string
	mtu                  uint16
	key                  []byte    // symmetric
	keyInfos             []KeyInfo // array of private keys
	privateKeyGlob       string
	target               string
	transport            string // Transport protocol: udp or tcp (default: udp)
	logLevel             string
	logFileName          string
	certFile             string
	keyFile              string
	caFile               string
	clientID             string
	topicTranslations    string
	translations         map[string]string // map from input topic to output topic
	logStatistics        int32             //  How often to log statistics in seconds. 0 means no logging
	numReceivers         int               // Number of UDP receivers to start
	channelBufferSize    int               // Size of the channel buffer between UDP receiver and Kafka writer
	batchSize            int               // Number of messages to batch together
	readBufferMultiplier uint16            // Multiplier for the read buffer size
	rcvBufSize           int               // Size of the OS receive buffer for UDP sockets
	keyPasswordFile      string            // File containing the password to decrypt the key file
	maximumDecompressSize int
}

// Builder pattern setters for TransferConfiguration
func NewTransferConfiguration() *TransferConfiguration {
	cfg := defaultConfiguration()
	return &cfg
}

func (c *TransferConfiguration) SetID(id string) *TransferConfiguration {
	c.id = id
	return c
}
func (c *TransferConfiguration) SetNic(nic string) *TransferConfiguration {
	c.nic = nic
	return c
}
func (c *TransferConfiguration) SetTargetIP(ip string) *TransferConfiguration {
	c.targetIP = ip
	return c
}
func (c *TransferConfiguration) SetTargetPort(port int) *TransferConfiguration {
	c.targetPort = port
	return c
}
func (c *TransferConfiguration) SetBootstrapServers(bs string) *TransferConfiguration {
	c.bootstrapServers = bs
	return c
}
func (c *TransferConfiguration) SetTopic(topic string) *TransferConfiguration {
	c.topic = topic
	return c
}
func (c *TransferConfiguration) SetMtu(mtu uint16) *TransferConfiguration {
	c.mtu = mtu
	return c
}
func (c *TransferConfiguration) SetKey(key []byte) *TransferConfiguration {
	c.key = key
	return c
}
func (c *TransferConfiguration) SetLogLevel(level string) *TransferConfiguration {
	c.logLevel = level
	return c
}
func (c *TransferConfiguration) SetLogFileName(name string) *TransferConfiguration {
	c.logFileName = name
	return c
}
func (c *TransferConfiguration) SetCertFile(cert string) *TransferConfiguration {
	c.certFile = cert
	return c
}
func (c *TransferConfiguration) SetKeyFile(key string) *TransferConfiguration {
	c.keyFile = key
	return c
}
func (c *TransferConfiguration) SetCaFile(ca string) *TransferConfiguration {
	c.caFile = ca
	return c
}
func (c *TransferConfiguration) SetClientID(id string) *TransferConfiguration {
	c.clientID = id
	return c
}
func (c *TransferConfiguration) SetTopicTranslations(tt string) *TransferConfiguration {
	c.topicTranslations = tt
	return c
}
func (c *TransferConfiguration) SetLogStatistics(stat int32) *TransferConfiguration {
	c.logStatistics = stat
	return c
}
func (c *TransferConfiguration) SetNumReceivers(n int) *TransferConfiguration {
	c.numReceivers = n
	return c
}

func defaultConfiguration() TransferConfiguration {
	config := TransferConfiguration{}
	config.logLevel = "INFO"
	config.id = "default_downstream"
	config.target = "kafka"
	config.transport = "udp" // default UDP for backward compatibility
	config.logFileName = ""
	config.mtu = 0 // default auto
	config.translations = make(map[string]string)
	config.logStatistics = 0            // default no logging
	config.numReceivers = 10            // default ten UDP receiver
	config.channelBufferSize = 16384    // default channel buffer size of 16384 bytes
	config.batchSize = 32               // default batching of 32 messages
	config.readBufferMultiplier = 16    // default 16 times mtu as memory buffer
	config.rcvBufSize = 4 * 1024 * 1024 // default 4MB OS receive buffer for UDP sockets
	config.topic = "airgap-internal"
	config.maximumDecompressSize = 256 * 1024 * 1024 // default 256 MiB
	return config
}

// Read the configuration file and return the configuration
func readConfiguration(fileName string, result TransferConfiguration) (TransferConfiguration, error) {
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

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "id":
			Logger.Printf("id: %s", value)
			result.id = value
		case "mtu":
			if value == "auto" {
				result.mtu = 0
			} else {
				tmp, err := strconv.Atoi(value)
				if err != nil {
					Logger.Fatalf("Error in config mtu. Ilegal value: %s. Legal values are 'auto' or a two byte integer", value)
				} else if tmp < 0 || tmp > 65535 {
					Logger.Fatalf("Error in config mtu. Ilegal value: %s. Legal values are 'auto' or 0-65535", value)
				} else {
					result.mtu = uint16(tmp)
				}
			}
			Logger.Printf("mtu: %d", result.mtu)
		case "logFileName":
			result.logFileName = value
			Logger.Printf("logFileName: %s", value)
		case "nic":
			result.nic = value
			Logger.Printf("nic: %s", value)
		case "targetIP":
			result.targetIP = value
			Logger.Printf("targetIP: %s", value)
		case "targetPort":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config targetPort. Ilegal value: %s. Legal values are 0-65535", value)
			} else {
				if tmp < 0 {
					Logger.Fatalf("Error in config targetPort. Ilegal value: %s. Legal values are 0-65535", value)
				} else if tmp > 65535 {
					Logger.Fatalf("Error in config targetPort. Ilegal value: %s. Legal values are 0-65535", value)
				} else {
					result.targetPort = tmp
				}
			}
			Logger.Printf("targetPort: %d", result.targetPort)
		case "bootstrapServers":
			result.bootstrapServers = value
			Logger.Printf("bootstrapServers: %s", value)
		case "clientId":
			result.clientID = value
			Logger.Printf("clientId: %s", value)
		case "topic":
			result.topic = value
			Logger.Printf("topic: %s", value)
		case "privateKeyFiles": // glob
			result.privateKeyGlob = value
			Logger.Printf("privateKeyGlob: %s", value)
		case "logLevel":
			result.logLevel = strings.ToUpper(value)
			Logger.Printf("logLevel: %s", result.logLevel)
		case "target": // optional
			if value == "kafka" || value == "cmd" || value == "null" {
				result.target = value
				Logger.Printf("target: %s", value)
			} else {
				Logger.Fatalf("Unknown target %s", value)
			}
		case "transport": // optional - tcp or udp (default: udp)
			if value == "udp" || value == "tcp" {
				result.transport = value
				Logger.Printf("transport: %s", value)
			} else {
				Logger.Fatalf("Unknown transport %s. Legal values are: udp, tcp", value)
			}
		case "certFile":
			result.certFile = value
			Logger.Printf("certFile: %s", value)
		case "keyFile":
			result.keyFile = value
			Logger.Printf("keyFile: %s", value)
		case "caFile":
			result.caFile = value
			Logger.Printf("caFile: %s", value)
		case "topicTranslations":
			// Json like {"inputTopic1":"outputTopic1","inputTopic2":"outputTopic2"}
			result.topicTranslations = value
		case "logStatistics":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config logStatistics. Illegal value: %s. Legal values are 0-60", value)
			} else if tmp < 0 || tmp > 60 {
				Logger.Fatalf("Error in config logStatistics. Illegal value: %s. Legal values are 0-60", value)
			} else {
				result.logStatistics = int32(tmp)
			}
		case "numReceivers":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config numReceivers. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 {
				Logger.Fatalf("Error in config numReceivers. Illegal value: %s. Legal values are positive integers", value)
			} else {
				result.numReceivers = tmp
			}
			Logger.Printf("numReceivers: %d", result.numReceivers)
		case "channelBufferSize":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config channelBufferSize. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 {
				Logger.Fatalf("Error in config channelBufferSize. Illegal value: %s. Legal values are positive integers", value)
			} else {
				result.channelBufferSize = tmp
			}
			Logger.Printf("channelBufferSize: %d", result.channelBufferSize)
		case "batchSize":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config batchSize. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 {
				Logger.Fatalf("Error in config batchSize. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp > 512 {
				Logger.Fatalf("Error in config batchSize. Illegal value: %s. Legal values are positive integers up to 512", value)
			} else {
				result.batchSize = tmp
			}
			Logger.Printf("batchSize: %d", result.batchSize)
		case "readBufferMultiplier":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config readBufferMultiplier. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 || tmp > 65535 {
				Logger.Fatalf("Error in config readBufferMultiplier. Illegal value: %s. Legal values are positive integers up to 65535", value)
			} else {
				result.readBufferMultiplier = uint16(tmp)
			}
		case "rcvBufSize":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				Logger.Fatalf("Error in config rcvBufSize. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 {
				Logger.Fatalf("Error in config rcvBufSize. Illegal value: %s. Legal values are positive integers", value)
			} else {
				result.rcvBufSize = tmp
			}
		case "internalTopic":
			result.topic = value
		case "keyPasswordFile":
			result.keyPasswordFile = value
		case "maximumDecompressSize":
			result.maximumDecompressSize, err = convertUnitSufix(value)
			if err != nil {
				Logger.Fatalf("Error converting maximumDecompressSize \"%s\" to integer value", value)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return TransferConfiguration{}, err
	}

	return result, nil
}

func overrideConfiguration(config TransferConfiguration) TransferConfiguration {
	Logger.Print("Checking configuration from environment variables...")

	prefix := "AIRGAP_DOWNSTREAM_"
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
	if mtu := os.Getenv(prefix + "MTU"); mtu != "" {
		Logger.Print("Overriding mtu with environment variable: " + prefix + "MTU" + " with value: " + mtu)
		if mtu == "auto" {
			config.mtu = 0
		} else if mtuInt, err := strconv.Atoi(mtu); err == nil {
			if mtuInt < 0 || mtuInt > 65535 {
				Logger.Fatalf("Error in config MTU. Illegal value: %s. Legal values are 'auto' or 0-65535", mtu)
			}
			config.mtu = uint16(mtuInt)
		}
	}
	if privateKeyGlob := os.Getenv(prefix + "PRIVATE_KEY_GLOB"); privateKeyGlob != "" {
		Logger.Print("Overriding privateKeyGlob with environment variable: " + prefix + "PRIVATE_KEY_GLOB" + " with value: " + privateKeyGlob)
		config.privateKeyGlob = privateKeyGlob
	}
	if target := os.Getenv(prefix + "TARGET"); target != "" {
		Logger.Print("Overriding target with environment variable: " + prefix + "TARGET" + " with value: " + target)
		config.target = target
	}
	if transport := os.Getenv(prefix + "TRANSPORT"); transport != "" {
		Logger.Print("Overriding transport with environment variable: " + prefix + "TRANSPORT" + " with value: " + transport)
		if transport == "udp" || transport == "tcp" {
			config.transport = transport
		} else {
			Logger.Fatalf("Error in environment variable AIRGAP_DOWNSTREAM_TRANSPORT. Illegal value: %s. Legal values are: udp, tcp", transport)
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
	if topicTranslations := os.Getenv(prefix + "TOPIC_TRANSLATIONS"); topicTranslations != "" {
		Logger.Print("Overriding topicTranslations with environment variable: " + prefix + "TOPIC_TRANSLATIONS" + " with value: " + topicTranslations)
		config.topicTranslations = topicTranslations
	}
	if logStatistics := os.Getenv(prefix + "LOG_STATISTICS"); logStatistics != "" {
		tmp, err := strconv.Atoi(logStatistics)
		if err != nil {
			Logger.Fatalf("Error in config logStatistics. Illegal value: %s. Legal values are 0-60", logStatistics)
		} else if tmp < 0 || tmp > 60 {
			Logger.Fatalf("Error in config logStatistics. Illegal value: %s. Legal values are 0-60", logStatistics)
		} else {
			config.logStatistics = int32(tmp)
		}
	}
	if numReceivers := os.Getenv(prefix + "NUM_RECEIVERS"); numReceivers != "" {
		tmp, err := strconv.Atoi(numReceivers)
		if err != nil {
			Logger.Fatalf("Error in config numReceivers. Illegal value: %s. Legal values are positive integers", numReceivers)
		} else if tmp < 1 {
			Logger.Fatalf("Error in config numReceivers. Illegal value: %s. Legal values are positive integers", numReceivers)
		} else {
			config.numReceivers = tmp
		}
	}
	if channelBufferSize := os.Getenv(prefix + "CHANNEL_BUFFER_SIZE"); channelBufferSize != "" {
		tmp, err := strconv.Atoi(channelBufferSize)
		if err != nil {
			Logger.Fatalf("Error in config channelBufferSize. Illegal value: %s. Legal values are positive integers", channelBufferSize)
		} else if tmp < 1 {
			Logger.Fatalf("Error in config channelBufferSize. Illegal value: %s. Legal values are positive integers", channelBufferSize)
		} else {
			config.channelBufferSize = tmp
		}
	}
	if batchSize := os.Getenv(prefix + "BATCH_SIZE"); batchSize != "" {
		tmp, err := strconv.Atoi(batchSize)
		if err != nil {
			Logger.Fatalf("Error in config batchSize. Illegal value: %s. Legal values are positive integers", batchSize)
		} else if tmp < 1 {
			Logger.Fatalf("Error in config batchSize. Illegal value: %s. Legal values are positive integers", batchSize)
		} else if tmp > 512 {
			Logger.Fatalf("Error in config batchSize. Illegal value: %s. Legal values are positive integers up to 512", batchSize)
		} else {
			if tmp > 65535 {
				Logger.Fatalf("Error in config batchSize. Illegal value: %s. Legal values are positive integers up to 65535", batchSize)
			}
			config.batchSize = tmp
		}
	}
	if readBufferMultiplier := os.Getenv(prefix + "READ_BUFFER_MULTIPLIER"); readBufferMultiplier != "" {
		tmp, err := strconv.Atoi(readBufferMultiplier)
		if err != nil {
			Logger.Fatalf("Error in config readBufferMultiplier. Illegal value: %s. Legal values are positive integers", readBufferMultiplier)
		} else if tmp < 1 || tmp > 65535 {
			Logger.Fatalf("Error in config readBufferMultiplier. Illegal value: %s. Legal values are positive integers up to 65535", readBufferMultiplier)
		} else {
			config.readBufferMultiplier = uint16(tmp)
		}
	}
	if rcvBufSize := os.Getenv(prefix + "RCV_BUF_SIZE"); rcvBufSize != "" {
		tmp, err := strconv.Atoi(rcvBufSize)
		if err != nil {
			Logger.Fatalf("Error in config rcvBufSize. Illegal value: %s. Legal values are positive integers", rcvBufSize)
		} else if tmp < 1 {
			Logger.Fatalf("Error in config rcvBufSize. Illegal value: %s. Legal values are positive integers", rcvBufSize)
		} else {
			config.rcvBufSize = tmp
		}
	}
	if internalTopic := os.Getenv(prefix + "INTERNAL_TOPIC"); internalTopic != "" {
		config.topic = internalTopic
	}
	if keyPasswordFile := os.Getenv(prefix + "KEY_PASSWORD_FILE"); keyPasswordFile != "" {
		config.keyPasswordFile = keyPasswordFile
	}
	if maximumDecompressSize := os.Getenv(prefix + "MAXIMUM_DECOMPRESS_SIZE"); maximumDecompressSize != "" {
		var err error
		config.maximumDecompressSize, err = convertUnitSufix(maximumDecompressSize)
		if err != nil {
			Logger.Fatalf("Error converting maximumDecompressSize \"%s\" to integer value", maximumDecompressSize)
		}
	}
	return config
}

// The current configuration
func logConfiguration(config TransferConfiguration) {
	Logger.Printf("Configuration for build number %s:", BuildNumber)
	Logger.Printf("  id: %s", config.id)
	Logger.Printf("  nic: %s", config.nic)
	Logger.Printf("  targetIP: %s", config.targetIP)
	Logger.Printf("  targetPort: %d", config.targetPort)
	Logger.Printf("  bootstrapServers: %s", config.bootstrapServers)
	Logger.Printf("  mtu: %d", config.mtu)
	Logger.Printf("  privateKeyGlob: %s", config.privateKeyGlob)
	Logger.Printf("  target: %s", config.target)
	Logger.Printf("  logLevel: %s", config.logLevel)
	Logger.Printf("  logFileName: %s", config.logFileName)
	Logger.Printf("  certFile: %s", config.certFile)
	Logger.Printf("  keyFile: %s", config.keyFile)
	Logger.Printf("  keyPasswordFile: %s", config.keyPasswordFile)
	Logger.Printf("  caFile: %s", config.caFile)
	Logger.Printf("  topicTranslations: %s", config.topicTranslations)
	Logger.Printf("  logStatistics: %d", config.logStatistics)
	Logger.Printf("  numReceivers: %d", config.numReceivers)
	Logger.Printf("  channelBufferSize: %d", config.channelBufferSize)
	Logger.Printf("  batchSize: %d", config.batchSize)
	Logger.Printf("  readBufferMultiplier: %d", config.readBufferMultiplier)
	Logger.Printf("  rcvBufSize: %d", config.rcvBufSize)
	Logger.Printf("  internalTopic: %s", config.topic)
	Logger.Printf("  internalTopic: %s", config.topic)
	if len(config.translations) > 0 {
		Logger.Printf("  topicTranslations: %s", config.topicTranslations)
	}
	Logger.Printf("  maximumDecompressSize: %s", config.maximumDecompressSize)
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
	if result.targetIP == "" {
		Logger.Fatal("Missing required configuration: targetIP")
	}
	if result.targetPort < 0 || result.targetPort > 65535 {
		Logger.Fatal("Invalid configuration: targetPort must be between 0 and 65535")
	}
	if result.target != "kafka" && result.target != "cmd" && result.target != "null" {
		Logger.Fatalf("Unknown target '%s'. Valid targets are: 'kafka', 'cmd', 'null'", result.target)
	}
	if result.target == "kafka" && result.bootstrapServers == "" {
		Logger.Fatal("Missing required configuration: bootstrapServers")
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
			Logger.Fatalf("Error in config logLevel. Illegal value: %s. Legal values are DEBUG, INFO, WARN, ERROR, FATAL", result.logLevel)
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
	if result.topicTranslations != "" {
		if err := json.Unmarshal([]byte(result.topicTranslations), &result.translations); err != nil {
			Logger.Fatalf("Error in config topicTranslations. Illegal value: %s. Legal values are JSON objects 'from': 'to', ...", result.topicTranslations)
		}
	}
	if result.numReceivers < 1 {
		Logger.Fatal("Invalid configuration: numReceivers must be a positive integer")
	}
	if result.channelBufferSize < 1 {
		Logger.Fatal("Invalid configuration: channelBufferSize must be a positive integer")
	}
	if result.batchSize < 1 || result.batchSize > 512 {
		Logger.Fatal("Invalid configuration: batchSize must be a positive integer up to 512")
	}
	if result.rcvBufSize < 1 {
		Logger.Fatal("Invalid configuration: rcvBufSize must be a positive integer")
	}

	logConfiguration(result)
	Logger.Print("Configuration OK")
	return result
}
func intPow(base int, exp uint) int {
	out := 1
	for _ = range exp {
		out = out*base
	}
	return out
}
func convertUnitSufix(size string) (int, error) {
	Logger.Debugf("Size is ", size)
	if num, found := strings.CutSuffix(size, "B"); found {
		Logger.Debugf("Found B")
		mult := 1
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult , nil
		}
	}
	if num, found := strings.CutSuffix(size, "KiB"); found {
		Logger.Debugf("Found KiB")
		mult := intPow(2, 10)
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult , nil
		}
	}
	if num, found := strings.CutSuffix(size, "MiB"); found {
		Logger.Debugf("Found MiB")
		mult := intPow(2, 20)
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult , nil
		}
	}
	if num, found := strings.CutSuffix(size, "GiB"); found {
		mult := intPow(2, 30)
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult , nil
		}
	}
	if num, found := strings.CutSuffix(size, "TiB"); found {
		mult := intPow(2, 40)
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult , nil
		}
	}
	out, err := strconv.Atoi(size)
	if err != nil {
		return 0, err 
	}
	return out, nil
}
