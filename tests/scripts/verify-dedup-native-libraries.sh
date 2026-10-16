#!/usr/bin/env bash
set -euo pipefail

JAR="${1:?usage: verify-dedup-native-libraries.sh <fat-jar>}"
MAX_BYTES="${MAX_DEDUP_JAR_BYTES:-41943040}"

[[ -f "$JAR" ]] || {
    echo "fat JAR not found: $JAR" >&2
    exit 1
}

entries="$(jar tf "$JAR")"
native_entries="$(printf '%s\n' "$entries" |
    grep -E '(^librocksdbjni-.*\.(so|dll|jnilib)$|^linux/.*/libzstd-jni-.*\.so$|^darwin/.*/libzstd-jni-.*\.dylib$|^freebsd/.*/libzstd-jni-.*\.so$|^win/.*/libzstd-jni-.*\.dll$|^org/xerial/snappy/native/.*/[^/]+\.(so|dll|dylib|jnilib|a)$|^net/jpountz/util/.*/liblz4-java\.(so|dll|dylib)$)' || true)"

expected_patterns=(
    '^librocksdbjni-linux64\.so$'
    '^linux/amd64/libzstd-jni-.*\.so$'
    '^org/xerial/snappy/native/Linux/x86_64/libsnappyjava\.so$'
    '^net/jpountz/util/linux/amd64/liblz4-java\.so$'
)

for pattern in "${expected_patterns[@]}"; do
    count="$(printf '%s\n' "$native_entries" | grep -Ec "$pattern" || true)"
    if [[ "$count" != "1" ]]; then
        echo "expected exactly one native library matching $pattern, found $count" >&2
        printf '%s\n' "$native_entries" >&2
        exit 1
    fi
done

native_count="$(printf '%s\n' "$native_entries" | grep -c . || true)"
if [[ "$native_count" != "${#expected_patterns[@]}" ]]; then
    echo "fat JAR contains unsupported native libraries:" >&2
    printf '%s\n' "$native_entries" >&2
    exit 1
fi

size="$(wc -c <"$JAR" | tr -d '[:space:]')"
if (( size > MAX_BYTES )); then
    echo "fat JAR is $size bytes; expected at most $MAX_BYTES bytes" >&2
    exit 1
fi

echo "dedup fat JAR verified: $size bytes, $native_count Linux AMD64 native libraries"
