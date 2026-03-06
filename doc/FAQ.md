# Frequently Asked Questions

## The deduplication only moves messages from the input topic to the output topic, but doesn't remove duplicates

This is usually explained by the third deduplication case in the algorithm:

- offset is lower than `nextExpectedId`
- but the offset is still inside a known gap

In that situation, the record is **not** treated as a duplicate. It is treated as a previously missing event that arrived late (for example due to UDP loss/reorder and later resend), so it is forwarded to the clean topic.

In other words, deduplication removes records that were already delivered before, but it intentionally accepts late gap-fill events.

Common reasons this can look like “dedup does not dedup”:

- Out-of-order delivery and resend are active, so old offsets legitimately arrive later.
- Gap state was purged (window moved on), so very old events may no longer be tracked as duplicates.
- Input key is not in expected `topic_partition_offset` format, so the record is forwarded directly.

To verify behavior, check logs/JMX for current gaps and compare with the record key (`topic_partition_offset`).

Also, see below for tuning `WINDOW_SIZE` and `MAX_WINDOWS` to better track duplicates in your expected traffic patterns.

This is also mentioned in [Deduplication.md](Deduplication.md#deduplication), but it is a common point of confusion, so it is worth reiterating here.

## How should I set WINDOW_SIZE and MAX_WINDOWS?

`WINDOW_SIZE` and `MAX_WINDOWS` control how much offset history each deduplication instance keeps in memory.

- `WINDOW_SIZE` = number of offsets tracked per window
- `MAX_WINDOWS` = number of windows kept before the oldest window is purged

Together, they define the retained range:

```text
retained offsets per partition ≈ WINDOW_SIZE * MAX_WINDOWS
```

### Practical rule of thumb

- Increase `MAX_WINDOWS` when you need to tolerate longer resend delays or longer out-of-order arrival.
- Increase `WINDOW_SIZE` when throughput is high and windows roll too quickly.
- Prefer many moderately sized windows over a few very large windows for better bitmap behavior.

### Memory tradeoff

Larger values improve duplicate detection across a longer history, but require more RAM.
If memory pressure appears, first try lowering `MAX_WINDOWS`, then tune `WINDOW_SIZE`.

### Example

If you expect late arrivals up to about 500,000 offsets behind current traffic, a starting point is:

```bash
WINDOW_SIZE=1000
MAX_WINDOWS=500
```

Then monitor memory and gap behavior, and adjust gradually.

## Deduplication gets stuck in a restart loop when FAIL_FAST is enabled

If `FAIL_FAST=true`, the deduplication process exits when it does not reach `RUNNING` within the configured startup timeout.

In environments with slower Kafka startup, DNS, TLS handshake, or topic metadata fetch, a low value for `FAIL_FAST_STARTUP_TIMEOUT_MS` can cause repeated restarts (boot loop).

Known working adjustment from field testing:

- `FAIL_FAST_STARTUP_TIMEOUT_MS=10000` caused restarts
- `FAIL_FAST_STARTUP_TIMEOUT_MS=30000` stabilized startup

Example:

```bash
export FAIL_FAST=true
export FAIL_FAST_STARTUP_TIMEOUT_MS=30000
```

If needed, increase further based on your environment and startup latency.


## I have disabled write for /tmp. Now I get an error running the deduplication

The error occurs because RocksDB (used by Kafka Streams for state storage) extracts its native library to tmp by default, but that directory is write-protected on your machine.

You can fix this by setting the ROCKSDB_SHAREDLIB_DIR environment variable to point to a writable directory before running the Java application:

```bash
export ROCKSDB_SHAREDLIB_DIR=/var/lib/kafka-streams/rocksdb
mkdir -p /var/lib/kafka-streams/rocksdb
java -jar java-streams/target/air-gap-deduplication-fat-*.jar
```

Or if you're running it as a systemd service, add this to your service file:

```bash
[Service]
Environment="ROCKSDB_SHAREDLIB_DIR=/var/lib/kafka-streams/rocksdb"
Environment="STATE_DIR_CONFIG=/var/lib/kafka-streams/state"
```

Alternatively, you can set the Java system property:

```bash
java -Dorg.rocksdb.tmpdir=/var/lib/kafka-streams/rocksdb -jar java-streams/target/air-gap-deduplication-fat-*.jar
```

Make sure the directory you choose is writable by the user running the application. The RocksDB native library will be extracted there instead of tmp.
