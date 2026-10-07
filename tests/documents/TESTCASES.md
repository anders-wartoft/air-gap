# Testcases

Source of truth for air-gap functional test cases. (Originated as
`tests/documents/Testfall.xlsx`; this document now supersedes the
spreadsheet.)

Each test case:

* **Covers** — requirement IDs from [REQUIREMENTS.md](./REQUIREMENTS.md).
* **Implemented in** — SNAPSHOT version.
* **Runner** — the `./run-testcase.sh` invocation, run from the
  [tests/env/](../env/) directory.
* **Setup** — any pre-condition (Kafka topics, cleared retention, keystores,
  …).
* **Procedure** — numbered steps from the original Testfall spreadsheet,
  translated into English and lightly modernised for the Docker testbed.
* **Expected result** — what to look for in the output.
* **What it proves** — the final "testfallet visar att …" sentence.

> Every path like `./run-testcase.sh`, `./generate-kafka-certs.sh`, and
> `testcases/NN.env` is relative to `tests/env/`. The runner lives there and
> expects to be invoked from there.

Common conventions used throughout:

* **LogGenerator** is an external Java tool (minimum version 1.1-5). The jar
  lives in `tests/env/bin/` on the host; the Docker environment ships two
  `lg-*` services that resolve it via a `LogGenerator-*.jar` glob so the
  version in the filename is irrelevant. The classic command line, used
  outside of Docker, is:

  ```bash
  java -jar LogGenerator-1.1-7.jar -pf <properties-file>
  ```

  LogGenerator reads a `.properties` file whose format is distinct from
  air-gap properties — the ones under `config/testcases/*-lg-*.properties`.

* **Kafka console tools** come from any Kafka distribution. In the Docker
  environment the brokers themselves ship them:

  ```bash
  docker compose exec kafka-upstream kafka-topics --bootstrap-server kafka-upstream:9092 --list
  docker compose exec kafka-upstream kafka-console-producer --bootstrap-server kafka-upstream:9092 --topic transfer
  ```

* **SSL client config** when connecting to the SSL listener (port 9094/8094):

  ```properties
  security.protocol=SSL
  ssl.truststore.location=/etc/kafka/secrets/kafka-trust.jks
  ssl.truststore.password=changeit
  ```

  Mounted inside the broker container at `/etc/kafka/secrets/`.

* **Clearing a topic** between runs (the Testfall text repeats this often):

  ```bash
  bin/kafka-configs.sh --bootstrap-server <bs> --alter --entity-type topics --entity-name <t> \
      --add-config retention.ms=1000,segment.ms=1000
  # wait a few seconds, then restore
  bin/kafka-configs.sh --bootstrap-server <bs> --alter --entity-type topics --entity-name <t> \
      --add-config retention.ms=604800000,segment.ms=604800000
  ```

  In the Docker env, prefer `docker compose down -v` and start the testcase
  again — the stack is cheap to tear down.

* **sitia.nu suffix** on hostnames in the properties files is preserved and
  resolved via Docker network aliases on each cluster's brokers.

---

## Topic bootstrap

Before any testcase runs the Docker environment stands up two one-shot
`topic-init-*` services. They create the following topics with the retention
and partition counts the testers used on bare metal (12 h retention,
`retention.ms=43200000`):

* **Upstream cluster** — `transfer`, `transfer2` (5 partitions each).
* **Downstream cluster** — `transfer`, `transfer2`, `transfer-11a`,
  `transfer-11b`, `transfer-12a`, `transfer-12b`, `dedup`, `dedup2`, `gaps`,
  `gaps2` (15 partitions each).
* **Second upstream cluster** (profile `second-cluster`) — `transfer`,
  `transfer2` (5 partitions each).

The air-gap services and the deduplicator depend on
`service_completed_successfully` of their side's `topic-init-*`, so by the
time they start the topics they expect are already there. This replaces the
manual `kafka-topics.sh --create ...` steps from the original Testfall text.

---

## Automated chain runner

The runner in [tests/env/run-testcases.sh](../env/run-testcases.sh)
executes a comma-separated spec of testcases back-to-back (with full
teardown between each). It understands ranges and wildcards:

```sh
./run-testcases.sh                 # no spec: defaults to 1- (all of them)
./run-testcases.sh 1,4-7,9,11      # specific list
./run-testcases.sh 1-              # all of them (trailing '*' is optional)
./run-testcases.sh --long 9,11     # include every verdict block, not just failures
./run-testcases.sh 9 -- -pause     # keep the stack up after TC-9
```

Each testcase prints a short summary line in the chain summary, with its
wall-clock runtime shown inline (e.g. `(1m26s)`) — handy for comparing
throughput across different machines/hosts. The total chain runtime is
printed on its own line at the very end (`total runtime: Xm YYs (Zs)`).

* `✅ testcase N PASS` — pass; verdict shown only in `--long` mode.
* `❌ testcase N FAIL` — fail; verdict block always shown so you see why
  without re-running.
* `🤔 testcase N SKIP` — the manifest `testcases/NN.env` doesn't declare
  `LG_PRODUCER_CONFIG` or `AUTO_EXIT_SERVICE`, so there's no auto-exit
  path and running it would hang the chain. Chain-safe testcases listed
  as "**Chain run**" below are: **TC-1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
  11, 12, 13, 14, 15, 17, 18, 19, 20, 21, 22, 23, 24, 25**. The others still work with `./run-testcase.sh N` directly
  for the manual procedure. Note: TC-16 SKIPs by deliberate design, not
  because it's unimplemented — see its section below for why a
  performance benchmark can't have a meaningful automated chain variant.

The runner's single-testcase variant [run-testcase.sh](../env/run-testcase.sh)
understands these manifest knobs:

| Knob | Effect |
| --- | --- |
| `LG_PRODUCER_CONFIG` | Activate the `lg-producer` compose profile, use it as the auto-exit container. |
| `LG_SINK_CONFIG` | Activate the `lg-sink` compose profile; its summary is the verdict source. |
| `AUTO_EXIT_PROFILE` + `AUTO_EXIT_SERVICE` | Override the LG defaults with a bespoke test service (e.g. TC-7's `tc7-binary-test` Go-based tester). |
| `EXPECTED_DUPLICATES=N` | Verdict pass requires observed duplicates to equal N (default 0). |
| `EXPECTED_MISSING=N` or `EXPECTED_MISSING=LO-HI` | Verdict pass requires observed missing events to equal N (default 0) or fall inside the inclusive `LO..HI` range. Used by TC-13 whose `deliverFilter` intentionally drops half the stream so dedup's `[MISSING-REPORT]` counters have something to report; the range form absorbs Kafka's uneven sticky-partitioner distribution. |
| `LG_STARTUP_DELAY=N` | Seconds to sleep inside `lg-entrypoint.sh` after compose dependencies are satisfied but before the first event is produced. Used by TC-9 and TC-10 so dedup's Kafka Streams tasks finish REBALANCING → RUNNING before the first event flows (otherwise the first-event-of-a-fresh-partition race against EOS-v2 task restoration can drop one event). Zero (default) means no wait. |
| `TC_DRAIN_SECONDS=N` | How long to wait after auto-exit for pipeline flush. |
| `UPSTREAM_A_RESTART_AT_SECONDS=N` | TC-2 style: force-recreate `upstream-a` at T+N s (graceful SIGTERM → fresh container). |
| `UPSTREAM_A_PHASE2_CONFIG=path` | TC-9 style: after the LG producer exits, recreate `upstream-a` with this alternative config. |
| `PHASE2_DRAIN_SECONDS=N` | Drain window after the phase-2 swap. |
| `VERIFY_SCRIPT=path` | Run an arbitrary verifier after the verdict (relative paths resolve against `tests/env/`). |
| `POST_DRAIN_SCRIPT=path` | Run an orchestration script AFTER the producer drains but BEFORE the sink's final summary is captured — for mid-test one-shot `docker compose run` steps whose effects the FINAL verdict should reflect (e.g. TC-14's create→resend gap-fill). Non-zero exit fails the testcase. |

Many of the manifest files set extra `UPSTREAM_A_BOOTSTRAP` /
`DOWNSTREAM_BOOTSTRAP` env vars — those override the compose defaults so
TLS testcases actually connect over 9094/8094 instead of the PLAINTEXT
9092/8092 defaults.

### Reporting individual assertions (management-readable output)

Every `verify-tc*.sh` and `tc*-test.sh` script sources
[tests/env/lib/report.sh](../env/lib/report.sh) and uses its
`report_check` function to print ONE line per concrete assertion, in
the form:

```text
<subject>, expected <expected>, <actual>
```

