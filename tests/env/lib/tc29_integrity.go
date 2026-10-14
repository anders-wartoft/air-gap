package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	chainDomain      = "airgap-log-chain-v1"
	checkpointDomain = "airgap-log-checkpoint-v1"
	signingEKU       = "1.3.6.1.4.1.55555.29.1"
)

type anyValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type logRecord struct {
	Body struct {
		StringValue *string `json:"stringValue,omitempty"`
	} `json:"body"`
	Attributes []keyValue `json:"attributes"`
}

type scopeLogs struct {
	LogRecords []logRecord `json:"logRecords"`
}

type resourceLogs struct {
	Resource struct {
		Attributes []keyValue `json:"attributes"`
	} `json:"resource"`
	ScopeLogs []scopeLogs `json:"scopeLogs"`
}

type envelope struct {
	ResourceLogs []resourceLogs `json:"resourceLogs"`
}

type parsedRecord struct {
	raw      []byte
	body     string
	attrs    map[string]anyValue
	resource map[string]anyValue
	seq      uint64
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: tc29_integrity.go generate|verify [options]")
	}
	switch os.Args[1] {
	case "generate":
		generate(os.Args[2:])
	case "verify":
		verify(os.Args[2:])
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func generate(arguments []string) {
	flags := flag.NewFlagSet("generate", flag.ExitOnError)
	directory := flags.String("dir", "", "output directory")
	name := flags.String("name", "signer", "output filename prefix")
	signerID := flags.String("signer-id", "urn:airgap:test:tc29", "URI SAN signer identity")
	if err := flags.Parse(arguments); err != nil {
		fatalf("parse generate options: %v", err)
	}
	if *directory == "" {
		fatalf("--dir is required")
	}
	if err := os.MkdirAll(*directory, 0o700); err != nil {
		fatalf("create certificate directory: %v", err)
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatalf("generate Ed25519 key: %v", err)
	}
	uri, err := url.Parse(*signerID)
	if err != nil {
		fatalf("parse signer URI: %v", err)
	}
	eku := mustOID(signingEKU)
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		fatalf("generate certificate serial: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "TC-29 dedicated log signer"},
		URIs:                  []*url.URL{uri},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		UnknownExtKeyUsage:    []asn1.ObjectIdentifier{eku},
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		fatalf("create signing certificate: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		fatalf("marshal private key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		fatalf("marshal public key: %v", err)
	}

	writePEM(filepath.Join(*directory, *name+".crt"), "CERTIFICATE", certificateDER, 0o644)
	writePEM(filepath.Join(*directory, *name+".key"), "PRIVATE KEY", privateDER, 0o600)
	writePEM(filepath.Join(*directory, *name+".pub"), "PUBLIC KEY", publicDER, 0o644)
	fmt.Printf("generated %s key-id=sha256:%x eku=%s signer=%s\n",
		*name, sha256.Sum256(publicDER), signingEKU, *signerID)
}

func mustOID(value string) asn1.ObjectIdentifier {
	parts := strings.Split(value, ".")
	result := make(asn1.ObjectIdentifier, len(parts))
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			fatalf("invalid OID %q", value)
		}
		result[index] = number
	}
	return result
}

func writePEM(path, kind string, der []byte, mode os.FileMode) {
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), mode); err != nil {
		fatalf("write %s: %v", path, err)
	}
}

