package pinnedtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GenerateCommand handles standalone generation before service initialization.
func GenerateCommand(args []string, role string, output io.Writer) (bool, error) {
	requested := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "--generate-tls-keysets") {
			return true, fmt.Errorf("--generate-tls-keysets was removed; use --generate-tls-key once per identity")
		}
		if arg == "--generate-tls-key" || strings.HasPrefix(arg, "--generate-tls-key=") {
			requested = true
		}
	}
	if !requested {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--tls-key-") || strings.HasPrefix(arg, "--json-file-dir") {
				return true, fmt.Errorf("tls-key options require --generate-tls-key")
			}
		}
		return false, nil
	}
	flags := flag.NewFlagSet("generate-tls-key", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	generateKey := flags.Bool("generate-tls-key", false, "generate one independent TLS keyset")
	dir := flags.String("tls-key-output-dir", "", "output directory")
	name := flags.String("tls-key-name", "", "output filename prefix")
	keyRole := flags.String("tls-key-role", "", "client or server")
	jsonDir := flags.String("json-file-dir", "", "optional directory for public trust JSON entries")
	if err := flags.Parse(args); err != nil {
		return true, err
	}
	if !*generateKey || flags.NArg() != 0 || *dir == "" || !validName(*name) ||
		*keyRole != role {
		return true, fmt.Errorf("generation requires --generate-tls-key, output directory, safe filename prefix, and role=%s", role)
	}
	if err := generate(*dir, *name, *keyRole, 1, *jsonDir, output); err != nil {
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

func generate(dir, name, role string, count int, jsonDir string, output io.Writer) (err error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := prepareOutputDirectory(dir); err != nil {
		return err
	}
	if jsonDir != "" {
		if err := os.MkdirAll(jsonDir, 0700); err != nil {
			return err
		}
		if err := prepareOutputDirectory(jsonDir); err != nil {
			return err
		}
	}
	startIndex, err := nextKeysetIndex(name, dir, jsonDir)
	if err != nil {
		return err
	}
	if startIndex > int(^uint(0)>>1)-(count-1) {
		return fmt.Errorf("keyset index exceeds supported range")
	}
	type artifact struct {
		path string
		data []byte
	}
	var artifacts []artifact
	var fingerprints []string
	for offset := 0; offset < count; offset++ {
		i := startIndex + offset
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
		if jsonDir != "" {
			entry := Entry{
				Version: 1, Peer: fmt.Sprintf("%s-%d", name, i), Role: role,
				PublicKey:   string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public})),
				Fingerprint: fingerprint,
			}
			entryJSON, err := json.MarshalIndent(entry, "", "  ")
			if err != nil {
				return err
			}
			artifacts = append(artifacts, artifact{
				filepath.Join(jsonDir, fmt.Sprintf("%s-%d.json", name, i)),
				append(entryJSON, '\n'),
			})
		}
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
		index := startIndex + i
		line := fmt.Sprintf("%s-%d role=%s fingerprint=%s", name, index, role, fingerprint)
		if jsonDir != "" {
			line += " trust-entry=" + filepath.Join(jsonDir, fmt.Sprintf("%s-%d.json", name, index))
		}
		if _, err := fmt.Fprintln(output, line); err != nil {
			return err
		}
	}
	return nil
}

func nextKeysetIndex(name string, dirs ...string) (int, error) {
	maxIndex := 0
	extensions := map[string]bool{
		".key": true, ".crt": true, ".pub": true, ".fingerprint": true, ".json": true,
	}
	seenDirs := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if dir == "" || seenDirs[dir] {
			continue
		}
		seenDirs[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0, err
		}
		for _, entry := range entries {
			base := entry.Name()
			prefix := name + "-"
			if !strings.HasPrefix(base, prefix) {
				continue
			}
			extension := filepath.Ext(base)
			if !extensions[extension] {
				continue
			}
			suffix := strings.TrimSuffix(strings.TrimPrefix(base, prefix), extension)
			index, err := strconv.Atoi(suffix)
			if err != nil || index < 1 {
				continue
			}
			if index > maxIndex {
				maxIndex = index
			}
		}
	}
	if maxIndex == int(^uint(0)>>1) {
		return 0, fmt.Errorf("keyset index exceeds supported range")
	}
	return maxIndex + 1, nil
}

func prepareOutputDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("unsafe output directory %s", dir)
	}
	return nil
}
