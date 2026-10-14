package logging

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Configuration keys recognised by ApplyOption. They are shared by every
// air-gap binary so that log integrity is configured identically everywhere.
const (
	// OptionFormat selects text or otel output.
	OptionFormat = "logFormat"
	// OptionHash names the payload hash algorithm, or none.
	OptionHash = "hash"
	// OptionHashChain sets the anchor interval of the hash chain.
	OptionHashChain = "hash-chain"
	// OptionSigningCertificate identifies the dedicated log-signing certificate.
	OptionSigningCertificate = "log-signing-cert"
	// OptionSigningKey identifies the private key for the signing certificate.
	OptionSigningKey = "log-signing-key"
	// OptionSignerID identifies the machine or signing service.
	OptionSignerID = "log-signer-id"
	// OptionSigningEKU requires a custom signing EKU when set.
	OptionSigningEKU = "log-signing-eku"
)

// IsOption reports whether a configuration key belongs to the logging package.
func IsOption(key string) bool {
	switch key {
	case OptionFormat, OptionHash, OptionHashChain,
		OptionSigningCertificate, OptionSigningKey, OptionSignerID, OptionSigningEKU:
		return true
	default:
		return false
	}
}

// ApplyOption applies a shared logging option. It returns false when the key
// belongs to the caller rather than to logging, so a configuration parser can
// delegate unknown keys without having to know which ones exist here.
func ApplyOption(key, value string) (bool, error) {
	switch key {
	case OptionFormat:
		if err := Logger.SetFormat(value); err != nil {
			return true, fmt.Errorf("error in config %s: %w", OptionFormat, err)
		}
		return true, nil

	case OptionHash:
		if err := Logger.SetHashAlgorithm(value); err != nil {
			return true, fmt.Errorf("error in config %s: %w", OptionHash, err)
		}
		return true, nil

	case OptionHashChain:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return true, nil
		}

		length, err := strconv.Atoi(trimmed)
		if err != nil {
			return true, fmt.Errorf("error in config %s. Illegal value: %s. Legal values are 0 or a positive integer",
				OptionHashChain, value)
		}

		if err := Logger.SetHashChainLength(length); err != nil {
			return true, fmt.Errorf("error in config %s: %w", OptionHashChain, err)
		}
		return true, nil

	case OptionSigningCertificate, OptionSigningKey, OptionSignerID, OptionSigningEKU:
		signing := currentSigningConfiguration()
		switch key {
		case OptionSigningCertificate:
			signing.certificatePath = value
		case OptionSigningKey:
			signing.keyPath = value
		case OptionSignerID:
			signing.signerID = value
		case OptionSigningEKU:
			signing.requiredEKU = value
		}
		// Configuration files may list these related options in any order.
		// Defer validation until both files are known; once both are present,
		// ConfigureSigningCertificate performs the complete validation.
		if strings.TrimSpace(signing.certificatePath) == "" ||
			strings.TrimSpace(signing.keyPath) == "" {
			signingConfig = signing
			return true, nil
		}
		if err := Logger.SetSigningCertificate(
			signing.certificatePath,
			signing.keyPath,
			signing.signerID,
			signing.requiredEKU,
		); err != nil {
			return true, fmt.Errorf("error in config %s: %w", key, err)
		}
		signingConfig = signing
		return true, nil

	default:
		return false, nil
	}
}

// environmentBindings maps the environment variable suffixes to the options
// they override. The binaries use different prefixes, such as AIRGAP_UPSTREAM_,
// but the suffixes are the same everywhere.
var environmentBindings = []struct {
	suffix string
	option string
}{
	{"LOG_FORMAT", OptionFormat},
	{"LOG_HASH", OptionHash},
	{"LOG_HASH_CHAIN", OptionHashChain},
	{"LOG_SIGNING_CERT", OptionSigningCertificate},
	{"LOG_SIGNING_KEY", OptionSigningKey},
	{"LOG_SIGNER_ID", OptionSignerID},
	{"LOG_SIGNING_EKU", OptionSigningEKU},
}

type signingConfiguration struct {
	certificatePath string
	keyPath         string
	signerID        string
	requiredEKU     string
}

var signingConfig signingConfiguration

func currentSigningConfiguration() signingConfiguration {
	return signingConfig
}

// ApplyEnvironmentOptions applies the shared logging options that are set
// through environment variables, overriding whatever the configuration file
// said. The prefix is the binary specific one, for example "AIRGAP_UPSTREAM_".
func ApplyEnvironmentOptions(prefix string) error {
	for _, binding := range environmentBindings {
		name := prefix + binding.suffix

		value := os.Getenv(name)
		if value == "" {
			continue
		}

		Logger.Info(Event(EventConfigOverride,
			Str("airgap.config.setting", binding.option),
			Str("airgap.config.source", name),
			Str("airgap.config.value", value),
		), "Overriding "+binding.option+" with environment variable: "+name+" with value: "+value)

		if _, err := ApplyOption(binding.option, value); err != nil {
			return err
		}
	}

	return nil
}
