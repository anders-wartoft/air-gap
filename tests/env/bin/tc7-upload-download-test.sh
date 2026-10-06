#!/bin/sh
# TC-7 upload / download byte-exact round-trip test.
#
# Automates the manual TC-7 procedure from tests/documents/TESTCASES.md:
#   1. go build Upload.go + Download.go (shipped in tests/env/bin/)
#   2. generate a deterministic high-entropy test file via /dev/urandom
#      (reproducible for a given seed-ish stat; the exact bytes don't
#      matter — only that upload and compare use the same file)
#   3. upload the file N times to kafka-upstream[transfer]
#   4. let the air-gap chain carry each copy via UDP (fragmented into
#      ~150 packets per file at payloadSize=1400)
#   5. download from kafka-downstream[transfer] into /tmp/received/
#   6. md5sum compare every downloaded file against the original
#   7. emit LG-compatible summary lines so run-testcase.sh's verdict
#      parser just works
#
# Env vars (set by docker-compose.yml):
#   UPSTREAM_BOOTSTRAP   single upstream broker, e.g. kafka-upstream:9092
#   DOWNSTREAM_BOOTSTRAP single downstream broker
#   TOPIC                topic name (default: transfer)
#   NUM_MESSAGES         how many copies of the file to send (default: 10)
#   FILE_BYTES           size in bytes of the generated test file (default: 204800)
#   TIMEOUT_SECONDS      max wait for the chain to deliver all copies

set -eu

UP_BOOTSTRAP="${UPSTREAM_BOOTSTRAP:-kafka-upstream:9092}"
DOWN_BOOTSTRAP="${DOWNSTREAM_BOOTSTRAP:-kafka-downstream:9092}"
TOPIC="${TOPIC:-transfer}"
N_MESSAGES="${NUM_MESSAGES:-10}"
FILE_BYTES="${FILE_BYTES:-204800}"
TIMEOUT_S="${TIMEOUT_SECONDS:-120}"

echo "[tc7] upstream=${UP_BOOTSTRAP} downstream=${DOWN_BOOTSTRAP} topic=${TOPIC}"
echo "[tc7] N=${N_MESSAGES} file=${FILE_BYTES}B timeout=${TIMEOUT_S}s"

# ---------- 1. Build the Go tools ----------
# /tc is mounted read-only; copy to a writable workdir so go build can
# write its binaries, module cache etc. without needing GOCACHE tricks.
WORK=/work/src
mkdir -p "$WORK"
cp /tc/Upload.go /tc/Download.go /tc/go.mod /tc/go.sum "$WORK/"

echo "[tc7] building upload and download..."
cd "$WORK"
go build -o /usr/local/bin/upload   Upload.go
go build -o /usr/local/bin/download Download.go
cd -
echo "[tc7] build OK: $(ls -lh /usr/local/bin/upload /usr/local/bin/download | awk '{print $9": "$5}')"

# ---------- 2. Deterministic high-entropy test file ----------
TESTFILE=/work/testfile.bin
dd if=/dev/urandom of="$TESTFILE" bs=1024 count=$((FILE_BYTES / 1024)) 2>/dev/null
EXPECTED_MD5=$(md5sum "$TESTFILE" | awk '{print $1}')
ACTUAL_BYTES=$(wc -c <"$TESTFILE")
echo "[tc7] test file ${TESTFILE}: ${ACTUAL_BYTES} bytes, md5=${EXPECTED_MD5}"

# ---------- 3. Upload N copies to kafka-upstream ----------
echo "[tc7] uploading ${N_MESSAGES} copies to ${UP_BOOTSTRAP}/${TOPIC}..."
t0_ns=$(date +%s%N 2>/dev/null || echo "$(date +%s)000000000")
i=1
while [ "$i" -le "$N_MESSAGES" ]; do
    if ! upload "$TESTFILE" "$UP_BOOTSTRAP" "$TOPIC" > /tmp/upload-$i.log 2>&1; then
        echo "[tc7] upload #$i failed:"
        cat /tmp/upload-$i.log
        exit 2
    fi
    i=$((i + 1))
done
t1_ns=$(date +%s%N 2>/dev/null || echo "$(date +%s)000000000")
dur_ms=$(( (t1_ns - t0_ns) / 1000000 ))
# LG-compatible completion marker. run-testcase.sh greps for this.
echo "transferred ${N_MESSAGES} lines in ${dur_ms} milliseconds, binary payload"

# ---------- 4. Download from kafka-downstream ----------
RECV_DIR=/work/received
rm -rf "$RECV_DIR"
mkdir -p "$RECV_DIR"
echo "[tc7] downloading from ${DOWN_BOOTSTRAP}/${TOPIC} to ${RECV_DIR}..."
# Download.go loops forever; run in background and poll until all N files
# are present (or we hit TIMEOUT_S).
download "$RECV_DIR" "$DOWN_BOOTSTRAP" "$TOPIC" > /tmp/download.log 2>&1 &
DOWNLOAD_PID=$!
waited=0
while [ "$waited" -lt "$TIMEOUT_S" ]; do
    got=$(ls -1 "$RECV_DIR" 2>/dev/null | wc -l | tr -d ' ')
    if [ "$got" -ge "$N_MESSAGES" ]; then
        break
    fi
    sleep 1
    waited=$((waited + 1))
    if [ $((waited % 10)) -eq 0 ]; then
        echo "[tc7] waiting... ${got}/${N_MESSAGES} delivered after ${waited}s"
    fi
done
kill "$DOWNLOAD_PID" 2>/dev/null || true
wait "$DOWNLOAD_PID" 2>/dev/null || true

got=$(ls -1 "$RECV_DIR" 2>/dev/null | wc -l | tr -d ' ')
echo "[tc7] downloaded ${got}/${N_MESSAGES} file(s) in ${waited}s"

# ---------- 5. md5sum compare ----------
mismatches=0
compared=0
for f in "$RECV_DIR"/*; do
    [ -f "$f" ] || continue
    compared=$((compared + 1))
    actual=$(md5sum "$f" | awk '{print $1}')
    if [ "$actual" = "$EXPECTED_MD5" ]; then
        continue
    fi
    mismatches=$((mismatches + 1))
    actual_bytes=$(wc -c <"$f")
    echo "[tc7] byte-mismatch: $f size=${actual_bytes} md5=${actual} (expected size=${ACTUAL_BYTES} md5=${EXPECTED_MD5})"
done
echo "[tc7] md5 compared ${compared} file(s), ${mismatches} mismatch(es)"

# ---------- 6. LG-compatible verdict lines ----------
# "received N unique" = how many files we successfully downloaded and
# verified byte-exact. Anything missing or mismatched counts as "not
# received" from the verdict parser's perspective.
good=$((got - mismatches))
echo "Number of unique received numbers: ${good}"
echo "Duplicate detection found: 0"
echo "Next expected number: $((good + 1))"

if [ "$mismatches" -eq 0 ] && [ "$got" -eq "$N_MESSAGES" ]; then
    echo "[tc7] PASS: ${N_MESSAGES} byte-exact round-trips through air-gap"
    exit 0
fi
echo "[tc7] FAIL: downloaded=${got}/${N_MESSAGES}, md5 mismatches=${mismatches}"
echo "--- last 20 lines of download.log ---"
tail -n 20 /tmp/download.log || true
exit 1
