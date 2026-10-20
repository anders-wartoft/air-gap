package downstream

import (
	"bufio"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"sitia.nu/airgap/src/internal/pinnedtls"
	"sitia.nu/airgap/src/internal/properties"
	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/protocol"
)

// configStr builds the attributes for a configuration record whose value is a
// string. The setting name and its value are what a consumer needs in order to
// follow how a running configuration was assembled, without parsing the body.
func configStr(event, setting, value string) logging.Attrs {
	return logging.Event(event,
		logging.Str("airgap.config.setting", setting),
		logging.Str("airgap.config.value", value),
	)
}

// configInt is configStr for a numeric setting.
func configInt(event, setting string, value int64) logging.Attrs {
	return logging.Event(event,
		logging.Str("airgap.config.setting", setting),
		logging.Int("airgap.config.value", value),
	)
}

// configBool is configStr for a boolean setting.
func configBool(event, setting string, value bool) logging.Attrs {
	return logging.Event(event,
		logging.Str("airgap.config.setting", setting),
		logging.Bool("airgap.config.value", value),
	)
}

// configInvalid describes a rejected setting value.
func configInvalid(setting, value string) logging.Attrs {
	return configStr(logging.EventConfigInvalid, setting, value).With(logging.Failure())
}

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
	id                    string            // unique id for this downstream
	nic                   string            // network interface card to bind the listener to
	targetIP              string            // local IP address to listen on
	targetPort            int               // local port to listen on
	bootstrapServers      string            // Kafka bootstrap servers to deliver to
	topic                 string            // default Kafka topic when the message carries none
	mtu                   uint16            // maximum transmission unit used to size the UDP read buffer
	key                   []byte            // symmetric
	keyInfos              []KeyInfo         // array of private keys
	privateKeyGlob        string            // glob matching the private key files used to unwrap symmetric keys
	target                string            // delivery target: kafka, file or console
	transport             string            // Transport protocol: udp or tcp (default: udp)
	logLevel              string            // log level: DEBUG, INFO, WARN, ERROR, FATAL
	logFileName           string            // log file name, redirect logging to a file from the console
	certFile              string            // Certificate to use to communicate with Kafka with TLS
	keyFile               string            // Key file to use for TLS
	caFile                string            // CA file to use for TLS
	clientID              string            // Kafka client id reported to the broker
	topicTranslations     string            // topic translations in JSON format
	translations          map[string]string // map from input topic to output topic
	logStatistics         int32             //  How often to log statistics in seconds. 0 means no logging
	numReceivers          int               // Number of UDP receivers to start
	channelBufferSize     int               // Size of the channel buffer between UDP receiver and Kafka writer
	batchSize             int               // Number of messages to batch together
	readBufferMultiplier  uint16            // Multiplier for the read buffer size
	rcvBufSize            int               // Size of the OS receive buffer for UDP sockets
	keyPasswordFile       string            // File containing the password to decrypt the key file
	maximumDecompressSize int               // maximum number of bytes to decompress before the packet is dropped; -1 means no limit
	// Security limits - protect the downstream listening port from DoS
	maxCacheEntries            int  // Maximum in-flight fragment-reassembly entries (OOM protection)
	maxNrMessages              int  // Maximum nrMessages value accepted in a single packet
	maxTCPConnections          int  // Maximum concurrent TCP connections
	keyExchangeMinIntervalSecs int  // Minimum seconds between accepted KEY_EXCHANGE messages
	enableRxqOvfl              bool // Enable SO_RXQ_OVFL to track socket-level packet drops (has small performance overhead)
	// TCP TLS settings
	tcpTLSCertFile          string // server certificate file for TCP TLS
	tcpTLSKeyFile           string // server private key file for TCP TLS
	tcpTLSKeyPasswordFile   string // password file for encrypted TCP TLS key
	tcpTLSCAFile            string // CA certificate to verify client certs (mTLS)
	tcpTLSClientAuth        string // client auth mode: "none" (default), "allow", "require"
	tcpTLSClientCNRegex     string // regex to match client Common Name (used with allow/require)
	tcpTLSCipherSuites      string // TLS protocol policy: empty or "TLS1.3" = TLS 1.3 only; comma-separated TLS 1.2 cipher names = TLS 1.2 with those suites
	tcpTLSAuthMode          string // TCP peer authentication mode: "ca" (default, CA-issued certificates) or "pinned" (exact SHA-256 public-key fingerprints, no CA)
	tcpTLSTrustedKeysDir    string // directory with JSON trust entries for pinned mode; holds the public keys of the upstream clients we accept
	tcpTLSAutomaticRotation bool   // allow installing a new client pin presented by an already authenticated peer (default false)
	tcpTLSPinIdleSeconds    int    // deactivate unused overlapping pins in memory after this many idle seconds; 0 (default) disables aging
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
	config.logStatistics = 0                         // default no logging
	config.numReceivers = 10                         // default ten UDP receiver
	config.channelBufferSize = 16384                 // default channel buffer size of 16384 bytes
	config.batchSize = 32                            // default batching of 32 messages
	config.readBufferMultiplier = 16                 // default 16 times mtu as memory buffer
	config.rcvBufSize = 4 * 1024 * 1024              // default 4MB OS receive buffer for UDP sockets
	config.topic = ""                                // default disabled (empty = no internal Kafka logging)
	config.maximumDecompressSize = 256 * 1024 * 1024 // default 256 MiB
	config.maxCacheEntries = int(protocol.DefaultMaxCacheEntries)
	config.maxNrMessages = int(protocol.DefaultMaxNrMessages)
	config.maxTCPConnections = 256
	config.keyExchangeMinIntervalSecs = 1
	config.enableRxqOvfl = false     // default: disabled for maximum performance
	config.tcpTLSClientAuth = "none" // default: no client authentication
	config.tcpTLSCipherSuites = ""   // default: TLS 1.3 enforced
	config.tcpTLSAuthMode = "ca"
	return config
}

// Read the configuration file and return the configuration
func readConfiguration(fileName string, result TransferConfiguration) (TransferConfiguration, error) {
	return readConfigurationFile(fileName, result, false)
}

