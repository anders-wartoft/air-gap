# Release Notes

## 0.1.16-SNAPSHOT

### CI testcase reports

- The batch Docker testcase runner supports optional `--json PATH` and
  `--junit PATH` reports (individually or together), plus `--log-dir DIR` for
  retained logs. JSON records each selected case's status, process exit code,
  duration, and skip reason; JUnit exposes failures and manual skips in CI.
- TC-16 is explicitly manual in batch runs (`-1` / skipped) while direct
  execution remains unchanged. Batch exit codes are `0` with no failures
  (skips allowed), `1` for testcase failures, and `2` for usage/setup/report
  errors. Explicit nonexistent case selections are now rejected.
- Documented CI usage, report formats, and the separately planned split-host
  TC-16 benchmark workflow.

### Bounded TCP reconnects and repeated-log summaries

- TCP connection attempts now respect `tcpRetryInterval` (default 1000 ms)
  across messages and concurrent sending threads, including TLS 1.3 connections
  rejected after the client initially completes its handshake. Misconfigured
  peers can no longer bypass the send retry delay through rapid reconnects.
- The shared Go application logger now combines repeated records, including
  interleaved repeats, into `Message repeated N times: <original text>`
  summaries. The package reads `AIRGAP_LOG_REPEAT_INTERVAL` at startup
  (milliseconds; default 10000; `0` disables suppression), without application
  configuration or call-site changes. The cache holds at most 1024 entries and
  expires idle entries. Counts flush on timer expiry, eviction, and log-file,
  format, or service-identity changes. Summaries preserve severity and
  participate in log integrity; fatal records and mandatory production warnings
  remain unsuppressed. Records with changing bodies/attributes and direct
  third-party text output are not combined. Counts still pending at process exit
  are not guaranteed a final summary.
- JSON repeat records retain the original event body, name, severity, and
  semantic attributes instead of replacing them with a summary event. The
  integer `airgap.repeat.count` attribute is present only when the represented
  occurrence count differs from one; timestamps and integrity stamps remain
  fresh. Text logs keep the human-readable repeat summary.
- Removed tracked Python bytecode under `tests/env/__pycache__/` and ignored
  that cache directory.
- Added TC-38, an isolated Docker end-to-end test with real upstream/downstream
  services: missing-client-certificate mTLS failures must respect the configured
  reconnect interval, repeated JSON events retain original semantics and carry
  integer repeat counts, interval `0` disables suppression, and supplying the
  client certificate restores authentication and payload delivery.
- Moved the standalone TC-29 and TC-37 helper programs into separate packages
  (`tests/env/lib/tc29integrity` and `tests/env/lib/tc37fixture`) so
  `go vet ./...` and `go test ./...` no longer fail on duplicate `main` and
  `fatalf` declarations.
- Fixed an intermittent `UDP_callback` failure in the pinned-TLS tests: probes
  are now sent from an unconnected UDP socket, so datagrams sent before the
  downstream binds are retried instead of failing with `connection refused`.
- Disabled repeated-log suppression in the pinned-TLS application test harness,
  which uses individual log records to synchronize reloads. Repeated SIGHUP
  completion messages can no longer be delayed beyond the test timeout.
- Fixed downstream pinned-TLS SIGHUP reloads rejecting commented properties
  such as `# privateKeyFiles=` as unknown configuration keys. Startup and reload
  now explicitly skip blank lines and full-line `#` comments, including indented
  comments; uncommented unknown keys remain rejected during reload.
- All application properties readers now share whitespace-delimited inline
  comment handling, including TLS validation and log-signing reloads. A `#`
  starts a comment at the beginning of a line or after a space/tab; embedded
  hashes such as `id=tcp#1` remain literal. Values containing a literal space/tab
  followed by `#` now require an environment or CLI override; quoting and
  backslash escaping are not introduced. Input-filter files are unchanged.
- Added TC-39, an isolated Docker regression for properties comments and
  pinned-TLS SIGHUP reloads in both roles, including literal embedded hashes,
  unknown-key rejection, and payload delivery on existing and new connections.

## 0.1.15-SNAPSHOT

### Stable Kafka subscriptions during slow UDP processing

- Every stamped log record now publishes `airgap.log.integrity.version=v1`, so
  a verifier reads the hash, chain and checkpoint rules from the record instead
  of knowing the format out of band. The value is the same one framed into the
  signed checkpoint statement, and is outside every hash input, so existing
  hashes and signatures stay valid and logs written before the attribute are
  still verified as Version 1.
- Fixed two bugs that made `tests/env/verify_signed_anchor.py` unusable on a
  real exported window: it probed the anchor-only `airgap.log.anchor` attribute
  on every record and aborted on the first ordinary record, and it wrote two
  zero bytes after the checkpoint domain where the signer writes one, so the
  checkpoint hash never matched. Both verifiers now reject a record declaring
  an unimplemented format version instead of reinterpreting it.
- Added `tests/env/verify_integrity_report.py`, an operator-facing verifier that
  retrieves tagged case events through Kibana with API-key or basic
  authentication, reports each event's declared format version, and checks
  every available body hash and chain. It groups continuity checks by
  run/segment, accepts declared chain restarts, accumulates malformed and
  unsupported events instead of stopping at the first one, and writes a compact
  terminal result plus an attachable full report when the query exceeds the
  detail limit. Kibana requests now require an exact `hits.total` and fail
  closed when `--size` truncates the matching case instead of issuing a green
  report over a partial result. Report endings say `Overall result: ✅` or
  `Overall result: ❌` instead of printing an unexplained bare icon. TC-36
  covers the Kibana request, result truncation, independent chains, restart
  handling, and tampering negative controls. Signed reports now verify
  Ed25519 checkpoints through an independently trusted registry or CA bundle,
  automatically fetch complete run/window evidence, and recursively verify
  old-key-signed certificate transitions. Each positive case has a
  matching negative control built by editing a message body: one edit leaves
  the recorded hash untouched so the body-hash layer catches it, and one
  recomputes the hash so only the chain value can still expose the edit.
- Added authenticated log-signing certificate rotation for the Go services.
  `SIGHUP` rereads changed signing paths atomically; a valid replacement is
  announced in a locally hashed STATUS document, force-anchored with the old
  key, then activated and sent downstream. Invalid candidates leave the old
  signer active. `scripts/configure-log-signing.py` installs either self-signed
  or CA-issued leaf/key paths atomically and can signal a live service. TC-37
  covers CA trust, automatic evidence acquisition, rotation delegation, and
  body/timestamp/severity/hash/signature negative controls. TC-37 now uses a real upstream process
  to produce and rotate the signed OTLP chain, indexes the preserved records
  into Elasticsearch 8.15.3, proves the tagged time-bounded selection directly
  with the verifier's JSON query wrapper, and performs automatic evidence and
  signature verification through Kibana 8.15.3. Separate real indices retain
  the body, timestamp, severity, hash, and signature negative controls; no mock Kibana responses or
  synthetic logger events remain in TC-37.
