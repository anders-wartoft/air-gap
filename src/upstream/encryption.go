package upstream

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"time"

	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/protocol"
	"sitia.nu/airgap/src/udp"
)

// Create a new symmetric key for the encryption.
func createNewKey() []byte {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	if err != nil {
		Logger.Panicf(logging.Event(logging.EventCryptoKeyLoad,
			logging.Failure(),
			logging.Str("airgap.crypto.key.kind", "symmetric"),
			logging.ErrorType(err),
		), "Error generating key: %v", err)
	}
	return key
}

// Read the public key from a file and return it.
func readPublicKey(fileName string) *rsa.PublicKey {
	file, err := os.Open(fileName)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventCryptoKeyLoad,
			logging.Failure(),
			logging.Str("airgap.crypto.key.kind", "public"),
			logging.Str("file.path", fileName),
			logging.ErrorType(err),
		), "Can't read public key from file %s, %v", fileName, err)
	}
	defer file.Close()

	var builder strings.Builder

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		builder.WriteString(line)
		builder.WriteString("\n")
	}

	block, _ := pem.Decode([]byte(builder.String()))
	if block == nil {
		Logger.Fatal(logging.Event(logging.EventCryptoKeyLoad,
			logging.Failure(),
			logging.Str("airgap.crypto.key.kind", "public"),
			logging.Str("file.path", fileName),
			logging.Str("error.type", "pem.decode"),
		), "failed to parse PEM block containing the public key")
	}
	Logger.Infof(logging.Event(logging.EventCryptoKeyLoad,
		logging.Str("airgap.crypto.key.kind", "public"),
		logging.Str("file.path", fileName),
		logging.Str("airgap.crypto.pem.block_type", block.Type),
	), "block type %s", block.Type)

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		Logger.Fatalf(logging.Event(logging.EventCryptoKeyLoad,
			logging.Failure(),
			logging.Str("airgap.crypto.key.kind", "public"),
			logging.Str("file.path", fileName),
			logging.Str("error.type", "x509.parse"),
			logging.ErrorType(err),
		), "failed to parse DER encoded certificate: %v", err)
	}

	pubKey, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		Logger.Fatal(logging.Event(logging.EventCryptoKeyLoad,
			logging.Failure(),
			logging.Str("airgap.crypto.key.kind", "public"),
			logging.Str("file.path", fileName),
			logging.Str("error.type", "key.not.rsa"),
		), "public key is not of type *rsa.PublicKey")
	}
	return pubKey
}

// Generate a new symmetric key and encrypt the key with the
// public key from the certificate.
// Return the encrypted symmetric key
func generateNewKey() []byte {
	// Create a symmetric key
	config.newkey = createNewKey()

	// Get the public key for downstream
	config.publicKey = readPublicKey(config.publicKeyFile)

	// We would like to encrypt an [] byte that contains
	// - A Guard: KEY_UPDATE#
	// - The new key

	// Create a Guard
	// In downstream.go we decrypt key exchange messages with
	// every private key we have until we find a decrypted message
	// that starts with this string.
	guard := "KEY_UPDATE#"
	toEncrypt := []byte(guard)
	toEncrypt = append(toEncrypt, config.newkey...)

	// Encrypt the initial key with the downstream public key
	encryptedKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, config.publicKey, toEncrypt, nil)

	if err != nil {
		Logger.Panicf(logging.Event(logging.EventCryptoEncrypt,
			logging.Failure(),
			logging.Str("airgap.crypto.key.kind", "symmetric"),
			logging.Str("file.path", config.publicKeyFile),
			logging.ErrorType(err),
		), "Error encrypting key: %v", err)
	}

	// encryptedKey now contains the encrypted key
	return encryptedKey
}

// Create a message with message type KEY_EXCHANGE
// and message id KEY_UPDATE# (the guard)
// Set the next time for key re-generation
func sendNewKey(conn *udp.UDPConn) {
	byteKey := generateNewKey()
	guard := "KEY_UPDATE#"
	toSend := []byte(guard)
	toSend = append(toSend, byteKey...)

	Logger.Info(logging.Event(logging.EventCryptoKeyExchange,
		logging.Str("airgap.message.id", guard),
		logging.Int("airgap.message.size", int64(len(toSend))),
	), "Sending key exchange message...")
	messages := protocol.FormatMessage(protocol.TYPE_KEY_EXCHANGE, "KEY_UPDATE#", toSend, config.payloadSize)
	if conn != nil {
		udpErr := conn.SendMessages(messages)
		if udpErr != nil {
			Logger.Errorf(logging.Event(logging.EventCryptoKeyExchange,
				logging.Failure(),
				logging.Str("airgap.message.id", guard),
				logging.Int("airgap.message.count", int64(len(messages))),
				logging.ErrorType(udpErr),
			), "Error sending encryption key (will continue): %v", udpErr)
		}
	}
	// Now, we should be using the new key too
	config.key = config.newkey

	// Set the time for the next key generation
	if config.generateNewSymmetricKeyEvery > 0 {
		nextKeyGeneration = time.Now().Add(time.Duration(config.generateNewSymmetricKeyEvery) * time.Second)
	}

}
