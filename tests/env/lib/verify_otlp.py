#!/usr/bin/env python3
"""Validate line-delimited OTLP/JSON application logs for REQ-52."""

import json
import re
import sys
from pathlib import Path

SEVERITY = {
    "TRACE": 1,
    "DEBUG": 5,
    "INFO": 9,
    "WARN": 13,
    "ERROR": 17,
    "FATAL": 21,
}
APPLICATION_TEXT = re.compile(r"^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ")
# REQ-52.09: event names are hierarchical, from the general domain to the
# specific action, so that a consumer can match on a prefix such as "kafka.".
EVENT_NAME = re.compile(r"^[a-z][a-z0-9]*(\.[a-z0-9]+)+$")


def fail(message: str) -> None:
    raise ValueError(message)


def any_value(value: object, context: str) -> object:
    if not isinstance(value, dict):
        fail(f"{context}: value is not an object")
    fields = [name for name in ("stringValue", "intValue", "boolValue") if name in value]
    if len(fields) != 1:
        fail(f"{context}: expected exactly one supported AnyValue field, got {fields}")
    field = fields[0]
    result = value[field]
    if field == "stringValue" and not isinstance(result, str):
        fail(f"{context}: stringValue is not a string")
    if field == "intValue" and (not isinstance(result, str) or not re.fullmatch(r"-?\d+", result)):
        fail(f"{context}: intValue is not a decimal string")
    if field == "boolValue" and not isinstance(result, bool):
        fail(f"{context}: boolValue is not a boolean")
    return result


def attributes(items: object, context: str) -> dict[str, object]:
    if not isinstance(items, list):
        fail(f"{context}: attributes is not an array")
    result: dict[str, object] = {}
    for index, item in enumerate(items):
        if not isinstance(item, dict) or not isinstance(item.get("key"), str):
            fail(f"{context}[{index}]: invalid key/value object")
        key = item["key"]
        if key in result:
            fail(f"{context}: duplicate attribute key {key!r}")
        result[key] = any_value(item.get("value"), f"{context}.{key}")
    return result


def one(items: object, context: str) -> dict:
    if not isinstance(items, list) or len(items) != 1 or not isinstance(items[0], dict):
        length = len(items) if isinstance(items, list) else "non-array"
        fail(f"{context}: expected exactly one object, got {length}")
    return items[0]