- Version 1 chain values now bind each record's original `timeUnixNano`,
  `severityNumber`, and `severityText` in addition to its sequence number and
  body hash. Timestamp or severity edits therefore invalidate the chain and
  any signed checkpoint covering it. The operator verifier now performs
  certificate parsing, PKIX CA-path validation, public-key extraction, and
  Ed25519 verification in-process through pinned `cryptography` and
  `pyOpenSSL` dependencies instead of forking OpenSSL. Certificate validity is
  evaluated at the protected event
  time: a certificate that was valid then but has since expired remains a
  successful historical verification and is reported with a non-failing 🧣
  warning and summary warning count.
- Published the normative Air-gap Log Integrity Profile for OpenTelemetry
  Version 1, including exact integer, UTF-8, framing, chain, and checkpoint
  encodings plus an explicit protected/unprotected field table. A deterministic
  public test vector exposes every chain and checkpoint input byte,
  intermediate digest, SPKI-derived key ID, and Ed25519 signature; both the Go
  producer and Python verifier must reproduce it byte-for-byte. The format now
  documents RFC 5848 as evaluated syslog-specific prior art, RFC 8785 as an
  intentionally rejected whole-envelope canonicalization, COSE Sign1 as a
  possible future versioned checkpoint container, and RFC 3161 as the external
  trusted-time layer for deployments that require it.
- `verify_integrity_report.py` now accepts `--eql` as an alternative to
  `--query`, so an operator can paste a saved Event Query Language search
  instead of rewriting it as a query-string expression. EQL queries go to
  `<index>/_eql/search` with `@timestamp` and `event.category` configured, and
  sequence results are rejected because every matching event must be verified
  individually. New `--date-from` and `--date-to` bounds work with both
  selection forms, so the date range no longer has to be embedded in the query
  text. Date bounds apply only to the case query: automatic evidence
  acquisition stays unbounded, so a signed window or certificate-rotation
  record outside the operator's range is still fetched and verified. TC-37
  proves the same case event, evidence, and signature through both a
  query-string selection and a real EQL query.
- Registered TC-36 in `tests/env/testcases/36.env`, so it runs through the
  standard `run-testcase.sh` and `run-testcases.sh` dispatchers instead of
  only working when its shell script is invoked directly.
- New `tests/env/verify_log_chain.py` verifies Version 1 body hashes and
  hash-chain links for an exported log without needing key material, and
  `doc/Logging.md` now walks through investigating a single Kibana event:
  identifying it, exporting its chain segment, saving it as JSON Lines, and
  running the verifier. The verifier reports its hard-coded Version 1 format
  and rejects records declaring a future schema instead of interpreting them
  with Version 1 rules. The documented anchor body was corrected to the
  constant `airgap log chain anchor`; the covered range lives in attributes,
  so hashing the previously documented body with a range appended produced a
  spurious mismatch.
- `verify_integrity_report.py` no longer aborts when signing-key trust cannot
  be resolved — for example when a certificate-rotation event has aged out to
  cold storage or been deleted before an investigation runs. The affected key
  stays untrusted, the affected windows fail their signature checks with the
  reason and the remediation (supply the key via `--trust-registry`, or trust
  the issuing CA via `--ca-file`), and every other event is still verified
  and reported. Ambiguous rotations and trust cycles degrade the same way.
  Evidence that cannot be authenticated is never reported as passed.
- Pinned TLS trust directories now ignore well-formed entries for the other
  endpoint role instead of rejecting the whole directory: upstream skips
  `"client"` entries and downstream skips `"server"` entries, so both sides
  can point `tcpTLSTrustedKeysDir` at one shared directory holding both
  roles. Role-less entries, malformed entries, fingerprint mismatches, and
  conflicting peer bindings still fail closed, and an ignored entry grants
  nothing: a peer whose only entry carries the other role is denied exactly
  as with an empty store. TC-27 now mounts one shared directory on both
  containers and adds negative phases proving that upstream with only client
  entries and downstream with only server entries reject the peer by its
  exact fingerprint while both services remain running.

- Fixed the stale gzip compression test after `protocol.DecompressGzip` gained
  its decompressed-size limit parameter. The test now uses downstream's
  production default of 256 MiB and verifies that payloads exceeding an
  explicit limit are rejected, so `go test ./...` compiles and runs
  `src/test` again.
- **New `kafkaMaxProcessingTimeMs`** configures Sarama's
  `Consumer.MaxProcessingTime` and defaults to 30 seconds. Upstream processes a
  Kafka record synchronously through rate limiting, memory admission,
  transformation and transport delivery; the former 100 ms Sarama default
  could therefore log `abandoned subscription ... because consuming was taking
  too long` and recreate a healthy partition subscription. This was local
  consumer backpressure, not a lost Kafka connection.
- TC-34 deliberately limits a UDP upstream to one event per second with a
  one-record Sarama channel. It verifies eight records arrive exactly once,
  consumer lag reaches zero, no subscription is abandoned, and neither
  upstream nor downstream restarts.
- Log rotation now closes the previously opened log descriptor when handling a
  reload, preventing deleted rotated logs from retaining disk space. Standard
  logger output, including Sarama client records, is switched to the new file
  before the old descriptor is released.
- TC-35 verifies repeated rotation and reload cycles leave no descriptor
  pointing to a rotated log and that post-reload application and Kafka client
  records use the new log file. Its negative control runs a build with the
  non-default `airgap_logrotate_leak` tag, which must still be detected as
  leaking.
- TC-34 and TC-35 now compose their common Kafka transfer topology from
  `docker-compose.kafka-transfer.yml` plus a testcase-specific override,
  reducing duplicated infrastructure while preserving isolated test behavior.
- The dedup build now produces one universal fat JAR with all dependency
  natives and 13 platform-specific JARs covering every RocksDB native
  platform. Each platform JAR contains only its matching RocksDB, zstd, Snappy
  and LZ4 libraries; unmapped RocksDB platforms or missing codec libraries
  fail the build. The AMD64 package uses the roughly 22.5 MB `linux-amd64`
  variant instead of the roughly 84 MB universal JAR and is now correctly
  marked AMD64 instead of `noarch`. The README describes how to choose between
  the universal and platform JARs, including the startup failure seen when the
  JAR does not match the host.

## 0.1.14-SNAPSHOT

### Upstream Large-Message Memory Safety

