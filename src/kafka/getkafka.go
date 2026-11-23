package kafka

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"os"
	"strings"

	"golang.org/x/crypto/pbkdf2"
	"sitia.nu/airgap/src/logging"
)

var (
	tlsConfigParameters *TLSConfiguration = nil
)

var Logger = logging.Logger

type TLSConfiguration struct {
	CertFile        string
	KeyFile         string
	CAFile          string
	KeyPasswordFile string
}

func SetTLSConfigParameters(certFile, keyFile, caFile, keyPasswordFile string) (*tls.Config, error) {
	tlsConfigParameters = &TLSConfiguration{
		CertFile:        certFile,
		KeyFile:         keyFile,
		CAFile:          caFile,
		KeyPasswordFile: keyPasswordFile,
	}
	return createTLSConfig()
}

func keyPasswordFileReadError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("failed to read key password file: file does not exist")
	case errors.Is(err, os.ErrPermission):
		return errors.New("failed to read key password file: permission denied")
	default:
		return errors.New("failed to read key password file")
	}
}

// LoadTLSCertificate loads a TLS certificate and private key, decrypting the key if a
// password file is provided. This is exported so that non-Kafka TLS clients (e.g. TCP
// transport) can reuse the same key-decryption logic.
func LoadTLSCertificate(certFile, keyFile, keyPasswordFile string) (tls.Certificate, error) {
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to read key file: %w", err)
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return tls.Certificate{}, fmt.Errorf("failed to decode PEM block from key file")
	}

	isEncrypted := false
	if block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED" {
		isEncrypted = true
	} else if block.Type == "ENCRYPTED PRIVATE KEY" {
		isEncrypted = true
	}

	if isEncrypted && keyPasswordFile == "" {
		return tls.Certificate{}, fmt.Errorf("key file %q is encrypted but no keyPasswordFile is configured", keyFile)
	}

	if keyPasswordFile != "" {
		passphrase, err := os.ReadFile(keyPasswordFile)
		if err != nil {
			return tls.Certificate{}, keyPasswordFileReadError(err)
		}
		passphraseBytes := []byte(strings.TrimSpace(string(passphrase)))

		var decryptedPEM []byte
		if block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED" {
			decryptedDER, derr := decryptLegacyPEM(block, passphraseBytes)
			if derr != nil {
				return tls.Certificate{}, fmt.Errorf("failed to decrypt key file: %w", derr)
			}
			decryptedPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: decryptedDER})
		} else if block.Type == "ENCRYPTED PRIVATE KEY" {
			decryptedDER, derr := decryptPKCS8EncryptedKey(block, passphraseBytes)
			if derr != nil {
				return tls.Certificate{}, fmt.Errorf("failed to decrypt PKCS#8 key file: %w", derr)
			}
			decryptedPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: decryptedDER})
		} else {
			return tls.Certificate{}, fmt.Errorf("unsupported encrypted key format: %s", block.Type)
		}

		certPEM, err := os.ReadFile(certFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to read certificate file: %w", err)
		}
		return tls.X509KeyPair(certPEM, decryptedPEM)
	}

	return tls.LoadX509KeyPair(certFile, keyFile)
}

