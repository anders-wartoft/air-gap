# Air-gap requirements

Source of truth for air-gap functional requirements. (Originated as
`tests/documents/Krav.xlsx`; this document now supersedes the spreadsheet.)

Each entry carries:

* **Status** — e.g. withdrawn, pending.
* **Implemented in** — SNAPSHOT version where the requirement was fulfilled.
* **Covered by** — testcase numbers from [TESTCASES.md](./TESTCASES.md).
* **Notes** — comments from the original Kommentar / Implementationskommentar
  columns, where useful.

If a cell is blank it means the spreadsheet left that cell blank.

---

## REQ-1 — Deliver to the existing receiver API

> Be able to send logs to the existing receiver API, so LIA can keep acting as
> a diode-side supplier into systems that still use the current receiver.

* Status: **Withdrawn** (JL, 2025-08-15).

## REQ-2 — Opt-in retransmission

> It must be configurable whether air-gap performs retransmissions, regardless
> of which receiver is in use.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-1

## REQ-3 — Configurable retransmission strategy

> It must be configurable **how** air-gap performs retransmissions. See
> REQ-3a..REQ-3d.

* Covered by: TC-1, TC-14

### REQ-3a — Automatic time intervals

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-1
* Notes: via `sendingThreads`.

### REQ-3b — Manual (file-driven) lists

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-14
* Notes: via the `create` / `resend` tools.

### REQ-3c — Number of retransmissions

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-1
* Notes: via `sendingThreads`.

### REQ-3d — Configurable time interval (one-shot job)

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-14
* Notes: via the `create` / `resend` tools.

## REQ-4 — Deduplicate on the receiving side

> On retransmission / restart, duplicates must be removed on the receiving
> side.

* Implemented in: 0.1.3-SNAPSHOT
* Covered by: TC-9, TC-10

## REQ-5 — Resume from last known log entry

> On restart, air-gap must resume from the last known log entry, not restart
> from the beginning.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-2
* Notes: Kafka stores client offsets; naming the client and the group
  deterministically is enough.

## REQ-6 — Plain-text traffic

> Air-gap must support plain-text traffic.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-3
* Notes: Testcase 3 covers Kafka→upstream and upstream→downstream (downstream
  is emulated with LogGenerator).

## REQ-7 — Encrypted traffic

> Air-gap must support encrypted traffic.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-4, TC-6

### REQ-7a — Encrypted upstream↔downstream

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-6
* Notes: Testcase 6 covers encryption across the diode.

### REQ-7b — Encrypted Kafka ↔ upstream/downstream

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-4
* Notes: Testcase 4 covers Kafka→upstream→downstream→Kafka with encryption
  to Kafka only (not across the diode).

## REQ-8 — Memory bounds

> Air-gap must be configurable for how much memory it is allowed to consume
> (max and min).

* Implemented in: 0.1.3-SNAPSHOT
* Covered by: TC-10
* Notes: Supported in the systemd templates (`MemoryMax`, `MemoryHigh`).

## REQ-9 — No log loss on restart

> Air-gap must not lose logs on restart.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-2

## REQ-10 — Clusterable

> Air-gap must be clusterable.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-1, TC-2, TC-3, TC-4, TC-6, TC-7, TC-8

## REQ-11 — Throughput target

> Air-gap must handle a traffic rate of **?** EPS.

* Covered by: TC-16
* Notes: Rate depends heavily on the environment. Aim for a stable version
  that works even virtualised. Measure with LogGenerator and watch the
  `logStatistics` output on both sides.

## REQ-12 — Linux support

> Air-gap must run on Linux-based operating systems.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: all
* Notes: All development is on macOS and Fedora 42.

## REQ-13 — Dedicated log path

> Air-gap must do its own internal logging to a dedicated path for
> troubleshooting and general operations info. Logs must not go to
> `/var/log/messages`.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-8

## REQ-14 — Deliver to a Kafka cluster

