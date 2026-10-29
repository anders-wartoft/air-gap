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

### REQ-8a — Upstream large-message memory safety

> Upstream must bound Kafka prefetch memory and pause record admission before
> the effective process memory limit is exhausted.

* Implemented in: 0.1.14-SNAPSHOT
* Covered by: TC-30, TC-31

For a Kafka-backed upstream:

1. `kafkaMaxMessageBytes` sets the largest Kafka record upstream accepts and
   is applied to Sarama's maximum fetch size. A larger record is rejected
   explicitly instead of being admitted with unbounded memory use.
2. `kafkaChannelBufferSize` bounds the number of fetched records Sarama may
   buffer per consumer channel. `kafkaFetchDefaultBytes` bounds the normal
   fetch target.
3. Upstream determines the effective memory ceiling from cgroup v2, then
   cgroup v1, and finally process RSS plus system memory. A nonzero
   `memoryLimitBytes` may lower, but never raise, the detected ceiling.
4. At `memoryHighWatermarkPercent`, upstream pauses before removing another
   record from Sarama's bounded channel. It resumes only after usage reaches
   `memoryLowWatermarkPercent`, preventing rapid pause/resume oscillation.
5. The wait is context-aware so SIGTERM is not blocked by memory pressure.
6. `goMemoryLimitPercent` applies a Go runtime soft heap limit relative to the
   same effective ceiling. It supplements rather than replaces the cgroup and
   fetch bounds because the Go heap is not the complete process working set.
7. Throttle and resume transitions are logged with current bytes, limit
   bytes, source, both watermarks, and `airgap.memory.throttled`. The severity
   and text of both transitions are configurable through
   `memoryHighWatermarkLog` and `memoryLowWatermarkLog` as described in
   REQ-8c. The attributes do not depend on the configured text.

The deterministic envelope is the fixed process working set plus, for every
configured sending thread, at most `kafkaChannelBufferSize` records of up to
`kafkaMaxMessageBytes`, Sarama fetch/decode overhead, and one record being
transformed and fragmented. Operators must choose a systemd/cgroup limit with
headroom for those copies. Throttling controls sustained pressure; it cannot
make a single over-limit record safe, which is why the hard record-size limit
is mandatory.

### REQ-8b — Airgap Lost Event guard

> An event that upstream cannot deliver must neither disappear silently nor
> block the partition.

* Implemented in: 0.1.14-SNAPSHOT
* Covered by: TC-32, TC-33

1. `maxEventBytes` sets the largest Kafka record upstream forwards. A record
   above the ceiling is not transmitted as-is. `0` disables the ceiling.
2. When `lostEventGuard` is true (the default), upstream transmits a guard
   payload in place of the record. The guard carries the *same* message id, so
   downstream observes a continuous offset sequence and the gap detector does
   not request a resend of an event that can never be delivered.
3. The guard payload is rendered from `airgapLostEventMessage` using the rules
   of REQ-8c. On top of every configuration property it may use
   `{topic_name}`, `{partition}`, `{row}`, `{reason}`, `{eventBytes}` and
   `{lostEventFile}`. The default is
   `AIRGAP LOST EVENT: Event {topic_name} {partition}:{row} couldn't be delivered due to {reason}. The message can be found in its entirety on upstream machine {upstream_id}, in {lostEventDirectory}. Max message length is {kafkaMaxMessageBytes}.`
   The `AIRGAP LOST EVENT` marker is what downstream and operators search for
   to find every event that was lost in transfer.
4. `airgapLostEventMessageFormat` selects `text` (the default) or `json`. In
   JSON mode the configured template must be one JSON object and placeholders
   must occur inside JSON string values. Upstream validates the object before
   startup and re-serializes it after substitution, so quotes, backslashes and
   control characters in values cannot produce invalid JSON.
5. A record whose encryption fails also produces a guard. Upstream must never
   fall back to transmitting an unencrypted payload when encryption is enabled,
   and must never transmit an empty ciphertext.