func readConfigurationFile(fileName string, result TransferConfiguration, strict bool) (TransferConfiguration, error) {
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
		case "id":
			Logger.Infof(configStr(logging.EventConfigParse, "id", value), "id: %s", value)
			result.id = value
			logging.Logger.SetInstanceID(value)
		case logging.OptionFormat, logging.OptionHash, logging.OptionHashChain,
			logging.OptionSigningCertificate, logging.OptionSigningKey,
			logging.OptionSignerID, logging.OptionSigningEKU:
			if _, err := logging.ApplyOption(key, value); err != nil {
				return result, err
			}
			Logger.Infof(configStr(logging.EventConfigParse, key, value), "%s: %s", key, value)
		case "mtu":
			if value == "auto" {
				result.mtu = 0
			} else {
				tmp, err := strconv.Atoi(value)
				if err != nil {
					return result, fmt.Errorf("Error in config mtu. Ilegal value: %s. Legal values are 'auto' or a two byte integer", value)
				} else if tmp < 0 || tmp > 65535 {
					return result, fmt.Errorf("Error in config mtu. Ilegal value: %s. Legal values are 'auto' or 0-65535", value)
				} else {
					result.mtu = uint16(tmp)
				}
			}
			Logger.Infof(configInt(logging.EventConfigParse, "mtu", int64(result.mtu)), "mtu: %d", result.mtu)
		case "logFileName":
			result.logFileName = value
			Logger.Infof(configStr(logging.EventConfigParse, "logFileName", value), "logFileName: %s", value)
		case "nic":
			result.nic = value
			Logger.Infof(configStr(logging.EventConfigParse, "nic", value), "nic: %s", value)
		case "targetIP":
			result.targetIP = value
			Logger.Infof(configStr(logging.EventConfigParse, "targetIP", value), "targetIP: %s", value)
		case "targetPort":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config targetPort. Ilegal value: %s. Legal values are 0-65535", value)
			} else {
				if tmp < 0 {
					return result, fmt.Errorf("Error in config targetPort. Ilegal value: %s. Legal values are 0-65535", value)
				} else if tmp > 65535 {
					return result, fmt.Errorf("Error in config targetPort. Ilegal value: %s. Legal values are 0-65535", value)
				} else {
					result.targetPort = tmp
				}
			}
			Logger.Infof(configInt(logging.EventConfigParse, "targetPort", int64(result.targetPort)), "targetPort: %d", result.targetPort)
		case "bootstrapServers":
			result.bootstrapServers = value
			Logger.Infof(configStr(logging.EventConfigParse, "bootstrapServers", value), "bootstrapServers: %s", value)
		case "clientId":
			result.clientID = value
			Logger.Infof(configStr(logging.EventConfigParse, "clientId", value), "clientId: %s", value)
		case "topic":
			result.topic = value
			Logger.Infof(configStr(logging.EventConfigParse, "topic", value), "topic: %s", value)
		case "privateKeyFiles": // glob
			result.privateKeyGlob = value
			Logger.Infof(configStr(logging.EventConfigParse, "privateKeyGlob", value), "privateKeyGlob: %s", value)
		case "logLevel":
			result.logLevel = strings.ToUpper(value)
			Logger.Infof(configStr(logging.EventConfigParse, "logLevel", result.logLevel), "logLevel: %s", result.logLevel)
		case "target": // optional
			if value == "kafka" || value == "cmd" || value == "null" {
				result.target = value
				Logger.Infof(configStr(logging.EventConfigParse, "target", value), "target: %s", value)
			} else {
				return result, fmt.Errorf("Unknown target %s", value)
			}
		case "transport": // optional - tcp or udp (default: udp)
			if value == "udp" || value == "tcp" {
				result.transport = value
				Logger.Infof(configStr(logging.EventConfigParse, "transport", value), "transport: %s", value)
			} else {
				return result, fmt.Errorf("Unknown transport %s. Legal values are: udp, tcp", value)
			}
		case "certFile":
			result.certFile = value
			Logger.Infof(configStr(logging.EventConfigParse, "certFile", value), "certFile: %s", value)
		case "keyFile":
			result.keyFile = value
			Logger.Infof(configStr(logging.EventConfigParse, "keyFile", value), "keyFile: %s", value)
		case "caFile":
			result.caFile = value
			Logger.Infof(configStr(logging.EventConfigParse, "caFile", value), "caFile: %s", value)
		case "topicTranslations":
			// Json like {"inputTopic1":"outputTopic1","inputTopic2":"outputTopic2"}
			result.topicTranslations = value
		case "logStatistics":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config logStatistics. Illegal value: %s. Legal values are 0-60", value)
			} else if tmp < 0 || tmp > 60 {
				return result, fmt.Errorf("Error in config logStatistics. Illegal value: %s. Legal values are 0-60", value)
			} else {
				result.logStatistics = int32(tmp)
			}
		case "numReceivers":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config numReceivers. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 {
				return result, fmt.Errorf("Error in config numReceivers. Illegal value: %s. Legal values are positive integers", value)
			} else {
				result.numReceivers = tmp
			}
			Logger.Infof(configInt(logging.EventConfigParse, "numReceivers", int64(result.numReceivers)), "numReceivers: %d", result.numReceivers)
		case "channelBufferSize":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config channelBufferSize. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 {
				return result, fmt.Errorf("Error in config channelBufferSize. Illegal value: %s. Legal values are positive integers", value)
			} else {
				result.channelBufferSize = tmp
			}
			Logger.Infof(configInt(logging.EventConfigParse, "channelBufferSize", int64(result.channelBufferSize)), "channelBufferSize: %d", result.channelBufferSize)
		case "batchSize":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config batchSize. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 {
				return result, fmt.Errorf("Error in config batchSize. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp > 512 {
				return result, fmt.Errorf("Error in config batchSize. Illegal value: %s. Legal values are positive integers up to 512", value)
			} else {
				result.batchSize = tmp
			}
			Logger.Infof(configInt(logging.EventConfigParse, "batchSize", int64(result.batchSize)), "batchSize: %d", result.batchSize)
		case "readBufferMultiplier":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config readBufferMultiplier. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 || tmp > 65535 {
				return result, fmt.Errorf("Error in config readBufferMultiplier. Illegal value: %s. Legal values are positive integers up to 65535", value)
			} else {
				result.readBufferMultiplier = uint16(tmp)
			}
		case "rcvBufSize":
			tmp, err := strconv.Atoi(value)
			if err != nil {
				return result, fmt.Errorf("Error in config rcvBufSize. Illegal value: %s. Legal values are positive integers", value)
			} else if tmp < 1 {
				return result, fmt.Errorf("Error in config rcvBufSize. Illegal value: %s. Legal values are positive integers", value)
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
				return result, fmt.Errorf("Error converting maximumDecompressSize \"%s\" to integer value", value)
			}
		case "maxCacheEntries":
			// Maximum in-flight fragment-reassembly entries. Protects against OOM when
			// the listening port is flooded with packets using unique message IDs.
			tmp, err := strconv.Atoi(value)
			if err != nil || tmp < 1 {
				return result, fmt.Errorf("Error in config maxCacheEntries. Illegal value: %s. Legal values are positive integers", value)
			}
			result.maxCacheEntries = tmp
			Logger.Infof(configInt(logging.EventConfigParse, "maxCacheEntries", int64(result.maxCacheEntries)), "maxCacheEntries: %d", result.maxCacheEntries)
		case "maxNrMessages":
			// Maximum nrMessages value in a single packet. Packets claiming more fragments
			// than this are rejected early to prevent cache entries that can never complete.
			tmp, err := strconv.Atoi(value)
			if err != nil || tmp < 1 || tmp > 65535 {
				return result, fmt.Errorf("Error in config maxNrMessages. Illegal value: %s. Legal values are 1-65535", value)
			}
			result.maxNrMessages = tmp
			Logger.Infof(configInt(logging.EventConfigParse, "maxNrMessages", int64(result.maxNrMessages)), "maxNrMessages: %d", result.maxNrMessages)
		case "maxTCPConnections":
			// Maximum simultaneous TCP connections. Without a cap every connection spawns
			// a goroutine; a flood of connections exhausts memory and goroutine stacks.
			tmp, err := strconv.Atoi(value)
			if err != nil || tmp < 1 {
				return result, fmt.Errorf("Error in config maxTCPConnections. Illegal value: %s. Legal values are positive integers", value)
			}
			result.maxTCPConnections = tmp
			Logger.Infof(configInt(logging.EventConfigParse, "maxTCPConnections", int64(result.maxTCPConnections)), "maxTCPConnections: %d", result.maxTCPConnections)
		case "keyExchangeMinIntervalSecs":
			// Minimum seconds between accepted KEY_EXCHANGE messages. Each KEY_EXCHANGE
			// triggers RSA-OAEP decryption for every loaded private key; rate-limiting
			// prevents CPU exhaustion. Legitimate key rotation runs every hundreds of
			// seconds so a 1 s floor does not affect normal operation.
			tmp, err := strconv.Atoi(value)
			if err != nil || tmp < 0 {
				return result, fmt.Errorf("Error in config keyExchangeMinIntervalSecs. Illegal value: %s. Legal values are 0 or higher", value)
			}
			result.keyExchangeMinIntervalSecs = tmp
			Logger.Infof(configInt(logging.EventConfigParse, "keyExchangeMinIntervalSecs", int64(result.keyExchangeMinIntervalSecs)), "keyExchangeMinIntervalSecs: %d", result.keyExchangeMinIntervalSecs)
		case "enableRxqOvfl":
			tmp := strings.ToLower(value)
			if tmp != "true" && tmp != "false" {
				return result, fmt.Errorf("Error in config enableRxqOvfl. Illegal value: %s. Legal values are true or false", value)
			}
			result.enableRxqOvfl = tmp == "true"
			Logger.Infof(configBool(logging.EventConfigParse, "enableRxqOvfl", result.enableRxqOvfl), "enableRxqOvfl: %t", result.enableRxqOvfl)
		case "tcpTLSCertFile":
			result.tcpTLSCertFile = value
			Logger.Infof(configStr(logging.EventConfigParse, "tcpTLSCertFile", value), "tcpTLSCertFile: %s", value)
		case "tcpTLSKeyFile":
			result.tcpTLSKeyFile = value
			Logger.Infof(configStr(logging.EventConfigParse, "tcpTLSKeyFile", value), "tcpTLSKeyFile: %s", value)
		case "tcpTLSKeyPasswordFile":
			result.tcpTLSKeyPasswordFile = value
		case "tcpTLSCAFile":
			result.tcpTLSCAFile = value
			Logger.Infof(configStr(logging.EventConfigParse, "tcpTLSCAFile", value), "tcpTLSCAFile: %s", value)
		case "tcpTLSClientAuth":
			mode := strings.ToLower(value)
			switch mode {
			case "none", "allow", "require":
				result.tcpTLSClientAuth = mode
				Logger.Infof(configStr(logging.EventConfigParse, "tcpTLSClientAuth", mode), "tcpTLSClientAuth: %s", mode)
			default:
				return result, fmt.Errorf("Error in config tcpTLSClientAuth. Illegal value: %s. Legal values are none, allow, require", value)
			}
		case "tcpTLSClientCNRegex":
			result.tcpTLSClientCNRegex = value
			Logger.Infof(configStr(logging.EventConfigParse, "tcpTLSClientCNRegex", value), "tcpTLSClientCNRegex: %s", value)
		case "tcpTLSCipherSuites":
			result.tcpTLSCipherSuites = value
			Logger.Infof(configStr(logging.EventConfigParse, "tcpTLSCipherSuites", value), "tcpTLSCipherSuites: %s", value)
		case "tcpTLSAuthMode":
			result.tcpTLSAuthMode = value
		case "tcpTLSTrustedKeysDir":
			result.tcpTLSTrustedKeysDir = value
		case "tcpTLSAutomaticRotation":
			if value != "true" && value != "false" {
				return result, fmt.Errorf("invalid tcpTLSAutomaticRotation; expected true or false")
			}
			result.tcpTLSAutomaticRotation = value == "true"
		case "tcpTLSPinIdleSeconds":
			seconds, err := pinnedtls.ParsePinIdleSeconds(value)
			if err != nil {
				return result, err
			}
			result.tcpTLSPinIdleSeconds = seconds
		default:
			if strict {
				return result, fmt.Errorf("unknown configuration key: %s", key)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return TransferConfiguration{}, err
	}

	return result, nil
}

