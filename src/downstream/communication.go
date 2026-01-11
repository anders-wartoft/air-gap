package downstream

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IBM/sarama"
	"github.com/google/uuid"
	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/protocol"
)

// Send a message to Kafka or stdout
func sendMessage(messageType uint8, id string, topic string, message []byte) {
	// If topic is "" then the configuration prohibits sending messages
	if topic == "" {
		if Logger.CanLog(logging.DEBUG) {
			Logger.Debugf("Not sending message %s since topic name is empty", string(message))
		}
		return
	}
	partition := int32(0)
	// If no id, just create one
	var messageKey string
	if id == "" {
		messageKey = uuid.New().String()
	} else {
		messageKey = id
	}
	if Logger.CanLog(logging.DEBUG) {
		Logger.Debugf("id: %s", id)
		// Print the first 40 bytes of the message, add ... if truncated
		preview := string(message)
		var limit = 60
		if len(message) > limit {
			preview = string(message[:limit]) + "..."
		}
		Logger.Debugf("%s Sending cleartext message to %s: '%s' on topic: %s", config.id, config.target, preview, topic)
	}
	// If this is an error message, prepend a timestamp
	var toSend []byte = message
	if protocol.IsMessageType(messageType, protocol.TYPE_ERROR) || protocol.IsMessageType(messageType, protocol.TYPE_STATUS) {
		toSend = fmt.Appendf(nil, "%s %s %s", protocol.GetTimestamp(), config.id, string(message))
		Logger.Print(string(toSend))
	}

	// Send the data to Kafka
	if config.target == "kafka" {
		// Send the result to Kafka
		kafka.WriteToKafka(messageKey, topic, partition, toSend)
	} else {
		// send to stdout
		os.Stdout.Write(toSend)
		os.Stdout.Write([]byte("\n"))
	}
}

// readNewKey updates the symmetric key
func readNewKey(message []byte) string {
	// We have our private key in memory. This message
	// contains the new symmetric key to use, enrypted with
	// our public key
	// The message can look like this:
	// KEY_UPDATE#(here comes binary data)
	Logger.Debug("Message length is " + strconv.Itoa(len(message)))
	if len(message) < 11 {
		return fmt.Sprintf("Message length for the new key is %d, but it must be more than 11", len(message))
	}
	payload := message[11:]

	// Just to be sure, we read the private keys again
	// since they can have changed on disk
	readPrivateKeys(config.privateKeyGlob)

	// Decrypt the symmetric key with the private keys until we get the message:
	// KEY_UPDATE# as the first 11 bytes
	//	Logger.Debug("Checking " + fmt.Sprint(len(config.keyInfos)) + " keys for new symmetric key")
	for i := range config.keyInfos {
		Logger.Debug("Checking key " + fmt.Sprint(i))
		privateKey := config.keyInfos[i].privateKey
		decrypted, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, payload, nil)
		if err == nil {
			if string(decrypted[0:11]) == "KEY_UPDATE#" {
				// We got the correct key
				sendMessage(protocol.TYPE_STATUS, "", config.topic, fmt.Appendf(nil, "Got new symmetric key: %s", config.keyInfos[i].privateKeyFilename))
				//				Logger.Debugf("Got new symmetric key: %s", decrypted)
				config.key = decrypted[11:]
				return config.keyInfos[i].privateKeyFilename
			}
		}
	}
	Logger.Errorf("Couldn't decrypt the new symmetric key with any of the %v private keys loaded with %s", len(config.keyInfos), config.privateKeyGlob)
	sendMessage(protocol.TYPE_ERROR, "", config.topic, fmt.Appendf(nil, "Can't decrypt the new symmetric key. Tried all %v private keys\n", len(config.keyInfos)))
	return "ERROR: No key found that can decrypt the new symmetric key"
}