> Air-gap must support delivering to a Kafka cluster.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-4, TC-6, TC-7, TC-9

## REQ-15 — Install / upgrade / uninstall docs

> Air-gap must have documentation for installation / upgrade /
> uninstallation.

* Notes: Covered by the top-level `README.md` and
  `doc/Installation and Configuration.md`.

## REQ-16 — Send / receive / dedup logic docs

> Air-gap must have documentation for the send / receive / dedup logic.

* Notes: Covered by `README.md`, `Kafka-encryption.md`, `Monitoring.md`,
  `Resend.md`, `Deduplication.md`.

## REQ-17 — Run as systemd service

> Air-gap must be runnable as a service (via systemd).

* Implemented in: 0.1.3-SNAPSHOT
* Covered by: TC-10
* Notes: Service templates ship under `packaging/systemd/`.

## REQ-18 — Run as command-line binary

> Air-gap must be runnable as a binary (command line).

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-8

## REQ-19 — Large log entries

> Air-gap must handle large log entries so the full log entry is stored in
> Kafka.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-7
* Notes: Implemented by fragmentation/defragmentation in `src/protocol/`.

## REQ-20 — RPM install

> Air-gap must be installable from an RPM package.

## REQ-21 — RPM upgrade logic

> Air-gap must support upgrade via RPM.

* Notes: Package build uses NFPM (see `packaging/nfpm-*.yaml`).

## REQ-22 — Config file and environment variables

> Air-gap must be configurable both via config file and via environment
> variables.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-5
* Notes: 0.1.2-SNAPSHOT ships env-var overrides for upstream and downstream.
  Resend overrides tested later.

## REQ-23 — logrotate / SIGHUP

> Air-gap must support log-file rotation via `logrotate` and must resume
> writing to the configured file name again when it receives SIGHUP.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-8
* Notes: The deduplicator uses log4j2 via `log4j2.xml`.

## REQ-24 — Redundancy and load sharing

> Air-gap must be able to send with redundancy and/or load sharing over
> several physical channels.

* Implemented in: 0.1.4-SNAPSHOT
* Covered by: TC-11, TC-12

## REQ-25 — Upstream event-count logging

> Number of received and sent events must be logged at a configurable
> interval in upstream.

* Implemented in: 0.1.4-SNAPSHOT
* Covered by: TC-4

## REQ-26 — Downstream event-count logging

> Number of received and sent events must be logged at a configurable
> interval in downstream.

* Implemented in: 0.1.4-SNAPSHOT
* Covered by: TC-4

## REQ-27 — Dedup event-count logging

> Number of received and sent events must be logged at a configurable
> interval in dedup.

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-13

## REQ-28 — Dedup missing-entry logging

> Number of missing log entries must be logged from dedup at a configurable
> interval.

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-13
* Notes: Logged as `INFO [MISSING-REPORT] {json...}`.

## REQ-29 — Log levels

> Upstream and downstream must support log levels of at least debug, info,
> and error.

* Implemented in: 0.1.4-SNAPSHOT
* Covered by: TC-4

## REQ-30 — Compression

> Upstream and downstream must be able to compress / decompress log data
> independently of whether encryption is used across the diode. Compression
> must be selectable for all payloads over a configurable length (for
> example 1200, 0, 10000).

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-15

## REQ-31 — Dedup TLS to Kafka

> The deduplicator must be able to connect to Kafka with TLS and
> certificate-based authentication.

* Implemented in: 0.1.6-SNAPSHOT
* Covered by: TC-17

## REQ-32 — Encrypted key files

> Upstream, downstream, `create`, and `resend` must be able to use encrypted
> (password-protected) key files.

* Implemented in: 0.1.7-SNAPSHOT
* Covered by: TC-18

## REQ-33 — Regex input filter

> Upstream must be able to filter out logs using regular expressions.

