#!/usr/bin/env bash

AIRGAP_TEST_ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Import only image settings from the shared, shell-compatible .env file.
# An explicitly exported setting wins, just as it does in Compose.
if [[ -f "$AIRGAP_TEST_ENV_DIR/.env" ]]; then
    _airgap_image_settings="$(
        set -a
        source "$AIRGAP_TEST_ENV_DIR/.env" || exit
        env
    )" || return 1
    while IFS='=' read -r _airgap_key _airgap_value; do
        case "$_airgap_key" in
            *_IMAGE|ELASTIC_STACK_VERSION)
                if ! printenv "$_airgap_key" >/dev/null; then
                    export "$_airgap_key=$_airgap_value"
                fi
                ;;
        esac
    done <<< "$_airgap_image_settings"
    unset _airgap_image_settings _airgap_key _airgap_value
    export COMPOSE_ENV_FILES="${COMPOSE_ENV_FILES:-$AIRGAP_TEST_ENV_DIR/.env}"
fi
source "$AIRGAP_TEST_ENV_DIR/lib/docker-images.sh"

airgap_test_project() {
    local id="$1" suffix="${2:-}" padded random
    [[ "$id" =~ ^[0-9]+$ ]] || { echo "Invalid testcase ID: $id" >&2; return 2; }
    padded="$(printf '%02d' "$((10#$id))")"
    if [[ "${COMPOSE_PROJECT_NAME:-}" == "airgap-tc$padded-"* ]]; then
        if [[ -n "$suffix" && "$COMPOSE_PROJECT_NAME" == *"$suffix" ]]; then
            suffix=""
        fi
        printf '%s%s\n' "$COMPOSE_PROJECT_NAME" "$suffix"
        return
    fi
    random="$(LC_ALL=C od -An -N6 -tx1 /dev/urandom | tr -d ' \n')" || return
    printf 'airgap-tc%s-%s%s\n' "$padded" "$random" "$suffix"
}

airgap_init_test() {
    PROJECT="$(airgap_test_project "$@")" || return
    export PROJECT_NAME="$PROJECT" COMPOSE_PROJECT_NAME="$PROJECT"
    echo "Docker project: $PROJECT" >&2
}

airgap_docker_run() {
    docker run --label "com.docker.compose.project=${COMPOSE_PROJECT_NAME:?Initialize the testcase project first}" \
        --label "com.docker.compose.service=test-helper" "$@"
}
