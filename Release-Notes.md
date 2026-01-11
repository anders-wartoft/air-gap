# Release Notes

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
