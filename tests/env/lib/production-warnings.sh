#!/usr/bin/env bash

# Validate the complete warning set, including multiplicity and final ordering.
# Expected entries are comma-separated "setting=value" pairs.
validate_production_warnings() {
    local log="$1" expected="$2" phases="$3"
    awk -v expected="$expected" -v phases="$phases" '
        BEGIN {
            n = split(expected, entries, ",")
            for (i = 1; i <= n; i++) wanted[entries[i]] = 1
        }
        /\[PRODUCTION-CONFIG\]/ {
            if ($0 !~ /\[WARN\]/ || $0 !~ / risk=.+/) {
                print "Malformed warning: " $0
                bad = 1
            }
            envelope = $0
            sub(/^.*\[PRODUCTION-CONFIG\] phase=/, "", envelope)
            phase = envelope
            sub(/ .*/, "", phase)
            sub(/^[^ ]+ setting=/, "", envelope)
            sub(/ risk=.*/, "", envelope)
            sub(/ value=/, "=", envelope)
            if (!(envelope in wanted) || (phase != "startup" && phase != "shutdown") ||
                (phases == "startup" && phase != "startup")) {
                print "Unexpected warning: " $0
                bad = 1
            }
            seen[phase SUBSEP envelope]++
            if (phase == "shutdown") shutdown = 1
            else if (shutdown) {
                print "Startup warning after shutdown"
                bad = 1
            }
            next
        }
        shutdown && NF {
            print "Output after final shutdown warning block: " $0
            bad = 1
        }
        END {
            for (entry in wanted) {
                if (seen["startup" SUBSEP entry] != 1) {
                    print "Expected exactly one startup warning: " entry
                    bad = 1
                }
                if (phases == "both" && seen["shutdown" SUBSEP entry] != 1) {
                    print "Expected exactly one shutdown warning: " entry
                    bad = 1
                }
            }
            exit bad
        }
    ' "$log"
}