* Implemented in: 0.1.8-SNAPSHOT
* Covered by: TC-19

## REQ-34 — Filter counters

> Upstream must log the number of filtered and unfiltered logs.

* Implemented in: 0.1.8-SNAPSHOT
* Covered by: TC-19

## REQ-35 — TCP transport

> Upstream and downstream must be configurable to use TCP instead of UDP.

* Implemented in: 0.1.8-SNAPSHOT
* Covered by: TC-20

## REQ-36 — Multi-cluster, same-topic fan-in

> It must be possible to send logs from several different clusters that share
> the same topic name through several upstreams to a single downstream and
> single dedup.

* Implemented in: 0.1.9-SNAPSHOT
* Covered by: TC-21

## REQ-37 — Downstream restart over TCP without loss

> It must be possible to restart downstream without losing logs when
> `transport=tcp`. Once downstream is up again, normal log transfer must
> resume.

* Implemented in: 0.1.9-SNAPSHOT
* Covered by: TC-22

## REQ-38 — Filtered events do not create gaps

> When events are filtered out by `inputFilterRules`, those lines must not be
> reported as missing by dedup.

* Implemented in: 0.1.10-SNAPSHOT
* Covered by: TC-23

## REQ-39 — Resend respects input filter rules

> When logs are retransmitted with `resend`, `inputFilterRules` must be
> applied, just as in upstream.

* Implemented in: 0.1.10-SNAPSHOT
* Covered by: TC-23

## REQ-40 — Fixed-size gap storage

> Gaps must be stored in Kafka with a fixed size so the events do not grow
> during runtime until they can no longer be stored and the application
> stops.

* Implemented in: 0.1.11-SNAPSHOT
* Covered by: TC-14

## REQ-41 — Plain-text status messages during encryption

> For encrypted upstream→downstream transfer, only status messages must be
> sent **without** encryption. Status messages may contain important
> information about the upstream (and possibly about why encryption is not
> working), so they may be sent in plain text.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24
* Notes: For TLS, status messages are also encrypted.

## REQ-42 — Enforce encryption on downstream

> Downstream must be configurable to only accept encrypted messages (apart
> from status) to prevent event injection. Downstream must also be
> configurable to accept both encrypted and unencrypted messages — but not
> both at the same time.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24

## REQ-43 — One-way and mutual authentication

> For encryption, both one-way (server) and mutual authentication must be
> supported.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24

## REQ-44 — SIGHUP certificate rotation

> Certificates and keys must be rotatable with SIGHUP without packet loss.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24

## REQ-45 — X.509 / crt-key format

> Certificates must follow the X.509 standard and be in `.crt` / `.key`
> format.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24
* Notes: See `README.md` and `Encryption.md`.

## REQ-46 — Shared certificate for TCP and UDP

> A TCP and a UDP sender/receiver must be able to use the same certificate.

## REQ-47 — TCP crypto profile

> For TCP:
>
> * encryption protocol must be TLS 1.3 (mTLS for mutual authentication);
> * encryption algorithms must be NIST-approved;
> * encryption must work independently of the number of senders.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24

## REQ-48 — UDP crypto profile

> For UDP:
>
> * encryption must work over a diode;
> * encryption must work independently of the number of senders (together
>   with `deliveryFilter`, `partitionOffset`, and similar);
> * the encryption algorithm must be well documented, with its properties
>   and non-properties clearly stated.

## REQ-49 — SO_RXQ_OVFL kernel drop counter

> `SO_RXQ_OVFL` is a socket flag (see `man socket(7)`) that counts how many
> packets Linux dropped (because downstream could not keep up). Include
> that counter in the statistics so we can tell air-gap-induced loss from
> network-induced loss.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-25
* Notes: `SO_RXQ_OVFL` has some overhead; it is configurable and should not
  be enabled in production when the system is near its throughput ceiling.
  See `doc/FAQ.md` and `doc/Monitoring.md`.

