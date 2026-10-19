#!/usr/bin/env python3
"""Create an operator report for log-integrity events from Kibana or a file."""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import re
import sys
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Tuple

try:
    from cryptography import x509
    from cryptography.exceptions import InvalidSignature, UnsupportedAlgorithm
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import ed25519
    from cryptography.x509.oid import ExtensionOID
    from OpenSSL import crypto
except ImportError as exc:
    raise SystemExit(
        "verify_integrity_report.py requires the pinned dependencies in "
        "tests/env/requirements-integrity-report.txt"
    ) from exc

SUPPORTED_VERSIONS = {"v1"}
CHAIN_DOMAINS = {"v1": b"airgap-log-chain-v1"}
HASH_ALGORITHMS = {
    "SHA256": "sha256",
    "SHA384": "sha384",
    "SHA512": "sha512",
    "SHA3-256": "sha3_256",
    "SHA3-384": "sha3_384",
    "SHA3-512": "sha3_512",
}
VERSION_ATTRIBUTE = "airgap.log.integrity.version"
PASS_SYMBOL = "✅"
FAIL_SYMBOL = "❌"
CHECK_FAIL_SYMBOL = "❌"
NOT_PRESENT_SYMBOL = "🚫"
WARNING_SYMBOL = "🧣"


class VerificationFailure(RuntimeError):
    """The input cannot be interpreted or fails an integrity check."""


@dataclass
class ChainRecord:
    group: Tuple[str, str]
    seq: int
    version: str
    algorithm: str
    previous: bytes
    computed: bytes
    recorded: bytes
    restart: bool
    result: "EventResult"


@dataclass
class EventResult:
    index: int
    ok: bool = True
    lines: List[str] = field(default_factory=list)
    chain: Optional[ChainRecord] = None
    attributes: Dict[str, Any] = field(default_factory=dict)
    resource: Dict[str, Any] = field(default_factory=dict)
    body: Any = None
    selected: bool = False
    time_unix_nano: Optional[int] = None
    severity_number: Optional[int] = None
    severity_text: Optional[str] = None
    warning_count: int = 0

    def check(
        self,
        label: str,
        ok: bool,
        message: str,
        symbol: Optional[str] = None,
    ) -> None:
        check_symbol = symbol if symbol is not None else PASS_SYMBOL if ok else CHECK_FAIL_SYMBOL
        self.lines.append(f"[{label}] {check_symbol} {message}")
        self.ok = self.ok and ok

    def warn(self, label: str, message: str) -> None:
        self.lines.append(f"[{label}] {WARNING_SYMBOL} {message}")
        self.warning_count += 1


@dataclass
class TrustedKey:
    key_id: str
    signer_id: str
    public_key_pem: str
    source: str
    certificate_not_before: Optional[datetime] = None
    certificate_not_after: Optional[datetime] = None


@dataclass
class CertificateMaterial:
    public_key_pem: str
    not_before: datetime
    not_after: datetime


def digest_bytes(algorithm: str, data: bytes) -> bytes:
    name = HASH_ALGORITHMS.get(algorithm)
    if name is None:
        raise VerificationFailure(f"unsupported hash algorithm: {algorithm}")
    return hashlib.new(name, data).digest()


def value_of(attributes: Dict[str, Any], key: str) -> Any:
    if key not in attributes:
        raise VerificationFailure(f"missing attribute: {key}")
    item = attributes[key]
    if isinstance(item, dict):
        if "stringValue" in item:
            return item["stringValue"]
        if "intValue" in item:
            return int(item["intValue"])
        if "boolValue" in item:
            return bool(item["boolValue"])
        raise VerificationFailure(f"attribute has unsupported scalar type: {key}")
    return item


def optional_value(attributes: Dict[str, Any], key: str, default: Any = None) -> Any:
    if key not in attributes:
        return default
    return value_of(attributes, key)


def flatten_attributes(items: Iterable[Dict[str, Any]]) -> Dict[str, Any]:
    result: Dict[str, Any] = {}
    for item in items:
        if not isinstance(item, dict) or "key" not in item:
            raise VerificationFailure("attribute list contains an invalid item")
        key = str(item["key"])
        if key in result:
            raise VerificationFailure(f"duplicate attribute: {key}")
        result[key] = item.get("value", item)
    return result


def extract_otlp_record(envelope: Dict[str, Any]) -> Dict[str, Any]:
    resource_logs = envelope.get("resourceLogs", [])
    if len(resource_logs) != 1:
        raise VerificationFailure("expected one resourceLogs block in the OTLP envelope")
    resource_log = resource_logs[0]
    resource = flatten_attributes(resource_log.get("resource", {}).get("attributes", []))
    scopes = resource_log.get("scopeLogs", [])
    if len(scopes) != 1 or len(scopes[0].get("logRecords", [])) != 1:
        raise VerificationFailure("expected one LogRecord in the OTLP payload")
    record = scopes[0]["logRecords"][0]
    attrs = flatten_attributes(record.get("attributes", []))
    body = record.get("body", {}).get("stringValue")
    return {
        "attributes": attrs,
        "body": body,
        "resource": resource,
        "time_unix_nano": record.get("timeUnixNano"),
        "severity_number": record.get("severityNumber"),
        "severity_text": record.get("severityText"),
    }