6. When `lostEventDirectory` is set (default `/airgap/lost_events/`), the
   original payload is written there as one file per lost event before the
   guard is sent. A failed write is logged but never prevents delivery of the
   guard. When it is empty, the guard states that no directory is configured
   rather than pointing at one.
7. Every lost event produces the configurable `lostEvent` record, which always
   carries `eventName=message.lost`, the message id, topic, partition, offset,
   original size, reason, whether a guard was sent, and the stored file path
   when one was written.
8. Lost events are counted and reported as `lost_events` and
   `total_lost_events` in the periodic statistics record.
9. `lostEventGuard=false` restores the previous behaviour: the record is
   discarded and only the log record remains.
10. Before writing a new retained payload, upstream removes regular files older
    than `lostEventDirectoryDeleteAfterDays` (default 60), then compresses
    non-zip regular files older than `lostEventDirectoryCompressAfterDays`
    (default 7). `0` disables the corresponding stage.
11. Compression is streamed rather than loading the retained payload into
    memory. The resulting archive and its entry preserve the source mtime.
    Deletion runs before compression, so an already-expired source cannot gain
    a fresh retention period by first becoming an archive.
12. If the filesystem still lacks room for the new payload and write margin,
    upstream removes remaining regular files oldest first until the write fits
    or no candidate remains. A free-space measurement failure must not trigger
    emergency deletion.
13. Every successful Go removal of a retained content file emits the
    configurable `lostMessageTruncatedMessage` locally and as a protocol
    `TYPE_STATUS` message with id `STATUS`. Downstream therefore routes it to
    `internalTopic`, not to the original event topic. The record carries
    `eventName=message.lost.truncated`, file name, original mtime, size, current
    free bytes, and action (`compressed`, `deleted`, `evicted`, or
    `write_failed`). Its default body is
    `AIRGAP LOST MESSAGE TRUNCATED: The message file {fileName} created on {fileDate} was truncated by upstream due to lack of disc space. Current disc free space is {diskFreeBytes} bytes and the truncated file was {fileSize} bytes.`
14. `lostMessageTruncatedMessageFormat` selects `text` (default) or `json`.
    JSON uses the same validated and escaping-safe object rendering as the ALE
    guard. `lostMessageTruncatedMessage=NONE` disables the configurable local
    record and status message.

Records larger than `kafkaMaxMessageBytes` are skipped by the Kafka client
before upstream sees them and therefore cannot be replaced by a guard.
`maxEventBytes` must be configured at or below `kafkaMaxMessageBytes`;
upstream warns at startup when it is not.

### REQ-8c — Configurable Airgap events

> An operator must be able to decide the severity and the wording of the events
> that report what upstream is doing, without a code change.

* Implemented in: 0.1.14-SNAPSHOT
* Covered by: TC-30, TC-32

1. A configurable event is a single property holding both severity and text.
   The value is either `NONE`, which means no record is written, or
   `LEVEL:message`, where the level is the text before the first colon and the
   message is everything after it.
2. Legal levels are `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR` and `FATAL`,
   matched case-insensitively.
3. A value whose prefix is not a legal level is used as the message in its
   entirety, at the event's default level, so a message may contain colons
   without being quoted. Upstream warns at startup when such a prefix looks
   like a misspelled level.
4. Every configuration property is available as a `{placeholder}` in every
   message. Placeholder names are matched ignoring case and underscores, so
   `{lostEventDirectory}` and `{lost_event_directory}` resolve to the same
   value regardless of the notation used in the configuration file.
5. Each event may add context placeholders, which take precedence over the
   configuration for the duration of that event.
6. An unknown placeholder is left exactly as written, so a typo is visible in
   the output instead of rendering an empty field.
7. Every field of the configuration resolves to a placeholder value, so a
   placeholder is never silently unavailable. The fields that do not hold a
   configured value resolve to a description of their state: the symmetric keys
   render as `(redacted)` or `(none)`, the public key as its SHA-256
   fingerprint or `(none)`, and the compiled filters as `(set)` or `(none)`.
   Symmetric key material must never be rendered, because a message is written
   to the log and, for a lost event, delivered downstream.