## REQ-50 — Production configuration warnings

> Upstream, downstream, create, resend, and the Java deduplicator must warn
> at startup and again as their final application log events on orderly
> shutdown when effective configuration values indicate test settings or
> production risks.

* Status: **Implemented**, with unit and subprocess lifecycle acceptance tests.
* Covered by: TC-26
* Scope: The four named Go applications and Java dedup; `gaps` is excluded.
* Notes:
  * Evaluate the effective configuration after defaults, file settings,
    environment variables, and command-line overrides have been applied.
  * A dedicated, reusable `WarnProductionConfiguration(phase)` method in each
    Go configuration type, and an equivalent Java method, must be called as
    the last step of successful configuration validation and after shutdown
    cleanup. Use the same warning rules at both points.
  * Emit one WARN per affected setting, including its name, effective value,
    risk, and lifecycle phase. Multiple risks must not hide each other.
    Warnings must remain visible at ERROR/FATAL log levels, without changing
    the configured log level or exposing keys, passwords, or message payloads.
  * Warnings are advisory: do not reject valid configuration, change settings,
    interrupt startup, alter delivery, or change exit codes.
  * Shutdown includes SIGINT/SIGTERM and normal batch completion for
    create/resend, and normal/error-driven termination of Java dedup.
    Emit the final warning block once per process, after workers, clients,
    statistics, and application shutdown logs have finished. SIGKILL, power
    loss, and invalid configuration rejected before startup are excluded.
  * Thresholds in TC-26 are provisional heuristics, not production guarantees.
    Operators must size dedup history for per-partition throughput multiplied
    by maximum resend/out-of-order delay, with headroom and memory monitoring.
    A below-range offset can be delivered without deduplication; this does
    not imply that every packet bypasses deduplication.

## REQ-51 — Pinned public-key TLS and key lifecycle

> Upstream and downstream must support explicit CA/certificate-based or
> pinned-public-key authentication for TCP TLS. Pinned mode must support
> manual provisioning, multiple authorized keys per peer, restart-free
> rotation, and optional authenticated automatic client-key rotation,
> without weakening peer authentication.

* Status: **Partially implemented / first acceptance batch passing**.
  Generation, startup trust loading, mutual pinned TLS, and explicit mode
  validation, atomic pinned SIGHUP identity/trust reload, manual rotation,
  and immediate revocation are implemented. The first and second executable
  batches pass. An opt-in acknowledged client-rotation protocol, durable
  replay records, restart recovery, and functional 2,000-pin baseline are
  implemented; exhaustive fault/concurrency/logging acceptance, optional
  expiry, and performance budgets remain pending.
* Covered by: TC-27, subcases TC-27.01 through TC-27.16.
* Scope: Upstream and downstream TCP transport. Kafka TLS, UDP encryption,
  create/resend, Noise, and diode key exchange are outside this requirement.
* Terminology:
  * A **key-set** contains a private key, an exported public key, and a
    fingerprint. A local X.509 certificate wraps the key for Go TLS; it is
    not a CA-issued authorization in pinned mode.
  * A **pin** authorizes the exact public key, not the whole certificate.
    Its fingerprint is SHA-256 of DER-encoded SubjectPublicKeyInfo (SPKI),
    displayed as `SHA256:` followed by padded standard Base64.
  * A **peer identity** is a locally authorized identity with one or more
    pins. The certificate CN, filename, and remotely claimed identity do
    not independently grant authorization.
  * **Client** means upstream; **server** means downstream. Client and server
    trust stores have separate roles.

### REQ-51.01 — Explicit authentication mode and compatibility

Upstream and downstream must explicitly select CA-based or pinned mode.
Existing CA-based configuration remains compatible and its default is
unchanged. Pinned mode uses TLS 1.3 with mutual public-key authentication:
downstream authorizes upstream's key and upstream authorizes downstream's
key. It requires no CA or CN regex. An unknown mode, conflicting trust
settings, missing required trust store, or incompatible endpoint mode must
fail explicitly; no fallback to plaintext, unauthenticated TLS, or the
other trust mode is allowed. Pinned mode must not permit optional client
authentication.

