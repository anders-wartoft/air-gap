#!/usr/bin/env bash
# TC-17 verifier — prove dedup actually used TLS to reach Kafka, not
# that the test happened to pass via a silent PLAINTEXT fallback.
#
# Checks:
#   1. dedup's own startup log shows BOOTSTRAP_SERVERS pointing at the
#      SSL-labeled hostname:port (kafka-downstream.sitia.nu:9094,...),
#      proving DEDUP_BOOTSTRAP actually reached the container (not the
#      PLAINTEXT kafka-downstream:9092 default).
#   2. No SSL/TLS handshake or authentication failures appear anywhere
#      in dedup's log — a wrong truststore, expired cert, or hostname
#      mismatch would show up as SSLHandshakeException /
#      SslAuthenticationException / "Failed to..." retries.
#   3. dedup's own counters show it actually RECEIVED and EMITTED
#      events (total_received > 0, total_emitted > 0 in the last
#      MISSING-REPORT cycle) — if the SSL handshake failed outright,
#      Kafka Streams would be stuck retrying forever and nothing would
#      ever reach the dedup topic, so this is the strongest practical
#      proof that TLS didn't just get configured but actually WORKED.
#
# Env (set by run-testcase.sh): TC_ID, PROJECT_NAME, VERIFY_RC_IN
# Optional: TC17_PARTITION_COUNT (default 15, matches RAW_TOPICS=transfer
# partition count in this Docker rig).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
DEDUP_CTR="${PROJECT}-dedup-1"
PARTITION_COUNT="${TC17_PARTITION_COUNT:-15}"

echo "─── verify-tc17: did dedup actually use TLS to reach Kafka? ───"

if ! docker inspect "$DEDUP_CTR" >/dev/null 2>&1; then
    report_check "Container ${DEDUP_CTR}" "running" "NOT running" 1 || true
    exit 2
fi

dedup_log="$(docker logs "$DEDUP_CTR" 2>&1 || true)"
if [[ -z "$dedup_log" ]]; then
    report_check "${DEDUP_CTR} log output" "non-empty" "empty" 1 || true
    exit 2
fi

rc=0

# ── 1. BOOTSTRAP_SERVERS points at the SSL-labeled hostname:port ──
bootstrap_line="$(printf '%s\n' "$dedup_log" | grep -F 'BOOTSTRAP_SERVERS=' | head -1)"
if printf '%s' "$bootstrap_line" | grep -qE 'kafka-downstream\.sitia\.nu:(9094|8094)'; then
    report_check "Dedup BOOTSTRAP_SERVERS" "SSL port (9094/8094)" "${bootstrap_line#*=}" 0 || rc=1
else
    report_check "Dedup BOOTSTRAP_SERVERS" "SSL port (9094/8094)" "${bootstrap_line:-<not found>}" 1 || rc=1
fi

# ── 2. No SSL/TLS failure signatures anywhere in the log ──
ssl_error_pattern='SSLHandshakeException|SslAuthenticationException|SSLException|certificate_unknown|unable to find valid certification path'
ssl_errors="$(printf '%s\n' "$dedup_log" | grep -ciE "$ssl_error_pattern" || true)"
ssl_errors="${ssl_errors:-0}"
if (( ssl_errors == 0 )); then
    report_check "SSL/TLS error signatures in dedup log" "0" "0" 0 || rc=1
else
    report_check "SSL/TLS error signatures in dedup log" "0" "${ssl_errors} found" 1 || rc=1
    printf '%s\n' "$dedup_log" | grep -iE "$ssl_error_pattern" | head -5 >&2
fi

# ── 3. dedup's own counters show real traffic flowed over the TLS connection ──
report_lines="$(printf '%s\n' "$dedup_log" | grep -F '[MISSING-REPORT]' || true)"
last_cycle="$(printf '%s\n' "$report_lines" | tail -n "$PARTITION_COUNT")"
sum_total_received="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_received":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
sum_total_emitted="$(printf '%s\n' "$last_cycle" \
    | sed -n 's/.*"total_emitted":\([0-9]*\).*/\1/p' \
    | awk '{ s += $1 } END { print s+0 }')"
if (( sum_total_received > 0 )); then
    report_check "Dedup total_received over TLS" "> 0" "${sum_total_received}" 0 || rc=1
else
    report_check "Dedup total_received over TLS" "> 0" "0 — handshake likely never succeeded" 1 || rc=1
fi
if (( sum_total_emitted > 0 )); then
    report_check "Dedup total_emitted over TLS" "> 0" "${sum_total_emitted}" 0 || rc=1
else
    report_check "Dedup total_emitted over TLS" "> 0" "0" 1 || rc=1
fi

report_summary "verify-tc17" "$rc"
exit "$rc"
