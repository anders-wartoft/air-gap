#!/usr/bin/env bash
# TC-19 post-drain hook — invoked by run-testcase.sh via POST_DRAIN_SCRIPT.
#
# The main LG_PRODUCER_CONFIG burst (set in testcases/19.env) is only a
# quick auto-exit ANCHOR so run-testcase.sh has something to wait on —
# its TEST_N counter content never matches any of the deny rules in
# upstream-airgap-filter-19.txt, so it's harmless noise alongside the
# real assertions below.
#
# This script produces a set of hand-crafted literal payloads directly
# to kafka-upstream[transfer] via kafka-console-producer (bypassing
# LogGenerator, which has no "send this exact literal string" mode),
# one per line, covering every deny rule in the current
# upstream-airgap-filter-19.txt:
#   deny:\b\d{3}-\d{2}-\d{4}\b                                  (SSN)
#   deny:(?i)[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}              (email)
#   deny:\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b              (credit card)
#   deny:(?i)(password|passwd|pwd|secret|token|api_key)\s*[:=]  (credentials)
# plus a bare "password" with no trailing colon/equals, which should
# NOT match the credentials rule and fall through to
# inputFilterDefaultAction=allow.
#
# Verifies:
#   1. Each ALLOWED payload's exact text appears in
#      kafka-downstream[transfer] (passed the filter, forwarded intact).
#   2. Each BLOCKED payload's exact text does NOT appear there (input
#      filter cleared the payload before it was ever sent on the wire —
#      see src/upstream/upstream.go's "Clear payload but still send so
#      gap-detector sees the offset").
#   3. Upstream's own STATISTICS log shows total_filtered/
#      total_unfiltered matching the expected counts (the LG anchor
#      burst's 100 TEST_N messages all count as unfiltered too).
#
# Env inherited from run-testcase.sh's manifest (set -o allexport across
# `source testcases/19.env`): TC19_LG_ANCHOR_LIMIT.
# Explicitly passed: PROJECT_NAME, TC_ID.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
UP_BROKER="${PROJECT}-kafka-upstream-1"
DOWN_BROKER="${PROJECT}-kafka-downstream-1"
UPSTREAM_CTR="${PROJECT}-upstream-a-1"
LG_ANCHOR_LIMIT="${TC19_LG_ANCHOR_LIMIT:-100}"
PROPAGATION_WAIT="${TC19_PROPAGATION_WAIT:-8}"

overall_rc=0

ensure_container() {
    local ctr="$1"
    if ! docker inspect "$ctr" >/dev/null 2>&1; then
        report_check "Container ${ctr}" "running" "NOT running" 1
        return 1
    fi
    return 0
}
ensure_container "$UP_BROKER" || exit 2
ensure_container "$DOWN_BROKER" || exit 2
ensure_container "$UPSTREAM_CTR" || exit 2

# Payload list: "EXPECT<TAB>literal text". EXPECT is ALLOW or BLOCK.
# Each payload is spaced so the sensitive pattern has a word-boundary
# before it (e.g. a space before digits/letters) — a marker glued
# directly onto the pattern via '_' would itself be a word character
# and could suppress \b matching.
read -r -d '' PAYLOADS <<'EOF' || true
ALLOW	TC19 ALLOW plain hello world
BLOCK	TC19 BLOCK ssn 123-45-6789 end
BLOCK	TC19 BLOCK email anders@sitia.nu end
BLOCK	TC19 BLOCK EMAIL ANDERS@SITIA.NU END
BLOCK	TC19 BLOCK cc 4111 1111 1111 1111 end
BLOCK	TC19 BLOCK credentials password: hunter2
ALLOW	TC19 ALLOW password bare word
BLOCK	TC19 BLOCK apikey api_key=abc123xyz end
EOF

echo "tc19: ── producing $(printf '%s\n' "$PAYLOADS" | grep -c .) literal payloads to kafka-upstream[transfer] ──" >&2
printf '%s\n' "$PAYLOADS" | cut -f2 | \
    docker exec -i "$UP_BROKER" kafka-console-producer \
        --bootstrap-server kafka-upstream:9092 --topic transfer \
        >/tmp/tc19-producer.log 2>&1