colored green on pass / red on fail. This is deliberately separate
from the narrative progress `echo` lines (e.g. "── producing 8 literal
payloads ──") — `report_check` lines are the ONLY thing a reviewer
needs to scan to see exactly what was tested and why it passed or
failed, without reading the shell logic that produced it (e.g. for a
management-facing summary of what a test run actually verified).
Example from TC-19:

```text
Input "TC19 BLOCK ssn 123-45-6789 end", expected blocked, not found
Input "TC19 BLOCK apikey api_key=abc123xyz end", expected blocked, not found
Upstream total_filtered counter, expected 6, 6
```

`report_summary <label> <rc>` prints one final green/red banner line
(`─── <label>: PASS/FAIL ───`) summarizing the whole script, used once
at the very end after all `report_check` calls. New chain-run scripts
should use both — see any `verify-tc*.sh` for the pattern. Colors are
unconditional (set `NO_COLOR=1` to suppress, per https://no-color.org).


---

## TC-1 — Retransmission with multiple time offsets

Verify that logs can be sent with multiple time offsets simultaneously.

* Covers: REQ-2, REQ-3a, REQ-3c
* Implemented in: 0.1.1-SNAPSHOT
* Runner: `./run-testcase.sh 1`
* External: LogGenerator ≥ 1.1-6 (SIGTERM-aware shutdown hook)

**Setup.**

* Kafka upstream has a topic `transfer` (created by `topic-init-upstream`).
* `tests/env/testcases/01.env` selects
  `config/testcases/upstream-airgap-1.properties` for upstream and
  `config/testcases/downstream-airgap-1.properties` for downstream.
* The bare-metal procedure in the original Testfall replaces the air-gap
  downstream with LogGenerator-UDP. The Docker env keeps the air-gap
  downstream running (it owns UDP/1234 inside the network) and reads events
  from `kafka-downstream[transfer]` using the LG sink config
  `downstream-lg-1-docker.properties` instead.

**Chain run (Docker — automated).**

The runner starts the stack, LogGenerator sends 1000 counter events to
`kafka-upstream[transfer]`, upstream-airgap-1 reads them through two
sendingThreads (consumer-group suffixes `test-No-delay` and
`test-10-seconds-delay`), each thread delivers every event once over UDP, the
downstream decrypts/decodes and writes to `kafka-downstream[transfer]`, and
the LG sink reads them from Kafka. Every counter should therefore appear
exactly twice.

Manifest knobs in `tests/env/testcases/01.env`:

* `LG_PRODUCER_CONFIG=/airgap/config/testcases/upstream-lg-1.properties`
* `LG_SINK_CONFIG=/airgap/config/testcases/downstream-lg-1-docker.properties`
* `EXPECTED_DUPLICATES=1000` — the whole point of TC-1 is 2× delivery, so
  the verdict's "duplicates must be 0" default is replaced with "duplicates
  must equal 1000".
* `TC_DRAIN_SECONDS=20` so the delayed sendingThread has time to catch up
  to the last producer message before teardown.

Expected chain verdict:

```text
✅ testcase 1 PASS
─── testcase 1 verdict ───
  sent      : 1000
  received  : 1000 unique
  duplicates: 1000
  next-exp  : 1001
  missing   : 0
  result    : PASS (1000/1000 received, 0 missing, 1000 duplicate as expected)
```

**Procedure (bare-metal / interactive).**

1. **Start the UDP log sink** (LogGenerator-UDP) with
   `downstream-lg-1.properties`:

   ```properties
   input=udp
   port=1234
   filter=gap
   regex=_(\d+)$
   duplicate-detection=true
   output=cmd
   ```

   Expected: `INFO: Serving UDP server on port 1234`.

1. **Start air-gap upstream.** Properties file
   `config/testcases/upstream-airgap-1.properties` ships
   `sendingThreads=[{"No-delay":0},{"10-seconds-delay":-10}]` — two logical
   sender threads, one at the current timestamp, one back-dated 10 s.

   Expected on startup: the upstream logs to stdout (the committed properties
   file keeps `logFileName=` commented out so the Docker image doesn't
   fatal on a missing `./tmp/` directory), and the LogGenerator-UDP window
   shows `Upstream_1 starting up`.

1. **Start the LogGenerator producer** against the upstream Kafka with
   `upstream-lg-1.properties`:

   ```properties
   input=counter
   string=TEST_
   filter=guard
   filter=gap
   regex=_(\d+)$
   duplicate-detection=true
   output=kafka
   client-id=test
   topic=transfer
   bootstrap-server=kafka-upstream:9092,kafka-upstream-2:8092
   eps=50
   statistics=true
   limit=1000
   ```

1. Watch the LogGenerator-UDP window. Each `TEST_N` must arrive **twice**
   (one from the no-delay thread, one from the delayed thread).

1. Wait until the last log line is `TEST_1000`. Takes ~20 s. The producer
   terminates on its own.

1. Stop the LogGenerator-UDP with `Ctrl-C` (SIGINT) or `kill` (SIGTERM) —
   either triggers the JVM shutdown hook and prints a summary.

**Expected result.**

```text
Duplicate detection found: 1000 duplicate (or more) values.
1 - 2
2 - 2
...
1000 - 2
Number of unique received numbers: 1000
Next expected number: 1001
```

**What it proves.**

Retransmission can be opted in and configured with multiple `sendingThreads`
each delivering the same logs at a different time offset. The dedup downstream
would collapse these duplicates in a full chain (TC-9).

---

## TC-2 — Upstream restart does not lose logs

Verify that even if the upstream air-gap process restarts mid-stream, every
log is delivered at least once.

* Covers: REQ-5, REQ-9
* Implemented in: 0.1.1-SNAPSHOT
* Runner: `./run-testcase.sh 2`

**Setup.**

* Kafka upstream topic `transfer` (created by topic-init).
* `tests/env/testcases/02.env` selects
  `config/testcases/upstream-airgap-2.properties`
  (`sendingThreads=[{"No-delay":0}]`, no delayed thread) for upstream and
  `config/testcases/downstream-airgap-2.properties` for downstream.

**Chain run (Docker — automated graceful-restart).**

The runner starts the stack and the LG producer begins sending 1000 counter
events at `eps=50` (≈ 20 s runtime). At `T+10 s` — mid-send — the runner
force-recreates `upstream-a`:

```bash
docker compose up -d --force-recreate --no-deps upstream-a
```

Compose sends SIGTERM to the Go process first (10 s grace); the upstream's
signal handler runs its graceful shutdown — Sarama commits consumer-group
offsets, UDP buffers flush — and the container is replaced with a fresh one
holding the same `AIRGAP_CONFIG`. On restart Sarama resumes from the
committed offset, so no event is lost or duplicated.

Manifest knobs in `tests/env/testcases/02.env`:

* `LG_PRODUCER_CONFIG=/airgap/config/testcases/upstream-lg-2.properties`
* `LG_SINK_CONFIG=/airgap/config/testcases/downstream-lg-2-docker.properties`
* `UPSTREAM_A_RESTART_AT_SECONDS=10` — triggers the mid-run restart
  watchdog in [run-testcase.sh](../env/run-testcase.sh).
* `EXPECTED_DUPLICATES=0` — the graceful variant must be lossless *and*
  duplicate-free. (If you flip to a hard-kill test later, swap in a
  tolerance-based knob; a comment in `02.env` points at the exact spot.)
* `TC_DRAIN_SECONDS=25` so the fresh upstream has time to drain the second
  half of the producer window.

Expected chain verdict:

```text
✅ testcase 2 PASS
─── testcase 2 verdict ───
  sent      : 1000
  received  : 1000 unique
  duplicates: 0
  missing   : 0
  result    : PASS (1000/1000 received, 0 missing, 0 duplicate)
```

**Procedure (bare-metal / interactive).**

1. Start LogGenerator-UDP as in TC-1 using `downstream-lg-2.properties`.
1. Start air-gap upstream. The config emits only once.
1. Start the LogGenerator producer `upstream-lg-2.properties`, limit 1000.
1. The producer takes ~20 s.
1. **Graceful restart test:** during the run, send `Ctrl-C` (or SIGTERM) to
   the upstream process. Wait for it to stop. Start it again with the same
   command. The remaining logs should be delivered.
1. Stop the LogGenerator-UDP with `Ctrl-C` or `kill` (SIGTERM also triggers
   the shutdown-hook summary).

   Expected: 0 or very few duplicates, 1000 unique numbers.

1. **Hard-kill restart test:** repeat the procedure, but this time use
   `pkill -f "upstream-airgap-2.properties"` instead of `Ctrl-C` (on macOS
   where `pkill -f` is unavailable, grab the PID from the log line
   `[INFO] pid:11803` and `kill -9 11803`).
1. Restart the upstream and wait for the producer to finish.
1. Stop the LogGenerator-UDP.

   Expected: a handful of duplicates (because Kafka rewinds to the last
   committed offset for the client), the first duplicate is **not** `1`, 1000
   unique numbers, next expected 1001.

**Expected result.** (hard-kill case)

```text
Duplicate detection found: 19 duplicate (or more) values.
170 - 2
...
188 - 2
Number of unique received numbers: 1000
Next expected number: 1001
```

**What it proves.**

Kafka client offsets, driven by the configured `groupID`, allow upstream to
resume from the last known entry across restarts (graceful or hard) without
losing logs. The chain variant proves the graceful (SIGTERM) path is
dupe-free; the manual hard-kill variant proves at-least-once holds even
when the shutdown handler never fires.

---

## TC-3 — Plain-text traffic

Verify that logs can flow unencrypted across the air-gap.

* Covers: REQ-6
* Implemented in: 0.1.1-SNAPSHOT
* Runner: `./run-testcase.sh 3`

**Setup.**

* Kafka upstream topic `transfer`.
* `upstream-airgap-3.properties` sets `source=kafka`, `logLevel=DEBUG` and
  `generateNewSymmetricKeyEvery=50` but `encryption=false` by omission. The
  committed file now has `logFileName=` commented out so the Docker
  container doesn't fatal on a missing `./tmp/` dir on startup.
* `tests/env/testcases/03.env` points the downstream at
  `config/testcases/downstream-airgap-3.properties`, which binds UDP on
  `0.0.0.0` so the upstream-a container can reach it across the Docker
  bridge.

**Chain run (Docker — automated).**

LG producer → `kafka-upstream[transfer]` → upstream-airgap-3 (one
sendingThread, plain-text) → UDP → downstream-airgap-3 → `kafka-downstream[transfer]`
→ LG sink (`downstream-lg-3-docker.properties`, reads Kafka). Every event
is delivered exactly once end-to-end.

Manifest knobs in `tests/env/testcases/03.env`:

* `LG_PRODUCER_CONFIG=/airgap/config/testcases/upstream-lg-3.properties`
* `LG_SINK_CONFIG=/airgap/config/testcases/downstream-lg-3-docker.properties`
* `EXPECTED_DUPLICATES=0`
* `TC_DRAIN_SECONDS=15`

Expected chain verdict:

```text
✅ testcase 3 PASS
─── testcase 3 verdict ───
  sent      : 1000
  received  : 1000 unique
  duplicates: 0
  missing   : 0
  result    : PASS (1000/1000 received, 0 missing, 0 duplicate)
```

**Procedure (bare-metal / interactive).**

1. Start LogGenerator-UDP with `downstream-lg-3.properties`.
1. Start air-gap upstream with `upstream-airgap-3.properties`.
1. Start the LogGenerator producer with `upstream-lg-3.properties`, limit
   1000.
1. Expected: 1000 logs in one thread.
1. When log 1000 is received, stop both LogGenerators with `Ctrl-C` or
   `kill` (SIGTERM also triggers the shutdown-hook summary). The upstream
   producer terminates on its own.

**Expected result.**

The text `TEST_1` through `TEST_1000` is printed in the downstream window.

**What it proves.**

Air-gap handles plain-text traffic end-to-end, Kafka→upstream→UDP.

---

## TC-4 — Encrypted traffic to and from Kafka

Verify TLS to the Kafka brokers at both ends.

* Covers: REQ-7b, REQ-25, REQ-26, REQ-29
* Implemented in: 0.1.2-SNAPSHOT
* Runner: `./run-testcase.sh 4`

**Setup.**

* Both Kafka clusters have SSL on 9094 / 8094 (default in this environment).
* The runner automatically calls `./generate-kafka-certs.sh` (writes
  `certs/kafka/ssl/kafka-*.keystore.jks`, `kafka-trust.jks`, and the client
  certificates, keys, and passwords under `certs/tmp/`). Run it manually
  before starting Compose directly.
* The two broker clusters are signed by **different CAs** — the script's
  idempotent "keep existing" check means the shipped
  `kafka-downstream.keystore.jks` is still signed by `MyKafkaCA`
  (included in the `certs/tmp/kafka-ca.crt` CA bundle) while
  `kafka-upstream.keystore.jks` is a
  freshly regenerated keystore signed by `airgap-testenv-ca`
  (= `certs/kafka/ssl/testenv-ca.crt`). The brokers' combined
  `kafka-trust.jks` trusts both CAs, so client certs from either CA work
  on either side (and mTLS is off anyway via
  `KAFKA_SSL_CLIENT_AUTH=none`).
* The committed `*-airgap-4.properties` files reference
  `certs/tmp/kafka-ca.crt` because that matched both clusters in the
  pre-regen era. For the Docker chain run we instead use the dedicated
  `*-airgap-4-docker.properties` files which point each side at the
  correct CA for its cluster — upstream → `testenv-ca.crt`, downstream →
  `kafka-ca.crt`.

**Chain run (Docker — automated).**

LG producer writes 1000 counter events over **plaintext** to
`kafka-upstream[transfer]`. upstream-airgap-4 reads them over **SSL on
9094** and sends them via plain UDP to downstream-airgap-4, which writes
them to `kafka-downstream[transfer]` over **SSL on 9094**. LG sink reads
`kafka-downstream[transfer]` over plaintext for the counter verdict.
The SSL hops under test are therefore airgap↔Kafka at both ends.

Manifest knobs in `tests/env/testcases/04.env`:

* `UPSTREAM_A_CONFIG=/airgap/config/testcases/upstream-airgap-4-docker.properties`
  (caFile → `certs/kafka/ssl/testenv-ca.crt`).
* `DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-airgap-4-docker.properties`
  (targetIP → `0.0.0.0`, caFile → `certs/tmp/kafka-ca.crt`).
* `UPSTREAM_A_BOOTSTRAP=kafka-upstream.sitia.nu:9094,kafka-upstream.sitia.nu:8094`
  — critical, because the compose default for this env var is the
  **PLAINTEXT** 9092 ports which would silently bypass the SSL hop under
  test.
* `DOWNSTREAM_BOOTSTRAP=kafka-downstream.sitia.nu:9094,kafka-downstream.sitia.nu:8094`
  — same reason.
* `LG_PRODUCER_CONFIG` / `LG_SINK_CONFIG` reuse the plaintext configs; the
  LG hops are not what's being verified.
* `EXPECTED_DUPLICATES=0`, `TC_DRAIN_SECONDS=20` (extra slack for the TLS
  handshake and first metadata fetch).

Expected chain verdict:

```text
✅ testcase 4 PASS
─── testcase 4 verdict ───
  sent      : 1000
  received  : 1000 unique
  duplicates: 0
  missing   : 0
  result    : PASS (1000/1000 received, 0 missing, 0 duplicate)
```

If TLS misconfiguration slips in, Sarama's log will carry
`client/metadata got error from broker -1 ... x509: certificate signed by
unknown authority` — most commonly from a caFile that doesn't match the
specific cluster's actual CA.

**Procedure (bare-metal / interactive).**

1. **Verify topic listing over SSL** from inside a Kafka container:

   ```bash
   docker compose exec kafka-downstream bash -c '
     cat >/tmp/ssl.props <<EOF
     security.protocol=SSL
     ssl.truststore.location=/etc/kafka/secrets/kafka-trust.jks
     ssl.truststore.password=changeit
     EOF
     kafka-topics --bootstrap-server kafka-downstream:9094,kafka-downstream-2:8094 \
       --command-config /tmp/ssl.props --list
   '
   ```

   `transfer` must appear.

1. **Start a Kafka console consumer** on the downstream, reading `transfer`
   over SSL:

   ```bash
   docker compose exec kafka-downstream kafka-console-consumer \
     --bootstrap-server kafka-downstream:9094 \
     --consumer.config /tmp/ssl.props \
     --topic transfer --from-beginning
   ```

1. **Start a Kafka console producer** on the upstream, writing `transfer`
   over SSL:

   ```bash
   docker compose exec kafka-upstream kafka-console-producer \
     --bootstrap-server kafka-upstream:9094,kafka-upstream-2:8094 \
     --producer.config /tmp/ssl.props \
     --topic transfer
   ```

1. **Start air-gap downstream** via `./run-testcase.sh 4`. Expected: boots
   and listens; its startup message (and all later events) appear in the
   downstream consumer.

1. **Start air-gap upstream** (included in the runner). It should connect to
   Kafka over SSL.

1. In the console-producer window, type a line and press Enter. Expected: a
   new `>` prompt appears, and the text is echoed by the console-consumer.

1. **LogGenerator producer** (`upstream-lg-4.properties`) at eps=50,
   limit=1000. The log entries `TEST_1`, `TEST_2`, … arrive in the
   console-consumer.

1. Stop the console-consumer and restart it with output redirected to a
   file:

   ```bash
   kafka-console-consumer ... --topic transfer > result.txt
   ```

1. Run LogGenerator again (step 7). Wait for completion.

1. Stop the console-consumer. Move `result.txt` next to the LogGenerator
    offline verifier (`upstream-lg-4b.properties`) and run it:

    ```bash
    java -jar LogGenerator-1.1-7.jar -pf upstream-lg-4b.properties
    ```

    Expected: `Number of unique received numbers: 1000`.

1. **Statistics check** — the running downstream prints:

    ```text
    [INFO] STATISTICS: {"id":"Downstream_4","interval":30,"received":0,"sent":0,"time":...,"total_received":1000,"total_sent":1000}
    ```

1. The running upstream prints the matching JSON on its side.

1. Both sides log `DEBUG` entries (logLevel=DEBUG in the testcase config).

1. Stop air-gap downstream with `Ctrl-C`, start the `-4b` variant that
    overrides `logLevel=WARN`. Expected: no DEBUG/INFO lines after
    `Validating the configuration…`.

1. Same for upstream `-4b`.

**What it proves.**

TLS to a Kafka cluster works on both the upstream and downstream air-gap
sides, event counters are logged on both sides at the configured interval,
and `logLevel` is honoured.

---

## TC-5 — Configuration via environment variables

Verify that every property can be overridden by `AIRGAP_UPSTREAM_*` /
`AIRGAP_DOWNSTREAM_*` environment variables with no config file content.

* Covers: REQ-22
* Implemented in: 0.1.2-SNAPSHOT
* Runner: `./run-testcase.sh 5`

**Setup.**

* Kafka cluster(s) already up.
* The bare-metal procedure walks every required field one at a time. The
  chain-run version is a smoke-level check that reuses TC-3's plain-text
  configs and lets the pre-existing `AIRGAP_*` env-var pass-through in
  [docker-compose.yml](../env/docker-compose.yml) do the actual overriding.

**Chain run (Docker — automated smoke).**

The committed TC-3 properties files contain file-level values that would
**not** work in the Docker env (`nic=lo0`, `targetIP=127.0.0.1`,
`bootstrapServers=kafka-upstream.sitia.nu:9092,…`). The pipeline only
functions because the compose file exports a working override through
every `UPSTREAM_A_*` / `DOWNSTREAM_*` knob. If any of the overrides stop
being applied the pipeline immediately breaks (packets never cross the
Docker bridge, hostnames never resolve). A clean `1000/1000` chain
verdict is therefore a live proof that env-var overriding is wired end-
to-end.

Manifest knobs in `tests/env/testcases/05.env`:

* `UPSTREAM_A_CONFIG=/airgap/config/testcases/upstream-airgap-3.properties`
* `DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-airgap-3.properties`
* `UPSTREAM_A_NIC=eth0`, `UPSTREAM_A_TARGET_IP=downstream`,
  `UPSTREAM_A_BOOTSTRAP=kafka-upstream:9092,kafka-upstream-2:8092`,
  `DOWNSTREAM_NIC=eth0`,
  `DOWNSTREAM_BOOTSTRAP=kafka-downstream:9092,kafka-downstream-2:8092` —
  these are set explicitly even though they shadow the compose defaults,
  so the manifest documents which fields the chain is actively exercising.
* `LG_PRODUCER_CONFIG` / `LG_SINK_CONFIG` reuse TC-3's.

On startup the upstream and downstream logs should emit lines like:

```text
[INFO] Overriding nic with environment variable: AIRGAP_UPSTREAM_NIC with value: eth0
[INFO] Overriding targetIP with environment variable: AIRGAP_UPSTREAM_TARGET_IP with value: downstream
[INFO] Overriding bootstrapServers with environment variable: AIRGAP_UPSTREAM_BOOTSTRAP_SERVERS with value: kafka-upstream:9092,kafka-upstream-2:8092
```

Expected chain verdict:

```text
✅ testcase 5 PASS
─── testcase 5 verdict ───
  sent      : 1000
  received  : 1000 unique
  duplicates: 0
  missing   : 0
  result    : PASS (1000/1000 received, 0 missing, 0 duplicate)
```

For the exhaustive "every required field in turn" walkthrough, use the
bare-metal procedure below — that version is inherently interactive and
is left as-is.

**Procedure (bare-metal / interactive).**

The original testfall walks through the required env vars one at a time,
observing a `[FATAL] Missing required configuration: ...` message for each
missing field until all required values are present. The Docker env already
supplies the complete set via `.env` — this testcase is best done
interactively by overriding each `AIRGAP_UPSTREAM_*` / `AIRGAP_DOWNSTREAM_*`
one at a time:

1. `AIRGAP_UPSTREAM_ID=Testcase_5` → fails with "Missing required
   configuration: nic".
1. Add `AIRGAP_UPSTREAM_NIC=eth0` → fails with "Missing: targetIP".
1. Add `AIRGAP_UPSTREAM_SOURCE=kafka` → fails with "Missing: targetIP"
   (unchanged — SOURCE alone does not satisfy it).
1. Add `AIRGAP_UPSTREAM_TARGET_IP=downstream` → fails with "Missing:
   bootstrapServers".
1. Add `AIRGAP_UPSTREAM_BOOTSTRAP_SERVERS=kafka-upstream:9092,kafka-upstream-2:8092`
   → fails with "Missing: topic".
1. Add `AIRGAP_UPSTREAM_TOPIC=transfer` → fails with "Missing: groupID".
1. Add `AIRGAP_UPSTREAM_GROUP_ID=test` → now the configuration validates and
   upstream boots.
1. Optional: `AIRGAP_UPSTREAM_GENERATE_NEW_SYMMETRIC_KEY_EVERY=100` to
   observe the override logging.
1. Optional: `AIRGAP_UPSTREAM_TARGET_PORT=1234`.
1. To switch Kafka to TLS (reusing the dual listener from this env):

    ```bash
    export AIRGAP_UPSTREAM_CA_FILE=/airgap/certs/kafka/ssl/testenv-ca.crt
    export AIRGAP_UPSTREAM_BOOTSTRAP_SERVERS=kafka-upstream:9094,kafka-upstream-2:8094
    ```

11–17. Repeat the equivalent "Missing required" walk for downstream with
    `AIRGAP_DOWNSTREAM_*` (`ID`, `NIC`, `TARGET_IP`, `TARGET=kafka`,
    `BOOTSTRAP_SERVERS`, `TOPIC`, `TARGET_PORT=1234`). After each addition,
    boot the binary and look at the next complaint until the config is
    complete.

1. Start a Kafka console consumer on the downstream, verify traffic flows.
19–21. Add TLS to downstream the same way (`CA_FILE`, `BOOTSTRAP_SERVERS` on
    port 9094).
1. Override the log destination:
    `AIRGAP_DOWNSTREAM_LOG_FILE_NAME=/tmp/airgap-downstream.log` and confirm
    the file appears with the config dump.
1. `AIRGAP_DOWNSTREAM_LOG_LEVEL=WARN` suppresses DEBUG/INFO after
    "Validating the configuration…".
1. `Ctrl-C` to stop. In the Kafka console consumer, the shutdown message
    `Upstream Testcase_5 terminating by signal…` appears.

**Expected result.**

Every `AIRGAP_*` env var overrides the corresponding property and is logged
on startup:
`[INFO] Overriding <key> with environment variable: AIRGAP_UPSTREAM_<KEY> with value: <value>`.

**What it proves.**

Both upstream and downstream can be fully configured from environment
variables with no properties file.

---

## TC-6 — Encryption across the diode

Verify that the UDP payload between upstream and downstream can be hidden
via the symmetric-key encryption scheme.

* Covers: REQ-7a
* Implemented in: 0.1.2-SNAPSHOT
* Runner: `./run-testcase.sh 6`

**Setup.**

Five terminals plus two Kafka clusters. In the Docker environment:

* Terminal 1: Kafka console producer against the upstream.
* Terminal 2: air-gap upstream (`upstream-airgap-6.properties` sets
  `encryption=true`, `publicKeyFile=certs/server2.pem`).
* Terminal 3: `tcpdump -Ai any port 1234` (as root).
* Terminal 4: air-gap downstream (`downstream-airgap-6.properties`,
  `privateKeyFiles=certs/private*.pem`).
* Terminal 5: Kafka console consumer against the downstream.

**Chain run (Docker — automated).**

Encryption rides on top of TC-4's TLS pipeline — same CA asymmetry
applies (upstream cluster → `testenv-ca.crt`, downstream cluster →
`kafka-ca.crt`). LG producer writes plaintext to `kafka-upstream[transfer]`,
upstream-airgap-6 reads over TLS and sends **encrypted UDP** to
downstream-airgap-6 (symmetric key exchanged via the downstream's public
key, rotated every `generateNewSymmetricKeyEvery=50 s`), downstream
decrypts, writes to `kafka-downstream[transfer]` over TLS, LG sink reads
plaintext for the counter verdict.

Manifest knobs in `tests/env/testcases/06.env`:

* `UPSTREAM_A_CONFIG=/airgap/config/testcases/upstream-airgap-6-docker.properties`
  — identical to the bare-metal `upstream-airgap-6.properties` plus
  `caFile=certs/kafka/ssl/testenv-ca.crt` and `payloadSize=1400` (auto is
  unreliable inside containers).
* `DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-airgap-6-docker.properties`
  — `targetIP=0.0.0.0`, `mtu=1500`, `caFile=certs/tmp/kafka-ca.crt`.
* `UPSTREAM_A_BOOTSTRAP` / `DOWNSTREAM_BOOTSTRAP` override to the SSL
  9094/8094 ports.
* `LG_PRODUCER_CONFIG=/airgap/config/testcases/upstream-lg-6.properties`
  (now topic=transfer and sitia.nu brokers — a stale 192.168.* broker and
  topic=upstream had leaked into the committed file in the past).
* `LG_SINK_CONFIG=/airgap/config/testcases/downstream-lg-6-docker.properties`.
* `EXPECTED_DUPLICATES=0`, `TC_DRAIN_SECONDS=20` (extra slack for the
  key-exchange handshake plus one rotation window).

Expected chain verdict:

```text
✅ testcase 6 PASS
─── testcase 6 verdict ───
  sent      : 1000
  received  : 1000 unique
  duplicates: 0
  missing   : 0
  result    : PASS (1000/1000 received, 0 missing, 0 duplicate)
```

The chain run can't visually verify "payload is not plaintext on the
wire" — that's what the bare-metal tcpdump step below is for. A `PASS`
verdict in the chain proves end-to-end delivery with encryption + TLS
turned on; the bare-metal confirmation remains the authoritative "and
the bytes really are opaque on UDP:1234" check.

**Procedure (bare-metal / interactive).**

1. **Console consumer on the downstream**, `transfer`, over SSL.

1. **Air-gap downstream.** On boot it emits
   `2026-10-10T14:46:52+02:00 Downstream_6 Downstream Downstream_6 starting UDP server on port 1234`
   which the console-consumer must see.

1. **tcpdump.** `tcpdump -Ai any port 1234`. Prints `listening on any…`.

1. **Air-gap upstream.** Boot log must include `[INFO]   encryption: true`.
   Final subscription lines look like:

   ```text
   consumer/broker/2 added subscription to transfer/0
   consumer/broker/2 added subscription to transfer/3
   ...
   ```

   In the console consumer: `Upstream_6 starting up…`. A short burst of
   encrypted packets for the key exchange follows in tcpdump.

1. **Console producer on the upstream.** Open the producer against `transfer`
   over SSL.

1. Type `AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA` + Enter.

1. Verify in terminal 5 the exact string `AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA`
   appears in the downstream topic.

1. Verify in tcpdump the string does **not** appear in plain text (packets
   on port 1234 look like ciphertext). On loopback both directions show in
   tcpdump — the plaintext version from the Kafka side will still appear
   there; the UDP packet between upstream and downstream is what must be
   encrypted.

**What it proves.**

Payload encryption between upstream and downstream hides the content from a
passive observer on the diode link while the receiver still delivers the
plain text to Kafka.

---

## TC-7 — Large log entries (fragmentation / defragmentation)

Verify that payloads bigger than MTU are fragmented by upstream, reassembled
by downstream, and delivered to Kafka intact.

* Covers: REQ-19
* Implemented in: 0.1.2-SNAPSHOT
* Runner: `./run-testcase.sh 7`

**Setup.**

* `Upload.go` + `Download.go` ship under
  [tests/env/bin/](../env/bin/) along with their own `go.mod` / `go.sum`
  (depends on `github.com/segmentio/kafka-go`). They produce / consume
  arbitrary binary files to / from a single Kafka topic.
* Both Kafka clusters have PLAINTEXT on 9092/8092 (default).
* `upstream-airgap-7.properties` fixes `payloadSize=1400`,
  `downstream-airgap-7-docker.properties` fixes `mtu=1500` and binds UDP
  on `0.0.0.0` (the generic `downstream-airgap-7.properties` keeps the
  `127.0.0.1` listen address for bare-metal use).

**Chain run (Docker — automated byte-exact round-trip).**

A one-shot `tc7-binary-test` service (profile `tc7-binary-test`,
image `golang:1.24-alpine`) runs
[tc7-upload-download-test.sh](../env/bin/tc7-upload-download-test.sh) on
start. The script:

1. `go build Upload.go` and `go build Download.go` from a writable copy of
   `/tc` (the volume is RO).
2. Generates `/work/testfile.bin` of `FILE_BYTES=204800` random bytes
   (`dd if=/dev/urandom`) and records its md5.
3. Loops `upload /work/testfile.bin kafka-upstream:9092 transfer`
   `NUM_MESSAGES=10` times.
4. Spawns `download /work/received kafka-downstream:9092 transfer` in the
   background and polls `/work/received/` until all 10 files are present
   (or `TIMEOUT_SECONDS=120` elapses).
5. `md5sum` compares every downloaded file against the original.
6. Emits LG-compatible summary lines so the runner's existing verdict
   parser sees `received: N unique` / `duplicates: 0` / `next-exp: N+1`.
7. Exits 0 only when every file is byte-exact.

Each 200 KB file fragments into ≈ `200000 / 1400 ≈ 150` UDP packets at
`payloadSize=1400`; **~1500 fragments cross the diode per chain run**.
Losing any single fragment kills one md5 compare.

Manifest knobs in `tests/env/testcases/07.env`:

* `AUTO_EXIT_PROFILE=tc7-binary-test` and `AUTO_EXIT_SERVICE=tc7-binary-test`
  — tell [run-testcase.sh](../env/run-testcase.sh) to orchestrate on this
  one-shot container instead of the LG producer/sink pair.
* `TC7_EXPECTED_COUNT=10`, `TC7_FILE_BYTES=204800`,
  `TC7_TIMEOUT_SECONDS=120`.

Expected chain verdict:

```text
✅ testcase 7 PASS
─── testcase 7 verdict ───
  sent      : 10
  received  : 10 unique
  duplicates: 0
  missing   : 0
  result    : PASS (10/10 received, 0 missing, 0 duplicate)
```

If a byte flips or a fragment drops, the driver prints
`[tc7] byte-mismatch: /work/received/N size=X md5=…` and the verdict
flips to FAIL with `missing>0`.

Note on startup noise: `upstream-a` now declares
`depends_on: downstream (required: false)` so you won't see the
`[FATAL] lookup downstream: no such host` cascade that used to appear
before `downstream`'s DNS entry was registered. If it reappears something
has genuinely gone wrong.

**Procedure (bare-metal / interactive).**

1. Verify PLAINTEXT reachability:

   ```bash
   nc -vz kafka-upstream.sitia.nu 9092
   ```

1. **Baseline — upload a binary to Kafka and read it back without the
   air-gap in the middle:**

   ```bash
   cd tests/env/bin
   go run Upload.go /bin/ls kafka-upstream.sitia.nu:9092 transfer
   ```

   Expected: `Sent file /bin/ls (202760 bytes) to Kafka`.

1. Read back:

   ```bash
   go run Download.go ./tmp/ kafka-upstream.sitia.nu:9092 transfer
   ```

   Interrupt once the first file is saved.

1. Compare:

   ```bash
   diff /bin/ls ./tmp/1
   md5sum /bin/ls ./tmp/1
   ```

   Expected: identical.

1. **Now with the air-gap in the middle.** Start downstream and upstream
   (`./run-testcase.sh 7`).

1. Point `Download.go` at the downstream Kafka:

   ```bash
   go run Download.go ./tmp/ kafka-downstream.sitia.nu:9092 transfer
   ```

   It will save one file per message. Startup messages produce small files;
   ignore those.

1. Upload again to the upstream Kafka:

   ```bash
   go run Upload.go /bin/ls kafka-upstream.sitia.nu:9092 transfer
   ```

   Expected: `Sent file /bin/ls (202760 bytes) to Kafka`. In the Download
   window the largest file is ~200 KB and the numbered output of
   `Download.go` records which message index it was.

1. Verify `./tmp/<idx>` is ~200 KB.

1. Compare with the original:

   ```bash
   diff ./tmp/<idx> /bin/ls
   md5sum ./tmp/<idx> /bin/ls
   ```

1. For the brave:

    ```bash
    chmod +x ./tmp/<idx>
    ./tmp/<idx>
    ```

    must produce the same output as `ls`.

**What it proves.**

Payloads significantly larger than the UDP MTU survive the fragmentation and
reassembly logic — including as byte-exact binaries.

---

## TC-8 — logrotate / SIGHUP

Verify that upstream and downstream can swap log files on SIGHUP without
being stopped.

* Covers: REQ-13, REQ-18, REQ-23
* Implemented in: 0.1.2-SNAPSHOT
* Runner: `./run-testcase.sh 8`

**Setup.**

Because `go run` consumes SIGHUP itself, the binaries must be used. Build
them first (bare-metal only — in Docker the compose stack calls the
pre-built binary directly via [entrypoint.sh](../env/entrypoint.sh)):

```bash
make all
```

The chain-run configs are the dedicated `-docker` variants:

* [config/testcases/upstream-airgap-8-docker.properties](../../config/testcases/upstream-airgap-8-docker.properties)
  — `logFileName=/tmp/upstream.log` (writable inside the container; the
  bare-metal file uses `./tmp/upstream.log` which has no backing directory
  in the airgap image).
* [config/testcases/downstream-airgap-8-docker.properties](../../config/testcases/downstream-airgap-8-docker.properties)
  — `logFileName=/tmp/downstream.log` plus `targetIP=0.0.0.0`.

**Chain run (Docker — automated SIGHUP log rotation).**

1. Compose starts `upstream-a`, `downstream`, and the one-shot
   `tc8-settle-timer` (busybox) alongside the Kafka brokers.
2. `tc8-settle-timer` sleeps `TC8_SETTLE_SECONDS=6` so both airgap
   processes have time to open their `/tmp/*.log` files, then emits the
   LG-compatible completion lines and exits.
3. The runner detects the one-shot's exit via the standard auto-exit
   plumbing, drains for 2 s, then invokes
   `VERIFY_SCRIPT=verify-tc8.sh`.
4. [verify-tc8.sh](../env/verify-tc8.sh) is a host-side bash driver.
   For each service in {`upstream-a`, `downstream`} it:
   1. `docker exec` to confirm the configured log file exists.
   2. `docker exec` to `mv` it out of the way (`.log` → `-1.log`).
   3. `docker kill --signal=HUP <container>` — compose's PID 1 is the
      Go binary itself (the entrypoint uses `exec`), so the SIGHUP
      handler fires directly.
   4. Poll (up to 10 s) for the new file at the original path AND for
      `SIGHUP handling completed` to appear in it — the SIGHUP goroutine
      prints "SIGHUP received…" and "Reopening logs for logrotate…" to
      the *old* (renamed) file, then opens the new file and prints
      "SIGHUP handling completed" to it.
   5. `docker exec grep` confirms both markers are where they should be:
      `SIGHUP received` in the rotated file,
      `SIGHUP handling completed` in the fresh file. Any missing marker
      fails verification and dumps the last 10 lines of the offending
      file for diagnosis.
5. verify-tc8.sh exits non-zero if either rotation failed, which the
   runner surfaces as `❌ testcase 8 FAIL`.

Manifest knobs in [testcases/08.env](../env/testcases/08.env):

* `AUTO_EXIT_PROFILE=tc8-settle-timer`,
  `AUTO_EXIT_SERVICE=tc8-settle-timer`
* `TC8_SETTLE_SECONDS=6`
* `VERIFY_SCRIPT=verify-tc8.sh`
* `EXPECTED_DUPLICATES=0`, `TC_DRAIN_SECONDS=2`

Expected chain verdict:

```text
✅ testcase 8 PASS
─── testcase 8 verdict ───
  sent      : 2
  received  : 2 unique
  duplicates: 0
  missing   : 0
  result    : PASS (2/2 received, 0 missing, 0 duplicate)
─── verify-script: /.../tests/env/verify-tc8.sh ───
verify-tc8: ─── upstream-a:/tmp/upstream.log ───
verify-tc8: upstream-a:/tmp/upstream.log: initial size NNN bytes
verify-tc8: upstream-a:/tmp/upstream.log: sending SIGHUP to airgap-testenv-upstream-a-1
verify-tc8: upstream-a:/tmp/upstream.log: new file appeared after 0s
verify-tc8: upstream-a:/tmp/upstream.log: PASS
verify-tc8: ─── downstream:/tmp/downstream.log ───
verify-tc8: downstream:/tmp/downstream.log: PASS
verify-tc8: ─── summary: 2 ok, 0 failed ───
─── verify-script: PASS ───
```

The "sent/received = 2" verdict comes from the settle-timer's emitted
marker lines; the real test is the verify-script block beneath it. A
settle-timer PASS without a verify-script PASS still gets flipped to
FAIL by the runner.

**Procedure (bare-metal / interactive).**

1. **Start downstream** from the binary:

   ```bash
   target/<arch>/downstream config/testcases/downstream-airgap-8.properties
   ```

   Last line: `[INFO] Configuring log to: ./tmp/downstream.log`.

1. Rename the file:

   ```bash
   mv ./tmp/downstream.log ./tmp/downstream-1.log
   ```

1. Find downstream's PID (logged right after Configuration OK as
   `[INFO] pid:40933`) and send SIGHUP:

   ```bash
   kill -HUP <pid>
   ```

   Expected: a fresh `./tmp/downstream.log` is created with
   `[INFO] SIGHUP handling completed`, and the old `./tmp/downstream-1.log`
   ends with `[INFO] SIGHUP received: reopening logs with name
   ./tmp/downstream.log and reloading TLS certificates` followed by
   `[INFO] Reopening logs for logrotate. New name: ./tmp/downstream.log`.

1. Start upstream from the binary:

   ```bash
   target/<arch>/upstream config/testcases/upstream-airgap-8.properties
   ```

   `tmp/upstream.log` opens with
   `[INFO]   logFileName: ./tmp/upstream.log`.

1. Rename:

   ```bash
   mv ./tmp/upstream.log ./tmp/upstream-1.log
   ```

1. Grab upstream's PID (also logged) and `kill -HUP <pid>`.

   Expected: `./tmp/upstream.log` is recreated with `SIGHUP handling
   completed`; old file ends with `SIGHUP received: reopening logs with
   name …` + `Reopening logs for logrotate. New name: …`.

**What it proves.**

Both binaries, run outside a wrapper that would eat SIGHUP, honour SIGHUP
by closing and reopening their configured log file so `logrotate` can rotate
them cleanly.

---

## TC-9 — Deduplication

Verify that logs sent multiple times to downstream are only delivered to the
clean topic once, and that gap detection works dynamically.

* Covers: REQ-4
* Implemented in: 0.1.3-SNAPSHOT
* Runner: `./run-testcase.sh 9`

**Setup.**

* `topic-init-downstream` has created `dedup` and `gaps` on the downstream
  cluster.
* `tests/env/testcases/09.env` enables the `dedup` profile and selects
  `dedup-9.env`.
* `config/testcases/downstream-airgap-9.properties` binds UDP on
  `0.0.0.0` (previously `127.0.0.1`, which silently dropped packets from
  `upstream-a` across the Docker bridge).
* `config/testcases/upstream-airgap-9.properties` carries the gap-creation
  filter `deliverFilter=1,2,3,4,6,7,8,9,11,12,13,14` (deliver 4 of every
  5 — multiples of 5 are the on-purpose gaps);
  `upstream-airgap-9b.properties` is the no-filter variant used by
  phase 2 with a fresh `groupID=test2`.

**Chain run (Docker — two-phase automated).**

Phase 1 (counter mismatch): LG producer sends 100 counter events at
`eps=100` to `kafka-upstream[transfer]`. upstream-a (groupID=test, with
the deliverFilter) sends only ~80 through UDP — multiples of 5 are
dropped on purpose. Downstream writes them to
`kafka-downstream[transfer]`; the Kafka Streams dedup app forwards them
to the clean `dedup` topic and tracks the 20 gaps.

Phase 2 (gap-fill): after the LG producer exits, the runner reads
`UPSTREAM_A_PHASE2_CONFIG=/airgap/config/testcases/upstream-airgap-9b.properties`
and runs:

```bash
docker compose up -d --force-recreate --no-deps upstream-a
```

Compose sends SIGTERM to the running upstream (graceful shutdown
commits the current offsets), then starts a fresh container with
`AIRGAP_CONFIG=…/upstream-airgap-9b.properties`. The new groupID
`test2` is a new consumer group, so Sarama starts from offset 0 and
re-reads **every** event. Dedup discards the ~80 it has already seen
and lets the previously-dropped multiples of 5 through, filling the
gaps. LG sink reads `kafka-downstream[dedup]` and sees all 100 unique
with no duplicates (the dedup app absorbs the second pass).

Manifest knobs in `tests/env/testcases/09.env`:

* `UPSTREAM_A_CONFIG=/airgap/config/testcases/upstream-airgap-9.properties`
  (deliverFilter active).
* `UPSTREAM_A_PHASE2_CONFIG=/airgap/config/testcases/upstream-airgap-9b.properties`
  (triggers the phase-2 force-recreate).
* `PHASE2_DRAIN_SECONDS=45` so the gap-fill events have time to flow all
  the way to the sink.
* `DEDUP_ENV_FILE=../../config/testcases/dedup-9.env`
  (activates the `dedup` profile and gives the dedup app its env vars).
* `LG_PRODUCER_CONFIG=/airgap/config/testcases/upstream-lg-9.properties`
  (bumped to `limit=100 eps=100`, matching this test case's "generate
  100 logs" step).
* `LG_SINK_CONFIG=/airgap/config/testcases/downstream-lg-9.properties`.
* `LG_STARTUP_DELAY=15` — defensive. Dedup needs 10–15 s to finish
  REBALANCING → RUNNING across all 15 partitions before the first
  event lands; TC-9 happens to usually clear this race by chance
  (smaller workload) but the identical pattern caused flakes in
  TC-10, so the same safety is applied here. See
  [lg-entrypoint.sh](../env/lg-entrypoint.sh) for the actual sleep
  implementation.

Expected chain verdict:

```text
✅ testcase 9 PASS
─── testcase 9 verdict ───
  sent      : 100
  received  : 100 unique
  duplicates: 0
  missing   : 0
  result    : PASS (100/100 received, 0 missing, 0 duplicate)
```

**Procedure (bare-metal / interactive).**

1. Verify PLAINTEXT reachability.
1. Clear any leftover data on the upstream `transfer` topic (short-retention
   trick above).
1. Confirm the downstream topic `dedup` exists (topic-init did this).
1. Start LogGenerator-cmd against the downstream `dedup`
   (`downstream-lg-9.properties`). Expected: `Subscribed to topic dedup`.
1. **Start dedup**:

   ```bash
   export $(grep -v '^#' config/testcases/dedup-9.env | xargs)
   java -Dlog4j.configurationFile=./config/log4j2.xml \
        -jar ./java-streams/target/air-gap-deduplication-fat-*.jar
   ```

1. Start air-gap downstream (`downstream-airgap-9.properties`). The
   `[INFO] Downstream_9 starting UDP server on port 1234` message should
   reach the LogGenerator-cmd.
1. Start air-gap upstream with `upstream-airgap-9.properties`. This config
   has `deliverFilter=1,2,3,4,6,7,8,9,11,12,13,14` so **every log whose
   counter is a multiple of 5 is dropped** — creating gaps on purpose.
1. **Start JConsole** (`jconsole`), connect to the dedup process
   (choose "Insecure connection" when the TLS prompt appears).
1. Open MBeans → `nu.sitia.airgap`. If there is a `GapDetector` folder from
   an earlier run, click `purge…` on every topic under *Operations* and
   confirm *Attributes* are now empty.
1. Open a gap-topic reader:

    ```bash
    ./target/<arch>/gaps config/testcases/downstream-airgap-9.properties --topic gaps
    ```

1. **Generate 100 logs**:

    ```bash
    java -jar LogGenerator-1.1-7.jar -pf upstream-lg-9.properties -l 100 --eps 100
    ```

1. Verify the LogGenerator-cmd shows ~100 entries like:

    ```text
    [transfer_4_13749: TEST_92]
    [transfer_3_13764: TEST_96]
    ...
    ```

1. In JConsole → GapDetectors → Attributes → *Refresh*: gaps present in
    every partition (indices divisible by 5).

1. Wait ≥ 2× `GAP_EMIT_INTERVAL_SEC`. The gap topic reader shows the same
    gaps that JConsole does.

1. **Fill the gaps.** Stop the upstream (`Ctrl-C` / SIGTERM — either
   triggers the same graceful offset commit). No errors expected.

1. Restart the upstream with the no-filter variant and a fresh `groupID` so
    Kafka replays everything:

    ```bash
    target/<arch>/upstream config/testcases/upstream-airgap-9b.properties
    ```

1. Refresh *GapDetectors / Attributes* in JConsole. Expected: no gaps.

1. Wait 2× `GAP_EMIT_INTERVAL_SEC`. The gap topic reader shows the
    diminishing-missing sequence:

    ```text
    transfer/0 [0..999]: 3 missing  first: 5, 10, 15
    ...
    transfer/* Total: 11 missing
    ...
    transfer/* Total: 0 missing
    ```

1. Stop the LogGenerator-cmd. Expected:

    ```text
    Duplicate detection found: 0 duplicate (or more) values.
    Number of unique received numbers: 100
    Next expected number: 101
    ```

**What it proves.**

Duplicates are filtered out and gaps can be created and removed dynamically
during a run.

---

## TC-10 — systemd service lifecycle

Verify that upstream, downstream and dedup can all run as systemd services,
with memory bounds applied.

* Covers: REQ-8, REQ-13, REQ-17
* Implemented in: 0.1.3-SNAPSHOT
* Runner: `./run-testcase.sh 10` (exercises the config/dedup path end-
  to-end; the actual systemd aspects — unit files, memory bounds,
  service dependencies — need the RPM installed on the host and stay
  manual)

**Setup.**

Build the binaries plus the dedup jar:

```bash
make all
```

The chain-run variant uses dedicated `-docker` configs on the
`transfer2 / dedup2 / gaps2` topic family (same family the systemd
procedure uses):

* [config/testcases/upstream-airgap-10-docker.properties](../../config/testcases/upstream-airgap-10-docker.properties)
  — reads `transfer2`, `groupID=test`, carries the
  `deliverFilter=1,2,3,4,6,7,8,9,11,12,13,14` that drops multiples of 5.
* [config/testcases/upstream-airgap-10b-docker.properties](../../config/testcases/upstream-airgap-10b-docker.properties)
  — phase 2: same topic, fresh `groupID=test2`, no filter.
* [config/testcases/downstream-airgap-10-docker.properties](../../config/testcases/downstream-airgap-10-docker.properties)
  — `targetIP=0.0.0.0`, `internalTopic=airgap-logs` (as the stock file).
* [config/testcases/dedup-10.env](../../config/testcases/dedup-10.env)
  — `RAW_TOPICS=transfer2`, `CLEAN_TOPIC=dedup2`, `GAP_TOPIC=gaps2`,
  dedicated `STATE_DIR_CONFIG=/tmp/dedup_state_10/`.
* [config/testcases/upstream-lg-10-docker.properties](../../config/testcases/upstream-lg-10-docker.properties)
  — same shape as the bare-metal `upstream-lg-10.properties` but with
  `limit=100 eps=100` instead of `limit=1000 eps=200`. The 10× smaller
  burst keeps TC-10 inside the UDP buffer envelope that TC-9 is already
  proven to clear.
* The upstream `-docker` configs both carry `eps=50` which engages the
  Go upstream's `TokenBucket` on the sending thread. This is the
  critical throttle: upstream has no built-in rate limit, so during
  **phase 2** (where all 100 events are already staged in
  `kafka-upstream[transfer2]` and a fresh-group upstream drains them
  from offset 0 as fast as the fetch loop yields) it would otherwise
  dump 100 UDP packets into the socket at line rate and occasionally
  lose one. `eps=50` spaces them 20 ms apart, well inside the kernel
  UDP receive buffer's headroom. Without it, chain runs on busy
  Docker Desktop hosts intermittently reported `missing: 1` with a
  different missing event ID each time (surfaced by the runner's
  `missing-ids:` line, see "Automated chain runner" above).

**Chain run (Docker — two-phase, dedup end-to-end; systemd not exercised).**

Same two-phase shape as TC-9 but on the `transfer2 / dedup2 / gaps2`
family:

1. LG producer writes 100 counter events to `kafka-upstream[transfer2]`
   at `eps=100` (same scale as the TC-9 chain run — see the note on
   `upstream-lg-10-docker.properties` above for why).
2. upstream-10 (`groupID=test`, with the deliverFilter) sends ~80
   events through UDP → downstream → `kafka-downstream[transfer2]`.
3. The dedup app consumes `transfer2`, forwards to `dedup2` and
   records the ~20 gaps on `gaps2`.
4. When the LG producer exits, the runner reads
   `UPSTREAM_A_PHASE2_CONFIG=…/upstream-airgap-10b-docker.properties`
   and runs `docker compose up -d --force-recreate --no-deps upstream-a`
   (SIGTERM → fresh container with `groupID=test2`). Sarama starts
   phase 2 at offset 0, replays all 100 events; dedup discards the
   ~80 duplicates and emits only the previously-missing ~20.
5. LG sink reads `kafka-downstream[dedup2]` and sees 100 unique with
   0 duplicates.

Manifest knobs in [testcases/10.env](../env/testcases/10.env):

* `TC_PROFILES="dedup"`.
* `UPSTREAM_A_CONFIG`, `UPSTREAM_A_PHASE2_CONFIG`,
  `PHASE2_SETTLE_SECONDS=10`, `PHASE2_DRAIN_SECONDS=45`.
* `DEDUP_ENV_FILE=../../config/testcases/dedup-10.env`.
* `LG_PRODUCER_CONFIG=/airgap/config/testcases/upstream-lg-10-docker.properties`
  (`topic=transfer2`, `eps=100`, `limit=100`).
* `LG_SINK_CONFIG=/airgap/config/testcases/downstream-lg-10.properties`
  (`topic=dedup2`).
* `LG_STARTUP_DELAY=15` — critical. The two upstream `-docker.properties`
  configs apply an `eps=50` TokenBucket throttle on UDP output, but
  that alone isn't enough: a fresh dedup consumer group needs
  10–15 s to finish REBALANCING → RUNNING across all 15 downstream
  partitions, and without this delay LG can finish all 100 writes to
  `kafka-upstream[transfer2]` in ~1 s and race dedup's EOS-v2 state-
  store initialisation. The symptom was an intermittent 1-event loss
  with a different counter missing each run (TEST_18, 31, 34, 57,
  …). The delay lives in [lg-entrypoint.sh](../env/lg-entrypoint.sh);
  the lg-producer compose service also now has a
  `depends_on: dedup (required: false)` so the LG container doesn't
  even start until dedup's container is `Started`.

Expected chain verdict:

```text
✅ testcase 10 PASS
─── testcase 10 verdict ───
  sent      : 100
  received  : 100 unique
  duplicates: 0
  missing   : 0
  result    : PASS (100/100 received, 0 missing, 0 duplicate)
```

The systemd verification remains in the bare-metal procedure below —
nothing about `systemctl status`, memory bounds, or
`/var/log/airgap/*/stdout.log` is observable from the chain.

**Procedure (bare-metal / interactive).**

Steps 1–20 cover **downstream and upstream**, 21–34 cover **dedup**. See
`packaging/systemd/airgap-{upstream,downstream,dedup}@.service` for the
templates referenced below.

**Downstream half:**

1. Both Kafka clusters started.
1. Create upstream topic `transfer2` (5 partitions, replication 1).
1. Create downstream topic `gaps2` (5 partitions, replication 1).
1. On the downstream host, write
   `/etc/systemd/system/downstream.service` from the shipped template.
   Change the user from root; the service requires a dedicated `airgap`
   user.
1. `sudo mkdir /var/log/airgap/downstream` and set the right owner.
1. Copy the `downstream` binary into `/opt/airgap/downstream/bin/`.
1. Make sure `downstream.service` has
   `Environment="AIRGAP_DOWNSTREAM_INTERNAL_TOPIC=airgap-internal"` so status
   messages hit the right topic.
1. `sudo systemctl start downstream`.
1. `systemctl status downstream -l` — must show `Active: active (running)`.
   Troubleshoot via `/var/log/airgap/downstream/stdout.log`.
1. Verify with a Kafka console consumer against `airgap-internal` on the
    downstream Kafka that `Downstream_10 starting UDP …` appears.

**Upstream half:**

1. Write `/etc/systemd/system/upstream.service` from the template.
1. Create `/var/log/airgap/upstream`.
1. Copy the `upstream` binary into `/opt/airgap/upstream/bin/`.
1. Edit `upstream.service`:

    ```text
    Environment="AIRGAP_UPSTREAM_ID=Upstream_10"
    ...
    Environment="AIRGAP_UPSTREAM_DELIVER_FILTER=1,2,3,4,6,7,8,9,11,12,13,14"
    ```

1. `systemctl daemon-reload && sudo systemctl start upstream`.
1. `systemctl status upstream -l`.
1. Verify `Upstream_10 starting UDP …` on `airgap-internal`.

**Dedup instances:**

1. Write `/etc/systemd/system/dedup@.service` from the template.
1. Write two env files: `/opt/airgap/dedup/dedup-1.env` and `dedup-2.env`,
    with **distinct `STATE_DIR_CONFIG` paths and distinct `APPLICATION_ID`
    values** — otherwise Kafka reassigns previously-handled topics.
1. `sudo systemctl daemon-reload && sudo systemctl start dedup@1 dedup@2`.
1. `systemctl status dedup@1 dedup@2` and tail
    `/var/log/airgap/dedup/stdout-1.log` for exceptions.
1. `sudo systemctl enable dedup@1 dedup@2`.

**End-to-end:**

1. Start LogGenerator-cmd on `dedup2` (`downstream-lg-10.properties`).
1. Start a console consumer on `transfer2`.
1. Generate 100 logs with `upstream-lg-10.properties` (eps=100).
1. The downstream LogGenerator receives ~100 logs. If dedup state is
    stale, delete `dedup-gap-app-gap-tracker-store-changelog` and restart
    `dedup@2 dedup@1`.
1. Verify the `gaps2` topic contains gaps for all partitions:

    ```bash
    ./target/<arch>/gaps config/testcases/downstream-airgap-10.properties --topic gaps2
    ```

1. **Fill the gaps** by rotating the upstream `groupID` (edit
    `upstream.service` to `AIRGAP_UPSTREAM_GROUP_ID=test2`, comment out the
    `DELIVER_FILTER`, optionally rename
    `AIRGAP_UPSTREAM_ID=Upstream_10b`).
1. `systemctl daemon-reload && sudo systemctl restart upstream`.
1. Wait ≥ 2× `GAP_EMIT_INTERVAL_SEC`; use Jolokia for live-updating gap
    counts:

    ```bash
    curl http://localhost:8778/jolokia/read/nu.sitia.airgap:partition=3,type=GapDetectors/transfer2_3_gaps
    ```

    (second instance exposes port 8779).

1. Expected: `transfer2/* Total: 0 missing`.

1. Stop the LogGenerator-cmd.

**Expected result.**

```text
Duplicate detection found: 0 duplicate (or more) values.
Number of unique received numbers: 100
Next expected number: 101
```

**What it proves.**

All three services run under systemd, memory bounds apply, duplicates are
filtered and gaps are repaired dynamically during live operation.

---

## TC-11 — Redundancy (two chains, one dedup)

Verify that two upstream/downstream chains against the same upstream topic
produce a single combined topic downstream containing every event exactly
once.

* Covers: REQ-24
* Implemented in: 0.1.4-SNAPSHOT
* Runner: `./run-testcase.sh 11` (auto-adds profiles `dual` + `dedup`)

**Setup.**

* Both Kafka clusters up (one upstream, one downstream; TC-11 uses one
  cluster per side).
* `topic-init-downstream` creates `transfer-11a` and `transfer-11b` (15
  partitions, 12 h retention).
* `tests/env/testcases/11.env` activates `TC_PROFILES="dual dedup"`,
  selects both `upstream-airgap-11a.properties` and
  `upstream-airgap-11b.properties`, the
  `downstream-airgap-11a.properties` config (with `topicTranslations`
  mapping `transfer` → `transfer-11a`), and `dedup-11a.env`.

**Chain run (Docker — automated).**

LG producer sends 1000 counter events at `eps=200` to
`kafka-upstream[transfer]`. Both `upstream-a` and `upstream-b` run as
redundant chains (different `groupID` so each consumes every event),
and each sends UDP to the same `downstream` which writes to
`kafka-downstream[transfer-11a]`. The Kafka Streams dedup app
(`RAW_TOPICS=transfer-11a`) collapses the duplicates into the clean
`dedup` topic, and the LG sink
(`downstream-lg-11.properties`, reads `kafka-downstream[dedup]`) counts
exactly 1000 unique events with 0 duplicates.

Manifest knobs in `tests/env/testcases/11.env`:

* `TC_PROFILES="dual dedup"`.
* `UPSTREAM_A_CONFIG`, `UPSTREAM_B_CONFIG` → the two 11-side configs.
* `DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-airgap-11a.properties`.
* `DEDUP_ENV_FILE=../../config/testcases/dedup-11a.env`.
* `LG_PRODUCER_CONFIG=/airgap/config/testcases/upstream-lg-11.properties`
  (`limit=1000`, `eps=200`).
* `LG_SINK_CONFIG=/airgap/config/testcases/downstream-lg-11.properties`.

Expected chain verdict:

```text
✅ testcase 11 PASS
─── testcase 11 verdict ───
  sent      : 1000
  received  : 1000 unique
  duplicates: 0
  missing   : 0
  result    : PASS (1000/1000 received, 0 missing, 0 duplicate)
```

The chain variant doesn't exercise the "stop one chain mid-run" step
from the bare-metal procedure below — that step requires an interactive
stop/restart (worth keeping on-paper to validate the redundancy claim).
What the chain *does* prove is that two independent upstream consumers
feeding the same downstream+dedup don't produce any clean-topic
duplicates in steady state.

**Procedure (bare-metal / interactive).**

1. Both Kafka clusters started.
1. Clear the upstream `transfer` topic (or `docker compose down -v`).
1. (Already done by topic-init.) Create `transfer-11a` and `transfer-11b` on
   the downstream, 5 partitions each on bare metal — the Docker env creates
   them with 15.
1. Stop any downstream systemd service that would clash. In Docker,
   `./run-testcase.sh 11` only runs the compose stack.
1. **Start two downstream instances** manually (bare metal) — in Docker you
   get one `downstream` service; the two topic-translations are handled by
   the dedup via `RAW_TOPICS=transfer-11a,transfer-11b`.
1. Open a console-consumer on `airgap-internal` to see startup messages.
1. **Start the two upstreams** (`upstream-a` and `upstream-b`). Expected:
   `Upstream_11a starting up` and `Upstream_11b starting up` appear in the
   `airgap-internal` consumer.
1. **Start the dedup** with `dedup-11a.env` (and optionally `dedup-11b.env`
   on bare metal; in Docker a single dedup consumes
   `RAW_TOPICS=transfer-11a,transfer-11b`).
1. Start a LogGenerator-cmd on the downstream `dedup`
   (`downstream-lg-11.properties`).
1. **Generate 1000 logs**:

    ```bash
    java -jar LogGenerator-1.1-7.jar -pf upstream-lg-11.properties -l 1000 --eps 1000
    ```

1. Stop the LogGenerator-cmd (SIGINT or SIGTERM — the newer jar's shutdown
   hook catches both). Expected:

    ```text
    Duplicate detection found: 0 duplicate (or more) values.
    Number of unique received numbers: 1000
    Next expected number: 1001
    ```

1. Restart the LogGenerator-cmd for the next round.
1. **Stop one downstream chain** (one of the two in the original testfall;
    in Docker this means stopping `upstream-a` or `upstream-b`).
1. Generate another 1000 logs.
1. Stop the LogGenerator-cmd. Expected: same clean 1000, 0 duplicates.
1. Restart the LogGenerator-cmd.
1. **Stop the other chain** (restart the one you stopped before).
1. Generate another 1000 logs.
1. Stop the LogGenerator-cmd. Same clean result.

**What it proves.**

Logs can be sent over multiple redundant channels and still be delivered
exactly once, even when one of the redundant chains is temporarily down.

---

## TC-12 — Load sharing

Verify that two upstream/downstream chains that split a single topic
produce a single downstream topic containing every event.

* Covers: REQ-24
* Implemented in: 0.1.4-SNAPSHOT
* Runner: `./run-testcase.sh 12`

**Setup.**

Same as TC-11 but with `upstream-airgap-12a/b.properties` which split the
load so each chain handles ~half the events, and
`downstream-airgap-12a/b.properties` + `dedup-12a/b.env` which translate
and dedup both halves.

The chain run uses the dedicated `-docker.properties` variants with
the same filter split as the bare-metal procedure. The 50/50 split
delivers 100% only because of the `posMod` fix in
[src/filter/filter.go](../../src/filter/filter.go) — see "Chain run"
below for the backstory.

**Chain run (Docker — 50/50 load split, dedup confirms zero overlap).**

The committed `upstream-airgap-12a.properties` carries
`deliverFilter=1,3,5` and 12b has `=2,4,6`, intended to split Kafka
offsets so each chain delivers only half. Pre-posMod-fix, the Go
filter implementation computed `pos = ((0-1) % 2) + 1 = 0` for
offset 0 of every partition (Go's signed modulo returns `-1`), and
`0` isn't in either filter's group — so neither chain delivered
offset 0, costing one event per upstream Kafka partition (5/100 for
5 partitions). The fix is in
[`posMod`](../../src/filter/filter.go) which always returns a
non-negative result; see [filter_test.go](../../src/filter/filter_test.go)
for regression coverage. After the fix, offset 0 maps to `pos=2` and
is delivered by the even chain.

The chain variant uses the filter split:

* [upstream-airgap-12a-docker.properties](../../config/testcases/upstream-airgap-12a-docker.properties)
  carries `deliverFilter=1,3,5` → forwards offsets where `pos=1`
  (odd offsets 1,3,5,…; offset 0 goes to the other chain).
* [upstream-airgap-12b-docker.properties](../../config/testcases/upstream-airgap-12b-docker.properties)
  carries `deliverFilter=2,4,6` → forwards offsets where `pos=2`
  (even offsets 0,2,4,… including 0).
* Both carry the `eps=50` UDP TokenBucket throttle AND
  `logStatistics=5` so each app periodically emits a `STATISTICS: {…}`
  JSON line at INFO (consumed by `verify-tc12.sh` to confirm the
  per-chain forwarded count).
* Both target the same downstream (port 1234). The committed
  `upstream-airgap-12b.properties` aims at `targetPort=1235` for a
  *second* downstream instance — the Docker rig has only one.
* [downstream-airgap-12-docker.properties](../../config/testcases/downstream-airgap-12-docker.properties)
  binds UDP on `0.0.0.0`, translates `transfer` → `transfer-12a`.
* [dedup-12-docker.env](../../config/testcases/dedup-12-docker.env)
  consumes `RAW_TOPICS=transfer-12a` (just the one topic — the
  bare-metal procedure uses `transfer-12a,transfer-12b` because it
  runs two downstreams), has its own `STATE_DIR_CONFIG=/tmp/dedup_state_12/`.
* [upstream-lg-12-docker.properties](../../config/testcases/upstream-lg-12-docker.properties)
  writes 100 counter events to `kafka-upstream[transfer]` at
  `eps=100` (same scale as TC-9/TC-10 chain runs).

Expected flow: each chain reads all 100 Kafka records but forwards
only half (disjoint by `deliverFilter`) → 50 UDP packets from chain-a
+ 50 UDP packets from chain-b → 100 records on `transfer-12a` →
dedup emits 100 unique records to the `dedup` topic (zero duplicates
because the two halves don't overlap) → lg-sink reads 100 unique.

Manifest knobs in [testcases/12.env](../env/testcases/12.env):

* `TC_PROFILES="dual dedup"`.
* `UPSTREAM_A_CONFIG`, `UPSTREAM_B_CONFIG`, `DOWNSTREAM_CONFIG`,
  `DEDUP_ENV_FILE` → the `-docker` variants.
* `LG_PRODUCER_CONFIG`, `LG_SINK_CONFIG`, `EXPECTED_DUPLICATES=0`.
* `LG_STARTUP_DELAY=15` — same dedup REBALANCING grace period as
  TC-9/TC-10.
* `VERIFY_SCRIPT=verify-tc12.sh`, `TC12_LG_LIMIT=100`,
  `TC12_DOWNSTREAM_TOPIC=transfer-12a` — triggers the load-share
  verifier after the counter verdict.

Expected chain verdict:

```text
✅ testcase 12 PASS
─── testcase 12 verdict ───
  sent      : 100
  received  : 100 unique
  duplicates: 0
  missing   : 0
  result    : PASS (100/100 received, 0 missing, 0 duplicate)

─── verify-script: /.../tests/env/verify-tc12.sh ───
─── verify-tc12: 50/50 load share across both chains? ───
verify-tc12: ── per-chain forwarded count (STATISTICS JSON) ──
verify-tc12: upstream-a: total_sent=50, total_unfiltered=50 (ideal ≈50)
verify-tc12: upstream-b: total_sent=50, total_unfiltered=50 (ideal ≈50)
verify-tc12: ── kafka-downstream[transfer-12a] record count ──
verify-tc12: transfer-12a has 100 record(s) total
verify-tc12: transfer-12a: 100/100 (100% of LG_LIMIT) → within 50/50-split tolerance
verify-tc12: ─── PASS: both chains delivered a disjoint ~half of the 100 events to transfer-12a, dedup merged to 100 unique ───
─── verify-script: PASS ───
```

The counter verdict alone proves the dedup pipeline produced 100
unique records. The `verify-tc12.sh` block proves the load was
*actually split 50/50* and the two halves were disjoint:

* **Per-chain forwarded count** — each upstream emits a
  `STATISTICS: {…"total_sent":NN…}` JSON line at INFO every 5 s
  (enabled by `logStatistics=5` in the `-docker` upstream configs).
  The verifier reads the last such line from `/tmp/upstream.log`
  inside each container, extracts `total_sent`, and requires each
  chain to be inside `[0.4, 0.6] × LG_LIMIT` (40–60 for LG_LIMIT=100).
  This is the direct, app-reported measurement — no broker state
  guessing.
* **Downstream record count** — `kafka-downstream[transfer-12a]`
  must hold ≈ `LG_LIMIT` records (NOT 2×). Any value near 200 would
  mean a chain forwarded the full stream (filter misconfigured); any
  value near 50 would mean only one chain delivered. The verifier
  requires the count inside `[0.8, 1.2] × LG_LIMIT`.

What the chain-run demonstrates is "two upstreams, one dedup → each
event forwarded by exactly one chain, zero duplicates in the clean
topic". It does *not* reproduce the manual procedure's **chain-down**
step (stop one upstream, verify the remaining chain fills the gap
through dedup replay) — that still lives in the bare-metal procedure
below.

**Procedure (bare-metal / interactive).**

Steps 1–11 mirror TC-11 (set up clusters, verify topics, start the two
chains, dedup, LogGenerator-cmd on `dedup`, generate 100 logs at eps=1000).

Expected after step 12: 0 duplicates, 100 unique logs, next expected 101.

**Chain-down test (steps 13–19):**

1. Restart LogGenerator-cmd.
1. Stop one upstream (`Upstream_12a terminating by signal`).
1. Generate another 100 logs.
1. Stop the LogGenerator-cmd. Expected: a bunch of gaps (~50 missing),
    **0 duplicates**. Example:

    ```text
    Gaps found: 23.
    4-5
    8-9
    ...
    Duplicate detection found: 0 duplicate (or more) values.
    Number of unique received numbers: 50
    Next expected number: 99
    ```

1. Restart LogGenerator-cmd.
1. Restart the stopped upstream; logs start flowing to the downstream
    again.
1. Stop LogGenerator-cmd. Expected: fills the gaps from step 16 so the
    combined received count across step 16 + step 19 is 100.

20–26. Mirror of 13–19 with the **other** upstream stopped and restarted.

**What it proves.**

Load can be split between multiple diodes, deduped on arrival, and delivered
exactly once — and dropping any single chain only loses the events that
chain was handling.

---

## TC-13 — Dedup monitoring

Verify that dedup logs received, sent, and missing-event counters at a
configurable interval.

* Covers: REQ-27, REQ-28
* Implemented in: 0.1.5-SNAPSHOT
* Runner: `./run-testcase.sh 13`

**Setup.**

* Dedup running against `transfer` (via downstream translation to `dedup`).
* `dedup-13.env` sets a short `GAP_EMIT_INTERVAL_SEC`.

**Chain run (Docker).**

The chain variant produces exactly the pipeline the manual procedure
observes, and then asserts dedup's monitoring output machine-readably
instead of eyeballing the console.

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-13-docker.properties](../../config/testcases/upstream-airgap-13-docker.properties)
  — targets `kafka-downstream.sitia.nu:1234`, reads `transfer` from
  `kafka-upstream`, `groupID=13`, carries `deliverFilter=2,4,6` so only
  Kafka offsets where `pos=2` get forwarded (even offsets 0,2,4,… — the
  posMod-fix from TC-12 is what makes offset 0 land on this chain
  rather than disappearing). `eps=50` TokenBucket throttle on UDP out.
* [downstream-airgap-13-docker.properties](../../config/testcases/downstream-airgap-13-docker.properties)
  — binds UDP on `0.0.0.0`, no topic translation (`transfer` stays
  `transfer` on the downstream cluster).
* [dedup-13-docker.env](../../config/testcases/dedup-13-docker.env)
  — `RAW_TOPICS=transfer`, `CLEAN_TOPIC=dedup`, `GAP_TOPIC=gaps`,
  dedicated `STATE_DIR_CONFIG=/tmp/dedup_state_13/`, and critically
  **`GAP_EMIT_INTERVAL_SEC=5`** (vs. the committed 60 — a 60 s
  interval would never fire inside the ~30 s chain run).
* [upstream-lg-13-docker.properties](../../config/testcases/upstream-lg-13-docker.properties)
  — writes 100 counter events at `eps=100` to `kafka-upstream[transfer]`.
* [downstream-lg-13.properties](../../config/testcases/downstream-lg-13.properties)
  — reused as-is from the bare-metal procedure; already consumes
  `kafka-downstream.sitia.nu:9092[dedup]`.

Flow: LG writes 100 → upstream filter forwards 50 → downstream writes
50 to `kafka-downstream[transfer]` → dedup emits 50 to `dedup` and
`[MISSING-REPORT]` lines to the dedup container's stdout every 5 s →
LG sink reads `dedup`, sees 50 unique with 50 missing.

Manifest knobs in [testcases/13.env](../env/testcases/13.env):

* `TC_PROFILES="dedup"`.
* `UPSTREAM_A_CONFIG`, `DOWNSTREAM_CONFIG`, `DEDUP_ENV_FILE` → the
  `-docker` variants.
* `LG_PRODUCER_CONFIG`, `LG_SINK_CONFIG`, `LG_STARTUP_DELAY=15`
  (same dedup REBALANCING grace period as TC-9/10/12).
* `EXPECTED_MISSING=40-60` — the chain intentionally drops half the
  stream so dedup has gaps to report. Without this knob the runner
  would FAIL the verdict on the ~50 missing counters. The range form
  absorbs Kafka's uneven sticky-partitioner distribution — the exact
  observed missing count lands anywhere in a narrow window around 50.
  **This is the whole point of TC-13**: observing REQ-28 ("number of
  missing log entries must be logged") requires missing entries to
  exist.
* `EXPECTED_DUPLICATES=0`.
* `TC_DRAIN_SECONDS=20` — give the final 5 s MISSING-REPORT cycle a
  chance to be emitted before teardown.
* `VERIFY_SCRIPT=verify-tc13.sh`, `TC13_EXPECTED_INTERVAL=5`,
  `TC13_MIN_REPORTS=2`.

The [verify-tc13.sh](../env/verify-tc13.sh) hook greps the dedup
container's `docker logs` for `[MISSING-REPORT]` JSON lines and
asserts:

1. At least `TC13_MIN_REPORTS` lines appeared (periodic emit works,
   not just a one-shot).
2. Each line carries the REQ-27 counters (`total_received`,
   `delta_received`, `total_emitted`, `delta_emitted`) and the REQ-28
   counters (`total_missing`, `delta_missing`).
3. The last emit cycle shows all three totals (received / emitted /
   missing) are > 0 across the 15 partition lines — proves the
   counters tracked the real stream, not that they stayed at zero
   because nothing flowed through.
4. The `MISSING_REPORT_INTERVAL_SEC=N` the dedup app logged at
   startup matches `TC13_EXPECTED_INTERVAL` (REQ-27 / REQ-28
   "configurable interval" — the env var really does reach the app).

Expected chain verdict:

```text
✅ testcase 13 PASS
─── testcase 13 verdict ───
  sent      : 100
  received  : 50 unique
  duplicates: 0
  missing   : 50
  result    : PASS (50/100 received, 50 missing as expected, 0 duplicate)

─── verify-script: /.../tests/env/verify-tc13.sh ───
─── verify-tc13: dedup periodic MISSING-REPORT counters ───
verify-tc13: dedup emitted 42 [MISSING-REPORT] line(s) (min required: 2)
verify-tc13: all required counter fields present (total_received delta_received total_emitted delta_emitted total_missing delta_missing)
verify-tc13: last-cycle totals: received=50, emitted=50, missing=50
verify-tc13: dedup startup reported MISSING_REPORT_INTERVAL_SEC=5 (expected: 5)
verify-tc13: ─── PASS: dedup emitted 42 MISSING-REPORT lines at 5 s interval with non-zero received/emitted/missing counters ───
─── verify-script: PASS ───
```

**Procedure (bare-metal / interactive).**

1–7. Standard TC-9-style ramp-up: clear upstream topic, create downstream
`dedup`, start LogGenerator-cmd on `dedup`, start dedup
(`dedup-13.env`), start downstream (`downstream-airgap-13.properties`),
start upstream (`upstream-airgap-13.properties` with filter that drops
counters ending in 5 or 0).

1. Open JConsole against the dedup, verify no gaps initially.

1. Open the gap-topic reader:

   ```bash
   ./target/<arch>/gaps config/testcases/downstream-airgap-9.properties --topic gaps
   ```

1. **Check the dedup console** — a `MISSING-REPORT` line per partition at
    the configured interval:

    ```text
    [MISSING-REPORT] [{"report_time":1759323144995,"partition":2,"total_missing":0,
      "delta_missing":0,"total_received":0,"delta_received":0,
      "total_emitted":0,"delta_emitted":0}]
    ```

1. Generate 100 logs:

    ```bash
    java -jar LogGenerator-1.1-7.jar -pf upstream-lg-13.properties -l 100 --eps 100
    ```

1. The LogGenerator-cmd receives ~50 logs.

1. Wait ≤ `GAP_EMIT_INTERVAL_SEC`. Dedup emits new MISSING-REPORT lines:

    ```text
    [MISSING-REPORT] [{"report_time":...,"partition":3,"total_missing":13,
      "delta_missing":13,"total_received":14,"delta_received":14,
      "total_emitted":14,"delta_emitted":14}]
    ```

    Across partitions `total_received ~ 50`, `total_emitted ≥ total_received`
    (startup / key-exchange messages count on emitted, not received).

**What it proves.**

Received, sent and missing counters are logged by dedup at a configurable
interval, both as absolute totals and as deltas between intervals.

---

## TC-14 — Resend from exported gap file

Verify that gaps can be exported with the `create` tool and that `resend`
replays them through upstream to fill those gaps.

* Covers: REQ-3b, REQ-3d, REQ-40
* Implemented in: 0.1.5-SNAPSHOT
* Runner: `./run-testcase.sh 14` (adds `dedup`)

**Setup.**

Same as TC-13 but with `-14` property variants, including
`create-14.properties` and `resend-14.properties`.

**Chain run (Docker).**

The chain variant automates the manual procedure's create→resend steps
using a new runner hook — `POST_DRAIN_SCRIPT` — that runs AFTER the LG
producer finishes and the pipeline drains, but BEFORE the sink's final
summary is captured. That ordering matters: it means the sink's FINAL
verdict reflects the POST-resend (gap-filled) state, not the mid-run
gappy state.

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-14-docker.properties](../../config/testcases/upstream-airgap-14-docker.properties)
  — `deliverFilter=2,4,6` (same technique as TC-13) intentionally
  forwards only half the stream so dedup accumulates real gaps for
  `create` to export. `eps=50` UDP throttle, same as TC-9/10/12/13.
* [downstream-airgap-14-docker.properties](../../config/testcases/downstream-airgap-14-docker.properties)
  — no topic translation (mirrors TC-13's docker downstream).
* [dedup-14-docker.env](../../config/testcases/dedup-14-docker.env)
  — fixes two bugs in the committed `dedup-14.env` that only matter in
  Docker: `STATE_DIR_CONFIG` was a copy-paste leftover pointing at
  TC-13's state dir (fixed to `/tmp/dedup_state_14/`), and
  `GAP_EMIT_INTERVAL_SEC`/`MISSING_REPORT_INTERVAL_SEC` are dropped to
  5 s (from the committed 60 s) so `gaps` gets populated and
  `verify-tc14.sh` can observe multiple report cycles inside the
  chain's ~45 s runtime.
* [upstream-lg-14-docker.properties](../../config/testcases/upstream-lg-14-docker.properties)
  — `limit=100 eps=100` (TC-9/10/12/13 Docker scale) instead of the
  bare-metal procedure's `1000@1000`.
* `create-14.properties` / `resend-14.properties` / `downstream-lg-14.properties`
  are reused as-is — their `nic`/`targetIP`/`bootstrapServers` values
  are placeholders overridden by the `create`/`resend` compose
  services' `environment:` block regardless of what's in the file (see
  [docker-compose.yml](../env/docker-compose.yml)'s `create`/`resend`
  service definitions).

[tests/env/tc14-resend.sh](../env/tc14-resend.sh) is the
`POST_DRAIN_SCRIPT` body. It:

1. Runs `docker compose --profile create run --rm create create
   --resendFileName=/airgap/tmp/resend-14-all.json --limit=all` to
   export every current gap from `kafka-downstream[gaps]`.
   **Note the repeated `create`** after `--rm create` — `docker
   compose run SERVICE [COMMAND...]` replaces the compose-file
   `command:` entirely once any token follows SERVICE, and
   [entrypoint.sh](../env/entrypoint.sh)'s first argument must stay
   `create` (its role selector, used to pick the binary). A bare
   `-- --resendFileName=...` (as earlier drafts of this doc showed)
   makes `--` itself the override command, and the entrypoint fails
   looking for a `/airgap/bin/--` binary.
2. Confirms the bundle file is non-empty and has at least one
   partition's gap data (fails loudly if the deliverFilter didn't
   actually drop anything).
3. Runs `docker compose --profile resend run --rm resend resend
   --resendFileName=/airgap/tmp/resend-14-all.json --eps=50` to replay
   every gapped offset — reading the original payload from
   `kafka-upstream[transfer]` and re-sending it via UDP to the
   `downstream` container with the ORIGINAL `topic_partition_offset`
   id intact, so dedup recognizes it as a GAP_FILL, not a new event.
4. Sleeps `TC14_PROPAGATION_WAIT` (20 s) for the resent events to flow
   through downstream → kafka-downstream[transfer] → dedup → `dedup`
   topic → the (still-running) lg-sink.

This also required a real bug fix: [entrypoint.sh](../env/entrypoint.sh)
previously only forwarded `$1` (the role) to the binary and silently
dropped any further CLI args — `exec "${BIN}" "${CFG}"` with no `"$@"`.
That meant `--resendFileName=...`/`--limit=all` overrides could never
reach `create`/`resend` through Docker at all, even via the manual
procedure's documented commands. Fixed to `shift` past the role and
`exec "${BIN}" "${CFG}" "$@"`.

Manifest knobs in [testcases/14.env](../env/testcases/14.env):

* `TC_PROFILES="dedup"`.
* `UPSTREAM_A_CONFIG`, `DOWNSTREAM_CONFIG`, `DEDUP_ENV_FILE` → the
  `-docker` variants; `CREATE_CONFIG`, `RESEND_CONFIG` → reused
  bare-metal files.
* `LG_PRODUCER_CONFIG`, `LG_SINK_CONFIG`, `LG_STARTUP_DELAY=15`
  (dedup REBALANCING grace period, same as TC-9/10/12/13).
* `TC_DRAIN_SECONDS=15` — gives dedup 2-3 `GAP_EMIT_INTERVAL_SEC`
  cycles to populate `gaps` with the COMPLETE picture before `create`
  runs (not a partial mid-stream snapshot).
* `POST_DRAIN_SCRIPT=tc14-resend.sh`, `TC14_BUNDLE_FILE`,
  `TC14_PROPAGATION_WAIT=20`, `TC14_RESEND_EPS=50`.
* `EXPECTED_DUPLICATES=0`, `EXPECTED_MISSING=0-5` — see below for why
  a strict 0 isn't achievable.
* `VERIFY_SCRIPT=verify-tc14.sh`, `TC14_PARTITION_COUNT=15`.

**Why `EXPECTED_MISSING=0-5` and not `0`.** Windowed gap detection
needs a LATER same-parity offset to "close" a window and confirm a gap
exists. If `deliverFilter=2,4,6` drops the very LAST offset(s) of a
partition's finite stream — and nothing newer ever arrives, because
the LG burst has ended — there's no way for dedup (or `create`'s
export) to know that offset was ever "missing" vs. "not sent yet".
Those tail events are invisible to the create/resend mechanism itself,
not merely slow to resend. With 100 events round-robined across 5
partitions this costs at most a handful at the very end of the run;
observed 0-3 in practice across repeated runs. A real resend failure
looks nothing like this (missing stays ~45-50, matching the undelivered
half), so the narrow range doesn't mask an actual regression.

[verify-tc14.sh](../env/verify-tc14.sh) proves the gap-fill actually
happened — not just that the final numbers happen to look right:

1. At least one `[MISSING-REPORT]` line shows a **negative**
   `delta_missing` — direct evidence a gap closed during this run
   (the manual procedure's step 17 describes the same signal).
2. The LAST emit cycle (one line per partition) sums to
   `total_missing == 0` across every partition — dedup's own gap
   tracker agrees nothing it ever confirmed as missing is still open.

Expected chain verdict:

```text
✅ testcase 14 PASS
─── testcase 14 verdict ───
  sent      : 100
  received  : 97 unique
  duplicates: 0
  missing   : 3
  missing-ids: 87
  result    : PASS (97/100 received, 3 missing (0..5 as expected), 0 duplicate)

─── verify-script: verify-tc14.sh ───
─── verify-tc14: did create+resend actually close dedup's gaps? ───
verify-tc14: dedup emitted 210 [MISSING-REPORT] line(s) total
verify-tc14: observed 6 report(s) with negative delta_missing (gap closing)
verify-tc14: last-cycle totals: received=97, missing=0
verify-tc14: ─── PASS: dedup observed 6 gap-closing report(s); final total_missing=0 across all partitions ───
─── verify-script: PASS ───
```

**Procedure (bare-metal / interactive).**

Steps 1–7: standard ramp-up (see TC-9/TC-13).

1. Dedup is emitting `MISSING-REPORT` with zeros.
1. Generate 1000 logs:

   ```bash
   java -jar LogGenerator-1.1-7.jar -pf upstream-lg-14.properties -l 1000 --eps 1000
   ```

1. LogGenerator-cmd receives ~500 logs.
1. Wait and verify MISSING-REPORT shows totals around 500 received, ~500
    missing.
1. The gap-topic reader output looks like:

    ```text
    transfer/0 [0..99]: 48 missing  first: 3, 5, 7, 9, 11, ...
    transfer/0 [100..199]: 42 missing  first: 101, 103, 105, 107, 109, ...
    ...
    ```

1. **Create a resend bundle from the oldest gaps:**

    ```bash
    docker compose --profile create run --rm create create \
      --resendFileName=/airgap/tmp/resend-first.json
    ```

    (The trailing `create` after `--rm create` repeats the service name
    as the container's override command — `docker compose run SERVICE
    [COMMAND...]` replaces the compose-file `command:` entirely once any
    token follows SERVICE, and the entrypoint's first argument must stay
    `create`, its role selector. A bare `-- --resendFileName=...` makes
    `--` itself the override command and the entrypoint fails looking
    for a `/airgap/bin/--` binary.)

    File contents:

    ```json
    {
      "type": "first",
      "results": [
        {"gaps":[[3]],"partition":1,"topic":"transfer","window_max":999,"window_min":0},
        ...
      ]
    }
    ```

1. **Create a resend bundle covering every gap** (`--limit=all`):

    ```bash
    docker compose --profile create run --rm create create \
      --resendFileName=/airgap/tmp/resend-all.json --limit=all
    ```

1. **Replay** on the upstream side:

    ```bash
    docker compose --profile resend run --rm resend resend \
      --resendFileName=/airgap/tmp/resend-all.json
    ```

    ~500 events are resent:

    ```text
    [INFO] STATISTICS: {"id":"Resend","interval":0,"received":510,"sent":45,...}
    [INFO] Resend process finished. Exiting...
    ```

1. In the dedup console, MISSING-REPORT now shows **negative**
    `delta_missing` as the gaps close:

    ```text
    [MISSING-REPORT] [{"partition":0,"total_missing":0,"delta_missing":-8,
      "total_received":17,"delta_received":8,...}]
    ```

1. Gap reader: `transfer/* Total: 0 missing`.

18–23. **Repeat with time windows.** Generate another 1000 logs, run
`create` again, then test the `--from`/`--to` filters:

* `--from=2030-10-01T10:10:10.000Z` → no events sent (future).
* `--from=2020-10-01T10:10:10.000Z` → every gap fills.
* `--to=2020-10-01T10:10:10.000Z` → nothing (before everything).
* `--to=2030-10-01T10:10:10.000Z` → fills everything.

If gaps linger, retry with `--eps=500`.

**What it proves.**

Gaps can be exported from downstream, imported in upstream for retransmission,
and filled using either a flat bundle or time-windowed selection via `--from`
and `--to`.

---

## TC-15 — Payload compression

Verify that payload between upstream and downstream can be compressed when
longer than a configurable threshold, including over an encrypted channel.

* Covers: REQ-30
* Implemented in: 0.1.5-SNAPSHOT
* Runner: `./run-testcase.sh 15`

**Setup.**

Dedup is not needed; the test focuses on wire format.

* `upstream-airgap-15.properties` sets `compressWhenLengthExceeds=100`.
* `upstream-airgap-15b.properties` additionally sets `encryption=true`.

**Chain run (Docker — real tcpdump capture, automated).**

Unlike the dedup-focused chain tests, TC-15 is fundamentally about the
WIRE FORMAT, not event counters — so the chain variant does a REAL
`tcpdump` capture on the `downstream` container and asserts on its
content, the same way the manual procedure does by eye.

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-15-docker.properties](../../config/testcases/upstream-airgap-15-docker.properties)
  — `compressWhenLengthExceeds=100`, no encryption.
* [upstream-airgap-15b-docker.properties](../../config/testcases/upstream-airgap-15b-docker.properties)
  — same, plus `encryption=true` + `publicKeyFile=certs/server2.pem`
  (mirrors TC-6's encryption pattern).
* [downstream-airgap-15-docker.properties](../../config/testcases/downstream-airgap-15-docker.properties)
  — binds `0.0.0.0:1234`, no topic translation, `privateKeyFiles=certs/private*.pem`
  so it can decrypt the symmetric-key exchange in the encrypted sub-phase.
* `upstream-lg-15.properties` / `upstream-lg-15b.properties` are reused
  as-is (short vs. long message content) — they only talk to Kafka, so
  no Docker-specific networking fields need overriding.

[tests/env/Dockerfile](../env/Dockerfile) now installs `tcpdump` (added
alongside the existing `iproute`/`iputils`/`procps-ng` diagnostics) so
[tc15-compression-test.sh](../env/tc15-compression-test.sh) can
`docker exec` into the `downstream` container and capture its own wire
traffic directly — no extra sniffer container or `network_mode` tricks
needed, since the capture happens on the exact interface that already
terminates the UDP socket.

`tc15-compression-test.sh` is the `POST_DRAIN_SCRIPT` body. The
manifest's `LG_PRODUCER_CONFIG` (a short-message burst) is only a quick
auto-exit ANCHOR so `run-testcase.sh` has something to wait on — it is
**not** what gets captured or asserted on. The real test runs entirely
inside the hook, as three self-contained sub-phases, each: start a
`tcpdump` capture on `downstream`, run a *fresh* one-shot `lg-producer`
burst with known content, stop the capture, then check both (a) what
is/isn't visible on the wire and (b) what `kafka-downstream[transfer]`
ends up holding:

| Phase | Message length | Encryption | Expected wire content | Expected Kafka content |
| --- | --- | --- | --- | --- |
| A | short (<100 B) | no | **plaintext visible** (under threshold, not compressed) | plaintext |
| B | long (>100 B) | no | plaintext **absent** (gzip-compressed) | plaintext (decompressed) |
| C | long (>100 B) | yes | plaintext **absent** (ciphertext) | plaintext (decrypted + decompressed) |

Phase A and B use the SAME long-running `upstream-a` (no restart needed
— only the LG producer's message content changes). Phase C swaps
`upstream-a` to the encrypted config via `docker compose up -d
--force-recreate --no-deps upstream-a` (same technique as TC-10's
phase-2 swap, done manually here since this test needs it mid-script
rather than as the one post-producer swap `UPSTREAM_A_PHASE2_CONFIG`
supports), then waits `TC15_ENCRYPTION_SETTLE_SECONDS` (8s) for the
symmetric-key exchange to complete before capturing.

The wire-content check greps the capture for a substring that's unique
to ONLY one of the two LG message templates (`upstream-lg-15.properties`
contains "should not be longer"; `upstream-lg-15b.properties` contains
"To get the message longer" — picked so phase B/C's long-message grep
can't accidentally match on a leftover short-message string from an
earlier phase). The Kafka-content check reads
`kafka-downstream[transfer]` from the beginning each time (the topic
accumulates across phases, which is fine — each check just needs to
find ITS OWN phase's marker somewhere in the full history).

Manifest knobs in [testcases/15.env](../env/testcases/15.env):

* `TC_PROFILES=""` — no dedup, no dual upstream.
* `UPSTREAM_A_CONFIG`, `DOWNSTREAM_CONFIG` → the `-docker` variants
  (unencrypted to start; phase C swaps mid-script).
* `LG_PRODUCER_CONFIG` → the anchor burst (reused bare-metal file).
* `TC_DRAIN_SECONDS=2` — short, since there's no dedup REBALANCING race
  to wait out here.
* `POST_DRAIN_SCRIPT=tc15-compression-test.sh`, plus
  `TC15_SHORT_LG_CONFIG`, `TC15_LONG_LG_CONFIG`,
  `TC15_ENCRYPTED_UPSTREAM_CONFIG`, `TC15_CAPTURE_SECONDS=3`,
  `TC15_ENCRYPTION_SETTLE_SECONDS=8`.

**A quirk worth knowing:** because no `LG_SINK_CONFIG` is set (Kafka
content is checked directly with `kafka-console-consumer` instead),
`run-testcase.sh`'s generic sink-log parser falls back to treating the
LG **producer's own** "Number of unique received numbers" line (its
internal self-duplicate-check, not a real receive count) as the sink
summary. That prints a `testcase 15 verdict: PASS (100/100 received, 0
missing, 0 duplicate)` block that looks like a real counter check but
is actually vacuous (the anchor burst compared against itself). The
REAL pass/fail signal is the `tc15-compression-test.sh` output and its
exit code — look for the `tc15: ─── PASS ───` / `FAIL` line, not the
generic verdict block above it.

Expected chain output (abbreviated):

```text
─── post-drain hook: tc15-compression-test.sh ───

tc15: ═══ Phase A: short messages (<100 bytes), no encryption — expect PLAINTEXT on the wire ═══
tc15: phaseA wire state: PRESENT (expected PRESENT — short payloads stay under compressWhenLengthExceeds=100, so they're sent as plain text)
tc15: phaseA: kafka-downstream[transfer] contains the expected plaintext ✓

tc15: ═══ Phase B: long messages (>100 bytes), no encryption — expect NO plaintext on the wire (compressed) ═══
tc15: phaseB wire state: ABSENT (expected ABSENT — payloads over compressWhenLengthExceeds=100 are gzip-compressed before sending)
tc15: phaseB: kafka-downstream[transfer] contains the expected plaintext ✓

tc15: ═══ Phase C: long messages (>100 bytes), WITH encryption — expect NO plaintext on the wire (ciphertext) ═══
tc15: swapping upstream-a to /airgap/config/testcases/upstream-airgap-15b-docker.properties
tc15: settling 8s for the symmetric-key exchange to complete
tc15: phaseC wire state: ABSENT (expected ABSENT — ciphertext, plus still compressed underneath)
tc15: phaseC: kafka-downstream[transfer] contains the expected plaintext ✓

tc15: ─── PASS: compression respects the length threshold, and downstream round-trips plaintext correctly with and without encryption ───

✅ testcase 15 PASS
```

**Procedure (bare-metal / interactive).**

1. Start LogGenerator-cmd on the downstream `transfer`
   (`downstream-lg-15.properties`): `Subscribed to topic transfer`.
1. Start air-gap downstream (`downstream-airgap-15.properties`).
1. Start air-gap upstream (`upstream-airgap-15.properties`).
1. Start tcpdump: `tcpdump -Ai any port 1234`.
1. Start a Kafka console consumer on the downstream `transfer`.
1. **Short logs (<100 bytes):** generate with `upstream-lg-15.properties`.
   tcpdump shows plain text on the wire; console-consumer shows the
   messages.
1. **Long logs (>100 bytes):** generate with `upstream-lg-15b.properties`.
   tcpdump output must **not** show the plain text (compressed on the
   wire). The console-consumer still shows the plain-text messages
   (decompressed by downstream before publishing).
1. **Encrypted + compressed:** stop upstream and start it again with
   `upstream-airgap-15b.properties`.
1. Repeat the long-log generation. tcpdump shows ciphertext; console-consumer
   still decodes to plain text:

   ```text
   This is the test message and it should be longer than 100 bytes.
   To get the message longer than 100 bytes, we add more text. TEST_1
   ...
   ```

**What it proves.**

Compression is applied to payloads above the configured length regardless of
whether the channel is encrypted. The receiver decompresses before writing to
Kafka.

---

## TC-16 — Performance / throughput

Measure sustained EPS and loss on the chosen host.

* Covers: REQ-11
* Runner: `./run-testcase.sh 16`

**Not chain-safe — intentionally manual-only.** Unlike TC-1 through
TC-15, TC-16 is a performance *benchmark*, not a correctness check.
There is no automated chain variant, and this is a deliberate choice,
not pending work:

* REQ-11 asks to "measure sustained EPS and loss" — there's no single
  correct EPS value to assert pass/fail against; throughput is
  host-dependent by design. Any hard threshold the chain runner
  enforced would be arbitrary and likely to fail on slower CI/dev
  hosts for reasons that have nothing to do with a real regression.
* Docker Desktop (macOS/Windows) runs containers inside a virtualized
  VM with its own virtualized networking layer. UDP throughput numbers
  measured there are **not comparable** to the bare-metal numbers this
  test is meant to produce (the manual procedure's own baseline notes
  "~900 EPS" on an OSX desktop vs. "~375,000" on a Fedora i7 — already
  a 400x spread between two bare-metal hosts; a VM-in-a-VM Docker
  Desktop setup adds yet another, differently-shaped bottleneck).
* The documented procedure pre-fills Kafka with 1,000,000 events and
  runs an open-ended baseline (no Kafka, `NullAdapter`) plus a
  full-chain run — both are open-ended stress tests meant to be
  watched and tuned interactively (adjusting EPS, UDP buffer sizes,
  `numReceivers`, etc.), not a fixed-duration pass/fail step suitable
  for an unattended chain run.

If you need a quick automated smoke test that the pipeline doesn't
fall over under *some* load (distinct from measuring a meaningful
number), that would be a different, new testcase — not a replacement
for this benchmark. TC-16 stays on `./run-testcase.sh 16` for
interactive use; the chain runner correctly reports it as
chain-unsafe/SKIP and that's by design, not a gap to fill.

**Setup.**

* Dedicated performance configs: `config/upstream-perf.properties` and
  `config/downstream-perf.properties`.
* Ideally run with sufficient hardware for upstream, downstream, dedup and
  Kafka.

**Procedure.**

**Baseline (loopback only, no Kafka):**

1. Start downstream `target/downstream config/downstream-perf.properties`.

   ```text
   [INFO] UDP listener starting on 0.0.0.0:1234 with 10 workers (MTU=1500)
   ```

1. Start upstream `target/upstream config/upstream-perf.properties`.

1. After ~10 s, stop downstream then upstream with `Ctrl-C`. Expected
   output:

   ```text
   [INFO] NullAdapter flush: 65311 messages total
   [INFO] Processed 65311 messages in 1m9.003s (946.49 EPS)
   ```

   On an OSX desktop ~900 EPS; Fedora i7 consumer ~375 000; server
   hardware higher.

**Full chain:**

1. Make sure dedup, upstream, downstream are stopped. Reset Kafka from a
   known-good snapshot or `docker compose down -v`.
1. Pre-fill the upstream Kafka with ~1 M logs:

   ```bash
   java -jar LogGenerator-1.1-7.jar -pf upstream-lg-16.properties -l 1000000
   ```

1. Start a LogGenerator-cmd on the downstream `transfer`
   (`downstream-lg-16.properties`).
1. Start air-gap downstream (`downstream-airgap-16.properties`).
1. Start air-gap upstream (`upstream-airgap-16.properties`, EPS limited to
   50 000). Adjust EPS to taste.
1. Stop the LogGenerator-cmd. Expected:

   ```text
   Number of unique received numbers: 1000000
   Next expected number: 1000001
   ```

1. Start a LogGenerator-cmd on the gap topic
    (`downstream-lg-16b.properties`).
1. Launch gap reader → `Inga gap ska visas` (no gaps).
1. Isolate dedup to measure its own throughput:

    ```bash
    /mnt/hgfs/air-gap/config/testcases/launchDedup.sh start 5
    ```

    (count ≥ partitions but ≤ available cores). Dedup periodically prints
    gap counts; verify none or very few.

1. Stop dedup instances:

    ```bash
    launchDedup.sh stop
    ```

1. Stop LogGenerator-cmd.

**What it proves.**

Baseline EPS, chain EPS, and dedup EPS are all measurable. Adjust UDP
parameters on downstream and EPS on upstream and repeat with a fresh
`groupID` to iterate.

---

## TC-17 — Dedup TLS to Kafka

Verify that dedup can connect to Kafka with TLS and (optionally) a client
certificate.

* Covers: REQ-31, Issue #3
* Implemented in: 0.1.6-SNAPSHOT
* Runner: `./run-testcase.sh 17`

**Setup.**

* SSL listener on the Kafka cluster(s) with keystore/truststore (handled by
  `./generate-kafka-certs.sh`).
* `dedup-17.env` adds the TLS env vars for Kafka (see
  `doc/Kafka-encryption.md` for the full list).

**Chain run (Docker).**

The whole chain (upstream, downstream, AND dedup) runs over the SSL
listener (9094/8094) so dedup is reading from a genuinely TLS-populated
topic, not a PLAINTEXT one — mirroring the bare-metal procedure's full
setup rather than testing dedup's TLS in isolation. No `deliverFilter`
— this is a full-delivery test (100/100), unlike TC-13's deliberate
gaps; the point here is TLS connectivity, not gap detection.

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-17-docker.properties](../../config/testcases/upstream-airgap-17-docker.properties)
  — TLS to `kafka-upstream.sitia.nu:9094,:8094`, no UDP encryption
  (`encryption=false` — that's TC-6's concern, not this one).
* [downstream-airgap-17-docker.properties](../../config/testcases/downstream-airgap-17-docker.properties)
  — TLS to `kafka-downstream.sitia.nu:9094,:8094`, no topic translation.
* [dedup-17-docker.env](../../config/testcases/dedup-17-docker.env)
  — `KAFKA_SECURITY_PROTOCOL=SSL` + `KAFKA_SSL_TRUSTSTORE_LOCATION=
  /airgap/certs/kafka/ssl/kafka-trust.jks` (PKCS12, password `changeit`).
  No client keystore — the Docker brokers run
  `KAFKA_SSL_CLIENT_AUTH=none` (server-auth-only TLS, no mTLS), unlike
  the committed bare-metal `dedup-17.env` which assumes mTLS with a
  client keystore.
* `upstream-lg-17.properties` / `downstream-lg-17.properties` are
  reused as-is — LogGenerator talks PLAINTEXT directly to Kafka in
  both bare-metal and Docker; TLS is specifically the AIR-GAP
  binaries' and dedup's concern, not LogGenerator's.

**A genuinely confusing pitfall this test exposed: the test env's two
Kafka clusters are signed by DIFFERENT CAs.** `kafka-upstream`'s broker
cert is signed by `airgap-testenv-ca` (regenerated more recently, at
`certs/kafka/ssl/testenv-ca.crt`); `kafka-downstream`'s broker cert is
still signed by the older `MyKafkaCA` (included in the generated
`certs/tmp/kafka-ca.crt` CA bundle, alongside `airgap-testenv-ca`).
This is a pre-existing property of the repo's cert generation history,
not something TC-17 introduced — TC-6's existing downstream config
already works around it by using a different `caFile` than its
upstream config, which is easy to miss if you're skimming for "the
TLS pattern" and copy the wrong one (an early draft of this chain
variant did exactly that, and the symptom was a confusing `tls: failed
to verify certificate: x509: certificate signed by unknown authority`
only on the downstream side). dedup's truststore
(`kafka-trust.jks`) and the generated PEM bundle (`certs/tmp/kafka-ca.crt`)
sidestep this by bundling BOTH CAs.

Two other environment-specific overrides are required because the
compose services' `environment:` blocks always win over manifest
settings for these exact keys:

* `UPSTREAM_A_BOOTSTRAP` / `DOWNSTREAM_BOOTSTRAP` (same pattern TC-6
  uses) — without these, `upstream-a`/`downstream` silently connect to
  the PLAINTEXT ports (9092/8092) while still configured for TLS,
  producing an immediate `EOF` on every metadata fetch instead of a
  clear TLS error.
* `DEDUP_BOOTSTRAP` — without this, dedup connects PLAINTEXT on
  9092/8092 instead of the SSL listener, silently defeating the whole
  point of the test (the topics exist on both listeners of the same
  broker, so nothing else would look wrong).

Manifest knobs in [testcases/17.env](../env/testcases/17.env):

* `TC_PROFILES="dedup"`.
* `UPSTREAM_A_CONFIG`, `DOWNSTREAM_CONFIG`, `DEDUP_ENV_FILE` → the
  `-docker` variants.
* `UPSTREAM_A_BOOTSTRAP=kafka-upstream.sitia.nu:9094,kafka-upstream.sitia.nu:8094`,
  `DOWNSTREAM_BOOTSTRAP=kafka-downstream.sitia.nu:9094,kafka-downstream.sitia.nu:8094`,
  `DEDUP_BOOTSTRAP=kafka-downstream.sitia.nu:9094,kafka-downstream.sitia.nu:8094`.
* `LG_PRODUCER_CONFIG`, `LG_SINK_CONFIG` (reused bare-metal files),
  `LG_STARTUP_DELAY=15` (dedup REBALANCING grace period, same as
  TC-9/10/12/13/14, plus a little extra for the TLS handshake).
* `EXPECTED_DUPLICATES=0`, `EXPECTED_MISSING=0` — full delivery expected.
* `VERIFY_SCRIPT=verify-tc17.sh`.

[verify-tc17.sh](../env/verify-tc17.sh) proves dedup actually used TLS
— not that the test happened to pass via a silent PLAINTEXT fallback:

1. dedup's own startup log shows `BOOTSTRAP_SERVERS` pointing at the
   SSL-labeled `kafka-downstream.sitia.nu:9094,:8094` (proving
   `DEDUP_BOOTSTRAP` actually reached the container).
2. No SSL/TLS handshake or authentication failure signatures
   (`SSLHandshakeException`, `SslAuthenticationException`, etc.)
   anywhere in the log.
3. dedup's own `[MISSING-REPORT]` counters show real traffic
   (`total_received > 0`, `total_emitted > 0`) — if the handshake had
   failed outright, Kafka Streams would be stuck retrying forever and
   nothing would ever reach the `dedup` topic.

Expected chain verdict:

```text
✅ testcase 17 PASS
─── testcase 17 verdict ───
  sent      : 100
  received  : 100 unique
  missing   : 0
  result    : PASS (100/100 received, 0 missing, 0 duplicate)

─── verify-script: verify-tc17.sh ───
─── verify-tc17: did dedup actually use TLS to reach Kafka? ───
verify-tc17: dedup reported: ... BOOTSTRAP_SERVERS=kafka-downstream.sitia.nu:9094,kafka-downstream.sitia.nu:8094
verify-tc17: SSL/TLS error signatures found: 0
verify-tc17: last-cycle totals over TLS: received=100, emitted=100
verify-tc17: ─── PASS: dedup connected via SSL (...), no TLS errors, received=100/emitted=100 over the connection ───
─── verify-script: PASS ───
```

**Procedure (bare-metal / interactive).**

Mirror of TC-13 but with `-17` property variants. The only extra concern is
reaching Kafka over the SSL listener:

1. Confirm reachability of 9094:

   ```bash
   nc -vz kafka-upstream.sitia.nu 9094
   ```

1. Clear the upstream `transfer` topic.
1. Create the `dedup` topic on the downstream.
1. Start LogGenerator-cmd (`downstream-lg-17.properties`).
1. Start dedup with `dedup-17.env`.
1. Start air-gap downstream (`downstream-airgap-17.properties`).
1. Start air-gap upstream (`upstream-airgap-17.properties`).
1. Generate 100 logs (`upstream-lg-17.properties`, `-l 100 --eps 100`).
1. The downstream LogGenerator receives 100 logs.

**What it proves.**

Dedup authenticates to Kafka over TLS using its configured certificates and
functions identically to the plain-text case.

---

## TC-18 — Encrypted key files

Verify that upstream, downstream, `create` and `resend` can all use
passphrase-encrypted private key files to talk to a Kafka cluster over TLS.

* Covers: REQ-32
* Implemented in: 0.1.7-SNAPSHOT
* Runner: `./run-testcase.sh 18` (adds `create`, `resend`)

**Setup.**

Kafka with mTLS enabled, using **encrypted** PKCS#12 key files (`.enc` in
`doc/Kafka-encryption.md`). The shipped `consumer.mtls2.properties`
illustrates:

```properties
security.protocol=SSL
ssl.keystore.type=PKCS12
ssl.keystore.location=/opt/kafka/config/tmp/airgap-upstream.p12
ssl.keystore.password=changeit
ssl.truststore.location=/opt/kafka/config/tmp/airgap-upstream.truststore.jks
ssl.truststore.password=changeit
```

**Chain run (Docker).**

The main upstream/downstream pipeline proves the encrypted-key path for
the air-gap binaries themselves (full delivery, 100/100, no filter —
same pattern as TC-17). `create`/`resend`'s positive AND negative cases
then run via `POST_DRAIN_SCRIPT`, since they need **four** separate
one-shot invocations with different config/bootstrap overrides — more
than the single-value `CREATE_CONFIG`/`RESEND_CONFIG` manifest vars can
express.

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-18-docker.properties](../../config/testcases/upstream-airgap-18-docker.properties),
  [downstream-airgap-18-docker.properties](../../config/testcases/downstream-airgap-18-docker.properties)
  — `keyFile=....key.enc` + `keyPasswordFile=....pw`, with the same
  asymmetric-CA fix TC-17 needed (kafka-upstream signed by
  `airgap-testenv-ca`, kafka-downstream by the older `MyKafkaCA` — the
  committed bare-metal `upstream-airgap-18.properties` uses the wrong
  one for this Docker rig).
* [upstream-lg-18-docker.properties](../../config/testcases/upstream-lg-18-docker.properties),
  [downstream-lg-18-docker.properties](../../config/testcases/downstream-lg-18-docker.properties)
  — plain full-delivery LG burst (no dedup in this test), PLAINTEXT to
  Kafka (TLS is the air-gap binaries' and create/resend's concern, not
  LogGenerator's).
* `create-18.properties` / `create-18b.properties` are reused as-is —
  their `caFile` already correctly targets kafka-downstream's CA
  (`certs/tmp/kafka-ca.crt`), matching what `create` connects to.
* [resend-18-docker.properties](../../config/testcases/resend-18-docker.properties)
  (correct password) / [resend-18b-docker.properties](../../config/testcases/resend-18b-docker.properties)
  (password commented out) — new variants fixing the committed
  `resend-18.properties`'s CA: resend reads from kafka-**upstream**
  (the `airgap-testenv-ca` cluster), so it needs the OTHER CA than
  `create` does.

[tests/env/tc18-mtls-test.sh](../env/tc18-mtls-test.sh) is the
`POST_DRAIN_SCRIPT` body. It runs four one-shot checks, each via
`docker compose --profile <svc> run --rm <svc> <svc> --resendFileName=...
[--limit=all]` with explicit per-invocation `CONFIG`/`BOOTSTRAP` env
var overrides (the repeated `<svc>` after `--rm <svc>` is required —
see TC-14's writeup for why a bare `-- --flag=value` fails against
this entrypoint):

| # | Check | Config | Expected exit |
| --- | --- | --- | --- |
| 1 | create, correct password | `create-18.properties` | 0 |
| 2 | resend, correct password | `resend-18-docker.properties` | 0 |
| 3 | create, password file removed | `create-18b.properties` | non-zero |
| 4 | resend, password file removed | `resend-18b-docker.properties` | non-zero |

The negative cases (3, 4) rely on `src/kafka/getkafka.go`'s
`createTLSConfig()` detecting the encrypted PEM header
(`Proc-Type: 4,ENCRYPTED`) and failing fast via `Logger.Panicf` when no
`keyPasswordFile` is configured — a synchronous, immediate non-zero
exit, not a hang or a silent fallback to an unencrypted/failed
connection.

Manifest knobs in [testcases/18.env](../env/testcases/18.env):

* `TC_PROFILES=""` — `create`/`resend` are invoked entirely from
  inside the hook, not as standing profiles on the main stack.
* `UPSTREAM_A_CONFIG`, `DOWNSTREAM_CONFIG` → the `-docker` variants;
  `UPSTREAM_A_BOOTSTRAP`/`DOWNSTREAM_BOOTSTRAP` → the SSL ports (same
  "environment: always wins" gotcha as TC-17).
* `LG_PRODUCER_CONFIG`, `LG_SINK_CONFIG`, `EXPECTED_DUPLICATES=0`,
  `EXPECTED_MISSING=0`.
* `LG_STARTUP_DELAY=10` — unlike TC-17 (plain TLS), upstream/downstream
  here ALSO have to read+decrypt a passphrase-protected key (PEM parse
  + legacy OpenSSL decrypt) before the TLS handshake even begins —
  extra startup latency on top of TC-17's. Without this delay, LG
  could start producing before upstream's consumer had fully
  subscribed, losing the first event or two (observed:
  `missing-ids: 1` on the very first counter). Same root-cause pattern
  as TC-9/10's dedup REBALANCING race — a grace period, not a
  missing-count tolerance.
* `POST_DRAIN_SCRIPT=tc18-mtls-test.sh`.

Expected chain output (abbreviated):

```text
─── post-drain hook: tc18-mtls-test.sh ───

tc18: ═══ Positive case: create with the CORRECT password ═══
tc18: create (correct password): exit=0 (expected zero)
tc18: create (correct password): PASS (exited 0 as expected)

tc18: ═══ Positive case: resend with the CORRECT password ═══
tc18: resend (correct password): exit=0 (expected zero)
tc18: resend (correct password): PASS (exited 0 as expected)

tc18: ═══ Negative case: create with the password file REMOVED ═══
[PANIC] Failed to configure TLS: key file '....key.enc' is encrypted but no keyPasswordFile is configured.
tc18: create (missing password): PASS (exited non-zero as expected — encrypted key correctly rejected without a password)

tc18: ═══ Negative case: resend with the password file REMOVED ═══
[PANIC] Failed to configure TLS: key file '....key.enc' is encrypted but no keyPasswordFile is configured.
tc18: resend (missing password): PASS (exited non-zero as expected — encrypted key correctly rejected without a password)

tc18: ─── PASS ───

✅ testcase 18 PASS
─── testcase 18 verdict ───
  sent      : 100
  received  : 100 unique
  missing   : 0
  result    : PASS (100/100 received, 0 missing, 0 duplicate)
```

**Procedure (bare-metal / interactive).**
1. Topic `transfer` exists on both clusters (5 partitions each). List with
   SSL to confirm.
1. Start a Kafka console consumer on the downstream `transfer`, using
   `consumer.mtls2.properties`.
1. Start a Kafka console producer on the upstream `transfer` using the
   same file.
1. Start air-gap downstream (`downstream-airgap-18.properties`). Takes
   ~30 s before the startup message hits Kafka because of the passphrase
   unlock.
1. Start air-gap upstream (`upstream-airgap-18.properties`).
1. Switch to the producer; type some events + Enter. The console-consumer
   on the downstream should echo them.
1. **Export every gap:**

   ```bash
   docker compose --profile create run --rm create -- \
     config/testcases/create-18.properties \
     --resendFileName=./tmp/resend-all.json --limit=all
   ```

   File looks like `{"type":"all","results":[]}` (no gaps yet).

1. Try to use `resend-18.properties` with the encrypted key:

   ```bash
   docker compose --profile resend run --rm resend -- \
     config/testcases/resend-18.properties \
     --resendFileName=./tmp/resend-all.json
   ```

   Operation succeeds.

1. Repeat with `create-18b.properties` which references a **wrong** key
   password → operation must **fail**.

1. Repeat with `resend-18b.properties` (wrong password) → fail.

**What it proves.**

Passphrase-encrypted key files are loaded, decrypted and used to connect to
Kafka. Mis-configured passwords fail predictably rather than silently.

---

## TC-19 — Input filter (regex)

Verify that upstream can filter out specific logs using regex rules, and
log the number of filtered/unfiltered logs.

* Covers: REQ-33, REQ-34
* Implemented in: 0.1.10-SNAPSHOT
* Runner: `./run-testcase.sh 19`

**Setup.**

* `upstream-airgap-19.properties` references the rule file
  `config/testcases/upstream-airgap-filter-19.txt`, which currently
  contains (the rule file has drifted from an earlier doc example —
  this reflects what's actually committed):

  ```text
  # Block common PII patterns
  # SSN
  deny:\b\d{3}-\d{2}-\d{4}\b
  # Email (case-insensitive)
  deny:(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}
  # Credit cards
  deny:\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b
  # Credentials
  deny:(?i)(password|passwd|pwd|secret|token|api_key)\s*[:=]
  # Allow everything else
  ```

  with `inputFilterDefaultAction=allow` (the "allow everything else"
  fallback is a config-level default, not a rule-file line).
* `downstream-airgap-19.properties` is a standard UDP-to-Kafka receiver.

**Chain run (Docker).**

TC-19 is about WHICH LITERAL CONTENT gets through, not counters or
gaps — LogGenerator has no "send this exact literal string" mode, so
the real assertions run entirely inside `POST_DRAIN_SCRIPT`, which
produces hand-crafted payloads directly via `kafka-console-producer`
(bypassing LogGenerator for the actual test content). The manifest's
`LG_PRODUCER_CONFIG` (reused from TC-17, `TEST_N` counter content) is
only a quick auto-exit ANCHOR — its content can never match any deny
rule, so it's harmless noise alongside the real assertions.

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-19-docker.properties](../../config/testcases/upstream-airgap-19-docker.properties)
  — `inputFilterRules` path fixed to the Docker mount
  (`/airgap/config/testcases/upstream-airgap-filter-19.txt` — the
  committed bare-metal file points at `/mnt/hgfs/air-gap/...`, which
  doesn't exist in Docker); `logStatistics=5` (vs. the committed 60)
  so the STATISTICS line with `total_filtered`/`total_unfiltered`
  appears inside the chain's short runtime. No `deliverFilter` — full
  delivery, the point here is input filtering, not gap detection.
* [downstream-airgap-19-docker.properties](../../config/testcases/downstream-airgap-19-docker.properties)
  — standard UDP-to-Kafka receiver, no topic translation.
* The rule file itself (`upstream-airgap-filter-19.txt`) is reused
  as-is — already under `config/testcases/`, mounted read-only.

[tests/env/tc19-inputfilter-test.sh](../env/tc19-inputfilter-test.sh)
is the `POST_DRAIN_SCRIPT` body. It produces 8 literal payloads to
`kafka-upstream[transfer]` via `kafka-console-producer` (one per
line via stdin), one for every deny rule plus the "bare password with
no trailing `:`/`=`" edge case that should fall through to
`inputFilterDefaultAction=allow`:

| Expect | Payload | Matches |
| --- | --- | --- |
| ALLOW | `TC19 ALLOW plain hello world` | no rule — falls to default |
| BLOCK | `TC19 BLOCK ssn 123-45-6789 end` | SSN |
| BLOCK | `TC19 BLOCK email anders@sitia.nu end` | email |
| BLOCK | `TC19 BLOCK EMAIL ANDERS@SITIA.NU END` | email, case-insensitive |
| BLOCK | `TC19 BLOCK cc 4111 1111 1111 1111 end` | credit card |
| BLOCK | `TC19 BLOCK credentials password: hunter2` | credentials (`password` + `:`) |
| ALLOW | `TC19 ALLOW password bare word` | credentials rule needs a trailing `:`/`=` — bare "password" doesn't match, falls to default |
| BLOCK | `TC19 BLOCK apikey api_key=abc123xyz end` | credentials (`api_key` + `=`) |

Each payload is written with a space before the sensitive pattern
(`... ssn 123-45-6789`, not `..._123-45-6789`) — gluing a marker
directly onto the pattern with `_` would itself be a word character
and could suppress a leading `\b` match.

The script then:

1. Reads `kafka-downstream[transfer]` from the beginning and checks
   each payload's EXACT text is present (ALLOW) or absent (BLOCK).
   Filtered payloads aren't just missing — `src/upstream/upstream.go`
   clears their payload to empty but still sends them (to preserve the
   gap-detector's sequence), so an ALLOW/BLOCK mismatch here is a real
   filter-logic bug, not noise.
2. Greps upstream's own `STATISTICS` log for the last `total_filtered`/
   `total_unfiltered` values and checks them against the expected
   counts (`total_filtered` == 6 exactly; `total_unfiltered` >= 100
   anchor + 2 allowed, using `>=` because the anchor burst and the
   hand-crafted ALLOW payloads can straddle two 5-second STATISTICS
   intervals depending on timing, but the counter is monotonic).

Manifest knobs in [testcases/19.env](../env/testcases/19.env):

* `TC_PROFILES=""`.
* `UPSTREAM_A_CONFIG`, `DOWNSTREAM_CONFIG` → the `-docker` variants.
* `LG_PRODUCER_CONFIG` → reused TC-17 anchor burst (no `LG_SINK_CONFIG`
  — Kafka content is checked directly with `kafka-console-producer`/
  `-consumer` instead, same reasoning as TC-15).
* `TC_DRAIN_SECONDS=5`.
* `POST_DRAIN_SCRIPT=tc19-inputfilter-test.sh`, `TC19_LG_ANCHOR_LIMIT=100`,
  `TC19_PROPAGATION_WAIT=8`.

**The same vacuous-verdict quirk as TC-15** applies here: no
`LG_SINK_CONFIG` means the generic sink-log parser falls back to the
anchor LG producer's own internal duplicate-check, printing a
`testcase 19 verdict: PASS (100/100 ...)` block that looks like a real
counter check but isn't. The REAL pass/fail signal is the
`tc19: ─── PASS/FAIL ───` line.

Expected chain output (abbreviated):

```text
─── post-drain hook: tc19-inputfilter-test.sh ───
tc19: ── producing 8 literal payloads to kafka-upstream[transfer] ──
tc19: waiting 8s for payloads to flow through upstream (filter) -> UDP -> downstream -> kafka-downstream[transfer]
tc19: ── reading kafka-downstream[transfer] ──
tc19: PASS — ALLOW "TC19 ALLOW plain hello world" -> present in kafka-downstream[transfer] as expected
tc19: PASS — BLOCK "TC19 BLOCK ssn 123-45-6789 end" -> absent from kafka-downstream[transfer] as expected
tc19: PASS — BLOCK "TC19 BLOCK email anders@sitia.nu end" -> absent from kafka-downstream[transfer] as expected
tc19: PASS — BLOCK "TC19 BLOCK EMAIL ANDERS@SITIA.NU END" -> absent from kafka-downstream[transfer] as expected
tc19: PASS — BLOCK "TC19 BLOCK cc 4111 1111 1111 1111 end" -> absent from kafka-downstream[transfer] as expected
tc19: PASS — BLOCK "TC19 BLOCK credentials password: hunter2" -> absent from kafka-downstream[transfer] as expected
tc19: PASS — ALLOW "TC19 ALLOW password bare word" -> present in kafka-downstream[transfer] as expected
tc19: PASS — BLOCK "TC19 BLOCK apikey api_key=abc123xyz end" -> absent from kafka-downstream[transfer] as expected
tc19: ── checking upstream STATISTICS counters ──
tc19: total_filtered=6 (expected 6), total_unfiltered=102 (expected >= 102)
tc19: ─── PASS: input filter rules matched expected allow/block decisions, and total_filtered/total_unfiltered counters are correct ───

✅ testcase 19 PASS
```

**Procedure (bare-metal / interactive).**

1. Both Kafka clusters started.
1. Start the upstream alone — it may complain it cannot reach the downstream
   yet.
1. Start a Kafka console producer on the upstream `transfer` (SSL).
1. Type `TEST` + Enter → the console-producer shows the next `>` prompt.
1. **Check upstream logs.** Because downstream is not up:

   ```text
   [DEBUG] Send attempt 1/3 failed for id=transfer_3_1: udp-connection-refused: ... retrying in 100ms
   [WARN] Message id=transfer_3_1 sent after 2 attempt(s) - UDP delivery is not guaranteed (receiver may be down)
   ```

1. Start air-gap downstream.
1. Start a Kafka console consumer on the downstream `transfer`.
1. Type `TEST2` in the console-producer. The upstream window should **not**
   show the earlier error again, and `TEST2` appears in the downstream
   consumer.
1. Try inputs covering each deny rule in the CURRENT rule file (SSN,
   email, credit card, credentials with `:`/`=`) plus a bare `password`
   with no trailing `:`/`=`:

   | Input                 | Expected                                             |
   | --------------------- | ----------------------------------------------------- |
   | `123-45-6789`         | blocked (SSN)                                        |
   | `anders@sitia.nu`     | blocked (email)                                      |
   | `ANDERS@SITIA.NU`     | blocked (email, case-insensitive)                    |
   | `4111 1111 1111 1111` | blocked (credit card)                                |
   | `password: hunter2`   | blocked (credentials, has `:`)                       |
   | `password`            | **allowed** -- no trailing `:`/`=`, falls to default |
   | `api_key=abc123`      | blocked (credentials, has `=`)                       |

1. Wait for a statistics log line on the upstream and check
    `total_filtered`/`total_unfiltered` match what you sent (6 filtered,
    everything else -- including TEST, TEST2, and the bare `password` --
    unfiltered).

**What it proves.**

Regex filtering drops matching events on the upstream before they cross the
diode, filter counters are logged, and content that doesn't match any deny
rule falls through to the configured default action.

---

## TC-20 — TCP transport

Verify that upstream can deliver to downstream over TCP, and that a late-
starting downstream still catches up without upstream losing data.

* Covers: REQ-35
* Implemented in: 0.1.10-SNAPSHOT
* Runner: `./run-testcase.sh 20`

**Setup.**

* `upstream-airgap-20.properties` has `transport=tcp`.
* `downstream-airgap-20.properties` listens on TCP/1234.

**Procedure.**

1. Both Kafka clusters up.
1. **Start upstream only.** It logs repeated:

   ```text
   TCP connection unavailable to kafka-downstream.sitia.nu:1234
   ```

1. Start a Kafka console producer on upstream `transfer` (SSL).
1. Type `TEST` + Enter.
1. **Start downstream.** Upstream recovers:

   ```text
   [INFO] Transport status restored to running (was: TCP connection unavailable to kafka-downstream.sitia.nu:1234)
   ```

1. Start a Kafka console consumer on downstream `transfer`. The first entry
   should be `TEST`.
1. Type `TEST2` in the producer.
1. Upstream shows no further errors.
1. The consumer shows `TEST2`.

**What it proves.**

TCP transport works upstream→downstream. If downstream is late, events stay
unmarked on upstream so no logs are lost; delivery resumes as soon as
downstream is reachable.

**Chain run (Docker).**

The real `downstream` container's binary launch is held back by a new,
reusable `STARTUP_DELAY` entrypoint knob (see
[tests/env/entrypoint.sh](../env/entrypoint.sh), wired from
`DOWNSTREAM_STARTUP_DELAY` in
[docker-compose.yml](../env/docker-compose.yml)) so `upstream-a`'s TCP
connection is genuinely unavailable for the first 10s of the run — a
reusable primitive for any future testcase that needs a role to start late,
not a one-off hack. Meanwhile `lg-producer` streams 1000 counter events
through at the usual eps=50.

**Root-caused a doc/default-config mismatch while building this.**
`upstream.go`'s `transportStatus` only ever transitions away from
`"running"` in the "all retries exhausted" branch, which is unreachable
when `tcpRetryTimes=0` (the default, meaning infinite in-process retries on
the same message) — so with the stock config, the documented
`Transport status restored to running` line would **never** appear;
upstream would just block silently on the first message until downstream
came up. The chain config
([upstream-airgap-20-docker.properties](../../config/testcases/upstream-airgap-20-docker.properties))
sets `tcpRetryTimes=3` instead: each give-up cycle marks the message as
not-consumed (an explicit comment in `upstream.go` confirms Kafka
redelivers it next poll — still zero data loss) and genuinely exercises the
unavailable → error → restored transition the manual procedure describes.

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-20-docker.properties](../../config/testcases/upstream-airgap-20-docker.properties)
  — `tcpRetryTimes=3`/`tcpRetryInterval=1000` (see above); also fixes two
  copy/paste leftovers from the committed bare-metal file: `groupID` was
  stuck at `19`, and an entire TC-19 `inputFilterRules` block (pointing at
  a bare-metal-only `/mnt/hgfs/...` path) had nothing to do with TC-20 and
  was dropped.
* [downstream-airgap-20-docker.properties](../../config/testcases/downstream-airgap-20-docker.properties)
  — same as the bare-metal file with `nic=eth0`.
* [upstream-lg-20.properties](../../config/testcases/upstream-lg-20.properties) /
  [downstream-lg-20-docker.properties](../../config/testcases/downstream-lg-20-docker.properties)
  — standard LG producer/sink pair (1000 events via `kafka-upstream`,
  verified back out of `kafka-downstream[transfer]`), same shape as TC-3's.

[tests/env/verify-tc20.sh](../env/verify-tc20.sh) is the `VERIFY_SCRIPT`.
Past the generic 1000/1000-delivered verdict (which only proves delivery,
not that the late-start path was ever actually exercised), it greps
`upstream-a`'s log for:

| Check | Expected |
| --- | --- |
| `TCP connection unavailable` retry lines | > 0 (downstream really was unreachable) |
| `Transport status restored to running` | > 0 (recovered on its own, didn't crash/restart) |
| Chronological order | first "unavailable" line precedes first "restored" line |

Validated passing 3 consecutive runs.

---

## TC-21 — Two clusters, same topic, into one dedup

Verify that two separate Kafka clusters that both expose a topic named
`transfer` can be fed by two upstreams into a single downstream and
deduplicated by one dedup instance.

* Covers: REQ-36
* Implemented in: 0.1.10-SNAPSHOT
* Runner: `./run-testcase.sh 21` (adds `dual`, `second-cluster`, `dedup`)

**Setup.**

* Second upstream cluster `kafka-upstream-b`/`-b-2` is activated by the
  `second-cluster` profile.
* `topic-init-upstream-b` creates `transfer` and `transfer2` on it.
* **Partition offset trick.** The second upstream reads partitions 10–14
  (`partitionStartValue=10` in `upstream-airgap-21b.properties`) so events
  from the two clusters never collide on the same partition after
  translation. The downstream topic has 15 partitions (topic-init did
  this).

**Procedure.**

1. Cluster 1 up with `transfer`, 15 partitions.
1. Cluster 2 up with `transfer`, 15 partitions. On bare metal reset storage
   so cluster IDs differ:

   ```bash
   bin/kafka-storage.sh format -t "$KAFKA_CLUSTER_ID_1" -c config/server1.properties
   bin/kafka-storage.sh format -t "$KAFKA_CLUSTER_ID_2" -c config/server2.properties
   ```

1. **Start the two upstreams** manually, each pointing at its cluster
   (`upstream-airgap-21a.properties` and `upstream-airgap-21b.properties`).
   `21b` has `partitionStartValue=10` and a `deliverFilter` that creates
   gaps.
1. Start a LogGenerator-cmd on the downstream `dedup`
   (`downstream-lg-21.properties`).
1. Start air-gap downstream (`downstream-airgap-21.properties`).
1. Start the dedup with `dedup-21.env`:

   ```bash
   export $(grep -v '^#' config/testcases/dedup-21.env | xargs)
   java -Dlog4j.configurationFile=/opt/airgap/dedup/log4j2-1.xml \
        -jar .../air-gap-deduplication-fat-*.jar
   ```

1. Start a console-consumer on `transfer` of the downstream Kafka.
1. **Generate**, 10 logs per cluster at 1 EPS:

   ```bash
   java -jar LogGenerator-1.1-7.jar -pf upstream-lg-21a.properties -l 10 --eps 1
   java -jar LogGenerator-1.1-7.jar -pf upstream-lg-21b.properties -l 10 --eps 1
   ```

1. The downstream LogGenerator sees interleaved entries from both clusters:

   ```text
   [transfer_11_24: Cluster2_3]
   [transfer_3_24:  Cluster1_2]
   ...
   ```

1. Stop the downstream LogGenerator. Expected: ~10 received, ~10 missing
    (the detector counts after the `_` so cluster1/cluster2 events compete
    for the same ID), 3 duplicates on the three overlapping values (3, 5,
    10).
1. Gap reader:

    ```bash
    ./target/<arch>/gaps config/testcases/downstream-airgap-9.properties --topic gaps
    ```

    Expected: gaps across partitions 0–4 and 10–14.
1. **Restart upstreams** with the no-filter variants
    (`upstream-airgap-21a2.properties`, `upstream-airgap-21b2.properties`)
    using a fresh `groupID` so Kafka replays.
1. Restart the downstream LogGenerator.
1. Generate another 10 logs per cluster.
1. Stop the downstream LogGenerator. Expected: 20 received, 10 unique
    (each value delivered twice), 0 missing.
1. **Resend remaining historical gaps:**

    ```bash
    ./target/<arch>/create config/testcases/create-21.properties \
      --resendFileName=./resend-first.json
    ```

1. Copy the resulting JSON to a host with reach to cluster 1 and run
    `resend-21a.properties`; copy again to one that reaches cluster 2 and
    run `resend-21b.properties`. After a short time the gap topic shows
    `Total: 0 missing`.

**What it proves.**

Logs from two different clusters sharing a topic name arrive, are dedupped
by a single dedup, and gaps can be filled by cluster-specific resends.

**Chain run (Docker).**

Automates phases 1+2 (the test's unique claim: two clusters sharing a
topic name feeding one dedup, with no data loss after a full replay).
Phase 3 (create + cluster-scoped resend of whatever gaps are STILL open
after the phase-2 replay) is deliberately left manual-only — the
create/resend mechanism itself is already rigorously chain-tested
standalone by TC-14/TC-18, and phase 2's full replay already closes the
gaps here; re-run `./run-testcase.sh 21 -pause` to exercise phase 3 by
hand if needed.

Two new reusable runner primitives, generalized from patterns this
testcase needed but that apply to any future multi-cluster/multi-wave
testcase:

* **`lg-producer-b`** (new service in
  [docker-compose.yml](../env/docker-compose.yml)) — a second, independent
  LogGenerator producer targeting the second upstream cluster, activated
  by `LG_PRODUCER_B_CONFIG`. Not the compose `--exit-code-from` source
  (only one container can be); `run-testcase.sh` separately polls its own
  exit after the primary producer finishes, so neither cluster's stream is
  truncated by the other finishing first.
* **`UPSTREAM_B_PHASE2_CONFIG`** / **`PHASE2_RERUN_LG_PRODUCER`** (new
  manifest knobs in [run-testcase.sh](../env/run-testcase.sh)) — extend
  the existing TC-9-style `UPSTREAM_A_PHASE2_CONFIG` gap-fill mechanism to
  a second upstream, and optionally re-run both LG producers with their
  original configs during phase 2 (matching the documented "generate
  another 10 logs per cluster" step, which is what produces the "each
  value delivered twice" duplicates once combined with the fresh-groupID
  replay).

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-21a-docker.properties](../../config/testcases/upstream-airgap-21a-docker.properties) /
  [upstream-airgap-21b-docker.properties](../../config/testcases/upstream-airgap-21b-docker.properties)
  — phase 1, `deliverFilter=2,4,6` opens gaps in partitions 0-4 / 10-14
  respectively, `groupID=21a-phase1` / `21b-phase1`.
* [upstream-airgap-21a2-docker.properties](../../config/testcases/upstream-airgap-21a2-docker.properties) /
  [upstream-airgap-21b2-docker.properties](../../config/testcases/upstream-airgap-21b2-docker.properties)
  — phase 2, no `deliverFilter`, fresh `groupID=21a-phase2` / `21b-phase2`
  so Kafka replays each cluster's topic from the start.
* [downstream-airgap-21-docker.properties](../../config/testcases/downstream-airgap-21-docker.properties),
  [dedup-21-docker.env](../../config/testcases/dedup-21-docker.env) —
  plaintext Docker rewrites of the committed bare-metal/mTLS files
  (`dedup-21.env` was SSL-only, pointing at bare-metal-only
  `/mnt/hgfs/...` keystore paths).
* [upstream-lg-21a-docker.properties](../../config/testcases/upstream-lg-21a-docker.properties) /
  [upstream-lg-21b-docker.properties](../../config/testcases/upstream-lg-21b-docker.properties) /
  [downstream-lg-21-docker.properties](../../config/testcases/downstream-lg-21-docker.properties)
  — LG producer pair (10 events each at 1 EPS, matching the documented
  procedure) + sink reading dedup's `dedup` clean topic (same convention
  as TC-9/TC-11's sinks).

[tests/env/verify-tc21.sh](../env/verify-tc21.sh) is the `VERIFY_SCRIPT`
(the generic sent/received/duplicate verdict only reads cluster A's
producer log, so with a phase-2 re-run its numbers are informational, not
authoritative here). It checks:

| Check | Expected |
| --- | --- |
| Cluster 1 / Cluster 2 events reaching the sink | both > 0 (both clusters actually got dedupped together) |
| Dedup max per-cycle `total_missing` across the whole run | > 0 (gaps genuinely opened — not a trivial pass) |
| Dedup last-cycle `total_missing` | <= 5 (closed by replay; small allowance for the same windowed tail-boundary blind spot documented for TC-14) |
| Dedup last-cycle `total_received` | > 0 |

Validated passing 3 consecutive runs. Confirmed no regression on TC-9
(single-upstream phase 2) and TC-11 (dual profile, no second cluster).

---

## TC-22 — TCP: downstream restart without loss

Verify that if the downstream TCP receiver restarts, upstream does not need
to be restarted and no events are lost.

* Covers: REQ-37
* Runner: `./run-testcase.sh 22`

**Setup.**

Only upstream and downstream. No Kafka in the data path — the test uses
`source=random` upstream, counter random messages.

**Procedure.**

1. Start upstream `./upstream config/upstream-tcp.properties`. Logs:
   `TCP connection unavailable to 127.0.0.1:1234`.
1. In another terminal, start downstream
   `./downstream config/downstream-tcp.properties`. Upstream switches to
   normal logging:

   ```text
   [DEBUG] sending 1 tcp messages for id=random_0
   [DEBUG] sending 1 tcp messages for id=random_1
   ```

   Downstream shows:

   ```text
   [DEBUG] Case cleartext
   Random message 0
   Random message 1
   ```

   First message is `Random message 0`.
1. Stop downstream. Note the last received message (e.g.
   `Random message 12`).
1. Restart downstream. It logs the TCP listener startup, then begins
   receiving at the next message after the one noted:

   ```text
   [INFO] TCP listener started on 0.0.0.0:1234
   [INFO] New TCP connection from 127.0.0.1:63219
   Random message 13
   Random message 14
   ```

**What it proves.**

A passing run demonstrates reconnection without restarting upstream and no
missing or duplicate messages in that run. The verifier checks the real
downstream sequence; the generic timer-service verdict is not evidence of
delivery.

The current TCP protocol has no application-level delivery acknowledgment.
A successful socket write is not proof that downstream processed the event,
so this test does not establish a universal no-loss guarantee for in-flight
messages during shutdown or failure.

On orderly shutdown, downstream closes the TCP listener and active
connections before its two-second flush delay. Keeping the listener open
after handlers stop reading lets upstream reconnect and write events that
are discarded. The local regression test
`TestTCPShutdownClosesListenerBeforeFlushDelay` checks this shutdown ordering;
it does not replace the Docker restart/delivery test.

**Chain run (Docker).**

No Kafka, no LogGenerator anywhere in this testcase's data path at all
(`source=random` upstream, `target=cmd` downstream), so there's no natural
auto-exit anchor and nothing for the generic sent/received/duplicate
verdict to meaningfully compare. Two new pieces:

* **`tc22-settle-timer`** (new service in
  [docker-compose.yml](../env/docker-compose.yml), same shape as TC-8's)
  just bounds the total test duration as the `AUTO_EXIT_SERVICE` anchor.
* **`DOWNSTREAM_RESTART_AT_SECONDS` / `DOWNSTREAM_RESTART_DOWN_SECONDS`**
  (new manifest knobs in [run-testcase.sh](../env/run-testcase.sh)) — a
  host-orchestrated watchdog, modeled on TC-2's
  `UPSTREAM_A_RESTART_AT_SECONDS` but using `docker stop`/`docker start`
  (not `--force-recreate`) so the SAME downstream container persists
  across the restart — `docker logs downstream` then captures one
  continuous, gap-checkable history spanning the whole stop/restart cycle.

Config files:
[upstream-tcp-docker.properties](../../config/testcases/upstream-tcp-docker.properties) /
[downstream-tcp-docker.properties](../../config/testcases/downstream-tcp-docker.properties)
— Docker-friendly variants of the committed bare-metal files (`nic=eth0`,
`targetIP`/`targetPort` overridden by compose as usual); `tcpRetryTimes=0`
(infinite) preserved from the bare-metal file since the documented
procedure only checks for resumed delivery, not a specific
`Transport status restored to running` line (contrast TC-20, which
needed a finite retry count to exercise that log transition).

[tests/env/verify-tc22.sh](../env/verify-tc22.sh) is the `VERIFY_SCRIPT`
and the REAL pass/fail for this testcase:

| Check | Expected |
| --- | --- |
| `Random message N` count at downstream | > 0 |
| Duplicate deliveries | 0 |
| Sequence gaps (0..max) | none |
| `TCP connection unavailable` during the downtime window | > 0 |
| `New TCP connection from` at downstream | >= 2 (initial + post-restart) |

Validated passing 3 consecutive runs (120/120 messages, zero gaps, zero
duplicates each time). Confirmed no regression on TC-8 (shares the
settle-timer pattern).

---

## TC-23 — `inputFilterRules` + resend respects filters

Verify that events dropped by `inputFilterRules` do not generate gaps, and
that `resend` applies the same filter so re-sent events are also filtered.

* Covers: REQ-38, REQ-39
* Implemented in: 0.1.10-SNAPSHOT
* Runner: `./run-testcase.sh 23` (adds `dedup`, `create`, `resend`)

**Setup.**

Same as TC-9 ramp but with `-23` config variants and dedup running against
`dedup-14.env` (reused). Filtered logs are delivered to Kafka as **empty**
events — zero-length payloads — so there is no gap.

**Procedure.**

Steps 1–11: standard ramp (verify Kafka, clear topics, start
LogGenerator-cmd, dedup, downstream, upstream, open gap reader). Generate
1000 logs at eps=1000.

1. Downstream LogGenerator receives ~500 of them (every other, by the
    filter in `upstream-airgap-23.properties`).
1. Dedup MISSING-REPORT lines show ~500 received, ~500 missing.
1. **Create a resend bundle with every gap:**

    ```bash
    docker compose --profile create run --rm create -- \
      config/testcases/create-23.properties \
      --resendFileName=./resend-all.json --limit=all
    ```

1. **Resend using the filter-aware config:**

    ```bash
    docker compose --profile resend run --rm resend -- \
      config/testcases/resend-23.properties \
      --resendFileName=./resend-all.json
    ```

    Statistics:

    ```text
    [INFO] STATISTICS: {"id":"Resend","received":1000,"sent":501,...}
    ```

    i.e. resend reads 1000 events but only forwards 501 — the ones that
    pass the input filter rules.

1. Verify dedup MISSING-REPORT now shows negative `delta_missing`
    (gap-close deltas) → `Total: 0 missing`.
1. Verify the gap topic shows 0 missing.
1. **Verify the clean topic.** Dump `dedup` to a file:

    ```bash
    bin/kafka-console-consumer.sh --topic dedup \
      --bootstrap-server kafka-downstream.sitia.nu:9094 \
      --consumer.config config/consumer.mtls.properties --from-beginning > result.txt
    ```

    `grep 10 result.txt` must find nothing (every TEST_* containing a `10`
    was filtered).

**What it proves.**

Filtered events do not create downstream gaps, and `resend` honours the
input-filter rules so filtered events are not re-sent either.

**Chain run (Docker).**

Same create/resend gap-closing mechanism as TC-14 (reused heavily —
[tc23-resend.sh](../env/tc23-resend.sh) is a near-clone of
[tc14-resend.sh](../env/tc14-resend.sh)), plus the `inputFilterRules=deny:10`
content filter active on BOTH upstream and resend — **the committed
bare-metal `upstream-airgap-23.properties`/`resend-23.properties` had this
rule present but commented out**, meaning the committed config never
actually exercised REQ-38/REQ-39 even manually; enabled in the Docker
configs below.

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-23-docker.properties](../../config/testcases/upstream-airgap-23-docker.properties)
  — `deliverFilter=2,4,6` (real gaps, same as TC-14) **and**
  `inputFilterRules=deny:10` (payload cleared to empty but still sent —
  no gap — for anything containing "10").
* [resend-23-docker.properties](../../config/testcases/resend-23-docker.properties)
  — same `inputFilterRules=deny:10`, the actual new claim over TC-14:
  resend must apply the same filter when replaying gapped content, not
  resurrect it in full.
* [create-23-docker.properties](../../config/testcases/create-23-docker.properties),
  [dedup-23-docker.env](../../config/testcases/dedup-23-docker.env),
  [upstream-lg-23-docker.properties](../../config/testcases/upstream-lg-23-docker.properties),
  [downstream-lg-23-docker.properties](../../config/testcases/downstream-lg-23-docker.properties)
  — plaintext Docker variants, same shape as TC-14's.

**Known limitation, documented rather than silently tolerated:** the LG
sink's own gap/duplicate counters are informational only for this
testcase, not authoritative (see the caveat comment in
`downstream-lg-23-docker.properties`) — an input-filtered message arrives
with an empty Kafka VALUE but a non-empty KEY, and the sink's
`_(\d+)$` regex (anchored to end-of-line) falls back to matching the
KEY's own trailing offset digits when VALUE is empty, which is an
unrelated number sequence from LogGenerator's own `TEST_N` counter. The
real pass/fail is [verify-tc23.sh](../env/verify-tc23.sh):

| Check | Expected |
| --- | --- |
| Dedup reports with negative `delta_missing` | > 0 (a gap genuinely closed) |
| Dedup last-cycle `total_missing` | <= 5 (closed by resend; same tail-boundary allowance as TC-14) |
| Dedup last-cycle `total_received` | > 0 |
| Clean topic (`dedup`) messages scraped directly via `kafka-console-consumer` | > 0 |
| Clean topic VALUEs containing "10" | **0** — the actual test of REQ-38/REQ-39: neither upstream's original filtering nor resend's gap-fill may leak filtered content |

Validated passing 3 consecutive runs (0 leaked values each time).
Confirmed no regression on TC-14 (shares the create/resend mechanism).

---

## TC-24 — TLS 1.3 / mTLS over TCP

Verify that:

* TLS 1.3 can be used.
* mTLS can be used.
* Two senders can share one receiver with mTLS.
* One-way (server-only) authentication can be used.
* Downstream can be configured for encrypted-only, cleartext-only, or (as
  per REQ-42) *either but not both at once*.
* Certificates and keys can be rotated with SIGHUP without packet loss.

* Covers: REQ-41, REQ-42, REQ-43, REQ-44, REQ-47
* Implemented in: 0.1.12-SNAPSHOT
* Runner: `./run-testcase.sh 24` (optionally `dual` for the two-sender
  variant)

**Setup.**

Random-source upstream, cleartext mode first:

* `config/downstream.properties` — cleartext UDP receiver.
* `config/upstream.properties` — cleartext upstream.

Then TLS variants:

* `config/downstream-tls.properties` — TLS 1.3 TCP listener, requires client
  cert, verifies the CN with a regex.
* `config/upstream-tls.properties` — upstream over mTLS (client cert).
* `config/upstream2-tls.properties` — second upstream.
* `config/upstream-tcp.properties` — plain TCP upstream for the "connection
  is accepted but handshake fails" test.

**Procedure.**

**Part A — cleartext baseline.**

1. Start cleartext downstream:

   ```bash
   go run src/cmd/downstream/main.go config/downstream.properties
   ```

1. Start cleartext upstream:

   ```bash
   go run src/cmd/upstream/main.go config/upstream.properties
   ```

   Downstream prints `UDP listener starting on 0.0.0.0:1234 with 10 workers`
   and random messages begin to arrive.

**Part B — mTLS, two clients.**

1. Stop everything from Part A. Start the TLS downstream:

   ```bash
   go run src/cmd/downstream/main.go config/downstream-tls.properties
   ```

   `TLS TCP listener started on 0.0.0.0:1234`.

1. Start a **cleartext** upstream aimed at the TLS port. It succeeds in
   opening a TCP socket but the TLS handshake fails. Downstream logs:

   ```text
   [DEBUG] Starting handshake with 127.0.0.1:51799
   [WARN]  Handshake failed from 127.0.0.1:51799: tls: first record does not look like a TLS handshake
   ```

   Confirms REQ-42 — downstream rejects un-encrypted traffic.

1. Stop that upstream. Start the TLS upstream:

   ```bash
   go run src/cmd/upstream/main.go config/upstream-tls.properties
   ```

   Even though the config requests TLS 1.2 the server forces 1.3.
   Representative upstream log (stop upstream briefly to catch the TLS
   handshake lines):

   ```text
   [INFO] TCP TLS configured (mTLS=true, cipherSuites="TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,...")
   [DEBUG] [TLS upstream] Handshake complete, peer certs: 1, TLS 1.3 / TLS_AES_128_GCM_SHA256
   [DEBUG] [TLS upstream] Server cert subject: CN=nu.sitia.airgap.downstream-1,O=SITIA.nu,C=SE
   [INFO]  [TLS upstream] Authenticated server CN "nu.sitia.airgap.downstream-1" (pattern "^nu\\.sitia\\.airgap\\.downstream-[0-9]+$")
   [INFO] Connected to TCP server at 127.0.0.1:1234
   ```

   Downstream similarly authenticates the client's CN.

1. Start a second upstream with `upstream2-tls.properties`. Both clients
   should now be delivering messages concurrently. Downstream logs
   authentication for both CNs
   (`nu.sitia.airgap.upstream-1`, `nu.sitia.airgap.upstream-2`).

**Part C — one-way (server-only) authentication.**

1. Stop everything. Start the TLS downstream with the client-CN check
   disabled:

   ```bash
   go run src/cmd/downstream/main.go config/downstream-tls.properties --tcpTLSClientCNRegex=""
   ```

1. Start the TLS upstream; downstream now accepts the handshake with any
   client cert in the CA chain and does not reject by CN.

**Part D — SIGHUP rotation.**

1. Stop everything. Use the *binaries* (not `go run`) so Go does not
   intercept SIGHUP. Start downstream with `downstream-tls.properties`.

1. Prepare writable copies of the cert files:

   ```bash
   cp certs/upstream.crt ./tmp/upstream.crt
   cp certs/upstream.key ./tmp/upstream.key
   cp certs/upstream.key.pw ./tmp/upstream.key.pw
   ```

1. Start the upstream pointing at the writable copies:

   ```bash
   ./target/<arch>/upstream config/upstream-tls.properties \
     --tcpTLSCertFile=./tmp/upstream.crt \
     --tcpTLSKeyFile=./tmp/upstream.key \
     --tcpTLSKeyPasswordFile=./tmp/upstream.key.pw
   ```

1. Overwrite with a second certificate, then SIGHUP:

   ```bash
   cp certs/upstream2.crt ./tmp/upstream.crt
   cp certs/upstream2.key ./tmp/upstream.key
   cp certs/upstream2.key.pw ./tmp/upstream.key.pw
   kill -HUP "$(pgrep upstream)"
   ```

1. Upstream logs the reload:

   ```text
   [INFO] SIGHUP received: reopening logs with name  and reloading TLS certificates
   [DEBUG] [TLS upstream] ReloadTLSConfig: reloading TLS configuration
   ...
   [INFO] [TLS upstream] TLS certificates reloaded from ./tmp/upstream.crt; reconnecting with new cert
   ```

   Downstream re-authenticates the new client CN. No packets lost in
   transit.

**What it proves.**

TLS 1.3 is used end-to-end, mTLS works for several concurrent clients,
regex-based CN authorisation works, both one-way and mutual authentication
modes are supported, and both certificate and key can be rotated via SIGHUP
without packet loss. (Go's TLS 1.3 implementation does not expose cipher
selection; the three available suites are all NIST-approved — see
`doc/Encryption.md`.)

**Chain run (Docker).**

Automates Parts B, C, and D in full. Part A (cleartext baseline) is
skipped as redundant — already fully covered by TC-1/3/22.

Two new reusable runner primitives, added for this testcase but generally
applicable:

* **`PRE_UP_SCRIPT`** (new manifest knob in
  [run-testcase.sh](../env/run-testcase.sh), parallel to
  `POST_DRAIN_SCRIPT` but running BEFORE `docker compose up -d`) — needed
  because [tc24-pre-up.sh](../env/tc24-pre-up.sh) seeds writable
  cert-rotation copies (`/airgap/tmp/tc24-rotate.{crt,key,key.pw}`) that
  `upstream-a` reads at its very first startup; a post-up hook would run
  too late.
* **`tc24-settle-timer`** (new service in
  [docker-compose.yml](../env/docker-compose.yml), same shape as TC-8/22's)
  — bounds the initial mTLS handshake settle time; all real testing
  happens in `POST_DRAIN_SCRIPT=`[tc24-tls-test.sh](../env/tc24-tls-test.sh).

**Bugs found and fixed along the way:**

* `upstream-a`'s compose service had no `../../tmp` volume mount at all
  (only `config` and `certs`, both read-only) — needed for the
  cert-rotation files. Added (read-write, harmless for every other
  testcase, which never touches it).
* A genuinely surprising bash quirk, discovered via this testcase's
  `PRE_UP_SCRIPT` hitting an unbound-variable reference during
  development: a `VAR="$unbound_var" command` prefix-assignment, when
  `$unbound_var` is unset under `set -u`, aborts the script non-
  interactively but does **not** propagate its exit status to `$?` as
  seen by the `EXIT` trap — `cleanup_on_exit` saw `exit_code=0` and the
  chain silently reported **PASS** despite the fatal error. Root-caused
  to `PROJECT_NAME` being referenced (via the new `PRE_UP_SCRIPT` hook)
  before it was defined; fixed by moving its definition earlier. Left a
  comment on `cleanup_on_exit` documenting the quirk for future hook
  authors, since it's a real fragility (any future unbound-variable
  reference in a `VAR=val` prefix position anywhere in the hook-invocation
  code could silently mask a failure the same way).

tc24-tls-test.sh's phases (all using
[report_check](../env/lib/report.sh) assertions — these ARE the pass/fail
for this testcase; the generic sent/received/duplicate verdict is a
trivial marker-line pass, same as TC-22, not authoritative):

| Phase | Check |
| --- | --- |
| B | upstream-a authenticated downstream's server CN |
| B | downstream authenticated upstream-1's client CN |
| B | raw-bytes probe at the TLS port → downstream logs a handshake failure AND stays running |
| B | upstream-b (second concurrent mTLS client, different CN) also authenticates |
| C | downstream recreated with `tcpTLSClientCNRegex=""` → authenticates "by CA chain only", no handshake failures |
| D | downstream restored to CN-required; cert files overwritten with upstream2's cert/key; SIGHUP |
| D | upstream-a logs "TLS certificates reloaded" |
| D | downstream re-authenticates the NEW (rotated) client CN |
| D | downstream's `Random message N` sequence across the WHOLE rotation window has zero gaps — proof nothing was lost |

Validated passing 3 consecutive runs (identical results each time — gap
check always showed the same 99-value contiguous range). Confirmed no
regression on TC-1 (upstream-a's new volume mount) and TC-22 (shares the
settle-timer pattern).

---

## TC-25 — `SO_RXQ_OVFL` kernel drop counter

Verify that the downstream reports the Linux-kernel UDP overflow counter
(packets dropped by the kernel because downstream could not keep up) in its
statistics so you can distinguish kernel-dropped packets from air-gap-
dropped ones.

* Covers: REQ-49
* Implemented in: 0.1.12-SNAPSHOT
* Platform: **Linux only** (`SO_RXQ_OVFL` is a Linux socket flag).
* Runner: `./run-testcase.sh 25`

**Setup.**

Two downstream configs:

* `downstream-airgap-25.properties` — `SO_RXQ_OVFL` disabled (default).
* `downstream-airgap-25b.properties` — `SO_RXQ_OVFL` enabled and
  `logFileName=./tmp/downstream-25b.log` with `logLevel=DEBUG`.

**Procedure.**

**Negative control.**

1. Start downstream without the counter:

   ```bash
   go run src/cmd/downstream/main.go config/testcases/downstream-airgap-25.properties
   ```

1. Start upstream:

   ```bash
   go run src/cmd/upstream/main.go config/testcases/upstream-airgap-25.properties
   ```

   Downstream prints statistics. Verify `SO_RXQ_OVFL` is **not** in the
   JSON:

   ```text
   [INFO] STATISTICS: {"cache_entries":0,"eps":1,"id":"Downstream_25","interval":10,"kafka_status":"running","received":10,"sent":10,...}
   ```

**Positive control.**

1. Stop both and start downstream with the counter enabled:

   ```bash
   go run src/cmd/downstream/main.go config/testcases/downstream-airgap-25b.properties
   ```

1. In another terminal:

   ```bash
   tail -f tmp/downstream-25b.log | grep SO_RXQ_OVFL
   ```

   Statistics now include both `SO_RXQ_OVFL` (delta) and
   `SO_RXQ_OVFL_TOTAL`:

   ```text
   [INFO] STATISTICS: {"SO_RXQ_OVFL":0,"SO_RXQ_OVFL_TOTAL":0,"cache_entries":0,"eps":1,...}
   ```

1. Blast the receiver (`--eps=-1` means "as fast as possible"):

   ```bash
   go run src/cmd/upstream/main.go config/testcases/upstream-airgap-25.properties --eps=-1
   ```

   Watch the counter grow if downstream cannot keep up.

**What it proves.**

The kernel UDP overflow counter is exposed in the statistics log, so loss
caused by the kernel can be told apart from loss caused by air-gap itself.
`SO_RXQ_OVFL` has a small CPU cost and is only enabled when configured.

**Chain run (Docker).**

"Linux only" per the manual procedure note above — but every container in
this Docker rig IS Linux regardless of host OS (Docker Desktop runs a
Linux VM on macOS/Windows too), so this is fully chain-automatable despite
the platform restriction. No Kafka/LogGenerator in this testcase's data
path (`source=random` upstream, `target=cmd` downstream, same shape as
TC-22/24) — `tc25-settle-timer` just bounds the negative-control settle
window; all real testing happens in
[tc25-rxqovfl-test.sh](../env/tc25-rxqovfl-test.sh) (`POST_DRAIN_SCRIPT`).

Config files (all under [config/testcases/](../../config/testcases)):

* [upstream-airgap-25-docker.properties](../../config/testcases/upstream-airgap-25-docker.properties),
  [downstream-airgap-25-docker.properties](../../config/testcases/downstream-airgap-25-docker.properties)
  — negative control (`enableRxqOvfl=false`, the default).
* [downstream-airgap-25b-docker.properties](../../config/testcases/downstream-airgap-25b-docker.properties)
  — positive control (`enableRxqOvfl=true`). The bare-metal `25b` file
  this is modeled on is referenced throughout the manual procedure above
  but was **never actually committed** — created here.
* [upstream-airgap-25-blast-docker.properties](../../config/testcases/upstream-airgap-25-blast-docker.properties),
  [downstream-airgap-25c-docker.properties](../../config/testcases/downstream-airgap-25c-docker.properties)
  — the bonus "blast the receiver" stress phase (`eps=-1`). `target=null`
  (not `cmd`) here specifically — at "as fast as possible" throughput
  `target=cmd`'s one-print-per-message behavior produced **5.6 million**
  log lines in a 14s window on first attempt, which is pure waste (ballons
  docker's log storage for zero benefit) since the `STATISTICS` line this
  phase actually checks prints regardless of output target.
  `target=null` ("for performance testing" per
  `src/downstream/downstream.go`) discards per-message output while
  `SO_RXQ_OVFL` tracking — which happens at the UDP socket layer in
  `src/udp/receiver.go`, upstream of and independent from the output
  target — is completely unaffected.

[tc25-rxqovfl-test.sh](../env/tc25-rxqovfl-test.sh) checks:

| Check | Expected |
| --- | --- |
| Negative control: `STATISTICS` lines present | > 0 |
| Negative control: any mentioning `SO_RXQ_OVFL` | **0** |
| Positive control (downstream recreated with `enableRxqOvfl=true`): `STATISTICS` lines present | > 0 |
| Positive control: `STATISTICS` lines with `SO_RXQ_OVFL` key | > 0 |
| Positive control: `STATISTICS` lines with `SO_RXQ_OVFL_TOTAL` key | > 0 |
| Bonus blast (`eps=-1`): max `SO_RXQ_OVFL` delta observed | informational only — host/container-resource-dependent, doesn't affect pass/fail |

Validated passing 3 consecutive runs. The bonus blast check is
genuinely non-deterministic by design: one run triggered real kernel
drops (observed delta: 89260, with the earlier `target=cmd` blast
variant), later runs (with `target=null`, which processes each packet
fast enough to often avoid overflowing the kernel buffer at all) did not
— both outcomes are expected and neither fails the testcase, since
REQ-49's actual claim (the counter is exposed when enabled) is already
proven unconditionally by the positive/negative control checks above.
Confirmed no regression on TC-22/24 (share the settle-timer and
force-recreate patterns).

---

**All 25 testcases now have chain-run automation** (TC-16 excluded by
deliberate design — see its section above for why a performance benchmark
can't have a meaningful automated chain variant). `./run-testcases.sh`
with no arguments runs the full suite.

## TC-26 — Production configuration warning acceptance tests

* Covers: REQ-50
* Status: **Implemented**.
* Runner: Unit and subprocess lifecycle tests, not the Docker chain runner. No Kafka, Docker,
  LogGenerator, or real certificate files are required.
* Scope: Upstream, downstream, create, resend, and Java dedup only.

The rollout scenario is a previously verified test environment copied into
production without changing its configuration. Warnings identify risks for
review, not a universal definition of an invalid production deployment.

### Initial warning catalog

Each row requires one warning for its setting when its condition applies.
Threshold comparisons are strict: equality at a stated floor is not warned.

| Application | Setting / condition | Production risk |
| --- | --- | --- |
| All four Go applications | `logLevel=DEBUG` or `TRACE` | High log volume; debug logs may expose event data |
| Upstream/downstream/resend | `logStatistics=0` | Delivery/loss counters are not periodically visible |
| Upstream | `source=random` | Synthetic traffic rather than real Kafka input |
| Upstream | non-empty `deliverFilter` | Intentionally skips some events; complementary senders must cover them |
| Upstream | `transport=tcp` and `tcpRetryTimes>0` | Stops retrying after downstream outage; messages may be lost |
| Upstream | active Kafka input without `caFile` | Kafka connection is plaintext |
| Upstream | UDP without `encryption`, or TCP without `tcpTLSEnabled` | Transport payloads are plaintext |
| Downstream | `target=cmd` or `null` | Console-only output or discarded events, not Kafka delivery |
| Downstream | active Kafka output without `caFile` | Kafka connection is plaintext |
| Downstream | `transport=tcp` without `tcpTLSCertFile` | TCP listener is plaintext |
| Downstream | TLS TCP listener with `tcpTLSClientAuth=none` or `allow` | Client authentication is not mandatory |
| Downstream | `channelBufferSize<16384` | Small queue increases backpressure/drop risk |
| Downstream | UDP `rcvBufSize<4194304` | Small requested socket buffer increases overflow risk |
| Downstream | UDP `readBufferMultiplier<16` | Reduced receive-buffer headroom |
| Downstream | `maximumDecompressSize<1048576` | Larger legitimate decompressed events are rejected |
| Create/resend | active Kafka connection without `caFile` | Kafka connection is plaintext |
| Resend | `encryption=false` | UDP resend payloads are plaintext |
| Resend | encryption enabled with `generateNewSymmetricKeyEvery=0` | One symmetric key is used for the entire job |
| Java dedup | `WINDOW_SIZE<1000` | Windows roll quickly; old arrivals may bypass deduplication |
| Java dedup | `MAX_WINDOWS<500` | Short retained history; delayed resends may bypass deduplication |
| Java dedup | effective `state.dir` under `/tmp` or `/var/tmp` | Temporary local state may be removed, requiring restoration |
| Java dedup | effective `num.standby.replicas=0` | No warm standby state for failover |
| Java dedup | effective `security.protocol=PLAINTEXT` or `SASL_PLAINTEXT` | Kafka transport is unencrypted, even if SASL authenticates |

The dedup floors use the [FAQ's 500,000-offset example](../../doc/FAQ.md).
Warn for either small dimension even if a large other dimension compensates:
these are explicit review heuristics. Include the retained offset capacity in
the risk explanation; calculate it without 32-bit overflow.

### Settings reviewed without fixed warning thresholds

Worker counts, batch size, payload size/MTU, event rate limits, compression
thresholds, partition translations/ranges, resend date/offset ranges,
retry/commit/persistence/report intervals, fail-fast, and memory/reassembly
limits depend on throughput, packet sizes, topology, and retention policy.
Do not label their valid values unsafe based solely on arbitrary cutoffs.
`limit=first` is a valid production setting: create finds the first missing gap
in each partition, and resend replays from that offset onwards, not just that
single offset. It must not trigger a production warning.
Intentional input-content filters and redundant delivery filters require
deployment-wide coverage review; warn about configured delivery sampling
without claiming it is necessarily a mistake.

UDP downstream has no "encryption required" switch; do not infer the
sender's encryption policy from private-key filenames. `SO_RXQ_OVFL` is
not inherently unsafe and must not trigger a warning. A configured CA is
not proof of certificate validity or strong TLS policy. Certificate expiry,
permissions, and deployment workload sizing need separate operational checks.
Java logging is not a dedicated application environment setting; do not add
a fictional `LOG_LEVEL` knob in this first catalog.

### Executable unit acceptance contract

Go: `TransferConfiguration.WarnProductionConfiguration(phase string)`, with
either value or pointer receiver. Java: package-visible static
`PartitionDedupApp.warnProductionConfiguration(Properties effective, String phase)`.
The Java properties use resolved keys `WINDOW_SIZE`, `MAX_WINDOWS`,
`state.dir`, `num.standby.replicas`, and `security.protocol`.

Each warning is one log event with this searchable envelope:

```text
[WARN] [PRODUCTION-CONFIG] phase=startup setting=MAX_WINDOWS value=5 risk=...
```

For Java the logging framework supplies WARN severity. The message envelope
is the same; the message itself need not contain a duplicate `[WARN]`.
The risk must contain an explanation, not an empty placeholder.

Tests cover every catalog row, boundary/equality controls, inactive paths,
combined risks (no missing or duplicate warnings), startup/shutdown phase
parity, repeated evaluations, configuration immutability, and warning
visibility above WARN without changing the log threshold. Absence of the
required method is an explicit test failure, never a skipped or passing test.

From the repository root:

```bash
go test ./src/upstream ./src/downstream ./src/create ./src/resend -run ProductionWarnings -count=1
mvn -f java-streams/pom.xml -Dtest=ProductionConfigurationWarningsTest test
```

Both commands include subprocess lifecycle checks. They must not be
added to the Docker chain.

### Lifecycle acceptance

1. Start each application with multiple risk settings and valid dependencies.
   Verify exactly one startup warning per setting as the final events from
   configuration checking. Override a file risk via environment/CLI with a
   safe value; verify no stale-file warning.
2. Stop each daemon with SIGTERM and SIGINT. Also let create/resend finish
   normally and let dedup terminate via its orderly fail-fast path.
3. After all workers, Kafka clients, transports, final statistics, and
   shutdown hooks finish, verify the shutdown warning block is the final
   application log block, occurs once, and matches the effective settings.
   Include both file and stderr logging.
4. Repeat with no catalog risks: neither lifecycle phase emits production
   warnings. Delivery, configuration, exit codes, and log level are unchanged.
5. Use ERROR/FATAL thresholds: production warnings remain visible at WARN.
   Concurrent cleanup must not write application logs after the warning block.

Automated subprocess tests verify SIGINT/SIGTERM for the Go daemons, normal
completion and SIGINT/SIGTERM for create/resend, and file logging for all four
Go applications. They compare startup/shutdown warning sets and reject any
application log after the final warning block. Kafka access uses test adapters;
the Go daemon tests use real local UDP sockets. A TCP cleanup regression checks
that idle connections are closed and their handlers joined.

Java subprocess tests verify SIGTERM during startup and fail-fast termination
against an unavailable local broker, preserving the fail-fast failure exit code.
The JVM hook waits for main lifecycle cleanup and owns logger shutdown so Log4j
cannot close before the final warning block. These checks do not replace a
Kafka-backed delivery/rebalance integration run or Linux runtime verification.