// decryptLegacyPEM decrypts a legacy PEM encrypted private key (the old format with DEK-Info header)
func decryptLegacyPEM(block *pem.Block, password []byte) ([]byte, error) {
	// Get encryption info from headers
	dekInfo := block.Headers["DEK-Info"]
	if dekInfo == "" {
		return nil, fmt.Errorf("missing DEK-Info header")
	}

	// Parse DEK-Info: algorithm,iv
	parts := strings.Split(dekInfo, ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid DEK-Info format")
	}
	algorithm := parts[0]
	ivHex := parts[1]

	// Decode IV from hex
	iv := make([]byte, len(ivHex)/2)
	for i := 0; i < len(iv); i++ {
		fmt.Sscanf(ivHex[i*2:i*2+2], "%02x", &iv[i])
	}

	var key []byte
	var blockCipher cipher.Block
	var err error

	// Derive key based on algorithm
	switch algorithm {
	case "AES-256-CBC":
		key = deriveKey(password, iv[:8], 32)
		blockCipher, err = aes.NewCipher(key)
	case "AES-192-CBC":
		key = deriveKey(password, iv[:8], 24)
		blockCipher, err = aes.NewCipher(key)
	case "AES-128-CBC":
		key = deriveKey(password, iv[:8], 16)
		blockCipher, err = aes.NewCipher(key)
	case "DES-EDE3-CBC":
		key = deriveKey(password, iv[:8], 24)
		blockCipher, err = des.NewTripleDESCipher(key)
	default:
		return nil, fmt.Errorf("unsupported encryption algorithm: %s", algorithm)
	}

	if err != nil {
		return nil, err
	}

	// Decrypt using CBC mode
	mode := cipher.NewCBCDecrypter(blockCipher, iv)
	decrypted := make([]byte, len(block.Bytes))
	mode.CryptBlocks(decrypted, block.Bytes)

	// Remove PKCS#7 padding
	if len(decrypted) == 0 {
		return nil, fmt.Errorf("decrypted data is empty")
	}
	paddingLen := int(decrypted[len(decrypted)-1])
	if paddingLen > len(decrypted) || paddingLen == 0 {
		return nil, fmt.Errorf("invalid padding")
	}
	decrypted = decrypted[:len(decrypted)-paddingLen]

	return decrypted, nil
}

// deriveKey derives a key from password and salt using MD5 (OpenSSL EVP_BytesToKey)
//
// This use of MD5 is for compatibility with legacy OpenSSL-encrypted PEM files only.
// It is not used for new password hashing or general cryptography.
// codeql[weak-sensitive-data-hashing]: Required for legacy OpenSSL PEM decryption compatibility, not for new password hashing
func deriveKey(password, salt []byte, keyLen int) []byte {
	var derived []byte
	var previous []byte

	for len(derived) < keyLen {
		h := md5.New()
		h.Write(previous)
		h.Write(password)
		h.Write(salt)
		hash := h.Sum(nil)
		derived = append(derived, hash...)
		previous = hash
	}

	return derived[:keyLen]
}