- Upstream now applies explicit Sarama bounds through `kafkaFetchDefaultBytes`,
  `kafkaMaxMessageBytes`, and `kafkaChannelBufferSize`. The defaults preserve
  Sarama's 1 MiB fetch target and 100 MiB response ceiling while reducing the
  buffered-record count from 256 to 4.
- Memory-pressure backpressure resolves the process's cgroup v2 or v1 path,
  including systemd service subgroups, and pauses before admitting another
  Kafka record at `memoryHighWatermarkPercent`. Consumption resumes only at
  `memoryLowWatermarkPercent`, providing hysteresis.
- `memoryLimitBytes` can lower the detected ceiling, and
  `goMemoryLimitPercent` applies a corresponding Go soft heap limit. The
  systemd/cgroup limit remains the hard process boundary.
- TC-30 runs upstream with a 192 MiB Docker cgroup, queues twelve 4 MiB Kafka
  records under deterministic memory pressure, and verifies throttle/resume,
  zero consumer lag, complete TCP transfer, zero restarts, and no OOM kill.

### Airgap Lost Event (ALE) Guard

- **New `maxEventBytes`** sets the largest Kafka record upstream forwards.
  Records above it are replaced by an `AIRGAP LOST EVENT: Event <topic>
  <partition>:<offset> couldn't be delivered due to <reason>` payload carrying
  the original message id, so downstream keeps a continuous offset sequence and
  the gap detector does not pull the event into resend bundles. `0` (default)
  disables the ceiling.
- **New `lostEventGuard`** (default `true`) controls whether the guard is sent.
  Setting it to `false` restores the previous discard behaviour.
- **New `airgapLostEventMessage`** renders the guard payload. Keep the
  `AIRGAP LOST EVENT` marker: it is what downstream and operators search for.
- **New `airgapLostEventMessageFormat`** selects `text` (default) or `json`.
  JSON templates are validated at startup and re-serialized after placeholder
  substitution, so quotes, backslashes and control characters in configuration
  values or error text cannot corrupt the delivered JSON. The README includes
  an OTLP-shaped JSON LogRecord example.
- **Lost-event retention** now deletes files older than
  `lostEventDirectoryDeleteAfterDays` (default 60), compresses non-zip files
  older than `lostEventDirectoryCompressAfterDays` (default 7), and evicts the
  oldest remaining files only when the next payload does not fit. Deletion
  runs first and archives preserve the source mtime, so compression cannot
  restart the deletion clock.
- **New `lostMessageTruncatedMessage`** and
  `lostMessageTruncatedMessageFormat` configure the alarm emitted whenever Go
  removes a retained content file. It is logged with
  `eventName=message.lost.truncated` and sent as `TYPE_STATUS`/`STATUS`, causing
  downstream to store it in `internalTopic`. Text and escaping-safe JSON
  examples are documented. TC-33 covers deletion, compression, age
  preservation, a fresh-file negative case, JSON escaping, internal-topic
  routing, and transfer-topic isolation.
- **New `lostEventDirectory`** (default `/airgap/lost_events/`) writes the
  original payload of each lost event to disk for later inspection. A failed
  write never prevents the guard from being delivered, and an empty value makes
  the guard say so instead of pointing at a directory.
- Encryption failures now also produce a guard instead of formatting a nil
  ciphertext, so an unencryptable record can neither leave as cleartext nor
  disappear silently.
- Every lost event is reported through the configurable `lostEvent` event,
  always carries `eventName=message.lost`, and is counted as `lost_events` /
  `total_lost_events` in the statistics record.
- TC-31 is the negative control for TC-30: it runs the same stack with
  deliberately permissive memory settings and asserts that upstream is actually
  OOM-killed, proving the TC-30 bounds are what prevent the crash.

### Configurable Airgap Events

- Events an operator may need to act on are now configured with a single
  property carrying both the severity and the text, for example
  `memoryHighWatermarkLog=WARN:Upstream {upstreamId} entering high memory
  watermark.` The value is either `NONE`, which turns the event off completely,
  or `LEVEL:message`, where the level is the text before the first colon. A
  value whose prefix is not a level is used as the message in its entirety at
  the event's default level, so a message may contain colons without being
  quoted, and upstream warns at startup when a prefix looks like a misspelled
  level.
- **New `memoryHighWatermarkLog`** and **`memoryLowWatermarkLog`** replace the
  previously fixed memory-throttle messages. The resume event defaults to
  `NONE`: the pause is the event worth alerting on. Both may use
  `{memoryCurrentBytes}`, `{memoryUsedLimitBytes}`, `{memoryUsedPercent}` and
  `{memorySource}`.
- **New `lostEvent`** (default `INFO:...`) is the log record written when an
  event is lost, separate from the `airgapLostEventMessage` payload delivered
  downstream.
- Every configuration property is available as a `{placeholder}` in every
  message. Placeholder names are matched ignoring case and underscores, so
  `{lostEventDirectory}` and `{lost_event_directory}` are the same placeholder
  whichever notation the configuration file uses. Unknown placeholders are left
  exactly as written, so a typo is visible instead of rendering an empty field.
- The four settings that hold something other than a configured value resolve
  to a description of their state: `{key}` and `{newkey}` render as
  `(redacted)` or `(none)`, `{publicKey}` as its SHA-256 fingerprint,
  `{filter}` and `{inputFilter}` as `(set)` or `(none)`, and `{translations}`
  as the `from=to` pairs in force. Symmetric key material is never rendered,
  because a message is written to the log and, for a lost event, delivered
  downstream.
- A unit test fails if a configuration property is added without being made
  available to templates. There is no exclusion list, so the placeholder set
  cannot fall behind the configuration.
- The OTel attributes of every event are unchanged regardless of the configured
  text, so machine-readable processing does not depend on the wording.
- All of the new properties can be overridden with environment variables and
  command-line arguments in the usual way, for instance
  `AIRGAP_UPSTREAM_MEMORY_HIGH_WATERMARK_LOG` or `--lostEvent=NONE`.

### OpenTelemetry Log Output and Log Integrity

