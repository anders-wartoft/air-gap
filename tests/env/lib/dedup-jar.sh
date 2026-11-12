#!/usr/bin/env bash

# Require one build per platform, never guess from version numbers or timestamps.
current_dedup_jar() {
    local root="$1" override="$2" label="$3" arch="${4:-amd64}" jar candidate
    local candidates=()
    jar="$override"
    if [[ -z "$jar" ]]; then
        for candidate in "$root"/java-streams/target/air-gap-deduplication-fat-*-linux-"$arch".jar; do
            [[ -f "$candidate" ]] || continue
            candidates+=("$candidate")
        done
        if (( ${#candidates[@]} == 0 )); then
            echo "$label: dedup JAR missing for linux-$arch; run 'make build-java' or set ${label}_CURRENT_DEDUP_JAR" >&2
            return 2
        fi
        if (( ${#candidates[@]} != 1 )); then
            echo "$label: multiple dedup JARs for linux-$arch; run 'make build-java' for a clean build or set ${label}_CURRENT_DEDUP_JAR" >&2
            printf '  %s\n' "${candidates[@]}" >&2
            return 2
        fi
        jar="${candidates[0]}"
    fi
    if [[ ! -s "$jar" ]]; then
        echo "$label: current dedup JAR missing: $jar; run 'make build-java' or set ${label}_CURRENT_DEDUP_JAR" >&2
        return 2
    fi
    echo "$label: using dedup JAR $jar" >&2
    printf '%s\n' "$jar"
}
