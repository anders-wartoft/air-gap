package logging

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
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

func TestRotateSigningCertificateAuthenticatesNewCertificateWithOldKey(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(10); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	oldCert, oldKey, _ := writeTestSigningCertificate(
		t, t.TempDir(), x509.KeyUsageDigitalSignature,
	)
	newCert, newKey, _ := writeTestSigningCertificate(
		t, t.TempDir(), x509.KeyUsageDigitalSignature,
	)
	if err := Logger.SetSigningCertificate(
		oldCert, oldKey, "urn:airgap:test:signer", "",
	); err != nil {
		t.Fatalf("SetSigningCertificate(old): %v", err)
	}
	oldKeyID := integrity.signer.keyID

	rotation, err := Logger.RotateSigningCertificate(
		newCert, newKey, "urn:airgap:test:signer", "",
	)
	if err != nil {
		t.Fatalf("RotateSigningCertificate: %v", err)
	}
	if !rotation.Changed || !rotation.Authenticated {
		t.Fatalf("rotation = %#v, want changed authenticated rotation", rotation)
	}
	if rotation.OldKeyID != oldKeyID || rotation.NewKeyID == oldKeyID {
		t.Fatalf("rotation key IDs = old %q new %q", rotation.OldKeyID, rotation.NewKeyID)
	}

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("got %d records, want rotation STATUS plus forced anchor", len(records))
	}
	status := records[0]
	if status.eventName != EventLogSigningCertificateRotate {
		t.Fatalf("rotation eventName = %q", status.eventName)
	}
	if status.body != string(rotation.Status) {
		t.Fatalf("local body and downstream STATUS differ:\nlocal: %s\nstatus: %s",
			status.body, rotation.Status)
	}
	var announcement SigningCertificateRotationAnnouncement
	if err := json.Unmarshal([]byte(status.body), &announcement); err != nil {
		t.Fatalf("rotation body is not JSON: %v", err)
	}
	if announcement.OldKeyID != rotation.OldKeyID ||
		announcement.NewKeyID != rotation.NewKeyID ||
		!strings.Contains(announcement.NewCertificatePEM, "BEGIN CERTIFICATE") {
		t.Fatalf("rotation announcement = %#v", announcement)
	}

	anchor := records[1]
	if anchor.eventName != EventLogAnchor {
		t.Fatalf("second eventName = %q, want %q", anchor.eventName, EventLogAnchor)
	}
	if !recordSignatureVerifies(t, anchor, oldCert) {
		t.Fatal("rotation anchor was not signed by the old certificate")
	}
	if recordSignatureVerifies(t, anchor, newCert) {
		t.Fatal("rotation anchor unexpectedly verifies with the new certificate")
	}
	if integrity.signer.keyID != rotation.NewKeyID {
		t.Fatalf("active signer = %q, want %q", integrity.signer.keyID, rotation.NewKeyID)
	}
}

func TestRotateSigningCertificateRejectsCandidateAndKeepsOldSigner(t *testing.T) {
	resetLogging(t)
	oldCert, oldKey, _ := writeTestSigningCertificate(
		t, t.TempDir(), x509.KeyUsageDigitalSignature,
	)
	newCert, _, _ := writeTestSigningCertificate(
		t, t.TempDir(), x509.KeyUsageDigitalSignature,
	)
	_, unrelatedKey, _ := writeTestSigningCertificate(
		t, t.TempDir(), x509.KeyUsageDigitalSignature,
	)
	if err := Logger.SetSigningCertificate(
		oldCert, oldKey, "urn:airgap:test:signer", "",
	); err != nil {
		t.Fatalf("SetSigningCertificate(old): %v", err)
	}
	oldKeyID := integrity.signer.keyID

	if _, err := Logger.RotateSigningCertificate(
		newCert, unrelatedKey, "urn:airgap:test:signer", "",
	); err == nil {
		t.Fatal("mismatched certificate and key were accepted")
	}
	if integrity.signer == nil || integrity.signer.keyID != oldKeyID {
		t.Fatalf("active signer changed after rejected rotation: %#v", integrity.signer)
	}
}

func TestRotateSigningCertificateWithUnchangedKeyIsNoOp(t *testing.T) {
	buf := resetLogging(t)
	certPath, keyPath, _ := writeTestSigningCertificate(
		t, t.TempDir(), x509.KeyUsageDigitalSignature,
	)
	if err := Logger.SetSigningCertificate(
		certPath, keyPath, "urn:airgap:test:signer", "",
	); err != nil {
		t.Fatalf("SetSigningCertificate: %v", err)
	}

	rotation, err := Logger.RotateSigningCertificate(
		certPath, keyPath, "urn:airgap:test:signer", "",
	)
	if err != nil {
		t.Fatalf("RotateSigningCertificate: %v", err)
	}
	if rotation.Changed {
		t.Fatalf("unchanged key reported a rotation: %#v", rotation)
	}
	if buf.Len() != 0 {
		t.Fatalf("unchanged key emitted log records:\n%s", buf)
	}
}

func TestReloadSigningCertificateReadsChangedConfiguration(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(10); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}
	oldCert, oldKey, _ := writeTestSigningCertificate(
		t, t.TempDir(), x509.KeyUsageDigitalSignature,
	)
	newCert, newKey, _ := writeTestSigningCertificate(
		t, t.TempDir(), x509.KeyUsageDigitalSignature,
	)
	if err := Logger.SetSigningCertificate(
		oldCert, oldKey, "urn:airgap:test:signer", "",
	); err != nil {
		t.Fatalf("SetSigningCertificate(old): %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "upstream.conf")
	config := strings.Join([]string{
		"log-signing-cert=" + newCert,
		"log-signing-key=" + newKey,
		"log-signer-id=urn:airgap:test:signer",
		"log-signing-eku=",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rotation, err := Logger.ReloadSigningCertificate(configPath)
	if err != nil {
		t.Fatalf("ReloadSigningCertificate: %v", err)
	}
	if !rotation.Changed || !rotation.Authenticated {
		t.Fatalf("rotation = %#v", rotation)
	}
	if len(decodeOTLPLines(t, buf.String())) != 2 {
		t.Fatalf("reload output:\n%s", buf)
	}
}

func TestReloadSigningCertificateWithoutSigningConfigurationIsNoOp(t *testing.T) {
	resetLogging(t)
	configPath := filepath.Join(t.TempDir(), "upstream.conf")
	if err := os.WriteFile(configPath, []byte("id=test\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rotation, err := Logger.ReloadSigningCertificate(configPath)
	if err != nil {
		t.Fatalf("ReloadSigningCertificate: %v", err)
	}
	if rotation.Changed || rotation.Authenticated || len(rotation.Status) != 0 {
		t.Fatalf("unsigned reload = %#v, want no-op", rotation)
	}
}

func recordSignatureVerifies(t *testing.T, record decodedRecord, certificatePath string) bool {
	t.Helper()
	certificate, err := x509.ParseCertificate(loadPEMCertificate(t, certificatePath))
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	publicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatal("certificate key is not Ed25519")
	}
	checkpointHash, err := hex.DecodeString(record.attributes[AttrCheckpointHash])
	if err != nil {
		t.Fatalf("DecodeString(checkpoint): %v", err)
	}
	signature, err := base64.StdEncoding.DecodeString(record.attributes[AttrSignature])
	if err != nil {
		t.Fatalf("DecodeString(signature): %v", err)
	}
	return ed25519.Verify(publicKey, checkpointHash, signature)
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
