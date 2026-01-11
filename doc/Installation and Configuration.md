# Installation and Configuration

## Installation

1. **Clone the repository:**

```bash
git clone https://github.com/anders-wartoft/air-gap.git
cd air-gap
```

2\. **Build the binaries:**

```bash
make all
```

This builds upstream, downstream, and the deduplication Java application.

3\. **Install dependencies:**

- Go 1.18+ (for upstream/downstream)
- Java 17+ (for deduplication)
- Kafka 3.9+ (for event streaming)
- Optional: Metricbeat, Jolokia for monitoring

4\. **Prepare configuration files:**

- Copy and edit example configs in `config/` and `config/testcases/`.
- See below for details.

5\. **Generate keys for encryption (optional):**

- See README.md section "Keys" for key generation commands.

## Configuration

### Transport Selection

Air-gap supports both **UDP** (default) and **TCP** transports. Choose based on your network environment:

- **UDP**: For hardware diodes, high throughput, low latency
- **TCP**: For software connections, unreliable networks, connection state awareness

See [Transport Configuration.md](Transport%20Configuration.md) for detailed transport options and migration guide.

### Upstream

Edit your upstream config file (e.g., `config/upstream.properties`):

```bash
id=Upstream_1
nic=en0
targetIP=127.0.0.1
targetPort=1234
# Use TCP instead of UDP (optional, defaults to UDP)
transport=tcp          
source=kafka
bootstrapServers=192.168.153.138:9092
topic=transfer
groupID=test
publicKeyFile=certs/server2.pem
generateNewSymmetricKeyEvery=500
mtu=auto
```

Override any setting with environment variables (see README for details).

For TCP with environment variable:

```bash
export AIRGAP_UPSTREAM_TRANSPORT=tcp
./upstream config/upstream.properties
```

### Downstream

Edit your downstream config file (e.g., `config/downstream.properties`):

```bash
id=Downstream_1
nic=en0
targetIP=0.0.0.0
targetPort=1234
transport=tcp          # Use TCP instead of UDP (optional, defaults to UDP)
bootstrapServers=192.168.153.138:9092
topic=log
privateKeyFiles=certs/private*.pem
target=kafka
mtu=auto
clientId=downstream
```

For TCP with environment variable:

```bash
export AIRGAP_DOWNSTREAM_TRANSPORT=tcp
./downstream config/downstream.properties
```

### Deduplicator (Java)

Edit your deduplication config (e.g., `config/create.properties`):

```bash
RAW_TOPICS=transfer
CLEAN_TOPIC=dedup
GAP_TOPIC=gaps
BOOTSTRAP_SERVERS=192.168.153.148:9092
STATE_DIR_CONFIG=/tmp/dedup_state_16_1/
WINDOW_SIZE=10000
MAX_WINDOWS=10000
GAP_EMIT_INTERVAL_SEC=60
PERSIST_INTERVAL_MS=10000
APPLICATION_ID=dedup-gap-app
```

For details on how to run the applications as services, see README.md

#### Performance Tuning

For high event rates (e.g., 10,000 eps):

- `PERSIST_INTERVAL_MS`: 100–1000 ms (persist state every 0.1–1 second)
- `COMMIT_INTERVAL_MS`: 100–1000 ms (commit progress every 0.1–1 second)

Start with:

```bash
PERSIST_INTERVAL_MS=500
COMMIT_INTERVAL_MS=500
```

This means state and progress are checkpointed every 0.5 seconds, so at most 5,000 events would need to be re-processed after a crash.

**Tuning tips:**

- Lower values = less data loss on crash, but more I/O.
- Higher values = less I/O, but more data to reprocess after a failure.
- Monitor RocksDB and Kafka broker load; adjust if you see bottlenecks.

## Running the Applications

**Upstream:**

```bash
go run src/cmd/upstream/main.go config/upstream.properties
```

or (after build):

```bash
./src/cmd/upstream/upstream config/upstream.properties
```

**Downstream:**

```bash
go run src/cmd/downstream/main.go config/downstream.properties
```

or (after build):

```bash
./src/cmd/downstream/downstream config/downstream.properties
```

**Deduplicator:**

```bash
java -jar java-streams/target/air-gap-deduplication-fat-<version>.jar
```

## Troubleshooting

- If UDP sending fails, check static ARP and route setup.
- If performance is low, tune buffer sizes and batch settings (see README).
- If deduplication is not working, check environment variable scoping and config file paths.
- For monitoring, see `doc/Monitoring.md`.

## Monitoring

See `doc/Monitoring.md` for instructions on using Metricbeat, Jolokia, and JMX for resource and application monitoring.

## Uninstallation

To completely uninstall air-gap and its components:

1. **Stop running services:**

```bash
sudo systemctl stop upstream.service
sudo systemctl stop downstream.service
sudo systemctl stop dedup.service
```

2\. **Disable services:**

```bash
sudo systemctl disable upstream.service
sudo systemctl disable downstream.service
sudo systemctl disable dedup.service
```

3\. **Remove binaries:**

