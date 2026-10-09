package downstream

import (
	"crypto/tls"
	"fmt"

	"sitia.nu/airgap/src/internal/pinnedtls"
)

func (c TransferConfiguration) validateTLSMode() error {
	if c.tcpTLSPinIdleSeconds < 0 || (c.tcpTLSPinIdleSeconds > 0 && c.tcpTLSAuthMode != "pinned") {
		return fmt.Errorf("tcpTLSPinIdleSeconds must be nonnegative and requires pinned TLS when enabled")
	}
	if c.tcpTLSAutomaticRotation && c.tcpTLSAuthMode != "pinned" {
		return fmt.Errorf("tcpTLSAutomaticRotation requires pinned TLS")
	}
	if err := pinnedtls.ValidateMode(c.tcpTLSAuthMode, c.tcpTLSTrustedKeysDir,
		c.tcpTLSCAFile, c.tcpTLSClientCNRegex, c.tcpTLSKeyPasswordFile, c.tcpTLSCipherSuites); err != nil {
		return err
	}
	if c.tcpTLSAuthMode == "pinned" &&
		(c.transport != "tcp" || c.tcpTLSClientAuth != "require" || c.tcpTLSCertFile == "" || c.tcpTLSKeyFile == "") {
		return fmt.Errorf("pinned TLS requires transport=tcp, tcpTLSClientAuth=require, tcpTLSCertFile and tcpTLSKeyFile")
	}
	return nil
}

func (c TransferConfiguration) BuildTLSConfig() (*tls.Config, error) {
	if err := c.validateTLSMode(); err != nil {
		return nil, err
	}
	if c.tcpTLSAuthMode == "pinned" {
		return pinnedtls.Build(c.tcpTLSCertFile, c.tcpTLSKeyFile, c.tcpTLSTrustedKeysDir, true)
	}
	if c.tcpTLSCertFile == "" {
		return nil, nil
	}
	return buildDownstreamTLSConfig(c.tcpTLSCertFile, c.tcpTLSKeyFile,
		c.tcpTLSKeyPasswordFile, c.tcpTLSCAFile, c.tcpTLSClientAuth, c.tcpTLSCipherSuites)
}

func (t *TCPAdapter) reloadPinned(candidate TransferConfiguration) error {
	t.rotationMu.Lock()
	defer t.rotationMu.Unlock()
	if err := candidate.validateTLSMode(); err != nil {
		return err
	}
	if err := pinnedtls.ValidateRotationStore(candidate.tcpTLSTrustedKeysDir); err != nil {
		return err
	}
	snapshot, err := pinnedtls.Prepare(candidate.tcpTLSCertFile, candidate.tcpTLSKeyFile, candidate.tcpTLSTrustedKeysDir, true)
	if err != nil {
		return err
	}
	result, disconnectErr := t.pinManager.Activate(snapshot)
	t.trustedDir = candidate.tcpTLSTrustedKeysDir
	Logger.Infof("Pinned TLS reload: peers=%d keys=%d added=%v removed=%v disconnected=%d",
		result.Peers, result.Keys, result.Added, result.Removed, result.Disconnected)
	if disconnectErr != nil {
		Logger.Errorf("Pinned TLS revocation disconnect: %v", disconnectErr)
	}
	return nil
}

func reloadPinnedConfiguration(path string, overrides []string, active *TransferConfiguration, adapter *TCPAdapter) error {
	if err := pinnedtls.ValidateReloadFile(path); err != nil {
		return err
	}
	candidate, err := readConfigurationFile(path, defaultConfiguration(), true)
	if err != nil {
		return err
	}
	candidate = parseCommandLineOverrides(overrides, overrideConfiguration(candidate))
	if err := pinnedtls.CheckReloadSettings(*active, candidate); err != nil {
		return err
	}
	if err := adapter.reloadPinned(candidate); err != nil {
		return err
	}
	*active = candidate
	return nil
}
