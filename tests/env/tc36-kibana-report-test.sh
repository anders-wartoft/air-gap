#!/usr/bin/env bash
# Docker-smoke test for the Kibana integrity report script.
#
# It starts a tiny HTTP server that mimics Kibana's proxy endpoint, and then
# runs the verification script inside a Python 3.12 container against that local
# endpoint. File-based cases exercise independent chains, declared restarts, and
# error accumulation without requiring a second server.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
TMPDIR="$(mktemp -d)"
VERIFIER_IMAGE="airgap-integrity-report-tc36-$$"
# shellcheck source=lib/report.sh
source "${HERE}/lib/report.sh"
PORT="$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PY
)"
SERVER_PID=""

cleanup() {
  if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  docker image rm "${VERIFIER_IMAGE}" >/dev/null 2>&1 || true
  rm -rf "${TMPDIR}"
}
trap cleanup EXIT

docker build --quiet \
  -f "${HERE}/Dockerfile.integrity-report" \
  -t "${VERIFIER_IMAGE}" \
  "${HERE}" >/dev/null

docker run --rm \
  -e AIRGAP_INTEGRITY_VECTOR=/repo/src/logging/testdata/log-integrity-v1-test-vector.json \
  -v "${HERE}:/work" \
  -v "${ROOT}:/repo:ro" \
  -w /work \
  "${VERIFIER_IMAGE}" \
  python3 -m unittest -v test_verify_integrity_report.py

# Print what the operator would have seen, so a command-line run shows the
# reports instead of only the final verdict. Set TC36_QUIET=1 to suppress.
show_report() {
  local title="$1" stdout_file="$2" report_file="${3:-}"
  if [[ "${TC36_QUIET:-0}" == "1" ]]; then
    return 0
  fi
  printf '\n──────── %s ────────\n' "${title}"
  printf -- '--- stdout ---\n'
  cat "${stdout_file}"
  if [[ -n "${report_file}" && -f "${report_file}" ]]; then
    printf -- '--- full report (%s) ---\n' "$(basename "${report_file}")"
    cat "${report_file}"
  fi
}

cat >"${TMPDIR}/creds.json" <<'JSON'
{
  "api_key": "test-api-key",
  "kibana_url": "http://host.docker.internal:PORT"
}
JSON
sed -i.bak "s/PORT/${PORT}/" "${TMPDIR}/creds.json"
rm "${TMPDIR}/creds.json.bak"

python3 - "$TMPDIR" "$PORT" <<'PY' &
import http.server
import json
import socketserver
import sys
from pathlib import Path

root = Path(sys.argv[1])
port = int(sys.argv[2])


def make_event(seq: int, prev_chain: bytes, body: str):
    import hashlib
    payload_hash = hashlib.sha256(body.encode('utf-8')).digest()
    chain_prev = b"genesis" if seq == 1 else prev_chain
    time_unix_nano = 1_700_000_000_000_000_000 + seq
    severity_number = 9
    severity_text = "INFO"
    encoded_severity = severity_text.encode("utf-8")
    chain = hashlib.sha256(
        b"airgap-log-chain-v1\x00"
        + chain_prev
        + seq.to_bytes(8, "big")
        + time_unix_nano.to_bytes(8, "big")
        + severity_number.to_bytes(4, "big")
        + len(encoded_severity).to_bytes(4, "big")
        + encoded_severity
        + payload_hash
    ).digest()
    attributes = [
        {"key": "airgap.log.seq", "value": {"intValue": str(seq)}},
        {"key": "airgap.log.hash.alg", "value": {"stringValue": "SHA256"}},
        {"key": "airgap.log.hash", "value": {"stringValue": payload_hash.hex()}},
        {"key": "airgap.log.run.id", "value": {"stringValue": "server-run"}},
        {"key": "airgap.log.segment.id", "value": {"stringValue": "server-segment"}},
        {"key": "airgap.log.chain.prev", "value": {"stringValue": chain_prev.hex()}},
        {"key": "airgap.log.chain", "value": {"stringValue": chain.hex()}},
        {"key": "airgap.log.integrity.version", "value": {"stringValue": "v1"}},
    ]
    event = {
        "_source": {
            "resourceLogs": [{
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo"}}]},
                "scopeLogs": [{
                    "logRecords": [{
                        "timeUnixNano": str(time_unix_nano),
                        "severityNumber": severity_number,
                        "severityText": severity_text,
                        "attributes": attributes,
                        "body": {"stringValue": body},
                    }]
                }]
            }]
        }
    }
    return event, chain