8. The OTel attributes of an event do not depend on the configured text, so
   machine-readable processing is unaffected by wording and severity changes.
9. The configurable events are `memoryHighWatermarkLog` (default `WARN`),
   `memoryLowWatermarkLog` (default `NONE`) and `lostEvent` (default `INFO`).
   All of them can be overridden with environment variables and command-line
   arguments like any other property.

### REQ-8d — Stable Kafka subscriptions during slow processing

> Expected upstream processing delays must not make Sarama abandon and recreate
> an otherwise healthy Kafka partition subscription.

* Implemented in: 0.1.14-SNAPSHOT
* Covered by: TC-34

1. Upstream applies `kafkaMaxProcessingTimeMs` to Sarama
   `Consumer.MaxProcessingTime`. The default is 30000 ms.
2. The value must be a positive integer and can be set through the property
   file, `AIRGAP_UPSTREAM_KAFKA_MAX_PROCESSING_TIME_MS`, or
   `--kafkaMaxProcessingTimeMs`.
3. The configured interval must cover the longest expected synchronous
   processing time for one record, including rate limiting, memory
   backpressure, compression, encryption, fragmentation, transport writes and
   bounded retries.
4. Under processing delays shorter than the configured interval, upstream must
   not log `abandoned subscription ... because consuming was taking too long`,
   must reach consumer lag zero, and must not duplicate or lose records.
5. Increasing the interval does not claim to increase throughput. Sustained
   overload remains observable as Kafka consumer lag.

### REQ-8e — Log rotation releases replaced file descriptors

> Reopening the log after logrotate must release the descriptor of the rotated
> file, so that removing or compressing it frees the disk space immediately.

* Implemented in: 0.1.15-SNAPSHOT
* Covered by: TC-35

1. On SIGHUP, upstream, downstream and resend reopen `logFileName` and close
   the descriptor the reopen replaced. The same applies when a start-up path
   configures the log file more than once.
2. After a reload, the process must hold exactly one descriptor for the active
   log file and none for any rotated file, including removed files that the
   kernel reports as `(deleted)`.
3. The replacement must not drop records. The new file receives every record
   written after the reload, and the rotated file receives none.
4. Kafka client records written through the standard logger must follow the
   reopened file rather than the descriptor that was active when the Kafka
   connection was established.
5. A failure to close the replaced descriptor must be reported as an error
   from the reopen, not silently ignored.

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

## REQ-52 — Optional OpenTelemetry log format

> Upstream, downstream, dedup, create, and resend must optionally write their
> internal application logs as OpenTelemetry Protocol JSON (OTLP/JSON), while
> retaining the current plain-text logging as the default.

* Status: **Implemented**.
* Implemented in: 0.1.14-SNAPSHOT
* Covered by: Go unit tests and TC-28
* Scope: Upstream, downstream, dedup, create, and resend. Kafka message
  payloads transferred through air-gap are not application logs and are
  outside this requirement. The `gaps` utility is outside this acceptance
  batch.

### REQ-52.01 — Opt-in format and backwards-compatible default

The current plain-text format must remain the default when no log-format option
is configured. Operators must be able to opt in to OTLP/JSON without changing
the application's delivery behavior, log level, or log destination. Existing
plain-text configurations must continue to work without modification.

The Go applications select the format with `logFormat=text|otel` or their
application-specific `AIRGAP_<APPLICATION>_LOG_FORMAT` environment variable.
The environment variable overrides the configuration file. Dedup selects an
OTLP-capable Log4j configuration; its existing PatternLayout configuration
remains the default.

An empty value is equivalent to the default `text` value. The aliases `otlp`
and `otlp/json` may be accepted for operator convenience, but emitted records
must still meet the same OTLP/JSON contract. Any other value must fail
explicitly; it must not silently fall back to text or emit a success-shaped
record.

### REQ-52.02 — One complete OTLP/JSON envelope per line

In OTel mode, every application log event emitted after the format has been
activated must be one complete UTF-8 JSON object on one physical output line.
Embedded quotes, backslashes, control characters, Unicode, and newlines in a
message must be JSON-escaped and must not split or corrupt the record.

