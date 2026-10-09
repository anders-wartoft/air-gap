# Pinned TCP rotation protocol v1

Status: approved implementation contract; separate from legacy UDP symmetric
key exchange. Optional expiry and performance budgets are not covered.

## Configuration and compatibility

Upstream `tcpTLSRotationEnabled` defaults to `false`, preserving the existing
manual preprovisioned rotation workflow. When true, an identity change on
SIGHUP must obtain confirmation over old-key-authenticated pinned TLS before
activating the candidate. Downstream `tcpTLSAutomaticRotation` defaults to
`false`: preauthorized replacements can be confirmed, but new authorizations
require explicit opt-in. Both settings require pinned TCP and require restart
to change. Environment/CLI precedence matches other TCP TLS properties.

| Property | Environment |
| --- | --- |
| Upstream `tcpTLSRotationEnabled` | `AIRGAP_UPSTREAM_TCP_TLS_ROTATION_ENABLED` |
| Downstream `tcpTLSAutomaticRotation` | `AIRGAP_DOWNSTREAM_TCP_TLS_AUTOMATIC_ROTATION` |

CLI overrides are `--tcpTLSRotationEnabled=true|false` and
`--tcpTLSAutomaticRotation=true|false` on their respective binaries.
Each trusted directory must have only one downstream service writer;
sharing it between simultaneously running downstream processes is unsupported.

Optional `tcpTLSPinIdleSeconds` is an independent memory-only overlap policy
on both endpoints; see [Encryption.md](./Encryption.md#optional-in-memory-idle-pin-aging).
Connected pins and each peer's last successfully used pin remain protected.
Rotation's directory validation must not restore unrelated aged pins or
authorize an aged replacement. A verifier SIGHUP explicitly restores them.
Aging never changes public files, replay journals, or private recovery state,
and does not free disk-based trust capacity.

## Framing and proof

Reuse `TYPE_KEY_EXCHANGE` and existing header/checksum/length format. Reserved
message ID `PIN_TLS_V1` identifies this subtype; payload is strict version-1
JSON. No compression or fragmentation; maximum payload 8,192 bytes and peer
name 128 bytes. Unknown/duplicate fields, trailing JSON, invalid lengths,
unsupported versions/kinds, and multipart control frames reject explicitly.
Legacy `KEY_UPDATE#` is unchanged. Pinned control packets are rejected on UDP,
plaintext TCP, and CA TLS without invoking symmetric-key exchange.

Exchange: `begin` -> `challenge` -> `request` -> `accepted|rejected`.
Every message binds rotation ID, old and new fingerprints. Challenge also
supplies the locally authenticated peer identity and a random 32-byte nonce.
One challenge per connection; 30-second expiry, consumed on first request.
Responses return on the same TLS connection. A dedicated old-key TLS connection
avoids races between application traffic and control responses.

Rotation IDs are SHA-256 of domain-separated old/new SPKI fingerprints,
stable across retries. The request supplies new DER SPKI (Base64), peer,
nonce, and ECDSA P-256 ASN.1 signature (Base64). Signed bytes are Go JSON
encoding of the fixed wire struct in declared field order with signature
empty, prefixed with `airgap/pinned-rotation/v1\n`. The signature covers all
other fields. TLS authenticates the old key; signature proves the new key.
The short frame checksum is corruption detection, not authorization.

JSON field order for signing is `version`, `kind`, `id`, `old`, `new`,
`peer`, `nonce`, `public`, `reason`, `proof`; every field is included.
`version` is integer 1, all others are strings, and `proof` is empty when
signing. JSON string escaping uses `encoding/json.Marshal`, including its
HTML escaping. No independent remote permission field exists.
`id` is lowercase hex SHA-256 of
`airgap/pinned-rotation/id/v1\n<old>\n<new>`.
`old`/`new` use canonical `SHA256:` padded Base64 SPKI fingerprints.
`nonce`, `public`, and `proof` use padded standard Base64. `reason` is at
most 256 bytes. `begin` has empty peer/nonce/public/reason/proof; `challenge`
has peer/nonce; `request` has peer/nonce/public/proof; `accepted` has peer;
`rejected` has reason and, where authenticated, peer. All irrelevant fields
are empty and are checked, not silently ignored.

## Persistence and recovery

The trusted directory remains the authorization source. Owner-only hidden
`.pinned-rotations.json` stores bounded intent/commit records (at most 2,000;
one unfinished request per peer). It is never an alternate pin store.
The journal is a JSON array with record fields `ID`, `Peer`, `Old`, `New`,
and `State` (`intent` or `committed`); maximum file size is 2 MiB.
One old key can authorize only one distinct replacement; subsequent rotation
uses the replacement key. Identical retries obtain a fresh connection nonce.
Automatically installed public entries use `rotation-<rotationID>.json`.
Journal updates use a temporary file, file sync, atomic rename, and directory
sync. Public entries are fully written/synced under an ignored temporary name,
then published with an exclusive hard link (no overwrite), staging unlink,
and directory sync. An incomplete write never creates a recognized trust entry.

Order: validate proof/current old-key authorization/policy, persist intent,
install public entry, validate complete directory, persist committed record,
activate snapshot, acknowledge. Serialize commits and reloads. Existing
entries cannot be overwritten, and different-content ID reuse is rejected.
Retries obtain a fresh challenge/signature; semantic binding remains stable.
Retry with an existing record only confirms the current authorized replacement;
it never recreates a missing/deleted pin. Incomplete intent without an installed
pin requires explicit administrator reconciliation. This intentionally favors
fail-closed recovery over automatic reconstruction after an ambiguous removal.
Unrelated on-disk trust changes require SIGHUP before rotation; an exchange
does not implicitly activate staged administrative additions/removals.

Upstream syncs candidate identity files/directories and persists its old
certificate/private key, owner-only, in local `.pinned-client-rotation.json`
before requesting acceptance. This recovery file is never transmitted.
A lost/rejected response retains the old identity. Restart restores the old
identity from that file; SIGHUP retries the configured replacement with a fresh
challenge. A different configured replacement while recovery is pending fails
startup explicitly. After matching acceptance, upstream removes/syncs recovery
state before activating the new identity. A crash before removal safely retries;
a crash after removal starts the durably authorized new identity. Keep original
identity files for independent compromise recovery.
Disabling upstream exchange while private recovery state is pending is
rejected at startup; it cannot silently bypass the pending acknowledgment.
The upstream recovery JSON fields are `Version` (1), `New`, `Certificate`,
and `PrivateKey`; byte slices are Base64-encoded leaf DER and PKCS#8 DER.
It is bounded by the 16 KiB identity-file limit. Never copy the upstream
trusted directory wholesale as a public export: its hidden recovery file
can contain private material. Copy only intended public entries.

Record limits require administrator maintenance with services stopped; do not
silently discard replay records. No automatic old-key retirement. Compromise
of an authorized old key can authorize its replacement under opt-in policy.
No exactly-once/no-loss claim follows from TLS or an acknowledgment of trust.