- **New `logFormat` option** for all Go binaries. `logFormat=otel` writes OpenTelemetry Logs as OTLP/JSON, one record per line, in the same shape as the OpenTelemetry Collector file exporter, so a Collector `filelog` receiver can read it without a custom parser. `logFormat=text` remains the default and is unchanged. `service.name` is derived from the binary name and `service.instance.id` from the `id` option, falling back to the hostname.
- Dedup packages include an optional `/etc/airgap/log4j2-otel.xml` layout producing the same OTLP/JSON envelope. The existing `/etc/airgap/log4j2.xml` PatternLayout remains the default.
- **New `hash` option** hashes the payload of every log record and records the digest as the `airgap.log.hash` attribute. Supported algorithms are `SHA256`, `SHA384`, `SHA512`, `SHA3-256`, `SHA3-384` and `SHA3-512`, with `none` as the default. Only the record body is hashed, not the timestamp or severity, so that a Collector rewriting metadata does not invalidate the digest.
- **New `hash-chain` option** binds each record to its predecessor and writes an anchor record every N records. Because an anchor is itself hashed and chained, it transitively commits to every record in its window, which means the forthcoming signing support only has to sign anchors rather than every record. Attribute names for signatures are reserved but not yet emitted.
- Sequence numbers are unique and monotonic for the lifetime of a process. When the chain is configured after startup, the first chained record carries `airgap.log.chain.restart`, so a verifier can distinguish a declared restart from records having been removed.
- All three options can also be set through the usual environment variables, for example `AIRGAP_UPSTREAM_LOG_FORMAT`, `AIRGAP_UPSTREAM_LOG_HASH` and `AIRGAP_UPSTREAM_LOG_HASH_CHAIN`.
- Known limitations: records written before the configuration file is read are not stamped, and direct writes to the exposed `StdLogger` writer bypass the chain. Sarama is routed through the application logger in OTel mode. These details are documented in [Logging.md](doc/Logging.md), together with the chain formula and a standalone verification script.
- Routing every log method through a single formatting path made them recognisable printf wrappers to `go vet`, which surfaced twelve pre-existing format-string defects in `upstream`, `downstream`, `resend`, `protocol` and `kafka`, such as `%i` verbs, missing verbs and arguments passed to a format string with no placeholder. All twelve are fixed; they previously produced mangled log output.

### Dedup Restart Safety

- Dedup no longer exits while Kafka Streams is legitimately rebalancing or restoring local state. `FAIL_FAST_STARTUP_TIMEOUT_MS` now controls periodic startup diagnostics instead of imposing a fatal startup deadline.
- Temporary Kafka and DNS outages are reported as degraded connectivity while Kafka Streams reconnects in-process. Terminal Streams errors and fatal uncaught exceptions still exit non-zero for supervisor recovery.
- `INSTANCE_ID` now supplies Kafka's static `group.instance.id` unless `KAFKA_GROUP_INSTANCE_ID` is explicitly configured, reducing session-timeout delays when a service instance restarts.
- The packaged dedup unit orders itself after `confluent-kafka.service` and limits repeated failed starts to five attempts in five minutes, preventing unbounded restart loops without imposing a maximum state-restore duration.
- The connectivity probe no longer blocks its monitor thread on a stalled admin client, so an outage is reported within the configured `FAIL_FAST_CHECK_TIMEOUT_MS` rather than after the Kafka admin client defaults expire.
- TC-9 now includes an isolated Docker restart-safety check that stops and restarts Kafka under a running dedup instance, then restarts dedup itself, and verifies that it reports degraded connectivity, recovers in-process without restarting, reaches `RUNNING` again, and never logs a fail-fast termination.

### Systemd Reload Support

- Added `ExecReload=/bin/kill -HUP $MAINPID` to the upstream and downstream service templates. `systemctl reload airgap-upstream@<instance>.service` and `systemctl reload airgap-downstream@<instance>.service` now send SIGHUP to reopen log files and run the existing TLS reload handling without restarting.
- Run `systemctl daemon-reload` after installing updated units. Custom-named service templates need the same `ExecReload` directive or a drop-in. Reload completion confirms signal delivery, not completion of the application's asynchronous reload; check application logs for its outcome.
- TC-8 now includes an isolated systemd Docker check that runs `systemctl reload` for both packaged service templates, verifies unchanged process IDs and reload-completion logs, and checks that new logs grow while rotated logs stop growing.

## 0.1.13-SNAPSHOT

### Pinned Public-Key TLS

- **New `tcpTLSAuthMode=pinned`** for upstream/downstream TCP transport: mutual authentication by exact SHA-256 public-key fingerprints, without a certificate authority. Pinned mode requires ECDSA P-256 identities and TLS 1.3; existing CA-based TLS remains the default.
- **JSON trust directories**: `tcpTLSTrustedKeysDir` loads public-key entries for trusted servers on upstream and trusted clients on downstream. Entries contain a peer name, role, public key, and matching fingerprint. Multiple entries for the same peer support overlapping old/new keys.
- **Strict trust-store validation**: Rejects malformed entries, unknown JSON fields, incorrect roles, fingerprint mismatches, conflicting peer assignments, symlinks, and unsafe permissions. Hidden and `*.tmp` files are ignored for staging. An empty valid store denies every peer.
- **Standalone identity generation**: `--generate-tls-key` creates one local `.key`, `.crt`, `.pub`, and `.fingerprint` set and exits without starting the service or interrupting an already-running instance. Repeated commands automatically continue numbering without overwriting existing files.
- **Generated JSON provisioning**: Optional `--json-file-dir` exports a ready-to-install public trust entry for the opposite endpoint. Exchange only the JSON through trusted provisioning; keep the private key and local certificate on the originating machine. The earlier `--generate-tls-keysets=N` option is replaced by one-keyset-per-invocation generation.
- **Safe SIGHUP reloads**: Reload local identity and trust-store paths without restarting. Invalid candidates retain the last valid configuration. Removing a pin and successfully reloading revokes it and disconnects sessions using that key.
- **Optional acknowledged client rotation**: Upstream `tcpTLSRotationEnabled` requests confirmation before switching identities. Downstream `tcpTLSAutomaticRotation` explicitly permits installation of new client pins under an authenticated peer identity. Both default to false; acknowledgments follow durable public-key/replay-state persistence, with old-identity recovery for retryable interruptions.
- **Optional idle-pin aging**: `tcpTLSPinIdleSeconds` temporarily deactivates unused overlapping pins in memory while preserving connected keys and the last-used key per peer. Disabled by default; it does not delete trust files or replace explicit revocation.

**Operational notes:** Generated JSON entries have distinct peer names; for manual rotation, set the replacement entry's `peer` to the existing identity before provisioning. Trust the new key on the opposite endpoint before switching the local identity, and retire the old pin after verifying the replacement connection. Certificate names, issuers, and validity dates do not authorize pinned keys or expire their trust. Automatic rotation is not independent recovery from an old-key compromise. Expiry, measured performance acceptance, and the exhaustive fault/concurrency matrix remain pending.

See [Encryption](doc/Encryption.md) for setup, JSON exchange, and manual rotation, and [Pinned rotation](doc/PinnedRotation.md) for protocol and recovery details.

### Production Configuration Warnings

