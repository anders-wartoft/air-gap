package pinnedtest

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"testing"
)

func TestTLSFixtureRoundTripAndRejection(t *testing.T) {
	local, peer, unknown := newIdentity(t, nil, false), newIdentity(t, nil, false), newIdentity(t, nil, false)
	for _, server := range []bool{false, true} {
		t.Run(fmt.Sprintf("server=%t", server), func(t *testing.T) {
			config := &tls.Config{
				MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
				Certificates: []tls.Certificate{local.certificate},
			}
			if server {
				config.ClientAuth = tls.RequireAnyClientCert
			} else {
				config.InsecureSkipVerify = true
			}
			// This fixture verifier tests the harness, not application pinning.
			config.VerifyConnection = func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return fmt.Errorf("missing certificate")
				}
				key, err := x509.ParsePKIXPublicKey(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
				if err != nil {
					return err
				}
				if !peer.key.PublicKey.Equal(key) {
					return fmt.Errorf("unknown fixture key")
				}
				return nil
			}
			if err := exchange(config.Clone(), peer, server); err != nil {
				t.Fatal(err)
			}
			if err := exchange(config.Clone(), unknown, server); err == nil {
				t.Fatal("harness accepted rejected fixture")
			}
		})
	}
}
