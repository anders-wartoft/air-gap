// Package pinnedtest holds the first test-first acceptance batch for TC-27.
package pinnedtest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type ConfigBuilder interface {
	BuildTLSConfig() (*tls.Config, error)
}

type Factory func(t *testing.T, properties string) any

type identity struct {
	key         *ecdsa.PrivateKey
	certificate tls.Certificate
	publicPEM   string
	fingerprint string
	certPath    string
	keyPath     string
}

type trustEntry struct {
	Version     int    `json:"version"`
	Peer        string `json:"peer"`
	Role        string `json:"role"`
	PublicKey   string `json:"publicKey"`
	Fingerprint string `json:"fingerprint"`
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func newIdentity(t *testing.T, key *ecdsa.PrivateKey, expired bool) identity {
	t.Helper()
	var err error
	if key == nil {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "same-name"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	if expired {
		template.NotBefore, template.NotAfter = now.Add(-48*time.Hour), now.Add(-24*time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(spki)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	result := identity{
		key: key, certificate: cert,
		publicPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})),
		fingerprint: "SHA256:" + base64.StdEncoding.EncodeToString(hash[:]),
		certPath:    filepath.Join(dir, "identity.crt"), keyPath: filepath.Join(dir, "identity.key"),
	}
	write(t, result.certPath, certPEM)
	write(t, result.keyPath, keyPEM)
	return result
}

func entry(t *testing.T, dir, name, role string, id identity) {
	t.Helper()
	data, err := json.Marshal(trustEntry{1, "peer", role, id.publicPEM, id.fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, name+".json"), data)
}

func properties(local identity, dir string, server bool) string {
	result := fmt.Sprintf("transport=tcp\ntcpTLSAuthMode=pinned\ntcpTLSCertFile=%s\ntcpTLSKeyFile=%s\ntcpTLSTrustedKeysDir=%s\n",
		local.certPath, local.keyPath, dir)
	if server {
		return result + "tcpTLSClientAuth=require\n"
	}
	return result + "tcpTLSEnabled=true\n"
}

func build(t *testing.T, factory Factory, properties string) (*tls.Config, error) {
	t.Helper()
	value := factory(t, properties)
	builder, ok := value.(ConfigBuilder)
	if !ok {
		t.Fatal("REQ-51 pending: configuration must implement BuildTLSConfig() (*tls.Config, error)")
	}
	return builder.BuildTLSConfig()
}

// exchange uses real TLS and an application round trip in both directions.
func exchange(config *tls.Config, peer identity, server bool) error {
	listenerConfig, dialConfig := config, &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates:       []tls.Certificate{peer.certificate},
		InsecureSkipVerify: true, // The fixture isolates verification by the endpoint under test.
	}
	if !server {
		listenerConfig, dialConfig = dialConfig, config
		listenerConfig.ClientAuth = tls.RequireAnyClientCert
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", listenerConfig)
	if err != nil {
		return err
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		buffer := make([]byte, 1)
		if _, err = conn.Read(buffer); err == nil {
			if buffer[0] != 42 {
				err = fmt.Errorf("wrong application request: %d", buffer[0])
			} else {
				_, err = conn.Write(buffer)
			}
		}
		done <- err
	}()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", listener.Addr().String(), dialConfig)
	if err == nil {
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err = conn.Write([]byte{42}); err == nil {
			buffer := make([]byte, 1)
			_, err = conn.Read(buffer)
			if err == nil && buffer[0] != 42 {
				err = fmt.Errorf("wrong application response: %d", buffer[0])
			}
		}
		conn.Close()
	}
	listener.Close()
	serverErr := <-done
	if err != nil {
		return err
	}
	return serverErr
}

func Authentication(t *testing.T, factory Factory, server bool) {
	t.Helper()
	local, peer, replacement, unknown := newIdentity(t, nil, false), newIdentity(t, nil, false),
		newIdentity(t, nil, false), newIdentity(t, nil, false)
	role := "server"
	if server {
		role = "client"
	}

	t.Run("TC27_01_invalid_configuration", func(t *testing.T) {
		for _, override := range []string{
			"tcpTLSAuthMode=unknown", "tcpTLSAuthMode=ca",
			"tcpTLSCAFile=unrelated-ca.pem", "tcpTLSCipherSuites=TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			"tcpTLSTrustedKeysDir=", "transport=udp",
			"tcpTLSCertFile=", "tcpTLSKeyFile=",
		} {
			t.Run(override, func(t *testing.T) {
				if _, err := build(t, factory, properties(local, t.TempDir(), server)+override+"\n"); err == nil {
					t.Fatal("invalid/conflicting TLS configuration accepted")
				}
			})
		}
		override := "tcpTLSEnabled=false\n"
		if server {
			override = "tcpTLSClientAuth=allow\n"
		}
		if _, err := build(t, factory, properties(local, t.TempDir(), server)+override); err == nil {
			t.Fatal("pinned mutual authentication was optional")
		}
	})
	t.Run("TC27_01_04_07_exact_pins_and_overlap", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "old", role, peer)
		entry(t, dir, "new", role, replacement)
		config, err := build(t, factory, properties(local, dir, server))
		if err != nil {
			t.Fatal(err)
		}
		if config == nil || config.MinVersion != tls.VersionTLS13 || config.MaxVersion != tls.VersionTLS13 {
			t.Fatal("pinned mode must explicitly enforce TLS 1.3")
		}
		for name, id := range map[string]identity{
			"old": peer, "replacement": replacement,
			"expired_wrapper_same_key": newIdentity(t, peer.key, true),
		} {
			t.Run(name, func(t *testing.T) {
				if err := exchange(config.Clone(), id, server); err != nil {
					t.Fatalf("authorized public key rejected: %v", err)
				}
			})
		}
		if err := exchange(config.Clone(), unknown, server); err == nil {
			t.Fatal("same-CN unpinned peer authenticated")
		}
		copiedCertificate := peer
		copiedCertificate.certificate.PrivateKey = unknown.key
		if err := exchange(config.Clone(), copiedCertificate, server); err == nil {
			t.Fatal("copied certificate authenticated without its private key")
		}
	})
	t.Run("TC27_06_empty_store_denies_all", func(t *testing.T) {
		config, err := build(t, factory, properties(local, t.TempDir(), server))
		if err != nil {
			t.Fatalf("valid empty directory must load: %v", err)
		}
		if config == nil {
			t.Fatal("nil TLS configuration")
		}
		if err := exchange(config, peer, server); err == nil {
			t.Fatal("empty trust store accepted a peer")
		}
	})
	t.Run("TC27_03_04_05_13_trust_validation", func(t *testing.T) {
		for _, scenario := range []string{"mismatched_fingerprint", "wrong_role", "malformed", "private_key", "symlink", "missing_directory"} {
			t.Run(scenario, func(t *testing.T) {
				dir := t.TempDir()
				entry(t, dir, "peer", role, peer)
				path := filepath.Join(dir, "peer.json")
				switch scenario {
				case "mismatched_fingerprint", "wrong_role", "private_key":
					e := trustEntry{1, "peer", role, peer.publicPEM, peer.fingerprint}
					if scenario == "mismatched_fingerprint" {
						e.Fingerprint = unknown.fingerprint
					} else if scenario == "wrong_role" {
						e.Role = "server"
						if !server {
							e.Role = "client"
						}
					} else {
						data, err := os.ReadFile(peer.keyPath)
						if err != nil {
							t.Fatal(err)
						}
						e.PublicKey = string(data)
					}
					data, err := json.Marshal(e)
					if err != nil {
						t.Fatal(err)
					}
					write(t, path, data)
				case "malformed":
					write(t, path, []byte("{"))
				case "symlink":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(peer.certPath, path); err != nil {
						t.Fatal(err)
					}
				case "missing_directory":
					dir = filepath.Join(dir, "absent")
				}
				if _, err := build(t, factory, properties(local, dir, server)); err == nil {
					t.Fatal("invalid trust input accepted")
				}
			})
		}
	})
	t.Run("TC27_05_staging_file_ignored", func(t *testing.T) {
		dir := t.TempDir()
		entry(t, dir, "peer", role, peer)
		write(t, filepath.Join(dir, ".incomplete.tmp"), []byte("{"))
		config, err := build(t, factory, properties(local, dir, server))
		if err != nil {
			t.Fatal(err)
		}
		if config == nil {
			t.Fatal("nil TLS configuration")
		}
		if err := exchange(config, peer, server); err != nil {
			t.Fatal(err)
		}
	})
}

