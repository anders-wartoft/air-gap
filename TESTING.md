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
