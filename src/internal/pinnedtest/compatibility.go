package pinnedtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"testing"
	"time"
)

func CACompatibility(t *testing.T, factory Factory, server bool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "TC27 CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	write(t, caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	sign := func() identity {
		id := newIdentity(t, nil, false)
		leaf, err := x509.ParseCertificate(id.certificate.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &id.key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		id.certificate.Certificate = [][]byte{der}
		id.certificate.Leaf, err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		write(t, id.certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		return id
	}
	local, peer := sign(), sign()
	props := properties(local, "", server) + "tcpTLSAuthMode=ca\ntcpTLSCAFile=" + caPath + "\n"
	config, err := build(t, factory, props)
	if err != nil {
		t.Fatal(err)
	}
	if config == nil {
		t.Fatal("nil CA TLS configuration")
	}
	if err := exchange(config.Clone(), peer, server); err != nil {
		t.Fatalf("existing CA mutual TLS rejected: %v", err)
	}
	if err := exchange(config.Clone(), newIdentity(t, nil, false), server); err == nil {
		t.Fatal("CA mode accepted an untrusted self-signed peer")
	}
}