func Generation(t *testing.T, helper, role string) {
	t.Helper()
	dir := t.TempDir()
	run := func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+helper+"$")
		cmd.Env = append(os.Environ(), "AIRGAP_PINNED_GENERATE_DIR="+dir, "AIRGAP_PINNED_GENERATE_ROLE="+role)
		output, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("key generation did not finish: %v\n%s", ctx.Err(), output)
		}
		return output, err
	}
	output, err := run()
	if err != nil {
		t.Fatalf("REQ-51.02 pending: key generation command failed: %v\n%s", err, output)
	}
	var fingerprints []string
	var files []string
	var contents [][]byte
	for i := 1; i <= 2; i++ {
		base := filepath.Join(dir, fmt.Sprintf("identity-%d", i))
		read := func(suffix string) []byte {
			path := base + suffix
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			files, contents = append(files, path), append(contents, data)
			return data
		}
		keyPEM, certPEM, publicPEM, fingerprint := read(".key"), read(".crt"), read(".pub"), read(".fingerprint")
		info, err := os.Stat(base + ".key")
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("private key permissions: %v", info.Mode().Perm())
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		publicBlock, rest := pem.Decode(publicPEM)
		if publicBlock == nil || publicBlock.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 ||
			!bytes.Equal(publicBlock.Bytes, leaf.RawSubjectPublicKeyInfo) {
			t.Fatal("exported public key does not match TLS identity")
		}
		key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
		if !ok || key.Curve != elliptic.P256() {
			t.Fatal("generation must produce P-256 identities")
		}
		hash := sha256.Sum256(publicBlock.Bytes)
		want := "SHA256:" + base64.StdEncoding.EncodeToString(hash[:])
		if strings.TrimSpace(string(fingerprint)) != want || !bytes.Contains(output, []byte(want)) {
			t.Fatal("fingerprint file/command output does not match SPKI")
		}
		if bytes.Contains(output, []byte("PRIVATE KEY")) {
			t.Fatal("generation output exposes private key material")
		}
		fingerprints = append(fingerprints, want)
	}
	if fingerprints[0] == fingerprints[1] {
		t.Fatal("generated key-sets are not independent")
	}
	if output, err := run(); err == nil {
		t.Fatalf("generation overwrote occupied paths: %s", output)
	}
	for i, path := range files {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, contents[i]) {
			t.Fatalf("failed generation changed existing file %s: %v", path, err)
		}
	}
}