func overrideConfiguration(config TransferConfiguration) TransferConfiguration {
	Logger.Info(logging.Event(logging.EventConfigOverride), "Checking configuration from environment variables...")

	prefix := "AIRGAP_DOWNSTREAM_"
	if id := os.Getenv(prefix + "ID"); id != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "id", id).With(
			logging.Str("airgap.config.source", prefix+"ID"),
		), "Overriding id with environment variable: "+prefix+"ID"+" with value: "+id)
		config.id = id
	}

	if nic := os.Getenv(prefix + "NIC"); nic != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "nic", nic).With(
			logging.Str("airgap.config.source", prefix+"NIC"),
		), "Overriding nic with environment variable: "+prefix+"NIC"+" with value: "+nic)
		config.nic = nic
	}

	if targetIP := os.Getenv(prefix + "TARGET_IP"); targetIP != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "targetIP", targetIP).With(
			logging.Str("airgap.config.source", prefix+"TARGET_IP"),
		), "Overriding targetIP with environment variable: "+prefix+"TARGET_IP"+" with value: "+targetIP)
		config.targetIP = targetIP
	}

	if targetPort := os.Getenv(prefix + "TARGET_PORT"); targetPort != "" {
		if port, err := strconv.Atoi(targetPort); err == nil {
			Logger.Info(configStr(logging.EventConfigOverride, "targetPort", targetPort).With(
				logging.Str("airgap.config.source", prefix+"TARGET_PORT"),
			), "Overriding targetPort with environment variable: "+prefix+"TARGET_PORT"+" with value: "+targetPort)
			config.targetPort = port
		}
	}
	if bootstrapServers := os.Getenv(prefix + "BOOTSTRAP_SERVERS"); bootstrapServers != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "bootstrapServers", bootstrapServers).With(
			logging.Str("airgap.config.source", prefix+"BOOTSTRAP_SERVERS"),
		), "Overriding bootstrapServers with environment variable: "+prefix+"BOOTSTRAP_SERVERS"+" with value: "+bootstrapServers)
		config.bootstrapServers = bootstrapServers
	}
	if mtu := os.Getenv(prefix + "MTU"); mtu != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "mtu", mtu).With(
			logging.Str("airgap.config.source", prefix+"MTU"),
		), "Overriding mtu with environment variable: "+prefix+"MTU"+" with value: "+mtu)
		if mtu == "auto" {
			config.mtu = 0
		} else if mtuInt, err := strconv.Atoi(mtu); err == nil {
			if mtuInt < 0 || mtuInt > 65535 {
				Logger.Fatalf(configInvalid("MTU", mtu), "Error in config MTU. Illegal value: %s. Legal values are 'auto' or 0-65535", mtu)
				os.Exit(1)
			}
			// Safe: mtuInt is checked to fit in uint16 above, and Logger.Fatalf terminates execution if not.
			// codeql[incorrect-integer-conversion]: value is checked and fatal error terminates on overflow
			config.mtu = uint16(mtuInt)
		}
	}
	if privateKeyGlob := os.Getenv(prefix + "PRIVATE_KEY_GLOB"); privateKeyGlob != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "privateKeyGlob", privateKeyGlob).With(
			logging.Str("airgap.config.source", prefix+"PRIVATE_KEY_GLOB"),
		), "Overriding privateKeyGlob with environment variable: "+prefix+"PRIVATE_KEY_GLOB"+" with value: "+privateKeyGlob)
		config.privateKeyGlob = privateKeyGlob
	}
	if target := os.Getenv(prefix + "TARGET"); target != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "target", target).With(
			logging.Str("airgap.config.source", prefix+"TARGET"),
		), "Overriding target with environment variable: "+prefix+"TARGET"+" with value: "+target)
		config.target = target
	}
	if transport := os.Getenv(prefix + "TRANSPORT"); transport != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "transport", transport).With(
			logging.Str("airgap.config.source", prefix+"TRANSPORT"),
		), "Overriding transport with environment variable: "+prefix+"TRANSPORT"+" with value: "+transport)
		if transport == "udp" || transport == "tcp" {
			config.transport = transport
		} else {
			Logger.Fatalf(configInvalid("transport", transport).With(
				logging.Str("airgap.config.source", prefix+"TRANSPORT"),
			), "Error in environment variable AIRGAP_DOWNSTREAM_TRANSPORT. Illegal value: %s. Legal values are: udp, tcp", transport)
		}
	}
	if logLevel := os.Getenv(prefix + "LOG_LEVEL"); logLevel != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "logLevel", logLevel).With(
			logging.Str("airgap.config.source", prefix+"LOG_LEVEL"),
		), "Overriding logLevel with environment variable: "+prefix+"LOG_LEVEL"+" with value: "+logLevel)
		config.logLevel = logLevel
	}
	if logFileName := os.Getenv(prefix + "LOG_FILE_NAME"); logFileName != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "logFileName", logFileName).With(
			logging.Str("airgap.config.source", prefix+"LOG_FILE_NAME"),
		), "Overriding logFileName with environment variable: "+prefix+"LOG_FILE_NAME"+" with value: "+logFileName)
		config.logFileName = logFileName
	}
	if err := logging.ApplyEnvironmentOptions(prefix); err != nil {
		Logger.Fatalf(logging.Event(logging.EventConfigInvalid,
			logging.Str("airgap.config.source", prefix),
		).Merge(logging.Err(err)), "%v", err)
	}
	if certFile := os.Getenv(prefix + "CERT_FILE"); certFile != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "certFile", certFile).With(
			logging.Str("airgap.config.source", prefix+"CERT_FILE"),
		), "Overriding certFile with environment variable: "+prefix+"CERT_FILE"+" with value: "+certFile)
		config.certFile = certFile
	}
	if keyFile := os.Getenv(prefix + "KEY_FILE"); keyFile != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "keyFile", keyFile).With(
			logging.Str("airgap.config.source", prefix+"KEY_FILE"),
		), "Overriding keyFile with environment variable: "+prefix+"KEY_FILE"+" with value: "+keyFile)
		config.keyFile = keyFile
	}
	if caFile := os.Getenv(prefix + "CA_FILE"); caFile != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "caFile", caFile).With(
			logging.Str("airgap.config.source", prefix+"CA_FILE"),
		), "Overriding caFile with environment variable: "+prefix+"CA_FILE"+" with value: "+caFile)
		config.caFile = caFile
	}
	if topicTranslations := os.Getenv(prefix + "TOPIC_TRANSLATIONS"); topicTranslations != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "topicTranslations", topicTranslations).With(
			logging.Str("airgap.config.source", prefix+"TOPIC_TRANSLATIONS"),
		), "Overriding topicTranslations with environment variable: "+prefix+"TOPIC_TRANSLATIONS"+" with value: "+topicTranslations)
		config.topicTranslations = topicTranslations
	}
	if logStatistics := os.Getenv(prefix + "LOG_STATISTICS"); logStatistics != "" {
		tmp, err := strconv.Atoi(logStatistics)
		if err != nil {
			Logger.Fatalf(configInvalid("logStatistics", logStatistics), "Error in config logStatistics. Illegal value: %s. Legal values are 0-60", logStatistics)
		} else if tmp < 0 || tmp > 60 {
			Logger.Fatalf(configInvalid("logStatistics", logStatistics), "Error in config logStatistics. Illegal value: %s. Legal values are 0-60", logStatistics)
		} else {
			config.logStatistics = int32(tmp)
		}
	}
	if numReceivers := os.Getenv(prefix + "NUM_RECEIVERS"); numReceivers != "" {
		tmp, err := strconv.Atoi(numReceivers)
		if err != nil {
			Logger.Fatalf(configInvalid("numReceivers", numReceivers), "Error in config numReceivers. Illegal value: %s. Legal values are positive integers", numReceivers)
		} else if tmp < 1 {
			Logger.Fatalf(configInvalid("numReceivers", numReceivers), "Error in config numReceivers. Illegal value: %s. Legal values are positive integers", numReceivers)
		} else {
			config.numReceivers = tmp
		}
	}
	if channelBufferSize := os.Getenv(prefix + "CHANNEL_BUFFER_SIZE"); channelBufferSize != "" {
		tmp, err := strconv.Atoi(channelBufferSize)
		if err != nil {
			Logger.Fatalf(configInvalid("channelBufferSize", channelBufferSize), "Error in config channelBufferSize. Illegal value: %s. Legal values are positive integers", channelBufferSize)
		} else if tmp < 1 {
			Logger.Fatalf(configInvalid("channelBufferSize", channelBufferSize), "Error in config channelBufferSize. Illegal value: %s. Legal values are positive integers", channelBufferSize)
		} else {
			config.channelBufferSize = tmp
		}
	}
	if batchSize := os.Getenv(prefix + "BATCH_SIZE"); batchSize != "" {
		tmp, err := strconv.Atoi(batchSize)
		if err != nil {
			Logger.Fatalf(configInvalid("batchSize", batchSize), "Error in config batchSize. Illegal value: %s. Legal values are positive integers", batchSize)
		} else if tmp < 1 {
			Logger.Fatalf(configInvalid("batchSize", batchSize), "Error in config batchSize. Illegal value: %s. Legal values are positive integers", batchSize)
		} else if tmp > 512 {
			Logger.Fatalf(configInvalid("batchSize", batchSize), "Error in config batchSize. Illegal value: %s. Legal values are positive integers up to 512", batchSize)
		} else {
			if tmp > 65535 {
				Logger.Fatalf(configInvalid("batchSize", batchSize), "Error in config batchSize. Illegal value: %s. Legal values are positive integers up to 65535", batchSize)
			}
			config.batchSize = tmp
		}
	}
	if readBufferMultiplier := os.Getenv(prefix + "READ_BUFFER_MULTIPLIER"); readBufferMultiplier != "" {
		tmp, err := strconv.Atoi(readBufferMultiplier)
		if err != nil {
			Logger.Fatalf(configInvalid("readBufferMultiplier", readBufferMultiplier), "Error in config readBufferMultiplier. Illegal value: %s. Legal values are positive integers", readBufferMultiplier)
		} else if tmp < 1 || tmp > 65535 {
			Logger.Fatalf(configInvalid("readBufferMultiplier", readBufferMultiplier), "Error in config readBufferMultiplier. Illegal value: %s. Legal values are positive integers up to 65535", readBufferMultiplier)
		} else {
			config.readBufferMultiplier = uint16(tmp)
		}
	}
	if rcvBufSize := os.Getenv(prefix + "RCV_BUF_SIZE"); rcvBufSize != "" {
		tmp, err := strconv.Atoi(rcvBufSize)
		if err != nil {
			Logger.Fatalf(configInvalid("rcvBufSize", rcvBufSize), "Error in config rcvBufSize. Illegal value: %s. Legal values are positive integers", rcvBufSize)
		} else if tmp < 1 {
			Logger.Fatalf(configInvalid("rcvBufSize", rcvBufSize), "Error in config rcvBufSize. Illegal value: %s. Legal values are positive integers", rcvBufSize)
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
			Logger.Fatalf(configInvalid("maximumDecompressSize", maximumDecompressSize).With(
				logging.Str("airgap.config.source", prefix+"MAXIMUM_DECOMPRESS_SIZE"),
			), "Error converting maximumDecompressSize \"%s\" to integer value", maximumDecompressSize)
		}
	}
	if v := os.Getenv(prefix + "MAX_CACHE_ENTRIES"); v != "" {
		tmp, err := strconv.Atoi(v)
		if err != nil || tmp < 1 {
			Logger.Fatalf(configInvalid("maxCacheEntries", v).With(
				logging.Str("airgap.config.source", prefix+"MAX_CACHE_ENTRIES"),
			), "Error in env var %sMAX_CACHE_ENTRIES. Illegal value: %s. Legal values are positive integers", prefix, v)
		}
		Logger.Info(configStr(logging.EventConfigOverride, "maxCacheEntries", v).With(
			logging.Str("airgap.config.source", prefix+"MAX_CACHE_ENTRIES"),
		), "Overriding maxCacheEntries with environment variable: "+prefix+"MAX_CACHE_ENTRIES with value: "+v)
		config.maxCacheEntries = tmp
	}
	if v := os.Getenv(prefix + "MAX_NR_MESSAGES"); v != "" {
		tmp, err := strconv.Atoi(v)
		if err != nil || tmp < 1 || tmp > 65535 {
			Logger.Fatalf(configInvalid("maxNrMessages", v).With(
				logging.Str("airgap.config.source", prefix+"MAX_NR_MESSAGES"),
			), "Error in env var %sMAX_NR_MESSAGES. Illegal value: %s. Legal values are 1-65535", prefix, v)
		}
		Logger.Info(configStr(logging.EventConfigOverride, "maxNrMessages", v).With(
			logging.Str("airgap.config.source", prefix+"MAX_NR_MESSAGES"),
		), "Overriding maxNrMessages with environment variable: "+prefix+"MAX_NR_MESSAGES with value: "+v)
		config.maxNrMessages = tmp
	}
	if v := os.Getenv(prefix + "MAX_TCP_CONNECTIONS"); v != "" {
		tmp, err := strconv.Atoi(v)
		if err != nil || tmp < 1 {
			Logger.Fatalf(configInvalid("maxTCPConnections", v).With(
				logging.Str("airgap.config.source", prefix+"MAX_TCP_CONNECTIONS"),
			), "Error in env var %sMAX_TCP_CONNECTIONS. Illegal value: %s. Legal values are positive integers", prefix, v)
		}
		Logger.Info(configStr(logging.EventConfigOverride, "maxTCPConnections", v).With(
			logging.Str("airgap.config.source", prefix+"MAX_TCP_CONNECTIONS"),
		), "Overriding maxTCPConnections with environment variable: "+prefix+"MAX_TCP_CONNECTIONS with value: "+v)
		config.maxTCPConnections = tmp
	}
	if v := os.Getenv(prefix + "KEY_EXCHANGE_MIN_INTERVAL_SECS"); v != "" {
		tmp, err := strconv.Atoi(v)
		if err != nil || tmp < 0 {
			Logger.Fatalf(configInvalid("keyExchangeMinIntervalSecs", v).With(
				logging.Str("airgap.config.source", prefix+"KEY_EXCHANGE_MIN_INTERVAL_SECS"),
			), "Error in env var %sKEY_EXCHANGE_MIN_INTERVAL_SECS. Illegal value: %s. Legal values are 0 or higher", prefix, v)
		}
		Logger.Info(configStr(logging.EventConfigOverride, "keyExchangeMinIntervalSecs", v).With(
			logging.Str("airgap.config.source", prefix+"KEY_EXCHANGE_MIN_INTERVAL_SECS"),
		), "Overriding keyExchangeMinIntervalSecs with environment variable: "+prefix+"KEY_EXCHANGE_MIN_INTERVAL_SECS with value: "+v)
		config.keyExchangeMinIntervalSecs = tmp
	}
	if v := os.Getenv(prefix + "ENABLE_RXQ_OVFL"); v != "" {
		tmp := strings.ToLower(v)
		if tmp != "true" && tmp != "false" {
			Logger.Fatalf(configInvalid("enableRxqOvfl", v).With(
				logging.Str("airgap.config.source", prefix+"ENABLE_RXQ_OVFL"),
			), "Error in env var %sENABLE_RXQ_OVFL. Illegal value: %s. Legal values are true or false", prefix, v)
		}
		Logger.Info(configStr(logging.EventConfigOverride, "enableRxqOvfl", v).With(
			logging.Str("airgap.config.source", prefix+"ENABLE_RXQ_OVFL"),
		), "Overriding enableRxqOvfl with environment variable: "+prefix+"ENABLE_RXQ_OVFL with value: "+v)
		config.enableRxqOvfl = tmp == "true"
	}
	if v := os.Getenv(prefix + "TCP_TLS_CERT_FILE"); v != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "tcpTLSCertFile", v).With(
			logging.Str("airgap.config.source", prefix+"TCP_TLS_CERT_FILE"),
		), "Overriding tcpTLSCertFile with environment variable: "+prefix+"TCP_TLS_CERT_FILE with value: "+v)
		config.tcpTLSCertFile = v
	}
	if v, present := os.LookupEnv(prefix + "TCP_TLS_AUTH_MODE"); present {
		config.tcpTLSAuthMode = v
	}
	if v, present := os.LookupEnv(prefix + "TCP_TLS_AUTOMATIC_ROTATION"); present {
		if v != "true" && v != "false" {
			Logger.Fatal(configInvalid("tcpTLSAutomaticRotation", v).With(
				logging.Str("airgap.config.source", prefix+"TCP_TLS_AUTOMATIC_ROTATION"),
			), "invalid TCP_TLS_AUTOMATIC_ROTATION; expected true or false")
		}
		config.tcpTLSAutomaticRotation = v == "true"
	}
	if v, present := os.LookupEnv(prefix + "TCP_TLS_PIN_IDLE_SECONDS"); present {
		seconds, err := pinnedtls.ParsePinIdleSeconds(v)
		if err != nil {
			Logger.Fatal(configInvalid("tcpTLSPinIdleSeconds", v).With(
				logging.Str("airgap.config.source", prefix+"TCP_TLS_PIN_IDLE_SECONDS"),
				logging.ErrorType(err),
			), err)
		}
		config.tcpTLSPinIdleSeconds = seconds
	}
	if v, present := os.LookupEnv(prefix + "TCP_TLS_TRUSTED_KEYS_DIR"); present {
		config.tcpTLSTrustedKeysDir = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_KEY_FILE"); v != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "tcpTLSKeyFile", v).With(
			logging.Str("airgap.config.source", prefix+"TCP_TLS_KEY_FILE"),
		), "Overriding tcpTLSKeyFile with environment variable: "+prefix+"TCP_TLS_KEY_FILE with value: "+v)
		config.tcpTLSKeyFile = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_KEY_PASSWORD_FILE"); v != "" {
		Logger.Info(logging.Event(logging.EventConfigOverride,
			logging.Str("airgap.config.setting", "tcpTLSKeyPasswordFile"),
			logging.Str("airgap.config.source", prefix+"TCP_TLS_KEY_PASSWORD_FILE"),
		), "Overriding tcpTLSKeyPasswordFile with environment variable: "+prefix+"TCP_TLS_KEY_PASSWORD_FILE")
		config.tcpTLSKeyPasswordFile = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_CA_FILE"); v != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "tcpTLSCAFile", v).With(
			logging.Str("airgap.config.source", prefix+"TCP_TLS_CA_FILE"),
		), "Overriding tcpTLSCAFile with environment variable: "+prefix+"TCP_TLS_CA_FILE with value: "+v)
		config.tcpTLSCAFile = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_CLIENT_AUTH"); v != "" {
		mode := strings.ToLower(v)
		switch mode {
		case "none", "allow", "require":
			Logger.Info(configStr(logging.EventConfigOverride, "tcpTLSClientAuth", v).With(
				logging.Str("airgap.config.source", prefix+"TCP_TLS_CLIENT_AUTH"),
			), "Overriding tcpTLSClientAuth with environment variable: "+prefix+"TCP_TLS_CLIENT_AUTH with value: "+v)
			config.tcpTLSClientAuth = mode
		default:
			Logger.Fatalf(configInvalid("tcpTLSClientAuth", v).With(
				logging.Str("airgap.config.source", prefix+"TCP_TLS_CLIENT_AUTH"),
			), "Error in env var %sTCP_TLS_CLIENT_AUTH. Illegal value: %s. Legal values are none, allow, require", prefix, v)
		}
	}
	if v := os.Getenv(prefix + "TCP_TLS_CLIENT_CN_REGEX"); v != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "tcpTLSClientCNRegex", v).With(
			logging.Str("airgap.config.source", prefix+"TCP_TLS_CLIENT_CN_REGEX"),
		), "Overriding tcpTLSClientCNRegex with environment variable: "+prefix+"TCP_TLS_CLIENT_CN_REGEX with value: "+v)
		config.tcpTLSClientCNRegex = v
	}
	if v := os.Getenv(prefix + "TCP_TLS_CIPHER_SUITES"); v != "" {
		Logger.Info(configStr(logging.EventConfigOverride, "tcpTLSCipherSuites", v).With(
			logging.Str("airgap.config.source", prefix+"TCP_TLS_CIPHER_SUITES"),
		), "Overriding tcpTLSCipherSuites with environment variable: "+prefix+"TCP_TLS_CIPHER_SUITES with value: "+v)
		config.tcpTLSCipherSuites = v
	}
	return config
}

