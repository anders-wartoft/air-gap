#!/usr/bin/env bash

# Select the current Maven project's JAR, never an arbitrary leftover build.
current_dedup_jar() {
    local root="$1" override="$2" label="$3" jar version
    jar="$override"
    if [[ -z "$jar" ]]; then
        if ! command -v mvn >/dev/null 2>&1; then
            echo "$label: Maven is required to resolve the current JAR; set ${label}_CURRENT_DEDUP_JAR explicitly instead" >&2
            return 2
        fi
        if ! version="$(mvn -f "$root/java-streams/pom.xml" help:evaluate \
                -Dexpression=project.version -q -DforceStdout -Dstyle.color=never)"; then
            echo "$label: unable to resolve the current Maven project version" >&2
            return 2
        fi
        if [[ ! "$version" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]]; then
            echo "$label: invalid Maven project version: $version" >&2
            return 2
        fi
        jar="$root/java-streams/target/air-gap-deduplication-fat-$version-linux-amd64.jar"
    fi
    if [[ ! -s "$jar" ]]; then
        echo "$label: current dedup JAR missing: $jar; run 'make build-java' or set ${label}_CURRENT_DEDUP_JAR" >&2
        return 2
    fi
    echo "$label: using dedup JAR $jar" >&2
    printf '%s\n' "$jar"
}
