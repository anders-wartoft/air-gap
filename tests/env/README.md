# Freestanding air-gap test environment

A Docker playground for running air-gap end-to-end from a Kafka source, over
UDP or TCP (optionally through a diode or diode-restricted firewall), into a
Kafka sink — plus optional dedup / create / resend processes.

Independent from `tests/docker-compose.yml` (package-install smoke tests) and
the repository root `docker-compose.yml` (RPM-installed demo).

## Design goals

| Axis           | How you update it                                                                              |
| -------------- | ---------------------------------------------------------------------------------------------- |
| OS             | Change `UPSTREAM_BASE_IMAGE` / `DOWNSTREAM_BASE_IMAGE` in `.env`, run `docker compose build`   |
| Kafka          | Change `KAFKA_IMAGE` in `.env`, run `docker compose pull`                                      |
| JVM (dedup)    | Change `DEDUP_IMAGE` in `.env`, run `docker compose pull`                                      |
| Air-gap binary | Rebuild on the host. The container always runs the latest file in the mounted build directory. |
| Air-gap config | Edit files under `../../config` (mounted read-only), or pick a different testcase via runner.  |

The container image is deliberately minimal — an OS plus `iproute` / `ping` /
`ps`. The upstream / downstream / create / resend binaries and the dedup jar
are **not** baked in; they are bind-mounted from the host build directory.

## Topology

The Kafka layout mirrors the real deployment: each cluster is a two-broker
KRaft cluster. On the real nodes both brokers run on the same host; here each
broker is its own container, but they still form a single cluster and clients
bootstrap against both brokers. Each broker exposes **both** a PLAINTEXT
listener and an SSL listener at the same time, so a client can pick either
protocol (or run both side-by-side for comparison).

```text
                      upstream cluster (one host)                                     downstream cluster (one host)
  ┌────────────────────────────────────────────────┐              ┌──────────────────────────────────────────────────┐
  │ kafka-upstream   (node 1, PLAIN 9092 SSL 9094) │              │ kafka-downstream   (node 1, PLAIN 9092 SSL 9094) │
  │ kafka-upstream-2 (node 2, PLAIN 8092 SSL 8094) │              │ kafka-downstream-2 (node 2, PLAIN 8092 SSL 8094) │
  └────────────────────────────────────────────────┘              └──────────────────────────────────────────────────┘
                   ▲                                                          ▲        ▲
                   │                                                          │        │
            (kafka)│                                                   (kafka)│ (kafka)│
                   │                                                          │        │
          ┌────────┴────────┐                ┌────────────┐  ─(kafka)─>  ─────┘        │
          │   upstream-a    │  ─(UDP/TCP)─>  │ downstream │                            │
          └─────────────────┘                └────────────┘                            │
          ┌─────────────────┐                      ▲                                   │
          │   upstream-b    │  ─(UDP/TCP)─>────────┘                                   │
          │   (profile dual)│                                                          │
          └─────────────────┘                                                          │
                                                                                       │
                                                            ┌──────────────────┐       │
                                                            │ dedup, create,   │  ─────┘
                                                            │ resend (profiles)│
                                                            └──────────────────┘

  Second upstream cluster for cross-cluster testcases (profile second-cluster):
  kafka-upstream-b (node 1), kafka-upstream-b-2 (node 2)
```

* `upstream-a` always runs.
* `upstream-b` runs under profile `dual`.
* A second upstream cluster `kafka-upstream-b` / `kafka-upstream-b-2` runs
  under profile `second-cluster` (TC-21 only).