// The current configuration
func logConfiguration(config TransferConfiguration) {
	Logger.Infof(logging.Event(logging.EventConfigApplied,
		logging.Str("airgap.build.number", BuildNumber),
	), "Configuration for build number %s:", BuildNumber)
	Logger.Infof(configStr(logging.EventConfigApplied, "id", config.id), "  id: %s", config.id)
	Logger.Infof(configStr(logging.EventConfigApplied, "nic", config.nic), "  nic: %s", config.nic)
	Logger.Infof(configStr(logging.EventConfigApplied, "targetIP", config.targetIP), "  targetIP: %s", config.targetIP)
	Logger.Infof(configInt(logging.EventConfigApplied, "targetPort", int64(config.targetPort)), "  targetPort: %d", config.targetPort)
	Logger.Infof(configStr(logging.EventConfigApplied, "bootstrapServers", config.bootstrapServers), "  bootstrapServers: %s", config.bootstrapServers)
	Logger.Infof(configInt(logging.EventConfigApplied, "mtu", int64(config.mtu)), "  mtu: %d", config.mtu)
	Logger.Infof(configStr(logging.EventConfigApplied, "privateKeyGlob", config.privateKeyGlob), "  privateKeyGlob: %s", config.privateKeyGlob)
	Logger.Infof(configStr(logging.EventConfigApplied, "target", config.target), "  target: %s", config.target)
	Logger.Infof(configStr(logging.EventConfigApplied, "logLevel", config.logLevel), "  logLevel: %s", config.logLevel)
	Logger.Infof(configStr(logging.EventConfigApplied, "logFileName", config.logFileName), "  logFileName: %s", config.logFileName)
	Logger.Infof(configStr(logging.EventConfigApplied, "certFile", config.certFile), "  certFile: %s", config.certFile)
	Logger.Infof(configStr(logging.EventConfigApplied, "keyFile", config.keyFile), "  keyFile: %s", config.keyFile)
	Logger.Info(configStr(logging.EventConfigApplied, "keyPasswordFile", "[set]"), "  keyPasswordFile: [set]")
	Logger.Infof(configStr(logging.EventConfigApplied, "caFile", config.caFile), "  caFile: %s", config.caFile)
	Logger.Infof(configStr(logging.EventConfigApplied, "topicTranslations", config.topicTranslations), "  topicTranslations: %s", config.topicTranslations)
	Logger.Infof(configInt(logging.EventConfigApplied, "logStatistics", int64(config.logStatistics)), "  logStatistics: %d", config.logStatistics)
	Logger.Infof(configInt(logging.EventConfigApplied, "numReceivers", int64(config.numReceivers)), "  numReceivers: %d", config.numReceivers)
	Logger.Infof(configInt(logging.EventConfigApplied, "channelBufferSize", int64(config.channelBufferSize)), "  channelBufferSize: %d", config.channelBufferSize)
	Logger.Infof(configInt(logging.EventConfigApplied, "batchSize", int64(config.batchSize)), "  batchSize: %d", config.batchSize)
	Logger.Infof(configInt(logging.EventConfigApplied, "readBufferMultiplier", int64(config.readBufferMultiplier)), "  readBufferMultiplier: %d", config.readBufferMultiplier)
	Logger.Infof(configInt(logging.EventConfigApplied, "rcvBufSize", int64(config.rcvBufSize)), "  rcvBufSize: %d", config.rcvBufSize)
	Logger.Infof(configStr(logging.EventConfigApplied, "internalTopic", config.topic), "  internalTopic: %s", config.topic)
	Logger.Infof(configStr(logging.EventConfigApplied, "internalTopic", config.topic), "  internalTopic: %s", config.topic)
	if len(config.translations) > 0 {
		Logger.Infof(configStr(logging.EventConfigApplied, "topicTranslations", config.topicTranslations), "  topicTranslations: %s", config.topicTranslations)
	}
	Logger.Infof(configInt(logging.EventConfigApplied, "maximumDecompressSize", int64(config.maximumDecompressSize)), "  maximumDecompressSize: %d", config.maximumDecompressSize)
	Logger.Infof(configInt(logging.EventConfigApplied, "maxCacheEntries", int64(config.maxCacheEntries)), "  maxCacheEntries: %d", config.maxCacheEntries)
	Logger.Infof(configInt(logging.EventConfigApplied, "maxNrMessages", int64(config.maxNrMessages)), "  maxNrMessages: %d", config.maxNrMessages)
	Logger.Infof(configInt(logging.EventConfigApplied, "maxTCPConnections", int64(config.maxTCPConnections)), "  maxTCPConnections: %d", config.maxTCPConnections)
	Logger.Infof(configInt(logging.EventConfigApplied, "keyExchangeMinIntervalSecs", int64(config.keyExchangeMinIntervalSecs)), "  keyExchangeMinIntervalSecs: %d", config.keyExchangeMinIntervalSecs)
	Logger.Infof(configBool(logging.EventConfigApplied, "enableRxqOvfl", config.enableRxqOvfl), "  enableRxqOvfl: %t", config.enableRxqOvfl)
	if config.transport == "tcp" && config.tcpTLSCertFile != "" {
		Logger.Infof(configStr(logging.EventConfigApplied, "tcpTLSCertFile", config.tcpTLSCertFile), "  tcpTLSCertFile: %s", config.tcpTLSCertFile)
		Logger.Infof(configStr(logging.EventConfigApplied, "tcpTLSKeyFile", config.tcpTLSKeyFile), "  tcpTLSKeyFile: %s", config.tcpTLSKeyFile)
		Logger.Infof(configStr(logging.EventConfigApplied, "tcpTLSCAFile", config.tcpTLSCAFile), "  tcpTLSCAFile: %s", config.tcpTLSCAFile)
		Logger.Infof(configStr(logging.EventConfigApplied, "tcpTLSClientAuth", config.tcpTLSClientAuth), "  tcpTLSClientAuth: %s", config.tcpTLSClientAuth)
		if config.tcpTLSClientCNRegex != "" {
			Logger.Infof(configStr(logging.EventConfigApplied, "tcpTLSClientCNRegex", config.tcpTLSClientCNRegex), "  tcpTLSClientCNRegex: %s", config.tcpTLSClientCNRegex)
		}
		Logger.Infof(configStr(logging.EventConfigApplied, "tcpTLSAuthMode", config.tcpTLSAuthMode), "  tcpTLSAuthMode: %s", config.tcpTLSAuthMode)
		Logger.Infof(configBool(logging.EventConfigApplied, "tcpTLSAutomaticRotation", config.tcpTLSAutomaticRotation), "  tcpTLSAutomaticRotation: %t", config.tcpTLSAutomaticRotation)
		Logger.Infof(configInt(logging.EventConfigApplied, "tcpTLSPinIdleSeconds", int64(config.tcpTLSPinIdleSeconds)), "  tcpTLSPinIdleSeconds: %d", config.tcpTLSPinIdleSeconds)
		Logger.Infof(configStr(logging.EventConfigApplied, "tcpTLSTrustedKeysDir", config.tcpTLSTrustedKeysDir), "  tcpTLSTrustedKeysDir: %s", config.tcpTLSTrustedKeysDir)
		upper := strings.ToUpper(strings.TrimSpace(config.tcpTLSCipherSuites))
		if upper == "" || upper == "TLS1.3" {
			Logger.Info(configStr(logging.EventConfigApplied, "tcpTLSCipherSuites", "TLS1.3").With(
				logging.Str("tls.protocol.version", "1.3"),
			), "  tcpTLS protocol: TLS 1.3 (enforced)")
		} else {
			Logger.Infof(configStr(logging.EventConfigApplied, "tcpTLSCipherSuites", config.tcpTLSCipherSuites).With(
				logging.Str("tls.protocol.version", "1.2"),
			), "  tcpTLS protocol: TLS 1.2 with cipher suites: %s", config.tcpTLSCipherSuites)
		}
	}
}