// Remove, update or read keys from the OS from the privateKeyGlob
// We want the keys to be in memory so we can decrypt messages. The keys
// in memory should be the same as the keys on disk. If a key is removed
// from disk, it should be removed from memory. If a key is added to disk,
// it should be added to memory. If a key is changed on disk, the new key
// should be loaded into memory.
func readPrivateKeys(fileGlob string) []KeyInfo {
	// Read the files
	var fileNames []string
	var err error
	fileNames, err = filepath.Glob(fileGlob)

	if err != nil {
		// Send message to Kafka
		message := fmt.Sprintf("Can't read private key files from file glob %s, %v", fileGlob, err)
		sendMessage(protocol.TYPE_ERROR, "", config.topic, []byte(message))
	}

	// Purge from memory if not on disk
	// First, iterate over the array and save an array of indicies that should be removed
	purgeIndices := []int{}
	for j := 0; j < len(config.keyInfos); j++ {
		found := false
		for _, fileName := range fileNames {
			if config.keyInfos[j].privateKeyFilename == fileName {
				found = true
			}
		}
		if !found {
			Logger.Printf("Removing private key from memory: %s", config.keyInfos[j].privateKeyFilename)
			purgeIndices = append(purgeIndices, j)
		}
	}
	// Here, we remove the indicies, start from the back of the array so we don't have to re-calculate indicies
	for i := len(purgeIndices) - 1; i >= 0; i-- {
		// Go splice format
		config.keyInfos = append(config.keyInfos[:purgeIndices[i]], config.keyInfos[purgeIndices[i]+1:]...)
	}

	// Now, iterate over all file names matching the glob and try to load as a key
	for _, fileName := range fileNames {
		file, err := os.Open(fileName)
		if err != nil {
			// File open error
			sendMessage(protocol.TYPE_ERROR, "", config.topic, []byte(fmt.Appendf(nil, "Can't read private key from file %s, %v", fileName, err)))
		}
		defer file.Close()

		var builder strings.Builder

		// Read the key as a text file
		// First, get the base64 encoded data
		scanner := bufio.NewScanner(file)
		var inFile = false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "-----") {
				inFile = !inFile
			} else if inFile {
				builder.WriteString(line)
			}
		}

		// Now add to a variable
		b64Data := builder.String()

		// Decode the base64 encoded variable
		derData, err := base64.StdEncoding.DecodeString(b64Data)
		if err != nil {
			sendMessage(protocol.TYPE_ERROR, "", config.topic, fmt.Appendf(nil, "Error decoding base64 data: %v", err))
		}
		// hash the key so we can see if it's loaded already
		derString := fmt.Sprintf("%x", sha256.Sum256(derData))

		// Check if we have the key loaded already
		addKey := true
		for j := 0; j < len(config.keyInfos); j++ {
			if config.keyInfos[j].privateKeyHash == derString {
				sendMessage(protocol.TYPE_ERROR, "", config.topic, fmt.Appendf(nil, "Key file %s already loaded", config.keyInfos[j].privateKeyFilename))
				addKey = false
			}
		}

		// Parse the DER encoded private key
		key, err := x509.ParsePKCS8PrivateKey(derData)
		if err != nil {
			sendMessage(protocol.TYPE_ERROR, "", config.topic, fmt.Appendf(nil, "Error parsing private key: %v", err))
		}

		privateKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			sendMessage(protocol.TYPE_ERROR, "", config.topic, fmt.Appendf(nil, "Private key is not of type *rsa.PrivateKey"))
		}

		if addKey {
			keyInfo := KeyInfo{
				privateKey:         privateKey,
				privateKeyFilename: fileName,
				privateKeyHash:     derString,
			}
			config.keyInfos = append(config.keyInfos, keyInfo)
			sendMessage(protocol.TYPE_STATUS, "", config.topic, fmt.Appendf(nil, "Successfully loaded key file: %s", fileName))
		}
	}
	return config.keyInfos
}

func connectToKafka(config TransferConfiguration) {
	Logger.Printf("Connecting to %s\n", config.bootstrapServers)

	kafka.ConfigureSaramaLogger()

	// Check if we have TLS to Kafka
	conf := sarama.NewConfig()
	// Tell Sarama to use manual partitioning - we provide the partition in the ProducerMessage
	conf.Producer.Partitioner = sarama.NewManualPartitioner
	// Check if we have TLS to Kafka
	if config.certFile != "" || config.keyFile != "" || config.caFile != "" {
		Logger.Print("Using TLS for Kafka")
		tlsConfig, err := kafka.SetTLSConfigParameters(config.certFile, config.keyFile, config.caFile, config.keyPasswordFile)
		if err != nil {
			Logger.Panicf("Error setting TLS config parameters: %v", err)
		}
		conf.Net.TLS.Enable = true
		conf.Net.TLS.Config = tlsConfig
	}
	conf.Producer.RequiredAcks = sarama.WaitForLocal
	conf.Producer.Return.Successes = true
	conf.Producer.Return.Errors = true
	conf.Net.KeepAlive = 30 * time.Second
	conf.ClientID = config.clientID
	producer, err := sarama.NewAsyncProducer(strings.Split(config.bootstrapServers, ","), conf)
	if err != nil {
		Logger.Panicf("Error creating producer: %v\n", err)
	}
	go func() {
		for err := range producer.Errors() {
			Logger.Println("Failed to send message:", err)
		}
	}()
	go func() {
		for msg := range producer.Successes() {
			if Logger.CanLog(logging.DEBUG) {
				// Log at most 80 characters from msg. Note that msg can be shorter
				valueBytes, err := msg.Value.Encode()
				keyBytes, _ := msg.Key.Encode()
				if err != nil {
					Logger.Debugf("Error encoding message value: %v", err)
				} else {
					if len(valueBytes) > 120 {
						Logger.Debugf("Message sent: key=%s partition=%d %s ...", string(keyBytes), msg.Partition, string(valueBytes[:120]))
					} else {
						Logger.Debugf("Message sent: key=%s partition=%d %s", string(keyBytes), msg.Partition, string(valueBytes))
					}
				}
			}
		}
	}()
	Logger.Printf("Connected to Kafka. Creating startup message...\n")
	kafka.SetProducer(producer, config.batchSize)
	// Set error callback to report Kafka errors to status
	kafka.SetErrorCallback(UpdateKafkaStatus)
	// Start a new goroutine for sending kafka messages
	kafka.StartBackgroundThread()
}