hits = []
prev = b""
for seq in range(1, 12):
    body = f"message {seq}"
    event, prev = make_event(seq, prev, body)
    hits.append(event)

# Negative control for the Kibana path: one body is edited in place while its
# recorded hash is left alone, which is what editing an indexed document looks
# like. The body-hash layer must catch it.
tampered_hits = json.loads(json.dumps(hits))
tampered_record = tampered_hits[6]["_source"]["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]
tampered_record["body"]["stringValue"] = "message 7 edited by an intruder"

datasets = {
    "tags:case-36": hits,
    "tags:case-36-tampered": tampered_hits,
    "tags:case-36-truncated": hits,
}

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        if self.headers.get("Authorization") != "ApiKey test-api-key":
            self.send_error(401)
            return
        request = json.loads(body)
        query = request["query"]["bool"]["filter"][0]["query_string"]["query"]
        if query not in datasets:
            self.send_error(400)
            return
        if request.get("track_total_hits") is not True:
            self.send_error(400)
            return
        dataset = datasets[query]
        returned_hits = dataset[:request["size"]]
        payload = {
            "hits": {
                "total": {"value": len(dataset), "relation": "eq"},
                "hits": returned_hits,
            }
        }
        response = json.dumps(payload).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)

    def log_message(self, format, *args):
        return

class Server(socketserver.TCPServer):
    allow_reuse_address = True

server = Server(("0.0.0.0", port), Handler)
server.timeout = 5
print(f"listening on port {port}", flush=True)
server.serve_forever()
PY
SERVER_PID=$!

# wait until the fake Kibana server is ready.
READY=0
for _ in $(seq 1 50); do
  if python3 - "${PORT}" <<'PY' >/dev/null 2>&1
import socket
import sys
s = socket.socket()
s.settimeout(0.2)
try:
    s.connect(("127.0.0.1", int(sys.argv[1])))
except Exception:
    raise SystemExit(1)
else:
    raise SystemExit(0)
finally:
    s.close()
PY
  then
    READY=1
    break
  fi
  sleep 0.2
done
if [[ "${READY}" != "1" ]]; then
  echo "fake Kibana server did not become ready" >&2
  exit 1
fi

# Positive case: all 11 events are valid and should pass.
docker run --rm --network host --add-host host.docker.internal:host-gateway \
  -v "${HERE}:/work" \
  -v "${TMPDIR}:/tmp/testwork" \
  -w /work \
  "${VERIFIER_IMAGE}" \
  python3 /work/verify_integrity_report.py \
    --kibana-url http://host.docker.internal:${PORT} \
    --credentials-file /tmp/testwork/creds.json \
    --query 'tags:case-36' \
    --max-details 10 \
    --report-file /tmp/testwork/report.txt \
    >"${TMPDIR}/positive.out"

show_report "Kibana query (positive)" "${TMPDIR}/positive.out" "${TMPDIR}/report.txt"

grep -q "11 event(s) checked" "${TMPDIR}/positive.out"
grep -q "Full report written" "${TMPDIR}/positive.out"
[[ "$(tail -n 1 "${TMPDIR}/positive.out")" == "Overall result: ✅" ]]
[[ "$(tail -n 1 "${TMPDIR}/report.txt")" == "Overall result: ✅" ]]

