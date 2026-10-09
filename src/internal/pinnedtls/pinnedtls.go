// Package pinnedtls implements explicit public-key trust for TCP TLS.
package pinnedtls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxPins      = 2000
	MaxEntrySize = 16384
	MaxFiles     = 4096
)

type Entry struct {
	Version     int    `json:"version"`
	Peer        string `json:"peer"`
	Role        string `json:"role"`
	PublicKey   string `json:"publicKey"`
	Fingerprint string `json:"fingerprint"`
}

func Fingerprint(spki []byte) string {
	hash := sha256.Sum256(spki)
	return "SHA256:" + base64.StdEncoding.EncodeToString(hash[:])
}

func ValidatePublicKey(key any) error {
	public, ok := key.(*ecdsa.PublicKey)
	if !ok || public.Curve != elliptic.P256() || !public.Curve.IsOnCurve(public.X, public.Y) {
		return fmt.Errorf("pinned TLS requires an ECDSA P-256 public key")
	}
	return nil
}

func readFile(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 ||
		(private && info.Mode().Perm()&0077 != 0) {
		return nil, fmt.Errorf("unsafe file type or permissions: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, actual) {
		return nil, fmt.Errorf("file changed while loading: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxEntrySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxEntrySize {
		return nil, fmt.Errorf("file exceeds %d bytes: %s", MaxEntrySize, path)
	}
	return data, nil
}

func loadTrust(dir, role string) (map[string]Entry, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("trusted directory must be a real directory without group/other write permission: %s", dir)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	files, err := directory.ReadDir(MaxFiles + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(files) > MaxFiles {
		return nil, fmt.Errorf("trusted directory exceeds %d files including staged files", MaxFiles)
	}
	pins := make(map[string]Entry)
	count := 0
	for _, file := range files {
		name := file.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			return nil, fmt.Errorf("unrecognized trusted directory entry: %s", name)
		}
		count++
		if count > MaxPins {
			return nil, fmt.Errorf("trusted directory exceeds %d entries", MaxPins)
		}
		data, err := readFile(filepath.Join(dir, name), false)
		if err != nil {
			return nil, err
		}
		var entry Entry
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("invalid trust entry %s: %w", name, err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("trailing content in trust entry %s", name)
		}
		if entry.Version != 1 || entry.Peer == "" || strings.ContainsAny(entry.Peer, "\r\n\t") ||
			entry.Role != role {
			return nil, fmt.Errorf("invalid version, peer, or role in trust entry %s", name)
		}
		block, rest := pem.Decode([]byte(entry.PublicKey))
		if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
			return nil, fmt.Errorf("trust entry %s must contain exactly one SPKI public key", name)
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid public key in %s: %w", name, err)
		}
		if err := ValidatePublicKey(key); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		canonical, err := x509.MarshalPKIXPublicKey(key)
		if err != nil {
			return nil, err
		}
		fingerprint := Fingerprint(canonical)
		if fingerprint != entry.Fingerprint {
			return nil, fmt.Errorf("public-key fingerprint mismatch in %s", name)
		}
		if previous, exists := pins[fingerprint]; exists && previous.Peer != entry.Peer {
			return nil, fmt.Errorf("conflicting peer identities for %s", fingerprint)
		}
		pins[fingerprint] = entry
	}
	return pins, nil
}

func Build(certFile, keyFile, trustedDir string, server bool) (*tls.Config, error) {
	snapshot, err := Prepare(certFile, keyFile, trustedDir, server)
	if err != nil {
		return nil, err
	}
	return snapshot.config, nil
}

type Snapshot struct {
	config *tls.Config
	pins   map[string]Entry
	role   string
}

func (s *Snapshot) Identity() string {
	return Fingerprint(s.config.Certificates[0].Leaf.RawSubjectPublicKeyInfo)
}

func Prepare(certFile, keyFile, trustedDir string, server bool) (*Snapshot, error) {
	certPEM, err := readFile(certFile, false)
	if err != nil {
		return nil, fmt.Errorf("pinned TLS certificate: %w", err)
	}
	keyPEM, err := readFile(keyFile, true)
	if err != nil {
		return nil, fmt.Errorf("pinned TLS private key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("pinned TLS identity: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	if err := ValidatePublicKey(leaf.PublicKey); err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	role := "server"
	if server {
		role = "client"
	}
	pins, err := loadTrust(trustedDir, role)
	if err != nil {
		return nil, fmt.Errorf("pinned TLS trust: %w", err)
	}
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}
	if server {
		config.ClientAuth = tls.RequireAnyClientCert
	} else {
		// Authorization is exact SPKI matching below, not CA/name/date validation.
		config.InsecureSkipVerify = true
	}
	snapshot := &Snapshot{config: config, pins: pins, role: role}
	config.VerifyConnection = snapshot.verify
	return snapshot, nil
}

func (s *Snapshot) verify(state tls.ConnectionState) error {
	_, _, err := s.authorize(state)
	return err
}

func (s *Snapshot) authorize(state tls.ConnectionState) (string, Entry, error) {
	if len(state.PeerCertificates) == 0 {
		return "", Entry{}, fmt.Errorf("pinned TLS: peer certificate missing")
	}
	peer := state.PeerCertificates[0]
	if err := ValidatePublicKey(peer.PublicKey); err != nil {
		return "", Entry{}, err
	}
	fingerprint := Fingerprint(peer.RawSubjectPublicKeyInfo)
	entry, ok := s.pins[fingerprint]
	if !ok {
		return "", Entry{}, fmt.Errorf("pinned TLS: untrusted %s key %s", s.role, fingerprint)
	}
	return fingerprint, entry, nil
}

func ValidateMode(mode, dir, caFile, cnRegex, passwordFile, cipherSuites string) error {
	switch mode {
	case "", "ca":
		if dir != "" {
			return fmt.Errorf("tcpTLSTrustedKeysDir requires tcpTLSAuthMode=pinned")
		}
	case "pinned":
		if dir == "" {
			return fmt.Errorf("pinned TLS requires tcpTLSTrustedKeysDir")
		}
		if caFile != "" || cnRegex != "" || passwordFile != "" {
			return fmt.Errorf("pinned TLS conflicts with CA, CN regex, or encrypted-key password settings")
		}
		if cipherSuites != "" && strings.ToUpper(strings.TrimSpace(cipherSuites)) != "TLS1.3" {
			return fmt.Errorf("pinned TLS requires TLS 1.3")
		}
	default:
		return fmt.Errorf("unknown tcpTLSAuthMode %q; expected ca or pinned", mode)
	}
	return nil
}
