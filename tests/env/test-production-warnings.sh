#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/production-warnings.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-warning-validator.XXXXXX")"
trap 'rm -r "$WORK"' EXIT

printf '[WARN] [PRODUCTION-CONFIG] phase=startup setting=source value=random risk=Synthetic traffic.\n' > "$WORK/startup"
printf '[INFO] Work completed\n' > "$WORK/work"
printf '[WARN] [PRODUCTION-CONFIG] phase=shutdown setting=source value=random risk=Synthetic traffic.\n' > "$WORK/shutdown"
cat "$WORK/startup" "$WORK/work" "$WORK/shutdown" > "$WORK/valid"
validate_production_warnings "$WORK/valid" "source=random" both
validate_production_warnings "$WORK/startup" "source=random" startup

reject() {
    local label="$1" file="$2" expected="${3:-source=random}"
    if validate_production_warnings "$file" "$expected" both > "$WORK/diagnostic" 2>&1; then
        echo "FAIL: validator accepted $label" >&2
        exit 1
    fi
    [[ -s "$WORK/diagnostic" ]] || { echo "FAIL: no diagnostic for $label" >&2; exit 1; }
    printf 'PASS: rejected %s\n' "$label"
}

reject "missing shutdown" "$WORK/startup"
reject "missing startup" "$WORK/shutdown"
reject "unexpected setting or effective value" "$WORK/valid" "source=kafka"
cat "$WORK/startup" "$WORK/valid" > "$WORK/duplicate"
reject "duplicate warning" "$WORK/duplicate"
cat "$WORK/valid" "$WORK/work" > "$WORK/late"
reject "output after shutdown block" "$WORK/late"
sed 's/\[WARN\]/[INFO]/g' "$WORK/valid" > "$WORK/severity"
reject "wrong severity" "$WORK/severity"
sed 's/risk=Synthetic traffic\./risk=/g' "$WORK/valid" > "$WORK/risk"
reject "empty risk" "$WORK/risk"
cat "$WORK/shutdown" "$WORK/startup" > "$WORK/order"
reject "startup after shutdown" "$WORK/order"
echo "Production-warning validator: PASS"