### REQ-51.02 — Key-set generation command

A documented command must generate one or several independent key-sets for
upstream or downstream without starting the transfer service or contacting
a CA. Each set has a distinct key generated with cryptographically secure
randomness, its public export, fingerprint, and TLS certificate wrapper.
The command must verify consistency before reporting success, never
overwrite existing files silently, and exit nonzero on failure. Private
files must be owner-only on supported Unix platforms; private keys and
passwords must never appear in command output or logs. Supported algorithms
must follow the approved crypto profile; a FIPS-compliance claim requires
verification of the deployed module and mode, not just an algorithm name.

### REQ-51.03 — Manual provisioning and independent verification

Administrators must be able to copy the upstream public key and fingerprint
to downstream, and downstream's public key and fingerprint to upstream,
using ordinary OS file operations. Private keys stay on their owning
endpoint. Each endpoint must recompute the fingerprint and reject any
supplied fingerprint mismatch before accepting an entry. Both endpoints
must expose the same canonical fingerprint for optional verification over
an independent authenticated voice channel. Local fingerprint consistency
does not authenticate the initial copy: replacing both public key and
fingerprint must still be addressed by trusted provisioning or independent
verification. Trust-on-first-use is not part of this requirement.

### REQ-51.04 — Multiple peers, keys, and roles

Downstream must authorize multiple upstream identities, each with multiple
public-key/fingerprint pairs. Upstream must likewise support multiple
authorized downstream keys, including old/new keys for server rotation,
and multiple locally configured client key-sets with one explicitly active
identity key. Keys for the same peer inherit the same authorization.
Entries must not authorize the opposite role or unrelated identities.
Duplicate identical entries must not create duplicate authorization;
conflicting identity/role assignments for a key must be rejected.

### REQ-51.05 — OS file operations and explicit activation

Adding, replacing, or removing trust entries must be possible without a
management API. Administrators stage complete files under ignored temporary
names and atomically rename them into the trusted directory. File changes
alone do not activate new trust; startup or successful SIGHUP does.
Document the recognized files, staging convention, fingerprint format,
identity binding, permissions, and local private-key configuration.

### REQ-51.06 — Atomic configuration reload on SIGHUP

Both applications must re-read their configuration file on SIGHUP, then
reapply the existing environment/CLI precedence. A reload must prepare and
validate the complete TLS identity and trust-store snapshot before
atomically activating it. Concurrent handshakes must not see partial
updates. On invalid or unreadable configuration, keys, or trust entries,
retain the last valid snapshot and emit an explicit reload failure.
At startup, the same failures prevent startup. An intentionally empty
peer trust directory is valid and denies all peers; it differs from a
missing, unreadable, or malformed directory.

Authentication-mode changes require a restart and must be rejected on
SIGHUP without changing active state. Other settings outside the supported
reload set must likewise be reported rather than silently ignored or
partially applied. The implementation specification must enumerate that set.

The approved lifecycle test contract permits pinned-mode reload of only
`tcpTLSCertFile`, `tcpTLSKeyFile`, and `tcpTLSTrustedKeysDir`. Any other
resolved-setting change requires restart and must be rejected atomically.
Startup environment/CLI overrides are reapplied after reading the original
file. Manual preprovisioned rotation is tested in this batch; replacement
announcements remain a separate protocol batch.

### REQ-51.07 — TLS proof of possession and exact pin matching

Authentication must require TLS proof of possession of the private key
corresponding to an authorized public key. A public key or fingerprint
alone cannot authenticate a connection. Pin matching uses the leaf
certificate's SPKI, not its CN, issuer, filename, or whole-certificate hash.
In pinned mode, certificate chain, hostname, and certificate validity dates
do not establish authorization; replacing a certificate wrapper with one
containing the same authorized key must preserve trust. TLS protocol and
key-strength checks still apply. Optional trust-entry expiry, if configured,
must be enforced independently of the wrapper certificate.