// parseCommandLineOverrides parses --name=value arguments and applies them to the config struct.
func parseCommandLineOverrides(args []string, config TransferConfiguration) TransferConfiguration {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		kv := strings.SplitN(arg[2:], "=", 2)
		if len(kv) != 2 {
			Logger.Warnf(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.config.setting", arg),
			), "Ignoring malformed command line override: %s", arg)
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
				Logger.Fatal(configInvalid(key, value).Merge(logging.Err(err)), err)
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
		case "bootstrapServers":
			config.bootstrapServers = value
		case "mtu":
			if value == "auto" {
				config.mtu = 0
			} else {
				v, err := strconv.Atoi(value)
				if err != nil || v < 0 || v > 65535 {
					Logger.Fatalf(configInvalid("mtu", value), "Error parsing --mtu: %s", value)
				}
				config.mtu = uint16(v) // Safe: bounds checked above [0, 65535]
			}
		case "privateKeyFiles":
			config.privateKeyGlob = value
		case "target":
			switch value {
			case "kafka", "cmd", "null":
				config.target = value
			default:
				Logger.Fatalf(configInvalid("target", value), "Unknown --target %s. Legal values are: kafka, cmd, null", value)
			}
		case "transport":
			t := strings.ToLower(value)
			if t != "udp" && t != "tcp" {
				Logger.Fatalf(configInvalid("transport", value), "Unknown --transport %s. Legal values are: udp, tcp", value)
			}
			config.transport = t
		case "logLevel":
			config.logLevel = strings.ToUpper(value)
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
		case "topicTranslations":
			config.topicTranslations = value
		case "logStatistics":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 || v > 60 {
				Logger.Fatalf(configInvalid("logStatistics", value), "Error parsing --logStatistics: %s", value)
			}
			config.logStatistics = int32(v) // Safe: bounds checked above [0, 60]
		case "numReceivers":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				Logger.Fatalf(configInvalid("numReceivers", value), "Error parsing --numReceivers: %s", value)
			}
			config.numReceivers = v
		case "channelBufferSize":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				Logger.Fatalf(configInvalid("channelBufferSize", value), "Error parsing --channelBufferSize: %s", value)
			}
			config.channelBufferSize = v
		case "batchSize":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 || v > 512 {
				Logger.Fatalf(configInvalid("batchSize", value), "Error parsing --batchSize: %s", value)
			}
			config.batchSize = v
		case "readBufferMultiplier":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 || v > 65535 {
				Logger.Fatalf(configInvalid("readBufferMultiplier", value), "Error parsing --readBufferMultiplier: %s", value)
			}
			config.readBufferMultiplier = uint16(v) // Safe: bounds checked above [1, 65535]
		case "rcvBufSize":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				Logger.Fatalf(configInvalid("rcvBufSize", value), "Error parsing --rcvBufSize: %s", value)
			}
			config.rcvBufSize = v
		case "internalTopic":
			config.topic = value
		case "maximumDecompressSize":
			v, err := convertUnitSufix(value)
			if err != nil {
				Logger.Fatalf(configInvalid("maximumDecompressSize", value), "Error parsing --maximumDecompressSize: %s", value)
			}
			config.maximumDecompressSize = v
		case "maxCacheEntries":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				Logger.Fatalf(configInvalid("maxCacheEntries", value), "Error parsing --maxCacheEntries: %s", value)
			}
			config.maxCacheEntries = v
		case "maxNrMessages":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 || v > 65535 {
				Logger.Fatalf(configInvalid("maxNrMessages", value), "Error parsing --maxNrMessages: %s", value)
			}
			config.maxNrMessages = v
		case "maxTCPConnections":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				Logger.Fatalf(configInvalid("maxTCPConnections", value), "Error parsing --maxTCPConnections: %s", value)
			}
			config.maxTCPConnections = v
		case "keyExchangeMinIntervalSecs":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				Logger.Fatalf(configInvalid("keyExchangeMinIntervalSecs", value), "Error parsing --keyExchangeMinIntervalSecs: %s", value)
			}
			config.keyExchangeMinIntervalSecs = v
		case "enableRxqOvfl":
			v, err := strconv.ParseBool(value)
			if err != nil {
				Logger.Fatalf(configInvalid("enableRxqOvfl", value).Merge(logging.Err(err)), "Error parsing --enableRxqOvfl: %s. Legal values are true or false", value)
			}
			config.enableRxqOvfl = v
		case "tcpTLSCertFile":
			config.tcpTLSCertFile = value
		case "tcpTLSKeyFile":
			config.tcpTLSKeyFile = value
		case "tcpTLSKeyPasswordFile":
			config.tcpTLSKeyPasswordFile = value
		case "tcpTLSCAFile":
			config.tcpTLSCAFile = value
		case "tcpTLSClientAuth":
			mode := strings.ToLower(value)
			switch mode {
			case "none", "allow", "require":
				config.tcpTLSClientAuth = mode
			default:
				Logger.Fatalf(configInvalid("tcpTLSClientAuth", value), "Error in --tcpTLSClientAuth. Illegal value: %s. Legal values are none, allow, require", value)
			}
		case "tcpTLSClientCNRegex":
			config.tcpTLSClientCNRegex = value
		case "tcpTLSCipherSuites":
			config.tcpTLSCipherSuites = value
		case "tcpTLSAuthMode":
			config.tcpTLSAuthMode = value
		case "tcpTLSTrustedKeysDir":
			config.tcpTLSTrustedKeysDir = value
		case "tcpTLSAutomaticRotation":
			if value != "true" && value != "false" {
				Logger.Fatal(configInvalid("tcpTLSAutomaticRotation", value), "invalid --tcpTLSAutomaticRotation; expected true or false")
			}
			config.tcpTLSAutomaticRotation = value == "true"
		case "tcpTLSPinIdleSeconds":
			seconds, err := pinnedtls.ParsePinIdleSeconds(value)
			if err != nil {
				Logger.Fatal(configInvalid("tcpTLSPinIdleSeconds", value).Merge(logging.Err(err)), err)
			}
			config.tcpTLSPinIdleSeconds = seconds
		default:
			Logger.Warnf(configStr(logging.EventConfigOverride, key, value), "Unknown command line override: --%s", key)
		}
	}
	return config
}

