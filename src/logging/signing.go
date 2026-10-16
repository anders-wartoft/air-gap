package logging

import (
	"bufio"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	checkpointDomain = "airgap-log-checkpoint-v1"
	signatureAlg     = "Ed25519"
)

type logSigner struct {
	certificate    *x509.Certificate
	certificatePEM string
	signer         crypto.Signer
	signerID       string
	keyID          string
}

// SigningCertificateRotationAnnouncement is both the locally hashed log body
// and the downstream STATUS payload for an authenticated key transition.
type SigningCertificateRotationAnnouncement struct {
	Schema               string `json:"schema"`
	Event                string `json:"event"`
	SignerID             string `json:"signer_id"`
	OldKeyID             string `json:"old_key_id"`
	NewKeyID             string `json:"new_key_id"`
	NewCertificateSerial string `json:"new_certificate_serial"`
	NewCertificatePEM    string `json:"new_certificate_pem"`
}

// SigningCertificateRotation describes a reload outcome. Status is non-empty
// only when the old signer authenticated a changed certificate.
type SigningCertificateRotation struct {
	Changed       bool
	Authenticated bool
	OldKeyID      string
	NewKeyID      string
	Status        []byte
}

// ConfigureSigningCertificate loads and validates a dedicated log-signing
// certificate and private key. Empty paths disable signing.
func (s *integrityState) ConfigureSigningCertificate(certPath, keyPath, signerID, requiredEKU string) error {
	signer, err := loadLogSigner(certPath, keyPath, signerID, requiredEKU)
	if err != nil {
		return err
	}
	s.signer = signer
	return nil
}

func loadLogSigner(certPath, keyPath, signerID, requiredEKU string) (*logSigner, error) {
	certPath = strings.TrimSpace(certPath)
	keyPath = strings.TrimSpace(keyPath)
	signerID = strings.TrimSpace(signerID)
	requiredEKU = strings.TrimSpace(requiredEKU)

	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("log-signing-cert and log-signing-key must be configured together")
	}
	certificatePEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read log-signing-cert %q: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read log-signing-key %q: %w", keyPath, err)
	}

	certificate, privateKey, err := parseSigningMaterial(certificatePEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if len(certificate.URIs) != 1 {
		return nil, fmt.Errorf("log-signing certificate must have exactly one URI SAN")
	}
	certificateSignerID := certificate.URIs[0].String()
	if signerID == "" {
		signerID = certificateSignerID
	} else if signerID != certificateSignerID {
		return nil, fmt.Errorf("log-signer-id %q does not match certificate URI SAN %q", signerID, certificateSignerID)
	}
	if certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return nil, fmt.Errorf("log-signing certificate must allow digitalSignature")
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return nil, fmt.Errorf("log-signing certificate is not valid at %s", now.UTC().Format(time.RFC3339))
	}
	if requiredEKU != "" {
		oid, err := parseOID(requiredEKU)
		if err != nil {
			return nil, fmt.Errorf("invalid log-signing-eku %q: %w", requiredEKU, err)
		}
		found := false
		for _, candidate := range certificate.UnknownExtKeyUsage {
			if candidate.Equal(oid) {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("log-signing certificate does not contain required EKU %s", requiredEKU)
		}
	}

	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("log-signing certificate must use an Ed25519 key")
	}
	derPublicKey, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal log-signing public key: %w", err)
	}
	keyFingerprint := sha256.Sum256(derPublicKey)

	return &logSigner{
		certificate:    certificate,
		certificatePEM: string(certificatePEM),
		signer:         privateKey,
		signerID:       signerID,
		keyID:          "sha256:" + hex.EncodeToString(keyFingerprint[:]),
	}, nil
}

func parseSigningMaterial(certificatePEM, keyPEM []byte) (*x509.Certificate, crypto.Signer, error) {
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("log-signing-cert does not contain a PEM CERTIFICATE")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse log-signing-cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("log-signing-key does not contain a PEM private key")
	}
	privateKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		if _, rsaErr := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); rsaErr == nil {
			return nil, nil, fmt.Errorf("log-signing-key must contain an Ed25519 PKCS#8 key, got RSA")
		} else {
			return nil, nil, fmt.Errorf("parse log-signing-key PKCS#8: %w", err)
		}
	}
	signer, ok := privateKey.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("log-signing-key is not a signing key")
	}
	certificatePublic, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal certificate public key: %w", err)
	}
	privatePublic, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key public key: %w", err)
	}
	if !equalBytes(certificatePublic, privatePublic) {
		return nil, nil, fmt.Errorf("log-signing certificate and private key do not match")
	}
	return certificate, signer, nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

