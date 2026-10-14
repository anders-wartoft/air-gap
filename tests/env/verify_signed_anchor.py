#!/usr/bin/env python3
"""Verify one current air-gap signed anchor window exported as OTLP JSON Lines."""

import base64
import hashlib
import json
import sys
from pathlib import Path

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

CHAIN_DOMAIN = b"airgap-log-chain-v1\x00"
CHECKPOINT_DOMAIN = b"airgap-log-checkpoint-v1\x00"


def fail(message):
    raise ValueError(message)


def frame(value):
    if isinstance(value, str):
        value = value.encode()
    return len(value).to_bytes(4, "big") + value


def attr_map(items):
    result = {}
    for item in items:
        if item["key"] in result:
            fail(f"duplicate attribute: {item['key']}")
        result[item["key"]] = item["value"]
    return result


def value(attrs, key):
    if key not in attrs:
        fail(f"missing attribute: {key}")
    item = attrs[key]
    if "stringValue" in item:
        return item["stringValue"]
    if "intValue" in item:
        return int(item["intValue"])
    if "boolValue" in item:
        return item["boolValue"]
    fail(f"unsupported attribute value: {key}")


def text(attrs, key):
    result = value(attrs, key)
    if not isinstance(result, str):
        fail(f"attribute is not a string: {key}")
    return result


def digest(algorithm, data):
    names = {
        "SHA256": "sha256",
        "SHA384": "sha384",
        "SHA512": "sha512",
        "SHA3-256": "sha3_256",
        "SHA3-384": "sha3_384",
        "SHA3-512": "sha3_512",
    }
    if algorithm not in names:
        fail(f"unsupported hash algorithm: {algorithm}")
    return hashlib.new(names[algorithm], data).digest()


def records(path):
    result = []
    for line_number, line in enumerate(Path(path).read_text().splitlines(), 1):
        if not line.strip():
            continue
        try:
            envelope = json.loads(line)
            resource_logs = envelope["resourceLogs"]
            if len(resource_logs) != 1:
                fail("expected one resourceLogs entry")
            resource = attr_map(resource_logs[0]["resource"]["attributes"])
            scopes = resource_logs[0]["scopeLogs"]
            if len(scopes) != 1 or len(scopes[0]["logRecords"]) != 1:
                fail("expected one LogRecord")
            record = scopes[0]["logRecords"][0]
            result.append((resource, record, attr_map(record.get("attributes", []))))
        except (KeyError, TypeError, ValueError) as error:
            fail(f"{path}:{line_number}: {error}")
    if not result:
        fail("no records found")
    return result


def verify_key(registry_path, key_id, signer_id):
    registry = json.loads(Path(registry_path).read_text())
    if registry.get("schema") != "airgap-log-trust-v1":
        fail("unsupported trust registry schema")
    entries = [item for item in registry["keys"] if item["key_id"] == key_id]
    if len(entries) != 1:
        fail(f"expected one registry entry for {key_id}")
    entry = entries[0]
    if entry["signer_id"] != signer_id or entry["algorithm"] != "Ed25519":
        fail("trusted key does not match the checkpoint signer")
    if entry.get("status") not in {"active", "retired"}:
        fail("trusted key is not active or retired")
    key = serialization.load_pem_public_key(entry["public_key_pem"].encode())
    if not isinstance(key, Ed25519PublicKey):
        fail("trusted key is not Ed25519")
    der = key.public_bytes(
        serialization.Encoding.DER,
        serialization.PublicFormat.SubjectPublicKeyInfo,
    )
    actual_id = "sha256:" + hashlib.sha256(der).hexdigest()
    if actual_id != key_id:
        fail("key ID does not match the registered public key")
    return key


