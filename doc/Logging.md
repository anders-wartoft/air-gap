# Logging

All Go binaries (`upstream`, `downstream`, `resend`, `create`, `gaps`,
`multiDownstream`) share one logging framework. It can write plain text, which
is the default, or OpenTelemetry Logs, and
it can stamp every record with a hash and a hash chain so that later tampering
becomes detectable. Java dedup supports the same OTLP/JSON envelope through its
optional `log4j2-otel.xml` configuration; its existing `log4j2.xml`
PatternLayout remains the default.

- [Output format](#output-format)
- [Repeated messages](#repeated-messages)
- [Log integrity](#log-integrity)
- [Attribute reference](#attribute-reference)
- [Illustrative chain-link verification](#illustrative-chain-link-verification)
- [Signatures](#signatures)
- [Verifying a signed window from Kibana](#verifying-a-signed-window-from-kibana)
- [Limitations](#limitations)

## Output format

| Option | Environment variable | Default | Description |
|---|---|---|---|
| logFormat | *PREFIX*_LOG_FORMAT | text | `text` or `otel` |

Environment variables use the same binary specific prefix as every other
option, for example `AIRGAP_UPSTREAM_LOG_FORMAT` or
`AIRGAP_DOWNSTREAM_LOG_FORMAT`, and override the configuration file.

`text` produces the familiar one-line-per-record output:

```text
2026/10/13 10:55:50 [INFO] Upstream version: 0.1.13-SNAPSHOT starting up...
```

`otel` produces an OTLP File Exporter-compatible JSON Lines stream. Each line
contains one `ExportLogsServiceRequest`-shaped OTLP/JSON object with one
LogRecord. Collector ingestion requires a receiver and parser configured for
that envelope:

```json
{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"upstream"}},{"key":"service.instance.id","value":{"stringValue":"Upstream_1"}},{"key":"service.version","value":{"stringValue":"0.1.13-SNAPSHOT"}}]},"scopeLogs":[{"scope":{"name":"sitia.nu/airgap"},"logRecords":[{"timeUnixNano":"1760345750123456789","severityNumber":9,"severityText":"INFO","eventName":"app.lifecycle.start","body":{"stringValue":"Upstream version: 0.1.13-SNAPSHOT starting up..."},"attributes":[]}]}]}]}
```

`service.name` is taken from the name of the running binary. `service.instance.id`
is taken from the `id` option when it is set, and otherwise from the hostname.
`service.version` identifies the application build.

Severity text maps to the OTel severity numbers: TRACE 1, DEBUG 5, INFO 9,
WARN 13, ERROR 17, FATAL 21.

Writing the records to a file is where air-gap's responsibility ends. Shipping
them onwards is a job for a Collector or any other agent that can read the file.

For packaged dedup installations, select the optional layout by setting the JVM
property to `-Dlog4j.configurationFile=/etc/airgap/log4j2-otel.xml`. The
package installs this beside the unchanged default
`/etc/airgap/log4j2.xml`. Dedup's layout uses `service.name=dedup` and
`INSTANCE_ID` as `service.instance.id`, falling back to `dedup`.

## Repeated messages

The Go application logger writes the first message immediately and keeps a
bounded map of message text, severity, attributes, repeat count, and last
emission time. Repeats are combined even when other messages occur between
them. Every 10 seconds it writes a summary at the original severity:

```text
[WARN] Connection unavailable
[WARN] Message repeated 1234 times: Connection unavailable
```

The count is the number of additional calls suppressed since the original
message or previous summary, not a cumulative total. Each summary includes the
original text so it remains unambiguous with interleaved messages. Counts are
flushed when the interval expires (even if no more messages arrive), on cache
eviction, and before changing the log file, format, or service identity.

| Environment variable | Default | Description |
|---|---|---|
| AIRGAP_LOG_REPEAT_INTERVAL | 10000 | Summary interval in milliseconds; `0` disables suppression |

For example, set `AIRGAP_LOG_REPEAT_INTERVAL=30000` before starting any Go
service. The logging package reads this shared environment variable once at
process startup; no application configuration or call-site changes are needed.
Negative, fractional, overflowing, empty, and nonnumeric values fail startup
with an explicit error. An unset variable uses the default.

The map holds at most 1024 entries; the least recently seen entry is evicted
when full, flushing its pending count first. Entries idle for one interval
expire automatically. Pending counts can be lost if the process exits before
the next summary; callers needing an explicit final flush can use
`logging.Logger.FlushRepeated()`.

Only records with the same body, severity, and ordered semantic attributes are
combined. Changing IDs, retry counts, or remote ports therefore keeps records
distinct. FATAL records and
mandatory production-configuration warnings are never suppressed. Direct
third-party text output (including Sarama's historical text writer) bypasses
the application logger and is not suppressed.

OTLP repeat records preserve the original body, event name, severity, resource,
and semantic attributes. The integer attribute `airgap.repeat.count` gives the
number of suppressed calls represented by that record and is only present when
it differs from `1`. Its absence means one occurrence, including on the initial
record. For example, the initial record followed by a repeat record with count
`5` represents six calls. Timestamps and integrity stamps are fresh for each
emitted record. Text output retains the human-readable summary above.
Repeat records participate in hashing, chaining, and signed checkpoints just
like ordinary records. Suppressed calls are not separate integrity records.

Log suppression is not a connection-attempt rate limit. TCP reconnects use
`tcpRetryInterval` (default 1000 ms), including reconnects across messages and
concurrent senders. See [Transport Configuration](Transport%20Configuration.md#tcp-retry-configuration).

## Log integrity

### Integrity format versions

The implemented hash-chain and signed-anchor format is **Air-gap Log Integrity
Format Version 1**. In this document, “Version 1” means the complete combination
of:

- the `airgap-log-chain-v1` genesis and chain-link encoding;
- body-only record hashes;
- an additional anchor record after every configured number of application
  records;
- the `airgap-log-checkpoint-v1` signed-checkpoint encoding;
- the Version 1 integrity attributes listed below; and
- optional Ed25519 signatures over the checkpoint hash.

Version 1 is identified cryptographically by its domain-separation strings and
the literal `v1` in the signed checkpoint statement. A verifier must reject an
unknown domain or version instead of attempting best-effort verification.

Every stamped record also states its format explicitly in
`airgap.log.integrity.version`, which carries `v1`. A verifier therefore reads
the version from the record and selects its rules from it, instead of having to
know out of band which version wrote the log. The same value is framed into the
signed checkpoint statement, so the attribute and the signature cannot drift
apart: rewriting the attribute makes a verifier apply rules that then fail,
rather than letting it silently downgrade.

The attribute is deliberately outside every hash input. Record hashes cover the
body only, and the checkpoint statement frames the version explicitly, so logs
written before the attribute existed keep verifying unchanged. A verifier must
treat a missing `airgap.log.integrity.version` as Version 1, because no other
version was ever emitted, and must reject any value it does not implement.

Version 1 does not emit `airgap.log.record.schema`. Version 1 records and
signatures must remain verifiable after a future format is added; a verifier
must never reinterpret a Version 1 record using another version's rules.

| Option | Environment variable | Default | Description |
|---|---|---|---|
| hash | *PREFIX*_LOG_HASH | none | `none`, `SHA256`, `SHA384`, `SHA512`, `SHA3-256`, `SHA3-384`, `SHA3-512` |
| hash-chain | *PREFIX*_LOG_HASH_CHAIN | 0 | Records per anchor. `0` disables chaining |
| log-signing-cert | *PREFIX*_LOG_SIGNING_CERT | none | PEM certificate for the dedicated Ed25519 log-signing key |
| log-signing-key | *PREFIX*_LOG_SIGNING_KEY | none | PEM PKCS#8 private key matching `log-signing-cert` |
| log-signer-id | *PREFIX*_LOG_SIGNER_ID | certificate URI SAN | Stable machine or signing-service identity |
| log-signing-eku | *PREFIX*_LOG_SIGNING_EKU | none | Optional custom EKU OID required on the certificate |

Example:

```properties
logFormat=otel
hash=SHA256
hash-chain=10
log-signing-cert=/etc/airgap/log-signing.crt
log-signing-key=/etc/airgap/log-signing.key
log-signer-id=urn:airgap:node:site-a:stream-017
```

Signing is enabled only when both signing files are configured. The certificate
must be a dedicated Ed25519 certificate with `digitalSignature` key usage.
`log-signing-eku` can require an organization-specific extended-key-usage OID.
Exactly one URI SAN is required. If `log-signer-id` is omitted, that URI is
used as the signer identity; if it is configured, it must exactly match the
certificate URI SAN.

The signing options and signed checkpoints are currently implemented in the
shared Go logger used by the Go binaries. Java dedup currently supports the
OTLP semantic layout but does not yet emit hash-chain or signature attributes.

### Installing and rotating a signing certificate

The service does not need to issue its own certificate. An organization CA can
issue an Ed25519 leaf certificate with `digitalSignature`, exactly one URI SAN
for the signer identity, and the optional EKU required by local policy. A
typical CA request starts with a locally generated PKCS#8 key and CSR:

```sh
openssl genpkey -algorithm ED25519 -out log-signing.key
openssl req -new \
  -key log-signing.key \
  -out log-signing.csr \
  -subj '/CN=Air-gap log signer stream-017' \
  -addext 'subjectAltName=URI:urn:airgap:node:site-a:stream-017' \
  -addext 'keyUsage=digitalSignature'
```

After the CA returns `log-signing.crt`, install the files with restrictive
ownership and update the service configuration atomically:

```sh
sudo python3 scripts/configure-log-signing.py \
  --config /etc/airgap/upstream.properties \
  --certificate /etc/airgap/pki/log-signing.crt \
  --key /etc/airgap/pki/log-signing.key \
  --signer-id urn:airgap:node:site-a:stream-017 \
  --eku 1.3.6.1.4.1.55555.29.1
```

At initial setup, restart the service after reviewing the configuration. To
rotate a running service, point the same command at the replacement
certificate and key and add either `--pid PID` or
`--pid-file /run/airgap-upstream.pid`. The script writes the changed
configuration with `fsync` plus an atomic rename and then sends `SIGHUP`.

The service validates the complete candidate before changing keys. It keeps
the old signer on any error. For a valid changed certificate it logs a
`log.integrity.signing_certificate.rotate` STATUS document, immediately closes
that window with an old-key signature, activates the new key, and sends the
same JSON document through the role's downstream STATUS path. The document
contains the new leaf certificate in its hashed body, so the old signature
authenticates the exact replacement certificate. Reinstalling the unchanged
certificate is a no-op.

### Version 1 record hash

The hash input is exactly the UTF-8 byte sequence of the fully formatted log
message stored in `body.stringValue`. For example, `Logger.Infof("count=%d", 3)`
hashes the seven bytes of `count=3`. This happens before JSON escaping. The
timestamp, severity, resource attributes, record attributes, and the rest of
the OTLP envelope are not hash input.

Consequently, JSON object field order in the OTLP envelope is irrelevant: the
envelope is never serialized and hashed. If the message body itself contains
JSON, however, its original bytes are hashed without parsing or
canonicalization. Reordering fields, changing whitespace, or changing escape
spelling inside that embedded JSON changes the digest even when a JSON parser
would consider the two objects semantically equivalent.

Hashing only the message body is deliberate. A Collector, or any other
processor between the file and its final destination, may add resource or
semantic attributes without invalidating the body hash. The chain separately
protects the original event timestamp and severity because those fields are
material evidence and must not be rewritten after emission.

### Version 1 chain

A hash on its own proves very little. Anyone who can edit a record can recompute
its hash. The chain is what makes edits expensive, because each record binds the
one before it:

```python
chain[k] = H(
    "airgap-log-chain-v1" || 0x00
    || chain[k-1]
    || seq[k]
    || timeUnixNano[k]
    || severityNumber[k]
    || frame(severityText[k])
    || payloadHash[k]
)
```

`seq` and `timeUnixNano` are 8-byte big-endian integers, `severityNumber` is a
4-byte big-endian integer, and `frame` is a 4-byte big-endian byte length
followed by the UTF-8 severity text. These fields are part of the chain input,
so reordering, deletion, timestamp edits, and severity edits are detectable
even when two records have identical bodies, which is common in logs.

### Air-gap Log Integrity Profile for OpenTelemetry Version 1

This subsection is normative for records declaring
`airgap.log.integrity.version=v1`. Implementations must construct exactly the
bytes described here; JSON serialization, map order, locale, platform byte
order, and Unicode normalization must not affect the result.

The profile uses these primitives:

| Primitive | Encoding |
|---|---|
| `UTF8(value)` | The exact UTF-8 bytes present in the OTLP string; no normalization or trailing NUL |
| `U32(value)` | Four-byte unsigned big-endian integer |
| `U64(value)` | Eight-byte unsigned big-endian integer |
| `FRAME(bytes)` | `U32(len(bytes)) || bytes` |
| `H(bytes)` | The algorithm named by `airgap.log.hash.alg` |

Version 1 requires a non-negative OTLP `timeUnixNano` that fits in 64 bits and a
non-negative `severityNumber` that fits in 32 bits. The payload and chain values
are:

```text
payloadHash = H(UTF8(body.stringValue))

chainInput =
    UTF8("airgap-log-chain-v1") || 0x00
    || chainPrevious
    || U64(airgap.log.seq)
    || U64(timeUnixNano)
    || U32(severityNumber)
    || FRAME(UTF8(severityText))
    || payloadHash

chainValue = H(chainInput)
```

`chainPrevious`, `payloadHash`, and `chainValue` are raw digest bytes in this
formula. Their OTLP attributes use lowercase hexadecimal without a prefix.
The domain's terminating `0x00` is a separator and is not part of its UTF-8
text. The body hash deliberately remains independently useful; timestamp and
severity protection comes from the chain.

The Version 1 protection boundary is:

| Data | Protection |
|---|---|
| `body.stringValue` | Directly protected by `airgap.log.hash`, then transitively by the chain and signed checkpoint |
| `airgap.log.seq`, `timeUnixNano`, `severityNumber`, `severityText` | Directly protected by each chain step |
| Previous chain value and record ordering | Directly protected by each chain step |
| Integrity version, algorithms, run/segment/window IDs, service name and instance, process start, anchor range, signer and key IDs | Protected by the signed checkpoint statement |
| `observedTimeUnixNano`, EventName, TraceId, SpanId, TraceFlags, semantic attributes, `service.version`, and other resource attributes | Outside Version 1; use them to find evidence, then confirm protected fields and body |

Changing an unprotected search attribute cannot manufacture a valid body,
chain, or signature, but Version 1 does not prove that the attribute itself is
original.

The chain starts from a genesis value bound to this particular process run:

```python
genesis = H( "airgap-log-chain-v1" || 0x00 || "genesis" || 0x00 || serviceName || 0x00 || instanceID || 0x00 || processStartNanos )
```

Binding the genesis to the service identity and start time stops a chain segment
taken from one process from being spliced into the log of another.

Changing a record now means recomputing every chain value after it. That is
still possible for an attacker who holds the whole file, which is why the chain
is a foundation rather than a complete answer, but it rules out the cheap edit.

### Version 1 anchors

Every `hash-chain` records the framework writes one extra record, an **anchor**.
Its body is exactly this constant string, with no range appended:

```text
airgap log chain anchor
```

The covered range is carried in the `airgap.log.anchor.from` and
`airgap.log.anchor.to` attributes rather than in the body, which keeps the
anchor body byte-identical in every window. A verifier must hash the constant
alone; appending the range produces a different digest and a spurious failure.

The anchor carries the chain value for the window it closes, and the anchor is
itself hashed and chained like any other record. That is the point of the
design: an anchor transitively commits to every record in its window, so when
signing is added it is enough to sign the anchors rather than every single
record. One signature per `hash-chain` records covers the lot.

Anchor windows tile the stream without gaps or overlaps. Records written after
the last anchor are chained but not yet anchored, which is normal at the tail of
a running process.

## Semantic attributes

A log record has two audiences. The body is written for a human reading a
terminal. The attributes are written for a machine, and the goal is that a
consumer can classify, filter and alert on a record **without parsing the
body**.

Every record emitted by an air-gap binary therefore carries the OpenTelemetry
LogRecord **EventName** field, serialized as `eventName` in OTLP/JSON, and
carries whatever further attributes make the event machine-readable.

### Where they appear

Semantic attributes are emitted in `otel` output only. Text output is the
historical, human-readable format, and it stays byte-identical, which is what
allows OTLP to be opt-in without changing any existing installation. Integrity
attributes keep their existing behaviour and are appended as trailing
`key=value` pairs in text output when hashing is enabled.

### The call API

Every log method takes an attribute list as its first argument, built with
`logging.Event`. The event name is mandatory because `Event` is the only
constructor the log methods accept.

```go
logging.Logger.Info(logging.Event(logging.EventAppStart),
    "Starting upstream")

logging.Logger.Errorf(logging.Event(logging.EventKafkaConnect,
    logging.Str("server.address", broker),
).Merge(logging.Err(err)),
    "Failed to connect to Kafka: %v", err)
```

Dedup uses the equivalent Java API:

```java
Log.info(LOG, Attrs.event(Events.APP_START)
        .with("airgap.run.id", RUN_ID),
        "Starting PartitionDedupApp {} ... runId={}", APP_VERSION, RUN_ID);

Log.warn(LOG, Attrs.event(Events.KAFKA_CONNECT).exception(error),
        "Kafka connectivity check failed", error);
```

The Java wrapper also adds `code.namespace`, `log.message.template`, and typed
`airgap.event.argument.N` attributes. Explicit domain attributes remain
preferred for identifiers such as topic, partition, offset, configuration
setting, or server address. Kafka and other third-party records that do not use
the wrapper are still formatted by the same layout and receive a classified
fallback `eventName`.

Helpers:

| Helper | Produces |
|---|---|
| `logging.Event(name, extra...)` | LogRecord EventName plus any further attributes |
| `logging.Str(k, v)` | string attribute |
| `logging.Int(k, v)` | int attribute |
| `logging.Decimal(k, v)` | decimal identifier encoded as a string |
| `logging.Bool(k, v)` | bool attribute |
| `logging.Failure()` | `error.type=_OTHER`, unless a specific type is supplied |
| `logging.Error(type)` | a specific, low-cardinality `error.type` |
| `logging.ErrorType(err)` / `logging.Err(err)` | `error.type` derived from a Go error |
| `logging.Exception(value)` | panic `error.type` and `exception.*` attributes |

A call site cannot set an attribute whose key begins with `airgap.log.`. Those
keys belong to the integrity machinery, and a call site must not be able to
forge a hash, a sequence number or a chain value. Such attributes are dropped
before the record is encoded. Duplicate keys are dropped as well, keeping the
first occurrence, because the OTel log data model requires keys to be unique
within a record.

### Required fields and attributes

| Field or attribute | Type | When | Description |
|---|---|---|---|
| EventName (`eventName`) | string field | Always | What kind of event this is, from the catalogue in `src/logging/events.go` |
| `service.version` | string resource attribute | Always | Build version of the emitting binary |
| `error.type` | string attribute | Failed operations | Predictable, low-cardinality failure class; absent on success |

Recovery is a distinct event, such as `kafka.connection.restored`, rather than
a boolean result on an ordinary event. Actual exceptions use an EventName with
an `.exception` suffix and carry `exception.type`, `exception.message` when
available, and `exception.stacktrace`. Ordinary Go errors are not exceptions
and use `error.type` without `exception.*`.

### The event name catalogue

EventName values are hierarchical, from the general domain to the specific
action, so a consumer can match on a prefix such as `kafka.` without
enumerating every event. The vocabulary is closed and defined as constants in
`src/logging/events.go`; a new kind of event gets a constant there rather than
a literal at the call site.

| Prefix | Covers |
|---|---|
| `app.lifecycle.` | Startup, readiness, shutdown, fatal failure |
| `config.` | Loading, parsing, validating and overriding configuration |
| `kafka.` | Connecting, consuming, producing, committing, rebalancing |
| `udp.` | Binding, sending and receiving across the diode |
| `tls.` | Handshakes and certificate pinning |
| `crypto.` | Key loading, payload encryption and decryption |
| `protocol.` | Encoding, decoding and the message cache |
| `message.` | Filtering and delivery |
| `gap.` | Gap detection, reporting and closure |
| `resend.` / `create.` | Resend files: creation, reading and replay |
| `log.integrity.` | Hash chain anchors |

### Domain attributes

Beyond the required three, a call site adds whatever makes the event
self-describing. Use an OpenTelemetry semantic convention attribute where one
exists, and the `airgap.` namespace otherwise.

| Attribute | Example use |
|---|---|
| `server.address`, `server.port` | Kafka broker, UDP peer |
| `messaging.destination.name` | Kafka topic |
| `messaging.consumer.group.name` | Consumer group |
| `messaging.destination.partition.id` | Partition identifier, encoded as a string |
| `messaging.kafka.offset` | Offset |
| `file.path` | Configuration file, key file, resend file |
| `error.type` | Predictable, low-cardinality failure class |
| `exception.type`, `exception.message`, `exception.stacktrace` | Actual language exception detail |
| `airgap.config.setting`, `airgap.config.value` | Which setting, and what it was set to |
| `airgap.message.count` | How many messages an event concerns |

## Attribute reference

All integrity data is carried as OTel log record attributes, so nothing about
the record shape is non-standard and any OTLP consumer can read it.

| Attribute | Type | Description |
|---|---|---|
| airgap.log.integrity.version | string | Integrity format that wrote the record, currently `v1`. Absent on logs written before the attribute existed, which are Version 1 |
| airgap.log.hash.alg | string | Hash algorithm, for example `SHA256` |
| airgap.log.hash | string | Hex digest of the record body |
| airgap.log.seq | int | Sequence number, unique and monotonic for the life of the process |
| airgap.log.chain | string | Hex chain value for this record |
| airgap.log.chain.prev | string | Chain value of the preceding record |
| airgap.log.chain.restart | bool | Present and `true` on the first record of a new chain segment |
| airgap.log.anchor | bool | Present and `true` on an anchor record |
| airgap.log.anchor.from | int | First sequence number the anchor covers. Anchor records only |
| airgap.log.anchor.to | int | Last sequence number the anchor covers, which is the anchor itself. Anchor records only |
| airgap.log.chain.length | int | Configured records per anchor. Anchor records only |
| airgap.log.checkpoint.hash | string | Hash of the canonical signed checkpoint statement |
| airgap.log.signature | string | Base64 Ed25519 signature over the checkpoint hash |
| airgap.log.signature.alg | string | Signature algorithm, currently `Ed25519` |
| airgap.log.signer.id | string | Configured or certificate-derived signer identity |
| airgap.log.key.id | string | `sha256:` fingerprint of DER SubjectPublicKeyInfo |
| airgap.log.certificate.serial | string | Signing certificate serial number |
| airgap.log.process.start_unix_nano | int | Process start time bound into a signed Version 1 checkpoint |
| airgap.log.run.id | string | Random identity for one process execution. Hashed records |
| airgap.log.segment.id | string | Random identity for one uninterrupted chain segment. Chained records |
| airgap.log.window.id | string | Random identity shared by all records and the anchor in one Version 1 window. Chained records |
| airgap.log.signature.error | string | Explicit signing failure detail in place of a valid signature |

### Declared restarts

Options are applied in the order they appear in the configuration file, so a
record written between `hash=SHA256` and `hash-chain=10` is hashed but not yet
chained. When the chain then starts, that first chained record carries
`airgap.log.chain.restart=true`.

This matters for verification. A verifier must be able to tell a restart that
the framework declared from a silent break caused by someone removing records.
An undeclared break is a failure; a declared one is not. A restart appearing
anywhere other than near startup is itself worth investigating.

## Illustrative chain-link verification

The following partial example checks body hashes and individual chain links. It
is not a complete file-integrity verifier:

```python
import json, hashlib

DOMAIN = b"airgap-log-chain-v1"

def attr_value(v):
    if "stringValue" in v: return v["stringValue"]
    if "intValue" in v:    return str(v["intValue"])
    if "boolValue" in v:   return bool(v["boolValue"])

previous = None
for line in open("upstream.log"):
    if not line.startswith("{"):
        continue
    record = json.loads(line)["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]
    attrs = {a["key"]: attr_value(a["value"]) for a in record.get("attributes", [])}
    if "airgap.log.chain" not in attrs:
        continue

    body = record["body"]["stringValue"]
    seq = int(attrs["airgap.log.seq"])

    assert hashlib.sha256(body.encode()).hexdigest() == attrs["airgap.log.hash"], f"body edited at {seq}"

    if previous is not None and attrs.get("airgap.log.chain.restart") is not True:
        assert attrs["airgap.log.chain.prev"] == previous, f"records removed or reordered before {seq}"

    h = hashlib.sha256()
    h.update(DOMAIN)
    h.update(b"\x00")
    h.update(bytes.fromhex(attrs["airgap.log.chain.prev"]))
    h.update(seq.to_bytes(8, "big"))
    h.update(int(record["timeUnixNano"]).to_bytes(8, "big"))
    h.update(int(record["severityNumber"]).to_bytes(4, "big"))
    severity = record["severityText"].encode()
    h.update(len(severity).to_bytes(4, "big"))
    h.update(severity)
    h.update(bytes.fromhex(attrs["airgap.log.hash"]))
    assert h.hexdigest() == attrs["airgap.log.chain"], f"chain value forged at {seq}"

    previous = attrs["airgap.log.chain"]
```

Use the digest matching `airgap.log.hash.alg` rather than hardcoding SHA-256 if
you run with a different algorithm.

A full-stream verifier with trusted process-start metadata must additionally
derive and verify each genesis value and require every anchor expected for a
completed window. Even then, an unsigned local file cannot prove that its tail
was not truncated or that an attacker with the whole file did not recompute the
chain.

Rather than extending the snippet above, use `tests/env/verify_log_chain.py`,
which rejects selectively unstamped records, verifies sequence continuity and
algorithm consistency, and validates chain links, anchor boundaries, and
exported anchor coverage. It is driven step by step in the next section. Its
first trust boundary is stated there explicitly.

## Verifying one event and its chain from Kibana

This procedure starts from a single suspicious or disputed log event in Kibana
and ends with a machine-checked verdict on that event. It needs no key
material. Signature verification is a separate, stronger step described in
[Verifying a signed window from Kibana](#verifying-a-signed-window-from-kibana).

The chain is what makes the single event meaningful. Verifying one record in
isolation only proves that its body matches its own digest, which an editor who
rewrote both would also achieve. Exporting the surrounding segment proves that
the record is still bound to its neighbours.

### Step 1: Identify the event

Open the event in **Kibana Discover** and expand it. Record these attributes:

| Attribute | Why it is needed |
|---|---|
| airgap.log.seq | Identifies the event inside the run |
| airgap.log.run.id | Scopes the export to one process execution |
| airgap.log.segment.id | Scopes the export to one uninterrupted chain segment |
| airgap.log.hash.alg | Selects the digest the verifier must use |

If the event has `airgap.log.hash` but no `airgap.log.chain`, it was hashed
before chaining was configured. Only the body hash can be checked; say so in
the finding rather than implying chain evidence.

### Step 2: Export the surrounding segment

Export the whole chain segment, not just the one event. Use the segment rather
than the window so the export survives an event that sits near a window
boundary. The ingestion pipeline requirements in the signed-window section
apply unchanged: the original OTLP line must be preserved in `event.original`,
and CSV export must not be used.

Open **Dev Tools > Console** and run, adjusting the index pattern and dropping
`.keyword` if those fields are already mapped as `keyword`:

```json
GET logs-*/_search
{
  "size": 1000,
  "track_total_hits": true,
  "_source": ["event.original"],
  "query": {
    "bool": {
      "filter": [
        {"term": {"airgap.log.run.id.keyword": "RUN-ID-FROM-DISCOVER"}},
        {"term": {"airgap.log.segment.id.keyword": "SEGMENT-ID-FROM-DISCOVER"}}
      ]
    }
  },
  "sort": [
    {"airgap.log.seq": {"order": "asc", "unmapped_type": "long"}}
  ]
}
```

Check `hits.total.value` against the returned hit count. If the total is larger
than `size`, raise `size` or page with `search_after`. A silently truncated
response looks exactly like a log with records deleted from the end, and the
verifier will correctly refuse to call it intact.

### Step 3: Save the export as JSON Lines

Use the response menu to copy the full JSON response and save it as
`segment-search-response.json`, then extract the preserved OTLP records:

```sh
jq -r '.hits.hits[]._source["event.original"]' \
  segment-search-response.json > segment.ndjson
```

Confirm the event under investigation survived the extraction:

```sh
grep -c '"airgap.log.seq"' segment.ndjson
```

### Step 4: Verify the hashes and the chain

The verifier does not infer which fields to hash from the JSON. It reads
`airgap.log.integrity.version` from the records and implements the rules for
that version, here **Air-gap Log Integrity Format Version 1**, with the exact
byte encodings and field order defined earlier in this document:

| Value being verified | Version 1 input, in order |
|---|---|
| `airgap.log.hash` | UTF-8 bytes of `body.stringValue` only |
| `airgap.log.chain` | UTF-8 `airgap-log-chain-v1`, one zero byte, previous chain bytes, `airgap.log.seq` and `timeUnixNano` as unsigned 8-byte big-endian values, `severityNumber` as unsigned 4-byte big-endian, the UTF-8 `severityText` prefixed by its unsigned 4-byte big-endian length, and the decoded `airgap.log.hash` bytes |
| Genesis value | UTF-8 `airgap-log-chain-v1`, one zero byte, UTF-8 `genesis`, one zero byte, service name, one zero byte, instance ID, one zero byte, and process-start nanoseconds as unsigned 8-byte big-endian |
| Signed checkpoint hash | The ordered and length-framed Version 1 checkpoint statement specified under [Version 1 signed checkpoints](#version-1-signed-checkpoints) |

Thus the record hash itself is **not** calculated over several fields. It
protects the message body. The chain combines that body hash with the sequence
number, timestamp, severity, and previous chain value. The signed checkpoint
then commits to the chain range and process identity.

The format version is stated by each record in
`airgap.log.integrity.version`, and is additionally separated cryptographically
by the `airgap-log-chain-v1` domain string and the `v1` framed into a signed
checkpoint. The script verifies only `v1`, rejects any other declared version
rather than reinterpreting the record with Version 1 rules, and requires one
version across the whole export. A record with no version attribute predates
the attribute and is verified as Version 1, which the output states explicitly.
It likewise rejects `airgap.log.record.schema`, which Version 1 does not emit.
`airgap.log.hash.alg` selects SHA-256, SHA-384, SHA-512 or a SHA-3 variant; it
is not the integrity format version.

For the first record in `segment.ndjson`, the script takes the exported
`airgap.log.chain.prev` as its starting boundary. It then proves every
subsequent link relative to that value. It does **not** independently derive
the process genesis: ordinary unsigned records do not carry the trusted
process-start value needed to do so. A signed Version 1 checkpoint commits to
the window's initial previous-chain value, final chain value, process identity,
and start time; use `verify_signed_anchor.py` when an independently trusted
boundary and authenticity are required.

```sh
python3 tests/env/verify_log_chain.py segment.ndjson --seq 1234
```

The script uses only the Python standard library. For a run with
`hash=SHA256` and `hash-chain=5`, a good export reports:

```text
format:     Air-gap Log Integrity Format Version 1 (airgap-log-chain-v1)
            declared by the log: yes, airgap.log.integrity.version=v1
file:       segment.ndjson
run.id:     dd5b59919b59f3f84555fd36493d5733
segment.id: 055b07a6b2057108a1804ac9c643624e
algorithm:  SHA256
records:    14 (seq 0..13), 14 chained
anchor:     covers seq 0..5: all covered records present
anchor:     covers seq 6..11: all covered records present

target seq 7 verified
  body:  'sample record 7'
  hash:  3ee54699f4f220f57326da3950d27be8b9cff68bfa3ef7c595a634ee4309d20d
  chain: 34ec5a2cd3cc200b35b0d33515c0d9ddaaa80e8777654785c647751d7ea418bf
  anchored in this export: yes

OK: every body matches its digest and every chain link is intact.
```

The exit status is `0` on success and `1` on any verification failure, so the
script can gate an automated evidence-collection job.

### Step 5: Interpret the result

The checks are layered, and the failure message identifies which layer caught
the edit. These are the observed messages for the three realistic tampering
attempts:

| What was done to the export | Reported failure |
|---|---|
| Edited a message body | `body edited at seq 7: recorded 3ee546…, computed a419e8…` |
| Edited a body and recomputed its `airgap.log.hash` | `chain value forged at seq 7: recorded 34ec5a…, computed c1e0a0…` |
| Deleted a record | `sequence gap: expected 7, found 8; records were removed or the export is incomplete` |
| Rewrote `airgap.log.integrity.version` to `v2` | `line 1 declares integrity format 'v2'; this verifier implements 'v1' only` |

So an attacker must edit the body, recompute the digest, and recompute every
chain value to the end of the segment. That is the intended cost.

Two results are not failures but must still be read carefully. A record
carrying `airgap.log.chain.restart=true` opens a new segment; the verifier
accepts it at the first record and a restart appearing anywhere else is worth
investigating. Records after the last anchor are chained but not yet anchored,
which is normal at the tail of a running process and is reported as
`anchored in this export: no`.

Finally, a pass here is a statement about internal consistency. It does not
prove the log was not truncated at the tail, nor that an attacker holding the
whole file did not recompute the entire chain. Only a verified signature over
an anchor proves that, which is the subject of the next section.

## Signatures

### Version 1 signed checkpoints

Dedicated certificate loading and Ed25519 signatures on Version 1 anchor
records are implemented. The body hash covers only the body, while every
Version 1 chain step additionally binds `timeUnixNano`, `severityNumber`, and
`severityText`. A signed anchor therefore proves the body, original event
timestamp, and severity of every record in its window. EventName, semantic
attributes, and resource attributes other than the checkpoint's service
identity remain outside the per-record commitment.

For each Version 1 anchor, the logger constructs the checkpoint statement in
this exact order:

```text
"airgap-log-checkpoint-v1" || 0x00
|| frame("v1")
|| frame(hashAlgorithm)
|| frame(signatureAlgorithm)
|| frame(signerId)
|| frame(keyId)
|| frame(runId)
|| frame(segmentId)
|| frame(windowId)
|| frame(serviceName)
|| frame(serviceInstanceId)
|| processStartUnixNanoBE64
|| anchorFromBE64
|| anchorToBE64
|| frame(anchorPreviousChain)
|| frame(anchorFinalChain)
```

`frame(value)` is a four-byte unsigned big-endian length followed by the raw
bytes. Strings are UTF-8. Sequence numbers and timestamps are unsigned
eight-byte big-endian integers. Hash and chain values are decoded from their
hex attributes before framing.

The framed `v1` is the same value the record publishes in
`airgap.log.integrity.version`, so a verifier can read the version from the
record and still reconstruct the exact signed statement. Because the version is
inside the signed bytes, altering the attribute cannot persuade a verifier to
apply different rules undetected: the checkpoint hash, and therefore the
signature, no longer matches.

The configured hash algorithm hashes this statement. Ed25519 signs the raw
checkpoint-hash bytes:

```text
checkpointHash = H(checkpointStatementV1)
signature      = Ed25519.sign(privateKey, checkpointHash)
```

### Public Version 1 test vector

The complete deterministic vector is
[`src/logging/testdata/log-integrity-v1-test-vector.json`](../src/logging/testdata/log-integrity-v1-test-vector.json).
It publishes:

- every logical chain and checkpoint field;
- the exact UTF-8 body bytes and framed severity bytes;
- the complete chain input and resulting digest;
- the complete checkpoint statement and checkpoint hash;
- a deterministic Ed25519 seed, raw public key, DER SubjectPublicKeyInfo, key
  ID, signature bytes, and Base64 signature.

The seed is intentionally public test material and must never be provisioned
as a production key. Go unit tests reconstruct the vector through the
production chain and checkpoint builders; Python verifier tests independently
reconstruct and verify the same bytes. A format change that does not update the
declared version therefore fails in both implementations.

### Relationship to existing standards

There is no OpenTelemetry standard for cryptographic log chaining or signed
checkpoints. The OTel Logs Data Model supplies the field vocabulary; this
profile defines the integrity boundary over those fields.

[RFC 5848](https://www.rfc-editor.org/rfc/rfc5848.html), Signed Syslog
Messages, was evaluated as the closest protocol. It standardizes origin
authentication, sequencing, replay resistance, missing-message detection,
signature groups, and certificate blocks, but its wire format and verification
rules are coupled to RFC 5424 syslog. Air-gap preserves OTLP LogRecords and
therefore cannot emit RFC 5848 without translating to a different log format.
Version 1 adopts the same high-level ideas of sequence-aware groups and
periodic signatures but does not claim RFC 5848 compatibility.

[RFC 8785](https://www.rfc-editor.org/rfc/rfc8785.html) JSON canonicalization
was not used because signing an entire OTLP/JSON envelope would make collector
metadata additions and permissible transport transformations invalidate the
record. Version 1 instead commits to an explicit, stable field profile with
fixed-width and length-framed encodings.

A future, explicitly versioned format may carry the checkpoint as
`COSE_Sign1` from [RFC 9052](https://www.rfc-editor.org/rfc/rfc9052.html), with
X.509 material represented according to
[RFC 9360](https://www.rfc-editor.org/rfc/rfc9360.html). That would standardize
the signature container and protected headers, but it would not replace the
record-chain profile. A verifier must never reinterpret a Version 1 checkpoint
as COSE.

The protected OTLP timestamp proves the time asserted by the signing host. It
does not prove that an independent trustworthy clock observed the checkpoint.
Deployments requiring third-party proof of existence or long-term
non-repudiation should timestamp checkpoint hashes using an RFC 3161 Time-Stamp
Authority. [RFC 3161](https://www.rfc-editor.org/rfc/rfc3161.html) tokens are a
future external evidence layer; they are not currently emitted by Air-gap.

### Signer and public-key identity

The checkpoint contains both `airgap.log.signer.id` and
`airgap.log.key.id`:

- `signer.id` is a stable inventory identity for the machine, HSM, or signing
  service. It is the answer to "who signed this?" and must not be a mutable
  display name.
- `key.id` identifies the exact key version. It is
  `sha256:` followed by the lowercase SHA-256 digest of the DER
  SubjectPublicKeyInfo representation of the public key.

Do not trust a public key merely because it is embedded in the same event as
the signature. An attacker could replace the event, key, and signature
together. The verifier instead receives a separately trusted key registry that
maps every key ID to its public key and expected signer:

```json
{
  "schema": "airgap-log-trust-v1",
  "keys": [
    {
      "key_id": "sha256:9f8c...c214",
      "signer_id": "urn:airgap:node:site-a:stream-017",
      "display_name": "Site A stream host 017",
      "algorithm": "Ed25519",
      "public_key_pem": "-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----\n",
      "not_before": "2027-01-01T00:00:00Z",
      "not_after": "2028-01-01T00:00:00Z",
      "status": "active"
    }
  ]
}
```

For hundreds of machines, provisioning should register each public key in one
signed inventory export rather than require an operator to collect keys during
an investigation. Key rotation creates a new key ID; old public keys remain in
the registry for historical verification. Revocation state must distinguish a
key that was invalid from the beginning from one revoked after a known time.
The friendly `display_name` comes from this trusted registry, not from the
event. A deployment with an internal CA may use certificates instead, but the
same rule applies: the verifier trusts the CA out of band and must validate the
certificate identity and validity at the event time.

Certificate expiry after the event does not invalidate historical evidence.
`verify_integrity_report.py` checks the leaf and CA path at the protected
anchor timestamp. If the leaf was valid then but is expired when the report is
generated, the signature remains successful and the report adds:

```text
[certificate] 🧣 expired now, but was valid at the signed event time
```

The summary remains `PASS` and includes the warning count. A certificate that
was not valid at the event timestamp still fails closed.

The reference verifier below treats the registry file as already trusted. Its
signature, distribution, audit trail, and revocation policy belong to the
provisioning system and must be verified before using the file.

Note that a signature only raises the bar as far as the key is protected. If the
signing key lives on the same host that writes the logs, an attacker who owns
that host can re-sign an edited file. Getting real value out of signatures means
thinking about where the key lives, which is a larger piece of work than the
signing itself.

## Quick start: verify a local file or a Kibana search

Use `tests/env/verify_integrity_report.py` for an event-by-event report of
record hashes, chain continuity, and signatures on completed checkpoints.
Run the following commands from the repository root.

### One-time prerequisites

```bash
python3 -m pip install -r tests/env/requirements-integrity-report.txt
```

Obtain `trusted-log-keys.json` from your independently trusted provisioning
inventory, using the registry format in
[Signer and public-key identity](#signer-and-public-key-identity).
Include all key versions needed for the period being investigated. Do not
construct trust merely by copying keys from the log under investigation.
Private signing keys are not needed for verification.

### Verify a local file

For a saved Elasticsearch search-response JSON object or a JSON array of
supported events:

```bash
python3 tests/env/verify_integrity_report.py \
  --events-file exported-events.json \
  --trust-registry trusted-log-keys.json \
  --report-file local-integrity-report.txt
```

The file reader accepts one JSON document: a search response containing
`hits.hits`, an event array, or a single event object. Events may contain the
original OTLP envelope directly or preserve it in `event.original`.
Each OTLP envelope must contain exactly one resource block, scope block, and
log record, as emitted by Air-gap. A Discover CSV or ordinary text log is not
a substitute for these structured records.

**For a native Air-gap OTLP JSON Lines log**, first wrap its envelopes in a
JSON array. This preserves the parsed field values without modifying the
original file:

```bash
python3 - application.ndjson exported-events.json <<'PY'
import json
import sys
from pathlib import Path

source, destination = map(Path, sys.argv[1:])
if source.resolve() == destination.resolve():
    raise SystemExit("Input and output paths must differ")
events = []
with source.open(encoding="utf-8") as stream:
    for number, line in enumerate(stream, 1):
        if not line.strip():
            raise SystemExit(f"{source}:{number}: blank line is not an OTLP event")
        try:
            event = json.loads(line)
        except ValueError as exc:
            raise SystemExit(f"{source}:{number}: invalid JSON: {exc}") from exc
        if not isinstance(event, dict) or "resourceLogs" not in event:
            raise SystemExit(f"{source}:{number}: expected an OTLP envelope")
        events.append(event)
if not events:
    raise SystemExit("Input contains no events")
with destination.open("x", encoding="utf-8") as stream:
    json.dump(events, stream)
    stream.write("\n")
PY
```

Then run the local verification command above. The converter refuses to
overwrite an existing output file and does not silently discard malformed,
blank, or plain-text lines. A mixed text/JSON startup log needs explicit
separation before conversion; any excluded lines remain outside the
verification claim.

All supplied events are processed; `--size` does not limit local files.
Local verification cannot fetch missing checkpoint records or rotation
evidence from Kibana. Supply complete checkpoint windows and independently
trusted keys for their signers. This path reads the whole JSON file into
memory; it is not a streaming verifier for arbitrarily large logs.

### Verify a Kibana search expression

Create a protected credentials file, for example `case-kibana.json`:

```json
{
  "kibana_url": "https://kibana.example.com",
  "api_key": "REPLACE_WITH_YOUR_API_KEY",
  "trust_registry": "trusted-log-keys.json"
}
```

Keep credentials out of version control. The registry path is resolved
relative to the credentials file. Then run:

```bash
python3 tests/env/verify_integrity_report.py \
  --credentials-file case-kibana.json \
  --index 'logs-*' \
  --query 'tags:case-1234' \
  --size 10000 \
  --report-file kibana-integrity-report.txt
```

`--query` accepts an **Elasticsearch query-string expression**, not the KQL
normally used by Discover. For EQL use `--eql` instead, for example
`--eql 'any where tags == "case-1234"'`; do not combine the two options.
Optional `--date-from` and `--date-to` bound the selected events.

`--size` limits the requested search results; choose it large enough for the
selection and inspect the report's selected-event count. A successful report
must not be interpreted as proof that an arbitrary large search was fully
exported. With trusted-key or CA configuration, the verifier additionally
retrieves complete signed windows and required certificate-rotation evidence.
Those evidence events can lie outside the selection's date bounds.
See [Verifying a signed window from Kibana](#verifying-a-signed-window-from-kibana)
for CA-based trust, ingestion requirements, and search details.

### Read the result and automate it

Both commands write the full event-by-event report to `--report-file`.
The default `--max-details 10` limits console details, **not verification**.
Always supply `--report-file` for unattended operation: otherwise a large
result can prompt for a report filename.

Exit status is `0` when all reported checks pass and `1` for verification
failures, empty results, or handled input/access errors. Command-line usage
errors from the argument parser use status `2`. Automation should treat
**any nonzero status as failure**:

```bash
if python3 tests/env/verify_integrity_report.py \
    --events-file exported-events.json \
    --trust-registry trusted-log-keys.json \
    --report-file integrity-report.txt; then
  echo "Reported integrity checks passed; review coverage and warnings."
else
  status=$?
  echo "Verification failed (exit $status); inspect the report/diagnostic output." >&2
  exit "$status"
fi
```

**PASS means the available checks succeeded, not that every event has every
protection.** Review each event's hash, chain, and signature results:

- `not present` is not necessarily a failure. The utility has no strict mode
  requiring every event to be hashed, chained, and covered by a trusted
  signature.
- Ordinary records are authenticated through signed checkpoint windows;
  they do not each carry a separate signature. Require complete, successfully
  verified windows for the events whose authenticity matters.
- An unfinished final window is not yet signed. Missing protection and
  unsigned tails must be assessed separately from successful hash checks.
- Warnings, including certificates expired now but valid at the event time,
  require review even when the overall result passes.
- Neither a local-file PASS nor a search PASS proves that every generated
  event was retained. Detecting removal of the final portion requires
  independent retained checkpoints or other completeness evidence.

Do not use the process exit code alone as an enforcement policy for
mandatory signed coverage. Retain the report, original evidence, and trusted
key-registry version together.

## Verifying a signed window from Kibana

`verify_integrity_report.py` automates the manual procedure below. The
operator's `--query` selects only the case events. When the credentials file
contains a trusted registry or CA bundle, the script obtains every selected
event's run/window identity, fetches the complete signed window, and recursively
fetches old-key-signed certificate-rotation windows when a new key is
encountered:

```json
{
  "kibana_url": "https://kibana.example.com",
  "api_key": "...",
  "ca_file": "/etc/airgap/pki/log-signing-ca.pem"
}
```

`trust_registry` may be used instead of, or together with, `ca_file`. Relative
paths are resolved beside the credentials file. Automatically fetched records
are labelled `evidence` in the report; records returned by the case query are
labelled `selected`. Every expansion query requires exact `hits.total`, and a
missing record, ambiguous rotation, trust cycle, unknown key, incomplete
window, invalid certificate, or bad signature fails closed.

Failures while resolving signing keys do not abort the report. Rotation events
can age out to cold storage or be deleted before an investigation runs. When
trust for a key cannot be established, the key stays untrusted and the
affected windows fail their signature checks with the reason and the
remediation (supply the key via `--trust-registry`, or trust the issuing CA
via `--ca-file`), while every other event is still verified and reported.
Failing closed still means what it says: evidence that cannot be
authenticated is never reported as passed. Normal use is:

```sh
python3 -m pip install -r tests/env/requirements-integrity-report.txt
python3 tests/env/verify_integrity_report.py \
  --credentials-file case-kibana.json \
  --query 'tags:case-1234' \
  --date-from 2026-10-10T10:00:00.000Z \
  --date-to 2026-10-10T12:00:00.000Z \
  --report-file case-1234-integrity.txt
```

The pinned `cryptography` and `pyOpenSSL` dependencies perform public-key
parsing, PKIX CA-path validation at the event time, and Ed25519 verification
inside the Python process. The report script does not create temporary key
files or invoke the `openssl` executable.

`--query` is an Elasticsearch **query-string** expression. It is not EQL, not
KQL, and not a Dev Tools request body. The script builds the search body
itself and substitutes the option value verbatim into the `query_string`
clause:

```json
{
  "size": 100,
  "track_total_hits": true,
  "query": {
    "bool": {
      "filter": [
        {
          "query_string": {
            "query": "VALUE OF --query"
          }
        },
        {
          "range": {
            "@timestamp": {
              "gte": "VALUE OF --date-from",
              "lte": "VALUE OF --date-to",
              "format": "strict_date_optional_time_nanos"
            }
          }
        }
      ]
    }
  },
  "sort": [
    {
      "@timestamp": {
        "order": "asc"
      }
    }
  ]
}
```

The `range` filter is added only when `--date-from` or `--date-to` is given;
each bound is optional and inclusive.

A JSON object passed to `--query` is treated as literal search text rather
than as a query. Quote values that contain spaces or reserved characters.

Operators who already have a saved EQL search can paste it into `--eql`
instead of `--query`. The two options are mutually exclusive, and both accept
`--date-from` and `--date-to`:

```sh
python3 tests/env/verify_integrity_report.py \
  --credentials-file case-kibana.json \
  --eql 'any where tags == "case-1234"' \
  --date-from 2026-10-10T10:00:00.000Z \
  --report-file case-1234-integrity.txt
```

`--eql` is sent to `<index>/_eql/search` with `@timestamp` as the timestamp
field and `event.category` as the event-category field, and the date bounds
are passed as the EQL `filter`. Sequence queries are rejected, because every
matching event must be verifiable individually. Date bounds and the selection
syntax apply only to the operator's case query: automatically fetched
evidence always uses `query_string` searches without date bounds, so a signed
window or certificate-rotation event that falls outside the operator's time
range is still acquired and verified.

TC-37 exercises both selection paths through Elasticsearch 8.15.3 and Kibana
8.15.3. A real `airgap-upstream` process produces the OTLP records, rotates
between two CA-issued Ed25519 signing certificates on `SIGHUP`, and completes
signed windows with both keys. The test indexes `event.original` plus the
searchable integrity fields, proves the case expression selects exactly one
event directly in Elasticsearch, and then calls the verifier through Kibana
with `--query --date-from` and again with a real `--eql` query.
The verifier automatically fetches the selected event's complete window and
the old-key-signed certificate-rotation window. Separate indices with changed
body, hash, and signature values prove that the real query path fails closed.

The remainder of this section documents the equivalent manual process and the
index fields that automatic expansion requires.

The export procedure deliberately exports the original OTLP records rather
than a Discover CSV. CSV export can omit fields, flatten attributes, and
convert 64-bit integers, any of which makes cryptographic reconstruction
unreliable.

The ingestion pipeline should:

1. preserve each complete source JSON line in `event.original`;
2. materialize `airgap.log.run.id`, `airgap.log.window.id`, and
   `airgap.log.seq` as searchable fields;
3. map IDs and hashes as `keyword`;
4. map sequence fields as `long` or `unsigned_long`; and
5. never truncate `event.original`.

To collect a window:

1. Open the target event in **Kibana Discover**.
2. Copy `airgap.log.run.id` and `airgap.log.window.id`. Also note
   `airgap.log.signer.id` and `airgap.log.key.id` when the target is the final
   Version 1 anchor record. For an earlier record, those two values will be on
   the anchor returned by the window query.
3. Open **Dev Tools > Console** and query the exact run and window. Adjust the
   index pattern and omit `.keyword` if the fields are already mapped as
   `keyword`:

```json
GET logs-*/_search
{
  "size": 100,
  "track_total_hits": true,
  "_source": ["event.original"],
  "query": {
    "bool": {
      "filter": [
        {"term": {"airgap.log.run.id.keyword": "RUN-ID-FROM-DISCOVER"}},
        {"term": {"airgap.log.window.id.keyword": "WINDOW-ID-FROM-DISCOVER"}}
      ]
    }
  },
  "sort": [
    {"airgap.log.seq": {"order": "asc", "unmapped_type": "long"}}
  ]
}
```

4. Require exactly the number of hits stated by the checkpoint range. More
   than one checkpoint, duplicate sequence numbers, or a missing sequence is a
   verification failure, not a partial success.
5. Use the response menu to copy the complete JSON response and save it as
   `window-search-response.json`.
6. Extract the preserved OTLP records as JSON Lines:

```sh
jq -r '.hits.hits[]._source["event.original"]' \
  window-search-response.json > window.ndjson
```

7. Obtain the independently trusted registry containing the checkpoint's
   `airgap.log.key.id`, save it as `trusted-log-keys.json`, and verify the
   implemented Version 1 anchor format:

```sh
python3 -m pip install cryptography
python3 tests/env/verify_signed_anchor.py window.ndjson trusted-log-keys.json
```

The Version 1 verifier is also available as
`tests/env/verify_signed_anchor.py`. It rejects unsupported fields and
algorithms rather than silently weakening verification. It proves that the
exported source records form the signed window. It does not prove that Kibana
displayed every indexed record in the wider stream, nor can it authenticate an
incomplete window that has no checkpoint yet.

## Limitations

**The body hash does not cover semantic attributes.** Its input is the body
bytes alone, which keeps it reproducible and independent of OTLP encoding. The
Version 1 chain additionally protects `timeUnixNano`, `severityNumber`, and
`severityText`, but EventName and other semantic attributes remain outside the
commitment. A consumer that relies on those unprotected attributes must treat
them as search aids and confirm the protected body when evidence matters.

**Records written before the configuration is read are not stamped.** The
version banner and the "Reading configuration from file" line are emitted before
the framework knows what `hash` is set to. This is inherent and already true for
`logLevel` and `logFileName`.

**Direct writes to `StdLogger` bypass the chain.** Sarama is routed through the
application logger in OTel mode and is therefore encoded and stamped like other
application records. Any other code that writes directly to the exposed
`StdLogger` writer still bypasses the chain.

**The chain proves integrity, not completeness, at the tail.** Truncating the
file removes the end of the chain, and nothing inside the file reveals how long
it should have been. Anchors shipped off the host as they are produced close
that gap; a local file alone cannot.
