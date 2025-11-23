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

### Upstream

Edit your upstream config file (e.g., `config/upstream.properties`):

```bash
id=Upstream_1
nic=en0
targetIP=127.0.0.1
targetPort=1234
source=kafka
bootstrapServers=192.168.153.138:9092
topic=transfer
groupID=test
publicKeyFile=certs/server2.pem
generateNewSymmetricKeyEvery=500
mtu=auto
```

Override any setting with environment variables (see README for details).

### Downstream

Edit your downstream config file (e.g., `config/downstream.properties`):

```bash
id=Downstream_1
nic=en0
targetIP=0.0.0.0
targetPort=1234
bootstrapServers=192.168.153.138:9092
topic=log
privateKeyFiles=certs/private*.pem
target=kafka
mtu=auto
clientId=downstream
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