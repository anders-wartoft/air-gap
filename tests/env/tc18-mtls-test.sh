#!/usr/bin/env bash
# TC-18 post-drain hook — invoked by run-testcase.sh via POST_DRAIN_SCRIPT.
#
# The main LG producer/sink pair (set via LG_PRODUCER_CONFIG/LG_SINK_CONFIG
# in testcases/18.env) already proves upstream/downstream can use a
# passphrase-ENCRYPTED private key to connect to Kafka over TLS and
# deliver events end to end (100/100, no filter). This script covers the
# remaining REQ-32 behaviour: `create` and `resend` must ALSO be able to
# load an encrypted key (positive case), and must fail predictably — not
# silently or by hanging — when the password is missing (negative case).
#
# Four one-shot invocations, each with explicit env var overrides (the
# "repeat the service name as the override command" convention is
# required here — see tests/env/entrypoint.sh and TC-14's writeup in
# TESTCASES.md for why a bare `-- --flag=value` fails):
#   1. create  + correct password  -> expect exit 0
#   2. resend  + correct password  -> expect exit 0
#   3. create  + missing password  -> expect exit != 0 (Logger.Panicf)
#   4. resend  + missing password  -> expect exit != 0 (Logger.Panicf)
#
# Env inherited from run-testcase.sh's manifest (set -o allexport across
# `source testcases/18.env`): none required beyond PROJECT_NAME/TC_ID,
# which run-testcase.sh passes explicitly (same convention as TC-14).

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

BUNDLE_CONTAINER_PATH="/airgap/tmp/tc18-bundle.json"
BUNDLE_HOST_PATH="../../tmp/tc18-bundle.json"

overall_rc=0

# Runs `docker compose --profile <profile> run --rm <service> <service>
# --resendFileName=... --limit=all` (service repeated as the override
# command — see entrypoint.sh) with the given CONFIG/BOOTSTRAP env vars,
# and reports whether the exit code matched what was expected.
run_check() {
    local label="$1" service="$2" config_var="$3" config_val="$4" \
          bootstrap_var="$5" bootstrap_val="$6" expect="$7"
    shift 7
    local extra_args=("$@")

    echo "── ${label} ──"
    local rc=0
    env "${config_var}=${config_val}" "${bootstrap_var}=${bootstrap_val}" \
        docker compose --profile "$service" run --rm "$service" "$service" \
        "${extra_args[@]}" >/tmp/tc18-${service}-$$.log 2>&1 || rc=$?

    tail -5 "/tmp/tc18-${service}-$$.log" || true
    rm -f "/tmp/tc18-${service}-$$.log"

    case "$expect" in
        zero)
            if (( rc == 0 )); then
                report_check "${label}" "exit 0" "exit 0" 0
            else
                report_check "${label}" "exit 0" "exit ${rc}" 1
            fi
            ;;
        nonzero)
            if (( rc != 0 )); then
                report_check "${label}" "non-zero exit (password rejected)" "exit ${rc}" 0
            else
                report_check "${label}" "non-zero exit (password rejected)" "exit 0 — NOT rejected" 1
            fi
            ;;
    esac
}

rm -f "$BUNDLE_HOST_PATH"

echo
echo "═══ Positive case: create with the CORRECT password ═══"
run_check "create, correct password" create \
        CREATE_CONFIG /airgap/config/testcases/create-18.properties \
        CREATE_BOOTSTRAP kafka-downstream.sitia.nu:9094,kafka-downstream.sitia.nu:8094 \
        zero --resendFileName="$BUNDLE_CONTAINER_PATH" --limit=all || overall_rc=1

echo
echo "═══ Positive case: resend with the CORRECT password ═══"
run_check "resend, correct password" resend \
        RESEND_CONFIG /airgap/config/testcases/resend-18-docker.properties \
        RESEND_BOOTSTRAP kafka-upstream.sitia.nu:9094,kafka-upstream.sitia.nu:8094 \
        zero --resendFileName="$BUNDLE_CONTAINER_PATH" || overall_rc=1

echo
echo "═══ Negative case: create with the password file REMOVED ═══"
run_check "create, missing password" create \
        CREATE_CONFIG /airgap/config/testcases/create-18b.properties \
        CREATE_BOOTSTRAP kafka-downstream.sitia.nu:9094,kafka-downstream.sitia.nu:8094 \
        nonzero --resendFileName="$BUNDLE_CONTAINER_PATH" --limit=all || overall_rc=1

echo
echo "═══ Negative case: resend with the password file REMOVED ═══"
run_check "resend, missing password" resend \
        RESEND_CONFIG /airgap/config/testcases/resend-18b-docker.properties \
        RESEND_BOOTSTRAP kafka-upstream.sitia.nu:9094,kafka-upstream.sitia.nu:8094 \
        nonzero --resendFileName="$BUNDLE_CONTAINER_PATH" || overall_rc=1

echo
report_summary "testcase 18" "$overall_rc"
rm -f "$BUNDLE_HOST_PATH"
exit "$overall_rc"
