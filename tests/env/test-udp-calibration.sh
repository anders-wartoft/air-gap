#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
source "$HERE/lib/docker-test.sh"

project="airgap-calibration-test-$(LC_ALL=C od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
mkdir -p "$HERE/tmp"
work="$(mktemp -d "$HERE/tmp/udp-calibration.XXXXXX")"
export TC53_WORK="$work" TC53_RESULTS="$work"
export TC53_RATE=1000 TC53_DURATION=1 TC53_IDLE=1 TC53_CYCLES=1 TC53_PAYLOAD_SIZE=600
export TC53_MAX_SPARSE_LATENCY=2 TC53_DRAIN_TIMEOUT=3
compose() { docker compose -p "$project" -f "$HERE/docker-compose.tc53.yml" "$@"; }
cleanup() {
    local rc=$?
    if (( rc != 0 )); then
        compose logs --no-color downstream >&2 || true
    fi
    compose down -v --remove-orphans >&2 || rc=1
    if (( rc == 0 )); then
        rm -f "$work/bin/upstream" "$work/bin/downstream" "$work/downstream.properties" \
            "$work"/{good,missing,wrong-route}.{out,err}
        rmdir "$work/bin" "$work"
    else
        echo "Calibration evidence: $work" >&2
    fi
    exit "$rc"
}
trap cleanup EXIT

case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "Unsupported Docker architecture" >&2; exit 2 ;;
esac
mkdir "$work/bin"
chmod 0755 "$work" "$work/bin"
cd "$ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$work/bin/" ./src/cmd/upstream ./src/cmd/downstream
cat >"$work/downstream.properties" <<'EOF'
id=CAL-DS
nic=eth0
targetIP=0.0.0.0
targetPort=1234
transport=udp
target=kafka
bootstrapServers=target-kafka:9092
topic=transfer
internalTopic=airgap-internal
clientId=cal-downstream
numReceivers=1
batchSize=64
logLevel=INFO
EOF
compose up -d --wait target-kafka
topics() { compose exec -T target-kafka kafka-topics --bootstrap-server localhost:9092 "$@"; }
for topic in transfer airgap-internal airgap-calibration-test; do
    topics --create --topic "$topic" --partitions 2 --config retention.ms=60000
done
compose up -d downstream
deadline=$((SECONDS + 60))
until compose logs --no-color downstream | grep -E 'UDP socket [0-9]+: SO_RCVBUF requested=' >/dev/null; do
    (( SECONDS < deadline )) || { echo "Downstream did not bind UDP" >&2; exit 1; }
    sleep 1
done
calibrate() {
    compose run --rm --no-deps -T --entrypoint /airgap/work/bin/upstream driver \
        --calibrate --address="$1" --topic="$2" --brokers=target-kafka:9092 \
        --rates=1000,2000 --duration=1s --drain=3s --burst=8
}
calibrate downstream:1234 airgap-calibration-test >"$work/good.out" 2>"$work/good.err"
awk -F= '
    NR==1 && $1=="udpPacingDatagramsPerSecond" && $2~/^[0-9]+$/ && $2>=800 && $2<=1600 { rate=$2; next }
    NR==2 && $1=="udpPacingBytesPerSecond" && rate>0 && $2==rate*1400 { byte_rate=1; next }
    NR==3 && $0=="udpPacingBurstDatagrams=8" { burst=1; next }
    { exit 1 }
    END { if (NR!=3 || !rate || !byte_rate || !burst) exit 1 }
' "$work/good.out"
awk '
    /^calibration: cap=/ {
        split($6, sent, "="); split($7, verified, "=")
        if (sent[2] <= 0 || sent[2] != verified[2]) exit 1
        count++
    }
    END { if (count != 3) exit 1 }
' "$work/good.err"

if calibrate downstream:1234 airgap-calibration-missing >"$work/missing.out" 2>"$work/missing.err"; then
    echo "Missing topic should fail" >&2; exit 1
fi
[[ ! -s "$work/missing.out" ]]
grep -q 'read pre-created verification topic' "$work/missing.err"
if topics --list | grep -qx airgap-calibration-missing; then
    echo "Calibration must not create topics" >&2; exit 1
fi

if calibrate downstream:1235 airgap-calibration-test >"$work/wrong-route.out" 2>"$work/wrong-route.err"; then
    echo "Unreachable downstream UDP should fail" >&2; exit 1
fi
[[ ! -s "$work/wrong-route.out" ]]
grep -q 'routing probe' "$work/wrong-route.err"
cat "$work/good.err" "$work/good.out"
echo "Calibration Docker integration passed (3 scenarios)."
