#!/usr/bin/env python3
"""Load real Air-gap OTLP logs into Elasticsearch for TC-37."""

from __future__ import annotations

import argparse
import base64
import copy
import datetime
import json
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


def log_record(envelope: dict[str, Any]) -> dict[str, Any]:
    return envelope["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]


def attributes(envelope: dict[str, Any]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for item in log_record(envelope).get("attributes", []):
        value = item["value"]
        result[item["key"]] = next(iter(value.values()))
    return result


def resource_attributes(envelope: dict[str, Any]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    resource = envelope["resourceLogs"][0].get("resource", {})
    for item in resource.get("attributes", []):
        value = item["value"]
        result[item["key"]] = next(iter(value.values()))
    return result


def read_complete_events(path: Path) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict):
            events.append(value)
    return events


def wait_for_signed_rotation(
    path: Path,
    new_key_id: str,
    timeout: int,
) -> tuple[list[dict[str, Any]], dict[str, Any], dict[str, Any]]:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if path.exists():
            events = read_complete_events(path)
            rotations = [
                event
                for event in events
                if log_record(event).get("eventName")
                == "log.integrity.signing_certificate.rotate"
            ]
            anchors = [
                event
                for event in events
                if attributes(event).get("airgap.log.anchor") is True
                and attributes(event).get("airgap.log.key.id") == new_key_id
            ]
            if rotations and anchors:
                anchor = anchors[-1]
                anchor_attrs = attributes(anchor)
                candidates = [
                    event
                    for event in events
                    if attributes(event).get("airgap.log.run.id")
                    == anchor_attrs["airgap.log.run.id"]
                    and attributes(event).get("airgap.log.window.id")
                    == anchor_attrs["airgap.log.window.id"]
                    and attributes(event).get("airgap.log.anchor") is not True
                ]
                if candidates:
                    return events, candidates[0], anchor
        time.sleep(1)
    raise SystemExit(
        f"timed out waiting for a complete new-key-signed window in {path}"
    )


def mutate(
    events: list[dict[str, Any]],
    selected: dict[str, Any],
    anchor: dict[str, Any],
    mode: str,
) -> list[dict[str, Any]]:
    result = copy.deepcopy(events)
    selected_attrs = attributes(selected)
    anchor_attrs = attributes(anchor)

    def matching_event(expected: dict[str, Any]) -> dict[str, Any]:
        expected_attrs = attributes(expected)
        for event in result:
            actual = attributes(event)
            if (
                actual.get("airgap.log.run.id") == expected_attrs.get("airgap.log.run.id")
                and actual.get("airgap.log.seq") == expected_attrs.get("airgap.log.seq")
            ):
                return event
        raise SystemExit("TC-37 mutation target is missing")

    if mode == "body":
        log_record(matching_event(selected))["body"]["stringValue"] = (
            "TC-37 body changed after signing"
        )
    elif mode == "timestamp":
        record = log_record(matching_event(selected))
        record["timeUnixNano"] = str(int(record["timeUnixNano"]) + 1)
    elif mode == "severity":
        record = log_record(matching_event(selected))
        if record.get("severityNumber") == 17 and record.get("severityText") == "ERROR":
            record["severityNumber"] = 9
            record["severityText"] = "INFO"
        else:
            record["severityNumber"] = 17
            record["severityText"] = "ERROR"
    elif mode == "hash":
        event = matching_event(selected)
        item = next(
            item
            for item in log_record(event)["attributes"]
            if item["key"] == "airgap.log.hash"
        )
        item["value"]["stringValue"] = "00" * 32
    elif mode == "signature":
        event = matching_event(anchor)
        item = next(
            item
            for item in log_record(event)["attributes"]
            if item["key"] == "airgap.log.signature"
        )
        signature = bytearray(base64.b64decode(item["value"]["stringValue"]))
        signature[0] ^= 1
        item["value"]["stringValue"] = base64.b64encode(signature).decode("ascii")
    elif mode != "good":
        raise SystemExit(f"unsupported mutation: {mode}")

    selected_after = matching_event(selected)
    if attributes(selected_after) != selected_attrs and mode == "body":
        raise SystemExit("body mutation unexpectedly changed integrity attributes")
    if mode == "signature" and attributes(matching_event(anchor)).get(
        "airgap.log.window.id"
    ) != anchor_attrs.get("airgap.log.window.id"):
        raise SystemExit("signature mutation changed the anchor identity")
    return result


def request(
    method: str,
    url: str,
    payload: bytes | None = None,
    content_type: str = "application/json",
) -> dict[str, Any]:
    req = urllib.request.Request(url, data=payload, method=method)
    if payload is not None:
        req.add_header("Content-Type", content_type)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            body = response.read()
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        raise SystemExit(f"{method} {url} failed: HTTP {exc.code}: {detail}") from exc
    if not body:
        return {}
    value = json.loads(body)
    if not isinstance(value, dict):
        raise SystemExit(f"{method} {url} returned a non-object response")
    return value


def event_timestamp(envelope: dict[str, Any]) -> str:
    nanos = int(log_record(envelope)["timeUnixNano"])
    seconds, remainder = divmod(nanos, 1_000_000_000)
    instant = datetime.datetime.fromtimestamp(seconds, datetime.timezone.utc)
    return instant.strftime("%Y-%m-%dT%H:%M:%S") + f".{remainder:09d}Z"


def document(envelope: dict[str, Any], selected: bool) -> dict[str, Any]:
    attrs = attributes(envelope)
    resource = resource_attributes(envelope)
    log_fields: dict[str, Any] = {
        "seq": int(attrs["airgap.log.seq"]),
        "run": {"id": attrs["airgap.log.run.id"]},
        "window": {"id": attrs["airgap.log.window.id"]},
    }
    new_key_id = attrs.get("airgap.log.signing.new.key.id")
    if new_key_id is not None:
        log_fields["signing"] = {"new": {"key": {"id": new_key_id}}}
    result: dict[str, Any] = {
        "@timestamp": event_timestamp(envelope),
        "event": {
            "category": ["process"],
            "original": json.dumps(envelope, separators=(",", ":")),
        },
        "airgap": {"log": log_fields},
        "service": {"name": resource.get("service.name", "airgap-upstream")},
    }
    if selected:
        result["tags"] = ["case-37"]
    return result


def index_variant(
    elasticsearch: str,
    index: str,
    events: list[dict[str, Any]],
    selected: dict[str, Any],
) -> None:
    try:
        request("DELETE", f"{elasticsearch}/{index}")
    except SystemExit as exc:
        if "HTTP 404" not in str(exc):
            raise
    mapping = {
        "mappings": {
            "properties": {
                "@timestamp": {"type": "date_nanos"},
                "tags": {"type": "keyword"},
                "event": {
                    "properties": {
                        "category": {"type": "keyword"},
                        "original": {
                            "type": "keyword",
                            "index": False,
                            "doc_values": False,
                        },
                    }
                },
                "service": {"properties": {"name": {"type": "keyword"}}},
                "airgap": {
                    "properties": {
                        "log": {
                            "properties": {
                                "seq": {"type": "long"},
                                "run": {"properties": {"id": {"type": "keyword"}}},
                                "window": {
                                    "properties": {"id": {"type": "keyword"}}
                                },
                                "signing": {
                                    "properties": {
                                        "new": {
                                            "properties": {
                                                "key": {
                                                    "properties": {
                                                        "id": {"type": "keyword"}
                                                    }
                                                }
                                            }
                                        }
                                    }
                                },
                            }
                        }
                    }
                },
            }
        }
    }
    request(
        "PUT",
        f"{elasticsearch}/{index}",
        json.dumps(mapping).encode("utf-8"),
    )
    selected_attrs = attributes(selected)
    lines: list[str] = []
    for event in events:
        attrs = attributes(event)
        is_selected = (
            attrs.get("airgap.log.run.id") == selected_attrs["airgap.log.run.id"]
            and attrs.get("airgap.log.seq") == selected_attrs["airgap.log.seq"]
        )
        lines.append(json.dumps({"index": {"_index": index}}))
        lines.append(json.dumps(document(event, is_selected), separators=(",", ":")))
    payload = ("\n".join(lines) + "\n").encode("utf-8")
    response = request(
        "POST",
        f"{elasticsearch}/_bulk?refresh=wait_for",
        payload,
        "application/x-ndjson",
    )
    if response.get("errors") is not False:
        raise SystemExit(f"Elasticsearch bulk indexing failed: {response}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--events", required=True)
    parser.add_argument("--metadata", required=True)
    parser.add_argument("--elasticsearch", required=True)
    parser.add_argument("--index-prefix", default="airgap-tc37")
    parser.add_argument("--wait-seconds", type=int, default=120)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    metadata = json.loads(Path(args.metadata).read_text(encoding="utf-8"))
    events, selected, anchor = wait_for_signed_rotation(
        Path(args.events),
        str(metadata["new_key_id"]),
        args.wait_seconds,
    )
    for mode in ("good", "body", "timestamp", "severity", "hash", "signature"):
        variant = mutate(events, selected, anchor, mode)
        index_variant(
            args.elasticsearch.rstrip("/"),
            f"{args.index_prefix}-{mode}",
            variant,
            selected,
        )

    selected_time = event_timestamp(selected)
    output = {
        "query": "tags:case-37",
        "eql": 'any where tags == "case-37"',
        "date_from": selected_time,
        "selected_time": selected_time,
        "event_count": len(events),
        "selected_seq": int(attributes(selected)["airgap.log.seq"]),
        "selected_window_id": attributes(selected)["airgap.log.window.id"],
    }
    Path(args.output).write_text(
        json.dumps(output, indent=2) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(output))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
