package logging

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func writeTestSigningCertificate(t *testing.T, directory string, usage x509.KeyUsage) (string, string, ed25519.PrivateKey) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signerURI, err := url.Parse("urn:airgap:test:signer")
	if err != nil {
		t.Fatalf("Parse signer URI: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "Air-gap test log signer"},
		URIs:                  []*url.URL{signerURI},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              usage,
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certificatePath := directory + "/signer.crt"
	keyPath := directory + "/signer.key"
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificateDER,
	}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: keyDER,
	}), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	return certificatePath, keyPath, privateKey
}

func TestSignedAnchorIncludesVerifiableCheckpoint(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(2); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}
	certificatePath, keyPath, privateKey := writeTestSigningCertificate(t, t.TempDir(), x509.KeyUsageDigitalSignature)
	if err := Logger.SetSigningCertificate(certificatePath, keyPath, "urn:airgap:test:signer", ""); err != nil {
		t.Fatalf("SetSigningCertificate: %v", err)
	}

	Logger.Info(Event(EventDiagnostic), "first")
	Logger.Info(Event(EventDiagnostic), "second")

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 3 {
		t.Fatalf("got %d records, want two records plus one anchor", len(records))
	}
	anchor := records[2]
	signature := anchor.attributes[AttrSignature]
	if signature == "" {
		t.Fatalf("anchor has no signature: %#v", anchor.attributes)
	}
	if anchor.attributes[AttrSignatureAlg] != signatureAlg {
		t.Fatalf("signature algorithm = %q, want %q", anchor.attributes[AttrSignatureAlg], signatureAlg)
	}
	if anchor.attributes[AttrSignerID] != "urn:airgap:test:signer" {
		t.Fatalf("signer ID = %q", anchor.attributes[AttrSignerID])
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(anchor.attributes[AttrKeyID], "sha256:")); err != nil {
		t.Fatalf("key ID is not a SHA-256 fingerprint: %v", err)
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	parsedCertificate, err := x509.ParseCertificate(loadPEMCertificate(t, certificatePath))
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	checkpointHash, err := hex.DecodeString(anchor.attributes[AttrCheckpointHash])
	if err != nil {
		t.Fatalf("decode checkpoint hash: %v", err)
	}
	publicKey := parsedCertificate.PublicKey.(ed25519.PublicKey)
	if !ed25519.Verify(publicKey, checkpointHash, signatureBytes) {
		t.Fatal("checkpoint signature did not verify")
	}
	if !bytes.Equal(publicKey, privateKey.Public().(ed25519.PublicKey)) {
		t.Fatal("test certificate key mismatch")
	}

	t.Log("OTLP/JSON example:")
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		t.Log(line)
	}
}

func TestSigningRequiresDigitalSignatureCertificateUsage(t *testing.T) {
	resetLogging(t)
	certificatePath, keyPath, _ := writeTestSigningCertificate(t, t.TempDir(), 0)
	if err := Logger.SetSigningCertificate(certificatePath, keyPath, "urn:airgap:test:signer", ""); err == nil {
		t.Fatal("certificate without digitalSignature usage was accepted")
	}
}

func TestSigningRequiresSignerIDToMatchCertificateURISAN(t *testing.T) {
	resetLogging(t)
	certificatePath, keyPath, _ := writeTestSigningCertificate(t, t.TempDir(), x509.KeyUsageDigitalSignature)
	if err := Logger.SetSigningCertificate(certificatePath, keyPath, "urn:airgap:test:different", ""); err == nil {
		t.Fatal("signer ID that does not match the certificate URI SAN was accepted")
	}
}

func TestSigningConfigurationCanBeAppliedInAnyFileOrder(t *testing.T) {
	resetLogging(t)
	certificatePath, keyPath, _ := writeTestSigningCertificate(t, t.TempDir(), x509.KeyUsageDigitalSignature)

	if handled, err := ApplyOption(OptionSigningKey, keyPath); !handled || err != nil {
		t.Fatalf("ApplyOption(key): handled=%v err=%v", handled, err)
	}
	if handled, err := ApplyOption(OptionSigningCertificate, certificatePath); !handled || err != nil {
		t.Fatalf("ApplyOption(cert): handled=%v err=%v", handled, err)
	}
	if handled, err := ApplyOption(OptionSignerID, "urn:airgap:test:signer"); !handled || err != nil {
		t.Fatalf("ApplyOption(signer): handled=%v err=%v", handled, err)
	}
	if integrity.signer == nil {
		t.Fatal("signer was not enabled after all signing options were applied")
	}
}

func loadPEMCertificate(t *testing.T, path string) []byte {
	t.Helper()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read certificate: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("certificate PEM has no block")
	}
	return block.Bytes
}
