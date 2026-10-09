package pinnedtls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

type pendingClient struct {
	Version     int
	New         string
	Certificate []byte
	PrivateKey  []byte
}

func SyncIdentity(certPath, keyPath string) error {
	for _, path := range []string{certPath, keyPath} {
		if _, err := readFile(path, path == keyPath); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return nil
}

func SavePendingClient(dir string, oldConfig *tls.Config, candidate *Snapshot) error {
	if _, err := loadTrust(dir, "server"); err != nil {
		return err
	}
	key, err := x509.MarshalPKCS8PrivateKey(oldConfig.Certificates[0].PrivateKey)
	if err != nil {
		return err
	}
	if restored, pending, err := RestorePendingClient(dir, candidate); err != nil {
		return err
	} else if pending && restored.Identity() != Fingerprint(oldConfig.Certificates[0].Leaf.RawSubjectPublicKeyInfo) {
		return fmt.Errorf("pending recovery identity differs from active client")
	}
	return atomicJSON(filepath.Join(dir, ".pinned-client-rotation.json"), pendingClient{
		Version: 1, New: candidate.Identity(), Certificate: oldConfig.Certificates[0].Certificate[0], PrivateKey: key,
	})
}

func RestorePendingClient(dir string, candidate *Snapshot) (*Snapshot, bool, error) {
	path := filepath.Join(dir, ".pinned-client-rotation.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return candidate, false, nil
	} else if err != nil {
		return nil, false, err
	}
	data, err := readFile(path, true)
	if err != nil {
		return nil, false, err
	}
	var pending pendingClient
	if err := strictJSON(data, &pending); err != nil {
		return nil, false, err
	}
	if pending.Version != 1 || pending.New != candidate.Identity() {
		return nil, false, fmt.Errorf("pending client replacement differs from configuration; reconcile before startup")
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pending.Certificate}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pending.PrivateKey}),
	)
	if err != nil {
		return nil, false, fmt.Errorf("invalid pending client identity: %w", err)
	}
	cert.Leaf, err = x509.ParseCertificate(pending.Certificate)
	if err != nil || ValidatePublicKey(cert.Leaf.PublicKey) != nil {
		return nil, false, fmt.Errorf("invalid pending client key policy")
	}
	copy := *candidate
	copy.config = candidate.config.Clone()
	copy.config.Certificates = []tls.Certificate{cert}
	copy.config.VerifyConnection = copy.verify
	return &copy, true, nil
}

func ClearPendingClient(dir string) error {
	if err := os.Remove(filepath.Join(dir, ".pinned-client-rotation.json")); err != nil {
		return err
	}
	return syncDirectory(dir)
}
