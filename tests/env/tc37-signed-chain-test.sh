#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
cd "${HERE}"
# shellcheck source=lib/report.sh
source "${HERE}/lib/report.sh"
export COMPOSE_PROGRESS=plain BUILDKIT_PROGRESS=plain
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-tc37.XXXXXX")"
WORK="$(cd "${WORK}" && pwd -P)"
PROJECT="$(basename "${WORK}" | tr '[:upper:].' '[:lower:]-')"
export TC_WORK="${WORK}"
COMPOSE_BASE="${HERE}/docker-compose.kafka-transfer.yml"
COMPOSE_OVERRIDE="${HERE}/docker-compose.tc37.yml"
VERIFIER_IMAGE="${PROJECT}-integrity-report"

compose() {
  docker compose -p "${PROJECT}" -f "${COMPOSE_BASE}" -f "${COMPOSE_OVERRIDE}" "$@"
}

cleanup() {
  local rc=$?
  trap - EXIT
  if ((rc != 0)); then
    compose logs --no-color >&2 || true
    echo "TC-37 artifacts retained at ${WORK}" >&2
  fi
  if [[ "${TC_PAUSE:-0}" != "1" || ${rc} -ne 0 ]]; then
    compose down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  docker image rm "${VERIFIER_IMAGE}" >/dev/null 2>&1 || true
  if ((rc == 0)) && [[ "${TC_PAUSE:-0}" != "1" ]]; then
    rm -rf "${WORK}"
  fi
  report_summary "TC-37 real signed-chain verification through Elasticsearch and Kibana" "${rc}" || true
  exit "${rc}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

wait_for_old_anchor() {
  python3 - "${WORK}/events.ndjson" "${WORK}/metadata.json" <<'PY'
import json
import sys
import time
from pathlib import Path

events_path = Path(sys.argv[1])
old_key_id = json.loads(Path(sys.argv[2]).read_text())["old_key_id"]
deadline = time.monotonic() + 120
while time.monotonic() < deadline:
    if events_path.exists():
        for line in events_path.read_text(errors="replace").splitlines():
            try:
                envelope = json.loads(line)
                record = envelope["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]
                attrs = {}
                for item in record.get("attributes", []):
                    attrs[item["key"]] = next(iter(item["value"].values()))
                if (
                    attrs.get("airgap.log.anchor") is True
                    and attrs.get("airgap.log.key.id") == old_key_id
                ):
                    raise SystemExit(0)
            except (json.JSONDecodeError, KeyError, IndexError, StopIteration, TypeError):
                continue
    time.sleep(1)
raise SystemExit("timed out waiting for an old-key-signed upstream window")
PY
}

wait_for_url() {
  local url="$1" description="$2" deadline=$((SECONDS + 180))
  while ((SECONDS < deadline)); do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "${description} did not become ready at ${url}" >&2
  return 1
}

run_report() {
  local index="$1" output="$2"
  shift 2
  if docker run --rm \
    --network "${PROJECT}_default" \
    -v "${HERE}:/work:ro" \
    -v "${WORK}:/tmp/testwork:ro" \
    -w /work \
    "${VERIFIER_IMAGE}" \
    python3 /work/verify_integrity_report.py \
      --credentials-file /tmp/testwork/credentials.json \
      --index "${index}" \
      "$@" \
      --size 200 \
      --max-details 200 >"${output}" 2>&1; then
    return 0
  fi
  return 1
}

show_report() {
  local title="$1" output="$2"
  if [[ "${TC37_QUIET:-0}" == "1" ]]; then
    return 0
  fi
  printf '\n──────── %s ────────\n' "${title}"
  cat "${output}"
}

echo "TC-37: generating CA-issued signing certificates and building the real upstream"
docker build --quiet \
  -f "${HERE}/Dockerfile.integrity-report" \
  -t "${VERIFIER_IMAGE}" \
  "${HERE}" >/dev/null
go run "${HERE}/lib/tc37fixture" --output "${WORK}"
mkdir -p "${ROOT}/target/linux-amd64"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -o "${ROOT}/target/linux-amd64/upstream" "${ROOT}/src/cmd/upstream"
chmod 0755 "${ROOT}/target/linux-amd64/upstream"

cat >"${WORK}/upstream.properties" <<EOF
id=TC37-upstream
nic=lo
targetIP=127.0.0.1
targetPort=1234
transport=udp
source=random
eps=2
payloadSize=1200
logLevel=INFO
logStatistics=1
logFileName=${WORK}/events.ndjson
logFormat=otel
hash=SHA256
hash-chain=3
log-signing-cert=${WORK}/old.crt
log-signing-key=${WORK}/old.key
log-signer-id=urn:airgap:test:tc37
EOF

echo "TC-37: starting real upstream, Elasticsearch, and Kibana containers"
compose build tc37-upstream
compose up -d elasticsearch kibana tc37-upstream

UPSTREAM_CID="$(compose ps -q tc37-upstream)"
[[ -n "${UPSTREAM_CID}" ]] || {
  echo "real upstream container did not start" >&2
  exit 1
}

ELASTIC_ADDRESS="$(compose port elasticsearch 9200)"
KIBANA_ADDRESS="$(compose port kibana 5601)"
ELASTIC_URL="http://${ELASTIC_ADDRESS}"
KIBANA_URL="http://${KIBANA_ADDRESS}"
wait_for_url "${ELASTIC_URL}/_cluster/health?wait_for_status=yellow&timeout=1s" "Elasticsearch"
wait_for_url "${KIBANA_URL}/api/status" "Kibana"

echo "TC-37: waiting for a complete old-key-signed window from the real upstream"
wait_for_old_anchor

echo "TC-37: rotating the real upstream to the CA-issued replacement with SIGHUP"
python3 "${ROOT}/scripts/configure-log-signing.py" \
  --config "${WORK}/upstream.properties" \
  --certificate "${WORK}/new.crt" \
  --key "${WORK}/new.key" \
  --signer-id urn:airgap:test:tc37 >"${WORK}/configure.out"
docker exec "${UPSTREAM_CID}" kill -HUP 1

echo "TC-37: indexing complete signed windows into real Elasticsearch"
python3 "${HERE}/lib/tc37_index_events.py" \
  --events "${WORK}/events.ndjson" \
  --metadata "${WORK}/metadata.json" \
  --elasticsearch "${ELASTIC_URL}" \
  --index-prefix airgap-tc37 \
  --output "${WORK}/selection.json" | tee "${WORK}/index.out"

QUERY="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["query"])' "${WORK}/selection.json")"
EQL="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["eql"])' "${WORK}/selection.json")"
DATE_FROM="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["date_from"])' "${WORK}/selection.json")"
QUERY_ARGS=(--query "${QUERY}" --date-from "${DATE_FROM}")
EQL_ARGS=(--eql "${EQL}" --date-from "${DATE_FROM}")
cat >"${WORK}/credentials.json" <<EOF
{
  "kibana_url": "http://kibana:5601",
  "username": "tc37",
  "password": "tc37",
  "ca_file": "/tmp/testwork/ca.crt"
}
EOF

python3 - "${QUERY}" "${DATE_FROM}" "${WORK}/search.json" <<'PY'
import json
import sys

query, date_from, output = sys.argv[1], sys.argv[2], sys.argv[3]
body = {
    "size": 100,
    "track_total_hits": True,
    "query": {
        "bool": {
            "filter": [
                {"query_string": {"query": query}},
                {
                    "range": {
                        "@timestamp": {
                            "gte": date_from,
                            "format": "strict_date_optional_time_nanos",
                        }
                    }
                },
            ]
        }
    },
    "sort": [{"@timestamp": {"order": "asc"}}],
}
with open(output, "w", encoding="utf-8") as handle:
    json.dump(body, handle, indent=2)
    handle.write("\n")
PY

echo "TC-37: proving the case event is searchable with the verifier's Elasticsearch wrapper"
curl -fsS -H 'Content-Type: application/json' \
  --data-binary "@${WORK}/search.json" \
  "${ELASTIC_URL}/airgap-tc37-good/_search" >"${WORK}/search-result.json"
python3 - "${WORK}/search-result.json" <<'PY'
import json
import sys

result = json.load(open(sys.argv[1], encoding="utf-8"))
total = result["hits"]["total"]
count = total if isinstance(total, int) else total["value"]
if count != 1 or len(result["hits"]["hits"]) != 1:
    raise SystemExit(f"expected one selected event from real Elasticsearch, got {count}")
print("real Elasticsearch selected exactly one signed case event")
PY

echo "TC-37: verifying the selected event and automatically acquired evidence through real Kibana"
if ! run_report "airgap-tc37-good" "${WORK}/good.out" "${QUERY_ARGS[@]}"; then
  echo "positive signed-chain report failed" >&2
  cat "${WORK}/good.out" >&2
  exit 1
fi
show_report "Real Kibana signed-chain verification (positive)" "${WORK}/good.out"
grep -q "1 selected event(s)" "${WORK}/good.out"
grep -q "automatically fetched" "${WORK}/good.out"
grep -q "\[signature\] ✅ verified (Ed25519)" "${WORK}/good.out"
[[ "$(tail -n 1 "${WORK}/good.out")" == "Overall result: ✅" ]]

echo "TC-37: selecting the same case event with a real EQL query and --date-from"
if ! run_report "airgap-tc37-good" "${WORK}/good-eql.out" "${EQL_ARGS[@]}"; then
  echo "positive signed-chain report with --eql failed" >&2
  cat "${WORK}/good-eql.out" >&2
  exit 1
fi
show_report "Real Kibana signed-chain verification (--eql selection)" "${WORK}/good-eql.out"
grep -q "1 selected event(s)" "${WORK}/good-eql.out"
grep -q "automatically fetched" "${WORK}/good-eql.out"
grep -q "\[signature\] ✅ verified (Ed25519)" "${WORK}/good-eql.out"
grep -q "eql=" "${WORK}/good-eql.out"
[[ "$(tail -n 1 "${WORK}/good-eql.out")" == "Overall result: ✅" ]]

for mutation in body timestamp severity hash signature; do
  if run_report "airgap-tc37-${mutation}" "${WORK}/${mutation}.out" "${QUERY_ARGS[@]}"; then
    echo "${mutation} mutation unexpectedly passed" >&2
    cat "${WORK}/${mutation}.out" >&2
    exit 1
  fi
  show_report "Real Elasticsearch negative control: changed ${mutation}" \
    "${WORK}/${mutation}.out"
  [[ "$(tail -n 1 "${WORK}/${mutation}.out")" == "Overall result: ❌" ]]
done

grep -q "\[hash\] ❌" "${WORK}/body.out"
grep -q "\[chain\] ❌ value FAILED" "${WORK}/timestamp.out"
grep -q "\[chain\] ❌ value FAILED" "${WORK}/severity.out"
grep -q "\[hash\] ❌" "${WORK}/hash.out"
grep -q "\[signature\] ❌" "${WORK}/signature.out"

if [[ "${TC_PAUSE:-0}" == "1" ]]; then
  echo "TC-37 containers retained. Kibana: ${KIBANA_URL} Elasticsearch: ${ELASTIC_URL}"
  echo "TC-37 artifacts retained at ${WORK}"
fi
echo "✅ tc37-signed-chain-test passed"
