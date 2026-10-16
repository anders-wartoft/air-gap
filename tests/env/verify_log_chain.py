#!/usr/bin/env python3
"""Verify air-gap Version 1 body hashes and hash-chain links for an exported log.

The input is OTLP JSON Lines, one ExportLogsServiceRequest envelope per line,
as produced by logFormat=otel or extracted from a Kibana search response.

Unlike verify_signed_anchor.py, this script needs no key material. It proves
that the exported records are internally consistent: every body matches its
recorded digest and every chain value follows from its predecessor. It cannot
prove that the tail was not truncated, nor that someone holding the whole file
did not recompute the chain. Only a signed anchor proves that.
"""

import argparse
import hashlib
import json
import sys

FORMAT_VERSION = "v1"
FORMAT_NAME = "Air-gap Log Integrity Format Version 1"
CHAIN_DOMAIN = b"airgap-log-chain-v1"
ANCHOR_BODY = "airgap log chain anchor"
VERSION_ATTRIBUTE = "airgap.log.integrity.version"
RECORD_SCHEMA_ATTRIBUTE = "airgap.log.record.schema"

HASH_ALGORITHMS = {
    "SHA256": "sha256",
    "SHA384": "sha384",
    "SHA512": "sha512",
    "SHA3-256": "sha3_256",
    "SHA3-384": "sha3_384",
    "SHA3-512": "sha3_512",
}


class VerificationError(Exception):
    """A record failed verification, or the export is not usable as evidence."""


def fail(message):
    raise VerificationError(message)


def digest(algorithm, data):
    if algorithm not in HASH_ALGORITHMS:
        fail(f"unsupported hash algorithm: {algorithm}")
    return hashlib.new(HASH_ALGORITHMS[algorithm], data).digest()


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


def optional(attributes, key, default=None):
    if key not in attributes:
        return default
    return scalar(attributes, key)


def unpack(envelope):
    resource_logs = envelope.get("resourceLogs", [])
    if len(resource_logs) != 1:
        fail("each line must contain exactly one resourceLogs entry")
    resource = attribute_map(resource_logs[0].get("resource", {}).get("attributes", []))
    scope_logs = resource_logs[0].get("scopeLogs", [])
    if len(scope_logs) != 1 or len(scope_logs[0].get("logRecords", [])) != 1:
        fail("each line must contain exactly one LogRecord")
    record = scope_logs[0]["logRecords"][0]
    return resource, record, attribute_map(record.get("attributes", []))


def load(path):
    records = []
    with open(path, encoding="utf-8") as handle:
        for number, line in enumerate(handle, start=1):
            line = line.strip()
            if not line:
                continue
            if not line.startswith("{"):
                fail(f"line {number} is not a JSON object; export OTLP, not CSV")
            try:
                envelope = json.loads(line)
            except json.JSONDecodeError as error:
                fail(f"line {number} is not valid JSON: {error}")
            resource, record, attributes = unpack(envelope)
            if RECORD_SCHEMA_ATTRIBUTE in attributes:
                schema = scalar(attributes, RECORD_SCHEMA_ATTRIBUTE)
                fail(
                    f"line {number} declares unsupported record schema "
                    f"{schema!r}; this verifier accepts only Version 1, "
                    "which does not emit airgap.log.record.schema"
                )
            declared = optional(attributes, VERSION_ATTRIBUTE)
            if declared is not None and declared != FORMAT_VERSION:
                fail(
                    f"line {number} declares integrity format {declared!r}; "
                    f"this verifier implements {FORMAT_VERSION!r} only. Use the "
                    "verifier for that format rather than reinterpreting the "
                    "record with Version 1 rules"
                )
            if "airgap.log.hash" not in attributes:
                fail(f"line {number} carries no airgap.log.hash; "
                     "a selectively unstamped record cannot be verified")
            records.append({
                "line": number,
                "resource": resource,
                "body": record.get("body", {}).get("stringValue"),
                "time_unix_nano": record.get("timeUnixNano"),
                "severity_number": record.get("severityNumber"),
                "severity_text": record.get("severityText"),
                "attributes": attributes,
                "declared_version": declared,
                "seq": scalar(attributes, "airgap.log.seq"),
            })
    if not records:
        fail("no log records found in export")
    return records


def check_homogeneous(records):
    """A chain is only meaningful inside one run, segment, version and algorithm."""
    for key, label in ((VERSION_ATTRIBUTE, "integrity format version"),
                       ("airgap.log.run.id", "run"),
                       ("airgap.log.segment.id", "segment"),
                       ("airgap.log.hash.alg", "hash algorithm")):
        values = {optional(record["attributes"], key) for record in records}
        if len(values) != 1:
            fail(f"export mixes more than one {label}: {sorted(map(str, values))}")
    return (
        optional(records[0]["attributes"], "airgap.log.run.id"),
        optional(records[0]["attributes"], "airgap.log.segment.id"),
        scalar(records[0]["attributes"], "airgap.log.hash.alg"),
    )


def check_sequence(records):
    """Reject duplicates and gaps, which hide deletions and replays."""
    seen = {}
    for record in records:
        if record["seq"] in seen:
            fail(f"duplicate sequence number {record['seq']} on lines "
                 f"{seen[record['seq']]} and {record['line']}")
        seen[record["seq"]] = record["line"]
    records.sort(key=lambda record: record["seq"])
    expected = records[0]["seq"]
    for record in records:
        if record["seq"] != expected:
            fail(f"sequence gap: expected {expected}, found {record['seq']}; "
                 "records were removed or the export is incomplete")
        expected += 1


