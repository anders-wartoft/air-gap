package pinnedtls

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"strings"
)

// ValidateReloadFile checks syntax and local permissions before application
// parsing. Empty paths support deployments configured entirely by overrides.
func ValidateReloadFile(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("configuration %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("unsafe configuration file permissions/type: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, _, ok := strings.Cut(text, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("malformed configuration %s at line %d", path, line)
		}
	}
	return scanner.Err()
}

// CheckReloadSettings compares resolved, pre-validation configuration values,
// excluding only the explicitly reloadable paths. It never logs their values.
func CheckReloadSettings(previous, candidate any) error {
	a, b := reflect.ValueOf(previous), reflect.ValueOf(candidate)
	if a.Type() != b.Type() || a.Kind() != reflect.Struct {
		return fmt.Errorf("reload configuration types must match")
	}
	for i := 0; i < a.NumField(); i++ {
		name := a.Type().Field(i).Name
		switch name {
		case "tcpTLSCertFile", "tcpTLSKeyFile", "tcpTLSTrustedKeysDir":
			continue
		case "key", "newkey", "publicKey", "keyInfos", "translations", "filter", "inputFilter":
			// Runtime/derived state is not a configuration-file setting.
			continue
		}
		if !sameValue(a.Field(i), b.Field(i)) {
			return fmt.Errorf("setting %s changed; restart required", name)
		}
	}
	return nil
}

func sameValue(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.Slice, reflect.Array:
		if a.Kind() == reflect.Slice && a.IsNil() != b.IsNil() {
			return false
		}
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !sameValue(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		for _, key := range a.MapKeys() {
			value := b.MapIndex(key)
			if !value.IsValid() || !sameValue(a.MapIndex(key), value) {
				return false
			}
		}
		return true
	default:
		return a.Equal(b)
	}
}
