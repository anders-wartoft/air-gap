# Air-gap requirements

Source of truth for air-gap functional requirements. (Originated as
`tests/documents/Krav.xlsx`; this document now supersedes the spreadsheet.)

Each entry carries:

* **Status** — e.g. withdrawn, pending.
* **Implemented in** — SNAPSHOT version where the requirement was fulfilled.
* **Covered by** — testcase numbers from [TESTCASES.md](./TESTCASES.md).
* **Notes** — comments from the original Kommentar / Implementationskommentar
  columns, where useful.

If a cell is blank it means the spreadsheet left that cell blank.

---

## REQ-1 — Deliver to the existing receiver API

> Be able to send logs to the existing receiver API, so LIA can keep acting as
> a diode-side supplier into systems that still use the current receiver.

* Status: **Withdrawn** (JL, 2025-08-15).

## REQ-2 — Opt-in retransmission

> It must be configurable whether air-gap performs retransmissions, regardless
> of which receiver is in use.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-1

## REQ-3 — Configurable retransmission strategy

> It must be configurable **how** air-gap performs retransmissions. See
> REQ-3a..REQ-3d.

* Covered by: TC-1, TC-14

### REQ-3a — Automatic time intervals

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-1
* Notes: via `sendingThreads`.

### REQ-3b — Manual (file-driven) lists

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-14
* Notes: via the `create` / `resend` tools.

### REQ-3c — Number of retransmissions

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-1
* Notes: via `sendingThreads`.

### REQ-3d — Configurable time interval (one-shot job)

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-14
* Notes: via the `create` / `resend` tools.

## REQ-4 — Deduplicate on the receiving side

> On retransmission / restart, duplicates must be removed on the receiving
> side.

* Implemented in: 0.1.3-SNAPSHOT
* Covered by: TC-9, TC-10

## REQ-5 — Resume from last known log entry

> On restart, air-gap must resume from the last known log entry, not restart
> from the beginning.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-2
* Notes: Kafka stores client offsets; naming the client and the group
  deterministically is enough.

## REQ-6 — Plain-text traffic

> Air-gap must support plain-text traffic.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-3
* Notes: Testcase 3 covers Kafka→upstream and upstream→downstream (downstream
  is emulated with LogGenerator).

## REQ-7 — Encrypted traffic

> Air-gap must support encrypted traffic.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-4, TC-6

### REQ-7a — Encrypted upstream↔downstream

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-6
* Notes: Testcase 6 covers encryption across the diode.

### REQ-7b — Encrypted Kafka ↔ upstream/downstream

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-4
* Notes: Testcase 4 covers Kafka→upstream→downstream→Kafka with encryption
  to Kafka only (not across the diode).

## REQ-8 — Memory bounds

> Air-gap must be configurable for how much memory it is allowed to consume
> (max and min).

* Implemented in: 0.1.3-SNAPSHOT
* Covered by: TC-10
* Notes: Supported in the systemd templates (`MemoryMax`, `MemoryHigh`).

## REQ-9 — No log loss on restart

> Air-gap must not lose logs on restart.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-2

## REQ-10 — Clusterable

> Air-gap must be clusterable.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-1, TC-2, TC-3, TC-4, TC-6, TC-7, TC-8

## REQ-11 — Throughput target

> Air-gap must handle a traffic rate of **?** EPS.

* Covered by: TC-16
* Notes: Rate depends heavily on the environment. Aim for a stable version
  that works even virtualised. Measure with LogGenerator and watch the
  `logStatistics` output on both sides.

## REQ-12 — Linux support

> Air-gap must run on Linux-based operating systems.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: all
* Notes: All development is on macOS and Fedora 42.

## REQ-13 — Dedicated log path

> Air-gap must do its own internal logging to a dedicated path for
> troubleshooting and general operations info. Logs must not go to
> `/var/log/messages`.

* Implemented in: 0.1.1-SNAPSHOT
* Covered by: TC-8

## REQ-14 — Deliver to a Kafka cluster

