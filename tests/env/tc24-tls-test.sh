#!/usr/bin/env bash
# TC-24 post-drain hook — invoked by run-testcase.sh via POST_DRAIN_SCRIPT
# after tc24-settle-timer exits. Does ALL of TC-24's actual testing in one
# continuous flow (Part A/cleartext-baseline is skipped — already covered
# by TC-1/3/22):
#
#   Part B — mTLS, cleartext rejected, two concurrent clients (REQ-41,
#            REQ-42, REQ-44):
#     1. Confirm upstream-a's initial mTLS handshake with downstream
#        already succeeded (both sides authenticated each other's CN).
#     2. Raw-bytes probe against the TLS port: downstream must log a
#        handshake failure and KEEP RUNNING (not crash).
#     3. Start upstream-b (second mTLS client, different cert/CN)
#        concurrently; confirm downstream authenticates BOTH CNs.
#
#   Part C — one-way / server-only authentication (REQ-41):
#     4. Recreate downstream with tcpTLSClientCNRegex="" (CN check
#        disabled); confirm it authenticates a client "by CA chain" —
#        i.e. without a CN pattern — rather than rejecting it.
#
#   Part D — live SIGHUP cert rotation without packet loss (REQ-47):
#     5. Restore downstream to the CN-required config; let upstream-a
#        reconnect and stream a baseline.
#     6. Overwrite upstream-a's writable cert copy with upstream2's
#        cert/key, send SIGHUP.
#     7. Confirm the reload log line + downstream re-authenticating the
#        NEW CN.
#     8. Confirm the "Random message N" sequence downstream received
#        during this whole window has zero gaps — the actual proof nothing
#        was lost across the live rotation.
#
# Env inherited from run-testcase.sh's manifest. Explicitly passed:
# PROJECT_NAME, TC_ID.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

PROJECT="${PROJECT_NAME:-airgap-testenv}"
UPSTREAM_A_CTR="${PROJECT}-upstream-a-1"
DOWNSTREAM_CTR="${PROJECT}-downstream-1"

rc=0

# ───────────────────────── Part B ─────────────────────────
echo "tc24-tls-test: ── Part B: mTLS handshake, cleartext rejection, two concurrent clients ──"

upstream_a_log="$(docker logs "$UPSTREAM_A_CTR" 2>&1 || true)"
downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"

if printf '%s\n' "$upstream_a_log" | grep -qF 'Authenticated server CN'; then
    report_check "upstream-a authenticated downstream's server CN" "found" "found" 0 || rc=1
else
    report_check "upstream-a authenticated downstream's server CN" "found" "NOT found" 1 || rc=1
fi
if printf '%s\n' "$downstream_log" | grep -qF 'Authenticated client CN "nu.sitia.airgap.upstream-1"'; then
    report_check "downstream authenticated upstream-1's client CN" "found" "found" 0 || rc=1
else
    report_check "downstream authenticated upstream-1's client CN" "found" "NOT found" 1 || rc=1
fi

echo "tc24-tls-test: ── cleartext-rejection probe (raw bytes at the TLS port) ──"
# Reuse whatever network "downstream" is actually attached to, rather than
# guessing compose's network-naming scheme.
net_name="$(docker inspect "$DOWNSTREAM_CTR" --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' 2>/dev/null || true)"
if [[ -z "$net_name" ]]; then
    report_check "Resolve downstream's docker network" "found" "NOT found" 1 || rc=1
else
    docker run --rm --network "$net_name" busybox:latest \
        sh -c 'echo "not a tls handshake" | nc -w 2 downstream 1234' >/dev/null 2>&1 || true
    sleep 2
    downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"
    if printf '%s\n' "$downstream_log" | grep -qF 'Handshake failed from'; then
        report_check "downstream logged a handshake failure for the cleartext probe" "found" "found" 0 || rc=1
    else
        report_check "downstream logged a handshake failure for the cleartext probe" "found" "NOT found" 1 || rc=1
    fi
    if docker inspect --format '{{.State.Status}}' "$DOWNSTREAM_CTR" 2>/dev/null | grep -qx running; then
        report_check "downstream still running after the cleartext probe" "running" "running" 0 || rc=1
    else
        report_check "downstream still running after the cleartext probe" "running" "NOT running" 1 || rc=1
    fi
fi

echo "tc24-tls-test: ── starting upstream-b (second concurrent mTLS client, CN upstream-2) ──"
env UPSTREAM_B_CONFIG=/airgap/config/testcases/upstream-tls-24b-docker.properties \
    docker compose --profile dual up -d --no-deps upstream-b >/dev/null 2>&1 || true
