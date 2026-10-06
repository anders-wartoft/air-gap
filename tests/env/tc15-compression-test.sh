#!/usr/bin/env bash
# TC-15 post-drain hook — invoked by run-testcase.sh via POST_DRAIN_SCRIPT.
#
# The "main" LG_PRODUCER_CONFIG phase (short messages, see 15.env) only
# exists to give run-testcase.sh an auto-exit anchor quickly; it is NOT
# captured or asserted on. This script performs the actual TC-15 test as
# three self-contained sub-phases, each: start a tcpdump capture on the
# downstream container's wire interface, produce a one-shot burst of
# known content via a fresh lg-producer container, stop the capture, then
# assert both (a) what is/isn't visible on the wire and (b) what Kafka
# ends up with after downstream decompresses/decrypts it.
#
#   Phase A — short messages (<100 bytes), no encryption:
#     compressWhenLengthExceeds=100 means these stay UNcompressed, so the
#     plaintext MUST be visible on the wire capture.
#   Phase B — long messages (>100 bytes), no encryption:
#     these exceed the threshold and get gzip-compressed, so the plaintext
#     must NOT appear in the capture (opaque compressed bytes instead).
#   Phase C — long messages (>100 bytes), WITH encryption:
#     upstream-a is swapped to upstream-airgap-15b-docker.properties
#     (encryption=true). Plaintext must still not appear (now ciphertext).
#
# In all three phases, kafka-downstream[transfer] must show the correct
# ORIGINAL plaintext — downstream decompresses (and decrypts, phase C)
# before writing to Kafka, regardless of what the wire looked like.
#
# Env inherited from run-testcase.sh's manifest (set -o allexport across
# `source testcases/15.env`): UPSTREAM_A_CONFIG, TC15_SHORT_LG_CONFIG,
# TC15_LONG_LG_CONFIG, TC15_ENCRYPTED_UPSTREAM_CONFIG, TC15_SHORT_MARKER,
# TC15_LONG_MARKER, TC15_CAPTURE_SECONDS, TC15_ENCRYPTION_SETTLE_SECONDS.
# Explicitly passed: PROJECT_NAME, TC_ID.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
DOWNSTREAM_CTR="${PROJECT}-downstream-1"
KAFKA_DOWN_BROKER="${PROJECT}-kafka-downstream-1"

SHORT_LG_CONFIG="${TC15_SHORT_LG_CONFIG:-/airgap/config/testcases/upstream-lg-15.properties}"
LONG_LG_CONFIG="${TC15_LONG_LG_CONFIG:-/airgap/config/testcases/upstream-lg-15b.properties}"
ENCRYPTED_UPSTREAM_CONFIG="${TC15_ENCRYPTED_UPSTREAM_CONFIG:-/airgap/config/testcases/upstream-airgap-15b-docker.properties}"
# Unique substrings present in ONLY one of the two LG message templates —
# see upstream-lg-15.properties ("should not be longer") vs
# upstream-lg-15b.properties ("To get the message longer").
SHORT_MARKER="${TC15_SHORT_MARKER:-should not be longer than 100 bytes}"
LONG_MARKER="${TC15_LONG_MARKER:-To get the message longer than 100 bytes}"
CAPTURE_SECONDS="${TC15_CAPTURE_SECONDS:-3}"
ENCRYPTION_SETTLE_SECONDS="${TC15_ENCRYPTION_SETTLE_SECONDS:-8}"

overall_rc=0

ensure_container() {
    local ctr="$1"
    if ! docker inspect "$ctr" >/dev/null 2>&1; then
        report_check "Container ${ctr}" "running" "NOT running" 1
        return 1
    fi
    return 0
}

# Runs a one-shot lg-producer burst with the given LG_CONFIG. Blocks
# until the producer exits (docker compose run is synchronous).
run_producer() {
    local lg_config="$1"
    LG_PRODUCER_CONFIG="$lg_config" \
        docker compose --profile lg-producer run --rm lg-producer \
        >/tmp/tc15-producer-out.log 2>&1
    return $?
}