> Air-gap must support delivering to a Kafka cluster.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-4, TC-6, TC-7, TC-9

## REQ-15 — Install / upgrade / uninstall docs

> Air-gap must have documentation for installation / upgrade /
> uninstallation.

* Notes: Covered by the top-level `README.md` and
  `doc/Installation and Configuration.md`.

## REQ-16 — Send / receive / dedup logic docs

> Air-gap must have documentation for the send / receive / dedup logic.

* Notes: Covered by `README.md`, `Kafka-encryption.md`, `Monitoring.md`,
  `Resend.md`, `Deduplication.md`.

## REQ-17 — Run as systemd service

> Air-gap must be runnable as a service (via systemd).

* Implemented in: 0.1.3-SNAPSHOT
* Covered by: TC-10
* Notes: Service templates ship under `packaging/systemd/`.

## REQ-18 — Run as command-line binary

> Air-gap must be runnable as a binary (command line).

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-8

## REQ-19 — Large log entries

> Air-gap must handle large log entries so the full log entry is stored in
> Kafka.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-7
* Notes: Implemented by fragmentation/defragmentation in `src/protocol/`.

## REQ-20 — RPM install

> Air-gap must be installable from an RPM package.

## REQ-21 — RPM upgrade logic

> Air-gap must support upgrade via RPM.

* Notes: Package build uses NFPM (see `packaging/nfpm-*.yaml`).

## REQ-22 — Config file and environment variables

> Air-gap must be configurable both via config file and via environment
> variables.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-5
* Notes: 0.1.2-SNAPSHOT ships env-var overrides for upstream and downstream.
  Resend overrides tested later.

## REQ-23 — logrotate / SIGHUP

> Air-gap must support log-file rotation via `logrotate` and must resume
> writing to the configured file name again when it receives SIGHUP.

* Implemented in: 0.1.2-SNAPSHOT
* Covered by: TC-8
* Notes: The deduplicator uses log4j2 via `log4j2.xml`.

## REQ-24 — Redundancy and load sharing

> Air-gap must be able to send with redundancy and/or load sharing over
> several physical channels.

* Implemented in: 0.1.4-SNAPSHOT
* Covered by: TC-11, TC-12

## REQ-25 — Upstream event-count logging

> Number of received and sent events must be logged at a configurable
> interval in upstream.

* Implemented in: 0.1.4-SNAPSHOT
* Covered by: TC-4

## REQ-26 — Downstream event-count logging

> Number of received and sent events must be logged at a configurable
> interval in downstream.

* Implemented in: 0.1.4-SNAPSHOT
* Covered by: TC-4

## REQ-27 — Dedup event-count logging

> Number of received and sent events must be logged at a configurable
> interval in dedup.

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-13

## REQ-28 — Dedup missing-entry logging

> Number of missing log entries must be logged from dedup at a configurable
> interval.

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-13
* Notes: Logged as `INFO [MISSING-REPORT] {json...}`.

## REQ-29 — Log levels

> Upstream and downstream must support log levels of at least debug, info,
> and error.

* Implemented in: 0.1.4-SNAPSHOT
* Covered by: TC-4

## REQ-30 — Compression

> Upstream and downstream must be able to compress / decompress log data
> independently of whether encryption is used across the diode. Compression
> must be selectable for all payloads over a configurable length (for
> example 1200, 0, 10000).

* Implemented in: 0.1.5-SNAPSHOT
* Covered by: TC-15

## REQ-31 — Dedup TLS to Kafka

> The deduplicator must be able to connect to Kafka with TLS and
> certificate-based authentication.

* Implemented in: 0.1.6-SNAPSHOT
* Covered by: TC-17

## REQ-32 — Encrypted key files

> Upstream, downstream, `create`, and `resend` must be able to use encrypted
> (password-protected) key files.

* Implemented in: 0.1.7-SNAPSHOT
* Covered by: TC-18

## REQ-33 — Regex input filter

> Upstream must be able to filter out logs using regular expressions.