def verify_body_hash(record, algorithm):
    body = record["body"]
    if body is None:
        fail(f"record {record['seq']} has no string body to hash")
    recorded = scalar(record["attributes"], "airgap.log.hash")
    computed = digest(algorithm, body.encode("utf-8")).hex()
    if computed != recorded:
        fail(f"body edited at seq {record['seq']}: "
             f"recorded {recorded}, computed {computed}")
    return computed


def verify_chain_link(record, algorithm, previous_chain):
    attributes = record["attributes"]
    if "airgap.log.chain" not in attributes:
        return None

    previous = bytes.fromhex(scalar(attributes, "airgap.log.chain.prev"))
    restart = optional(attributes, "airgap.log.chain.restart", False)

    if previous_chain is not None and not restart and previous != previous_chain:
        fail(f"broken chain before seq {record['seq']}: records were removed, "
             "reordered, or an undeclared restart occurred")

    if record["time_unix_nano"] is None:
        fail(f"record {record['seq']} has no timeUnixNano")
    if record["severity_number"] is None or record["severity_text"] is None:
        fail(f"record {record['seq']} has incomplete severity metadata")
    severity_text = str(record["severity_text"]).encode("utf-8")
    computed = digest(algorithm, b"".join([
        CHAIN_DOMAIN,
        b"\x00",
        previous,
        int(record["seq"]).to_bytes(8, "big"),
        int(record["time_unix_nano"]).to_bytes(8, "big", signed=False),
        int(record["severity_number"]).to_bytes(4, "big"),
        len(severity_text).to_bytes(4, "big"),
        severity_text,
        bytes.fromhex(scalar(attributes, "airgap.log.hash")),
    ]))

    recorded = scalar(attributes, "airgap.log.chain")
    if computed.hex() != recorded:
        fail(f"chain value forged at seq {record['seq']}: "
             f"recorded {recorded}, computed {computed.hex()}")
    return computed


def verify_anchor(record, records_by_seq, algorithm):
    """An anchor transitively commits to every record in its window."""
    attributes = record["attributes"]
    if record["body"] != ANCHOR_BODY:
        fail(f"record {record['seq']} claims to be an anchor but its body is "
             f"{record['body']!r}, not {ANCHOR_BODY!r}")

    first = scalar(attributes, "airgap.log.anchor.from")
    last = scalar(attributes, "airgap.log.anchor.to")
    if last != record["seq"]:
        fail(f"anchor {record['seq']} claims to end at {last}; an anchor must "
             "close the window with its own sequence number")
    if first > last:
        fail(f"anchor {record['seq']} covers an empty range {first}..{last}")

    covered = [seq for seq in range(first, last + 1) if seq in records_by_seq]
    if len(covered) != last - first + 1:
        return first, last, False
    return first, last, True


def verify(path, target_seq=None):
    records = load(path)
    run_id, segment_id, algorithm = check_homogeneous(records)
    check_sequence(records)
    records_by_seq = {record["seq"]: record for record in records}

    chain = None
    chained = 0
    anchors = []
    for record in records:
        verify_body_hash(record, algorithm)
        result = verify_chain_link(record, algorithm, chain)
        if result is not None:
            chain = result
            chained += 1
        if optional(record["attributes"], "airgap.log.anchor", False):
            anchors.append(verify_anchor(record, records_by_seq, algorithm))

    print(f"format:     {FORMAT_NAME} ({CHAIN_DOMAIN.decode()})")
    if records[0]["declared_version"] is None:
        print("            declared by the log: no, assumed; written before "
              f"{VERSION_ATTRIBUTE} existed")
    else:
        print(f"            declared by the log: yes, {VERSION_ATTRIBUTE}="
              f"{records[0]['declared_version']}")
    print(f"file:       {path}")
    print(f"run.id:     {run_id}")
    print(f"segment.id: {segment_id}")
    print(f"algorithm:  {algorithm}")
    print(f"records:    {len(records)} "
          f"(seq {records[0]['seq']}..{records[-1]['seq']}), {chained} chained")

    for first, last, complete in anchors:
        state = "all covered records present" if complete else "WINDOW INCOMPLETE"
        print(f"anchor:     covers seq {first}..{last}: {state}")
        if not complete:
            fail(f"anchor window {first}..{last} is missing records; "
                 "the export does not cover the whole window")

    if target_seq is not None:
        if target_seq not in records_by_seq:
            fail(f"seq {target_seq} is not present in the export")
        target = records_by_seq[target_seq]
        print()
        print(f"target seq {target_seq} verified")
        print(f"  body:  {target['body']!r}")
        print(f"  hash:  {scalar(target['attributes'], 'airgap.log.hash')}")
        print(f"  chain: {optional(target['attributes'], 'airgap.log.chain', 'not chained')}")
        anchored = any(first <= target_seq <= last for first, last, _ in anchors)
        print(f"  anchored in this export: {'yes' if anchored else 'no'}")

    print()
    print("OK: every body matches its digest and every chain link is intact.")
    print("Note: this proves internal consistency only. Without a verified "
          "signed anchor it does not prove the log was not truncated or "
          "wholly recomputed.")


def main():
    parser = argparse.ArgumentParser(
        description="Verify air-gap Version 1 log body hashes and chain links.")
    parser.add_argument("export", help="OTLP JSON Lines file to verify")
    parser.add_argument("--seq", type=int, default=None,
                        help="sequence number of the event under investigation")
    arguments = parser.parse_args()

    try:
        verify(arguments.export, arguments.seq)
    except VerificationError as error:
        print(f"VERIFICATION FAILED: {error}", file=sys.stderr)
        return 1
    except OSError as error:
        print(f"cannot read export: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
