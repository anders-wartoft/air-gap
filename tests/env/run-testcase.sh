#!/usr/bin/env bash
# Run an air-gap testcase under Docker Compose.
#
# Usage:
#   ./run-testcase.sh <id> [role] [extra-profile ...] [-pause] [-- compose-args...]
#
#   id             testcase number from TESTCASES.md (available manifests)
#   role           all        - full stack on this machine (default)
#                  up-only    - only the upstream half (kafka-upstream + mates
#                               and the air-gap upstream processes,
#                               plus dual / second-cluster when activated)
#                  down-only  - only the downstream half (kafka-downstream +
#                               mates and the air-gap downstream, plus dedup
#                               / create when activated)
#   extra-profile  additional Compose profiles to activate:
#                    dual            second upstream process (upstream-b)
#                    second-cluster  second upstream Kafka cluster (TC-21)
#                    dedup           deduplicator
#                    create          one-shot gap exporter
#                    resend          one-shot gap replayer
#   -pause|--pause|-p   leave containers running after the test completes so
#                       you can inspect Kafka topics and copy log files out;
#                       prints a cheat sheet of useful commands.
#   anything after a lone `--` is passed verbatim to `docker compose up`.
#
# Examples:
#   ./run-testcase.sh 3                       # testcase 3, single machine
#   ./run-testcase.sh 11                      # testcase 11, auto-adds dual+dedup
#   ./run-testcase.sh 11 -pause               # run TC-11, keep stack up for inspection
#   ./run-testcase.sh 11 up-only              # upstream half (two upstreams, one cluster)
#   ./run-testcase.sh 11 down-only            # downstream + dedup
#   ./run-testcase.sh 21 up-only              # upstream half with two upstream clusters
#   ./run-testcase.sh 14 all -- --build       # pass --build through to compose

set -euo pipefail

# When the runner is launched interactively (stdout and/or stderr attached to
# a terminal), re-exec with a `| cat` wrapper so our FDs are always pipes,
# never TTYs. The docker CLI and docker-compose both behave differently when
# their parent's output FDs are TTYs — in particular, backgrounded `docker
# inspect` and `docker wait` calls queue behind TTY-attached log streams and
# the main poll loop can take 10+ minutes instead of ~5 seconds. Running
# everything through a pipe eliminates that whole class of behaviour, and
# the user still sees the same output on their terminal.
if [[ -z "${AIRGAP_RUNNER_REEXEC:-}" ]] && { [[ -t 1 ]] || [[ -t 2 ]]; }; then
    export AIRGAP_RUNNER_REEXEC=1
    "$0" "$@" 2>&1 | cat
    exit "${PIPESTATUS[0]}"
fi

# Force `docker compose` into plain (non-animated) progress output. When
# stdout is a TTY, compose uses TTY escape sequences for an animated status
# display that can stall the whole runner if the terminal buffer fills, and
# backgrounded compose children can be paused with SIGTTIN by the kernel.
# `plain` keeps all docker-compose output line-oriented and non-interactive,
# which is what the runner needs for reliable detached orchestration.
export COMPOSE_PROGRESS=plain
export BUILDKIT_PROGRESS=plain

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

# shellcheck source=lib/report.sh
source "$HERE/lib/report.sh"

usage() {
    sed -n '2,33p' "$0"
    exit 1
}
# Build a comma-separated list of active profiles for COMPOSE_PROFILES.
compose_profiles_csv() {
    local csv="" p
    for p in "${PROFILES[@]-}"; do
        [[ -z "$p" ]] && continue
        if [[ -z "$csv" ]]; then csv="$p"; else csv+=",$p"; fi
    done
    printf '%s' "$csv"
}

# Print the cheat sheet shown when -pause keeps the stack up.
print_pause_hints() {
    local rc="${1-0}"
    local csv
    csv="$(compose_profiles_csv)"
    cat <<EOF

─── testcase ${TC_ID} finished (exit=${rc}) — containers left running ───

Set the active profiles once in your shell so the commands below are short:

  export COMPOSE_PROFILES=${csv}
  cd $(pwd)

Inspect Kafka topics (plaintext listener on broker-1):
  docker compose exec kafka-upstream   kafka-topics           --bootstrap-server kafka-upstream:9092   --list
  docker compose exec kafka-downstream kafka-topics           --bootstrap-server kafka-downstream:9092 --list
  docker compose exec kafka-upstream   kafka-console-consumer --bootstrap-server kafka-upstream:9092   --topic transfer --from-beginning --max-messages 20
  docker compose exec kafka-downstream kafka-console-consumer --bootstrap-server kafka-downstream:9092 --topic transfer --from-beginning --max-messages 20
  docker compose exec kafka-downstream kafka-console-consumer --bootstrap-server kafka-downstream:9092 --topic dedup    --from-beginning --max-messages 20
  docker compose exec kafka-downstream kafka-console-consumer --bootstrap-server kafka-downstream:9092 --topic gaps     --from-beginning --max-messages 20

Read / tail logs:
  docker compose logs lg-producer
  docker compose logs lg-sink
  docker compose logs -f dedup
  docker compose logs upstream-a upstream-b downstream

Copy log files out of a container:
  docker cp airgap-testenv-dedup-1:/var/log/airgap ./dedup-logs
  docker cp airgap-testenv-downstream-1:/var/log/airgap ./downstream-logs

Open a shell inside a container:
  docker compose exec kafka-upstream bash
  docker compose exec dedup          sh

Re-run the testcase (reuses the running stack where possible):
  ./run-testcase.sh ${TC_ID} -pause

Tear everything down when you're finished:
  docker compose down -v --remove-orphans
EOF
}
[[ ${1-} =~ ^[0-9]+$ ]] || usage
TC_ID="$1"
shift
TC_PADDED="$(printf '%02d' "$TC_ID")"
MANIFEST="testcases/${TC_PADDED}.env"
[[ -f "$MANIFEST" ]] || { echo "no manifest: $MANIFEST" >&2; exit 2; }

ROLE="all"
EXTRA_PROFILES=()
PASSTHROUGH=()
PAUSE=0
# Split args at the first lone `--`.
seen_sep=0
for arg in "$@"; do
    if [[ $seen_sep -eq 1 ]]; then
        PASSTHROUGH+=("$arg")
        continue
    fi
    case "$arg" in
        up-only|down-only|all) ROLE="$arg" ;;
        -pause|--pause|-p) PAUSE=1 ;;
        --) seen_sep=1 ;;
        *) EXTRA_PROFILES+=("$arg") ;;
    esac
done

# Load the testcase manifest.  All variables become ambient so Compose
# interpolates them with the usual ${NAME:-default} rules.
set -o allexport
# shellcheck disable=SC1090
source "$MANIFEST"
set +o allexport