- **Startup and shutdown warnings** in upstream, downstream, create, resend, and the Java deduplication application highlight production-risk settings, including verbose logging, unencrypted connections, disabled statistics, synthetic/discard targets, and constrained buffers where applicable.
- Warnings use the `[PRODUCTION-CONFIG]` marker and remain visible independently of the configured logging threshold. They report risks without changing the selected configuration.
- Added warning and shutdown-order coverage, including TCP shutdown tests.

### Test Environment and Coverage

- **Freestanding Docker test environment** with configurable base images, mounted application builds, Kafka TLS fixtures, and automated testcase/chain runners.
- **TC-27 pinned TLS Docker smoke test** generates JSON entries with the real `--json-file-dir` option and installs them unchanged into the opposite endpoint's trust directory. It checks exact peer/fingerprint authentication, TLS 1.3, application delivery, rejection of untrusted client/server identities, and recovery.
- Added Go unit, TLS integration, and subprocess tests for generation, trust validation, reload/revocation, acknowledged rotation/recovery, idle-pin aging, and functional authentication with 2,000 distinct pins. Functional capacity coverage is not a measured performance guarantee.
- Expanded [testcase documentation](tests/documents/TESTCASES.md) and [test environment instructions](tests/env/README.md).

### Dependency and Build Updates

- Updated the Go toolchain requirement to `go1.25.12` and refreshed Go dependencies.
- Updated Java logging dependencies to Log4j `2.25.5`, with aligned Jackson dependencies.
- Added a tracked fallback Go version file so dependency tooling can resolve the module before running `make`; builds still regenerate the application version.
- Updated the Java application version to `0.1.13-SNAPSHOT`.

## 0.1.12-SNAPSHOT

### Packet Loss Monitoring and VMware Optimization

#### SO_RXQ_OVFL Socket Queue Overflow Monitoring (Downstream)

- **New `enableRxqOvfl` configuration parameter** (Linux only): Enables tracking of kernel socket buffer drops via the `SO_RXQ_OVFL` socket option. When enabled, downstream statistics include `SO_RXQ_OVFL` (interval drops) and `SO_RXQ_OVFL_TOTAL` (cumulative) counters.
- Requires `ReadMsgUDP` with ancillary data parsing (40-byte out-of-band buffer) to extract drop counts from the kernel.
- Configurable via property file (`enableRxqOvfl=true`), environment variable (`AIRGAP_DOWNSTREAM_ENABLE_RXQ_OVFL=true`), or CLI flag (`--enableRxqOvfl=true`).
- **Important limitation**: `SO_RXQ_OVFL` only measures drops between kernel UDP stack and socket buffer. It does **not** capture NIC drops, kernel backlog drops, or hypervisor-level packet loss in virtual environments. See `doc/Monitoring.md` for multi-layer monitoring strategy.

#### Message-Level Tracking (Upstream)

- **New atomic counters**: `messages_sent`, `messages_dropped`, `total_messages_sent`, `total_messages_dropped` now distinguish Kafka messages from UDP fragments (one large message may produce multiple fragments).
- **Fixed critical mutex bug**: Added missing `Lock()` before `Unlock()` in transport success path that caused "sync: unlock of unlocked mutex" panic, preventing statistics logging.
- Enables accurate end-to-end message loss calculation: compare upstream `total_messages_sent` with downstream `total_received` to measure air-gap delivery rate.
- Statistics output format unchanged; new fields added to JSON logs when `logStatistics` is enabled.

#### UDP Broadcast Optimization for Virtual Environments

- **Documented existing broadcast support**: Confirmed `targetIP=255.255.255.255` (global broadcast) works with automatic `SO_BROADCAST` socket option setting (implemented in 0.1.11). Broadcast support was already present but undocumented.
- **New optimized configuration files**:
  - `config/upstream-broadcast-vm.properties`: Conservative settings for VM environments (EPS=3000, compression enabled at 1KB threshold, statistics logging)
  - `config/downstream-broadcast-vm.properties`: Tuned buffers for high-throughput broadcast reception:
    - `rcvBufSize=16777216` (16MB, up from 4MB default)
    - `numReceivers=20` (up from 10 default)
    - `channelBufferSize=32768` (up from 16384)
    - `readBufferMultiplier=32` (up from 16)
    - Includes system tuning commands (`sysctl` for 32MB max buffer)
    - Includes firewall DNAT configuration to avoid duplication with multiple receivers
- **Realistic expectations documented**: In constrained VM environments with UDP broadcast, expect 5-10% packet loss even with optimizations. The deduplication and gap-based resend architecture is designed to handle this.

#### Comprehensive VMware and Virtualization Documentation

- **New `doc/Monitoring.md` section** (lines 307-541): "Special Case: VMware and Hypervisor Packet Loss"
  - Explains why `SO_RXQ_OVFL=0` doesn't mean "no drops" in virtual environments
  - Step-by-step diagnostic process for identifying hypervisor vs application drops
  - VMware detection checklist (`hostnamectl`, `lspci`, `/mnt/hgfs/` mount points)
  - Visual diagram showing invisible hypervisor packet drop layer
  - Six ranked solutions: reduce EPS, VMXNET3 driver, tune vNIC, pin CPU resources, bare metal, accept and monitor
  - VMware-specific monitoring strategy (what NOT to trust vs what to monitor)
- **Multi-layer monitoring strategy** documented in `doc/Monitoring.md`:
  1. End-to-End: Compare upstream sent vs downstream received (ultimate truth)
  2. Application: Message/fragment counters, SO_RXQ_OVFL
  3. NIC: `ip -s link`, `ethtool -S` counters
  4. Kernel: `netstat -s`, `/proc/net/snmp`
  5. VM-Specific: Hypervisor metrics (invisible to guest OS)
- **New FAQ entry**: "UDP broadcast is dropping packets in VMware/virtual environments - what can I do?" (`doc/FAQ.md`)
  - 10 optimization strategies beyond lowering EPS:
    1. Enable compression (50-80% bandwidth reduction)
    2. Increase downstream receive buffers (OS and application-level)
    3. Increase receiver threads (better CPU utilization)
    4. Enable SO_RXQ_OVFL monitoring (diagnostic)
    5. Use better virtual NIC drivers (VMXNET3 vs E1000)
    6. Pin VMs to dedicated CPU cores (avoid contention)
    7. Increase VM NIC queue depth (ESXi advanced)
    8. DSCP/TOS QoS marking (not yet implemented)
    9. Switch to TCP (alternative if broadcast drops unacceptable)
    10. Deploy redundant upstreams (high availability)
  - Recommended configuration for broadcast in VMs
  - Expected results and success criteria
  - Monitoring commands and verification steps
  - Links to `doc/Monitoring.md` for detailed drop analysis