def verify(path, registry_path):
    items = sorted(records(path), key=lambda item: int(value(item[2], "airgap.log.seq")))
    anchors = [
        item for item in items
        if value(item[2], "airgap.log.anchor") is True
    ]
    if len(anchors) != 1 or anchors[0] is not items[-1]:
        fail("export must contain exactly one final anchor")

    resource, _, anchor = anchors[0]
    run_id = text(anchor, "airgap.log.run.id")
    segment_id = text(anchor, "airgap.log.segment.id")
    window_id = text(anchor, "airgap.log.window.id")
    algorithm = text(anchor, "airgap.log.hash.alg")
    first = int(value(anchor, "airgap.log.anchor.from"))
    last = int(value(anchor, "airgap.log.anchor.to"))
    sequences = [int(value(item[2], "airgap.log.seq")) for item in items]
    if sequences != list(range(first, last + 1)):
        fail(f"sequence gap or duplicate: {sequences}")

    previous = bytes.fromhex(text(items[0][2], "airgap.log.chain.prev"))
    anchor_previous = bytes.fromhex(text(anchor, "airgap.log.chain.prev"))
    for index, (item_resource, record, attrs) in enumerate(items):
        if (
            text(attrs, "airgap.log.run.id") != run_id
            or text(attrs, "airgap.log.segment.id") != segment_id
            or text(attrs, "airgap.log.window.id") != window_id
            or text(attrs, "airgap.log.hash.alg") != algorithm
        ):
            fail("inconsistent window identity or hash algorithm")
        body = record["body"]["stringValue"]
        payload_hash = digest(algorithm, body.encode())
        if payload_hash.hex() != text(attrs, "airgap.log.hash"):
            fail(f"payload hash mismatch at sequence {value(attrs, 'airgap.log.seq')}")
        chain_previous = bytes.fromhex(text(attrs, "airgap.log.chain.prev"))
        if chain_previous != previous:
            fail(f"chain predecessor mismatch at sequence {value(attrs, 'airgap.log.seq')}")
        if index == len(items) - 1 and chain_previous != anchor_previous:
            fail("anchor does not follow the final record in the window")
        sequence = int(value(attrs, "airgap.log.seq")).to_bytes(8, "big")
        chain = digest(
            algorithm,
            CHAIN_DOMAIN + chain_previous + sequence + payload_hash,
        )
        if chain.hex() != text(attrs, "airgap.log.chain"):
            fail(f"chain mismatch at sequence {value(attrs, 'airgap.log.seq')}")
        previous = chain

    if previous.hex() != text(anchor, "airgap.log.chain"):
        fail("final chain mismatch")

    statement = b"".join(
        [
            CHECKPOINT_DOMAIN,
            b"\x00",
            frame("v1"),
            frame(algorithm),
            frame(text(anchor, "airgap.log.signature.alg")),
            frame(text(anchor, "airgap.log.signer.id")),
            frame(text(anchor, "airgap.log.key.id")),
            frame(run_id),
            frame(segment_id),
            frame(window_id),
            frame(text(resource, "service.name")),
            frame(text(resource, "service.instance.id")),
            int(value(anchor, "airgap.log.process.start_unix_nano")).to_bytes(8, "big"),
            first.to_bytes(8, "big"),
            last.to_bytes(8, "big"),
            frame(anchor_previous),
            frame(previous),
        ]
    )
    checkpoint_hash = digest(algorithm, statement)
    if checkpoint_hash.hex() != text(anchor, "airgap.log.checkpoint.hash"):
        fail("checkpoint hash mismatch")
    key = verify_key(
        registry_path,
        text(anchor, "airgap.log.key.id"),
        text(anchor, "airgap.log.signer.id"),
    )
    try:
        key.verify(
            base64.b64decode(text(anchor, "airgap.log.signature"), validate=True),
            checkpoint_hash,
        )
    except InvalidSignature as error:
        fail(f"signature verification failed: {error}")
    print(
        f"verified sequences {first}..{last}; "
        f"signer={text(anchor, 'airgap.log.signer.id')}; "
        f"key={text(anchor, 'airgap.log.key.id')}"
    )


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: verify_signed_anchor.py WINDOW.ndjson TRUSTED-KEYS.json")
    try:
        verify(sys.argv[1], sys.argv[2])
    except (KeyError, TypeError, ValueError) as error:
        raise SystemExit(f"verification failed: {error}") from error