func parseOID(value string) (asn1.ObjectIdentifier, error) {
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("must contain at least two numeric components")
	}
	oid := make(asn1.ObjectIdentifier, len(parts))
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return nil, fmt.Errorf("invalid component %q", part)
		}
		oid[index] = number
	}
	return oid, nil
}

func frameSigningValue(value []byte) []byte {
	result := make([]byte, 4+len(value))
	binary.BigEndian.PutUint32(result, uint32(len(value)))
	copy(result[4:], value)
	return result
}

func frameSigningString(value string) []byte {
	return frameSigningValue([]byte(value))
}

func uint64SigningValue(value uint64) []byte {
	result := make([]byte, 8)
	binary.BigEndian.PutUint64(result, value)
	return result
}

type checkpointStatementFields struct {
	version          string
	hashAlgorithm    string
	signatureAlg     string
	signerID         string
	keyID            string
	runID            string
	segmentID        string
	windowID         string
	serviceName      string
	serviceInstance  string
	processStartNano uint64
	anchorFrom       uint64
	anchorTo         uint64
	previousChain    []byte
	finalChain       []byte
}

func buildCheckpointStatement(fields checkpointStatementFields) []byte {
	statement := []byte(checkpointDomain)
	statement = append(statement, 0)
	statement = append(statement, frameSigningString(fields.version)...)
	statement = append(statement, frameSigningString(fields.hashAlgorithm)...)
	statement = append(statement, frameSigningString(fields.signatureAlg)...)
	statement = append(statement, frameSigningString(fields.signerID)...)
	statement = append(statement, frameSigningString(fields.keyID)...)
	statement = append(statement, frameSigningString(fields.runID)...)
	statement = append(statement, frameSigningString(fields.segmentID)...)
	statement = append(statement, frameSigningString(fields.windowID)...)
	statement = append(statement, frameSigningString(fields.serviceName)...)
	statement = append(statement, frameSigningString(fields.serviceInstance)...)
	statement = append(statement, uint64SigningValue(fields.processStartNano)...)
	statement = append(statement, uint64SigningValue(fields.anchorFrom)...)
	statement = append(statement, uint64SigningValue(fields.anchorTo)...)
	statement = append(statement, frameSigningValue(fields.previousChain)...)
	statement = append(statement, frameSigningValue(fields.finalChain)...)
	return statement
}

func attrString(attrs []Attr, key string) (string, bool) {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value, true
		}
	}
	return "", false
}