# Standalone cases own their isolated stack and verdict, without Kafka/LG.
if [[ -n "${TC_RUNNER-}" ]]; then
    if [[ "$ROLE" != all ]] || ((${#EXTRA_PROFILES[@]})); then
        echo "TC-${TC_ID} requires role=all and no extra profiles" >&2
        exit 2
    fi
    if ((${#PASSTHROUGH[@]})); then
        for arg in "${PASSTHROUGH[@]}"; do
            if [[ "$arg" != --build ]]; then
                echo "TC-${TC_ID} only accepts --build after -- (images are built by default)" >&2
                exit 2
            fi
        done
    fi
    export TC_PAUSE="$PAUSE"
    exec bash "$HERE/$TC_RUNNER"
fi

# Collect the full profile set (manifest + extras).
PROFILES=()
if [[ -n "${TC_PROFILES-}" ]]; then
    read -r -a _mp <<< "$TC_PROFILES"
    PROFILES+=("${_mp[@]}")
fi
if ((${#EXTRA_PROFILES[@]})); then
    PROFILES+=("${EXTRA_PROFILES[@]}")
fi

# Automatically activate the LogGenerator profiles when the manifest declares
# the respective config. This also tells the runner to pass --exit-code-from
# lg-producer so the full stack tears down as soon as the producer finishes.
# (down-only sides don't start lg-producer; the exit-code-from is suppressed
# below in that case.)
AUTO_EXIT_FROM=""
if [[ -n "${LG_PRODUCER_CONFIG-}" ]]; then
    PROFILES+=("lg-producer")
    AUTO_EXIT_FROM="lg-producer"
fi
# Second LogGenerator producer, targeting the second upstream Kafka cluster
# (TC-21: two clusters sharing a topic name, feeding one dedup). Never the
# --exit-code-from source itself (compose only allows one); the AUTO_EXIT
# wait logic below separately polls this container's own exit so cluster
# B's stream is never truncated early just because cluster A's producer
# happened to finish first.
if [[ -n "${LG_PRODUCER_B_CONFIG-}" ]]; then
    PROFILES+=("lg-producer-b")
fi
if [[ -n "${LG_SINK_CONFIG-}" ]]; then
    PROFILES+=("lg-sink")
fi
# Alternative auto-exit source: a bespoke test container (e.g. TC-7's
# tc7-binary-test, which does its own produce/consume/verify roundtrip in
# Python and emits LG-compatible summary lines). AUTO_EXIT_PROFILE wins
# over the LG defaults so a manifest can opt into either model without
# conflict.
if [[ -n "${AUTO_EXIT_PROFILE-}" && -n "${AUTO_EXIT_SERVICE-}" ]]; then
    PROFILES+=("$AUTO_EXIT_PROFILE")
    AUTO_EXIT_FROM="$AUTO_EXIT_SERVICE"
fi

# Role filter -> explicit service list.
case "$ROLE" in
    up-only)
        SERVICES=(kafka-upstream kafka-upstream-2 topic-init-upstream upstream-a)
        for p in "${PROFILES[@]}"; do
            case "$p" in
                dual) SERVICES+=(upstream-b) ;;
                second-cluster) SERVICES+=(kafka-upstream-b kafka-upstream-b-2 topic-init-upstream-b) ;;
                resend) SERVICES+=(resend) ;;
                lg-producer) SERVICES+=(lg-producer) ;;
                lg-producer-b) SERVICES+=(lg-producer-b) ;;
            esac
        done
        ;;
    down-only)
        SERVICES=(kafka-downstream kafka-downstream-2 topic-init-downstream downstream)
        for p in "${PROFILES[@]}"; do
            case "$p" in
                dedup) SERVICES+=(dedup) ;;
                create) SERVICES+=(create) ;;
                lg-sink) SERVICES+=(lg-sink) ;;
            esac
        done
        # lg-producer only exists on the upstream half.
        AUTO_EXIT_FROM=""
        ;;
    all)
        SERVICES=()
        ;;
esac

