#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib/dedup-jar.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/airgap-dedup-jar-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin" "$WORK/repo/java-streams/target"
cat > "$WORK/bin/mvn" <<'EOF'
#!/usr/bin/env bash
if [[ "${FAKE_MAVEN_FAILURE:-0}" == 1 ]]; then exit 1; fi
printf '%s' "${FAKE_MAVEN_VERSION:-0.1.18-SNAPSHOT}"
EOF
chmod +x "$WORK/bin/mvn"
export PATH="$WORK/bin:$PATH"
OLD="$WORK/repo/java-streams/target/air-gap-deduplication-fat-0.1.14-SNAPSHOT-linux-amd64.jar"
CURRENT="$WORK/repo/java-streams/target/air-gap-deduplication-fat-0.1.18-SNAPSHOT-linux-amd64.jar"
FUTURE="$WORK/repo/java-streams/target/air-gap-deduplication-fat-9.0.0-SNAPSHOT-linux-amd64.jar"
printf 'old' > "$OLD"
printf 'current' > "$CURRENT"
printf 'future' > "$FUTURE"
[[ "$(current_dedup_jar "$WORK/repo" "" TC41)" == "$CURRENT" ]]
echo "PASS: selects current project version, not oldest or highest leftover"
[[ "$(current_dedup_jar "$WORK/repo" "$OLD" TC42)" == "$OLD" ]]
echo "PASS: explicit override is preserved"
rm "$CURRENT"
rc=0
current_dedup_jar "$WORK/repo" "" TC41 > "$WORK/out" 2> "$WORK/error" || rc=$?
[[ "$rc" == 2 && ! -s "$WORK/out" ]]
grep -q 'current dedup JAR missing' "$WORK/error"
echo "PASS: missing current artifact never falls back to another version"
rc=0
FAKE_MAVEN_FAILURE=1 current_dedup_jar "$WORK/repo" "" TC42 > "$WORK/out" 2> "$WORK/error" || rc=$?
[[ "$rc" == 2 && ! -s "$WORK/out" ]]
grep -q 'unable to resolve' "$WORK/error"
echo "PASS: Maven failure is explicit"
rc=0
FAKE_MAVEN_VERSION='invalid version' current_dedup_jar "$WORK/repo" "" TC42 > "$WORK/out" 2> "$WORK/error" || rc=$?
[[ "$rc" == 2 && ! -s "$WORK/out" ]]
grep -q 'invalid Maven project version' "$WORK/error"
echo "PASS: invalid version output cannot select a JAR"
rc=0
current_dedup_jar "$WORK/repo" "$WORK/missing.jar" TC42 > "$WORK/out" 2> "$WORK/error" || rc=$?
[[ "$rc" == 2 && ! -s "$WORK/out" ]]
grep -q 'TC42_CURRENT_DEDUP_JAR' "$WORK/error"
echo "PASS: missing override fails with actionable diagnostics"
