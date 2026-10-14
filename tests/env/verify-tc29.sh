#!/usr/bin/env bash
# TC-29 verifier: positive and negative Version 1 signature verification on
# both upstream and downstream OTLP/JSON logs.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
WORK="../../tmp/tc29"
CERT_DIR="../../certs/tmp/tc29"
SIGNER_ID="urn:airgap:test:tc29"
rc=0

mkdir -p "$WORK"
docker logs "${PROJECT}-upstream-a-1" >"$WORK/upstream.log" 2>&1 || rc=1
docker logs "${PROJECT}-downstream-1" >"$WORK/downstream.log" 2>&1 || rc=1

go build -o "$WORK/tc29-integrity" ./lib/tc29_integrity.go

tamper_hash() {
    python3 - "$1" "$2" <<'PY'
import json
import sys

source, destination = sys.argv[1:3]
lines = [line for line in open(source, encoding="utf-8") if line.strip()]
changed = False
with open(destination, "w", encoding="utf-8") as output:
    for line in lines:
        payload = json.loads(line)
        record = payload["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]
        attributes = record.get("attributes", [])
        is_anchor = any(
            item["key"] == "airgap.log.anchor"
            and item["value"].get("boolValue") is True
            for item in attributes
        )
        if not changed and not is_anchor:
            for item in attributes:
                if item["key"] == "airgap.log.hash":
                    value = item["value"]["stringValue"]
                    item["value"]["stringValue"] = format(
                        (int(value, 16) + 1) % (1 << (4 * len(value))),
                        f"0{len(value)}x",
                    )
                    changed = True
                    break
        output.write(json.dumps(payload, separators=(",", ":")) + "\n")
if not changed:
    raise SystemExit("no ordinary record hash found to tamper")
PY
}

tamper_signature() {
    python3 - "$1" "$2" <<'PY'
import base64
import json
import sys

source, destination = sys.argv[1:3]
changed = False
with open(destination, "w", encoding="utf-8") as output:
    for line in open(source, encoding="utf-8"):
        if not line.strip():
            continue
        payload = json.loads(line)
        attributes = payload["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0].get("attributes", [])
        for item in attributes:
            if item["key"] == "airgap.log.signature":
                signature = bytearray(base64.b64decode(item["value"]["stringValue"], validate=True))
                signature[0] ^= 1
                item["value"]["stringValue"] = base64.b64encode(signature).decode()
                changed = True
                break
        output.write(json.dumps(payload, separators=(",", ":")) + "\n")
if not changed:
    raise SystemExit("no signature found to tamper")
PY
}

expect_failure() {
    local subject="$1"
    shift
    if "$@" >"$WORK/expected-failure.log" 2>&1; then
        report_check "$subject" "verification failure" "verification unexpectedly succeeded" 1 || rc=1
    else
        report_check "$subject" "verification failure" "rejected as expected" 0 || rc=1
    fi
}

verify_side() {
    local side="$1" service="$2"
    local log="$WORK/$side.log"
    local window="$WORK/$side-window.ndjson"

    if "$WORK/tc29-integrity" verify \
        --log "$log" \
        --certificate "$CERT_DIR/signer.crt" \
        --window-out "$window" \
        --expect-service "$service" \
        --expect-signer "$SIGNER_ID"; then
        report_check "$side Version 1 checkpoint" \
            "valid signature, digitalSignature certificate, signing EKU, serial, and public-key fingerprint" \
            "verified" 0 || rc=1
    else
        report_check "$side Version 1 checkpoint" "valid signature and certificate identity" "verification failed" 1 || rc=1
        return
    fi

    record_count="$(grep -c . "$window" || true)"
    if [[ "$record_count" == "11" ]]; then
        report_check "$side signed window shape" \
            "10 ordinary records plus the Version 1 anchor" \
            "11 records" 0 || rc=1
    else
        report_check "$side signed window shape" \
            "10 ordinary records plus the Version 1 anchor" \
            "$record_count records" 1 || rc=1
    fi

    expect_failure "$side wrong public key" \
        "$WORK/tc29-integrity" verify \
        --log "$window" \
        --certificate "$CERT_DIR/wrong.crt" \
        --expect-service "$service" \
        --expect-signer "$SIGNER_ID"

    tamper_hash "$window" "$WORK/$side-tampered-hash.ndjson"
    expect_failure "$side record 1 hash increased by one" \
        "$WORK/tc29-integrity" verify \
        --log "$WORK/$side-tampered-hash.ndjson" \
        --certificate "$CERT_DIR/signer.crt" \
        --expect-service "$service" \
        --expect-signer "$SIGNER_ID"

    sed '1d' "$window" >"$WORK/$side-missing-record.ndjson"
    expect_failure "$side missing record" \
        "$WORK/tc29-integrity" verify \
        --log "$WORK/$side-missing-record.ndjson" \
        --certificate "$CERT_DIR/signer.crt" \
        --expect-service "$service" \
        --expect-signer "$SIGNER_ID"

    tamper_signature "$window" "$WORK/$side-tampered-signature.ndjson"
    expect_failure "$side modified signature" \
        "$WORK/tc29-integrity" verify \
        --log "$WORK/$side-tampered-signature.ndjson" \
        --certificate "$CERT_DIR/signer.crt" \
        --expect-service "$service" \
        --expect-signer "$SIGNER_ID"
}

verify_side upstream upstream
verify_side downstream downstream

if (( rc == 0 )); then
    rm -f \
        "$CERT_DIR/signer.crt" "$CERT_DIR/signer.key" "$CERT_DIR/signer.pub" \
        "$CERT_DIR/wrong.crt" "$CERT_DIR/wrong.key" "$CERT_DIR/wrong.pub"
    rm -rf "$WORK"
fi

report_summary "TC-29 signature verification" "$rc"
exit "$rc"
