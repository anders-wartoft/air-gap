#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/report.sh"
PROJECT="airgap-tc8-systemd-$$"

compose() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.tc8-systemd.yml" "$@"
}

cleanup() {
    local rc=$?
    trap - EXIT
    if (( rc != 0 )); then
        compose logs --no-color >&2 || true
        compose exec -T systemd journalctl --no-pager -n 100 >&2 || true
    fi
    if ! compose down --timeout 10 --rmi local; then
        echo "TC-8 systemd teardown failed for project $PROJECT" >&2
        rc=1
    fi
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

compose up -d --build
ready=0
for (( attempt=0; attempt<30; attempt++ )); do
    if compose exec -T systemd systemctl show --property=Version --value >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done
report_check "TC-8 container systemd" "responsive within 30s" "ready=$ready" "$((1-ready))"

compose exec -T systemd systemctl start airgap-downstream@tcp.service airgap-upstream@tcp.service
for service in upstream downstream; do
    if compose exec -T systemd bash -s -- "$service" <<'CHECK'
set -euo pipefail
trap 'echo "TC-8 systemd check failed at line $LINENO: $BASH_COMMAND" >&2' ERR
service="$1"
unit="airgap-${service}@tcp.service"
log="/var/log/airgap/${service}.log"
pid="$(systemctl show "$unit" --property=MainPID --value)"
test "$pid" -gt 0
systemctl is-active --quiet "$unit"
for (( attempt=0; attempt<30; attempt++ )); do
    if test -s "$log"; then break; fi
    sleep 1
done
test -s "$log"
mv "$log" "${log}.1"
systemctl reload "$unit"
for (( attempt=0; attempt<30; attempt++ )); do
    if test -f "$log" && grep -q 'SIGHUP handling completed' "$log"; then break; fi
    sleep 1
done
grep -q 'SIGHUP handling completed' "$log"
grep -q 'SIGHUP received' "${log}.1"
test "$(systemctl show "$unit" --property=MainPID --value)" = "$pid"
systemctl is-active --quiet "$unit"
old_bytes="$(wc -c < "${log}.1")"
new_bytes="$(wc -c < "$log")"
sleep 3
test "$(wc -c < "${log}.1")" -eq "$old_bytes"
test "$(wc -c < "$log")" -gt "$new_bytes"
CHECK
    then
        report_check "TC-8 systemctl reload $service" \
            "same PID, completed reload, new log grows and rotated log stops growing" "verified" 0
    else
        report_check "TC-8 systemctl reload $service" \
            "same PID, completed reload, new log grows and rotated log stops growing" "failed" 1
    fi
done
report_summary "TC-8 systemd reload" 0