// decryptPKCS8EncryptedKey decrypts an "ENCRYPTED PRIVATE KEY" PEM block
// (PKCS#8 EncryptedPrivateKeyInfo, produced by OpenSSL 3.x "openssl genrsa -aes256").
// Supports PBES2 with PBKDF2 (HMAC-SHA1/SHA-256/SHA-384/SHA-512) and AES-{128,192,256}-CBC.
// The returned bytes are the inner unencrypted PrivateKeyInfo DER, suitable for
// pem.Block{Type: "PRIVATE KEY", Bytes: …}.
func decryptPKCS8EncryptedKey(block *pem.Block, password []byte) ([]byte, error) {
	// Local ASN.1 structure definitions
	type encKeyInfo struct {
		Algorithm pkix.AlgorithmIdentifier
		Encrypted []byte
	}
	type pbes2Def struct {
		KDF     pkix.AlgorithmIdentifier
		Encrypt pkix.AlgorithmIdentifier
	}
	type kdfParamsDef struct {
		Salt   []byte
		Iters  int
		KeyLen int                      `asn1:"optional"`
		PRF    pkix.AlgorithmIdentifier `asn1:"optional"`
	}

	// OIDs
	oidPBES2 := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2 := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACsha1 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACsha256 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACsha384 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 10}
	oidHMACsha512 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}

	var info encKeyInfo
	if _, err := asn1.Unmarshal(block.Bytes, &info); err != nil {
		return nil, fmt.Errorf("failed to parse EncryptedPrivateKeyInfo: %w", err)
	}
	if !info.Algorithm.Algorithm.Equal(oidPBES2) {
		return nil, fmt.Errorf("unsupported PKCS#8 encryption scheme %v (only PBES2 is supported)", info.Algorithm.Algorithm)
	}

	var p2 pbes2Def
	if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &p2); err != nil {
		return nil, fmt.Errorf("failed to parse PBES2 params: %w", err)
	}
	if !p2.KDF.Algorithm.Equal(oidPBKDF2) {
		return nil, fmt.Errorf("unsupported PBES2 KDF %v (only PBKDF2 is supported)", p2.KDF.Algorithm)
	}

	var kdf kdfParamsDef
	if _, err := asn1.Unmarshal(p2.KDF.Parameters.FullBytes, &kdf); err != nil {
		return nil, fmt.Errorf("failed to parse PBKDF2 params: %w", err)
	}

	// Select PRF hash function
	var newHash func() hash.Hash
	var defaultKeyLen int
	prfOID := kdf.PRF.Algorithm
	switch {
	case len(prfOID) == 0 || prfOID.Equal(oidHMACsha1):
		newHash, defaultKeyLen = sha1.New, 20
	case prfOID.Equal(oidHMACsha256):
		newHash, defaultKeyLen = sha256.New, 32
	case prfOID.Equal(oidHMACsha384):
		newHash, defaultKeyLen = sha512.New384, 48
	case prfOID.Equal(oidHMACsha512):
		newHash, defaultKeyLen = sha512.New, 64
	default:
		return nil, fmt.Errorf("unsupported PBKDF2 PRF %v", prfOID)
	}

	// Determine AES key length from KDF params or cipher OID
	keyLen := kdf.KeyLen
	if keyLen == 0 {
		encOID := p2.Encrypt.Algorithm
		switch {
		case encOID.Equal(oidAES128CBC):
			keyLen = 16
		case encOID.Equal(oidAES192CBC):
			keyLen = 24
		case encOID.Equal(oidAES256CBC):
			keyLen = 32
		default:
			keyLen = defaultKeyLen
		}
	}

	// Derive key via PBKDF2
	key := pbkdf2.Key(password, kdf.Salt, kdf.Iters, keyLen, newHash)

	// Extract IV (OCTET STRING in Encrypt parameters)
	var iv []byte
	if _, err := asn1.Unmarshal(p2.Encrypt.Parameters.FullBytes, &iv); err != nil {
		return nil, fmt.Errorf("failed to parse encryption IV: %w", err)
	}

	// Verify cipher is an AES-CBC variant
	encOID := p2.Encrypt.Algorithm
	if !encOID.Equal(oidAES128CBC) && !encOID.Equal(oidAES192CBC) && !encOID.Equal(oidAES256CBC) {
		return nil, fmt.Errorf("unsupported encryption algorithm %v (only AES-CBC variants are supported)", encOID)
	}

	blockCipher, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	if len(info.Encrypted)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted data length %d is not a multiple of AES block size", len(info.Encrypted))
	}

	decrypted := make([]byte, len(info.Encrypted))
	cipher.NewCBCDecrypter(blockCipher, iv).CryptBlocks(decrypted, info.Encrypted)

	// Remove PKCS#7 padding
	if len(decrypted) == 0 {
		return nil, fmt.Errorf("decrypted data is empty")
	}
	pad := int(decrypted[len(decrypted)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(decrypted) {
		return nil, fmt.Errorf("invalid PKCS#7 padding (wrong password?)")
	}
	for i := len(decrypted) - pad; i < len(decrypted); i++ {
		if int(decrypted[i]) != pad {
			return nil, fmt.Errorf("invalid PKCS#7 padding byte at position %d (wrong password?)", i)
		}
	}
	return decrypted[:len(decrypted)-pad], nil
}

// Creates a new TLS configuration for the Kafka client
func createTLSConfig() (*tls.Config, error) {
	var cert tls.Certificate
	var err error

	// Read the key file to check if it's encrypted
	keyPEM, err := os.ReadFile(tlsConfigParameters.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}

	// Decode the PEM block to check if it's encrypted
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from key file")
	}

	keyAttrs := logging.Event(logging.EventCryptoKeyLoad,
		logging.Str("file.path", tlsConfigParameters.KeyFile),
		logging.Str("airgap.crypto.pem.block_type", block.Type),
		logging.Str("airgap.crypto.pem.proc_type", block.Headers["Proc-Type"]),
	)
	Logger.Debugf(keyAttrs, "DEBUG: PEM block type: '%s'", block.Type)
	Logger.Debugf(keyAttrs, "DEBUG: PEM Proc-Type header: '%s'", block.Headers["Proc-Type"])
	Logger.Debug(keyAttrs.With(logging.Bool("airgap.crypto.key_password_file.set", true)), "DEBUG: keyPasswordFile: [set]")

	// Check if the key file is encrypted
	isEncrypted := false
	if block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED" {
		isEncrypted = true
	} else if block.Type == "ENCRYPTED PRIVATE KEY" {
		isEncrypted = true
	}

	// If key is encrypted but no password file provided, fail
	if isEncrypted && tlsConfigParameters.KeyPasswordFile == "" {
		return nil, fmt.Errorf("key file '%s' is encrypted but no keyPasswordFile is configured. Please provide keyPasswordFile in configuration", tlsConfigParameters.KeyFile)
	}

	// Check if we need to decrypt the key file
	if tlsConfigParameters.KeyPasswordFile != "" {
		Logger.Infof(logging.Event(logging.EventCryptoDecrypt,
			logging.Str("file.path", tlsConfigParameters.KeyFile),
			logging.Str("airgap.crypto.stage", "start"),
		), "Attempting to decrypt encrypted key file: %s", tlsConfigParameters.KeyFile)

		// Read the passphrase from the password file
		passphrase, err := os.ReadFile(tlsConfigParameters.KeyPasswordFile)
		if err != nil {
			return nil, keyPasswordFileReadError(err)
		}
		passphraseBytes := []byte(strings.TrimSpace(string(passphrase)))

		var decryptedPEM []byte
		switch {
		case block.Type == "RSA PRIVATE KEY" && block.Headers["Proc-Type"] == "4,ENCRYPTED":
			Logger.Info(logging.Event(logging.EventCryptoDecrypt,
				logging.Str("file.path", tlsConfigParameters.KeyFile),
				logging.Str("airgap.crypto.key_format", "pem.legacy"),
				logging.Str("airgap.crypto.stage", "detected"),
			), "Detected legacy PEM encrypted format (DEK-Info), attempting decryption...")
			decryptedDER, derr := decryptLegacyPEM(block, passphraseBytes)
			if derr != nil {
				return nil, fmt.Errorf("failed to decrypt legacy PEM key: %w", derr)
			}
			Logger.Info(logging.Event(logging.EventCryptoDecrypt,
				logging.Str("file.path", tlsConfigParameters.KeyFile),
				logging.Str("airgap.crypto.key_format", "pem.legacy"),
				logging.Str("airgap.crypto.stage", "complete"),
			), "Successfully decrypted key using legacy PEM format")
			decryptedPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: decryptedDER})
		case block.Type == "ENCRYPTED PRIVATE KEY":
			Logger.Info(logging.Event(logging.EventCryptoDecrypt,
				logging.Str("file.path", tlsConfigParameters.KeyFile),
				logging.Str("airgap.crypto.key_format", "pkcs8"),
				logging.Str("airgap.crypto.stage", "detected"),
			), "Detected PKCS#8 encrypted format, attempting decryption...")
			decryptedDER, derr := decryptPKCS8EncryptedKey(block, passphraseBytes)
			if derr != nil {
				return nil, fmt.Errorf("failed to decrypt PKCS#8 key: %w", derr)
			}
			Logger.Info(logging.Event(logging.EventCryptoDecrypt,
				logging.Str("file.path", tlsConfigParameters.KeyFile),
				logging.Str("airgap.crypto.key_format", "pkcs8"),
				logging.Str("airgap.crypto.stage", "complete"),
			), "Successfully decrypted key using PKCS#8 format")
			decryptedPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: decryptedDER})
		default:
			return nil, fmt.Errorf("unsupported encrypted key format: %s", block.Type)
		}

		// Read the certificate file
		certPEM, err := os.ReadFile(tlsConfigParameters.CertFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read certificate file: %w", err)
		}

		// Load certificate and decrypted key
		cert, err = tls.X509KeyPair(certPEM, decryptedPEM)
		if err != nil {
			return nil, fmt.Errorf("failed to load X509 key pair: %w", err)
		}
	} else {
		// No decryption needed - use files directly
		cert, err = tls.LoadX509KeyPair(tlsConfigParameters.CertFile, tlsConfigParameters.KeyFile)
		if err != nil {
			return nil, err
		}
	}

	// Load CA cert
	caCert, err := os.ReadFile(tlsConfigParameters.CAFile)
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
	}
	return tlsConfig, nil
}