func verify(arguments []string) {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	logPath := flags.String("log", "", "application log")
	certificatePath := flags.String("certificate", "", "trusted signing certificate")
	windowPath := flags.String("window-out", "", "write selected window as JSON Lines")
	expectedService := flags.String("expect-service", "", "expected service.name")
	expectedSigner := flags.String("expect-signer", "", "expected signer identity")
	if err := flags.Parse(arguments); err != nil {
		fatalf("parse verify options: %v", err)
	}
	if *logPath == "" || *certificatePath == "" {
		fatalf("--log and --certificate are required")
	}

	records, err := readRecords(*logPath)
	if err != nil {
		fatalf("%v", err)
	}
	window, anchor, err := firstSignedWindow(records)
	if err != nil {
		fatalf("%v", err)
	}
	if err := verifyWindow(window, anchor, *certificatePath, *expectedService, *expectedSigner); err != nil {
		fatalf("%v", err)
	}
	if *windowPath != "" {
		var output bytes.Buffer
		for _, record := range window {
			output.Write(record.raw)
			output.WriteByte('\n')
		}
		if err := os.WriteFile(*windowPath, output.Bytes(), 0o600); err != nil {
			fatalf("write selected window: %v", err)
		}
	}
	fmt.Printf("verified Version 1 window %s sequences %d..%d signer=%s key=%s\n",
		stringAttr(anchor.attrs, "airgap.log.window.id"),
		intAttr(anchor.attrs, "airgap.log.anchor.from"),
		intAttr(anchor.attrs, "airgap.log.anchor.to"),
		stringAttr(anchor.attrs, "airgap.log.signer.id"),
		stringAttr(anchor.attrs, "airgap.log.key.id"))
}