Each line must use the OTLP JSON representation and contain exactly one
`resourceLogs` entry, one `scopeLogs` entry, and one `logRecords` entry. The
record must be independently parseable; consumers must not need records before
or after it to decode the envelope.

Bootstrap diagnostics emitted before an application has read and activated its
logging configuration may retain the default text format. Container-entrypoint,
JVM launcher, and Log4j status diagnostics are not application log events.

### REQ-52.03 — Resource and instrumentation identity

Every OTLP/JSON envelope must include these non-empty resource attributes:

* `service.name` — the emitting application (`upstream`, `downstream`, `dedup`,
  `create`, or `resend`);
* `service.instance.id` — the configured application instance ID when one is
  available, otherwise a stable hostname-based fallback for the process.

Every envelope must include a non-empty instrumentation scope name. Identity
values must be JSON-escaped and must not be inferred from the log message body.

### REQ-52.04 — Timestamp, severity, and body mapping

Each log record must include:

* `timeUnixNano` and `observedTimeUnixNano` as decimal-string Unix timestamps
  in nanoseconds;
* `severityText` preserving the application's effective level;
* the corresponding OpenTelemetry base `severityNumber`: TRACE=1, DEBUG=5,
  INFO=9, WARN=13, ERROR=17, FATAL=21;
* `body.stringValue` containing the complete formatted application message.

The two timestamps may be equal when the event is observed locally. They must
be positive and must not move an event into a different process run. Formatting
arguments must be resolved before the body is encoded.

### REQ-52.05 — Attributes and OTLP type fidelity

Application metadata must be represented as OTLP key/value attributes, not by
adding non-standard siblings to the OTLP envelope. String, integer, and boolean
attributes must use `stringValue`, decimal-string `intValue`, and `boolValue`
respectively, following protobuf JSON mapping for 64-bit integers.

Attribute keys within one record must be unique. Enabling OTel format alone
must not invent integrity or signature attributes. Optional hashing or chaining
may add documented `airgap.log.*` attributes, but signing and signature
verification are explicitly deferred to a later requirement.

### REQ-52.06 — Destination and transport boundary

OTel mode changes only the record encoding. Console output must remain console
output and a configured log file must remain the destination. Air-gap must not
open an OTLP network connection, require an OpenTelemetry Collector, buffer
logs for export, or change application exit behavior merely because OTel mode
is enabled. Once a record has been written to the selected stream or file,
shipping and retention are outside air-gap's responsibility.

Write or encoding failures must be reported explicitly through the existing
logging/error path. They must not be represented as successful OTLP events and
must not recursively loop forever.

### REQ-52.07 — Filtering, ordering, and concurrency

Existing log-level filtering must apply before encoding: filtered events must
not produce empty envelopes. Concurrent writers must produce complete,
non-interleaved JSON lines, with no lost or duplicated application event caused
by the formatter. The output order must be the order in which the logging
framework accepts records for writing.

Third-party library messages routed through the application's configured logger
must use the selected format. Direct output written outside the application
logging framework is outside this requirement and must not be mistaken for an
OTLP application record.

### REQ-52.08 — End-to-end acceptance

TC-28 must run the real packaged binaries and:

1. send exactly 100 Kafka events through upstream, downstream, and dedup;
2. deliberately filter part of the first transfer so the downstream result is
   incomplete;
3. use `create` to export the detected gaps, copy the resend file across the
   simulated boundary to the upstream side, and run `resend`;
4. verify that the downstream clean Kafka topic receives every recoverable
   event with no duplicates and that dedup reports the detected gaps closed;
5. inspect internal application logs from upstream, downstream, dedup, create,
   and resend and validate every REQ-52.02 through REQ-52.05 field and type,
   and the semantic attributes required by REQ-52.09;
6. prove the default separately in Go unit tests: without opt-in, the existing
   text format is retained and no OTLP envelope is emitted.

The resend-file copy must be an explicit host-side or Docker copy step; create
and resend must not share the same file path in a way that bypasses the
operational transfer the tools are intended to support.

### REQ-52.09 — Semantic attributes

