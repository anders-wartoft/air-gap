#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-docker-settings.XXXXXX")"
WORK="$(cd "$WORK" && pwd)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/lib"
cp "$HERE/lib/docker-test.sh" "$HERE/lib/docker-images.sh" "$WORK/lib/"

env -i PATH="$PATH" HOME="$HOME" bash -s -- "$WORK" <<'CHECK'
set -euo pipefail
source "$1/lib/docker-test.sh"
first="$(airgap_test_project 8)"
second="$(airgap_test_project 8)"
[[ "$first" =~ ^airgap-tc08-[a-f0-9]{12}$ && "$first" != "$second" ]]
export COMPOSE_PROJECT_NAME="$first"
[[ "$(airgap_test_project 8)" == "$first" ]]
[[ "$(airgap_test_project 8 "-systemd")" == "$first-systemd" ]]
project="$(COMPOSE_PROJECT_NAME="$first-systemd" airgap_test_project 8 "-systemd")"
[[ "$project" == "$first-systemd" ]]
[[ "$(airgap_test_project 9)" == airgap-tc09-* ]]
[[ "$UPSTREAM_BASE_IMAGE" == "$DOWNSTREAM_BASE_IMAGE" ]]
[[ "$LG_IMAGE" == "$DEDUP_IMAGE" ]]
if airgap_test_project bad >/dev/null 2>&1; then exit 1; fi
CHECK
echo "PASS: padded, unique testcase projects, inherited hooks and separate child stacks"

cat > "$WORK/.env" <<'CONFIG'
AIRGAP_BASE_IMAGE=example.org/os/shared:9
KAFKA_IMAGE=example.org/kafka:next
ELASTIC_STACK_VERSION=8.99.0
UPSTREAM_A_CONFIG=must-not-be-exported
CONFIG
env -i PATH="$PATH" HOME="$HOME" KAFKA_IMAGE=example.org/kafka:shell bash -s -- "$WORK" <<'CHECK'
set -euo pipefail
source "$1/lib/docker-test.sh"
[[ "$KAFKA_IMAGE" == example.org/kafka:shell ]]
[[ "$UPSTREAM_BASE_IMAGE" == example.org/os/shared:9 ]]
[[ "$DOWNSTREAM_BASE_IMAGE" == example.org/os/shared:9 ]]
[[ "$SYSTEMD_BASE_IMAGE" == example.org/os/shared:9 ]]
[[ "$ELASTICSEARCH_IMAGE" == docker.elastic.co/elasticsearch/elasticsearch:8.99.0 ]]
[[ "$KIBANA_IMAGE" == docker.elastic.co/kibana/kibana:8.99.0 ]]
[[ -z "${UPSTREAM_A_CONFIG:-}" ]]
[[ "$COMPOSE_ENV_FILES" == "$1/.env" ]]
CHECK
echo "PASS: common image baseline, shared .env, environment precedence and matched Elastic versions"

env -i PATH="$PATH" HOME="$HOME" UPSTREAM_BASE_IMAGE=example.org/os/up:next \
    INTEGRITY_BASE_IMAGE=example.org/python:next bash -s -- "$WORK" <<'CHECK'
set -euo pipefail
source "$1/lib/docker-test.sh"
[[ "$UPSTREAM_BASE_IMAGE" == example.org/os/up:next ]]
[[ "$DOWNSTREAM_BASE_IMAGE" == example.org/os/shared:9 ]]
[[ "$INTEGRITY_BASE_IMAGE" == example.org/python:next ]]
airgap_init_test 24
docker() { printf '%s\n' "$*"; }
output="$(airgap_docker_run --rm "$BUSYBOX_IMAGE" true)"
[[ "$output" == *"com.docker.compose.project=$PROJECT"* ]]
[[ "$output" == *"com.docker.compose.service=test-helper"* ]]
CHECK
echo "PASS: role-specific overrides and helper container project labels"