#### VMware Drop Detection Tooling

- **New script**: `tools/dev/vmware-drop-detector.sh`
  - Automated diagnosis of hypervisor vs application packet drops
  - Parses upstream and downstream JSON logs for total message comparison
  - Calculates loss rate and compares with `SO_RXQ_OVFL_TOTAL`
  - If `SO_RXQ_OVFL=0` but packets missing → diagnoses as hypervisor drops
  - Provides actionable solutions based on diagnosis type
  - Requires `jq` for JSON parsing

### Documentation Enhancements

- **README.md**:
  - Added `enableRxqOvfl` parameter documentation with VMware warning (line 501)
  - Expanded UDP Broadcast section (lines 588-633) with configuration instructions, macOS loopback behavior note, and broadcast duplication solution with firewall DNAT examples
  - Added `nic` parameter requirement when using `targetIP=255.255.255.255`
- **doc/Monitoring.md**:
  - New "Understanding Packet Drops and Loss Detection" section with packet path diagram
  - "Where Packets Can Be Dropped" taxonomy (5 layers)
  - Interpreting combined metrics with diagnostic scenarios
  - Complete VMware troubleshooting guide (307+ lines)
- **doc/FAQ.md**:
  - New comprehensive FAQ on UDP broadcast packet loss in VMs (240+ lines)
  - Link to Monitoring.md from SO_RXQ_OVFL monitoring section

### TLS Enhancements

- **PKCS#8 encrypted private key support**: Full implementation of PKCS#8 EncryptedPrivateKeyInfo decryption (previously commented out/unsupported). Supports modern OpenSSL 3.x key formats generated with `openssl genrsa -aes256` or `openssl genpkey -algorithm RSA -aes256`.
- **PBES2 with PBKDF2**: Supports PBES2 (Password-Based Encryption Scheme 2) with PBKDF2 key derivation using multiple hash algorithms:
  - HMAC-SHA1 (legacy compatibility)
  - HMAC-SHA-256 (recommended)
  - HMAC-SHA-384
  - HMAC-SHA-512
- **AES encryption support**: AES-128-CBC, AES-192-CBC, and AES-256-CBC cipher modes
- **Exported TLS certificate loader**: New `LoadTLSCertificate()` function in `src/kafka/getkafka.go` allows non-Kafka components (e.g., TCP transport) to reuse the same key-decryption logic without duplicating PKCS#8 parsing code
- **Better error messages**: Clear diagnostics for wrong password ("invalid PKCS#7 padding") and unsupported key formats
- Both legacy PEM (`Proc-Type: 4,ENCRYPTED`) and modern PKCS#8 (`ENCRYPTED PRIVATE KEY`) formats are now supported

### Test Fixes

- **Fixed `src/create/create_test.go`** for binary format migration:
  - Replaced `buildGapMessage()` helper (JSON-based) with `buildGapWindow()` (struct-based)
  - Changed test data types from `map[string]json.RawMessage` to `map[string]parsedWindow`
  - Fixed gap array types from `[][]float64` to `[][]int64`
  - All 5 test cases now passing

### Code Quality

