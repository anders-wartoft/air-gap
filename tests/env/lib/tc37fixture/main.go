package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type trustRegistry struct {
	Schema string     `json:"schema"`
	Keys   []trustKey `json:"keys"`
}

type trustKey struct {
	KeyID        string `json:"key_id"`
	SignerID     string `json:"signer_id"`
	Algorithm    string `json:"algorithm"`
	PublicKeyPEM string `json:"public_key_pem"`
	Status       string `json:"status"`
}

type metadata struct {
	SignerID string `json:"signer_id"`
	OldKeyID string `json:"old_key_id"`
	NewKeyID string `json:"new_key_id"`
}

func main() {
	output := flag.String("output", "", "fixture output directory")
	flag.Parse()
	if *output == "" {
		fatalf("--output is required")
	}
	if err := os.MkdirAll(*output, 0o700); err != nil {
		fatalf("create output: %v", err)
	}

	const signerID = "urn:airgap:test:tc37"
	caCertificate, caKey := generateCA(*output)
	_, _, oldPublic := generateCertificate(
		*output, "old", signerID, caCertificate, caKey,
	)
	_, _, newPublic := generateCertificate(
		*output, "new", signerID, caCertificate, caKey,
	)

	oldKeyID, publicPEM := keyIdentity(oldPublic)
	registry := trustRegistry{
		Schema: "airgap-log-trust-v1",
		Keys: []trustKey{{
			KeyID:        oldKeyID,
			SignerID:     signerID,
			Algorithm:    "Ed25519",
			PublicKeyPEM: string(publicPEM),
			Status:       "active",
		}},
	}
	encoded, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		fatalf("marshal registry: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(*output, "trust.json"), append(encoded, '\n'), 0o600,
	); err != nil {
		fatalf("write registry: %v", err)
	}
	newKeyID, _ := keyIdentity(newPublic)
	encoded, err = json.MarshalIndent(metadata{
		SignerID: signerID,
		OldKeyID: oldKeyID,
		NewKeyID: newKeyID,
	}, "", "  ")
	if err != nil {
		fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(*output, "metadata.json"), append(encoded, '\n'), 0o600,
	); err != nil {
		fatalf("write metadata: %v", err)
	}
}

func keyIdentity(publicKey ed25519.PublicKey) (string, []byte) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		fatalf("marshal public key: %v", err)
	}
	fingerprint := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(fingerprint[:]), pem.EncodeToMemory(&pem.Block{
		Type: "PUBLIC KEY", Bytes: der,
	})
}

func generateCA(directory string) (*x509.Certificate, ed25519.PrivateKey) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatalf("generate CA key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "TC-37 log-signing CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(
		rand.Reader, template, template, publicKey, privateKey,
	)
	if err != nil {
		fatalf("create CA certificate: %v", err)
	}
	path := filepath.Join(directory, "ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: der,
	}), 0o600); err != nil {
		fatalf("write CA certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		fatalf("parse CA certificate: %v", err)
	}
	return certificate, privateKey
}

func generateCertificate(
	directory, name, signerID string,
	issuer *x509.Certificate,
	issuerKey ed25519.PrivateKey,
) (string, string, ed25519.PublicKey) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatalf("generate %s key: %v", name, err)
	}
	uri, err := url.Parse(signerID)
	if err != nil {
		fatalf("parse signer ID: %v", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		fatalf("generate serial: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "TC-37 " + name + " log signer"},
		URIs:                  []*url.URL{uri},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(
		rand.Reader, template, issuer, publicKey, issuerKey,
	)
	if err != nil {
		fatalf("create %s certificate: %v", name, err)
	}
	certPath := filepath.Join(directory, name+".crt")
	keyPath := filepath.Join(directory, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: der,
	}), 0o600); err != nil {
		fatalf("write %s certificate: %v", name, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		fatalf("marshal %s key: %v", name, err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "PRIVATE KEY", Bytes: keyDER,
	}), 0o600); err != nil {
		fatalf("write %s key: %v", name, err)
	}
	return certPath, keyPath, publicKey
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "tc37-fixture: "+format+"\n", args...)
	os.Exit(1)
}