# A green report over only the first page would falsely claim that the entire
# case was verified. The verifier must fail closed when --size truncates the
# matching result set.
if docker run --rm --network host --add-host host.docker.internal:host-gateway \
  -v "${HERE}:/work" \
  -v "${TMPDIR}:/tmp/testwork" \
  -w /work \
  "${VERIFIER_IMAGE}" \
  python3 /work/verify_integrity_report.py \
    --kibana-url http://host.docker.internal:${PORT} \
    --credentials-file /tmp/testwork/creds.json \
    --query 'tags:case-36-truncated' \
    --size 5 \
    >"${TMPDIR}/truncated.out" 2>&1; then
  echo "truncated Kibana result set was accepted" >&2
  cat "${TMPDIR}/truncated.out" >&2
  exit 1
fi
show_report "Kibana query (negative control: truncated result set)" \
  "${TMPDIR}/truncated.out"
grep -q "matched 11 event(s), but returned 5 because --size=5" \
  "${TMPDIR}/truncated.out"
[[ "$(tail -n 1 "${TMPDIR}/truncated.out")" == "Overall result: ❌" ]]

# Negative control for the Kibana path: the same query returns the same eleven
# events, but one body was edited after indexing. Without this control a
# verifier that never recomputes a digest would also report the positive case
# as passing.
if docker run --rm --network host --add-host host.docker.internal:host-gateway \
  -v "${HERE}:/work" \
  -v "${TMPDIR}:/tmp/testwork" \
  -w /work \
  "${VERIFIER_IMAGE}" \
  python3 /work/verify_integrity_report.py \
    --kibana-url http://host.docker.internal:${PORT} \
    --credentials-file /tmp/testwork/creds.json \
    --query 'tags:case-36-tampered' \
    --max-details 10 \
    --report-file /tmp/testwork/kibana-tampered-report.txt \
    >"${TMPDIR}/kibana-tampered.out" 2>&1; then
  echo "edited body was not detected over the Kibana path" >&2
  cat "${TMPDIR}/kibana-tampered.out" >&2
  exit 1
fi
show_report "Kibana query (negative control: edited body)" \
  "${TMPDIR}/kibana-tampered.out" "${TMPDIR}/kibana-tampered-report.txt"
grep -q "11 event(s) checked: 10 passed, 1 failed" "${TMPDIR}/kibana-tampered.out"
grep -q "\[hash\] ❌ FAILED (SHA256)" "${TMPDIR}/kibana-tampered-report.txt"
grep -q "message 7 edited by an intruder" "${TMPDIR}/kibana-tampered-report.txt"
# The ten untouched events must still be reported as passing.
grep -q "Event 11:" "${TMPDIR}/kibana-tampered-report.txt"
[[ "$(tail -n 1 "${TMPDIR}/kibana-tampered.out")" == "Overall result: ❌" ]]
[[ "$(tail -n 1 "${TMPDIR}/kibana-tampered-report.txt")" == "Overall result: ❌" ]]

# Generate file-based cases. The valid case deliberately interleaves two
# run/segment pairs and restarts a third chain inside one pair.
python3 - "$TMPDIR" <<'PY'
import copy
import hashlib
import json
import sys
from pathlib import Path
root = Path(sys.argv[1])

