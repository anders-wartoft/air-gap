#!/usr/bin/env bash
# TC-24 pre-up hook — invoked by run-testcase.sh via PRE_UP_SCRIPT, BEFORE
# `docker compose up -d`. upstream-a (upstream-tls-24a-docker.properties)
# reads its TLS cert/key from /airgap/tmp/tc24-rotate.{crt,key,key.pw} —
# writable copies, since the real certs/ mount is read-only and Part D
# needs to overwrite these files live mid-run to exercise SIGHUP rotation.
# They must exist before upstream-a's very first start, which is why this
# runs before `up -d` rather than as a POST_DRAIN_SCRIPT step.
#
# Seeds from certs/upstream.{crt,key,key.pw} — the SAME cert Part B's
# initial handshake uses, so the container boots into a normal, working
# mTLS state; tc24-tls-test.sh overwrites these files with upstream2's
# cert/key later, during Part D.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

mkdir -p ../../tmp
cp ../../certs/upstream.crt     ../../tmp/tc24-rotate.crt
cp ../../certs/upstream.key     ../../tmp/tc24-rotate.key
cp ../../certs/upstream.key.pw  ../../tmp/tc24-rotate.key.pw
echo "tc24-pre-up: seeded /airgap/tmp/tc24-rotate.{crt,key,key.pw} from certs/upstream.*"
