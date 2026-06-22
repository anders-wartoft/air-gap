# Encryption

Air-gap provides two independent layers of encryption:

1. **UDP payload encryption** — symmetric-key encryption of event payloads sent over the UDP transport. The key is automatically rotated and exchanged using public-key cryptography. Works with both UDP and TCP transports.
2. **TCP transport TLS** — TLS 1.2/1.3 for the TCP transport layer. Supports one-way TLS (server authentication only) and mutual TLS (mTLS, both sides authenticate). Not applicable to UDP transport.

## UDP Payload Encryption

When `publicKeyFile` is set on upstream, every event payload is encrypted with a randomly generated symmetric key before transmission. The symmetric key itself is encrypted with the receiver's public key and sent as a key-exchange event. Downstream decrypts the key with its private key and uses it to decrypt subsequent payloads.

### Configuration

**Upstream** — set the receiver's public key file:

```properties
publicKeyFile=certs/server.pem
generateNewSymmetricKeyEvery=500
```

**Downstream** — set the glob pointing to one or more private key files:

```properties
privateKeyFiles=certs/private*.pem
```

Multiple private key files are tried in sequence, so key rotation on downstream does not cause gaps.

| Property (upstream) | Env variable | Description |
| ------------------- | ------------ | ----------- |
| `publicKeyFile` | `AIRGAP_UPSTREAM_PUBLIC_KEY_FILE` | PEM-encoded public key of the receiver. Encryption is disabled when empty |
| `generateNewSymmetricKeyEvery` | `AIRGAP_UPSTREAM_GENERATE_NEW_SYMMETRIC_KEY_EVERY` | Seconds between symmetric key rotations |

| Property (downstream) | Env variable | Description |
| --------------------- | ------------ | ----------- |
| `privateKeyFiles` | `AIRGAP_DOWNSTREAM_PRIVATE_KEY_FILES` | Glob covering all private key PEM files to load |

### Key Generation

```bash
# Generate RSA key pair (adjust key size for your security policy)
openssl genrsa -out server.key 4096
openssl rsa -in server.key -pubout -out server.pem
```

Place `server.pem` on upstream and `server.key` on downstream. Keep `server.key` confidential.

---

## TCP Transport TLS

When `transport=tcp` and TLS is configured, the TCP connection is protected by TLS 1.3 by default (TLS 1.2 is also supported with explicit cipher suite selection). Mutual TLS (mTLS) is supported: downstream can require upstream to present a client certificate, and upstream validates the server certificate CN against a configurable regex (useful for load-balanced deployments).

### Certificate Generation

Certificates follow the X.509 standard and use `.crt` / `.key` / `.key.pw` file extensions. Private keys are AES-256 encrypted; the passphrase is stored in a separate password file (one line, no trailing whitespace, not in shell history).

#### Certificate Authority

Create a CA once per environment. In production this is typically done on an air-gapped machine.

```bash
openssl genrsa -out ca.key 4096
openssl req -new -x509 -key ca.key -out ca.crt -days 3650 -subj "/C=SE/O=MyOrg/CN=AirGap-CA"
```

#### Upstream Certificate

The Common Name (CN) is matched by downstream via `tcpTLSClientCNRegex`.

```bash
# Capture passphrase without shell history
# Linux:
read -s -p "Enter passphrase for upstream key: " UPSTREAM_PW
# macOS:
printf "Enter passphrase for upstream key: "; read -s UPSTREAM_PW
# Windows (PowerShell):
$UPSTREAM_PW = Read-Host "Enter passphrase for upstream key"

# Save passphrase and restrict permissions
printf '%s' "$UPSTREAM_PW" > upstream.key.pw && unset UPSTREAM_PW
chmod 600 upstream.key.pw

# Generate AES-256 encrypted private key
openssl genrsa -aes256 -passout file:upstream.key.pw -out upstream.key 2048

# Create CSR — adjust -subj to match your organisation and CN naming scheme
openssl req -new -key upstream.key -passin file:upstream.key.pw -out upstream.csr -subj "/C=SE/O=MyOrg/CN=nu.sitia.airgap.upstream-1"

# Sign with CA
openssl x509 -req -in upstream.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out upstream.crt -days 825
```

#### Downstream Certificate

The Common Name (CN) is matched by upstream via `tcpTLSServerCNRegex`.

```bash
# Linux:
read -s -p "Enter passphrase for downstream key: " DOWNSTREAM_PW
# macOS:
printf "Enter passphrase for downstream key: "; read -s DOWNSTREAM_PW
# Windows (PowerShell):
$DOWNSTREAM_PW = Read-Host "Enter passphrase for downstream key"

printf '%s' "$DOWNSTREAM_PW" > downstream.key.pw && unset DOWNSTREAM_PW
chmod 600 downstream.key.pw

openssl genrsa -aes256 -passout file:downstream.key.pw -out downstream.key 2048

openssl req -new -key downstream.key -passin file:downstream.key.pw -out downstream.csr -subj "/C=SE/O=MyOrg/CN=nu.sitia.airgap.downstream-1"

openssl x509 -req -in downstream.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out downstream.crt -days 825
```

