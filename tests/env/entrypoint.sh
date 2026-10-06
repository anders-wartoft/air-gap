#!/usr/bin/env bash
# Entrypoint for the air-gap test containers.
#
# Usage: airgap-entrypoint <role>
#   role = upstream | downstream | resend | create | gaps
#
# Required mounts:
#   /airgap/bin         directory holding the <role> binary built on the host
#   /airgap/config      directory containing config files (testcases/, etc.)
#   /airgap/certs       (optional) directory for keys/certs if a config needs them
#
# Required env:
#   AIRGAP_CONFIG       absolute path to the properties file to run with
#
# All other AIRGAP_* env vars are honoured by the binaries themselves and
# override values in the properties file.
#
# Optional entrypoint-level env:
#   STARTUP_DELAY       seconds to sleep before exec'ing the binary. Lets a
#                       testcase manifest hold one role back deliberately
#                       (e.g. TC-20's "downstream starts late" scenario,
#                       via DOWNSTREAM_STARTUP_DELAY in docker-compose.yml)
#                       while the rest of the stack starts normally. Not
#                       prefixed AIRGAP_ so it's never confused with a
#                       property-file override the Go binary itself reads.

set -euo pipefail

ROLE="${1:-upstream}"
shift || true
BIN="/airgap/bin/${ROLE}"
CFG="${AIRGAP_CONFIG:-}"

if [[ -z "${CFG}" ]]; then
    echo "[entrypoint] AIRGAP_CONFIG is not set." >&2
    echo "[entrypoint] Set it to the absolute path of the properties file you want to run." >&2
    exit 2
fi

if [[ ! -x "${BIN}" ]]; then
    echo "[entrypoint] air-gap binary not found or not executable at ${BIN}" >&2
    echo "[entrypoint] Mount the host build directory containing the '${ROLE}' binary" >&2
    echo "[entrypoint] to /airgap/bin (read-only). Example:" >&2
    echo "[entrypoint]   -v \$(pwd)/target/linux-amd64:/airgap/bin:ro" >&2
    exit 3
fi

if [[ ! -r "${CFG}" ]]; then
    echo "[entrypoint] config file not readable at ${CFG}" >&2
    echo "[entrypoint] Check your AIRGAP_CONFIG value and that /airgap/config is mounted." >&2
    exit 4
fi

STARTUP_DELAY="${STARTUP_DELAY:-0}"
if [[ "$STARTUP_DELAY" -gt 0 ]] 2>/dev/null; then
    echo "[entrypoint] STARTUP_DELAY=${STARTUP_DELAY}s — holding ${ROLE} back before starting"
    sleep "$STARTUP_DELAY"
fi

echo "[entrypoint] role=${ROLE} binary=${BIN} config=${CFG}"
exec "${BIN}" "${CFG}" "$@"