func readRecords(path string) ([]parsedRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer file.Close()

	var result []parsedRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var payload envelope
		if err := json.Unmarshal(line, &payload); err != nil {
			continue
		}
		if len(payload.ResourceLogs) != 1 ||
			len(payload.ResourceLogs[0].ScopeLogs) != 1 ||
			len(payload.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
			return nil, errors.New("OTLP line does not contain exactly one LogRecord")
		}
		wire := payload.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
		if wire.Body.StringValue == nil {
			return nil, errors.New("OTLP LogRecord has no string body")
		}
		attrs, err := attributeMap(wire.Attributes)
		if err != nil {
			return nil, err
		}
		sequence, ok := optionalIntAttr(attrs, "airgap.log.seq")
		if !ok {
			continue
		}
		resource, err := attributeMap(payload.ResourceLogs[0].Resource.Attributes)
		if err != nil {
			return nil, err
		}
		result = append(result, parsedRecord{
			raw:      append([]byte(nil), line...),
			body:     *wire.Body.StringValue,
			attrs:    attrs,
			resource: resource,
			seq:      sequence,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	return result, nil
}

func firstSignedWindow(records []parsedRecord) ([]parsedRecord, parsedRecord, error) {
	for _, candidate := range records {
		if boolAttr(candidate.attrs, "airgap.log.anchor") &&
			stringAttr(candidate.attrs, "airgap.log.signature") != "" {
			runID := stringAttr(candidate.attrs, "airgap.log.run.id")
			windowID := stringAttr(candidate.attrs, "airgap.log.window.id")
			var window []parsedRecord
			for _, record := range records {
				if stringAttr(record.attrs, "airgap.log.run.id") == runID &&
					stringAttr(record.attrs, "airgap.log.window.id") == windowID {
					window = append(window, record)
				}
			}
			sort.Slice(window, func(i, j int) bool { return window[i].seq < window[j].seq })
			return window, candidate, nil
		}
	}
	return nil, parsedRecord{}, errors.New("no signed Version 1 anchor found")
}

func verifyWindow(window []parsedRecord, anchor parsedRecord, certificatePath, expectedService, expectedSigner string) error {
	if len(window) == 0 {
		return errors.New("signed window is empty")
	}
	from := intAttr(anchor.attrs, "airgap.log.anchor.from")
	to := intAttr(anchor.attrs, "airgap.log.anchor.to")
	if uint64(len(window)) != to-from+1 {
		return fmt.Errorf("window has %d records, anchor covers %d..%d", len(window), from, to)
	}
	if intAttr(anchor.attrs, "airgap.log.chain.length") != 10 {
		return fmt.Errorf("chain length is %d, want 10", intAttr(anchor.attrs, "airgap.log.chain.length"))
	}
	for index, record := range window {
		if record.seq != from+uint64(index) {
			return fmt.Errorf("sequence discontinuity at index %d: got %d want %d", index, record.seq, from+uint64(index))
		}
	}
	if !boolAttr(window[len(window)-1].attrs, "airgap.log.anchor") {
		return errors.New("last record is not the signed anchor")
	}

	runID := stringAttr(anchor.attrs, "airgap.log.run.id")
	segmentID := stringAttr(anchor.attrs, "airgap.log.segment.id")
	windowID := stringAttr(anchor.attrs, "airgap.log.window.id")
	var previous []byte
	for index, record := range window {
		if stringAttr(record.attrs, "airgap.log.hash.alg") != "SHA256" {
			return fmt.Errorf("sequence %d does not use SHA256", record.seq)
		}
		if stringAttr(record.attrs, "airgap.log.run.id") != runID ||
			stringAttr(record.attrs, "airgap.log.segment.id") != segmentID ||
			stringAttr(record.attrs, "airgap.log.window.id") != windowID {
			return fmt.Errorf("sequence %d has inconsistent window identity", record.seq)
		}
		payloadHash := sha256.Sum256([]byte(record.body))
		if hex.EncodeToString(payloadHash[:]) != stringAttr(record.attrs, "airgap.log.hash") {
			return fmt.Errorf("payload hash mismatch at sequence %d", record.seq)
		}
		chainPrevious, err := hex.DecodeString(stringAttr(record.attrs, "airgap.log.chain.prev"))
		if err != nil {
			return fmt.Errorf("decode previous chain at sequence %d: %w", record.seq, err)
		}
		if index > 0 && !bytes.Equal(chainPrevious, previous) {
			return fmt.Errorf("chain predecessor mismatch at sequence %d", record.seq)
		}
		hash := sha256.New()
		hash.Write([]byte(chainDomain))
		hash.Write([]byte{0})
		hash.Write(chainPrevious)
		var sequence [8]byte
		binary.BigEndian.PutUint64(sequence[:], record.seq)
		hash.Write(sequence[:])
		hash.Write(payloadHash[:])
		previous = hash.Sum(nil)
		if hex.EncodeToString(previous) != stringAttr(record.attrs, "airgap.log.chain") {
			return fmt.Errorf("chain mismatch at sequence %d", record.seq)
		}
	}

	serviceName := stringAttr(anchor.resource, "service.name")
	if expectedService != "" && serviceName != expectedService {
		return fmt.Errorf("service.name is %q, want %q", serviceName, expectedService)
	}
	signerID := stringAttr(anchor.attrs, "airgap.log.signer.id")
	if expectedSigner != "" && signerID != expectedSigner {
		return fmt.Errorf("signer ID is %q, want %q", signerID, expectedSigner)
	}
	if stringAttr(anchor.attrs, "airgap.log.signature.alg") != "Ed25519" {
		return fmt.Errorf("signature algorithm is %q, want Ed25519",
			stringAttr(anchor.attrs, "airgap.log.signature.alg"))
	}
	certificate, publicKey, keyID, err := loadCertificate(certificatePath)
	if err != nil {
		return err
	}
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != signerID {
		return fmt.Errorf("certificate URI SAN does not identify signer %q", signerID)
	}
	if stringAttr(anchor.attrs, "airgap.log.key.id") != keyID {
		return fmt.Errorf("key fingerprint is %q, want %q", stringAttr(anchor.attrs, "airgap.log.key.id"), keyID)
	}
	if stringAttr(anchor.attrs, "airgap.log.certificate.serial") != certificate.SerialNumber.String() {
		return errors.New("certificate serial attribute does not match the trusted certificate")
	}

	anchorPrevious, err := hex.DecodeString(stringAttr(anchor.attrs, "airgap.log.chain.prev"))
	if err != nil {
		return fmt.Errorf("decode anchor previous chain: %w", err)
	}
	anchorFinal, err := hex.DecodeString(stringAttr(anchor.attrs, "airgap.log.chain"))
	if err != nil {
		return fmt.Errorf("decode anchor final chain: %w", err)
	}
	statement := []byte(checkpointDomain)
	statement = append(statement, 0)
	for _, value := range []string{
		"v1",
		"SHA256",
		"Ed25519",
		signerID,
		keyID,
		runID,
		segmentID,
		windowID,
		serviceName,
		stringAttr(anchor.resource, "service.instance.id"),
	} {
		statement = append(statement, frame([]byte(value))...)
	}
	for _, value := range []uint64{
		intAttr(anchor.attrs, "airgap.log.process.start_unix_nano"),
		from,
		to,
	} {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], value)
		statement = append(statement, encoded[:]...)
	}
	statement = append(statement, frame(anchorPrevious)...)
	statement = append(statement, frame(anchorFinal)...)
	checkpointHash := sha256.Sum256(statement)
	if hex.EncodeToString(checkpointHash[:]) != stringAttr(anchor.attrs, "airgap.log.checkpoint.hash") {
		return errors.New("checkpoint hash mismatch")
	}
	signature, err := base64.StdEncoding.DecodeString(stringAttr(anchor.attrs, "airgap.log.signature"))
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(publicKey, checkpointHash[:], signature) {
		return errors.New("Ed25519 signature verification failed")
	}
	return nil
}