* Implemented in: 0.1.8-SNAPSHOT
* Covered by: TC-19

## REQ-34 — Filter counters

> Upstream must log the number of filtered and unfiltered logs.

* Implemented in: 0.1.8-SNAPSHOT
* Covered by: TC-19

## REQ-35 — TCP transport

> Upstream and downstream must be configurable to use TCP instead of UDP.

* Implemented in: 0.1.8-SNAPSHOT
* Covered by: TC-20

## REQ-36 — Multi-cluster, same-topic fan-in

> It must be possible to send logs from several different clusters that share
> the same topic name through several upstreams to a single downstream and
> single dedup.

* Implemented in: 0.1.9-SNAPSHOT
* Covered by: TC-21

## REQ-37 — Downstream restart over TCP without loss

> It must be possible to restart downstream without losing logs when
> `transport=tcp`. Once downstream is up again, normal log transfer must
> resume.

* Implemented in: 0.1.9-SNAPSHOT
* Covered by: TC-22

## REQ-38 — Filtered events do not create gaps

> When events are filtered out by `inputFilterRules`, those lines must not be
> reported as missing by dedup.

* Implemented in: 0.1.10-SNAPSHOT
* Covered by: TC-23

## REQ-39 — Resend respects input filter rules

> When logs are retransmitted with `resend`, `inputFilterRules` must be
> applied, just as in upstream.

* Implemented in: 0.1.10-SNAPSHOT
* Covered by: TC-23

## REQ-40 — Fixed-size gap storage

> Gaps must be stored in Kafka with a fixed size so the events do not grow
> during runtime until they can no longer be stored and the application
> stops.

* Implemented in: 0.1.11-SNAPSHOT
* Covered by: TC-14

## REQ-41 — Plain-text status messages during encryption

> For encrypted upstream→downstream transfer, only status messages must be
> sent **without** encryption. Status messages may contain important
> information about the upstream (and possibly about why encryption is not
> working), so they may be sent in plain text.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24
* Notes: For TLS, status messages are also encrypted.

## REQ-42 — Enforce encryption on downstream

> Downstream must be configurable to only accept encrypted messages (apart
> from status) to prevent event injection. Downstream must also be
> configurable to accept both encrypted and unencrypted messages — but not
> both at the same time.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24

## REQ-43 — One-way and mutual authentication

> For encryption, both one-way (server) and mutual authentication must be
> supported.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24

## REQ-44 — SIGHUP certificate rotation

> Certificates and keys must be rotatable with SIGHUP without packet loss.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24

## REQ-45 — X.509 / crt-key format

> Certificates must follow the X.509 standard and be in `.crt` / `.key`
> format.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24
* Notes: See `README.md` and `Encryption.md`.

## REQ-46 — Shared certificate for TCP and UDP

> A TCP and a UDP sender/receiver must be able to use the same certificate.

## REQ-47 — TCP crypto profile

> For TCP:
>
> * encryption protocol must be TLS 1.3 (mTLS for mutual authentication);
> * encryption algorithms must be NIST-approved;
> * encryption must work independently of the number of senders.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-24

## REQ-48 — UDP crypto profile

> For UDP:
>
> * encryption must work over a diode;
> * encryption must work independently of the number of senders (together
>   with `deliveryFilter`, `partitionOffset`, and similar);
> * the encryption algorithm must be well documented, with its properties
>   and non-properties clearly stated.

## REQ-49 — SO_RXQ_OVFL kernel drop counter

> `SO_RXQ_OVFL` is a socket flag (see `man socket(7)`) that counts how many
> packets Linux dropped (because downstream could not keep up). Include
> that counter in the statistics so we can tell air-gap-induced loss from
> network-induced loss.

* Implemented in: 0.1.12-SNAPSHOT
* Covered by: TC-25
* Notes: `SO_RXQ_OVFL` has some overhead; it is configurable and should not
  be enabled in production when the system is near its throughput ceiling.
  See `doc/FAQ.md` and `doc/Monitoring.md`.