- **CodeQL alert for `InsecureSkipVerify`**: Alert #34/35 persists despite suppression attempts. `InsecureSkipVerify: true` is required for connecting by IP address and through load balancers (Go's default hostname verification would fail). The `VerifyConnection` callback performs manual certificate chain validation and CN regex matching. **Recommendation**: Dismiss alert in GitHub UI with justification, as inline CodeQL suppressions are not reliably recognized.
- **CodeQL alerts for integer conversions**: Removed suppression comments in favor of inline safety comments (alerts #29-#33). All conversions have explicit bounds checks immediately before conversion. The checks use fatal errors on overflow, ensuring safe conversion to smaller types (uint16, int32). CodeQL's pattern matching may not recognize these checks; if alerts persist, dismiss in GitHub UI with reference to the bounds validation code.

### Internal Changes

- Added atomic operation imports (`sync/atomic`) to `src/udp/receiver.go` and `src/upstream/upstream.go`
- Extended `TransportReceiver` interface in `src/downstream/interfaces.go` to accept `enableRxqOvfl` parameter
- Socket option constant: `SO_RXQ_OVFL = 40` defined in `src/udp/receiver.go`
- Configuration parsing extended across 20+ locations in `src/downstream/configuration.go` for `enableRxqOvfl`
- Statistics logger updates in `src/downstream/downstream.go` for conditional SO_RXQ_OVFL output

## 0.1.11-SNAPSHOT

### Upstream / Resend — NIC binding for broadcast sends

- **Fix: `nic` setting not honoured when `targetIP=255.255.255.255`**: Previously, the UDP sender socket was created with a plain `net.Dial` call, ignoring the configured `nic`. The OS would pick the outgoing interface freely, and the socket lacked `SO_BROADCAST`, causing sends to the limited-broadcast address to fail silently or go out the wrong interface.
- The new `NewUDPConnWithNIC` helper in `src/udp/sender.go` sets `SO_BROADCAST` when the target is `255.255.255.255`, and binds the socket to the IPv4 address of the named NIC when `nic` is set. Both upstream and resend pass `config.nic` through their `NewUDPAdapter` constructors.
- **No downstream change required**: downstream `targetIP=0.0.0.0` is correct for receiving broadcast; the `nic` field on downstream continues to serve only MTU auto-detection.

### Security Hardening — Downstream Listening Port

Four denial-of-service vulnerabilities on the downstream listening port have been fixed. All mitigations are on by default with safe values and can be tuned via configuration.

- **Fragment cache memory exhaustion (OOM)**: An attacker could flood the port with UDP/TCP packets bearing unique message IDs and `nrMessages=65535`, filling the fragment-reassembly cache indefinitely. The cache now enforces a configurable entry cap (`maxCacheEntries`, default 100 000). Fragments arriving when the cache is full are dropped with a warning rather than accepted. The limit is exposed as exported constants `DefaultMaxCacheEntries` / `DefaultMaxNrMessages` in the `protocol` package so the value is accessible without magic numbers.
- **Oversized nrMessages field**: Packets claiming an implausibly large fragment count (`nrMessages` > `maxNrMessages`, default 4 096) are now rejected at parse time before any cache memory is allocated. Legitimate fragmentation at MTU 1500 tops out at ~730 parts for a 1 MiB payload.
- **TCP goroutine exhaustion**: Without a connection cap, an attacker opening thousands of idle TCP connections could exhaust goroutine stacks and memory. A new `maxTCPConnections` limit (default 256) closes connections that exceed the cap immediately, with a warning log.
- **KEY_EXCHANGE CPU exhaustion via RSA flooding**: Any sender could submit `TYPE_KEY_EXCHANGE` packets to trigger `rsa.DecryptOAEP` against every loaded private key. A configurable rate limit (`keyExchangeMinIntervalSecs`, default 1 s) silently drops duplicate key-exchange requests that arrive before the interval expires. Legitimate key rotation runs every hundreds of seconds and is unaffected.

All four limits are configurable via `.properties` file and environment variable overrides — see README.md for the full parameter table and guidance on when to change the defaults.

### Security Hardening — Dependency and Toolchain Upgrades

- **Go toolchain upgraded** from `go1.24.7` to `go1.24.13`, resolving 14 actively reachable stdlib CVEs (including TLS DoS, ASN.1 memory exhaustion, PEM quadratic blowup, and x509 panic).
- **`jackson-databind` upgraded** from `2.15.1` (May 2023) to `2.18.3` in the Java deduplication module.

### Security Hardening — Sensitive Value Masking in Logs

- `keyPasswordFile` was logged as a literal file path in the startup configuration dump for the `resend`, `create`, and `downstream` applications. It is now masked as `[set]`, consistent with how `upstream` already handled it.

### Documentation and README

- Added a "DEBUG Logging Caveat" section to `doc/InputFilter.md`: when `logLevel=DEBUG` is active, up to 80 bytes of raw Kafka payload and the full message key are logged **before** the input filter runs, bypassing content suppression.
- Added four new rows to the downstream parameter table in `README.md` documenting `maxCacheEntries`, `maxNrMessages`, `maxTCPConnections`, and `keyExchangeMinIntervalSecs`.
- Added a "Listening Port Security" section to `README.md` explaining the threat model and guidance on when to raise each limit above its default.

## 0.1.10-SNAPSHOT

### Input Filtering — Empty-Payload Behaviour

- **No more false gaps from filtered events**: Previously, events matched by `inputFilterRules` were dropped entirely before transmission. This caused the downstream gap-detector to report them as missing and trigger unnecessary resend cycles. Filtered events are now sent across the diode with an **empty payload** instead of being dropped, so every sequence ID reaches the downstream and no gap is recorded.
- **Deduplication discards zero-length events**: `PartitionDedupApp` now checks the payload length before forwarding to the clean topic. Events with an empty payload are silently discarded, so they never appear in downstream Kafka — the effect is identical to the old drop behaviour from a consumer perspective, but without the side-effect of generating gaps.
- **Resend honours `inputFilterRules`**: The `resend` application now supports the same `inputFilterRules`, `inputFilterDefaultAction`, and `inputFilterTimeout` configuration options as upstream. Matched events are sent with empty payload using the same logic, keeping resend consistent with normal upstream operation. Environment variable overrides: `AIRGAP_RESEND_INPUT_FILTER_RULES`, `AIRGAP_RESEND_INPUT_FILTER_DEFAULT_ACTION`, `AIRGAP_RESEND_INPUT_FILTER_TIMEOUT`.

### Documentation Updates

- Updated `doc/InputFilter.md` to describe the empty-payload behaviour, corrected statistics field descriptions and troubleshooting log message references, and added `resend` environment variable examples.
- Updated `README.md` upstream configuration table to reflect the empty-payload + dedup-discard design.
- Updated `doc/Resend.md` parameter list to include the three new `inputFilter*` options.

## 0.1.9-SNAPSHOT

### TCP Transport Reliability

- **Configurable TCP retry behaviour**: When `transport=tcp`, upstream now retries failed sends with three new config parameters:
  - `tcpRetryInterval` (`AIRGAP_UPSTREAM_TCP_RETRY_INTERVAL`, default `1000` ms) — pause between retry attempts
  - `tcpRetryTimes` (`AIRGAP_UPSTREAM_TCP_RETRY_TIMES`, default `0`) — maximum retries; `0` means infinite (never give up)
  - `tcpRetryErrorLevel` (`AIRGAP_UPSTREAM_TCP_RETRY_ERROR_LEVEL`, default `WARN`) — log level for per-attempt retry messages (`DEBUG`/`INFO`/`WARN`/`ERROR`)
  - With the default infinite-retry setting, upstream blocks until the downstream comes back and no messages are ever lost, fixing a bug where restarting the downstream caused the upstream to stop sending permanently
  - UDP retry behaviour is unchanged (30 × 100 ms, then Kafka reprocessing)

### Deduplication (Java Streams)

- **Fail-fast startup with retry backoff**: Deduplication app now validates Kafka connectivity at startup and fails fast with clear error messages; retries with exponential backoff and jitter instead of entering a broken state
- **Fixed restart loop**: Resolved a bug where the deduplication app could get stuck in an infinite restart loop under certain error conditions
- **Password leak fix**: Fixed a bug where the Kafka password was exposed in log output (#11, contributed by Tobias Wennberg)
- **Improved logging**: More detailed event-level logging in `PartitionDedupApp` and `GapDetector`; tuned log levels for individual events to reduce noise at INFO level

### Upstream / Downstream

- **Decompression size limit**: New `maxDecompressedSize` config option for downstream to cap the maximum size of a decompressed message, protecting against decompression bombs. Defaults to no limit
- **Fix downstream assembly panic** (#10): Resolved a panic that could occur during message reassembly when fragments arrived in unexpected order or were corrupted; replaced with graceful error handling
- **UDP broadcast duplication remedy**: Documented and added configuration guidance for handling duplicate messages that arise from UDP broadcast environments

### Resend

- **Fixed TLS for resend app**: Resolved a bug that prevented the resend application from using TLS when connecting to Kafka; added `certFile`, `keyFile`, `caFile`, and `keyPasswordFile` support matching upstream/downstream configuration
- **TLS integration tests**: Added `resend_tls_test.go` with test coverage for TLS-authenticated Kafka connections in resend

### Partition Merging

- **Merge multiple topics into one**: New `partitionStartValue` feature (upstream) allows merging events from several source clusters/topics into a single downstream topic by offsetting partition numbers. Full test case (testcase 21) with two upstream sources, one downstream, one deduplicator and full resend support
- **Load balancing filter utility**: New `utils/loadbalance.py` script to calculate the correct `deliverFilter` values for a given number of upstream instances and partitions

### Internal / Testing

- **Compression integration tests**: Added `src/test/compression_test.go`
- **Test configs for testcase 21**: Complete set of properties/env files covering all components in the partition-merge scenario
- **Kafka TLS helper**: Extracted reusable TLS config setup in `src/kafka/getkafka.go` for use by resend and other clients
- **Documentation**: Updated README, Deduplication.md, FAQ.md, Redundancy and Load Balancing.md, Resend.md, and Transport Configuration.md with new features and troubleshooting guidance

## 0.1.8-SNAPSHOT

### Transport Layer Improvements

- **New TCP Transport Support**: Added TCP as an alternative to UDP for more reliable delivery over unstable networks
  - Configure with `transport=tcp` in upstream and downstream config files
  - Environment variable overrides: `AIRGAP_UPSTREAM_TRANSPORT`, `AIRGAP_DOWNSTREAM_TRANSPORT`
  - Automatic connection health checking with 1ms read deadline test
  - Automatic reconnection on connection loss
  - Lazy connection initialization - upstream doesn't fail on startup if downstream is unavailable
  - See [Transport Configuration.md](doc/Transport%20Configuration.md) for detailed TCP vs UDP comparison

- **Enhanced Message Delivery Reliability**:
  - Automatic retry logic: 30 attempts with fixed 100ms intervals (3+ seconds total) for transient failures
  - Messages not marked as consumed in Kafka if send fails - automatic reprocessing when receiver comes back up
  - Prevents message loss due to temporary network issues
  - Only marks Kafka message consumed on successful delivery
  - UDP: Transient failures log at WARN level to indicate delivery uncertainty
  - TCP: Automatic reconnection handling with detailed error reporting

- **Transport Status Monitoring**:
  - New `transport_status` field in periodic statistics showing transport health ("running" or error message)
  - Status change logging: ERROR level when status changes to error, INFO level when restored
  - Transport errors tracked and reported in structured statistics instead of separate log spam
  - Useful for monitoring and alerting on transport issues
  - UDP and TCP use same monitoring interface

### Kafka Connection Monitoring

- **Kafka Status Tracking**: New `kafka_status` field in statistics for both upstream and downstream
  - Monitors Kafka cluster availability and broker connectivity
  - Upstream: Custom Sarama logger captures metadata errors (leaderless partitions, connection refused, etc.)
  - Downstream: Kafka producer error monitoring via callback mechanism
  - Status changes logged at appropriate levels (ERROR for failures, INFO for recovery)
  - Both consumer and producer errors reported via unified callback system
  - Helps identify cluster issues early before message loss occurs

- **Error Handling Improvements**:
  - Kafka consumer no longer panics on connection errors - retries with 5-second backoff instead
  - Producer errors monitored and reported to status system
  - Error messages include context (attempt number, error details) for debugging
  - Health check goroutines with 10-second periodic validation

### Content-Based Filtering

- **New Input Filtering feature**: Content-based filtering of events at upstream before transmission
  - Filter events using regex patterns with allow/deny rules
  - Configure with `inputFilterRules`, `inputFilterDefaultAction`, `inputFilterTimeout` parameters
  - Environment variable overrides: `AIRGAP_UPSTREAM_INPUT_FILTER_*`
  - Useful for security (high-severity events only), privacy (block PII), compliance, and performance
  - First-match-wins rule evaluation with configurable default action (allow/deny)
  - Protection against ReDoS attacks with configurable regex timeout (default 100ms)
  - Dangerous pattern detection at startup (nested quantifiers)
  - New statistics counters: `filtered`, `unfiltered`, `filter_timeouts` per interval
  - Cumulative counters: `total_filtered`, `total_unfiltered`, `total_filter_timeouts`
  - See [InputFilter.md](doc/InputFilter.md) for detailed documentation and use cases

### Documentation

- Release Notes have been extracted to a separate document
- A FAQ (Frequently Asked Questions) document has been created
- Transport Configuration documentation added with TCP vs UDP comparison, tuning, and troubleshooting
- Input Filtering documentation with examples for security, privacy, and compliance use cases
- Configuration and Monitoring documentation updated with new transport and Kafka status fields

### Internal Improvements

- Protocol parser: Removed panic statements, now returns errors gracefully
- Code formatting: Fixed spacing in message parsing logic
- Kafka adapter: Updated to pass error callbacks for status monitoring
- Configuration: Support for all environment variable overrides (AIRGAP_UPSTREAM_*, AIRGAP_DOWNSTREAM_*)
- Test configurations: Added test cases 19 and 20 for input filtering and TCP testing

## 0.1.7-SNAPSHOT

- Fixed the following issues:
  - Support encrypted key files for communication with Kafka #5

## 0.1.6-SNAPSHOT

- Removed `topic` configuration from downstream. Downstream uses upstream's topic name, or a translation of that name.
- Fixed the following issues:
  - resend will not accept payloadSize=auto #1
  - Separate internal logging from event stream from Upstream Kafka #2
  - Dedup can't use TLS to connect to Kafka #3

## 0.1.5-SNAPSHOT

- Multiple sockets with SO_REUSEPORT for faster and more reliable UDP receive in Linux and Mac for downstream. Fallback to single thread in Windows.
- `create` application to create resend bundle files downstream
- `resend` application to resend missing events from the resend bundle created by `create`
- `compressWhenLengthExceeds` setting for upstream and resend to compress messages when length exceeds this value. As of now gzip is the only supported algorithm.
- More configuration for upstream and downstream for buffer size optimizations
- Upstream and downstream can translate topic names to other names. Useful in multi source and/or target setups.
- Statistics logging in upstream, downstream and dedup

## 0.1.4-SNAPSHOT

- Changed the logging for the go applications to include log levels. Monitoring and log updates.
- Changed the logging for the go applications to include log levels. Monitoring and log updates.
- Documented redundancy and load balancing (see doc folder)
- Documented resend (future updates will implement the new resend algorithm)

## 0.1.3-SNAPSHOT

- Added a Kafka Streams Java Application for deduplication and gap detection. Gap detection not finished.
- Added upstreams filter to filter on the offset number for each partition (used in redundancy an load balancing setups)
- Added a topic name mapping in downstream so a topic with a specified name upstream can be written to another topic downstream (used in redundancy an load balancing setups)
- Added documentation for the new features.
- Added JMX monitoring of the deduplication application. Added system monitoring documentation

## 0.1.2-SNAPSHOT

- All configuration from files can be overridden by environment variables. See Configuration Upstream
- UDP sending have been made more robust
- Transfer of binary data from upstream to downstream is now supported
- Sending a sighup to upstream or downstream will now force a re-write of the log file, so you can rotate the log file and then sighup the application to make it log to a new file with the name specified in the upstream or downstream configuration.
- air-gap now supports TLS and mTLS to Kafka upstream and downstream.
- air-gap now supports TLS and mTLS to Kafka upstream and downstream.

## 0.1.1-SNAPSHOT

air-gap now supports several sending threads that all have a specified time offset, so you can start one thread that consumes everything from Kafka as soon as it's available, one that inspects Kafka content that was added for an hour ago and so on. See Automatic resend above.