A log record has two audiences. The body is written for a human. EventName and
attributes are written for a machine, and a consumer must be able to classify,
filter, and alert on a record without parsing the body.

Every application log record emitted in OTel format must carry:

* EventName — a non-empty first-class OpenTelemetry LogRecord field, serialized
  as `eventName` in OTLP/JSON. It identifies what kind of event the record
  describes and must not be duplicated in the attributes array. Names are
  hierarchical and come from a closed source catalogue.

A record must additionally carry, where applicable:

* `service.version` — the build version of the emitting binary, as a Resource
  attribute on every record. It must not be duplicated as a LogRecord
  attribute.
* `error.type` — required for a failed operation and absent on success. It must
  be predictable and low-cardinality; an arbitrary error message is not a type.
  The deprecated `error.message` and project-specific `event.result` attributes
  must not be emitted.
* Actual exceptions use an EventName ending in `.exception` and carry
  `exception.type` or `exception.message` plus `exception.stacktrace` when
  available. Ordinary Go errors are not automatically exception events.
  Recovery is represented by a distinct EventName such as
  `kafka.connection.restored`, not a success boolean.
* Domain attributes that make the event self-describing, such as the broker,
  topic, partition, offset, file path, configured setting, or failure type the
  message refers to. A value that is interesting enough to interpolate into the
  body is interesting enough to expose as an attribute. Where the OpenTelemetry
  semantic conventions define a name for such a value, that name must be used;
  otherwise the `airgap.` namespace applies.

Semantic attributes must not be emitted in text format. Text output is the
backwards-compatible default required by REQ-52.01, and it must remain
byte-identical whether or not call sites supply attributes. The integrity
attributes of REQ-52.05 are unaffected and keep their existing text rendering.

A call site must not be able to set an attribute in the reserved `airgap.log.*`
namespace. Those keys are produced by the integrity machinery, and an
application record must not be able to assert a hash, sequence number, or chain
value that the framework did not compute. Such attributes must be discarded
before the record is encoded, as must attributes with an empty key, and
duplicate keys must be reduced to their first occurrence so that the uniqueness
rule of REQ-52.05 always holds.

Kafka attributes must follow the targeted OpenTelemetry messaging conventions,
including `messaging.consumer.group.name`,
`messaging.destination.partition.id`, `messaging.destination.name`, and
`messaging.kafka.offset`. This requirement targets OpenTelemetry semantic
conventions 1.38.0; conventions marked Development may be migrated explicitly
when the project updates that target.

### REQ-52.10 — Optional signed log checkpoints

When `log-signing-cert`, `log-signing-key`, and a signer identity are
configured together with a hash chain, completed anchor records must carry an
Ed25519 signature over a canonical checkpoint statement. The statement must
bind the hash algorithm, anchor range, preceding and final chain values,
service identity, process start, run/segment/window identities, signer ID, and
public-key ID.

Every Version 1 chain step must bind the record's `timeUnixNano`,
`severityNumber`, and `severityText` in addition to its sequence number and
body hash. Changing the displayed event time or either severity representation
must therefore invalidate that record's chain value and every signed
checkpoint covering it. The body hash remains a digest of the UTF-8 body alone.
The byte encoding must be specified normatively as fixed-width big-endian
integers and length-framed UTF-8 strings, with an explicit table of protected
and unprotected OpenTelemetry fields.

The signing certificate must be dedicated to log signing, use an Ed25519 key,
and allow `digitalSignature`. The public-key ID must be the SHA-256 fingerprint
of the DER SubjectPublicKeyInfo. A verifier must resolve that ID through an
independently trusted key registry; a public key supplied only by the log
record is not trusted.

Signing is optional and must not change the text default or unsigned OTLP
output. A signing failure must be surfaced in the log output and must never be
represented as a valid signature.

A deterministic, public Version 1 test vector must publish every chain and
checkpoint input byte, intermediate digest, SubjectPublicKeyInfo key ID, and
Ed25519 signature. Independent Go producer and Python verifier tests must
reproduce it byte-for-byte. Test-only private material must be conspicuously
identified and must never be usable as a production default.