def make_event(seq, prev_chain, body, run_id, segment_id, restart=False, version="v1"):
    payload_hash = hashlib.sha256(body.encode('utf-8')).digest()
    chain_prev = prev_chain
    time_unix_nano = 1_700_000_100_000_000_000 + seq
    severity_number = 9
    severity_text = "INFO"
    encoded_severity = severity_text.encode("utf-8")
    chain = hashlib.sha256(
        b"airgap-log-chain-v1\x00"
        + chain_prev
        + seq.to_bytes(8, "big")
        + time_unix_nano.to_bytes(8, "big")
        + severity_number.to_bytes(4, "big")
        + len(encoded_severity).to_bytes(4, "big")
        + encoded_severity
        + payload_hash
    ).digest()
    return {
        "_source": {
            "resourceLogs": [{
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo"}}]},
                "scopeLogs": [{
                    "logRecords": [{
                        "timeUnixNano": str(time_unix_nano),
                        "severityNumber": severity_number,
                        "severityText": severity_text,
                        "attributes": [
                            {"key": "airgap.log.seq", "value": {"intValue": str(seq)}},
                            {"key": "airgap.log.hash.alg", "value": {"stringValue": "SHA256"}},
                            {"key": "airgap.log.hash", "value": {"stringValue": payload_hash.hex()}},
                            {"key": "airgap.log.run.id", "value": {"stringValue": run_id}},
                            {"key": "airgap.log.segment.id", "value": {"stringValue": segment_id}},
                            {"key": "airgap.log.chain.prev", "value": {"stringValue": chain_prev.hex()}},
                            {"key": "airgap.log.chain", "value": {"stringValue": chain.hex()}},
                            {"key": "airgap.log.integrity.version", "value": {"stringValue": version}},
                            *([{"key": "airgap.log.chain.restart", "value": {"boolValue": True}}] if restart else []),
                        ],
                        "body": {"stringValue": body},
                    }]
                }]
            }]
        }
    }, chain

event_a0, chain_a0 = make_event(0, b"a-genesis", "a0", "run-a", "segment-a", True)
event_b0, chain_b0 = make_event(0, b"b-genesis", "b0", "run-b", "segment-b", True)
event_a1, chain_a1 = make_event(1, chain_a0, "a1", "run-a", "segment-a")
event_b1, chain_b1 = make_event(1, chain_b0, "b1", "run-b", "segment-b")
event_c0, chain_c0 = make_event(0, b"c-genesis", "c0", "run-c", "segment-c", True)
event_c5, chain_c5 = make_event(5, b"new-genesis", "c5", "run-c", "segment-c", True)
independent_hits = [event_a0, event_b0, event_a1, event_b1, event_c0, event_c5]
with open(root / "independent.json", "w", encoding="utf-8") as handle:
    json.dump({"hits": {"hits": independent_hits}}, handle)