producer_rc=$?
if (( producer_rc != 0 )); then
    report_check "kafka-console-producer invocation" "exit 0" "exit ${producer_rc}" 1
    cat /tmp/tc19-producer.log >&2 || true
    overall_rc=1
fi
rm -f /tmp/tc19-producer.log

echo "tc19: waiting ${PROPAGATION_WAIT}s for payloads to flow through upstream (filter) -> UDP -> downstream -> kafka-downstream[transfer]" >&2
sleep "$PROPAGATION_WAIT"

echo "tc19: ── reading kafka-downstream[transfer] ──" >&2
downstream_content="$(docker exec "$DOWN_BROKER" kafka-console-consumer \
    --bootstrap-server kafka-downstream:9092 --topic transfer \
    --from-beginning --timeout-ms 8000 2>/dev/null || true)"

while IFS=$'\t' read -r expect text; do
    [[ -z "$expect" ]] && continue
    if printf '%s' "$downstream_content" | grep -qF "$text"; then
        found="PRESENT"
    else
        found="ABSENT"
    fi
    if [[ "$expect" == "ALLOW" ]]; then
        if [[ "$found" == "PRESENT" ]]; then
            report_check "Input \"${text}\"" "allowed (found in Kafka)" "found in Kafka" 0 || overall_rc=1
        else
            report_check "Input \"${text}\"" "allowed (found in Kafka)" "NOT found in Kafka" 1 || overall_rc=1
        fi
    else
        if [[ "$found" == "ABSENT" ]]; then
            report_check "Input \"${text}\"" "blocked" "not found" 0 || overall_rc=1
        else
            report_check "Input \"${text}\"" "blocked" "FOUND in Kafka" 1 || overall_rc=1
        fi
    fi
done <<< "$PAYLOADS"

# ── Statistics counters ──
# Expected: LG anchor burst contributes LG_ANCHOR_LIMIT unfiltered
# events (TEST_N never matches any deny rule); the 8 hand-crafted
# payloads above split 2 allow / 6 block (computed below from
# $PAYLOADS directly rather than hardcoded, so this stays correct if
# the payload list above is ever edited).
allow_count="$(printf '%s\n' "$PAYLOADS" | awk -F'\t' '$1=="ALLOW"' | grep -c .)"
block_count="$(printf '%s\n' "$PAYLOADS" | awk -F'\t' '$1=="BLOCK"' | grep -c .)"
expected_unfiltered=$(( LG_ANCHOR_LIMIT + allow_count ))
expected_filtered="$block_count"

echo "── checking upstream STATISTICS counters ──"
stats_line="$(docker logs "$UPSTREAM_CTR" 2>&1 | grep -F 'STATISTICS:' | tail -1)"
echo "last STATISTICS line: ${stats_line:-<none found>}"
total_filtered="$(printf '%s' "$stats_line" | sed -n 's/.*"total_filtered":\([0-9]*\).*/\1/p')"
total_unfiltered="$(printf '%s' "$stats_line" | sed -n 's/.*"total_unfiltered":\([0-9]*\).*/\1/p')"
total_filtered="${total_filtered:-0}"
total_unfiltered="${total_unfiltered:-0}"

if [[ "$total_filtered" == "$expected_filtered" ]]; then
    report_check "Upstream total_filtered counter" "${expected_filtered}" "${total_filtered}" 0 || overall_rc=1
else
    report_check "Upstream total_filtered counter" "${expected_filtered}" "${total_filtered}" 1 || overall_rc=1
fi
# >= rather than == for unfiltered: the LG anchor and the hand-crafted
# ALLOW payloads may straddle two STATISTICS intervals depending on
# timing, but total_unfiltered is a monotonically increasing counter
# so it should never be LESS than what we know was sent.
if (( total_unfiltered >= expected_unfiltered )); then
    report_check "Upstream total_unfiltered counter" ">= ${expected_unfiltered}" "${total_unfiltered}" 0 || overall_rc=1
else
    report_check "Upstream total_unfiltered counter" ">= ${expected_unfiltered}" "${total_unfiltered}" 1 || overall_rc=1
fi

echo
report_summary "testcase 19" "$overall_rc"
exit "$overall_rc"