def validate(path: Path, expected_service: str) -> tuple[int, list[str], set[str]]:
    records = 0
    bodies: list[str] = []
    event_names: set[str] = set()
    saw_json = False
    for line_number, physical_line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        stripped = physical_line.strip()
        if not stripped:
            continue
        if not stripped.startswith("{"):
            if saw_json and APPLICATION_TEXT.match(stripped):
                fail(f"{path}:{line_number}: text application record after OTel activation: {stripped!r}")
            continue

        saw_json = True
        try:
            envelope = json.loads(stripped)
        except json.JSONDecodeError as error:
            fail(f"{path}:{line_number}: invalid JSON: {error}")
        if not isinstance(envelope, dict) or set(envelope) != {"resourceLogs"}:
            fail(f"{path}:{line_number}: envelope must contain only resourceLogs")

        resource_logs = one(envelope["resourceLogs"], "resourceLogs")
        if set(resource_logs) != {"resource", "scopeLogs"}:
            fail(f"{path}:{line_number}: resourceLogs has non-OTLP fields {set(resource_logs)}")

        resource = resource_logs["resource"]
        if not isinstance(resource, dict) or set(resource) != {"attributes"}:
            fail(f"{path}:{line_number}: invalid resource object")
        resource_attrs = attributes(resource["attributes"], "resource.attributes")
        if resource_attrs.get("service.name") != expected_service:
            fail(
                f"{path}:{line_number}: service.name={resource_attrs.get('service.name')!r}, "
                f"want {expected_service!r}"
            )
        instance = resource_attrs.get("service.instance.id")
        if not isinstance(instance, str) or not instance:
            fail(f"{path}:{line_number}: missing service.instance.id")
        service_version = resource_attrs.get("service.version")
        if not isinstance(service_version, str) or not service_version:
            fail(f"{path}:{line_number}: missing resource service.version")

        scope_logs = one(resource_logs["scopeLogs"], "scopeLogs")
        if set(scope_logs) != {"scope", "logRecords"}:
            fail(f"{path}:{line_number}: invalid scopeLogs fields {set(scope_logs)}")
        scope = scope_logs["scope"]
        if not isinstance(scope, dict) or not isinstance(scope.get("name"), str) or not scope["name"]:
            fail(f"{path}:{line_number}: empty instrumentation scope")

        record = one(scope_logs["logRecords"], "logRecords")
        allowed = {
            "timeUnixNano",
            "observedTimeUnixNano",
            "severityNumber",
            "severityText",
            "eventName",
            "body",
            "attributes",
        }
        if not set(record).issubset(allowed):
            fail(f"{path}:{line_number}: non-OTLP log record fields {set(record) - allowed}")
        for timestamp_name in ("timeUnixNano", "observedTimeUnixNano"):
            timestamp = record.get(timestamp_name)
            if not isinstance(timestamp, str) or not timestamp.isdigit() or int(timestamp) <= 0:
                fail(f"{path}:{line_number}: invalid {timestamp_name}={timestamp!r}")
        severity_text = record.get("severityText")
        if severity_text not in SEVERITY:
            fail(f"{path}:{line_number}: invalid severityText={severity_text!r}")
        if record.get("severityNumber") != SEVERITY[severity_text]:
            fail(
                f"{path}:{line_number}: severityNumber={record.get('severityNumber')!r}, "
                f"want {SEVERITY[severity_text]} for {severity_text}"
            )
        body = any_value(record.get("body"), "body")
        if not isinstance(body, str):
            fail(f"{path}:{line_number}: body must use stringValue")
        bodies.append(body)

        record_attrs = attributes(record.get("attributes", []), "logRecord.attributes")
        for key in record_attrs:
            if key.startswith("airgap.log.sig"):
                fail(f"{path}:{line_number}: signature attribute {key!r} is reserved for later")

        # REQ-52.09: EventName is a first-class LogRecord field, not an
        # attribute. It identifies the event structure without parsing body.
        event_name = record.get("eventName")
        if not isinstance(event_name, str) or not event_name:
            fail(f"{path}:{line_number}: record has no EventName")
        if not EVENT_NAME.fullmatch(event_name):
            fail(f"{path}:{line_number}: eventName={event_name!r} is not a hierarchical name")
        event_names.add(event_name)

        for forbidden in ("eventName", "event.result", "error.message", "service.version"):
            if forbidden in record_attrs:
                fail(f"{path}:{line_number}: deprecated or misplaced attribute {forbidden!r}")

        error_type = record_attrs.get("error.type")
        if error_type is not None and (not isinstance(error_type, str) or not error_type):
            fail(f"{path}:{line_number}: error.type must be a non-empty string")
        if severity_text in {"ERROR", "FATAL"} and not isinstance(error_type, str):
            fail(f"{path}:{line_number}: {severity_text} record requires error.type")

        exception_keys = {"exception.type", "exception.message", "exception.stacktrace"} & set(record_attrs)
        if exception_keys:
            if not event_name.endswith(".exception"):
                fail(f"{path}:{line_number}: exception attributes require an .exception EventName")
            exception_type = record_attrs.get("exception.type")
            exception_message = record_attrs.get("exception.message")
            if not isinstance(exception_type, str) and not isinstance(exception_message, str):
                fail(f"{path}:{line_number}: exception requires exception.type or exception.message")
            for key in exception_keys:
                if not isinstance(record_attrs[key], str):
                    fail(f"{path}:{line_number}: {key} must be a string")
            if not isinstance(error_type, str):
                fail(f"{path}:{line_number}: exception event requires error.type")

        records += 1

    if records == 0:
        fail(f"{path}: no OTLP/JSON application records found")
    return records, bodies, event_names


def main() -> int:
    argv = sys.argv[1:]

    print_bodies = False
    print_events = False
    required_events: list[str] = []
    required_prefixes: list[str] = []
    while argv and argv[0].startswith("--"):
        option = argv.pop(0)
        if option == "--bodies":
            print_bodies = True
        elif option == "--events":
            print_events = True
        elif option == "--require-event":
            if not argv:
                print(f"usage: {sys.argv[0]} --require-event NAME ...", file=sys.stderr)
                return 2
            required_events.append(argv.pop(0))
        elif option == "--require-prefix":
            if not argv:
                print(f"usage: {sys.argv[0]} --require-prefix PREFIX ...", file=sys.stderr)
                return 2
            required_prefixes.append(argv.pop(0))
        else:
            print(f"{sys.argv[0]}: unknown option {option}", file=sys.stderr)
            return 2

    if len(argv) != 2:
        print(
            f"usage: {sys.argv[0]} [--bodies] [--events] [--require-event NAME]... "
            "[--require-prefix PREFIX]... EXPECTED_SERVICE LOG_FILE",
            file=sys.stderr,
        )
        return 2

    expected_service, log_file = argv
    try:
        count, bodies, event_names = validate(Path(log_file), expected_service)
        missing = [name for name in required_events if name not in event_names]
        if missing:
            fail(f"{log_file}: required eventName values not emitted: {', '.join(sorted(missing))}")
        absent = [
            prefix
            for prefix in required_prefixes
            if not any(name.startswith(prefix) for name in event_names)
        ]
        if absent:
            fail(
                f"{log_file}: no eventName emitted for required domains: "
                f"{', '.join(sorted(absent))}"
            )
    except (OSError, UnicodeError, ValueError) as error:
        print(f"verify_otlp: FAIL: {error}", file=sys.stderr)
        return 1

    if print_bodies:
        for body in bodies:
            print(body)
    elif print_events:
        for name in sorted(event_names):
            print(name)
    else:
        print(
            f"verify_otlp: PASS: {expected_service}: {count} valid OTLP/JSON records, "
            f"{len(event_names)} distinct eventName values"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