func (s *integrityState) signAnchor(anchorAttrs, stamped []Attr) ([]Attr, error) {
	if s.signer == nil {
		return nil, nil
	}
	if !s.enabled || !s.chainingEnabled() {
		return nil, fmt.Errorf("log signing requires hash and hash-chain")
	}
	now := time.Now()
	if now.Before(s.signer.certificate.NotBefore) || now.After(s.signer.certificate.NotAfter) {
		return nil, fmt.Errorf("log-signing certificate is not valid at %s", now.UTC().Format(time.RFC3339))
	}

	chainPrevious, ok := attrString(stamped, AttrChainPrev)
	if !ok {
		return nil, fmt.Errorf("signed anchor is missing %s", AttrChainPrev)
	}
	chainFinal, ok := attrString(stamped, AttrChain)
	if !ok {
		return nil, fmt.Errorf("signed anchor is missing %s", AttrChain)
	}
	from, ok := attrString(anchorAttrs, AttrAnchorFrom)
	if !ok {
		return nil, fmt.Errorf("signed anchor is missing %s", AttrAnchorFrom)
	}
	to, ok := attrString(anchorAttrs, AttrAnchorTo)
	if !ok {
		return nil, fmt.Errorf("signed anchor is missing %s", AttrAnchorTo)
	}
	fromNumber, err := strconv.ParseUint(from, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid anchor start %q: %w", from, err)
	}
	toNumber, err := strconv.ParseUint(to, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid anchor end %q: %w", to, err)
	}
	previousBytes, err := hex.DecodeString(chainPrevious)
	if err != nil {
		return nil, fmt.Errorf("invalid previous chain value: %w", err)
	}
	finalBytes, err := hex.DecodeString(chainFinal)
	if err != nil {
		return nil, fmt.Errorf("invalid final chain value: %w", err)
	}
	runID, ok := attrString(stamped, AttrRunID)
	if !ok {
		return nil, fmt.Errorf("signed anchor is missing %s", AttrRunID)
	}
	segmentID, ok := attrString(stamped, AttrSegmentID)
	if !ok {
		return nil, fmt.Errorf("signed anchor is missing %s", AttrSegmentID)
	}
	windowID, ok := attrString(stamped, AttrWindowID)
	if !ok {
		return nil, fmt.Errorf("signed anchor is missing %s", AttrWindowID)
	}

	statement := buildCheckpointStatement(checkpointStatementFields{
		version:          integrityVersion,
		hashAlgorithm:    s.algorithm.name,
		signatureAlg:     signatureAlg,
		signerID:         s.signer.signerID,
		keyID:            s.signer.keyID,
		runID:            runID,
		segmentID:        segmentID,
		windowID:         windowID,
		serviceName:      resourceServiceName(),
		serviceInstance:  resourceInstanceID(),
		processStartNano: uint64(processStartUnixNano()),
		anchorFrom:       fromNumber,
		anchorTo:         toNumber,
		previousChain:    previousBytes,
		finalChain:       finalBytes,
	})

	hash := s.algorithm.factory()
	_, _ = hash.Write(statement)
	checkpointHash := hash.Sum(nil)
	signature, err := s.signer.signer.Sign(nil, checkpointHash, crypto.Hash(0))
	if err != nil {
		return nil, fmt.Errorf("sign log checkpoint: %w", err)
	}

	return []Attr{
		{Key: AttrCheckpointHash, Value: hex.EncodeToString(checkpointHash)},
		{Key: AttrSignature, Value: base64.StdEncoding.EncodeToString(signature)},
		{Key: AttrSignatureAlg, Value: signatureAlg},
		{Key: AttrSignerID, Value: s.signer.signerID},
		{Key: AttrKeyID, Value: s.signer.keyID},
		{Key: AttrCertificateSerial, Value: s.signer.certificate.SerialNumber.String()},
		{Key: AttrCertificateDER, Value: base64.StdEncoding.EncodeToString(s.signer.certificate.Raw)},
		{Key: AttrProcessStart, Value: strconv.FormatInt(processStartUnixNano(), 10), IsInt: true},
	}, nil
}

// SetSigningCertificate configures an optional dedicated log-signing key.
func (l *LoggerType) SetSigningCertificate(certPath, keyPath, signerID, requiredEKU string) error {
	emitMu.Lock()
	defer emitMu.Unlock()
	if err := integrity.ConfigureSigningCertificate(certPath, keyPath, signerID, requiredEKU); err != nil {
		return err
	}
	signingConfig = signingConfiguration{
		certificatePath: strings.TrimSpace(certPath),
		keyPath:         strings.TrimSpace(keyPath),
		signerID:        strings.TrimSpace(signerID),
		requiredEKU:     strings.TrimSpace(requiredEKU),
	}
	return nil
}

