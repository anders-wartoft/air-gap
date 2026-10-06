#!/bin/sh
# Entrypoint for the LogGenerator container services (lg-producer, lg-sink).
#
# Resolves the LogGenerator jar in /airgap/lg-bin via a glob (version in the
# filename is irrelevant), then runs the LG with the properties file in
# $LG_CONFIG plus any extra CLI args in $LG_ARGS.
#
# If $LG_SED is set it is applied as a sed expression against $LG_CONFIG
# before launch, so a config written for bare-metal hostnames can be rewritten
# to the Docker service names at run time without editing the committed file.
# Example manifest value:
#   LG_PRODUCER_SED='s|192.168.153.14[0-9]|kafka-upstream.sitia.nu|g'
#
# Optional startup delay via $LG_STARTUP_DELAY (seconds). Used by test cases
# whose downstream pipeline includes stateful Kafka Streams apps (e.g. the
# dedup profile) which need time to finish REBALANCING before the first
# event flows through. Without this, LG can finish all events in ~1 s and
# race the Streams app's EOS-v2 task-restoration, occasionally dropping a
# single event per run.

set -eu

JAR=$(ls /airgap/lg-bin/LogGenerator-*.jar 2>/dev/null | sort -V | tail -1 || true)
if [ -z "$JAR" ]; then
    echo "[lg] no LogGenerator jar in /airgap/lg-bin." >&2
    echo "[lg] Drop LogGenerator-X.Y-Z.jar into tests/env/bin/ on the host." >&2
    exit 2
fi

CFG="${LG_CONFIG:-}"
if [ -z "$CFG" ]; then
    echo "[lg] LG_CONFIG is not set." >&2
    exit 3
fi
if [ ! -r "$CFG" ]; then
    echo "[lg] cannot read $CFG" >&2
    exit 4
fi

if [ -n "${LG_SED:-}" ]; then
    TMP="$(mktemp)"
    sed -e "$LG_SED" "$CFG" > "$TMP"
    CFG="$TMP"
fi

STARTUP_DELAY="${LG_STARTUP_DELAY:-0}"
if [ "$STARTUP_DELAY" -gt 0 ] 2>/dev/null; then
    echo "[lg] LG_STARTUP_DELAY=${STARTUP_DELAY}s — waiting for downstream services (dedup, …) to fully settle before producing"
    sleep "$STARTUP_DELAY"
fi

ARGS="${LG_ARGS:-}"
echo "[lg] jar=$JAR config=$CFG args=$ARGS"
# shellcheck disable=SC2086
exec java -jar "$JAR" -pf "$CFG" $ARGS