The format documentation must identify RFC 5848 as evaluated syslog-specific
prior art and must not claim compatibility with it. It must identify COSE Sign1
as a possible future versioned checkpoint container and RFC 3161 as the
external mechanism needed when independently trusted time is required. Neither
future mechanism may be inferred or silently applied to Version 1 records.

### REQ-52.11 — Signing-certificate lifecycle and authenticated rotation

The dedicated log-signing certificate may be self-signed and registered
explicitly, or issued by an organization CA. Provisioning must accept an
existing Ed25519 PKCS#8 private key and certificate without requiring the
service to generate or control the CA. The leaf certificate must meet
REQ-52.10, and an optional configured signing EKU and URI SAN signer identity
must be validated before activation.

The signing certificate and key paths, signer identity, and required EKU must
be reloadable from the service configuration on `SIGHUP`. Reload is atomic:
the complete candidate configuration must be read and validated before the
active signer changes. A missing, malformed, expired, mismatched, wrongly
identified, or otherwise invalid candidate must leave the old signer active
and must be logged as a failed reload. Re-reading an unchanged key is a no-op.

Before replacing one valid signer with another, the service must:

1. locally log a `log.integrity.signing_certificate.rotate` STATUS event that
   identifies the old and new key IDs, signer ID, certificate serial, and the
   new leaf certificate;
2. close a checkpoint window containing that local event and sign its anchor
   with the old key; and
3. only then activate the new key for subsequent checkpoints and send the
   byte-identical STATUS document downstream through the role's existing
   STATUS channel.

The certificate announcement must be in the hashed event body, not only in
uncommitted semantic attributes, so the old signature authenticates the exact
new certificate. A process crash at any point must not produce an announcement
signed by the new key that falsely claims authorization by the old key.
Services without an active old signer may install their initial signer during
startup, but cannot claim an authenticated key transition.

### REQ-52.12 — Automatic acquisition of signature evidence

For an operator report, the supplied Kibana query selects the case events; it
must not require the operator to discover or manually export checkpoint
records. For every selected chained event, the verifier must automatically
query the exact `airgap.log.run.id` and `airgap.log.window.id`, require an
exact, non-truncated result count, and retrieve the complete window including
its signed anchor. Automatically fetched records are evidence and must be
identified separately from the operator-selected case events in the report.

When an anchor uses a key that is not directly trusted, the verifier may
recursively retrieve the unique certificate-rotation window announcing that
key and verify its old-key-signed checkpoint. Expansion must be bounded by
unique run/window and key IDs, reject cycles and ambiguous announcements, and
fail closed on missing records, missing anchors, incomplete windows, unknown
keys, untrusted certificate chains, or search-result truncation.

Initial trust must come from outside the searched logs: either an independently
trusted `airgap-log-trust-v1` registry or a configured CA bundle. A certificate
or public key found only in an unsigned or unverified log record is not trust.
The credentials/configuration file may name these trust inputs so normal
operator use requires only the case query and report destination.

The case query must be expressible either as an Elasticsearch query-string
expression or as an Event Query Language query, so an operator can reuse a
saved search without rewriting it. Both forms must accept an optional
inclusive start and end timestamp. The EQL form must reject sequence results,
because every matching event must be verifiable individually. Time bounds and
the selection syntax apply only to the case query: automatic evidence
acquisition must remain unbounded in time, so a window or certificate-rotation
record outside the operator's time range is still retrieved and verified.

The completed report must verify, for every selected event and every fetched
evidence record, the body hash, chain value, chain continuity, checkpoint
statement, signing certificate identity and validity, and Ed25519 signature.
Changing a body, event timestamp, severity number, severity text, recorded
hash, chain value, checkpoint hash, signature, certificate announcement, or
required record must make the report fail.

Certificate validity must be evaluated at the signed anchor's protected event
time, not at report-generation time. A certificate that was valid at the
event time but has since expired must remain valid historical evidence and
produce a visible non-failing warning in both the event detail and summary. A
certificate that was not yet valid or had expired at the event time must fail
verification. Certificate parsing, chain validation, public-key extraction,
and Ed25519 verification must run in the verifier process rather than invoke
an external OpenSSL command.