> **Key size**: Use at least 2048-bit RSA keys. Go 1.23+ hard-rejects RSA keys larger than 8192 bits.

#### Deploy Files

| File | Upstream config property | Downstream config property |
| ---- | ------------------------ | -------------------------- |
| `ca.crt` | `tcpTLSCAFile` (verify server cert) | `tcpTLSCAFile` (verify client cert) |
| `upstream.crt` | `tcpTLSCertFile` (mTLS client cert) | — |
| `upstream.key` | `tcpTLSKeyFile` | — |
| `upstream.key.pw` | `tcpTLSKeyPasswordFile` | — |
| `downstream.crt` | — | `tcpTLSCertFile` (server cert) |
| `downstream.key` | — | `tcpTLSKeyFile` |
| `downstream.key.pw` | — | `tcpTLSKeyPasswordFile` |

### Upstream TLS Settings

| Property | Env variable | Default | Description |
| -------- | ------------ | ------- | ----------- |
| `tcpTLSEnabled` | `AIRGAP_UPSTREAM_TCP_TLS_ENABLED` | `false` | Enable TLS on the TCP connection. Requires `tcpTLSCAFile` |
| `tcpTLSCertFile` | `AIRGAP_UPSTREAM_TCP_TLS_CERT_FILE` | | Client certificate PEM file (`.crt`). Required for mTLS when downstream has `tcpTLSClientAuth=require` |
| `tcpTLSKeyFile` | `AIRGAP_UPSTREAM_TCP_TLS_KEY_FILE` | | Client private key PEM file. Required together with `tcpTLSCertFile` |
| `tcpTLSKeyPasswordFile` | `AIRGAP_UPSTREAM_TCP_TLS_KEY_PASSWORD_FILE` | | Path to a file containing the passphrase for an encrypted `tcpTLSKeyFile` |
| `tcpTLSCAFile` | `AIRGAP_UPSTREAM_TCP_TLS_CA_FILE` | | CA certificate PEM file used to verify the downstream server certificate. Required when `tcpTLSEnabled=true` |
| `tcpTLSServerCNRegex` | `AIRGAP_UPSTREAM_TCP_TLS_SERVER_CN_REGEX` | | Go regex matched against the server certificate CN. Enables connecting by IP or through a load balancer where backends may present different CNs. The CA chain is always verified. Example: `^nu\.sitia\.airgap\.downstream-[0-9]+$`. Also accepted as `tcpTLSServerName` (legacy) |
| `tcpTLSCipherSuites` | `AIRGAP_UPSTREAM_TCP_TLS_CIPHER_SUITES` | | TLS version/cipher policy — see [Cipher Suites](#cipher-suites) below |

### Downstream TLS Settings

| Property | Env variable | Default | Description |
| -------- | ------------ | ------- | ----------- |
| `tcpTLSCertFile` | `AIRGAP_DOWNSTREAM_TCP_TLS_CERT_FILE` | | Server certificate PEM file (`.crt`). Setting this enables TLS on the TCP listener |
| `tcpTLSKeyFile` | `AIRGAP_DOWNSTREAM_TCP_TLS_KEY_FILE` | | Server private key PEM file |
| `tcpTLSKeyPasswordFile` | `AIRGAP_DOWNSTREAM_TCP_TLS_KEY_PASSWORD_FILE` | | Path to a file containing the passphrase for an encrypted `tcpTLSKeyFile` |
| `tcpTLSCAFile` | `AIRGAP_DOWNSTREAM_TCP_TLS_CA_FILE` | | CA certificate PEM file used to verify client certificates. Required when `tcpTLSClientAuth` is `allow` or `require` |
| `tcpTLSClientAuth` | `AIRGAP_DOWNSTREAM_TCP_TLS_CLIENT_AUTH` | `none` | Client certificate policy: `none` — no client cert required (one-way TLS); `allow` — accept and verify client cert if presented; `require` — client cert is mandatory (mTLS) |
| `tcpTLSClientCNRegex` | `AIRGAP_DOWNSTREAM_TCP_TLS_CLIENT_CN_REGEX` | | Go regex matched against the client certificate CN. Connections whose CN does not match are rejected. Only evaluated when a client cert is presented. Example: `^nu\.sitia\.airgap\.upstream-[0-9]+$` |
| `tcpTLSCipherSuites` | `AIRGAP_DOWNSTREAM_TCP_TLS_CIPHER_SUITES` | | TLS version/cipher policy — see [Cipher Suites](#cipher-suites) below |

### Example Configuration

**`config/upstream-tls.properties`** (mTLS client, connects by IP with CN regex):

```properties
transport=tcp
tcpTLSEnabled=true
tcpTLSCertFile=certs/upstream.crt
tcpTLSKeyFile=certs/upstream.key
tcpTLSKeyPasswordFile=certs/upstream.key.pw
tcpTLSCAFile=certs/ca.crt
# Regex matched against the server certificate CN. Accepts any CN matching the pattern,
# which handles load-balanced setups where different backends may present different certs.
# The CA chain is always verified regardless of this setting.
tcpTLSServerCNRegex=^nu\.sitia\.airgap\.downstream-[0-9]+$
```

**`config/downstream-tls.properties`** (mTLS server, requires client cert):

```properties
transport=tcp
tcpTLSCertFile=certs/downstream.crt
tcpTLSKeyFile=certs/downstream.key
tcpTLSKeyPasswordFile=certs/downstream.key.pw
tcpTLSCAFile=certs/ca.crt
tcpTLSClientAuth=require
tcpTLSClientCNRegex=^nu\.sitia\.airgap\.upstream-[0-9]+$
```

### Cipher Suites

The `tcpTLSCipherSuites` property accepts two formats:

| Value | Behaviour |
| ----- | --------- |
| Empty or `TLS1.3` | **Enforce TLS 1.3 only** (default and recommended). Cipher suites are fixed by the TLS 1.3 standard; all are NIST-approved |
| Comma-separated TLS 1.2 suite names | Use TLS 1.2 with those specific suites. Only NIST-approved suites from Go's `tls.CipherSuites()` are accepted; insecure legacy suites are rejected at startup |

Example TLS 1.2 override (only needed for interoperability with legacy endpoints):

```properties
tcpTLSCipherSuites=TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
```

### Certificate Rotation with SIGHUP

Certificates and keys can be replaced on disk and reloaded without stopping either process or losing any packets. Send `SIGHUP` to the process after the new files are in place.

**Downstream** (server — zero impact on live connections):

The server uses a `GetCertificate` callback that is invoked on every new TLS handshake. On SIGHUP the new certificate is loaded from disk and stored in an atomic pointer. Connections that are already established keep their existing TLS session and are never touched. New connections immediately use the new certificate.

**Upstream** (client — one lossless reconnect):

On SIGHUP the TLS configuration is rebuilt from the new cert files and the current TCP connection is closed. The retry loop (`tcpRetryTimes=0`) detects the closed connection, redials with the new configuration, and retries the message that was in flight. Because the Kafka offset is not committed until a send succeeds, no messages are lost regardless of how long the reconnect takes.

**Procedure:**

```bash
# 1. Generate new certificates (see Certificate Generation above)
# 2. Copy the new .crt, .key, and .key.pw files to the same paths configured
#    in tcpTLSCertFile / tcpTLSKeyFile / tcpTLSKeyPasswordFile
# 3. Signal both processes — downstream first so the new server cert is live
#    before upstream reconnects with the new client cert:
kill -HUP $(pidof downstream)
kill -HUP $(pidof upstream)
```

Expected log output on downstream:

```text
[INFO] [TLS downstream] Server certificate reloaded from certs/downstream.crt
```

Expected log output on upstream:

```text
[INFO] [TLS upstream] TLS certificates reloaded from certs/upstream.crt; reconnecting with new cert
[INFO] Connected to TCP server at 127.0.0.1:1234
[INFO] [TLS upstream] Authenticated server CN "nu.sitia.airgap.downstream-1" (pattern "…")
```

> Rotating only the downstream cert (without a matching upstream SIGHUP) is safe — upstream will continue to use its existing client cert until its own SIGHUP is sent.

### Encrypted Private Key Format

Private keys encrypted with OpenSSL 3.x are stored in PKCS#8 format (`BEGIN ENCRYPTED PRIVATE KEY`). Older OpenSSL versions produce the legacy PKCS#1 format (`BEGIN RSA PRIVATE KEY` with a `DEK-Info` header). Air-gap supports both formats transparently.

### Diagnostic Logging

Set `logLevel=DEBUG` to see detailed TLS handshake information:

- Server/client certificate subject, issuer, CN, validity dates, and public key size
- Which CN was accepted or rejected and which regex pattern was used
- Chain verification result
- TLS version negotiated

On authentication failure, ERROR/WARN log messages include the specific cause and a remediation hint:

| Cause | Log level | Hint |
| ----- | --------- | ---- |
| RSA key > 8192 bits | ERROR | Regenerate certificate with 2048- or 4096-bit key |
| Certificate expired or not yet valid | ERROR | Check system clock and certificate dates |
| Unknown CA / chain error | ERROR | Check `tcpTLSCAFile` matches the issuing CA |
| Client cert rejected by server | WARN | Check `tcpTLSCertFile`/`tcpTLSKeyFile` and CA match |
| CN does not match regex | ERROR/WARN | Update `tcpTLSServerCNRegex`/`tcpTLSClientCNRegex` or regenerate certificate |
| No client cert presented | WARN | Client must present a cert when `tcpTLSClientAuth=require` |