func loadCertificate(path string) (*x509.Certificate, ed25519.PublicKey, string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read certificate: %w", err)
	}
	block, _ := pem.Decode(encoded)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, "", errors.New("trusted file does not contain a certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, "", fmt.Errorf("parse certificate: %w", err)
	}
	if certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return nil, nil, "", errors.New("certificate does not allow digitalSignature")
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return nil, nil, "", errors.New("certificate is outside its validity period")
	}
	foundEKU := false
	expectedEKU := mustOID(signingEKU)
	for _, eku := range certificate.UnknownExtKeyUsage {
		if equalOID(eku, expectedEKU) {
			foundEKU = true
		}
	}
	if !foundEKU {
		return nil, nil, "", fmt.Errorf("certificate does not contain signing EKU %s", signingEKU)
	}
	publicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, nil, "", errors.New("certificate public key is not Ed25519")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("marshal public key: %w", err)
	}
	fingerprint := sha256.Sum256(publicDER)
	return certificate, publicKey, "sha256:" + hex.EncodeToString(fingerprint[:]), nil
}

func frame(value []byte) []byte {
	result := make([]byte, 4+len(value))
	binary.BigEndian.PutUint32(result, uint32(len(value)))
	copy(result[4:], value)
	return result
}

func equalOID(left, right asn1.ObjectIdentifier) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func attributeMap(attributes []keyValue) (map[string]anyValue, error) {
	result := make(map[string]anyValue, len(attributes))
	for _, attribute := range attributes {
		if _, exists := result[attribute.Key]; exists {
			return nil, fmt.Errorf("duplicate attribute %q", attribute.Key)
		}
		result[attribute.Key] = attribute.Value
	}
	return result, nil
}

func stringAttr(attributes map[string]anyValue, key string) string {
	value, ok := attributes[key]
	if !ok || value.StringValue == nil {
		return ""
	}
	return *value.StringValue
}

func optionalIntAttr(attributes map[string]anyValue, key string) (uint64, bool) {
	value, ok := attributes[key]
	if !ok || value.IntValue == nil {
		return 0, false
	}
	parsed, err := strconv.ParseUint(*value.IntValue, 10, 64)
	return parsed, err == nil
}

func intAttr(attributes map[string]anyValue, key string) uint64 {
	value, ok := optionalIntAttr(attributes, key)
	if !ok {
		fatalf("missing or invalid integer attribute %s", key)
	}
	return value
}

func boolAttr(attributes map[string]anyValue, key string) bool {
	value, ok := attributes[key]
	return ok && value.BoolValue != nil && *value.BoolValue
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "tc29-integrity: "+format+"\n", arguments...)
	os.Exit(1)
}
