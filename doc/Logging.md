# Logging

All Go binaries (`upstream`, `downstream`, `resend`, `create`, `gaps`,
`multiDownstream`) share one logging framework. It can write plain text, which
is the default and unchanged from earlier releases, or OpenTelemetry Logs, and
it can stamp every record with a hash and a hash chain so that later tampering
becomes detectable. Java dedup supports the same OTLP/JSON envelope through its
optional `log4j2-otel.xml` configuration; its existing `log4j2.xml`
PatternLayout remains the default.

- [Output format](#output-format)
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
Version 1 does not emit `airgap.log.record.schema`; that attribute is reserved
for the proposed canonical Version 2 record commitment.

The proposed canonical-record design is **Version 2**. Version 2 is documented
separately below and is not emitted. Version 1 records and signatures will
remain verifiable with the Version 1 verifier after a future version is added;
a verifier must never reinterpret a Version 1 record using Version 2 rules.

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
processor between the file and its final destination, is allowed to and often
will rewrite metadata, add resource attributes, or re-render the timestamp.
Those legitimate rewrites do not invalidate the hash, while an edit to what the
log actually said does.

### Version 1 chain

A hash on its own proves very little. Anyone who can edit a record can recompute
its hash. The chain is what makes edits expensive, because each record binds the
one before it:

```python
chain[k] = H( "airgap-log-chain-v1" || 0x00 || chain[k-1] || seq[k] || payloadHash[k] )
```

`seq` is an 8-byte big-endian sequence number and is part of the chain input, so
reordering or deleting records is detectable even when two records have
identical bodies, which is common in logs.

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

Every `hash-chain` records the framework writes one extra record, an **anchor**:

```text
airgap log chain anchor: records 1..11
```

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

The proposed Version 2 format described below is not yet emitted. It reserves
these additional attributes:

| Attribute | Type | Description |
|---|---|---|
| airgap.log.record.schema | string | Canonical record commitment version, initially `v2` |
| airgap.log.checkpoint | bool | Present and `true` on the last application record in a completed window |
| airgap.log.checkpoint.from | int | First sequence number in the window |
| airgap.log.checkpoint.to | int | Last sequence number in the window |
| airgap.log.checkpoint.first_prev | string | Chain value immediately before the first record in the window |
| airgap.log.checkpoint.final_chain | string | Final chain value for the window |
| airgap.log.checkpoint.hash | string | Hex digest of the canonical checkpoint statement |
| airgap.log.signature.alg | string | Signature algorithm, initially `Ed25519` |
| airgap.log.signature | string | Base64-encoded signature over the checkpoint hash |
| airgap.log.signer.id | string | Stable identity of the machine or signing service |
| airgap.log.key.id | string | Immutable identifier of the public key |

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
    h.update(bytes.fromhex(attrs["airgap.log.hash"]))
    assert h.hexdigest() == attrs["airgap.log.chain"], f"chain value forged at {seq}"

    previous = attrs["airgap.log.chain"]
```

Use the digest matching `airgap.log.hash.alg` rather than hardcoding SHA-256 if
you run with a different algorithm.

A complete verifier must additionally reject selectively unstamped records,
verify sequence continuity, derive and verify each genesis value, enforce
algorithm consistency, validate every anchor boundary and coverage interval,
and require every anchor expected for a completed window. Even then, an
unsigned local file cannot prove that its tail was not truncated or that an
attacker with the whole file did not recompute the chain.

## Signatures

### Version 1 signed checkpoints

Dedicated certificate loading and Ed25519 signatures on Version 1 anchor
records are implemented. The Version 1 chain hashes only the body and
therefore cannot prove that EventName, severity, timestamp, semantic
attributes, or resource identity were unchanged. The signature does bind the
anchor range, chain values, service identity, process start, signer identity,
key identity, and hash algorithm.

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

The configured hash algorithm hashes this statement. Ed25519 signs the raw
checkpoint-hash bytes:

```text
checkpointHash = H(checkpointStatementV1)
signature      = Ed25519.sign(privateKey, checkpointHash)
```

The Version 2 record commitment described below remains the next format
version. It will add canonical hashes for those semantic fields before changing
the checkpoint format.

### Proposed Version 2 windows

In Version 2, `hash-chain=10` means that ten application records form one
window. The tenth record carries the checkpoint and signature; the framework
does not add an eleventh anchor record. A random `airgap.log.window.id` is
allocated before the first record and copied to all ten records, so a search
does not have to infer window boundaries from timestamps or sequence
arithmetic.

The Version 2 record hash uses a versioned, length-prefixed binary encoding
rather than JSON serialization. It commits to the run, segment, window,
sequence, original timestamp, severity number, EventName, body, service
identity, and all source semantic attributes. Attribute keys are sorted by
their UTF-8 bytes and values retain their OTel type. `airgap.log.*` attributes
are excluded to avoid a circular hash. Collector or Logstash enrichment
outside the original LogRecord is not committed.

The final record carries a canonical checkpoint statement that binds:

- record schema and hash algorithm;
- signature algorithm, signer identity, and key identity;
- run, segment, and window identities;
- service name, instance, and version;
- process start time;
- first and last sequence number;
- the chain value preceding the window; and
- the final chain value.

The checkpoint hash is the configured hash of that statement. The signature is
made over the checkpoint hash, not merely over the last payload hash:

```text
checkpointHash = H(canonicalCheckpointStatement)
signature      = Ed25519.sign(privateKey, checkpointHash)
```

This lets one signature authenticate every record in the completed window.
Records in the current incomplete window remain provisionally chained until
their checkpoint is produced.

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

The reference verifier below treats the registry file as already trusted. Its
signature, distribution, audit trail, and revocation policy belong to the
provisioning system and must be verified before using the file.

Note that a signature only raises the bar as far as the key is protected. If the
signing key lives on the same host that writes the logs, an attacker who owns
that host can re-sign an edited file. Getting real value out of signatures means
thinking about where the key lives, which is a larger piece of work than the
signing itself.

## Verifying a signed window from Kibana

The export procedure applies to both implemented Version 1 signed anchors and
the proposed Version 2 format. It deliberately exports the original OTLP
records rather than a Discover CSV. CSV export can omit
fields, flatten attributes, and convert 64-bit integers, any of which makes
cryptographic reconstruction unreliable.

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
algorithms rather than silently weakening verification. The following embedded
reference script documents the proposed Version 2 canonical-record verifier and
is not compatible with Version 1 output:

```python
import base64
import hashlib
import json
import struct
import sys
from datetime import datetime, timezone

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

RECORD_DOMAIN = b"airgap-log-record-v2\x00"
CHAIN_DOMAIN = b"airgap-log-chain-v2\x00"
CHECKPOINT_DOMAIN = b"airgap-log-checkpoint-v1\x00"


def fail(message):
    raise ValueError(message)


def frame(value):
    if isinstance(value, str):
        value = value.encode("utf-8")
    return len(value).to_bytes(4, "big") + value


def u32(value):
    return int(value).to_bytes(4, "big", signed=False)


def u64(value):
    return int(value).to_bytes(8, "big", signed=False)


def digest(name, value):
    algorithms = {
        "SHA256": "sha256",
        "SHA384": "sha384",
        "SHA512": "sha512",
        "SHA3-256": "sha3_256",
        "SHA3-384": "sha3_384",
        "SHA3-512": "sha3_512",
    }
    if name not in algorithms:
        fail(f"unsupported hash algorithm: {name}")
    return hashlib.new(algorithms[name], value).digest()


def any_value(value):
    if set(value) == {"stringValue"}:
        return b"s" + frame(value["stringValue"])
    if set(value) == {"intValue"}:
        return b"i" + int(value["intValue"]).to_bytes(8, "big", signed=True)
    if set(value) == {"boolValue"}:
        return b"b" + (b"\x01" if value["boolValue"] else b"\x00")
    if set(value) == {"doubleValue"}:
        return b"f" + struct.pack(">d", float(value["doubleValue"]))
    if set(value) == {"bytesValue"}:
        return b"y" + frame(base64.b64decode(value["bytesValue"], validate=True))
    fail(f"unsupported OTLP AnyValue: {value}")


def attribute_map(items):
    result = {}
    for item in items:
        key = item["key"]
        if key in result:
            fail(f"duplicate attribute: {key}")
        result[key] = item["value"]
    return result


def scalar(attributes, key):
    if key not in attributes:
        fail(f"missing attribute: {key}")
    value = attributes[key]
    if "stringValue" in value:
        return value["stringValue"]
    if "intValue" in value:
        return int(value["intValue"])
    if "boolValue" in value:
        return bool(value["boolValue"])
    fail(f"attribute has unsupported scalar type: {key}")


def unpack_envelope(envelope):
    resource_logs = envelope.get("resourceLogs", [])
    if len(resource_logs) != 1:
        fail("each line must contain exactly one resourceLogs entry")
    resource = attribute_map(
        resource_logs[0].get("resource", {}).get("attributes", [])
    )
    scope_logs = resource_logs[0].get("scopeLogs", [])
    if len(scope_logs) != 1 or len(scope_logs[0].get("logRecords", [])) != 1:
        fail("each line must contain exactly one LogRecord")
    record = scope_logs[0]["logRecords"][0]
    return resource, record, attribute_map(record.get("attributes", []))


def text_attribute(attributes, key):
    value = scalar(attributes, key)
    if not isinstance(value, str):
        fail(f"attribute must be a string: {key}")
    return value


def canonical_semantic_attributes(attributes):
    semantic = [
        (key, value)
        for key, value in attributes.items()
        if not key.startswith("airgap.log.")
    ]
    semantic.sort(key=lambda item: item[0].encode("utf-8"))
    return u32(len(semantic)) + b"".join(
        frame(key) + any_value(value) for key, value in semantic
    )


def canonical_record(resource, record, attributes):
    body = record.get("body", {})
    if set(body) != {"stringValue"}:
        fail("v2 requires a string body")
    return b"".join(
        [
            RECORD_DOMAIN,
            frame(text_attribute(attributes, "airgap.log.run.id")),
            frame(text_attribute(attributes, "airgap.log.segment.id")),
            frame(text_attribute(attributes, "airgap.log.window.id")),
            u64(scalar(attributes, "airgap.log.seq")),
            u64(record["timeUnixNano"]),
            u32(record["severityNumber"]),
            frame(record["eventName"]),
            frame(body["stringValue"]),
            frame(text_attribute(resource, "service.name")),
            frame(text_attribute(resource, "service.instance.id")),
            frame(text_attribute(resource, "service.version")),
            canonical_semantic_attributes(attributes),
        ]
    )


def canonical_checkpoint(resource, attributes):
    return b"".join(
        [
            CHECKPOINT_DOMAIN,
            frame(text_attribute(attributes, "airgap.log.record.schema")),
            frame(text_attribute(attributes, "airgap.log.hash.alg")),
            frame(text_attribute(attributes, "airgap.log.signature.alg")),
            frame(text_attribute(attributes, "airgap.log.signer.id")),
            frame(text_attribute(attributes, "airgap.log.key.id")),
            frame(text_attribute(attributes, "airgap.log.run.id")),
            frame(text_attribute(attributes, "airgap.log.segment.id")),
            frame(text_attribute(attributes, "airgap.log.window.id")),
            frame(text_attribute(resource, "service.name")),
            frame(text_attribute(resource, "service.instance.id")),
            frame(text_attribute(resource, "service.version")),
            u64(scalar(attributes, "airgap.log.process.start_unix_nano")),
            u64(scalar(attributes, "airgap.log.checkpoint.from")),
            u64(scalar(attributes, "airgap.log.checkpoint.to")),
            frame(bytes.fromhex(text_attribute(
                attributes, "airgap.log.checkpoint.first_prev"
            ))),
            frame(bytes.fromhex(text_attribute(
                attributes, "airgap.log.checkpoint.final_chain"
            ))),
        ]
    )


def load_records(path):
    records = []
    with open(path, encoding="utf-8") as source:
        for line_number, line in enumerate(source, 1):
            if not line.strip():
                continue
            try:
                records.append(unpack_envelope(json.loads(line)))
            except (KeyError, TypeError, ValueError) as error:
                fail(f"{path}:{line_number}: {error}")
    if not records:
        fail("the export contains no records")
    return records


def parse_time(value):
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        fail(f"registry time has no timezone: {value}")
    return parsed


def trusted_key(path, key_id, signer_id, signing_time_unix_nano):
    with open(path, encoding="utf-8") as source:
        registry = json.load(source)
    if registry.get("schema") != "airgap-log-trust-v1":
        fail("unsupported trust-registry schema")
    matches = [key for key in registry.get("keys", []) if key["key_id"] == key_id]
    if len(matches) != 1:
        fail(f"trusted registry has {len(matches)} entries for {key_id}")
    entry = matches[0]
    if entry["signer_id"] != signer_id:
        fail("checkpoint signer does not match the trusted key owner")
    if entry["algorithm"] != "Ed25519":
        fail("registry entry is not an Ed25519 verification key")
    if entry.get("status") not in {"active", "retired"}:
        fail("key is revoked or has an unsupported registry status")
    signing_time = datetime.fromtimestamp(
        int(signing_time_unix_nano) / 1_000_000_000, timezone.utc
    )
    if not (
        parse_time(entry["not_before"])
        <= signing_time
        < parse_time(entry["not_after"])
    ):
        fail("checkpoint was signed outside the key validity interval")
    key = serialization.load_pem_public_key(entry["public_key_pem"].encode())
    if not isinstance(key, Ed25519PublicKey):
        fail("registry entry is not an Ed25519 public key")
    der = key.public_bytes(
        serialization.Encoding.DER,
        serialization.PublicFormat.SubjectPublicKeyInfo,
    )
    actual_id = "sha256:" + hashlib.sha256(der).hexdigest()
    if actual_id != key_id:
        fail("key ID does not match the registered public key")
    return key, entry


def verify(records, registry_path):
    records.sort(key=lambda item: int(scalar(item[2], "airgap.log.seq")))
    checkpoints = [
        item for item in records
        if item[2].get("airgap.log.checkpoint", {}).get("boolValue") is True
    ]
    if len(checkpoints) != 1 or checkpoints[0] is not records[-1]:
        fail("window must end in exactly one checkpoint")

    resource, checkpoint_record, checkpoint = checkpoints[0]
    expected = {
        "schema": "v2",
        "run": text_attribute(checkpoint, "airgap.log.run.id"),
        "segment": text_attribute(checkpoint, "airgap.log.segment.id"),
        "window": text_attribute(checkpoint, "airgap.log.window.id"),
        "algorithm": text_attribute(checkpoint, "airgap.log.hash.alg"),
    }
    if text_attribute(checkpoint, "airgap.log.record.schema") != expected["schema"]:
        fail("unsupported record schema")

    start = int(scalar(checkpoint, "airgap.log.checkpoint.from"))
    end = int(scalar(checkpoint, "airgap.log.checkpoint.to"))
    sequences = [int(scalar(item[2], "airgap.log.seq")) for item in records]
    if sequences != list(range(start, end + 1)):
        fail(f"missing, duplicate, or unexpected sequence: {sequences}")

    previous = bytes.fromhex(text_attribute(
        checkpoint, "airgap.log.checkpoint.first_prev"
    ))
    for item_resource, record, attributes in records:
        for key in ["service.name", "service.instance.id", "service.version"]:
            if text_attribute(item_resource, key) != text_attribute(resource, key):
                fail(f"inconsistent resource attribute: {key}")
        for key, expected_value in [
            ("airgap.log.record.schema", expected["schema"]),
            ("airgap.log.run.id", expected["run"]),
            ("airgap.log.segment.id", expected["segment"]),
            ("airgap.log.window.id", expected["window"]),
            ("airgap.log.hash.alg", expected["algorithm"]),
        ]:
            if text_attribute(attributes, key) != expected_value:
                fail(f"inconsistent {key}")

        sequence = int(scalar(attributes, "airgap.log.seq"))
        if bytes.fromhex(text_attribute(
            attributes, "airgap.log.chain.prev"
        )) != previous:
            fail(f"broken chain before sequence {sequence}")

        record_hash = digest(
            expected["algorithm"],
            canonical_record(item_resource, record, attributes),
        )
        if record_hash.hex() != text_attribute(attributes, "airgap.log.hash"):
            fail(f"record commitment mismatch at sequence {sequence}")

        chain = digest(
            expected["algorithm"],
            CHAIN_DOMAIN
            + frame(expected["run"])
            + frame(expected["segment"])
            + u64(sequence)
            + frame(previous)
            + frame(record_hash),
        )
        if chain.hex() != text_attribute(attributes, "airgap.log.chain"):
            fail(f"chain mismatch at sequence {sequence}")
        previous = chain

    if previous.hex() != text_attribute(
        checkpoint, "airgap.log.checkpoint.final_chain"
    ):
        fail("final chain does not match the checkpoint")

    checkpoint_hash = digest(
        expected["algorithm"], canonical_checkpoint(resource, checkpoint)
    )
    if checkpoint_hash.hex() != text_attribute(
        checkpoint, "airgap.log.checkpoint.hash"
    ):
        fail("checkpoint hash mismatch")

    signature_algorithm = text_attribute(
        checkpoint, "airgap.log.signature.alg"
    )
    if signature_algorithm != "Ed25519":
        fail(f"unsupported signature algorithm: {signature_algorithm}")
    key_id = text_attribute(checkpoint, "airgap.log.key.id")
    signer_id = text_attribute(checkpoint, "airgap.log.signer.id")
    key, registry_entry = trusted_key(
        registry_path,
        key_id,
        signer_id,
        checkpoint_record["timeUnixNano"],
    )
    signature = base64.b64decode(
        text_attribute(checkpoint, "airgap.log.signature"), validate=True
    )
    try:
        key.verify(signature, checkpoint_hash)
    except InvalidSignature:
        fail("invalid checkpoint signature")
    print(
        f"verified sequences {start}..{end}; "
        f"signer={signer_id}; "
        f"name={registry_entry.get('display_name', signer_id)}; "
        f"key={key_id}"
    )


if __name__ == "__main__":
    try:
        if len(sys.argv) != 3:
            fail("usage: verify_signed_window.py WINDOW.ndjson TRUSTED-KEYS.json")
        verify(load_records(sys.argv[1]), sys.argv[2])
    except (KeyError, TypeError, ValueError) as error:
        raise SystemExit(f"verification failed: {error}") from error
```

The script proves that the exported source records form the signed window. It
does not prove that Kibana displayed every indexed record in the wider stream,
nor can it authenticate an incomplete window that has no checkpoint yet.

## Limitations

**The hash covers the body, not the attributes.** Everything in [Semantic
attributes](#semantic-attributes) sits outside the integrity hash and outside
the chain. The hash input is defined as the body bytes alone, which is what
keeps it reproducible and independent of OTLP encoding. The body still contains
the formatted message, so nothing an operator reads is unprotected, but a
consumer that relies only on attributes is relying on data the chain does not
cover. Extending the hash to a canonical encoding of the attributes belongs
with the signing work, where the extra format commitment can be made once.

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