```bash
rm -f /opt/airgap/upstream/bin/*
rm -f /opt/airgap/downstream/bin/*
rm -f /opt/airgap/dedup/bin/*
rm -f /usr/local/bin/upstream
rm -f /usr/local/bin/downstream
rm -f /usr/local/bin/dedup
```

4\. **Remove configuration files:**

```bash
rm -rf /opt/airgap/upstream/*.properties
rm -rf /opt/airgap/downstream/*.properties
rm -rf /opt/airgap/dedup/*.properties
rm -rf /etc/airgap/
```

5\. **Remove keys and certificates (if used):**

```bash
rm -rf /opt/airgap/certs/
```

6\. **Remove systemd service files:**

```bash
sudo rm -f /etc/systemd/system/upstream.service
sudo rm -f /etc/systemd/system/downstream.service
sudo rm -f /etc/systemd/system/dedup.service
sudo systemctl daemon-reload
```

7\. **Remove log files:**

```bash
rm -rf /var/log/airgap/
```

8\. **(Optional) Remove cloned source directory:**

```bash
rm -rf ~/air-gap
```

**Note:** Adjust paths as needed for your installation. If you installed to custom locations, remove those as well.

## Statistics

The air-gap system provides detailed statistics logging to help you monitor throughput, performance, and system health. Statistics are emitted as JSON-formatted log messages with the prefix `STATISTICS:` or `MISSING-REPORT:`.

### Enabling Statistics

To enable statistics logging, configure the `logStatistics` parameter in your upstream or downstream configuration file:

```bash
logStatistics=60
```

This configures the application to log statistics every 60 seconds. Set to `0` to disable statistics logging.

You can also override this setting using an environment variable:

```bash
export UPSTREAM_LOG_STATISTICS=60
```

### Statistics Fields

#### Upstream and Downstream Statistics

Both upstream and downstream applications emit the following fields in their `STATISTICS:` log entries:

- **`id`**: The identifier of the application instance (from config)
- **`time`**: Current Unix timestamp (seconds since epoch) when the statistics were logged
- **`time_start`**: Unix timestamp when the application started (used to calculate uptime)
- **`interval`**: The configured statistics logging interval in seconds
- **`received`**: Number of events received during the last interval
- **`sent`**: Number of events sent during the last interval
- **`filtered`**: Number of events filtered (blocked) during the last interval (input filter only)
- **`unfiltered`**: Number of events that passed through the input filter during the last interval
- **`filter_timeouts`**: Number of regex timeout errors during the last interval (input filter only)
- **`eps`**: Events per second during the last interval (calculated as `received / interval`)
- **`total_received`**: Total number of events received since application start
- **`total_sent`**: Total number of events sent since application start
- **`total_filtered`**: Total number of events filtered (blocked) since application start (input filter only)
- **`total_unfiltered`**: Total number of events that passed through the input filter since application start
- **`total_filter_timeouts`**: Total number of regex timeout errors since application start (input filter only)

**Example upstream statistics log:**

```json
STATISTICS: {"id":"Upstream_1","time":1732723200,"time_start":1732720000,"interval":60,"received":3600,"sent":3600,"eps":60,"total_received":72000,"total_sent":72000}
```

**Example downstream statistics log:**

```json
STATISTICS: {"id":"Downstream_1","time":1732723200,"time_start":1732720000,"interval":60,"received":3590,"sent":3590,"eps":59,"total_received":71800,"total_sent":71800}
```

#### Deduplication Statistics

The Java deduplication application emits `MISSING-REPORT:` log entries for each partition, containing:

- **`partition`**: The Kafka partition number
- **`total_missing`**: Total number of gaps/missing events detected
- **`delta_missing`**: Number of new gaps detected during the last interval
- **`total_received`**: Total number of events received on this partition
- **`delta_received`**: Number of events received during the last interval
- **`total_emitted`**: Total number of deduplicated events emitted
- **`delta_emitted`**: Number of events emitted during the last interval
- **`eps`**: Events per second received during the last interval

**Example deduplication statistics log:**

```json
[MISSING-REPORT] [{"partition":0,"total_missing":15,"delta_missing":2,"total_received":72000,"delta_received":3600,"total_emitted":71985,"delta_emitted":3598,"eps":60}]
```

### Using Statistics for Monitoring

Statistics can be used to:

1. **Monitor throughput**: Track `eps` to ensure the system is processing events at the expected rate
2. **Detect packet loss**: Compare `total_sent` (upstream) with `total_received` (downstream)
3. **Identify gaps**: Monitor `total_missing` and `delta_missing` in deduplication logs
4. **Calculate uptime**: Use `time - time_start` to determine how long the application has been running
5. **Verify deduplication**: Compare `total_received` vs `total_emitted` to see how many duplicates were filtered

### Parsing Statistics Logs

You can extract and analyze statistics using standard log processing tools. For example, to extract statistics from logs:

```bash
grep "STATISTICS:" /var/log/airgap/upstream.log | jq .
```

Or to monitor gaps in real-time:

```bash
tail -f /var/log/airgap/dedup.log | grep "MISSING-REPORT" | jq '.[0].total_missing'
```

For more comprehensive monitoring solutions using Metricbeat and Jolokia, see `doc/Monitoring.md`.