# Negative control for the interleaved-chain case. Here the edited body also
# gets a matching recomputed hash, so the body-hash layer accepts it and only
# the chain value can still expose the edit.
tampered_independent = copy.deepcopy(independent_hits)
record = tampered_independent[2]["_source"]["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]
rewritten = "a1 rewritten with a matching hash"
record["body"]["stringValue"] = rewritten
recomputed = hashlib.sha256(rewritten.encode("utf-8")).hexdigest()
next(
    item for item in record["attributes"] if item["key"] == "airgap.log.hash"
)["value"]["stringValue"] = recomputed
with open(root / "independent-tampered.json", "w", encoding="utf-8") as handle:
    json.dump({"hits": {"hits": tampered_independent}}, handle)

bad_version, _ = make_event(0, b"v2-genesis", "unsupported", "run-v2", "segment-v2", True, "v2")
tampered, _ = make_event(0, b"bad-genesis", "tampered", "run-bad", "segment-bad", True)
tampered_record = tampered["_source"]["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]
tampered_record["body"]["stringValue"] = "tampered body in a mixed result set"
valid_after, _ = make_event(0, b"good-genesis", "still checked", "run-good", "segment-good", True)
missing_chain, _ = make_event(
    0, b"missing-chain-genesis", "chain field deleted", "run-missing", "segment-missing", True
)
missing_attrs = missing_chain["_source"]["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]["attributes"]
missing_attrs[:] = [item for item in missing_attrs if item["key"] != "airgap.log.chain"]
malformed = {"_source": "not an event object"}
with open(root / "failures.json", "w", encoding="utf-8") as handle:
    json.dump(
        {"hits": {"hits": [bad_version, malformed, tampered, missing_chain, valid_after]}},
        handle,
    )
PY

docker run --rm -v "${HERE}:/work" -v "${TMPDIR}:/tmp/testwork" -w /work "${VERIFIER_IMAGE}" \
  python3 /work/verify_integrity_report.py \
    --events-file /tmp/testwork/independent.json \
    --max-details 10 \
    >"${TMPDIR}/independent.out"
show_report "Interleaved chains (positive)" "${TMPDIR}/independent.out"
[[ "$(tail -n 1 "${TMPDIR}/independent.out")" == "Overall result: ✅" ]]

# Negative control for the interleaved-chain case. Grouping by run/segment must
# not become a way for an edited record to pass unnoticed.
if docker run --rm -v "${HERE}:/work" -v "${TMPDIR}:/tmp/testwork" -w /work "${VERIFIER_IMAGE}" \
  python3 /work/verify_integrity_report.py \
    --events-file /tmp/testwork/independent-tampered.json \
    --max-details 10 \
    --report-file /tmp/testwork/independent-tampered-report.txt \
    >"${TMPDIR}/independent-tampered.out" 2>&1; then
  echo "edited body with a recomputed hash was not detected" >&2
  cat "${TMPDIR}/independent-tampered.out" >&2
  exit 1
fi
show_report "Interleaved chains (negative control: edited body, recomputed hash)" \
  "${TMPDIR}/independent-tampered.out" "${TMPDIR}/independent-tampered-report.txt"
grep -q "6 event(s) checked: 5 passed, 1 failed" "${TMPDIR}/independent-tampered.out"
grep -q "\[chain\] ❌ value FAILED (SHA256)" "${TMPDIR}/independent-tampered.out"
grep -q "a1 rewritten with a matching hash" "${TMPDIR}/independent-tampered.out"
[[ "$(tail -n 1 "${TMPDIR}/independent-tampered.out")" == "Overall result: ❌" ]]
[[ "$(tail -n 1 "${TMPDIR}/independent-tampered-report.txt")" == "Overall result: ❌" ]]

if docker run --rm -v "${HERE}:/work" -v "${TMPDIR}:/tmp/testwork" -w /work "${VERIFIER_IMAGE}" \
  python3 /work/verify_integrity_report.py \
    --events-file /tmp/testwork/failures.json \
    --max-details 0 \
    --report-file /tmp/testwork/failures-report.txt \
    >"${TMPDIR}/negative.out" 2>&1; then
  echo "negative-control check unexpectedly passed" >&2
  cat "${TMPDIR}/negative.out" >&2
  exit 1
fi
show_report "Mixed failures (negative control)" \
  "${TMPDIR}/negative.out" "${TMPDIR}/failures-report.txt"
grep -q "Event 1:" "${TMPDIR}/failures-report.txt"
grep -q "Event 2:" "${TMPDIR}/failures-report.txt"
grep -q "Event 3:" "${TMPDIR}/failures-report.txt"
grep -q "Event 4:" "${TMPDIR}/failures-report.txt"
grep -q "Event 5:" "${TMPDIR}/failures-report.txt"
grep -q "unsupported integrity version" "${TMPDIR}/failures-report.txt"
grep -q "unsupported search hit format" "${TMPDIR}/failures-report.txt"
grep -q "\[hash\] ❌ FAILED (SHA256)" "${TMPDIR}/failures-report.txt"
grep -q "tampered body in a mixed result set" "${TMPDIR}/failures-report.txt"
grep -q "chain metadata is present but airgap.log.chain is missing" "${TMPDIR}/failures-report.txt"
grep -q "still checked" "${TMPDIR}/failures-report.txt"
[[ "$(tail -n 1 "${TMPDIR}/negative.out")" == "Overall result: ❌" ]]
[[ "$(tail -n 1 "${TMPDIR}/failures-report.txt")" == "Overall result: ❌" ]]

echo "✅ tc36-kibana-report-test passed"
report_summary "TC-36 Kibana integrity report verification" 0
