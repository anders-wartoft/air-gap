package pinnedtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GenerateCommand handles standalone generation before service initialization.
func GenerateCommand(args []string, role string, output io.Writer) (bool, error) {
	requested := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "--generate-tls-keysets") {
			requested = true
		}
	}
	if !requested {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--tls-key-") {
				return true, fmt.Errorf("tls-key options require --generate-tls-keysets")
			}
		}
		return false, nil
	}
	flags := flag.NewFlagSet("generate-tls-keysets", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	count := flags.Int("generate-tls-keysets", 0, "number of independent key-sets")
	dir := flags.String("tls-key-output-dir", "", "output directory")
	name := flags.String("tls-key-name", "", "output filename prefix")
	keyRole := flags.String("tls-key-role", "", "client or server")
	if err := flags.Parse(args); err != nil {
		return true, err
	}
	if flags.NArg() != 0 || *count < 1 || *count > MaxPins || *dir == "" ||
		!validName(*name) ||
		*keyRole != role {
		return true, fmt.Errorf("generation requires count 1..%d, output directory, safe filename prefix, and role=%s", MaxPins, role)
	}
	if err := generate(*dir, *name, *keyRole, *count, output); err != nil {
		return true, err
	}
	return true, nil
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 64 || strings.HasPrefix(name, ".") {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

func generate(dir, name, role string, count int, output io.Writer) (err error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("unsafe output directory %s", dir)
	}
	type artifact struct {
		path string
		data []byte
	}
	var artifacts []artifact
	var fingerprints []string
	for i := 1; i <= count; i++ {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return err
		}
		now := time.Now()
		usage := x509.ExtKeyUsageClientAuth
		if role == "server" {
			usage = x509.ExtKeyUsageServerAuth
		}
		template := &x509.Certificate{
			SerialNumber: serial, Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		}
		cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			return err
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			return err
		}
		fingerprint := Fingerprint(public)
		base := filepath.Join(dir, fmt.Sprintf("%s-%d", name, i))
		artifacts = append(artifacts,
			artifact{base + ".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})},
			artifact{base + ".crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})},
			artifact{base + ".pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public})},
			artifact{base + ".fingerprint", []byte(fingerprint + "\n")},
		)
		fingerprints = append(fingerprints, fingerprint)
	}
	for _, item := range artifacts {
		if _, err := os.Lstat(item.path); err == nil {
			return fmt.Errorf("refusing to overwrite %s", item.path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	var created []string
	defer func() {
		if err != nil {
			for _, path := range created {
				if cleanupErr := os.Remove(path); cleanupErr != nil {
					err = fmt.Errorf("%w; cleanup %s: %v", err, path, cleanupErr)
				}
			}
		}
	}()
	for _, item := range artifacts {
		file, err := os.OpenFile(item.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		created = append(created, item.path)
		_, writeErr := file.Write(item.data)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for i, fingerprint := range fingerprints {
		if _, err := fmt.Fprintf(output, "%s-%d role=%s fingerprint=%s\n", name, i+1, role, fingerprint); err != nil {
			return err
		}
	}
	return nil
}