Optional in-memory aging is separate from expiry: `tcpTLSPinIdleSeconds=0`
disables it; positive values remove idle overlapping peer pins after that
many seconds. Active sessions protect their pins; after the last session
closes, a new idle period starts. Always retain each peer's last successfully
used key (deterministic fingerprint ordering if no use/tie). Startup and
successful SIGHUP restore disk-backed pins and reset idle periods. Purging
must not alter disk, local private identities, or replay/recovery records;
automatic rotation must not implicitly restore other aged pins. Aged keys
fail full/resumed authorization until explicit restoration. Policy changes
require restart. This does not provide permanent revocation or time-based
trust-entry expiry.

### REQ-51.08 — Manual overlapping rotation without restart

An administrator must be able to add a replacement peer key, reload the
verifier, switch the owner's active key through configuration and SIGHUP,
confirm successful authentication with the new key, and finally remove the
old key and reload. Both authorized keys must work during overlap. Switching
an active identity must establish a connection with the replacement key;
existing sessions alone are not evidence of successful rotation.

This requirement does not introduce an exactly-once delivery guarantee:
TCP writes are not application delivery acknowledgments. Rotation must not
intentionally discard queued work, and delivery tests must measure loss
and duplicates rather than assume them from a successful handshake.

### REQ-51.09 — Client replacement announcement and authorization

When configuration reload selects a new upstream key, upstream must retain
access to the current identity long enough to send a structured key-exchange
request over TLS authenticated with the currently trusted old identity.
The request includes the new public key and fingerprint, protocol version,
rotation identifier, identity binding, freshness information, and proof of
possession of the new private key bound to the rotation request.
It must not contain a private key. Logs are audit output, never a source
of automatic authorization.

The approved protocol direction reuses the existing application message
framing and `TYPE_KEY_EXCHANGE`. Legacy `KEY_UPDATE#` symmetric-key exchange
remains unchanged. Pinned rotation uses a distinctly identified, versioned
payload subtype, with request, acceptance, and rejection messages on the
same mutually authenticated pinned-TLS TCP connection. Requests identify
old and new fingerprints; the new-key signature binds the complete request.
The authenticated old key determines the local peer identity, not a
remotely claimed identity field. The existing short frame checksum is not
an authentication mechanism.

Pinned-rotation messages must be rejected on UDP, plaintext TCP, and CA-mode
connections, without entering the legacy symmetric-key handler or changing
trust. Server-key automatic rotation is not supported. The concrete approved implementation contract is documented in
[PinnedRotation.md](../../doc/PinnedRotation.md): versioned bounded JSON,
connection-bound challenges, deterministic signed requests, durable records,
and fail-closed reconciliation of ambiguous incomplete intents. Upstream
exchange is opt-in (`tcpTLSRotationEnabled`, default false) to preserve the
existing manual workflow; downstream automatic authorization is separately
opt-in (`tcpTLSAutomaticRotation`, default false). Changing either policy
requires restart. Exhaustive acceptance is not implied by this implementation.

If the new key was manually preauthorized, downstream must confirm it.
Otherwise automatic authorization is permitted only under REQ-51.10.
If the old identity is unavailable/untrusted, or the request is rejected,
do not silently switch to the untrusted key: report the failure and retain
the active configuration. Initial pairing and recovery from compromise
require independent provisioning. Server-key rotation remains manual in
this requirement.

### REQ-51.10 — Optional automatic rotation policy