* `dedup`, `create`, `resend` run under their own profiles.
* A one-shot `topic-init` service formats both clusters with the standard
  topic set (`transfer`, `transfer2`, `dedup`, `dedup2`, `gaps`, `gaps2`,
  `transfer-11a`, `transfer-11b`, `transfer-12a`, `transfer-12b`) before
  anything else runs. See [TESTCASES.md](../documents/TESTCASES.md#topic-bootstrap).

## Prerequisites

1. Build the air-gap binaries for the architecture the containers will run on
   (Linux):

   ```bash
   make build-go              # produces target/linux-amd64/{upstream,downstream,create,resend,gaps}
   # or
   make build-go-all          # linux-amd64 + linux-arm64
   make build-java            # produces java-streams/target/air-gap-deduplication-fat-*.jar
   ```

   Default binary mount is `../../target/linux-amd64`. On Apple Silicon / arm64
   Linux hosts, set `AIRGAP_BIN_DIR=../../target/linux-arm64` in `.env`.

2. Install Docker and the Compose plugin (v2).

## Quick usage

```bash
cd tests/env
cp .env.example .env
docker compose up --build                             # single pair, default configs
docker compose --profile dual --profile second-cluster up  # two upstreams, two upstream clusters
docker compose down -v                                # clean shutdown + wipe volumes
```

Run a specific test case by number:

```bash
./run-testcase.sh 11                 # single machine, auto-exit + teardown when done
./run-testcase.sh 11 up-only         # upstream half only (see "Two machines")
./run-testcase.sh 11 down-only       # downstream half only
./run-testcase.sh 11 -pause          # run, then LEAVE containers up for inspection
```

With `-pause` (also `--pause` / `-p`), the stack stays up after `lg-producer`
finishes so you can inspect topics, copy log files, and open shells inside
containers. The runner prints a cheat sheet of ready-to-copy
`docker compose exec` / `kafka-console-consumer` / `docker cp` commands and
leaves the stack running until you tear it down with
`docker compose down -v --remove-orphans`.

Two environment knobs tune the auto-exit behaviour:

* `TC_DRAIN_SECONDS` (default **15**) — after `lg-producer` exits, the
  runner waits this long for in-flight events to propagate through the full
  pipeline before teardown. Raise it for larger `limit=` / higher `eps=`
  runs. Without this drain window the stack would be torn down mid-flight
  and the `MISSING-REPORT` output from `dedup` would record false positives.
* `LG_MAX_WAIT` (default **120**) — hard safety timeout. If `lg-producer`
  never exits within this many seconds, the runner bails with exit 124 and
  tears down anyway.

```bash
TC_DRAIN_SECONDS=30 ./run-testcase.sh 11     # longer drain for high-volume runs
LG_MAX_WAIT=120   ./run-testcase.sh 11       # shorter safety timeout
```

Teardown is always guaranteed: the runner installs a trap on
`EXIT`/`INT`/`TERM`/`HUP`, so even Ctrl-C or `kill` on the runner leaves zero
containers behind (except in `-pause` mode).

Chain several test cases back-to-back (each terminates automatically when its
LogGenerator producer is done — see "LogGenerator automation" below):

```bash
./run-all-testcases.sh                # every testcases/NN.env
./run-all-testcases.sh --from 9 --to 14
./run-all-testcases.sh 9 11 12
```

See [TESTCASES.md](../documents/TESTCASES.md) for the full catalogue and
[REQUIREMENTS.md](../documents/REQUIREMENTS.md) for the requirements each case covers.

## Updating the OS independently

The OS image is a build arg on the Dockerfile. The air-gap binary is **not**
baked in, so an OS bump never requires a rebuild of air-gap itself.

```bash
# in .env
UPSTREAM_BASE_IMAGE=rockylinux:9.4
DOWNSTREAM_BASE_IMAGE=rockylinux:9.4
```

Then rebuild only the air-gap containers:

```bash
docker compose build upstream-a upstream-b downstream
docker compose up -d
```

Valid values for `UPSTREAM_BASE_IMAGE` / `DOWNSTREAM_BASE_IMAGE` are any Linux
image whose package manager is one of `dnf`, `microdnf`, `apt-get`, or `apk`
(the Dockerfile auto-detects). Verified examples:

* `rockylinux:9`, `rockylinux:9.4`, `rockylinux:10`
* `almalinux:9`, `almalinux:10`
* `ubuntu:22.04`, `ubuntu:24.04`, `debian:12`
* `alpine:3.19`, `alpine:3.20`

The upstream and downstream sides can be upgraded independently of each other
and independently of Kafka.

## Updating Kafka independently

Kafka runs in its own containers and is referenced only by image tag:

```bash
# in .env
KAFKA_IMAGE=confluentinc/confluent-local:7.7.0
```

Then pull and restart only the Kafka services:

```bash
docker compose pull kafka-upstream kafka-upstream-2 kafka-downstream kafka-downstream-2
docker compose up -d kafka-upstream kafka-upstream-2 kafka-downstream kafka-downstream-2
```

Any image that honours the `KAFKA_*` environment variables used in
`docker-compose.yml` works — the compose services run Kafka in KRaft mode
(no ZooKeeper). Verified examples:

* `confluentinc/confluent-local:7.6.1` — default
* `confluentinc/confluent-local:7.7.0`
* `apache/kafka:3.7.0`
* `bitnami/kafka:3.7` — uses `KAFKA_CFG_*` variable names instead; prefer the
  Confluent or Apache images unless you have reason not to.

The dedup JVM image is also parameterised — bump `DEDUP_IMAGE` in `.env` to
move between `eclipse-temurin:17-jre-noble`, `eclipse-temurin:21-jre-noble`,
etc.

## Kafka SSL / PLAINTEXT dual listener

Every Kafka broker starts with **two listeners live at the same time**:

| Listener  | Port on broker 1 | Port on broker 2 | Auth                       |
| --------- | ---------------- | ---------------- | -------------------------- |
| PLAINTEXT | 9092             | 8092             | none                       |
| SSL       | 9094             | 8094             | server cert only (no mTLS) |

A client picks one or the other via its `bootstrap.servers` list and
`security.protocol`. In production you can simply remove the PLAINTEXT
listener from `KAFKA_LISTENERS` / `KAFKA_ADVERTISED_LISTENERS` and keep SSL.

### One-time setup

```bash
cd tests/env
./generate-kafka-certs.sh
```

That script is idempotent. It generates:

* `certs/kafka/ssl/testenv-ca.{crt,key}` — a per-test-env CA.
* `certs/kafka/ssl/kafka-upstream.keystore.jks` — SAN includes
  `kafka-upstream.sitia.nu`, `kafka-upstream`, `kafka-upstream-2`.
* `certs/kafka/ssl/kafka-upstream-b.keystore.jks` — matching alt-cluster SAN.
* `certs/kafka/ssl/kafka-downstream.keystore.jks` — generated **only if
  missing**; a pre-existing file is kept untouched.
* `certs/kafka/ssl/kafka-trust.jks` — contains the test-env CA **and** any CA
  already present in a pre-existing `kafka-*.truststore.jks`.
* `kafka-keystore-creds` / `kafka-key-creds` / `kafka-truststore-creds` —
  one-line password files consumed by the Confluent image's
  `KAFKA_SSL_*_CREDENTIALS` env vars.

Default password is `changeit`; override with
`KAFKA_KEYSTORE_PASSWORD=... ./generate-kafka-certs.sh` for a one-off, or set
it in `.env` so compose picks it up as well.

### Using the SSL listener from Kafka CLI

Create a client properties file (same format as the shipped
`certs/kafka/consumer.mtls.properties`, minus the keystore stanza since mTLS
is off):

```properties
security.protocol=SSL
ssl.truststore.location=/etc/kafka/secrets/kafka-trust.jks
ssl.truststore.password=changeit
```

Then from inside any Kafka broker container:

```bash
docker compose exec kafka-upstream \
  kafka-topics \
    --bootstrap-server kafka-upstream:9094,kafka-upstream-2:8094 \
    --command-config /path/to/client-ssl.properties \
    --list
```

The same file plugs into `kafka-console-producer` / `kafka-console-consumer`
as `--producer.config` / `--consumer.config`.

### Using the SSL listener from the air-gap binaries

The upstream, downstream, create, resend, and dedup services take Kafka TLS
options via their own config keys:

* `certFile` + `keyFile` + `caFile` — only `caFile` is needed when the broker
  does not require a client cert (our default). Point `caFile` at the shipped
  CA, e.g. the testenv one in `/airgap/certs/kafka/ssl/testenv-ca.crt`.
* Override via env var for the Docker environment:
  `AIRGAP_UPSTREAM_CA_FILE=/airgap/certs/kafka/ssl/testenv-ca.crt`.
* Switch the bootstrap list to the SSL port, e.g.
  `UPSTREAM_A_BOOTSTRAP=kafka-upstream:9094,kafka-upstream-2:8094` in `.env`.

### When mTLS does become a requirement

Flip the broker env var `KAFKA_SSL_CLIENT_AUTH: none` to `required` and point
each client at a keystore (for example the shipped
`certs/kafka/downstream.p12` or a freshly generated one). The current
testcases do not need this; it's a one-line change when the day comes.

## LogGenerator automation

The test cases use **LogGenerator** (a Java tool, external to this repo) as
their traffic source and as a convenient Kafka-consumer sink that prints the
events it receives. Drop any `LogGenerator-*.jar` into `tests/env/bin/` on the
host:

```bash
ls tests/env/bin/
# LogGenerator-1.1-6.jar
```

Two Compose services use it:

* **`lg-producer`** (profile `lg-producer`) — reads a producer-side
  `.properties` file, writes to Kafka, and exits when its `limit=N` is
  reached.
* **`lg-sink`** (profile `lg-sink`) — reads a sink-side `.properties` file
  (typically a Kafka consumer), prints the events it receives, and stays up
  until the stack tears down.

Both resolve the jar via a glob (`LogGenerator-*.jar`), so the version in the
filename is irrelevant — update the jar whenever a new release is published
and keep running.

### Wiring a testcase to LogGenerator

In `testcases/NN.env`, add either or both of:

```env
LG_PRODUCER_CONFIG=/airgap/config/testcases/upstream-lg-9.properties
LG_SINK_CONFIG=/airgap/config/testcases/downstream-lg-9.properties
```

The runner then:

1. Automatically activates the `lg-producer` and `lg-sink` profiles.
2. Starts the whole stack in detached mode (`docker compose up -d`).
3. Tails `lg-producer`, `lg-sink`, `dedup`, and the air-gap services' logs
   while the test runs.
4. Polls `docker inspect` on the `lg-producer` container every 2 s **and**
   in parallel tails its logs for a `transferred N lines` completion marker.
   Either path triggers the exit — on Docker Desktop `docker inspect` has
   been seen to keep reporting `status=running` for a surprisingly long
   time after the Java process has already exited, so the log-based
   detector is authoritative. A heartbeat prints every 10 s with the current
   state and the last log line from `lg-producer`, and a diagnostic peek is
   dumped at 30 s if the producer still hasn't exited.
5. Waits `TC_DRAIN_SECONDS` seconds (default 15) so the final events can
   finish propagating upstream → UDP → downstream → kafka → dedup → sink.
   Without this drain window the stack would get torn down mid-flight and
   the `MISSING-REPORT` output from `dedup` would record false positives.
6. Dumps the final `docker logs` tail for `lg-producer`, `lg-sink`, and
   `dedup` so the summary (`Transaction: transferred N lines` and
   `Number of unique received numbers: N`) is always visible in the
   terminal, even if the background log tail buffered.
7. Tears the whole stack down with `docker compose down -v --remove-orphans`
   via a trap, so Ctrl-C / signals also clean up cleanly. Suppress the
   teardown with `-pause` on the command line.

Polling `docker inspect` instead of `docker compose wait` avoids a race on
Docker Desktop where `wait` intermittently reports "no containers for
project" immediately after `up -d` returns. The `topic-init-*` services can
still exit cleanly during startup without tripping the shutdown — the
runner only keys off the `lg-producer` container's state. This is also why
the runner does **not** use `--exit-code-from` or `--abort-on-container-exit`.

To tweak LG behaviour per testcase without editing the shipped properties
file, pass CLI overrides via `LG_PRODUCER_ARGS` / `LG_SINK_ARGS`:

```env
LG_PRODUCER_ARGS=-l 1000 --eps 1000
```

If a legacy LG config file still contains bare-metal IPs, set
`LG_PRODUCER_SED` or `LG_SINK_SED` to a sed expression that rewrites them to
the Docker network hostnames at run time without touching the file. Example:

```env
LG_PRODUCER_SED=s|192\.168\.153\.14[0-9]|kafka-upstream.sitia.nu|g
```

The shipped LG property files under `config/testcases/*-lg-*.properties`
already use the `*.sitia.nu` hostnames that are DNS aliases on the Kafka
brokers in this environment, so `LG_SED` is empty by default.

### Chaining

`./run-all-testcases.sh` iterates over `testcases/NN.env` (optionally
filtered with `--from`/`--to` or an explicit list of ids). Each case is run
through `run-testcase.sh`, and the stack is torn down between cases, so the
whole chain is a clean sequence of independent runs.

## Picking a testcase manually

If you prefer to drive compose yourself, set these three values in `.env` and
run `docker compose up`:

```env
UPSTREAM_A_CONFIG=/airgap/config/testcases/upstream-airgap-11a.properties
UPSTREAM_B_CONFIG=/airgap/config/testcases/upstream-airgap-11b.properties
DOWNSTREAM_CONFIG=/airgap/config/testcases/downstream-airgap-11a.properties
```

The testcase properties reference bare-metal hostnames
(`kafka-upstream.sitia.nu`, `enp2s0`, …). Those are overridden with
container-friendly values via `AIRGAP_UPSTREAM_*` / `AIRGAP_DOWNSTREAM_*`
environment variables (see `.env.example`), so the testcase files themselves
stay untouched.

## Running across two machines (diode or diode-restricted firewall)

The compose project is split by Compose service names, so each half can run
on a separate machine. Services do **not** have cross-side `depends_on`
entries, so starting only the upstream half or only the downstream half is
safe.

### Two-machine prerequisites

* Both machines must have Docker + Compose and a checked-out copy of this repo
  with the air-gap binaries built (`make build-go`, `make build-java`).
* An IP route from the upstream machine to the downstream machine on
  **UDP/1234** (or **TCP/1234** for the TCP-based testcases 6, 20, 22, 24).
* For a real data-diode: a one-way link that physically prevents return
  traffic. See `doc/Transport Configuration.md` for the recommended ARP /
  routing setup.
* For a firewall emulating diode semantics: a stateless rule set that permits
  only `UPSTREAM_IP -> DOWNSTREAM_IP:1234/udp` outbound and drops everything
  in the reverse direction (no ESTABLISHED/RELATED matching).

### Layout

```text
┌────────── Upstream machine ──────────┐    ┌────────── Downstream machine ──────────┐
│ kafka-upstream    (compose service)  │    │ kafka-downstream    (compose service)  │
│ kafka-upstream-2  (compose service)  │    │ kafka-downstream-2  (compose service)  │
│ kafka-upstream-b (profile 2nd clust) │    │ downstream          (compose service)  │
│ upstream-a        (compose service)  │────┼─ UDP/1234 ──▶  (listen on 0.0.0.0)     │
│ upstream-b        (profile dual)     │    │ dedup               (profile dedup)    │
└──────────────────────────────────────┘    └────────────────────────────────────────┘
                                   (diode or diode-restricted firewall between them)
```

### 1. Pick a static IP on each machine

Record the two IPs; call them `UPSTREAM_HOST_IP` and `DOWNSTREAM_HOST_IP`.
These must survive reboots — `/etc/network/interfaces`, `nmcli`, or your IaC
of choice.

### 2. Make UDP/TCP leave the Docker network

Both the upstream and the downstream air-gap containers must bind the host
network for the diode path. The simplest option is to publish the UDP port:

```yaml
# on the DOWNSTREAM machine, override in a local docker-compose.override.yml
services:
  downstream:
    ports:
      - "1234:1234/udp"   # or /tcp for testcases 6, 20, 22, 24
```

```yaml
# on the UPSTREAM machine
services:
  upstream-a:
    network_mode: host    # outbound UDP leaves the host directly; no NAT return path
```

`network_mode: host` is important on the upstream side when a real diode is
present — Docker's user-defined bridge network expects return packets for NAT
state and a diode never sends any.

### 3. Configure the testcase for cross-host

On the **upstream** machine, in `.env`:

```env
UPSTREAM_A_TARGET_IP=<DOWNSTREAM_HOST_IP>
UPSTREAM_B_TARGET_IP=<DOWNSTREAM_HOST_IP>
UPSTREAM_A_BOOTSTRAP=kafka-upstream:9092,kafka-upstream-2:8092
UPSTREAM_B_BOOTSTRAP=kafka-upstream:9092,kafka-upstream-2:8092
UPSTREAM_A_NIC=<physical NIC facing the diode>
```

On the **downstream** machine, in `.env`:

```env
DOWNSTREAM_NIC=<physical NIC receiving from the diode>
DOWNSTREAM_BOOTSTRAP=kafka-downstream:9092,kafka-downstream-2:8092
```

### 4. Start the halves

Upstream machine:

```bash
./run-testcase.sh 11 up-only                     # upstream half
./run-testcase.sh 11 up-only dual                # add second upstream
./run-testcase.sh 21 up-only dual second-cluster # two upstreams, two clusters
```

Downstream machine:

```bash
./run-testcase.sh 11 down-only          # downstream + kafka-downstream pair
./run-testcase.sh 11 down-only dedup    # also start the deduplicator
```

The runner prints which services it starts and which Compose profiles it
activates so you can double-check before traffic flows.

### 5. Diode-specific checklist

* Set `transport=udp` in the upstream and downstream configs (default). Avoid
  TCP for the diode path; TCP needs a back-channel.
* Add a **static ARP entry** on the upstream for the downstream MAC so the
  upstream never issues ARP that the diode would drop. See
  `doc/Transport Configuration.md`.
* Add a **static route** on the upstream so the kernel does not consult the
  default gateway for the diode destination.
* Pin a **payloadSize** matching the diode's MTU
  (`AIRGAP_UPSTREAM_PAYLOAD_SIZE`) so the upstream does not try to auto-detect
  the MTU across a one-way link.
* Disable return-path features (do not use `transport=tcp` and do not try to
  run the dedup or resend on the upstream side).

### 6. Diode-restricted firewall ruleset (reference)

Linux `nftables` example that behaves like a diode for air-gap traffic without
a physical appliance:

```nft
table inet airgap {
  chain forward {
    type filter hook forward priority 0; policy drop;

    # Allow one-way UDP from upstream to downstream
    ip saddr $UPSTREAM_HOST_IP ip daddr $DOWNSTREAM_HOST_IP \
      udp dport 1234 accept

    # Explicitly drop return path — no stateful match
    ip saddr $DOWNSTREAM_HOST_IP ip daddr $UPSTREAM_HOST_IP drop
  }
}
```

Equivalent iptables:

```bash
iptables -P FORWARD DROP
iptables -A FORWARD -s $UPSTREAM_HOST_IP -d $DOWNSTREAM_HOST_IP \
  -p udp --dport 1234 -j ACCEPT
iptables -A FORWARD -s $DOWNSTREAM_HOST_IP -d $UPSTREAM_HOST_IP -j DROP
```

Keep **stateful** matching (`-m conntrack`, `ct state established,related`)
**out** of the ruleset. A stateful rule turns the firewall back into a
bidirectional device and defeats the purpose of the test.

## What this environment does *not* do yet

* No automated LogGenerator inside the stack — many testcases describe
  LogGenerator as the traffic source. Install it on either machine and point
  it at the appropriate Kafka port manually, following `TESTCASES.md`.
* No automated pass/fail assertions. The runner starts the right services with
  the right configs; verification is still a human read of the logs.
* No multi-instance dedup (`dedup@1`, `dedup@2`, …) in compose. Use the
  bare-metal script in `config/testcases/launchDedup.sh` for that.
* No TLS listener on the Kafka brokers by default. The real deployment also
  exposes an `SSL://...:9094/8094` listener. Enable it per-testcase by
  setting the `KAFKA_SSL_*` env vars and mounting a keystore (see TC-4,
  TC-17, TC-18).
