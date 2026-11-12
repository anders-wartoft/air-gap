#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/dedup-jar.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-dedup-jar-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/repo/java-streams/target" "$WORK/cache"
TARGET="$WORK/repo/java-streams/target"
CURRENT="$TARGET/air-gap-deduplication-fat-0.1.19-SNAPSHOT-linux-amd64.jar"
ARM="$TARGET/air-gap-deduplication-fat-0.1.19-SNAPSHOT-linux-arm64.jar"
OLD="$TARGET/air-gap-deduplication-fat-0.1.18-SNAPSHOT-linux-amd64.jar"
LEGACY="$WORK/cache/air-gap-deduplication-fat-0.1.10-SNAPSHOT.jar"
printf 'current' > "$CURRENT"
printf 'arm current' > "$ARM"
printf 'legacy' > "$LEGACY"
printf 'alias' > "$TARGET/air-gap-deduplication-fat.jar"
printf 'universal' > "$TARGET/air-gap-deduplication-fat-0.1.19-SNAPSHOT.jar"
printf 'musl' > "$TARGET/air-gap-deduplication-fat-0.1.19-SNAPSHOT-linux-amd64-musl.jar"
[[ "$(current_dedup_jar "$WORK/repo" "" TC41)" == "$CURRENT" ]]
[[ "$(current_dedup_jar "$WORK/repo" "" TC16 arm64)" == "$ARM" ]]
echo "PASS: one build per platform; other platforms, aliases and legacy cache coexist"
printf 'old' > "$OLD"
rc=0
current_dedup_jar "$WORK/repo" "" TC42 > "$WORK/out" 2> "$WORK/error" || rc=$?
[[ "$rc" == 2 && ! -s "$WORK/out" ]]
grep -q 'multiple dedup JARs' "$WORK/error"
grep -Fq "$CURRENT" "$WORK/error"
grep -Fq "$OLD" "$WORK/error"
echo "PASS: ambiguous builds fail instead of guessing by version"
[[ "$(current_dedup_jar "$WORK/repo" "$CURRENT" TC42)" == "$CURRENT" ]]
[[ "$(current_dedup_jar "$WORK/repo" "$LEGACY" TC41)" == "$LEGACY" ]]
echo "PASS: explicit overrides remain supported"
rm "$CURRENT" "$OLD"
rc=0
current_dedup_jar "$WORK/repo" "" TC16 > "$WORK/out" 2> "$WORK/error" || rc=$?
[[ "$rc" == 2 && ! -s "$WORK/out" ]]
grep -q 'dedup JAR missing' "$WORK/error"
echo "PASS: missing platform artifact fails with clean build instructions"
rc=0
current_dedup_jar "$WORK/repo" "$WORK/missing.jar" TC42 > "$WORK/out" 2> "$WORK/error" || rc=$?
[[ "$rc" == 2 && ! -s "$WORK/out" ]]
grep -q 'TC42_CURRENT_DEDUP_JAR' "$WORK/error"
echo "PASS: missing override fails with actionable diagnostics"
touch "$CURRENT"
rc=0
current_dedup_jar "$WORK/repo" "" TC16 > "$WORK/out" 2> "$WORK/error" || rc=$?
[[ "$rc" == 2 && ! -s "$WORK/out" ]]
echo "PASS: empty artifact is rejected"