// RotateSigningCertificate validates a candidate before changing any state.
// When an old signer exists, a local STATUS event and forced checkpoint
// authenticate the candidate certificate with the old key before activation.
func (l *LoggerType) RotateSigningCertificate(
	certPath, keyPath, signerID, requiredEKU string,
) (SigningCertificateRotation, error) {
	candidate, err := loadLogSigner(certPath, keyPath, signerID, requiredEKU)
	if err != nil {
		return SigningCertificateRotation{}, err
	}
	if candidate == nil {
		emitMu.Lock()
		unsigned := integrity.signer == nil
		emitMu.Unlock()
		if unsigned {
			return SigningCertificateRotation{}, nil
		}
		return SigningCertificateRotation{}, fmt.Errorf(
			"certificate rotation cannot disable log signing",
		)
	}

	emitMu.Lock()
	defer emitMu.Unlock()

	old := integrity.signer
	result := SigningCertificateRotation{
		Changed:  true,
		NewKeyID: candidate.keyID,
	}
	if old == nil {
		integrity.signer = candidate
		signingConfig = signingConfiguration{
			certificatePath: strings.TrimSpace(certPath),
			keyPath:         strings.TrimSpace(keyPath),
			signerID:        strings.TrimSpace(signerID),
			requiredEKU:     strings.TrimSpace(requiredEKU),
		}
		return result, nil
	}
	result.OldKeyID = old.keyID
	if equalBytes(old.certificate.Raw, candidate.certificate.Raw) {
		result.Changed = false
		signingConfig = signingConfiguration{
			certificatePath: strings.TrimSpace(certPath),
			keyPath:         strings.TrimSpace(keyPath),
			signerID:        strings.TrimSpace(signerID),
			requiredEKU:     strings.TrimSpace(requiredEKU),
		}
		return result, nil
	}
	if !integrity.chainingEnabled() {
		return SigningCertificateRotation{}, fmt.Errorf(
			"log-signing certificate rotation requires hash and hash-chain",
		)
	}

	announcement := SigningCertificateRotationAnnouncement{
		Schema:               "airgap-log-signing-certificate-rotation-v1",
		Event:                EventLogSigningCertificateRotate,
		SignerID:             candidate.signerID,
		OldKeyID:             old.keyID,
		NewKeyID:             candidate.keyID,
		NewCertificateSerial: candidate.certificate.SerialNumber.String(),
		NewCertificatePEM:    candidate.certificatePEM,
	}
	status, err := json.Marshal(announcement)
	if err != nil {
		return SigningCertificateRotation{}, fmt.Errorf(
			"encode signing certificate rotation: %w", err,
		)
	}

	now := time.Now()
	writeRecord(
		now,
		INFO,
		string(status),
		Event(
			EventLogSigningCertificateRotate,
			Str("airgap.log.signing.old.key.id", old.keyID),
			Str("airgap.log.signing.new.key.id", candidate.keyID),
			Str("airgap.log.signing.new.certificate.serial", candidate.certificate.SerialNumber.String()),
		),
		nil,
	)
	if err := writeAnchorLocked(now); err != nil {
		return SigningCertificateRotation{}, fmt.Errorf(
			"authenticate signing certificate rotation: %w", err,
		)
	}

	integrity.signer = candidate
	signingConfig = signingConfiguration{
		certificatePath: strings.TrimSpace(certPath),
		keyPath:         strings.TrimSpace(keyPath),
		signerID:        strings.TrimSpace(signerID),
		requiredEKU:     strings.TrimSpace(requiredEKU),
	}
	result.Authenticated = true
	result.Status = status
	return result, nil
}

// ReloadSigningCertificate reads only signing options from a service
// key=value configuration and performs an atomic authenticated rotation.
func (l *LoggerType) ReloadSigningCertificate(configPath string) (SigningCertificateRotation, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return SigningCertificateRotation{}, fmt.Errorf(
			"read signing configuration %q: %w", configPath, err,
		)
	}
	defer file.Close()

	emitMu.Lock()
	current := signingConfig
	emitMu.Unlock()
	values := map[string]string{
		OptionSigningCertificate: current.certificatePath,
		OptionSigningKey:         current.keyPath,
		OptionSignerID:           current.signerID,
		OptionSigningEKU:         current.requiredEKU,
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		switch key {
		case OptionSigningCertificate, OptionSigningKey, OptionSignerID, OptionSigningEKU:
			values[key] = strings.TrimSpace(parts[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return SigningCertificateRotation{}, fmt.Errorf(
			"read signing configuration %q: %w", configPath, err,
		)
	}
	return l.RotateSigningCertificate(
		values[OptionSigningCertificate],
		values[OptionSigningKey],
		values[OptionSignerID],
		values[OptionSigningEKU],
	)
}

// ClearSigningCertificate disables checkpoint signatures.
func (l *LoggerType) ClearSigningCertificate() {
	emitMu.Lock()
	integrity.signer = nil
	signingConfig = signingConfiguration{}
	emitMu.Unlock()
}