sleep 8
downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"
if printf '%s\n' "$downstream_log" | grep -qF 'Authenticated client CN "nu.sitia.airgap.upstream-2"'; then
    report_check "downstream authenticated upstream-2's client CN (concurrent)" "found" "found" 0 || rc=1
else
    report_check "downstream authenticated upstream-2's client CN (concurrent)" "found" "NOT found" 1 || rc=1
fi
docker compose stop upstream-b >/dev/null 2>&1 || true

# ───────────────────────── Part C ─────────────────────────
echo "tc24-tls-test: ── Part C: one-way / server-only authentication (CN check disabled) ──"
env DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-tls-24-noCN-docker.properties \
    docker compose up -d --force-recreate --no-deps downstream >/dev/null 2>&1 || true
echo "tc24-tls-test: waiting for upstream-a to reconnect to the recreated downstream..."
sleep 10
downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"
if printf '%s\n' "$downstream_log" | grep -qF '(CA-chain only)'; then
    report_check "downstream authenticated by CA chain only (no CN filter)" "found" "found" 0 || rc=1
else
    report_check "downstream authenticated by CA chain only (no CN filter)" "found" "NOT found" 1 || rc=1
fi
if printf '%s\n' "$downstream_log" | grep -qF 'Handshake failed from'; then
    report_check "No handshake failures with CN check disabled" "none" "found a failure" 1 || rc=1
else
    report_check "No handshake failures with CN check disabled" "none" "none" 0 || rc=1
fi

# ───────────────────────── Part D ─────────────────────────
echo "tc24-tls-test: ── Part D: live SIGHUP cert rotation without packet loss ──"
env DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-tls-24-docker.properties \
    docker compose up -d --force-recreate --no-deps downstream >/dev/null 2>&1 || true
echo "tc24-tls-test: waiting for upstream-a to reconnect and stream a baseline..."
sleep 12

echo "tc24-tls-test: overwriting upstream-a's writable cert copy with upstream2's cert/key, then SIGHUP..."
cp ../../certs/upstream2.crt    ../../tmp/tc24-rotate.crt
cp ../../certs/upstream2.key    ../../tmp/tc24-rotate.key
cp ../../certs/upstream2.key.pw ../../tmp/tc24-rotate.key.pw
docker kill --signal=HUP "$UPSTREAM_A_CTR" >/dev/null 2>&1 || true
sleep 10

upstream_a_log="$(docker logs "$UPSTREAM_A_CTR" 2>&1 || true)"
if printf '%s\n' "$upstream_a_log" | grep -qF 'TLS certificates reloaded from'; then
    report_check "upstream-a logged 'TLS certificates reloaded'" "found" "found" 0 || rc=1
else
    report_check "upstream-a logged 'TLS certificates reloaded'" "found" "NOT found" 1 || rc=1
fi

downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"
if printf '%s\n' "$downstream_log" | grep -qF 'Authenticated client CN "nu.sitia.airgap.upstream-2"'; then
    report_check "downstream re-authenticated the rotated client CN (now upstream-2)" "found" "found" 0 || rc=1
else
    report_check "downstream re-authenticated the rotated client CN (now upstream-2)" "found" "NOT found" 1 || rc=1
fi

echo "tc24-tls-test: letting it stream a bit more post-rotation before checking continuity..."
sleep 10

downstream_log="$(docker logs "$DOWNSTREAM_CTR" 2>&1 || true)"
numbers="$(printf '%s\n' "$downstream_log" | grep -oE 'Random message [0-9]+' | grep -oE '[0-9]+' | sort -n -u || true)"
total_unique="$(printf '%s\n' "$numbers" | grep -c . || true)"
total_unique="${total_unique:-0}"
if (( total_unique > 0 )); then
    min_n="$(printf '%s\n' "$numbers" | head -n 1)"
    max_n="$(printf '%s\n' "$numbers" | tail -n 1)"
    expected_count=$(( max_n - min_n + 1 ))
    if (( total_unique == expected_count )); then
        report_check "downstream sequence gaps across rotation (${min_n}..${max_n})" "none" "none — all ${expected_count} values present" 0 || rc=1
    else
        missing_count=$(( expected_count - total_unique ))
        report_check "downstream sequence gaps across rotation (${min_n}..${max_n})" "none" "${missing_count} missing out of ${expected_count}" 1 || rc=1
    fi
else
    report_check "downstream 'Random message N' lines after restore" "> 0" "0" 1 || rc=1
fi

report_summary "tc24-tls-test" "$rc"
exit "$rc"
