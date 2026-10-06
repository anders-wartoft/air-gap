#!/usr/bin/env bash
# TC-14 post-drain hook — invoked by run-testcase.sh via POST_DRAIN_SCRIPT
# after the LG producer has finished and the pipeline has drained, but
# BEFORE the sink is sent SIGTERM for its final summary.
#
# Implements the manual TESTCASES.md TC-14 steps:
#   1. `create` dumps ALL current gaps from kafka-downstream[gaps] into a
#      JSON bundle file (shared tmp/ volume).
#   2. `resend` reads that bundle, pulls the original payloads for each
#      gapped offset from kafka-upstream[transfer], and re-sends them via
#      UDP to the SAME downstream container — exactly as if upstream had
#      delivered them the first time.
#   3. Wait for the resent events to propagate: downstream → UDP →
#      kafka-downstream[transfer] → dedup (recognizes the original
#      topic_partition_offset id → GAP_FILL, not a new unique event) →
#      kafka-downstream[dedup] → lg-sink.
#
# After this script returns, run-testcase.sh proceeds to SIGTERM the sink
# and capture its final summary — which should now show all gaps closed
# (received == sent, missing == 0) because of step 3.
#
# Env inherited from run-testcase.sh's manifest (set -o allexport across
# `source testcases/14.env`): CREATE_CONFIG, RESEND_CONFIG,
# TC14_BUNDLE_FILE, TC14_PROPAGATION_WAIT, TC14_RESEND_EPS.
# Explicitly passed: PROJECT_NAME, TC_ID.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

BUNDLE_BASENAME="${TC14_BUNDLE_FILE:-resend-14-all.json}"
BUNDLE_HOST_PATH="../../tmp/${BUNDLE_BASENAME}"
BUNDLE_CONTAINER_PATH="/airgap/tmp/${BUNDLE_BASENAME}"
PROPAGATION_WAIT="${TC14_PROPAGATION_WAIT:-20}"
RESEND_EPS="${TC14_RESEND_EPS:-50}"

echo "tc14-resend: ── step 1: export all gaps via create → ${BUNDLE_HOST_PATH} ──"
rm -f "$BUNDLE_HOST_PATH"
# NOTE: the trailing `create` repeats the service name as the override
# COMMAND. `docker compose run SERVICE [COMMAND] [ARGS...]` replaces the
# compose-file `command:` entirely once ANY token follows SERVICE — so a
# bare `-- --resendFileName=...` (no repeated service name) makes "--"
# itself the override command, which entrypoint.sh then tries to exec as
# a binary name and fails ("binary not found at /airgap/bin/--"). The
# entrypoint's first arg must stay "create" (that's its role selector),
# with the real CLI overrides following as additional args — this is
# exactly what the entrypoint forwards to the Go binary as os.Args[2:].
if docker compose --profile create run --rm create create \
        --resendFileName="$BUNDLE_CONTAINER_PATH" --limit=all; then
    report_check "create (export all gaps)" "exit 0" "exit 0" 0
else
    report_check "create (export all gaps)" "exit 0" "non-zero exit" 1 || true
    exit 1
fi

if [[ -s "$BUNDLE_HOST_PATH" ]]; then
    report_check "Bundle file ${BUNDLE_HOST_PATH}" "exists and non-empty" "exists and non-empty" 0
else
    report_check "Bundle file ${BUNDLE_HOST_PATH}" "exists and non-empty" "missing or empty" 1 || true
    exit 1
fi
gap_entries="$(grep -c '"partition"' "$BUNDLE_HOST_PATH" || true)"
if (( gap_entries > 0 )); then
    report_check "Bundle partition entries" "> 0" "${gap_entries}" 0
else
    report_check "Bundle partition entries" "> 0" "0 — nothing for resend to fill (was deliverFilter actually dropping events?)" 1 || true
    exit 1
fi

echo "tc14-resend: ── step 2: replay bundle via resend (eps=${RESEND_EPS}) ──"
# Same repeated-service-name convention as the create invocation above.
if docker compose --profile resend run --rm resend resend \
        --resendFileName="$BUNDLE_CONTAINER_PATH" --eps="$RESEND_EPS"; then
    report_check "resend (replay gaps)" "exit 0" "exit 0" 0
else
    report_check "resend (replay gaps)" "exit 0" "non-zero exit" 1 || true
    exit 1
fi

echo "tc14-resend: ── step 3: waiting ${PROPAGATION_WAIT}s for resent events to reach the sink ──"
echo "tc14-resend:   path: resend -> UDP -> downstream -> kafka-downstream[transfer] -> dedup (GAP_FILL) -> kafka-downstream[dedup] -> lg-sink"
sleep "$PROPAGATION_WAIT"

echo "tc14-resend: post-drain hook complete"