Automatic client-key authorization must be disabled by default and explicitly
enabled by downstream policy. It may authorize replacement keys only for
the already authenticated client's identity and existing permissions.
An untrusted peer, arbitrary identity claim, audit log, or new key alone
must never bootstrap trust. With the policy disabled, an unprovisioned
replacement must receive an explicit rejection and must not change trust.
The documentation must explain that compromise of an authorized old key
can also authorize a replacement under this policy; it is not independent
compromise recovery.

### REQ-51.11 — Durable exchange acknowledgment and crash recovery

Before reporting acceptance, downstream must durably and atomically install
the new public trust entry in the administrator-visible trusted directory,
then activate the validated snapshot. The directory remains the source of
truth; there must be no hidden permanent authorization bypassing it.
Upstream must persist its replacement private identity before requesting
rotation and switch only after an authenticated acceptance acknowledgment.
Both sides must retain the old key during overlap. Persistence failure
must not result in a success response or premature switch.

After interruption or restart, retrying the same request must be idempotent
and produce no duplicate entries or permissions. A lost acknowledgment must
not strand the client. Replays or concurrent requests must not restore
revoked keys, overwrite a different pending rotation, or change identities.
Replay/rotation state needed to enforce this must survive restart.
Automatic retirement of old keys is not required; administrator removal
and SIGHUP provide retirement.

### REQ-51.12 — Purge, immediate revocation, and TLS resumption

On successful downstream SIGHUP, pins absent from the trusted directory must
be removed from active authorization, including automatically installed
entries removed by an administrator. Immediately close connections
authenticated with removed keys. New, in-progress, and resumed handshakes
must not retain removed authorization. Apply equivalent server-pin removal
behavior on upstream. A connection authenticated with a retained overlapping
key must remain authorized.

A revoked old key must not authorize another rotation or reintroduce
itself. Legitimate reauthorization requires explicit administrator
provisioning. Reload must report revocation-related disconnects clearly.

### REQ-51.13 — Local trust protection and bounded input

Trusted directories, configuration, and private identities must not be
writable by unauthorized users. Reject insecure permissions on supported
Unix platforms, private-key material in public trust entries, unsupported
key algorithms, malformed entries, and path/symlink escapes outside the
configured trust directory. File discovery and network key-exchange parsing
must enforce documented size, entry-count, and per-peer rotation limits.
At every limit, reject excess input explicitly without partial activation.
Temporary files are ignored, not interpreted as trust entries.

### REQ-51.14 — Observability without disclosure

Generation, startup, reload, authentication rejection, rotation acceptance/
rejection, and revocation must provide actionable logs or command output.
Include peer identity where known, role, public fingerprints, rotation
identifier where applicable, and the reason/result. Successful reloads
report active peer/key counts and added/removed fingerprints. Never log
private keys, passphrases, session secrets, or transfer payloads as part
of key lifecycle events. Unknown presented fingerprints must not be
misrepresented as authenticated identities.

### REQ-51.15 — Scale and concurrency acceptance

Use a provisional acceptance baseline of 1,000 client identities with two
authorized keys each (2,000 pins). Pin lookup must use indexed key identity,
not reparsing/scanning every trust file per handshake. Reload and rotation
must remain safe during concurrent authentication, key exchange, and
transfer. Test the complete baseline and configured bounds without treating
it as a universal production capacity guarantee. A latency/memory budget
must be agreed before implementation performance acceptance.

### REQ-51.16 — Scope, compatibility, and design gate

Existing CA-based TCP tests and UDP/Kafka behavior must remain unchanged.
Certificate wrappers keep REQ-45 applicable; trust does not require an
issuer in pinned mode. REQ-46's TCP/UDP certificate reuse is not extended
to raw pinned-key or automatic-rotation support by this requirement.

Before executable tests or implementation, approve exact configuration
names, command/file formats, supported algorithm profile, reloadable
settings, network message framing/versioning, freshness/replay rules,
resource limits, and scale budgets. These are design decisions, not
already implemented interfaces. No production FIPS or post-quantum claim
may be inferred solely from choosing TLS or a Go default.