// Check the configuration. On fail, will terminate the application
func checkConfiguration(result TransferConfiguration) TransferConfiguration {
	Logger.Info(logging.Event(logging.EventConfigLoad), "Validating the configuration...")

	// Must have an id
	if result.id == "" {
		Logger.Fatal(configInvalid("id", result.id), "Missing required configuration: id")
	}
	if result.nic == "" {
		Logger.Fatal(configInvalid("nic", result.nic), "Missing required configuration: nic")
	}
	if result.targetIP == "" {
		Logger.Fatal(configInvalid("targetIP", result.targetIP), "Missing required configuration: targetIP")
	}
	if result.targetPort < 0 || result.targetPort > 65535 {
		Logger.Fatal(configInvalid("targetPort", strconv.Itoa(result.targetPort)), "Invalid configuration: targetPort must be between 0 and 65535")
	}
	if result.target != "kafka" && result.target != "cmd" && result.target != "null" {
		Logger.Fatalf(configInvalid("target", result.target), "Unknown target '%s'. Valid targets are: 'kafka', 'cmd', 'null'", result.target)
	}
	if result.target == "kafka" && result.bootstrapServers == "" {
		Logger.Fatal(configInvalid("bootstrapServers", result.bootstrapServers), "Missing required configuration: bootstrapServers")
	}
	if result.logFileName != "" {
		// Check that the logFileName is a valid file name
		file, err := os.OpenFile(result.logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			Logger.Fatalf(configInvalid("logFileName", result.logFileName).Merge(logging.Err(err)), "Cannot open log file '%s' for writing: %v", result.logFileName, err)
		}
		defer file.Close()
		// Check that we can write to that file
		if _, err := file.WriteString(""); err != nil {
			Logger.Fatalf(configInvalid("logFileName", result.logFileName).Merge(logging.Err(err)), "Cannot write to log file '%s': %v", result.logFileName, err)
		}
	}
	if result.logLevel != "" {
		Logger.SetLogLevel(result.logLevel)
		var tmp = Logger.GetLogLevel()
		if (tmp == result.logLevel) == false {
			Logger.Fatalf(configInvalid("logLevel", result.logLevel), "Error in config logLevel. Illegal value: %s. Legal values are DEBUG, INFO, WARN, ERROR, FATAL", result.logLevel)
		} else {
			Logger.Infof(configStr(logging.EventConfigParse, "logLevel", tmp), "logLevel: %s", tmp)
		}
	}

	// if one of certFile, keyFile or caFile is given, they all must be
	if result.certFile != "" || result.keyFile != "" || result.caFile != "" {
		if result.certFile == "" {
			Logger.Fatalf(configInvalid("certFile", result.certFile), "Missing required configuration: certFile")
		}
		if result.keyFile == "" {
			Logger.Fatalf(configInvalid("keyFile", result.keyFile), "Missing required configuration: keyFile")
		}
		if result.caFile == "" {
			Logger.Fatalf(configInvalid("caFile", result.caFile), "Missing required configuration: caFile")
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
	if result.topicTranslations != "" {
		if err := json.Unmarshal([]byte(result.topicTranslations), &result.translations); err != nil {
			Logger.Fatalf(configInvalid("topicTranslations", result.topicTranslations).Merge(logging.Err(err)), "Error in config topicTranslations. Illegal value: %s. Legal values are JSON objects 'from': 'to', ...", result.topicTranslations)
		}
	}
	if result.numReceivers < 1 {
		Logger.Fatal(configInvalid("numReceivers", strconv.Itoa(result.numReceivers)), "Invalid configuration: numReceivers must be a positive integer")
	}
	if result.channelBufferSize < 1 {
		Logger.Fatal(configInvalid("channelBufferSize", strconv.Itoa(result.channelBufferSize)), "Invalid configuration: channelBufferSize must be a positive integer")
	}
	if result.batchSize < 1 || result.batchSize > 512 {
		Logger.Fatal(configInvalid("batchSize", strconv.Itoa(result.batchSize)), "Invalid configuration: batchSize must be a positive integer up to 512")
	}
	if result.rcvBufSize < 1 {
		Logger.Fatal(configInvalid("rcvBufSize", strconv.Itoa(result.rcvBufSize)), "Invalid configuration: rcvBufSize must be a positive integer")
	}
	// Validate TCP TLS settings
	if err := result.validateTLSMode(); err != nil {
		Logger.Fatal(logging.Event(logging.EventConfigInvalid).Merge(logging.Err(err)), err)
	}
	if result.tcpTLSAuthMode == "pinned" {
		if _, err := result.BuildTLSConfig(); err != nil {
			Logger.Fatal(logging.Event(logging.EventConfigInvalid).Merge(logging.Err(err)), err)
		}
	}
	if result.tcpTLSCertFile != "" || result.tcpTLSKeyFile != "" {
		if result.tcpTLSCertFile == "" {
			Logger.Fatal(configInvalid("tcpTLSCertFile", result.tcpTLSCertFile), "Missing required configuration: tcpTLSCertFile (required when tcpTLSKeyFile is set)")
		}
		if result.tcpTLSKeyFile == "" {
			Logger.Fatal(configInvalid("tcpTLSKeyFile", result.tcpTLSKeyFile), "Missing required configuration: tcpTLSKeyFile (required when tcpTLSCertFile is set)")
		}
	}
	if result.tcpTLSClientAuth != "none" && result.tcpTLSClientAuth != "allow" && result.tcpTLSClientAuth != "require" {
		Logger.Fatalf(configInvalid("tcpTLSClientAuth", result.tcpTLSClientAuth), "Invalid tcpTLSClientAuth '%s'. Legal values are: none, allow, require", result.tcpTLSClientAuth)
	}
	if result.tcpTLSAuthMode != "pinned" && (result.tcpTLSClientAuth == "allow" || result.tcpTLSClientAuth == "require") && result.tcpTLSCAFile == "" {
		Logger.Fatal(configInvalid("tcpTLSCAFile", result.tcpTLSCAFile), "Missing required configuration: tcpTLSCAFile (required when tcpTLSClientAuth is allow or require)")
	}
	if result.tcpTLSClientAuth != "none" && result.transport != "tcp" {
		Logger.Fatal(configInvalid("tcpTLSClientAuth", result.tcpTLSClientAuth), "Error: tcpTLSClientAuth requires transport=tcp")
	}
	if result.tcpTLSCertFile != "" && result.transport != "tcp" {
		Logger.Fatal(configInvalid("tcpTLSCertFile", result.tcpTLSCertFile), "Error: tcpTLSCertFile requires transport=tcp")
	}

	logConfiguration(result)
	Logger.Info(logging.Event(logging.EventConfigApplied), "Configuration OK")
	if result.logFileName != "" {
		if err := Logger.SetLogFile(result.logFileName); err != nil {
			Logger.Fatal(configInvalid("logFileName", result.logFileName).Merge(logging.Err(err)), err)
		}
	}
	result.WarnProductionConfiguration("startup")
	return result
}
func intPow(base int, exp uint) int {
	out := 1
	for _ = range exp {
		out = out * base
	}
	return out
}
func convertUnitSufix(size string) (int, error) {
	Logger.Debugf(configStr(logging.EventConfigParse, "size", size), "Size is %s", size)
	if num, found := strings.CutSuffix(size, "B"); found {
		Logger.Debugf(configStr(logging.EventConfigParse, "sizeUnit", "B"), "Found B")
		mult := 1
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult, nil
		}
	}
	if num, found := strings.CutSuffix(size, "KiB"); found {
		Logger.Debugf(configStr(logging.EventConfigParse, "sizeUnit", "KiB"), "Found KiB")
		mult := intPow(2, 10)
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult, nil
		}
	}
	if num, found := strings.CutSuffix(size, "MiB"); found {
		Logger.Debugf(configStr(logging.EventConfigParse, "sizeUnit", "MiB"), "Found MiB")
		mult := intPow(2, 20)
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult, nil
		}
	}
	if num, found := strings.CutSuffix(size, "GiB"); found {
		mult := intPow(2, 30)
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult, nil
		}
	}
	if num, found := strings.CutSuffix(size, "TiB"); found {
		mult := intPow(2, 40)
		out, err := strconv.Atoi(num)
		if err == nil {
			return out * mult, nil
		}
	}
	out, err := strconv.Atoi(size)
	if err != nil {
		return 0, err
	}
	return out, nil
}
