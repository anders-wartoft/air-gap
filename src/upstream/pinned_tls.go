package upstream

import (
	"crypto/tls"
	"fmt"
	"net"

	"sitia.nu/airgap/src/internal/pinnedtls"
)

func (c TransferConfiguration) validateTLSMode() error {
	if c.tcpTLSPinIdleSeconds < 0 || (c.tcpTLSPinIdleSeconds > 0 && c.tcpTLSAuthMode != "pinned") {
		return fmt.Errorf("tcpTLSPinIdleSeconds must be nonnegative and requires pinned TLS when enabled")
	}
	if c.tcpTLSRotationEnabled && c.tcpTLSAuthMode != "pinned" {
		return fmt.Errorf("tcpTLSRotationEnabled requires pinned TLS")
	}
	if err := pinnedtls.ValidateMode(c.tcpTLSAuthMode, c.tcpTLSTrustedKeysDir,
		c.tcpTLSCAFile, c.tcpTLSServerCNRegex, c.tcpTLSKeyPasswordFile, c.tcpTLSCipherSuites); err != nil {
		return err
	}
	if c.tcpTLSAuthMode == "pinned" &&
		(c.transport != "tcp" || !c.tcpTLSEnabled || c.tcpTLSCertFile == "" || c.tcpTLSKeyFile == "") {
		return fmt.Errorf("pinned TLS requires transport=tcp, tcpTLSEnabled=true, tcpTLSCertFile and tcpTLSKeyFile")
	}
	return nil
}

func (c TransferConfiguration) BuildTLSConfig() (*tls.Config, error) {
	if err := c.validateTLSMode(); err != nil {
		return nil, err
	}
	if c.tcpTLSAuthMode == "pinned" {
		return pinnedtls.Build(c.tcpTLSCertFile, c.tcpTLSKeyFile, c.tcpTLSTrustedKeysDir, false)
	}
	if !c.tcpTLSEnabled {
		return nil, nil
	}
	return buildUpstreamTLSConfig(c.tcpTLSCAFile, c.tcpTLSCertFile, c.tcpTLSKeyFile,
		c.tcpTLSKeyPasswordFile, c.tcpTLSCipherSuites, c.tcpTLSServerCNRegex)
}

func (t *TCPAdapter) admit(conn net.Conn) error {
	if t.pinManager == nil {
		return nil
	}
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return fmt.Errorf("pinned transport requires TLS")
	}
	peer, err := t.pinManager.Admit(tlsConn)
	if err == nil {
		Logger.Infof("Pinned TLS authenticated server peer=%s fingerprint=%s", peer.Peer,
			pinnedtls.Fingerprint(tlsConn.ConnectionState().PeerCertificates[0].RawSubjectPublicKeyInfo))
	}
	return err
}

func (t *TCPAdapter) forget(conn net.Conn) {
	if tlsConn, ok := conn.(*tls.Conn); ok && t.pinManager != nil {
		t.pinManager.Forget(tlsConn)
	}
}

func (t *TCPAdapter) reloadPinned(candidate TransferConfiguration) error {
	if err := candidate.validateTLSMode(); err != nil {
		return err
	}
	snapshot, err := pinnedtls.Prepare(candidate.tcpTLSCertFile, candidate.tcpTLSKeyFile, candidate.tcpTLSTrustedKeysDir, false)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	oldIdentity := t.pinManager.Identity()
	if candidate.tcpTLSRotationEnabled && oldIdentity != snapshot.Identity() {
		oldConfig := t.pinManager.Config()
		if err := pinnedtls.SyncIdentity(candidate.tcpTLSCertFile, candidate.tcpTLSKeyFile); err != nil {
			return fmt.Errorf("persist candidate identity: %w", err)
		}
		if err := pinnedtls.SavePendingClient(candidate.tcpTLSTrustedKeysDir, oldConfig, snapshot); err != nil {
			return fmt.Errorf("persist rotation recovery identity: %w", err)
		}
		connection, err := t.dialConfig(oldConfig)
		if err != nil {
			return fmt.Errorf("rotation old-key connection: %w", err)
		}
		tlsConn := connection.(*tls.Conn)
		id, exchangeErr := pinnedtls.RequestRotation(tlsConn, oldConfig, snapshot)
		tlsConn.Close()
		if exchangeErr != nil {
			return fmt.Errorf("rotation rejected id=%s: %w", id, exchangeErr)
		}
		if err := pinnedtls.ClearPendingClient(candidate.tcpTLSTrustedKeysDir); err != nil {
			return fmt.Errorf("finalize client rotation: %w", err)
		}
		Logger.Infof("Pinned rotation accepted id=%s old=%s new=%s", id, oldIdentity, snapshot.Identity())
	}
	result, disconnectErr := t.pinManager.Activate(snapshot)
	t.tlsConfig = t.pinManager.Config()
	t.generation++
	if oldIdentity != t.pinManager.Identity() && t.conn != nil {
		t.forget(t.conn)
		if err := t.conn.(*tls.Conn).NetConn().Close(); err != nil {
			Logger.Errorf("Pinned identity reconnect close: %v", err)
		}
		t.conn = nil
	}
	t.tlsCertFile, t.tlsKeyFile = candidate.tcpTLSCertFile, candidate.tcpTLSKeyFile
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
	candidate, err := ReadParameters(path, DefaultConfiguration())
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
