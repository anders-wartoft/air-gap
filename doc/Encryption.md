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

### Pinned Public-Key TLS (initial implementation)

Set `tcpTLSAuthMode=pinned` to authorize exact peer public keys without a CA.
The default remains `ca`; the certificate instructions below describe that
existing mode. Pinned mode requires mutual authentication, ECDSA P-256
identity keys, and TLS 1.3. Public pins are SHA-256 fingerprints of DER SPKI,
not hashes of certificate files. TLS proves possession of the private key;
copying a public key or certificate alone cannot authenticate a peer.

Generate local identities without starting the services:

```bash
upstream --generate-tls-keysets=2 --tls-key-output-dir=./client-identity --tls-key-name=sender --tls-key-role=client
downstream --generate-tls-keysets=1 --tls-key-output-dir=./server-identity --tls-key-name=receiver --tls-key-role=server
```

Each set produces `.key` (unencrypted PKCS#8 PEM, mode 0600), `.crt`,
`.pub` (PUBLIC KEY PEM), and `.fingerprint` files, for example
`sender-1.key` through `sender-1.fingerprint`. Output includes only public
fingerprints. Generation rejects occupied output paths instead of overwriting
keys. Names contain 1-64 ASCII letters/digits, dots, underscores, or hyphens,
and cannot start with a dot. Counts range from 1 to 2,000.

Keep private keys local. Copy only the peer's `.pub` and `.fingerprint` files
through trusted provisioning; optionally verify fingerprints independently
over a known voice channel. A fingerprint copied alongside a substituted key
does not authenticate the sender of that copy.

Create a public JSON trust entry on downstream, such as
`trusted-clients/sender-1.json`:

```json
{
  "version": 1,
  "peer": "sender-a",
  "role": "client",
  "publicKey": "-----BEGIN PUBLIC KEY-----\n<contents of sender-1.pub>\n-----END PUBLIC KEY-----\n",
  "fingerprint": "SHA256:<contents of sender-1.fingerprint after SHA256:>"
}
```

Use the complete exported PEM as the JSON string (escape its newlines),
and the complete exported fingerprint as the fingerprint string; the
placeholders above are not valid keys. On upstream, install an equivalent
entry for the receiver using `"role": "server"` and a local peer name.
Multiple entries for the same peer authorize old/new keys simultaneously.
Conflicting peer assignments for one fingerprint are rejected.

**Upstream:**

```properties
transport=tcp
tcpTLSEnabled=true
tcpTLSAuthMode=pinned
tcpTLSCertFile=client-identity/sender-1.crt
tcpTLSKeyFile=client-identity/sender-1.key
tcpTLSTrustedKeysDir=trusted-servers
```

**Downstream:**

```properties
transport=tcp
tcpTLSAuthMode=pinned
tcpTLSClientAuth=require
tcpTLSCertFile=server-identity/receiver-1.crt
tcpTLSKeyFile=server-identity/receiver-1.key
tcpTLSTrustedKeysDir=trusted-clients
```

Leave `tcpTLSCAFile`, CN regex, and `tcpTLSKeyPasswordFile` unset in pinned
mode. TLS 1.2 cipher selections are rejected. Both new properties also
support `--property=value` overrides and environment variables:

| Property | Upstream environment | Downstream environment |
| --- | --- | --- |
| `tcpTLSAuthMode` | `AIRGAP_UPSTREAM_TCP_TLS_AUTH_MODE` | `AIRGAP_DOWNSTREAM_TCP_TLS_AUTH_MODE` |
| `tcpTLSTrustedKeysDir` | `AIRGAP_UPSTREAM_TCP_TLS_TRUSTED_KEYS_DIR` | `AIRGAP_DOWNSTREAM_TCP_TLS_TRUSTED_KEYS_DIR` |

Public entries and directories must not be group/other-writable; use mode
0600 entries and mode 0700 directories. Private key files must have no
group/other permissions. Symlink entries, malformed JSON/keys, mismatched
fingerprints, wrong roles, unknown JSON fields, and unrecognized directory
entries are rejected. Hidden and `*.tmp` files are ignored for staging.
Maximums: 16 KiB per identity/trust file, 2,000 JSON entries per trust store,
and 4,096 total directory entries including staged files. An existing empty
trust directory loads successfully but denies every peer. Missing or
unreadable directories fail startup.

### Pinned identity/trust reload and manual rotation

Stage public entries as hidden or `*.tmp` files, then atomically rename them
to `*.json` in the trusted directory. On SIGHUP, both applications reread the
original configuration file and reapply their startup environment/CLI
overrides. Only `tcpTLSCertFile`, `tcpTLSKeyFile`, and
`tcpTLSTrustedKeysDir` may change in pinned mode; changes to authentication
mode or any other resolved setting require restart and reject the entire
candidate. Configuration files must be regular files without group/other
write permission. Invalid syntax/settings, mismatched identities, and
missing/malformed trust stores retain the last valid identity and trust.

```bash
kill -HUP <downstream-pid>
kill -HUP <upstream-pid>
```

Successful reload logs peer/key counts, added/removed public fingerprints,
and revocation disconnect counts. Removing a pin and successfully reloading
immediately closes sessions using it and rejects full/resumed handshakes.
Other authorized sessions remain open. An empty valid store revokes all
peer trust. Removing a file **without** SIGHUP does not activate revocation.
File replacement under an unchanged identity path is also reread.

For manual client rotation:

1. Install the new client public entry alongside the old entry on downstream,
   under the same peer identity, and SIGHUP downstream.
2. Select the new client certificate/key paths in upstream configuration
   (or replace their contents atomically), then SIGHUP upstream.
3. Confirm a fresh connection authenticated by the replacement fingerprint.
4. Remove the old client entry and SIGHUP downstream.

For manual server rotation, first install both server pins on upstream and
SIGHUP upstream, then change downstream's identity and SIGHUP downstream.
Confirm the replacement on a fresh connection before removing the old server
pin and reloading upstream. Existing sessions can still identify the old
server key during overlap; they are closed when that pin is revoked.
Environment/CLI identity-path overrides take precedence over file edits.

### Optional in-memory idle-pin aging

Both endpoints support `tcpTLSPinIdleSeconds` (default `0`, disabled).
For example, `tcpTLSPinIdleSeconds=3600` removes unused overlapping peer pins
from active memory after an idle hour. It requires pinned mode; valid values
are integer seconds in `0..2147483647`. The policy requires restart to change.
It also supports `--tcpTLSPinIdleSeconds=3600` and environment variables
`AIRGAP_UPSTREAM_TCP_TLS_PIN_IDLE_SECONDS` /
`AIRGAP_DOWNSTREAM_TCP_TLS_PIN_IDLE_SECONDS`.

* Aging is per trusted peer public key, not per local private identity.
* Startup and every successful SIGHUP give all loaded pins a fresh idle period.
  Failed reloads do not restore aged pins or reset timers.
* Successful authenticated session admission resets a pin's idle timer and
  records it as used. Any active session protects its pin, even if traffic
  is quiet. When the last session closes, its idle period starts again.
* Each peer always retains its last successfully used key. If none was used
  (or use times tie), lexical fingerprint order selects one deterministically.
  Multiple connected keys can remain. Therefore this is not a hard key-count
  cap and cannot eliminate the last route for a peer to reconnect.
* A background sweep runs once per second. Expired unused overlap pins are
  removed at the next sweep; full/resumed authentication then rejects them.
  Their files, replay records, and recovery state are never removed.
* SIGHUP or restart restores the disk-backed pins. Automatic rotation does
  not implicitly restore other aged pins; selecting an aged replacement
  requires SIGHUP on its verifier first.

This is **temporary overlap cleanup, not expiry or compromise revocation**.
It is useful for reducing unused active authorizations, but dormant backup
keys may need a reload before use. Allow enough overlap time for operators
and rotation to complete. Permanently revoke a key by removing its trust
entry and successfully reloading, as described above.

### Optional acknowledged client rotation

Set upstream `tcpTLSRotationEnabled=true` to request confirmation before
switching its configured identity. Downstream can confirm preauthorized
replacements by default. To permit new client pins under the already
authenticated client's local identity, explicitly set downstream
`tcpTLSAutomaticRotation=true`. Both flags default to false and changes
require restart. They apply only to pinned TCP, not CA TLS or UDP.

The exchange reuses the existing key-exchange framing with a separate pinned
subtype. It uses a connection-bound challenge and new-key possession proof,
and acknowledges only after the public entry/replay record are durable and
trust is active. Rejected or lost acknowledgment retains the old identity.
An owner-only local recovery file retains that identity across upstream
restart; SIGHUP retries the configured replacement. Copy only public export
files, never the whole upstream trust directory, which may contain private
recovery material.

For exact wire fields, file formats, limits, recovery/reconciliation rules,
and environment/CLI names see [PinnedRotation.md](./PinnedRotation.md).
Old-key compromise can authorize its replacement under automatic policy;
this is not independent compromise recovery. Retire old pins explicitly.

**Current limitations:** Trust-entry expiry, measured performance acceptance,
and the exhaustive crash/fault/logging/concurrency matrix remain pending.
Ambiguous incomplete intents require explicit administrator reconciliation;
retries never reconstruct a removed replacement. One downstream process
writes each trusted directory. CA-mode certificate reload is unchanged.
Rotation retains retryable work but does not add delivery acknowledgments
or an exactly-once/no-loss guarantee.

The certificate wrapper's name, issuer, and validity dates do not authorize
a pin. The generated wrapper has a one-year validity period, but pinned
verification intentionally ignores those dates. Permanent trust lasts until
the pin is removed and a successful reload (or restart) activates that
removal; optional idle aging can temporarily deactivate overlapping pins.
Key review and compromise recovery
remain operational responsibilities. This mode does not by itself establish
FIPS compliance, post-quantum assurance, or a no-loss delivery guarantee.

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
| `tcpTLSEnabled` | `AIRGAP_UPSTREAM_TCP_TLS_ENABLED` | `false` | Enable TLS on the TCP connection. Requires `tcpTLSCAFile` in CA mode |
| `tcpTLSCertFile` | `AIRGAP_UPSTREAM_TCP_TLS_CERT_FILE` | | Client certificate PEM file (`.crt`). Required for mTLS when downstream has `tcpTLSClientAuth=require` |
| `tcpTLSKeyFile` | `AIRGAP_UPSTREAM_TCP_TLS_KEY_FILE` | | Client private key PEM file. Required together with `tcpTLSCertFile` |
| `tcpTLSKeyPasswordFile` | `AIRGAP_UPSTREAM_TCP_TLS_KEY_PASSWORD_FILE` | | Path to a file containing the passphrase for an encrypted `tcpTLSKeyFile` |
| `tcpTLSCAFile` | `AIRGAP_UPSTREAM_TCP_TLS_CA_FILE` | | CA certificate PEM file used to verify the downstream server certificate. Required when `tcpTLSEnabled=true` in CA mode |
| `tcpTLSServerCNRegex` | `AIRGAP_UPSTREAM_TCP_TLS_SERVER_CN_REGEX` | | Go regex matched against the server certificate CN. Enables connecting by IP or through a load balancer where backends may present different CNs. The CA chain is always verified. Example: `^nu\.sitia\.airgap\.downstream-[0-9]+$`. Also accepted as `tcpTLSServerName` (legacy) |
| `tcpTLSCipherSuites` | `AIRGAP_UPSTREAM_TCP_TLS_CIPHER_SUITES` | | TLS version/cipher policy — see [Cipher Suites](#cipher-suites) below |

### Downstream TLS Settings

| Property | Env variable | Default | Description |
| -------- | ------------ | ------- | ----------- |
| `tcpTLSCertFile` | `AIRGAP_DOWNSTREAM_TCP_TLS_CERT_FILE` | | Server certificate PEM file (`.crt`). Setting this enables TLS on the TCP listener |
| `tcpTLSKeyFile` | `AIRGAP_DOWNSTREAM_TCP_TLS_KEY_FILE` | | Server private key PEM file |
| `tcpTLSKeyPasswordFile` | `AIRGAP_DOWNSTREAM_TCP_TLS_KEY_PASSWORD_FILE` | | Path to a file containing the passphrase for an encrypted `tcpTLSKeyFile` |
| `tcpTLSCAFile` | `AIRGAP_DOWNSTREAM_TCP_TLS_CA_FILE` | | CA certificate PEM file used to verify client certificates. Required when `tcpTLSClientAuth` is `allow` or `require` in CA mode |
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

### CA-Mode Certificate Rotation with SIGHUP

In CA mode, certificates and keys can be replaced on disk and reloaded without stopping either process. Send `SIGHUP` after the new files are in place. Pinned-mode identity/trust reload follows the separate procedure above.

**Downstream** (server — zero impact on live connections):

The server uses a `GetCertificate` callback that is invoked on every new TLS handshake. On SIGHUP the new certificate is loaded from disk and stored in an atomic pointer. Connections that are already established keep their existing TLS session and are never touched. New connections immediately use the new certificate.

**Upstream** (client — reconnect with retries):

On SIGHUP the TLS configuration is rebuilt from the new cert files and the current TCP connection is closed. The retry loop (`tcpRetryTimes=0`) detects the closed connection, redials with the new configuration, and retries failed sends. A successful TCP write is not an application delivery acknowledgment, so rotation does not guarantee no loss or exactly-once delivery; measure source/sink outcomes when validating delivery.

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