# Build the --profile flags.
PROFILE_FLAGS=()
# Deduplicate.
if ((${#PROFILES[@]})); then
    while IFS= read -r p; do
        PROFILE_FLAGS+=(--profile "$p")
    done < <(printf '%s\n' "${PROFILES[@]}" | awk 'NF && !seen[$0]++')
fi

echo "─── testcase ${TC_ID}: ${TC_TITLE:-?} ───"
echo "  covers requirements: ${TC_COVERS:-?}"
echo "  role               : ${ROLE}"
if ((${#PROFILES[@]})); then
    echo "  profiles           : ${PROFILES[*]}"
else
    echo "  profiles           : (none)"
fi
if ((${#SERVICES[@]})); then
    echo "  services           : ${SERVICES[*]}"
else
    echo "  services           : (all non-profile services + enabled profiles)"
fi
echo "  manifest           : $MANIFEST"
if [[ -n "$AUTO_EXIT_FROM" ]]; then
    echo "  auto-wait on       : $AUTO_EXIT_FROM"
    echo "  drain window       : ${TC_DRAIN_SECONDS:-15}s (override with TC_DRAIN_SECONDS)"
    echo "  max wait           : ${LG_MAX_WAIT:-90}s (override with LG_MAX_WAIT)"
fi
if [[ -n "${UPSTREAM_A_PHASE2_CONFIG-}" ]]; then
    echo "  phase 2 config (a) : ${UPSTREAM_A_PHASE2_CONFIG}"
    echo "  phase 2 drain      : ${PHASE2_DRAIN_SECONDS:-45}s (override with PHASE2_DRAIN_SECONDS)"
fi
if [[ -n "${UPSTREAM_B_PHASE2_CONFIG-}" ]]; then
    echo "  phase 2 config (b) : ${UPSTREAM_B_PHASE2_CONFIG}"
fi
if [[ "${PHASE2_RERUN_LG_PRODUCER:-0}" == "1" ]]; then
    echo "  phase 2 2nd wave   : yes (lg-producer/-b re-run with original configs)"
fi
if [[ -n "${UPSTREAM_A_RESTART_AT_SECONDS-}" ]]; then
    echo "  restart upstream-a : at T+${UPSTREAM_A_RESTART_AT_SECONDS}s (mid-run SIGTERM + fresh container)"
fi
if [[ -n "${DOWNSTREAM_RESTART_AT_SECONDS-}" ]]; then
    echo "  restart downstream : at T+${DOWNSTREAM_RESTART_AT_SECONDS}s, down for ${DOWNSTREAM_RESTART_DOWN_SECONDS:-8}s (stop/start, same container)"
fi
if [[ -n "${POST_DRAIN_SCRIPT-}" ]]; then
    echo "  post-drain hook    : ${POST_DRAIN_SCRIPT}"
fi
if [[ -n "${PRE_UP_SCRIPT-}" ]]; then
    echo "  pre-up hook        : ${PRE_UP_SCRIPT}"
fi
if (( PAUSE )); then
    echo "  pause after test   : yes (containers stay up for inspection)"
fi
echo

bash "$HERE/generate-kafka-certs.sh"

if [[ -n "$AUTO_EXIT_FROM" ]]; then
    # Compose names containers deterministically as <project>-<service>-<index>;
    # the project name is pinned via `name: airgap-testenv` in
    # docker-compose.yml, so this is a fixed constant, not derived from
    # anything dynamic. Defined early (before PRE_UP_SCRIPT, which needs it)
    # rather than down by the AUTO_EXIT polling loop where it's also used.
    PROJECT_NAME="airgap-testenv"

    UP_ARGS=()
    if ((${#PASSTHROUGH[@]})); then
        UP_ARGS+=("${PASSTHROUGH[@]}")
    fi
    if ((${#SERVICES[@]})); then
        UP_ARGS+=("${SERVICES[@]}")
    fi

    # Reap any orphan `docker compose logs -f` streams left over from a
    # previous aborted run on the same project. If they survive they keep a
    # connection open to the Docker API and new `docker inspect` / `docker
    # wait` calls can queue behind them, making the runner appear to hang
    # even though lg-producer has already exited.
    orphans="$(pgrep -f 'docker compose .*logs -f.*airgap-testenv' 2>/dev/null || true)"
    if [[ -z "$orphans" ]]; then
        orphans="$(pgrep -f 'docker compose .*logs -f.*lg-producer'   2>/dev/null || true)"
    fi
    if [[ -n "$orphans" ]]; then
        echo "runner: killing stale docker-compose-logs orphans: $(echo "$orphans" | tr '\n' ' ')" >&2
        # shellcheck disable=SC2086
        kill $orphans 2>/dev/null || true
        sleep 1
        # shellcheck disable=SC2086
        kill -9 $orphans 2>/dev/null || true
    fi

    # Install cleanup trap BEFORE `up -d` so Ctrl-C, SIGTERM, or any script
    # error still tears the stack down. The trap is a no-op if -pause was
    # requested and the test reached the success path (we flip STACK_PAUSED=1
    # right before exit in that case).
    LOGS_PID=""
    LG_WATCH_PID=""
    LG_WAIT_PID=""
    RESTART_WATCHDOG_PID=""
    LG_DONE_FILE=""
    LG_LOG_FILE=""
    STACK_PAUSED=0
    # NOTE: a `VAR="$unbound_var" command` prefix-assignment, if
    # $unbound_var is actually unset under `set -u`, aborts non-
    # interactively but does NOT propagate its exit status to `$?` as seen
    # by this EXIT trap (confirmed empirically — bash quirk, not a bug in
    # this script) — `$?` here would still show whatever the PREVIOUS
    # command's status was, reporting a false PASS. Keep every var
    # referenced in such a prefix (e.g. PROJECT_NAME below, used by the
    # PRE_UP_SCRIPT/POST_DRAIN_SCRIPT hooks) defined well before first use.
    cleanup_on_exit() {
        local exit_code=$?
        if [[ -n "$LG_WATCH_PID" ]]; then
            kill "$LG_WATCH_PID" 2>/dev/null || true
            wait "$LG_WATCH_PID" 2>/dev/null || true
        fi
        if [[ -n "$LG_WAIT_PID" ]]; then
            kill "$LG_WAIT_PID" 2>/dev/null || true
            wait "$LG_WAIT_PID" 2>/dev/null || true
        fi
        if [[ -n "$RESTART_WATCHDOG_PID" ]]; then
            kill "$RESTART_WATCHDOG_PID" 2>/dev/null || true
            wait "$RESTART_WATCHDOG_PID" 2>/dev/null || true
        fi
        if [[ -n "$LOGS_PID" ]]; then
            kill "$LOGS_PID" 2>/dev/null || true
            wait "$LOGS_PID" 2>/dev/null || true
        fi
        [[ -n "$LG_DONE_FILE" ]] && rm -f "$LG_DONE_FILE" 2>/dev/null || true
        [[ -n "$LG_LOG_FILE"  ]] && rm -f "$LG_LOG_FILE"  2>/dev/null || true
        if (( STACK_PAUSED )); then
            return
        fi
        echo
        echo "runner: tearing down stack (exit=${exit_code})" >&2
        docker compose "${PROFILE_FLAGS[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
    }
    trap cleanup_on_exit EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    trap 'exit 129' HUP

    # Optional pre-up hook. When the manifest declares
    #     PRE_UP_SCRIPT=/absolute/path/to/script.sh
    # (or a path relative to tests/env/), the runner invokes it here,
    # BEFORE `docker compose up -d` — for host-side setup that containers
    # need to exist at their very first startup (e.g. TC-24 seeding
    # writable cert-rotation copies into ../../tmp before upstream-a reads
    # them at boot; a hook running after `up -d`, like POST_DRAIN_SCRIPT,
    # would be too late).
    if [[ -n "${PRE_UP_SCRIPT-}" ]]; then
        pre_up_path="$PRE_UP_SCRIPT"
        if [[ ! -f "$pre_up_path" && -f "$HERE/$PRE_UP_SCRIPT" ]]; then
            pre_up_path="$HERE/$PRE_UP_SCRIPT"
        fi
        if [[ -x "$pre_up_path" || -f "$pre_up_path" ]]; then
            echo "runner: pre-up hook: ${pre_up_path}" >&2
            PROJECT_NAME="$PROJECT_NAME" TC_ID="$TC_ID" bash "$pre_up_path"
        else
            echo "runner: PRE_UP_SCRIPT='$PRE_UP_SCRIPT' not found (looked at '$pre_up_path')" >&2
            exit 1
        fi
    fi

    # Redirect `docker compose up -d` stdout/stderr to a temp file (NOT the
    # terminal) and detach stdin from any TTY. This avoids known hangs on
    # macOS where compose's TTY-animated output can block when stdout is a
    # terminal, and where piping compose through `cat` can inherit child
    # file descriptors that never close. Unlike discarding to /dev/null,
    # capturing to a file means a failure here isn't a silent, undiagnosable
    # `set -e` exit — if `up -d` itself fails (bad image, port conflict,
    # stale network/container name clash, Docker daemon hiccup, ...) we
    # dump the captured output before re-raising, so the banner isn't
    # immediately followed by "tearing down stack" with zero explanation.
    UP_LOG="$(mktemp -t airgap-up-XXXXXX)"
    up_rc=0
    if ((${#UP_ARGS[@]})); then
        docker compose "${PROFILE_FLAGS[@]}" up -d "${UP_ARGS[@]}" </dev/null >"$UP_LOG" 2>&1 || up_rc=$?
    else
        docker compose "${PROFILE_FLAGS[@]}" up -d </dev/null >"$UP_LOG" 2>&1 || up_rc=$?
    fi
    if (( up_rc != 0 )); then
        echo "runner: 'docker compose up -d' failed (rc=${up_rc}); captured output:" >&2
        cat "$UP_LOG" >&2
        rm -f "$UP_LOG"
        exit "$up_rc"
    fi
    rm -f "$UP_LOG"
    echo "runner: docker compose up -d returned (stack starting)" >&2

    # Resolve the container for the service we want to wait on.
    CONTAINER_NAME="${PROJECT_NAME}-${AUTO_EXIT_FROM}-1"
    for _ in $(seq 1 60); do
        if docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done
    if ! docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
        echo "runner: container '$CONTAINER_NAME' did not appear within 60s" >&2
        exit 1
    fi

    # Tail the interesting services while we wait so the user can see events
    # flowing. Only tail services that are actually running under the current
    # role; the profile flags are needed because some are profile-gated.
    # `</dev/null` detaches the background child from the controlling TTY so
    # the kernel never pauses it with SIGTTIN when it tries to read stdin.
    LOG_SERVICES=(lg-producer lg-sink dedup downstream upstream-a upstream-b)
    docker compose "${PROFILE_FLAGS[@]}" logs -f --no-log-prefix \
        "${LOG_SERVICES[@]}" </dev/null 2>/dev/null &
    LOGS_PID=$!

    # Three parallel completion signals. Any one of them wins the race and
    # breaks the main poll loop. We need all three because Docker Desktop
    # has been observed to:
    #   * keep reporting `State.Status=running` long after the Java process
    #     exited (so plain `docker inspect` polling can miss it);
    #   * buffer `docker logs -f` output (so grep-based detection can lag);
    #   * delay `docker wait` returns in some configurations.
    # One of these always fires promptly in practice.
    LG_DONE_FILE="$(mktemp -t airgap-lg-done.XXXXXX)"
    LG_LOG_FILE="$(mktemp -t airgap-lg-log.XXXXXX)"

    # Signal 1: dedicated log-tail that writes a sentinel the moment the
    # producer prints its "transferred N lines" summary.
    (
        exec </dev/null
        docker logs -f "$CONTAINER_NAME" >"$LG_LOG_FILE" 2>&1 &
        TAIL_PID=$!
        while :; do
            if grep -q 'transferred .* lines' "$LG_LOG_FILE" 2>/dev/null; then
                echo "log" > "$LG_DONE_FILE"
                break
            fi
            if ! kill -0 "$TAIL_PID" 2>/dev/null; then
                break
            fi
            sleep 1
        done
        kill "$TAIL_PID" 2>/dev/null || true
    ) &
    LG_WATCH_PID=$!

    # Signal 2: blocking `docker wait`, which returns as soon as Docker's
    # own event stream reports the container exited.
    (
        exec </dev/null
        exit_code="$(docker wait "$CONTAINER_NAME" 2>/dev/null || echo "")"
        if [[ -n "$exit_code" ]]; then
            echo "wait:${exit_code}" > "$LG_DONE_FILE"
        fi
    ) &
    LG_WAIT_PID=$!

    # Mid-run restart watchdog (TC-2 style): when the manifest declares
    # UPSTREAM_A_RESTART_AT_SECONDS, this background job sleeps for that
    # many seconds and then `docker compose up -d --force-recreate` the
    # upstream-a service. Compose sends SIGTERM first (so the Go upstream
    # runs its graceful shutdown — commits Kafka offsets, flushes UDP) and
    # then starts a fresh container with the same AIRGAP_CONFIG. Sarama
    # resumes from the committed offset, so no events should be lost or
    # duplicated. Proves REQ-5 / REQ-9 (at-least-once across restarts).
    if [[ -n "${UPSTREAM_A_RESTART_AT_SECONDS-}" ]]; then
        (
            exec </dev/null
            sleep "$UPSTREAM_A_RESTART_AT_SECONDS"
            echo "runner: restart-at-${UPSTREAM_A_RESTART_AT_SECONDS}s — recreating upstream-a (SIGTERM → fresh container)" >&2
            docker compose "${PROFILE_FLAGS[@]}" up -d --force-recreate --no-deps upstream-a \
                >/dev/null 2>&1 || true
        ) &
        RESTART_WATCHDOG_PID=$!
    fi

    # Downstream stop/restart watchdog (TC-22 style): when the manifest
    # declares DOWNSTREAM_RESTART_AT_SECONDS, this background job sleeps
    # for that many seconds, then `docker stop`s the downstream container
    # (graceful SIGTERM — upstream's TCP send loop starts retrying/
    # buffering, same as TC-20's late-start scenario), holds it down for
    # DOWNSTREAM_RESTART_DOWN_SECONDS, then `docker start`s the SAME
    # container again (not --force-recreate: TC-22 wants one continuous
    # `docker logs downstream` history spanning the stop/restart so
    # verify-tc22.sh can check for a gapless, duplicate-free sequence).
    # Proves REQ-37 (downstream restart doesn't need upstream restarted,
    # loses nothing).
    if [[ -n "${DOWNSTREAM_RESTART_AT_SECONDS-}" ]]; then
        (
            exec </dev/null
            sleep "$DOWNSTREAM_RESTART_AT_SECONDS"
            down_cid="${PROJECT_NAME}-downstream-1"
            echo "runner: restart-at-${DOWNSTREAM_RESTART_AT_SECONDS}s — stopping downstream (${down_cid})" >&2
            docker stop "$down_cid" >/dev/null 2>&1 || true
            sleep "${DOWNSTREAM_RESTART_DOWN_SECONDS:-8}"
            echo "runner: restarting downstream (${down_cid}) after ${DOWNSTREAM_RESTART_DOWN_SECONDS:-8}s down" >&2
            docker start "$down_cid" >/dev/null 2>&1 || true
        ) &
        RESTART_WATCHDOG_PID=$!
    fi

    MAX_WAIT="${LG_MAX_WAIT:-90}"
    # After the producer exits, wait this many seconds for in-flight events
    # to propagate through upstream → UDP → downstream → kafka → dedup → sink
    # before tearing the stack down. Reduces false "missing" events caused by
    # cutting the pipeline mid-flow. Override with TC_DRAIN_SECONDS=<seconds>.
    DRAIN_SECONDS="${TC_DRAIN_SECONDS:-15}"
    waited=0
    diagnosed=0

    echo "runner: ─── run-testcase.sh v3 (triple-signal detector) ───" >&2
    echo "runner: polling ${AUTO_EXIT_FROM} every 2s (max ${MAX_WAIT}s, heartbeat every 10s)" >&2
    echo "runner: watchers started: WATCH=${LG_WATCH_PID} WAIT=${LG_WAIT_PID}" >&2

    set +e
    rc=1
    while :; do
        state="$(docker inspect --format '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || true)"
        if [[ -z "$state" ]]; then
            echo "runner: container '$CONTAINER_NAME' disappeared before completing" >&2
            rc=1
            break
        fi
        if [[ "$state" == "exited" ]]; then
            rc="$(docker inspect --format '{{.State.ExitCode}}' "$CONTAINER_NAME" 2>/dev/null || echo 1)"
            echo "runner: ${AUTO_EXIT_FROM} state=exited (rc=${rc}) after ${waited}s" >&2
            break
        fi
        # Signal 2 or 1: either `docker wait` returned or the log tail saw
        # the "transferred ... lines" marker. Authoritative when
        # `docker inspect` is lagging.
        if [[ -s "$LG_DONE_FILE" ]]; then
            signal="$(cat "$LG_DONE_FILE")"
            # If `docker wait` reported, we can pass through its exit code.
            case "$signal" in
                wait:*) rc="${signal#wait:}" ;;
                *) rc=0 ;;
            esac
            echo "runner: ${AUTO_EXIT_FROM} completed via signal=${signal} after ${waited}s (state=${state}, rc=${rc})" >&2
            break
        fi
        if (( waited >= MAX_WAIT )); then
            echo "runner: ${AUTO_EXIT_FROM} did not exit within ${MAX_WAIT}s; forcing kill" >&2
            echo "--- last 30 lines of ${AUTO_EXIT_FROM} ---" >&2
            tail -n 30 "$LG_LOG_FILE" 2>/dev/null >&2 || true
            # SIGKILL so the Java process is actually gone even if it's stuck in
            # a retry loop (e.g. Kafka produce failure, slow broker, etc.).
            docker kill --signal=KILL "$CONTAINER_NAME" >/dev/null 2>&1 || true
            rc=124
            break
        fi
        # Heartbeat every 10 seconds so the user can see progress.
        if (( waited > 0 && waited % 10 == 0 )); then
            lastline="$(tail -n1 "$LG_LOG_FILE" 2>/dev/null | tr -d '\r' | cut -c1-120)"
            watch_alive="?" ; wait_alive="?"
            kill -0 "$LG_WATCH_PID" 2>/dev/null && watch_alive=live || watch_alive=dead
            kill -0 "$LG_WAIT_PID"  2>/dev/null && wait_alive=live  || wait_alive=dead
            echo "runner: waiting for ${AUTO_EXIT_FROM} (state=${state}, elapsed=${waited}s / max=${MAX_WAIT}s, watch=${watch_alive}, wait=${wait_alive})" >&2
            [[ -n "$lastline" ]] && echo "runner:   last log line: ${lastline}" >&2
        fi
        # One-shot diagnostic peek at 30 seconds — covers Kafka connect
        # failures, config errors, etc. without the user needing to Ctrl-C.
        if (( waited == 30 && diagnosed == 0 )); then
            diagnosed=1
            echo "--- ${AUTO_EXIT_FROM} log so far (diagnostic peek at 30s) ---" >&2
            tail -n 30 "$LG_LOG_FILE" 2>/dev/null >&2 || true
            echo "---" >&2
        fi
        sleep 2
        waited=$((waited + 2))
    done
    set -e

    # Second producer (TC-21's cluster B): the main loop above only tracks
    # AUTO_EXIT_FROM (cluster A). If lg-producer-b is active, wait for it to
    # exit too — otherwise a slower/faster cluster B stream could be cut off
    # mid-run by whatever teardown/phase-2 step cluster A's completion
    # triggers next.
    if (( rc == 0 )) && [[ -n "${LG_PRODUCER_B_CONFIG-}" ]]; then
        LG_PRODUCER_B_CID="${PROJECT_NAME}-lg-producer-b-1"
        if docker inspect "$LG_PRODUCER_B_CID" >/dev/null 2>&1; then
            echo "runner: waiting for lg-producer-b to also finish (max ${MAX_WAIT}s)" >&2
            b_waited=0
            while :; do
                b_state="$(docker inspect --format '{{.State.Status}}' "$LG_PRODUCER_B_CID" 2>/dev/null || true)"
                if [[ "$b_state" == "exited" ]]; then
                    b_rc="$(docker inspect --format '{{.State.ExitCode}}' "$LG_PRODUCER_B_CID" 2>/dev/null || echo 1)"
                    echo "runner: lg-producer-b state=exited (rc=${b_rc}) after ${b_waited}s" >&2
                    break
                fi
                if (( b_waited >= MAX_WAIT )); then
                    echo "runner: lg-producer-b did not exit within ${MAX_WAIT}s; forcing kill" >&2
                    docker kill --signal=KILL "$LG_PRODUCER_B_CID" >/dev/null 2>&1 || true
                    break
                fi
                sleep 2
                b_waited=$((b_waited + 2))
            done
        fi
    fi

    # Drain window: after the producer exited cleanly, give the rest of the
    # pipeline time to catch up before we tear it down. Skip the drain when
    # we're bailing out on failure or when -pause is set (there is no
    # subsequent teardown either way).
    #
    # Phase-2 gap-fill (TC-9 style): when the manifest declares
    # UPSTREAM_A_PHASE2_CONFIG (and/or UPSTREAM_B_PHASE2_CONFIG for a second
    # upstream, e.g. TC-21's two-cluster setup), the runner stops that
    # upstream and recreates it with the alternate config (typically a
    # no-filter variant + fresh groupID). Kafka replays every event, the
    # dedup app filters duplicates, and any gaps opened intentionally in
    # phase 1 get filled. This mirrors the manual "stop upstream, restart
    # with the 9b config" step in tests/documents/TESTCASES.md.
    # PHASE2_DRAIN_SECONDS replaces TC_DRAIN_SECONDS for the gap-fill window
    # (phase 2 needs more time).
    #
    # PHASE2_RERUN_LG_PRODUCER=1 additionally force-recreates lg-producer
    # (and lg-producer-b, if active) with their ORIGINAL configs — TC-21's
    # manual procedure generates a second wave of the same counter range
    # after the upstream restart, which both exercises the fresh-groupID
    # replay AND produces the documented "each value delivered twice"
    # duplicates. Not used by TC-9/10/12/13/14 (replay-only, no second wave).
    if (( rc == 0 )) && (( PAUSE == 0 )) && [[ -n "${UPSTREAM_A_PHASE2_CONFIG-}${UPSTREAM_B_PHASE2_CONFIG-}" ]]; then
        settle="${PHASE2_SETTLE_SECONDS:-5}"
        echo "runner: phase-1 settle ${settle}s before swapping upstream config(s)" >&2
        sleep "$settle"
        if [[ -n "${UPSTREAM_A_PHASE2_CONFIG-}" ]]; then
            echo "runner: phase 2 — recreating upstream-a with AIRGAP_CONFIG=${UPSTREAM_A_PHASE2_CONFIG}" >&2
            if ! env UPSTREAM_A_CONFIG="$UPSTREAM_A_PHASE2_CONFIG" \
                    docker compose "${PROFILE_FLAGS[@]}" up -d --force-recreate --no-deps upstream-a \
                    </dev/null >/dev/null 2>&1; then
                echo "runner: phase-2 recreate of upstream-a failed; continuing to teardown" >&2
            fi
        fi
        if [[ -n "${UPSTREAM_B_PHASE2_CONFIG-}" ]]; then
            echo "runner: phase 2 — recreating upstream-b with AIRGAP_CONFIG=${UPSTREAM_B_PHASE2_CONFIG}" >&2
            if ! env UPSTREAM_B_CONFIG="$UPSTREAM_B_PHASE2_CONFIG" \
                    docker compose "${PROFILE_FLAGS[@]}" up -d --force-recreate --no-deps upstream-b \
                    </dev/null >/dev/null 2>&1; then
                echo "runner: phase-2 recreate of upstream-b failed; continuing to teardown" >&2
            fi
        fi
        if [[ "${PHASE2_RERUN_LG_PRODUCER:-0}" == "1" ]]; then
            echo "runner: phase 2 — re-running lg-producer (second wave)" >&2
            docker compose "${PROFILE_FLAGS[@]}" up -d --force-recreate --no-deps lg-producer \
                </dev/null >/dev/null 2>&1 || echo "runner: phase-2 lg-producer recreate failed" >&2
            if [[ -n "${LG_PRODUCER_B_CONFIG-}" ]]; then
                echo "runner: phase 2 — re-running lg-producer-b (second wave)" >&2
                docker compose "${PROFILE_FLAGS[@]}" up -d --force-recreate --no-deps lg-producer-b \
                    </dev/null >/dev/null 2>&1 || echo "runner: phase-2 lg-producer-b recreate failed" >&2
            fi
            echo "runner: waiting for phase-2 producer wave(s) to finish (max ${MAX_WAIT}s)" >&2
            for p2_cid in "$CONTAINER_NAME" "${PROJECT_NAME}-lg-producer-b-1"; do
                if ! docker inspect "$p2_cid" >/dev/null 2>&1; then continue; fi
                p2_waited=0
                while :; do
                    p2_state="$(docker inspect --format '{{.State.Status}}' "$p2_cid" 2>/dev/null || true)"
                    [[ "$p2_state" == "exited" ]] && { echo "runner: ${p2_cid} state=exited after ${p2_waited}s" >&2; break; }
                    if (( p2_waited >= MAX_WAIT )); then
                        echo "runner: ${p2_cid} did not exit within ${MAX_WAIT}s; forcing kill" >&2
                        docker kill --signal=KILL "$p2_cid" >/dev/null 2>&1 || true
                        break
                    fi
                    sleep 2
                    p2_waited=$((p2_waited + 2))
                done
            done
        fi
        p2_drain="${PHASE2_DRAIN_SECONDS:-45}"
        echo "runner: draining phase-2 pipeline for ${p2_drain}s" >&2
        drained=0
        while (( drained < p2_drain )); do
            sleep 2
            drained=$((drained + 2))
            if (( drained % 10 == 0 )); then
                echo "runner: phase-2 drain progress ${drained}/${p2_drain}s" >&2
            fi
        done
    elif (( rc == 0 )) && (( PAUSE == 0 )) && (( DRAIN_SECONDS > 0 )); then
        echo "runner: ${AUTO_EXIT_FROM} exited rc=0; draining pipeline for ${DRAIN_SECONDS}s" >&2
        drained=0
        while (( drained < DRAIN_SECONDS )); do
            sleep 2
            drained=$((drained + 2))
            if (( drained % 10 == 0 )); then
                echo "runner: drain progress ${drained}/${DRAIN_SECONDS}s" >&2
            fi
        done
    fi

    # Optional post-drain orchestration hook. When the manifest declares
    #     POST_DRAIN_SCRIPT=/absolute/path/to/script.sh
    # (or a path relative to tests/env/), the runner invokes it here — after
    # the main producer has exited and the pipeline has drained, but BEFORE
    # the sink is sent SIGTERM and its final summary is captured. This is
    # the slot for orchestration that needs one-shot `docker compose run`
    # containers in the MIDDLE of a test (not at startup via `up -d`, and
    # not a simple verdict check like VERIFY_SCRIPT which only runs at the
    # very end). Used by TC-14: export gaps with `create`, replay them with
    # `resend`, then wait for the resent events to flow through downstream →
    # dedup → sink before the sink's shutdown-summary is captured — so the
    # FINAL verdict reflects the post-resend (gap-filled) state. Manifest
    # variables (CREATE_CONFIG, RESEND_CONFIG, DEDUP_ENV_FILE, TC14_*, ...)
    # are already exported into the ambient environment by the
    # `set -o allexport` around the manifest `source` above, so the script
    # sees them automatically; PROJECT_NAME is set later in this function
    # so it's passed explicitly, same as the VERIFY_SCRIPT convention.
    # The script runs with cwd=tests/env so `docker compose --profile X
    # run --rm X` joins the already-running stack's project/network (the
    # compose file pins `name: airgap-testenv` so this works regardless of
    # cwd nesting). A non-zero exit here is fatal to the testcase, same as
    # a producer failure — the hook is doing real pipeline work, not just
    # reporting.
    if (( rc == 0 )) && (( PAUSE == 0 )) && [[ -n "${POST_DRAIN_SCRIPT-}" ]]; then
        post_drain_path="$POST_DRAIN_SCRIPT"
        if [[ ! -f "$post_drain_path" && -f "$HERE/$POST_DRAIN_SCRIPT" ]]; then
            post_drain_path="$HERE/$POST_DRAIN_SCRIPT"
        fi
        if [[ -x "$post_drain_path" || -f "$post_drain_path" ]]; then
            echo
            echo "─── post-drain hook: ${post_drain_path} ───" >&2
            set +e
            PROJECT_NAME="$PROJECT_NAME" TC_ID="$TC_ID" bash "$post_drain_path"
            post_drain_rc=$?
            set -e
            if (( post_drain_rc != 0 )); then
                echo "runner: POST_DRAIN_SCRIPT failed (rc=${post_drain_rc})" >&2
                rc=$post_drain_rc
            fi
        else
            echo "runner: POST_DRAIN_SCRIPT='$POST_DRAIN_SCRIPT' not found (looked at '$post_drain_path')" >&2
            rc=1
        fi
    fi

    # Give the background log tail a short moment to flush any remaining
    # output from the services (lg-producer's final line, dedup summary, etc.)
    # before we kill the tail and tear everything down.
    sleep 2
    kill "$LOGS_PID" 2>/dev/null || true
    wait "$LOGS_PID" 2>/dev/null || true
    LOGS_PID=""

    # Nudge the sink (and dedup) into their shutdown-summary code path.
    # LogGenerator now installs a JVM shutdown hook, so SIGTERM (the default
    # for `docker compose stop` / `docker kill`) is enough to trigger the
    # summary — SIGINT is no longer required. Send SIGTERM via `docker kill`,
    # then poll the sink log for the summary line before the final dump.
    # Skipped under -pause (the user wants the stack to stay up).
    #
    # Sink-identity fallback: when the manifest uses the AUTO_EXIT_PROFILE
    # / AUTO_EXIT_SERVICE path (e.g. TC-7's tc7-binary-test which does its
    # own produce + consume + verify roundtrip) there's no separate lg-sink
    # container. The auto-exit container IS the sink and has already
    # printed the LG-compatible summary lines on its way out, so we point
    # SINK_CID at it and skip the SIGTERM dance.
    SINK_CID="${PROJECT_NAME}-lg-sink-1"
    DEDUP_CID="${PROJECT_NAME}-dedup-1"
    sink_is_auto_exit=0
    if ! docker inspect "$SINK_CID" >/dev/null 2>&1; then
        SINK_CID="$CONTAINER_NAME"
        sink_is_auto_exit=1
    fi
    if (( PAUSE == 0 )); then
        for cid in "$SINK_CID" "$DEDUP_CID"; do
            if (( sink_is_auto_exit == 1 )) && [[ "$cid" == "$SINK_CID" ]]; then
                # Already exited; nothing to kick.
                continue
            fi
            if docker inspect "$cid" >/dev/null 2>&1; then
                echo "runner: sending SIGTERM to ${cid} to trigger summary" >&2
                docker kill --signal=SIGTERM "$cid" </dev/null >/dev/null 2>&1 || true
            fi
        done
        # Wait up to ~20s for the sink's "Number of unique received numbers"
        # line (the one we actually parse) to appear. Dedup has its own
        # MISSING-REPORT output that LogGenerator doesn't gate on. Skipped
        # when the sink IS the auto-exit container (summary was emitted
        # before the container exited and we already have the logs).
        if (( sink_is_auto_exit == 0 )) && docker inspect "$SINK_CID" >/dev/null 2>&1; then
            summary_waited=0
            while (( summary_waited < 20 )); do
                if docker logs "$SINK_CID" 2>&1 | grep -q 'Number of unique received numbers:'; then
                    echo "runner: sink summary captured after ${summary_waited}s" >&2
                    break
                fi
                sleep 1
                summary_waited=$((summary_waited + 1))
            done
            if (( summary_waited >= 20 )); then
                echo "runner: sink summary not seen within 20s — escalating to SIGKILL" >&2
                docker kill --signal=KILL "$SINK_CID" </dev/null >/dev/null 2>&1 || true
                sleep 2
            fi
        fi
    fi

    # Dump the final producer / sink / dedup logs explicitly so the summary
    # is always visible in the testcase output, even if `logs -f` buffered.
    echo
    echo "--- final lg-producer log ---"
    docker logs "$CONTAINER_NAME" 2>&1 | tail -20 || true
    for svc in lg-sink dedup; do
        cid="${PROJECT_NAME}-${svc}-1"
        if docker inspect "$cid" >/dev/null 2>&1; then
            echo
            echo "--- final ${svc} log ---"
            docker logs "$cid" 2>&1 | tail -40 || true
        fi
    done

    # Verdict block: compare what the producer sent with what the sink
    # observed (unique received + duplicates reported by LogGenerator).
    # Only meaningful when both services are present in this role.
    if docker inspect "$SINK_CID" >/dev/null 2>&1; then
        prod_log="$(docker logs "$CONTAINER_NAME" 2>&1 || true)"
        sink_log="$(docker logs "$SINK_CID"      2>&1 || true)"

        sent="$(   printf '%s\n' "$prod_log" | grep -oE 'transferred [0-9]+ lines'                 | tail -1 | grep -oE '[0-9]+' || true)"
        received="$(printf '%s\n' "$sink_log" | grep -oE 'Number of unique received numbers: [0-9]+' | tail -1 | grep -oE '[0-9]+' || true)"
        dupes="$(  printf '%s\n' "$sink_log" | grep -oE 'Duplicate detection found: [0-9]+'       | tail -1 | grep -oE '[0-9]+' || true)"
        nextexp="$(printf '%s\n' "$sink_log" | grep -oE 'Next expected number: [0-9]+'            | tail -1 | grep -oE '[0-9]+' || true)"

        echo
        echo "─── testcase ${TC_ID} verdict ───"
        if [[ -z "$sent" ]]; then
            echo "  sent      : (lg-producer did not report 'transferred N lines')"
            report_check "testcase ${TC_ID} lg-producer sent-count" "a 'transferred N lines' line" "not found" 1 || true
            if (( rc == 0 )); then rc=1; fi
        else
            echo "  sent      : ${sent}"
        fi
        if [[ -z "$received" ]]; then
            echo "  received  : (lg-sink summary line not emitted; check '--- final lg-sink log ---' above)"
            report_check "testcase ${TC_ID} lg-sink summary" "a 'Number of unique received numbers' line" "not found" 1 || true
            if (( rc == 0 )); then rc=1; fi
        else
            echo "  received  : ${received} unique"
        fi
        if [[ -n "$dupes" ]]; then
            echo "  duplicates: ${dupes}"
        fi
        if [[ -n "$nextexp" ]]; then
            echo "  next-exp  : ${nextexp}"
        fi
        if [[ -n "$sent" && -n "$received" ]]; then
            missing=$(( sent - received ))
            if (( missing < 0 )); then missing=0; fi
            echo "  missing   : ${missing}"
            # Extract the specific missing counter ranges from the LG
            # sink's "Gaps found: N." block (one or more lines of
            # "<lo>-<hi>" immediately following, terminated by
            # "Duplicate detection found:"). Printed when the chain is
            # run with -l/--long OR whenever anything is actually
            # missing — the IDs help diagnose random UDP burst loss
            # ("which event got dropped?") without re-running.
            if (( missing > 0 )); then
                missing_ids="$(printf '%s\n' "$sink_log" \
                    | awk '
                        /^Gaps found:/            { grab = 1; next }
                        /^Duplicate detection/    { grab = 0 }
                        grab && NF && $0 ~ /^[0-9]+-[0-9]+$/ { print }
                    ' \
                    | awk '
                        {
                            split($0, a, "-")
                            if (a[1] == a[2]) {
                                printf "%s%s", (NR > 1 ? ", " : ""), a[1]
                            } else {
                                printf "%s%s", (NR > 1 ? ", " : ""), $0
                            }
                        }
                        END { if (NR > 0) printf "\n" }
                    ')"
                if [[ -n "$missing_ids" ]]; then
                    echo "  missing-ids: ${missing_ids}"
                fi
            fi
            # Some testcases are *supposed* to produce duplicates (e.g. TC-1
            # delivers every event twice via two sendingThreads). The
            # manifest can declare EXPECTED_DUPLICATES=N to pass under those
            # conditions; the default is 0. Likewise some testcases
            # intentionally create gaps (e.g. TC-13 uses a deliverFilter to
            # drop half the stream so dedup emits non-zero MISSING-REPORT
            # counters). EXPECTED_MISSING=N relaxes the strict missing==0
            # requirement in exactly the same way.
            #
            # EXPECTED_MISSING also accepts a `min-max` range syntax
            # (e.g. `EXPECTED_MISSING=40-60`) because Kafka's sticky
            # partitioner distributes a counter stream unevenly across
            # partitions, so a chain that *should* lose "half" the events
            # by Kafka offset may actually drop anywhere in a narrow
            # window around 50%. The strict `N` form is still the default.
            expected_dupes="${EXPECTED_DUPLICATES:-0}"
            expected_missing_spec="${EXPECTED_MISSING:-0}"
            observed_dupes="${dupes:-0}"
            # Parse EXPECTED_MISSING: either "N" (exact) or "LO-HI" (range).
            if [[ "$expected_missing_spec" == *-* ]]; then
                expected_missing_lo="${expected_missing_spec%-*}"
                expected_missing_hi="${expected_missing_spec#*-}"
                missing_in_range() { (( missing >= expected_missing_lo && missing <= expected_missing_hi )); }
                expected_missing_desc="${expected_missing_lo}..${expected_missing_hi} as expected"
                expected_missing_phrase="${expected_missing_lo}..${expected_missing_hi} missing"
            else
                expected_missing_lo="$expected_missing_spec"
                expected_missing_hi="$expected_missing_spec"
                missing_in_range() { (( missing == expected_missing_lo )); }
                expected_missing_desc="${expected_missing_lo} as expected"
                expected_missing_phrase="${expected_missing_lo} missing"
            fi
            if (( rc == 0 )) && missing_in_range && [[ "$observed_dupes" == "$expected_dupes" ]]; then
                if (( expected_missing_lo == 0 && expected_missing_hi == 0 )); then
                    missing_phrase="0 missing"
                else
                    missing_phrase="${missing} missing (${expected_missing_desc})"
                fi
                if [[ "$expected_dupes" == "0" ]]; then
                    echo "  result    : PASS (${received}/${sent} received, ${missing_phrase}, 0 duplicate)"
                else
                    echo "  result    : PASS (${received}/${sent} received, ${missing_phrase}, ${observed_dupes} duplicate as expected)"
                fi
            else
                echo "  result    : FAIL (rc=${rc}, missing=${missing}, expected-missing=${expected_missing_spec}, duplicates=${observed_dupes}, expected-duplicates=${expected_dupes})"
                # Only override a success rc; never mask a producer failure.
                if (( rc == 0 )); then rc=1; fi
            fi

            # Per-assertion green/red lines (see lib/report.sh) so a reviewer
            # can see exactly what was checked and why it passed or failed
            # without reading the shell logic above.
            if missing_in_range; then missing_check_rc=0; else missing_check_rc=1; fi
            report_check "testcase ${TC_ID} events delivered" "${sent} sent, ${expected_missing_phrase}" "${received} received, ${missing} missing" "$missing_check_rc" || true
            if [[ "$observed_dupes" == "$expected_dupes" ]]; then dupes_check_rc=0; else dupes_check_rc=1; fi
            report_check "testcase ${TC_ID} duplicate count" "${expected_dupes}" "${observed_dupes} duplicates" "$dupes_check_rc" || true
        fi
    fi
    if [[ -n "${sent-}" || -n "${received-}" ]]; then
        report_summary "testcase ${TC_ID}" "$rc" || true
    fi

    # Optional post-verdict verification hook. When the manifest declares
    #     VERIFY_SCRIPT=/absolute/path/to/script.sh
    # (or a path relative to tests/env/), the runner invokes it once the
    # pipeline-level verdict is in. The script inherits the manifest
    # environment (TC_ID, PROJECT_NAME, SINK_CID, ...) plus VERIFY_RC_IN
    # (the rc from the verdict so far). If the script exits non-zero we
    # downgrade the verdict to FAIL and surface its output. Used by TC-7
    # to byte-compare every message the sink saw against the deterministic
    # pattern the producer sent (manual TC-7 does md5sum/diff; this is the
    # automated equivalent).
    if [[ -n "${VERIFY_SCRIPT-}" ]]; then
        verify_path="$VERIFY_SCRIPT"
        if [[ ! -f "$verify_path" && -f "$HERE/$VERIFY_SCRIPT" ]]; then
            verify_path="$HERE/$VERIFY_SCRIPT"
        fi
        if [[ -x "$verify_path" || -f "$verify_path" ]]; then
            echo
            echo "─── verify-script: ${verify_path} ───"
            set +e
            VERIFY_RC_IN="$rc" \
            TC_ID="$TC_ID" \
            PROJECT_NAME="$PROJECT_NAME" \
            CONTAINER_NAME="$CONTAINER_NAME" \
            SINK_CID="$SINK_CID" \
                bash "$verify_path"
            verify_rc=$?
            set -e
            if (( verify_rc == 0 )); then
                echo "─── verify-script: PASS ───"
            else
                echo "─── verify-script: FAIL (rc=${verify_rc}) ───"
                if (( rc == 0 )); then rc=1; fi
            fi
        else
            echo "runner: VERIFY_SCRIPT='$VERIFY_SCRIPT' not found (looked at '$verify_path')" >&2
            if (( rc == 0 )); then rc=1; fi
        fi
    fi

    if (( PAUSE )); then
        STACK_PAUSED=1
        print_pause_hints "$rc"
        exit "$rc"
    fi

    # Normal path: trap will perform `docker compose down -v --remove-orphans`.
    exit "$rc"
fi

COMPOSE_ARGS=()
if ((${#PROFILE_FLAGS[@]})); then
    COMPOSE_ARGS+=("${PROFILE_FLAGS[@]}")
fi
COMPOSE_ARGS+=(up)
if ((${#PASSTHROUGH[@]})); then
    COMPOSE_ARGS+=("${PASSTHROUGH[@]}")
fi
if ((${#SERVICES[@]})); then
    COMPOSE_ARGS+=("${SERVICES[@]}")
fi

exec docker compose "${COMPOSE_ARGS[@]}"