Acceptance coverage must include the complete deployed query path, not only
mock search responses: a real Air-gap role must produce a signed window, the
preserved OTLP records and searchable integrity fields must be indexed into
Elasticsearch, the case query must select one event, and the verifier must
retrieve and validate that event's complete evidence through a real Kibana
console proxy. The same selection must also be proven through a real EQL
query. The test must retain altered-body, altered-hash, and
altered-signature indices as negative controls. Timestamp and severity
mutations must also be retained as negative controls.

## REQ-53 — Graceful upgrade of legacy gap records

> After dedup is upgraded from a version that wrote gap records in the legacy
> JSON format (0.1.10-SNAPSHOT and earlier, key `topic_partition:windowMin`)
> to a version that writes the binary format (key `topic:partition:windowMin`,
> REQ-40), `create` and `gaps` must keep working against the same gap topic
> without manual cleanup, and no gap in a window still within the detection
> range may be lost.

* Status: **Implemented**.
* Implemented in: 0.1.17-SNAPSHOT
* Covered by: Go unit tests (`src/create`, `src/gaps`) and TC-41
* Notes: Legacy records are never compacted away by the new records because
  the key format differs. Legacy records of windows restored from dedup state
  are tombstoned when those windows are purged (REQ-54); records of windows
  purged before the upgrade remain until removed manually. Reported as LIA stream issue 8 (one ERROR per legacy record and
  run; gaps missing from `gaps.json`).

Acceptance criteria:

1. Legacy JSON records are read. Topic, partition, `window_min`,
   `window_max` and the relative `gaps` are taken from the JSON value, not the
   ambiguous legacy key, so topics containing `_` are handled.
2. Gaps from legacy records are included in the resend bundle as absolute
   offsets, merged with gaps from binary records for the same partition.
3. When a legacy and a binary record describe the same window, the binary
   record is used regardless of gap-topic partition or read order. A binary
   tombstone for the window also hides the legacy record.
4. Reading legacy records must not produce one log record per record. Each
   run logs at most one summary of legacy records (count and how many were
   superseded) and at most one ERROR summary of records that cannot be parsed
   at all; per-record details are logged at DEBUG only.
5. Acceptance must use a real legacy dedup build writing to Kafka, an
   in-place upgrade to the current dedup (same application ID and state), and
   the current `create`, and must include a window that exists only as a
   legacy record.

## REQ-54 — Bounded gap topic: tombstones for purged windows

> When dedup purges a window from its detection range (`MAX_WINDOWS`
> exceeded), it must delete that window's gap record by writing a tombstone,
> whether or not the window had gaps, so that the compacted gap topic holds at
> most `partitions × MAX_WINDOWS` live keys instead of one key per window ever
> seen.

* Status: **Implemented**.
* Implemented in: 0.1.17-SNAPSHOT
* Covered by: Java unit tests (`PartitionDedupAppTest`, `GapDetectorTest`),
  Go unit tests (`src/create`, `src/gaps`), TC-41 and TC-42
* Notes: Late arrivals for a purged window are not deduplicated, so its gaps
  are no longer resendable. `MAX_WINDOWS × WINDOW_SIZE` must cover the events
  per partition between a gap and the next resend bundle. Previously every
  window left a record behind and the topic grew without bound.

Acceptance criteria:

1. Purging a window writes a tombstone for `topic:partition:windowMin`, both
   for gap-free windows and for windows with missing offsets.
2. Purging a window with missing offsets logs one WARN with the number of
   missing offsets; purging a gap-free window logs at DEBUG only.
3. Purging a window that was restored from dedup state after an upgrade also
   tombstones its legacy key `topic_partition:windowMin` (REQ-53), once.
4. `create` and `gaps` treat tombstones as deletions, not parse errors, for
   both key formats, independently of read order.
5. With a compacted gap topic, the number of keys physically present after
   compaction and tombstone removal is at most `MAX_WINDOWS` per source
   partition, and `create` exports only the gaps of the active windows.