def original_event(source: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    raw = source.get("event.original")
    if raw is None and isinstance(source.get("event"), dict):
        raw = source["event"].get("original")
    if raw is None:
        return None
    if not isinstance(raw, str):
        raise VerificationFailure("event.original is not a JSON string")
    try:
        envelope = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise VerificationFailure(f"event.original is not valid JSON: {exc}") from exc
    if not isinstance(envelope, dict):
        raise VerificationFailure("event.original does not contain a JSON object")
    return extract_otlp_record(envelope)


def first_record_from_hit(hit: Dict[str, Any]) -> Dict[str, Any]:
    if not isinstance(hit, dict):
        raise VerificationFailure(f"unsupported search hit format: {hit!r}")
    source = hit.get("_source", hit)
    if not isinstance(source, dict):
        raise VerificationFailure(f"unsupported search hit format: {hit!r}")
    if "resourceLogs" in source:
        return extract_otlp_record(source)
    preserved = original_event(source)
    if preserved is not None:
        return preserved
    if isinstance(source.get("attributes"), list):
        attrs = flatten_attributes(source["attributes"])
    else:
        attrs = {
            str(key): value
            for key, value in source.items()
            if str(key).startswith("airgap.log.")
        }
    body = source.get("body", source.get("message"))
    if isinstance(body, dict):
        body = body.get("stringValue")
    return {
        "attributes": attrs,
        "body": body,
        "time_unix_nano": source.get("timeUnixNano"),
        "severity_number": source.get("severityNumber"),
        "severity_text": source.get("severityText"),
    }


def load_credentials(path: str) -> Dict[str, Any]:
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    if not isinstance(data, dict):
        raise VerificationFailure(f"credentials file must contain a JSON object: {path}")
    if "api_key" in data or ("username" in data and "password" in data):
        return data
    raise VerificationFailure(
        "credentials file must contain either 'api_key' or both 'username' and 'password'"
    )


def load_ed25519_public_key(public_key_pem: str) -> ed25519.Ed25519PublicKey:
    try:
        public_key = serialization.load_pem_public_key(
            public_key_pem.encode("utf-8")
        )
    except (TypeError, ValueError, UnsupportedAlgorithm) as exc:
        raise VerificationFailure(f"invalid trusted public key: {exc}") from exc
    if not isinstance(public_key, ed25519.Ed25519PublicKey):
        raise VerificationFailure("trusted public key is not Ed25519")
    return public_key


def public_key_der(public_key_pem: str) -> bytes:
    return load_ed25519_public_key(public_key_pem).public_bytes(
        serialization.Encoding.DER,
        serialization.PublicFormat.SubjectPublicKeyInfo,
    )


def public_key_id(public_key_pem: str) -> str:
    return "sha256:" + hashlib.sha256(public_key_der(public_key_pem)).hexdigest()


def load_trust_registry(path: str) -> Dict[str, TrustedKey]:
    payload = json.loads(Path(path).read_text(encoding="utf-8"))
    if not isinstance(payload, dict) or payload.get("schema") != "airgap-log-trust-v1":
        raise VerificationFailure("unsupported trust registry schema")
    entries = payload.get("keys")
    if not isinstance(entries, list):
        raise VerificationFailure("trust registry keys is not a list")
    result: Dict[str, TrustedKey] = {}
    for entry in entries:
        if not isinstance(entry, dict):
            raise VerificationFailure("trust registry contains a non-object key entry")
        key_id = str(entry.get("key_id", ""))
        signer_id = str(entry.get("signer_id", ""))
        algorithm = str(entry.get("algorithm", ""))
        public_key_pem = str(entry.get("public_key_pem", ""))
        if entry.get("status") not in {"active", "retired"}:
            raise VerificationFailure(f"trusted key {key_id!r} is not active or retired")
        if algorithm != "Ed25519":
            raise VerificationFailure(f"trusted key {key_id!r} is not Ed25519")
        if not key_id or not signer_id or not public_key_pem:
            raise VerificationFailure("trust registry key entry is incomplete")
        if public_key_id(public_key_pem) != key_id:
            raise VerificationFailure(
                f"trusted key ID does not match its public key: {key_id}"
            )
        if key_id in result:
            raise VerificationFailure(f"duplicate trusted key ID: {key_id}")
        result[key_id] = TrustedKey(
            key_id=key_id,
            signer_id=signer_id,
            public_key_pem=public_key_pem,
            source=f"trust registry {path}",
        )
    return result


def certificate_valid_at(
    certificate: x509.Certificate,
    instant: datetime,
    label: str,
) -> None:
    if instant < certificate.not_valid_before_utc:
        raise VerificationFailure(
            f"{label} was not valid at event time {instant.isoformat()}"
        )
    if instant > certificate.not_valid_after_utc:
        raise VerificationFailure(
            f"{label} had expired at event time {instant.isoformat()}"
        )


def verify_certificate_chain(
    leaf: x509.Certificate,
    embedded: List[x509.Certificate],
    ca_file: str,
    event_time: datetime,
) -> None:
    try:
        trust_anchors = x509.load_pem_x509_certificates(
            Path(ca_file).read_bytes()
        )
    except (OSError, ValueError) as exc:
        raise VerificationFailure(
            f"cannot load configured CA bundle {ca_file!r}: {exc}"
        ) from exc
    if not trust_anchors:
        raise VerificationFailure("configured CA bundle contains no certificates")

    store = crypto.X509Store()
    store.set_time(event_time)
    try:
        for certificate in trust_anchors:
            store.add_cert(crypto.X509.from_cryptography(certificate))
        context = crypto.X509StoreContext(
            store,
            crypto.X509.from_cryptography(leaf),
            [
                crypto.X509.from_cryptography(certificate)
                for certificate in embedded
            ],
        )
        context.verify_certificate()
    except crypto.X509StoreContextError as exc:
        raise VerificationFailure(
            f"signing certificate is not trusted at event time: {exc}"
        ) from exc


def certificate_public_key(
    certificate_pem: str,
    expected_signer_id: str,
    expected_serial: str,
    event_time: datetime,
    ca_file: Optional[str] = None,
) -> CertificateMaterial:
    try:
        certificates = x509.load_pem_x509_certificates(
            certificate_pem.encode("utf-8")
        )
    except ValueError as exc:
        raise VerificationFailure(
            f"cannot parse announced signing certificate: {exc}"
        ) from exc
    if not certificates:
        raise VerificationFailure("announced signing certificate is empty")
    certificate = certificates[0]
    certificate_valid_at(certificate, event_time, "signing certificate")
    if ca_file:
        verify_certificate_chain(
            certificate,
            certificates[1:],
            ca_file,
            event_time,
        )
    try:
        names = certificate.extensions.get_extension_for_oid(
            ExtensionOID.SUBJECT_ALTERNATIVE_NAME
        ).value
    except x509.ExtensionNotFound as exc:
        raise VerificationFailure(
            "announced signing certificate has no subjectAltName"
        ) from exc
    signer_ids = names.get_values_for_type(x509.UniformResourceIdentifier)
    if signer_ids != [expected_signer_id]:
        raise VerificationFailure(
            "announced certificate URI SAN does not match signer ID"
        )
    if str(certificate.serial_number) != expected_serial:
        raise VerificationFailure(
            "announced certificate serial does not match STATUS"
        )
    try:
        usage = certificate.extensions.get_extension_for_oid(
            ExtensionOID.KEY_USAGE
        ).value
    except x509.ExtensionNotFound as exc:
        raise VerificationFailure(
            "announced signing certificate has no keyUsage extension"
        ) from exc
    if not usage.digital_signature:
        raise VerificationFailure(
            "announced signing certificate does not allow digitalSignature"
        )
    public_key = certificate.public_key()
    if not isinstance(public_key, ed25519.Ed25519PublicKey):
        raise VerificationFailure("announced signing certificate is not Ed25519")
    public_key_pem = public_key.public_bytes(
        serialization.Encoding.PEM,
        serialization.PublicFormat.SubjectPublicKeyInfo,
    ).decode("ascii")
    return CertificateMaterial(
        public_key_pem=public_key_pem,
        not_before=certificate.not_valid_before_utc,
        not_after=certificate.not_valid_after_utc,
    )


def certificate_pem_from_der(value: Any) -> str:
    try:
        der = base64.b64decode(str(value), validate=True)
    except ValueError as exc:
        raise VerificationFailure("airgap.log.certificate.der is not valid Base64") from exc
    encoded = base64.b64encode(der).decode("ascii")
    lines = [encoded[index : index + 64] for index in range(0, len(encoded), 64)]
    return "-----BEGIN CERTIFICATE-----\n" + "\n".join(lines) + "\n-----END CERTIFICATE-----\n"


def parse_timestamp_bound(value: str) -> datetime:
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    # datetime.fromisoformat accepts at most microsecond precision; Elasticsearch
    # accepts nanoseconds, so validate against a truncated copy.
    match = re.match(r"^(.*\.\d{6})\d+(.*)$", text)
    if match:
        text = match.group(1) + match.group(2)
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed


def validate_timestamp_bound(option: str, value: Optional[str]) -> Optional[str]:
    """Reject a malformed bound before any request is sent."""
    if value is None:
        return None
    text = value.strip()
    if not text:
        raise VerificationFailure(f"{option} must not be empty")
    try:
        parse_timestamp_bound(text)
    except ValueError as exc:
        raise VerificationFailure(
            f"{option}={value!r} is not an ISO-8601 timestamp, for example "
            "2026-10-10T10:00:00.000Z"
        ) from exc
    return text


def timestamp_range_filter(
    date_from: Optional[str],
    date_to: Optional[str],
) -> Optional[Dict[str, Any]]:
    """Build an @timestamp range filter, or None when no bound was given."""
    bounds: Dict[str, str] = {}
    if date_from:
        bounds["gte"] = date_from
    if date_to:
        bounds["lte"] = date_to
    if not bounds:
        return None
    bounds["format"] = "strict_date_optional_time_nanos"
    return {"range": {"@timestamp": bounds}}


def kibana_proxy_request(
    url: str,
    credentials: Dict[str, Any],
    path: str,
    payload: Dict[str, Any],
) -> Dict[str, Any]:
    proxy_path = urllib.parse.quote(path, safe="")
    endpoint = f"{url.rstrip('/')}/api/console/proxy?path={proxy_path}&method=POST"
    request = urllib.request.Request(
        endpoint,
        data=json.dumps(payload).encode("utf-8"),
        method="POST",
    )
    request.add_header("Content-Type", "application/json")
    request.add_header("kbn-xsrf", "true")
    if "api_key" in credentials:
        request.add_header("Authorization", f"ApiKey {credentials['api_key']}")
    else:
        token = base64.b64encode(
            f"{credentials['username']}:{credentials['password']}".encode("utf-8")
        ).decode("ascii")
        request.add_header("Authorization", f"Basic {token}")

    with urllib.request.urlopen(request, timeout=30) as response:
        result = json.loads(response.read().decode("utf-8"))
    if not isinstance(result, dict):
        raise VerificationFailure("Kibana response is not a JSON object")
    error = result.get("error")
    if error is not None:
        reason = error.get("reason") if isinstance(error, dict) else None
        raise VerificationFailure(
            f"Elasticsearch rejected the request for {path}: {reason or error}"
        )
    return result


def response_hits_object(result: Dict[str, Any]) -> Dict[str, Any]:
    search_hits = result.get("hits", {})
    if not isinstance(search_hits, dict):
        raise VerificationFailure("Kibana response does not contain a hits object")
    return search_hits


def check_complete_result(
    search_hits: Dict[str, Any],
    returned: int,
    size: int,
    description: str,
) -> None:
    """Fail closed unless the response proves every matching event was returned."""
    total_result = search_hits.get("total")
    if isinstance(total_result, int):
        total = total_result
        relation = "eq"
    elif isinstance(total_result, dict):
        total = total_result.get("value")
        relation = total_result.get("relation")
        if not isinstance(total, int) or relation not in {"eq", "gte"}:
            raise VerificationFailure(
                "Kibana response contains an invalid hits.total object"
            )
    else:
        raise VerificationFailure(
            "Kibana response does not contain hits.total; "
            "cannot prove that all matching events were returned"
        )
    if relation != "eq" or total != returned:
        total_description = str(total) if relation == "eq" else f"at least {total}"
        raise VerificationFailure(
            f"{description} matched {total_description} event(s), but returned "
            f"{returned} because --size={size}; increase --size so every "
            "matching event can be verified"
        )


def kibana_request(
    url: str,
    credentials: Dict[str, Any],
    query: str,
    index: str,
    size: int,
    date_from: Optional[str] = None,
    date_to: Optional[str] = None,
) -> List[Dict[str, Any]]:
    filters: List[Dict[str, Any]] = [{"query_string": {"query": query}}]
    range_filter = timestamp_range_filter(date_from, date_to)
    if range_filter is not None:
        filters.append(range_filter)
    payload = {
        "size": size,
        "track_total_hits": True,
        "query": {"bool": {"filter": filters}},
        "sort": [{"@timestamp": {"order": "asc"}}],
    }
    result = kibana_proxy_request(url, credentials, f"{index}/_search", payload)
    search_hits = response_hits_object(result)
    hits = search_hits.get("hits", [])
    if not isinstance(hits, list):
        raise VerificationFailure("Kibana response does not contain a hits.hits list")
    check_complete_result(search_hits, len(hits), size, "Kibana query")
    return hits


def kibana_eql_request(
    url: str,
    credentials: Dict[str, Any],
    query: str,
    index: str,
    size: int,
    date_from: Optional[str] = None,
    date_to: Optional[str] = None,
) -> List[Dict[str, Any]]:
    payload: Dict[str, Any] = {
        "query": query,
        "size": size,
        "timestamp_field": "@timestamp",
        "event_category_field": "event.category",
    }
    range_filter = timestamp_range_filter(date_from, date_to)
    if range_filter is not None:
        payload["filter"] = range_filter
    result = kibana_proxy_request(url, credentials, f"{index}/_eql/search", payload)
    if result.get("is_partial") or result.get("timed_out"):
        raise VerificationFailure(
            "Elasticsearch returned a partial or timed-out EQL result; "
            "cannot prove that all matching events were returned"
        )
    search_hits = response_hits_object(result)
    if "sequences" in search_hits:
        raise VerificationFailure(
            "--eql returned sequences; use an event query so every matching "
            "event can be verified individually"
        )
    events = search_hits.get("events", [])
    if not isinstance(events, list):
        raise VerificationFailure(
            "Elasticsearch response does not contain a hits.events list"
        )
    check_complete_result(search_hits, len(events), size, "EQL query")
    return events


def declared_version(attributes: Dict[str, Any]) -> Tuple[str, str]:
    value = optional_value(attributes, VERSION_ATTRIBUTE)
    if value is None:
        integrity_fields = any(
            key.startswith("airgap.log.")
            for key in attributes
        )
        if integrity_fields:
            return "v1", "v1 (assumed; attribute absent)"
        return "v1", "none declared (no integrity fields)"
    version = str(value)
    if version not in SUPPORTED_VERSIONS:
        raise VerificationFailure(
            f"unsupported integrity version {version!r}; "
            f"supported versions are {sorted(SUPPORTED_VERSIONS)}"
        )
    return version, version


def parse_hex(value: Any, label: str) -> bytes:
    try:
        return bytes.fromhex(str(value))
    except ValueError as exc:
        raise VerificationFailure(f"{label} is not valid hexadecimal") from exc


def record_time(time_unix_nano: int) -> datetime:
    seconds, nanoseconds = divmod(time_unix_nano, 1_000_000_000)
    return datetime.fromtimestamp(seconds, timezone.utc).replace(
        microsecond=nanoseconds // 1000
    )


def framed_text(value: str) -> bytes:
    encoded = value.encode("utf-8")
    return len(encoded).to_bytes(4, "big") + encoded


def verify_hash(attributes: Dict[str, Any], body: Any) -> Tuple[bool, str]:
    if "airgap.log.hash" not in attributes:
        hash_evidence = {
            VERSION_ATTRIBUTE,
            "airgap.log.hash.alg",
            "airgap.log.seq",
            "airgap.log.run.id",
            "airgap.log.chain",
            "airgap.log.chain.prev",
        }
        if hash_evidence.intersection(attributes):
            return False, "integrity metadata is present but airgap.log.hash is missing"
        return True, "not present"
    if not isinstance(body, str):
        raise VerificationFailure("hashed event has no string body")
    algorithm = str(optional_value(attributes, "airgap.log.hash.alg", "SHA256"))
    expected = str(value_of(attributes, "airgap.log.hash"))
    computed = digest_bytes(algorithm, body.encode("utf-8")).hex()
    if computed == expected:
        return True, f"OK ({algorithm})"
    return False, f"FAILED ({algorithm}): recorded {expected}, computed {computed}"


def build_chain_record(
    result: EventResult,
    attributes: Dict[str, Any],
    body: Any,
    version: str,
) -> Optional[ChainRecord]:
    if "airgap.log.chain" not in attributes:
        chain_evidence = {
            "airgap.log.chain.prev",
            "airgap.log.chain.restart",
            "airgap.log.segment.id",
            "airgap.log.window.id",
            "airgap.log.anchor",
            "airgap.log.anchor.from",
            "airgap.log.anchor.to",
        }
        if chain_evidence.intersection(attributes):
            result.check(
                "chain",
                False,
                "chain metadata is present but airgap.log.chain is missing",
            )
            return None
        result.check("chain", True, "not present", NOT_PRESENT_SYMBOL)
        return None
    if not isinstance(body, str):
        raise VerificationFailure("chained event has no string body")

    algorithm = str(optional_value(attributes, "airgap.log.hash.alg", "SHA256"))
    seq = int(value_of(attributes, "airgap.log.seq"))
    if seq < 0 or seq > (1 << 64) - 1:
        raise VerificationFailure(f"sequence number is outside uint64: {seq}")
    payload_hash = parse_hex(value_of(attributes, "airgap.log.hash"), "airgap.log.hash")
    previous = parse_hex(value_of(attributes, "airgap.log.chain.prev"), "airgap.log.chain.prev")
    recorded = parse_hex(value_of(attributes, "airgap.log.chain"), "airgap.log.chain")
    if result.time_unix_nano is None:
        raise VerificationFailure("chained event has no timeUnixNano")
    if result.severity_number is None:
        raise VerificationFailure("chained event has no severityNumber")
    if result.severity_text is None:
        raise VerificationFailure("chained event has no severityText")
    if result.time_unix_nano < -(1 << 63) or result.time_unix_nano > (1 << 63) - 1:
        raise VerificationFailure(
            f"timeUnixNano is outside int64: {result.time_unix_nano}"
        )
    if result.severity_number < 0 or result.severity_number > (1 << 32) - 1:
        raise VerificationFailure(
            f"severityNumber is outside uint32: {result.severity_number}"
        )
    computed = digest_bytes(
        algorithm,
        CHAIN_DOMAINS[version]
        + b"\x00"
        + previous
        + seq.to_bytes(8, "big")
        + (result.time_unix_nano & ((1 << 64) - 1)).to_bytes(8, "big")
        + result.severity_number.to_bytes(4, "big")
        + framed_text(result.severity_text)
        + payload_hash,
    )
    local_ok = computed == recorded
    result.check(
        "chain",
        local_ok,
        f"value OK ({algorithm})"
        if local_ok
        else f"value FAILED ({algorithm}): recorded {recorded.hex()}, computed {computed.hex()}",
    )
    run_id = str(value_of(attributes, "airgap.log.run.id"))
    segment_id = str(value_of(attributes, "airgap.log.segment.id"))
    return ChainRecord(
        group=(run_id, segment_id),
        seq=seq,
        version=version,
        algorithm=algorithm,
        previous=previous,
        computed=computed,
        recorded=recorded,
        restart=bool(optional_value(attributes, "airgap.log.chain.restart", False)),
        result=result,
    )


def signature_fields_present(attributes: Dict[str, Any]) -> List[str]:
    signature_fields = {
        "airgap.log.checkpoint.hash",
        "airgap.log.signature.alg",
        "airgap.log.signature",
        "airgap.log.signer.id",
        "airgap.log.key.id",
        "airgap.log.certificate.serial",
        "airgap.log.certificate.der",
        "airgap.log.signature.error",
    }
    return sorted(signature_fields.intersection(attributes))


def frame(value: bytes) -> bytes:
    return len(value).to_bytes(4, "big") + value


def frame_text(value: Any) -> bytes:
    return frame(str(value).encode("utf-8"))


def verify_ed25519(public_key_pem: str, message: bytes, signature: bytes) -> None:
    try:
        load_ed25519_public_key(public_key_pem).verify(signature, message)
    except InvalidSignature as exc:
        raise VerificationFailure("Ed25519 signature verification failed") from exc


def build_checkpoint_statement(result: EventResult) -> bytes:
    attrs = result.attributes
    version, _ = declared_version(attrs)
    algorithm = str(value_of(attrs, "airgap.log.hash.alg"))
    signature_algorithm = str(value_of(attrs, "airgap.log.signature.alg"))
    if signature_algorithm != "Ed25519":
        raise VerificationFailure(
            f"unsupported signature algorithm: {signature_algorithm}"
        )
    statement = b"airgap-log-checkpoint-v1\x00"
    statement += frame_text(version)
    statement += frame_text(algorithm)
    statement += frame_text(signature_algorithm)
    statement += frame_text(value_of(attrs, "airgap.log.signer.id"))
    statement += frame_text(value_of(attrs, "airgap.log.key.id"))
    statement += frame_text(value_of(attrs, "airgap.log.run.id"))
    statement += frame_text(value_of(attrs, "airgap.log.segment.id"))
    statement += frame_text(value_of(attrs, "airgap.log.window.id"))
    statement += frame_text(value_of(result.resource, "service.name"))
    statement += frame_text(value_of(result.resource, "service.instance.id"))
    statement += int(value_of(attrs, "airgap.log.process.start_unix_nano")).to_bytes(
        8, "big"
    )
    statement += int(value_of(attrs, "airgap.log.anchor.from")).to_bytes(8, "big")
    statement += int(value_of(attrs, "airgap.log.anchor.to")).to_bytes(8, "big")
    statement += frame(
        parse_hex(value_of(attrs, "airgap.log.chain.prev"), "airgap.log.chain.prev")
    )
    statement += frame(parse_hex(value_of(attrs, "airgap.log.chain"), "airgap.log.chain"))
    return statement


def verify_checkpoint_signature(result: EventResult, trusted: TrustedKey) -> None:
    attrs = result.attributes
    algorithm = str(value_of(attrs, "airgap.log.hash.alg"))
    statement = build_checkpoint_statement(result)
    checkpoint = digest_bytes(algorithm, statement)
    recorded = parse_hex(
        value_of(attrs, "airgap.log.checkpoint.hash"),
        "airgap.log.checkpoint.hash",
    )
    if checkpoint != recorded:
        raise VerificationFailure(
            "checkpoint hash FAILED: "
            f"recorded {recorded.hex()}, computed {checkpoint.hex()}"
        )
    try:
        signature = base64.b64decode(
            str(value_of(attrs, "airgap.log.signature")), validate=True
        )
    except ValueError as exc:
        raise VerificationFailure("airgap.log.signature is not valid Base64") from exc
    verify_ed25519(trusted.public_key_pem, checkpoint, signature)


def verify_signature(
    result: EventResult,
    trusted_keys: Dict[str, TrustedKey],
    resolution_errors: Optional[Dict[str, str]] = None,
) -> None:
    attributes = result.attributes
    present = signature_fields_present(attributes)
    if not present:
        result.check("signature", True, "not present", NOT_PRESENT_SYMBOL)
        return
    if "airgap.log.signature.error" in attributes:
        result.check(
            "signature",
            False,
            f"signer reported: {value_of(attributes, 'airgap.log.signature.error')}",
        )
        return
    try:
        key_id = str(value_of(attributes, "airgap.log.key.id"))
        signer_id = str(value_of(attributes, "airgap.log.signer.id"))
        trusted = trusted_keys.get(key_id)
        if trusted is None:
            if resolution_errors and key_id in resolution_errors:
                raise VerificationFailure(
                    f"no trusted key for {key_id}: {resolution_errors[key_id]}"
                )
            raise VerificationFailure(f"no trusted key for {key_id}")
        if trusted.signer_id != signer_id:
            raise VerificationFailure(
                f"trusted signer {trusted.signer_id!r} does not match {signer_id!r}"
            )
        if trusted.certificate_not_before is not None:
            if result.time_unix_nano is None:
                raise VerificationFailure(
                    "signed event has no timeUnixNano for certificate validity"
                )
            event_time = record_time(result.time_unix_nano)
            if event_time < trusted.certificate_not_before:
                raise VerificationFailure(
                    "signing certificate was not valid at event time "
                    f"{event_time.isoformat()}"
                )
            if (
                trusted.certificate_not_after is not None
                and event_time > trusted.certificate_not_after
            ):
                raise VerificationFailure(
                    "signing certificate had expired at event time "
                    f"{event_time.isoformat()}"
                )
        verify_checkpoint_signature(result, trusted)
        result.check("signature", True, "verified (Ed25519)")
        if (
            trusted.certificate_not_after is not None
            and datetime.now(timezone.utc) > trusted.certificate_not_after
        ):
            result.warn(
                "certificate",
                "expired now, but was valid at the signed event time "
                f"(expired {trusted.certificate_not_after.isoformat()})",
            )
    except (VerificationFailure, OSError, ValueError) as exc:
        result.check("signature", False, str(exc))


def verify_hit(index: int, hit: Dict[str, Any]) -> EventResult:
    result = EventResult(index=index)
    try:
        record = first_record_from_hit(hit)
        attributes = record.get("attributes", {})
        if not isinstance(attributes, dict):
            raise VerificationFailure("record attributes are not an object")
        body = record.get("body")
        result.attributes = attributes
        resource = record.get("resource", {})
        if isinstance(resource, dict):
            result.resource = resource
        result.body = body
        raw_time = record.get("time_unix_nano")
        raw_severity_number = record.get("severity_number")
        raw_severity_text = record.get("severity_text")
        if raw_time is not None:
            result.time_unix_nano = int(raw_time)
        if raw_severity_number is not None:
            result.severity_number = int(raw_severity_number)
        if raw_severity_text is not None:
            result.severity_text = str(raw_severity_text)
        version, version_display = declared_version(attributes)
        result.lines.append(f"version={version_display}")
        if "airgap.log.seq" in attributes:
            result.lines.append(f"seq={value_of(attributes, 'airgap.log.seq')}")
        else:
            result.lines.append("seq=unknown")
        result.lines.append(f"run.id={optional_value(attributes, 'airgap.log.run.id', 'unknown')}")
        result.lines.append(
            f"segment.id={optional_value(attributes, 'airgap.log.segment.id', 'unknown')}"
        )
        if result.time_unix_nano is not None:
            result.lines.append(
                f"timestamp={record_time(result.time_unix_nano).isoformat()}"
            )
        else:
            result.lines.append("timestamp=unknown")
        if result.severity_text is not None and result.severity_number is not None:
            result.lines.append(
                f"severity={result.severity_text}/{result.severity_number}"
            )
        else:
            result.lines.append("severity=unknown")
        result.lines.append(f"body={body!r}")

        try:
            hash_ok, hash_message = verify_hash(attributes, body)
            hash_symbol = (
                NOT_PRESENT_SYMBOL if hash_ok and hash_message == "not present" else None
            )
            result.check("hash", hash_ok, hash_message, hash_symbol)
        except (VerificationFailure, TypeError, ValueError) as exc:
            result.check("hash", False, str(exc))

        try:
            result.chain = build_chain_record(result, attributes, body, version)
        except (VerificationFailure, KeyError, OverflowError, TypeError, ValueError) as exc:
            result.check("chain", False, str(exc))

    except (VerificationFailure, KeyError, TypeError, ValueError) as exc:
        result.check("event", False, str(exc))
    return result


def verify_chain_continuity(results: List[EventResult]) -> None:
    groups: Dict[Tuple[str, str], List[ChainRecord]] = {}
    for result in results:
        if result.chain is not None:
            groups.setdefault(result.chain.group, []).append(result.chain)

    for records in groups.values():
        records.sort(key=lambda record: (record.seq, record.result.index))
        previous: Optional[ChainRecord] = None
        seen_sequences: Dict[int, ChainRecord] = {}
        for record in records:
            duplicate = seen_sequences.get(record.seq)
            if duplicate is not None:
                record.result.check(
                    "continuity",
                    False,
                    f"duplicate sequence {record.seq} in run/segment; "
                    f"also present in Event {duplicate.result.index}",
                )
            elif record.restart:
                record.result.check(
                    "continuity",
                    True,
                    "declared chain restart; previous boundary accepted",
                )
            elif previous is None:
                record.result.check(
                    "continuity",
                    True,
                    "first returned record in run/segment; previous boundary not in result set",
                )
            elif record.version != previous.version:
                record.result.check(
                    "continuity",
                    False,
                    f"integrity version changed from {previous.version} to {record.version} "
                    "without a declared restart",
                )
            elif record.algorithm != previous.algorithm:
                record.result.check(
                    "continuity",
                    False,
                    f"hash algorithm changed from {previous.algorithm} to {record.algorithm} "
                    "without a declared restart",
                )
            elif record.previous != previous.computed:
                record.result.check(
                    "continuity",
                    False,
                    f"broken chain after seq {previous.seq}: expected "
                    f"{previous.computed.hex()}, saw {record.previous.hex()}",
                )
            else:
                record.result.check(
                    "continuity",
                    True,
                    f"linked to seq {previous.seq} in the same run/segment",
                )
            seen_sequences[record.seq] = record
            previous = record


def hit_identity(hit: Dict[str, Any]) -> str:
    if isinstance(hit, dict) and "_id" in hit:
        return f"_id:{hit['_id']}"
    return json.dumps(hit, sort_keys=True, separators=(",", ":"))


def query_string_value(value: str) -> str:
    return value.replace("\\", "\\\\").replace('"', '\\"')


def validate_signed_window(
    hits: List[Dict[str, Any]],
    trusted_keys: Dict[str, TrustedKey],
) -> List[EventResult]:
    results = [verify_hit(index, hit) for index, hit in enumerate(hits, start=1)]
    verify_chain_continuity(results)
    if not results or any(not result.ok for result in results):
        raise VerificationFailure("automatically fetched signed window failed hash or chain checks")
    anchors = [
        result
        for result in results
        if optional_value(result.attributes, "airgap.log.anchor", False) is True
    ]
    if len(anchors) != 1:
        raise VerificationFailure(
            f"signed window contains {len(anchors)} anchors, expected exactly one"
        )
    anchor = anchors[0]
    first = int(value_of(anchor.attributes, "airgap.log.anchor.from"))
    last = int(value_of(anchor.attributes, "airgap.log.anchor.to"))
    sequences = sorted(
        int(value_of(result.attributes, "airgap.log.seq")) for result in results
    )
    if sequences != list(range(first, last + 1)):
        raise VerificationFailure(
            f"signed window is incomplete: expected seq {first}..{last}, got {sequences}"
        )
    verify_signature(anchor, trusted_keys)
    if not anchor.ok:
        raise VerificationFailure("signed-window anchor signature did not verify")
    return results


def expand_kibana_evidence(
    selected_hits: List[Dict[str, Any]],
    url: str,
    credentials: Dict[str, Any],
    index: str,
    size: int,
    trusted_keys: Dict[str, TrustedKey],
    ca_file: Optional[str],
) -> Tuple[List[Dict[str, Any]], int, Dict[str, str]]:
    """Fetch windows and resolve signing keys, degrading gracefully.

    Returns the deduplicated hits, the number of selected events, and a map
    of key ID to the reason its trust could not be resolved, for example
    because the certificate-rotation event aged out of Elasticsearch.
    """
    ordered: List[Dict[str, Any]] = []
    known: Dict[str, Dict[str, Any]] = {}

    def add_hits(hits: Iterable[Dict[str, Any]]) -> None:
        for hit in hits:
            identity = hit_identity(hit)
            if identity not in known:
                known[identity] = hit
                ordered.append(hit)

    add_hits(selected_hits)
    selected_count = len(ordered)
    windows: Dict[Tuple[str, str], List[Dict[str, Any]]] = {}
    for hit in selected_hits:
        record = first_record_from_hit(hit)
        attrs = record.get("attributes", {})
        if not isinstance(attrs, dict):
            continue
        run_id = optional_value(attrs, "airgap.log.run.id")
        window_id = optional_value(attrs, "airgap.log.window.id")
        if run_id is None or window_id is None:
            continue
        key = (str(run_id), str(window_id))
        if key in windows:
            continue
        query = (
            f'airgap.log.run.id:"{query_string_value(key[0])}" AND '
            f'airgap.log.window.id:"{query_string_value(key[1])}"'
        )
        window_hits = kibana_request(url, credentials, query, index, size)
        windows[key] = window_hits
        add_hits(window_hits)

    resolving: List[str] = []
    resolution_errors: Dict[str, str] = {}

    def trust_anchor_from_ca(results: Iterable[EventResult], key_id: str) -> None:
        if not ca_file or key_id in trusted_keys:
            return
        anchors = [
            result
            for result in results
            if optional_value(result.attributes, "airgap.log.anchor", False) is True
            and str(optional_value(result.attributes, "airgap.log.key.id", "")) == key_id
        ]
        if len(anchors) != 1:
            return
        anchor = anchors[0]
        certificate_der = optional_value(
            anchor.attributes, "airgap.log.certificate.der"
        )
        if certificate_der is None:
            return
        signer_id = str(value_of(anchor.attributes, "airgap.log.signer.id"))
        serial = str(value_of(anchor.attributes, "airgap.log.certificate.serial"))
        if anchor.time_unix_nano is None:
            raise VerificationFailure(
                "signed anchor has no timeUnixNano for certificate validity"
            )
        certificate = certificate_public_key(
            certificate_pem_from_der(certificate_der),
            signer_id,
            serial,
            record_time(anchor.time_unix_nano),
            ca_file,
        )
        actual_key_id = public_key_id(certificate.public_key_pem)
        if actual_key_id != key_id:
            raise VerificationFailure(
                f"CA-issued certificate key ID is {actual_key_id}, expected {key_id}"
            )
        trusted_keys[key_id] = TrustedKey(
            key_id=key_id,
            signer_id=signer_id,
            public_key_pem=certificate.public_key_pem,
            source=f"CA bundle {ca_file}",
            certificate_not_before=certificate.not_before,
            certificate_not_after=certificate.not_after,
        )

    def authorize_key(key_id: str) -> None:
        """Resolve trust for key_id without aborting the report.

        Rotation events can age out to cold storage or be deleted before an
        investigation runs. When trust cannot be established the key stays
        untrusted and the reason is recorded in resolution_errors, so the
        affected windows fail their signature checks with an explanation
        while every other event is still verified and reported.
        """
        if key_id in trusted_keys or key_id in resolution_errors:
            return
        if key_id in resolving:
            cycle = " -> ".join(resolving + [key_id])
            resolution_errors[key_id] = f"certificate-rotation trust cycle: {cycle}"
            return
        resolving.append(key_id)
        try:
            authorize_key_or_raise(key_id)
        except VerificationFailure as exc:
            resolution_errors[key_id] = str(exc)
        finally:
            resolving.pop()

    def authorize_key_or_raise(key_id: str) -> None:
        if key_id in trusted_keys:
            return
        query = (
            'airgap.log.signing.new.key.id:'
            f'"{query_string_value(key_id)}"'
        )
        rotation_hits = kibana_request(url, credentials, query, index, size)
        announcements: List[Tuple[Dict[str, Any], EventResult]] = []
        parsed = [
            verify_hit(index_number, hit)
            for index_number, hit in enumerate(rotation_hits, start=1)
        ]
        for result in parsed:
            try:
                document = json.loads(str(result.body))
            except (TypeError, json.JSONDecodeError):
                continue
            if (
                isinstance(document, dict)
                and document.get("schema")
                == "airgap-log-signing-certificate-rotation-v1"
                and document.get("new_key_id") == key_id
            ):
                announcements.append((document, result))
        if not announcements:
            raise VerificationFailure(
                f"no certificate-rotation event announcing {key_id} found in "
                "Elasticsearch (it may have aged out to cold storage or been "
                "deleted); supply the key via --trust-registry or trust the "
                "issuing CA via --ca-file"
            )
        if len(announcements) != 1:
            raise VerificationFailure(
                f"ambiguous certificate rotation: {len(announcements)} events "
                f"announce {key_id}; refusing to choose between them"
            )
        announcement, announcement_result = announcements[0]
        old_key_id = str(announcement.get("old_key_id", ""))
        if not old_key_id:
            raise VerificationFailure("certificate rotation has no old_key_id")
        trust_anchor_from_ca(parsed, old_key_id)
        authorize_key(old_key_id)
        if old_key_id not in trusted_keys:
            reason = resolution_errors.get(old_key_id)
            detail = f": {reason}" if reason else ""
            raise VerificationFailure(
                f"old signing key {old_key_id} could not be trusted{detail}"
            )
        try:
            verified = validate_signed_window(rotation_hits, trusted_keys)
        except VerificationFailure as exc:
            raise VerificationFailure(
                f"certificate-rotation window for {key_id} did not verify "
                f"({exc}); it may have partially aged out of Elasticsearch"
            ) from exc
        anchors = [
            result
            for result in verified
            if optional_value(result.attributes, "airgap.log.anchor", False) is True
        ]
        anchor_key_id = str(value_of(anchors[0].attributes, "airgap.log.key.id"))
        if anchor_key_id != old_key_id:
            raise VerificationFailure(
                f"rotation was signed by {anchor_key_id}, expected {old_key_id}"
            )
        signer_id = str(announcement.get("signer_id", ""))
        serial = str(announcement.get("new_certificate_serial", ""))
        certificate = str(announcement.get("new_certificate_pem", ""))
        if announcement_result.time_unix_nano is None:
            raise VerificationFailure(
                "certificate rotation has no timeUnixNano for certificate validity"
            )
        material = certificate_public_key(
            certificate,
            signer_id,
            serial,
            record_time(announcement_result.time_unix_nano),
        )
        actual_key_id = public_key_id(material.public_key_pem)
        if actual_key_id != key_id:
            raise VerificationFailure(
                f"announced certificate key ID is {actual_key_id}, expected {key_id}"
            )
        trusted_keys[key_id] = TrustedKey(
            key_id=key_id,
            signer_id=signer_id,
            public_key_pem=material.public_key_pem,
            source=f"old-key-signed rotation from {old_key_id}",
            certificate_not_before=material.not_before,
            certificate_not_after=material.not_after,
        )
        add_hits(rotation_hits)

    for window_hits in list(windows.values()):
        preliminary = [
            verify_hit(index_number, hit)
            for index_number, hit in enumerate(window_hits, start=1)
        ]
        for result in preliminary:
            if optional_value(result.attributes, "airgap.log.anchor", False) is True:
                key_id = str(value_of(result.attributes, "airgap.log.key.id"))
                try:
                    trust_anchor_from_ca(preliminary, key_id)
                except VerificationFailure as exc:
                    resolution_errors.setdefault(key_id, str(exc))
                authorize_key(key_id)

    return ordered, selected_count, resolution_errors


def load_events_from_file(path: str) -> List[Dict[str, Any]]:
    payload = json.loads(Path(path).read_text(encoding="utf-8"))
    if isinstance(payload, dict) and "hits" in payload:
        hits = payload["hits"].get("hits", [])
        if not isinstance(hits, list):
            raise VerificationFailure("event file hits.hits is not a list")
        return hits
    if isinstance(payload, list):
        return payload
    if isinstance(payload, dict):
        return [payload]
    raise VerificationFailure(f"unsupported event file format: {path}")


def status_symbol(ok: bool) -> str:
    return PASS_SYMBOL if ok else FAIL_SYMBOL


def overall_result(ok: bool) -> str:
    return f"Overall result: {status_symbol(ok)}"


def make_summary(results: List[EventResult]) -> str:
    passed = sum(result.ok for result in results)
    failed = len(results) - passed
    warnings = sum(result.warning_count for result in results)
    overall = failed == 0
    summary = (
        f"{status_symbol(overall)} {'PASS' if overall else 'FAIL'} "
        f"{len(results)} event(s) checked: {passed} passed, {failed} failed"
    )
    if warnings:
        summary += f", {warnings} warning(s) {WARNING_SYMBOL}"
    return summary


def detailed_report(
    results: List[EventResult],
    source: str,
    selected_count: Optional[int] = None,
) -> List[str]:
    generated = datetime.now(timezone.utc).isoformat(timespec="seconds")
    lines: List[str] = [
        "Air-gap log integrity report",
        f"generated={generated}",
        f"source={source}",
        make_summary(results),
    ]
    if selected_count is not None:
        evidence_count = len(results) - selected_count
        lines.append(
            f"selection={selected_count} selected event(s); "
            f"{evidence_count} automatically fetched evidence event(s)"
        )
    lines.append("")
    for result in results:
        role = "selected" if result.selected else "evidence"
        lines.append(f"Event {result.index}: {status_symbol(result.ok)} ({role})")
        lines.extend(result.lines)
        lines.append("")
    lines.append(overall_result(all(result.ok for result in results)))
    return lines


def write_report(report_path: str, lines: List[str]) -> None:
    path = Path(report_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def prompt_for_report_file() -> str:
    prompt = "More events matched than can be shown in detail. Full report filename: "
    if sys.stdin.isatty():
        try:
            return input(prompt).strip() or "integrity-report.txt"
        except EOFError:
            pass
    return "integrity-report.txt"


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Verify integrity fields for a Kibana search or JSON event export."
    )
    parser.add_argument(
        "--query",
        help=(
            "Elasticsearch query-string expression (not EQL, KQL, or a Dev Tools "
            "request body), for example: tags:case-123 AND service.name:airgap-upstream. "
            "Defaults to * when neither --query nor --eql is given."
        ),
    )
    parser.add_argument(
        "--eql",
        help=(
            "Event Query Language query sent to <index>/_eql/search, for example: "
            '\'any where tags == "case-123"\'. Mutually exclusive with --query.'
        ),
    )
    parser.add_argument(
        "--date-from",
        help=(
            "Earliest @timestamp to select, inclusive; ISO-8601, for example "
            "2026-10-10T10:00:00.000Z. Works with both --query and --eql."
        ),
    )
    parser.add_argument(
        "--date-to",
        help=(
            "Latest @timestamp to select, inclusive; ISO-8601, for example "
            "2026-10-10T12:00:00.000Z. Works with both --query and --eql."
        ),
    )
    parser.add_argument(
        "--index",
        default="logs-*",
        help="Elasticsearch index pattern to search (default: logs-*)",
    )
    parser.add_argument("--kibana-url", help="Kibana base URL")
    parser.add_argument(
        "--credentials-file",
        help="JSON containing api_key or username/password; may also contain kibana_url",
    )
    parser.add_argument(
        "--trust-registry",
        help=(
            "trusted airgap-log-trust-v1 registry; may also be configured as "
            "trust_registry in the credentials file"
        ),
    )
    parser.add_argument(
        "--ca-file",
        help=(
            "trusted CA bundle for signing certificates; may also be configured "
            "as ca_file in the credentials file"
        ),
    )
    parser.add_argument(
        "--events-file",
        help="Read an Elasticsearch search response or event list instead of querying Kibana",
    )
    parser.add_argument("--report-file", help="Full report path")
    parser.add_argument(
        "--max-details",
        type=int,
        default=10,
        help="Print full details for at most this many events (default: 10)",
    )
    parser.add_argument(
        "--size",
        type=int,
        default=100,
        help="Maximum number of hits requested from Kibana (default: 100)",
    )
    return parser


def main() -> int:
    args = build_parser().parse_args()
    if args.max_details < 0:
        raise VerificationFailure("--max-details must be zero or greater")
    if args.size <= 0:
        raise VerificationFailure("--size must be greater than zero")
    if args.query is not None and args.eql is not None:
        raise VerificationFailure("--query and --eql cannot be combined")
    date_from = validate_timestamp_bound("--date-from", args.date_from)
    date_to = validate_timestamp_bound("--date-to", args.date_to)
    if date_from and date_to and parse_timestamp_bound(date_to) < parse_timestamp_bound(date_from):
        raise VerificationFailure("--date-to is earlier than --date-from")
    if args.events_file and (date_from or date_to):
        raise VerificationFailure(
            "--date-from and --date-to only apply to Kibana queries, not --events-file"
        )

    trusted_keys: Dict[str, TrustedKey] = {}
    resolution_errors: Dict[str, str] = {}
    ca_file: Optional[str] = args.ca_file
    selected_count: Optional[int] = None
    selected_identities: set[str] = set()
    if args.events_file:
        hits = load_events_from_file(args.events_file)
        selected_identities = {hit_identity(hit) for hit in hits}
        selected_count = len(selected_identities)
        if args.trust_registry:
            trusted_keys = load_trust_registry(args.trust_registry)
        source = f"file {args.events_file}"
    else:
        if not args.credentials_file:
            raise VerificationFailure("--credentials-file is required unless --events-file is used")
        credentials = load_credentials(args.credentials_file)
        kibana_url = args.kibana_url or credentials.get("kibana_url")
        if not kibana_url:
            raise VerificationFailure(
                "--kibana-url is required unless kibana_url is in the credentials file"
            )
        if args.eql is not None:
            hits = kibana_eql_request(
                kibana_url, credentials, args.eql, args.index, args.size, date_from, date_to
            )
        else:
            hits = kibana_request(
                kibana_url,
                credentials,
                args.query if args.query is not None else "*",
                args.index,
                args.size,
                date_from,
                date_to,
            )
        selected_identities = {hit_identity(hit) for hit in hits}
        selected_count = len(selected_identities)
        trust_registry = args.trust_registry or credentials.get("trust_registry")
        if trust_registry:
            trust_path = Path(str(trust_registry))
            if not trust_path.is_absolute():
                trust_path = Path(args.credentials_file).resolve().parent / trust_path
            trusted_keys = load_trust_registry(str(trust_path))
        configured_ca = args.ca_file or credentials.get("ca_file")
        if configured_ca:
            ca_path = Path(str(configured_ca))
            if not ca_path.is_absolute():
                ca_path = Path(args.credentials_file).resolve().parent / ca_path
            ca_file = str(ca_path)
        if trust_registry or ca_file:
            hits, selected_count, resolution_errors = expand_kibana_evidence(
                hits,
                kibana_url,
                credentials,
                args.index,
                args.size,
                trusted_keys,
                ca_file,
            )
        if args.eql is not None:
            selection = f"eql={args.eql!r}"
        else:
            effective_query = args.query if args.query is not None else "*"
            selection = f"query={effective_query!r}"
        bounds = ""
        if date_from or date_to:
            bounds = f"; date-from={date_from or '*'}; date-to={date_to or '*'}"
        source = (
            f"Kibana {kibana_url}; index={args.index!r}; "
            f"{selection}{bounds}; requested-size={args.size}"
        )

    if not hits:
        print(f"{FAIL_SYMBOL} no events matched the query")
        print(overall_result(False))
        return 1

    results = [verify_hit(index, hit) for index, hit in enumerate(hits, start=1)]
    for result, hit in zip(results, hits):
        result.selected = hit_identity(hit) in selected_identities
    verify_chain_continuity(results)
    for result in results:
        verify_signature(result, trusted_keys, resolution_errors)
    overall_ok = all(result.ok for result in results)
    full_report = detailed_report(results, source, selected_count)

    if len(hits) <= args.max_details:
        print("\n".join(full_report))
        if args.report_file:
            write_report(args.report_file, full_report)
    else:
        print(make_summary(results))
        print(
            f"Details omitted because {len(hits)} hits exceed "
            f"--max-details={args.max_details}."
        )
        report_path = args.report_file or prompt_for_report_file()
        write_report(report_path, full_report)
        print(f"Full report written to {report_path}")
        print(overall_result(overall_ok))

    return 0 if overall_ok else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (VerificationFailure, OSError, ValueError) as exc:
        print(f"{FAIL_SYMBOL} {exc}")
        print(overall_result(False))
        raise SystemExit(1)