# Captures UDP:1234 traffic on the downstream container for the duration
# of a producer burst, then greps the capture for a marker substring.
# Echoes "PRESENT" or "ABSENT" (what was actually observed) to stdout;
# the caller compares that against what it expected for the phase.
capture_and_check() {
    local phase_name="$1" lg_config="$2" marker="$3"
    local capture_file="/tmp/tc15-capture-${phase_name}.txt"

    echo "tc15: ── ${phase_name}: starting tcpdump capture on ${DOWNSTREAM_CTR} ──" >&2
    docker exec "$DOWNSTREAM_CTR" sh -c \
        "tcpdump -l -A -i eth0 udp port 1234" \
        >"$capture_file" 2>/tmp/tc15-tcpdump-stderr-${phase_name}.log &
    local tcpdump_bg_pid=$!
    sleep 1  # let tcpdump actually attach before traffic starts

    echo "tc15: ── ${phase_name}: producing burst via ${lg_config} ──" >&2
    if ! run_producer "$lg_config"; then
        echo "tc15: FAIL — lg-producer failed for ${phase_name}; see /tmp/tc15-producer-out.log" >&2
        tail -20 /tmp/tc15-producer-out.log >&2 || true
    fi

    sleep "$CAPTURE_SECONDS"  # let UDP propagate through to downstream

    echo "tc15: ── ${phase_name}: stopping capture ──" >&2
    docker exec "$DOWNSTREAM_CTR" sh -c "pkill tcpdump" >/dev/null 2>&1 || true
    wait "$tcpdump_bg_pid" 2>/dev/null || true

    local bytes_captured
    bytes_captured="$(wc -c < "$capture_file" 2>/dev/null || echo 0)"
    echo "tc15: ${phase_name}: captured ${bytes_captured} byte(s) of tcpdump output" >&2

    if grep -qF "$marker" "$capture_file" 2>/dev/null; then
        echo "PRESENT"
    else
        echo "ABSENT"
    fi
}

# Reads kafka-downstream[transfer] from the beginning and checks whether
# the given marker substring appears anywhere (proves downstream
# correctly decompressed/decrypted at least one message from this phase,
# regardless of what earlier phases also left in the topic).
check_kafka_content() {
    local phase_name="$1" marker="$2"
    local out
    out="$(docker exec "$KAFKA_DOWN_BROKER" kafka-console-consumer \
                --bootstrap-server kafka-downstream:9092 \
                --topic transfer --from-beginning \
                --timeout-ms 8000 2>/dev/null || true)"
    if printf '%s' "$out" | grep -qF "$marker"; then
        report_check "${phase_name} kafka-downstream[transfer] content" "contains decoded plaintext" "found" 0
    else
        report_check "${phase_name} kafka-downstream[transfer] content" "contains decoded plaintext" "NOT found (didn't decompress/decrypt correctly, or never arrived)" 1
    fi
}

ensure_container "$DOWNSTREAM_CTR" || exit 2
ensure_container "$KAFKA_DOWN_BROKER" || exit 2

echo
echo "═══ Phase A: short messages (<100 bytes), no encryption — expect PLAINTEXT on the wire ═══"
wire_state="$(capture_and_check phaseA "$SHORT_LG_CONFIG" "$SHORT_MARKER")"
if [[ "$wire_state" == "PRESENT" ]]; then
    report_check "Phase A wire capture (short, uncompressed)" "plaintext visible" "found" 0 || overall_rc=1
else
    report_check "Phase A wire capture (short, uncompressed)" "plaintext visible" "NOT found" 1 || overall_rc=1
fi
check_kafka_content phaseA "$SHORT_MARKER" || overall_rc=1

echo
echo "═══ Phase B: long messages (>100 bytes), no encryption — expect NO plaintext on the wire (compressed) ═══"
wire_state="$(capture_and_check phaseB "$LONG_LG_CONFIG" "$LONG_MARKER")"
if [[ "$wire_state" == "ABSENT" ]]; then
    report_check "Phase B wire capture (long, compressed)" "plaintext absent (gzip)" "not found" 0 || overall_rc=1
else
    report_check "Phase B wire capture (long, compressed)" "plaintext absent (gzip)" "FOUND — not compressed" 1 || overall_rc=1
fi
check_kafka_content phaseB "$LONG_MARKER" || overall_rc=1

echo
echo "═══ Phase C: long messages (>100 bytes), WITH encryption — expect NO plaintext on the wire (ciphertext) ═══"
echo "swapping upstream-a to ${ENCRYPTED_UPSTREAM_CONFIG}"
if ! UPSTREAM_A_CONFIG="$ENCRYPTED_UPSTREAM_CONFIG" \
        docker compose up -d --force-recreate --no-deps upstream-a \
        </dev/null >/dev/null 2>&1; then
    report_check "upstream-a recreate with encrypted config" "success" "docker compose up -d failed" 1 || overall_rc=1
else
    echo "settling ${ENCRYPTION_SETTLE_SECONDS}s for the symmetric-key exchange to complete"
    sleep "$ENCRYPTION_SETTLE_SECONDS"
    wire_state="$(capture_and_check phaseC "$LONG_LG_CONFIG" "$LONG_MARKER")"
    if [[ "$wire_state" == "ABSENT" ]]; then
        report_check "Phase C wire capture (long, encrypted+compressed)" "plaintext absent (ciphertext)" "not found" 0 || overall_rc=1
    else
        report_check "Phase C wire capture (long, encrypted+compressed)" "plaintext absent (ciphertext)" "FOUND — not encrypted" 1 || overall_rc=1
    fi
    check_kafka_content phaseC "$LONG_MARKER" || overall_rc=1
fi

echo
report_summary "testcase 15" "$overall_rc"
exit "$overall_rc"
