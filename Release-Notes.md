# Release Notes

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
