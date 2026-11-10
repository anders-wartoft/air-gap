# Running Tests

## Go tests

### All packages at once

```bash
go test ./...
```

This covers every package including unit tests and integration-style tests that use mock adapters (no live Kafka or UDP required).

### Individual packages

| Package | Command |
| ------- | ------- |
| create (gap bundle creator) | `go test ./src/create/...` |
| resend | `go test ./src/resend/...` |
| upstream | `go test ./src/upstream/...` |
| downstream | `go test ./src/downstream/...` |
| protocol / UDP / encryption / format | `go test ./src/test/...` |

Add `-v` to any command to see individual test names:

```bash
go test ./src/create/... -v
```

### Fuzz testing

The upstream package includes a fuzz target:

```bash
go test ./src/upstream/... -fuzz=FuzzKafkaHandler -fuzztime=30s
```

## Java tests (deduplication engine)

```bash
cd java-streams && mvn test
```

This runs JUnit 5 tests for `GapDetector`, `GapDetectorSerialization`, and `PartitionDedupApp`.

### TC-46: dedup state regression tests with Docker Kafka

```bash
cd tests/env
./run-testcase.sh 46
# Also available through the batch runner:
./run-testcases.sh 46
```

See [TC-46](tests/documents/TESTCASES.md#tc-46--bounded-dedup-state-and-transaction-safe-crash-recovery)
and [REQ-59](tests/documents/REQUIREMENTS.md#req-59--bounded-transaction-safe-dedup-state)
for the procedure and acceptance criteria. The standalone
`bash tests/env/verify-dedup-state.sh` command (from the repository root)
remains supported.

The script starts an isolated single-broker Compose project, runs Maven unit
tests and the `docker-tests` integration profile, then removes only that
project's containers/volumes. It needs Docker, Maven and Java 17+; it does not
need a prebuilt JAR. The broker binds to localhost port 19092. Override
`DEDUP_TEST_KAFKA_PORT` if that port is in use, or `KAFKA_IMAGE` for an
alternative compatible broker image.

Tests use unique topics/application IDs and child dedup JVMs with fresh local
state directories. They verify hard crashes before/after transaction commit,
cold changelog restoration, delayed-copy suppression, migration of a fragmented
legacy value larger than 1 MiB, bounded new state records, repair of existing
delete-only changelogs and compaction for newly created changelogs. Consumers
assert committed output using `read_committed`. The broker allows the oversized
legacy fixture, but dedup retains its default 1 MiB producer limit.

Reports are under `java-streams/target/failsafe-reports/`; child JVM logs are
retained under `java-streams/target/dedup-it-logs/`. The integration tests do not
run in plain `mvn test`; the normal unit suite does not require Docker.

After packaging, also run the existing Kafka-outage/restart checks against the
exact Java 17 runtime JAR (rather than an older JAR left in the build directory):

```bash
TC9_RESTART_JAR_NAME=air-gap-deduplication-fat-0.1.18-SNAPSHOT-linux-amd64.jar \
  bash tests/env/verify-tc9-dedup-restart.sh
```

TC-41 and TC-42 select the Linux AMD64 JAR matching the current Maven project
version and print the selected path. Rebuild when that exact artifact is
missing; stale JARs in `target` are not used as a fallback. Explicit
`TC41_CURRENT_DEDUP_JAR` / `TC42_CURRENT_DEDUP_JAR` overrides remain available.
Their artifact-selection regressions run with
`bash tests/env/test-dedup-jar.sh` (no Docker required).

Build the universal fat JAR and every platform-specific fat JAR with:

```bash
cd java-streams && mvn clean package
```

Build all JARs and create the verified Linux AMD64 package alias with:

```bash
make build-java
tests/scripts/verify-dedup-native-libraries.sh \
  java-streams/target/air-gap-deduplication-fat-*.jar
```

The Maven tests verify that all RocksDB platforms produce a JAR containing
exactly the matching RocksDB, zstd, Snappy and LZ4 libraries, and reject an
unmapped RocksDB platform. The production verifier additionally enforces a
40 MiB maximum for the Linux AMD64 JAR.

## Make shortcuts

```bash
make test        # Go tests (go test ./...)
make test-java   # Java tests (mvn test)
```

## Integration / end-to-end tests

End-to-end tests require a running Kafka cluster. Start the test environment with Docker Compose:

```bash
make test-docker-up    # start containers (tests/docker-compose.yml)
# ... run scenario scripts from tests/scripts/
make test-docker-down  # stop containers
```

See [tests/README.md](tests/README.md) for scenario details.
