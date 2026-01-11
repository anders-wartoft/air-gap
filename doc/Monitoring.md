# Monitoring upstream, downstream and dedup with Metricbeat

In a production environment, jconsole is not availabe if you run the deduplicator as a service. You can collect metrics with the following guide, where only the system metrics are available for the upstream and downstream but the deduplicator exports some attributes to JMX and can be inspected with Jolokia.

## 0. Monitoring Transport Status

Upstream and downstream applications continuously monitor transport (UDP/TCP) status and report it in periodic statistics.

### Statistics with Transport Status

The statistics output includes a `status` field that shows the current transport state:

```json
{
  "id": "Upstream_1",
  "time": 1704811200,
  "received": 150,
  "sent": 150,
  "eps": 150,
  "status": "running",
  "total_sent": 45000,
  ...
}
```

**Status values:**

- `"running"` - Transport is operational and messages are being delivered
- `"<error message>"` - Transport error (e.g., "connection refused", "connection reset")

### Log Monitoring

Transport status changes are logged at appropriate levels:

**ERROR level** - Status changes to error:

```bash
[ERROR] Transport status changed to error: connection refused (was: running)
```

**INFO level** - Status restored:

```bash
[INFO] Transport status restored to running (was: connection refused)
```

**WARN level** (UDP only) - Messages sent after transient retries:

```bash
[WARN] Message id=transfer_3_0 sent after 2 attempt(s) - UDP delivery is not guaranteed (receiver may be down)
```

### Setting Up Alerts

To alert on transport failures, monitor for:

1. ERROR level logs containing "Transport status changed to error"
2. Statistics with `status` field not equal to "running"
3. Frequent WARN messages about UDP delivery uncertainty

## 1. Install Metricbeat

Follow the [official Elastic documentation](https://www.elastic.co/guide/en/beats/metricbeat/current/metricbeat-installation.html) for your OS, or on Fedora/RHEL:

```sh
sudo rpm --import https://artifacts.elastic.co/GPG-KEY-elasticsearch
cat <<EOF | sudo tee /etc/yum.repos.d/elastic.repo
[elastic-8.x]
name=Elastic repository for 8.x packages
baseurl=https://artifacts.elastic.co/packages/8.x/yum
gpgcheck=1
gpgkey=https://artifacts.elastic.co/GPG-KEY-elasticsearch
enabled=1
autorefresh=1
type=rpm-md
EOF
sudo dnf install metricbeat
```

## 2. Enable Metricbeat Modules

### System Metrics (CPU, memory, etc.)

```sh
sudo metricbeat modules enable system
```

### Deduplicator (JMX) Metrics via Jolokia

The deduplicator already exposes JMX methods to JConsole for monitoring (you can also purge the gaps). Those methods are also available to monitor with Jolokia.

1. **Download Jolokia agent:**
   - [Jolokia Releases](https://jolokia.org/download.html)

2\. **Change the .service file to start the Java app with Jolokia agent:**

```sh
java -javaagent:/path/to/jolokia-agent.jar=port=8778,host=127.0.0.1 -jar /path/to/air-gap/air-gap-deduplication-fat-<version>.jar
```

3\. **Enable and configure the Jolokia module:**

```sh
sudo metricbeat modules enable jolokia
sudo vi /etc/metricbeat/modules.d/jolokia.yml
```

Example config:

```yaml
- module: jolokia
  metricsets: [jmx]
  hosts: ["http://localhost:8778/jolokia"]
  namespace: "dedup"
  jmx.mappings:
    - mbean: 'java.lang:type=Memory'
      attributes:
        - attr: HeapMemoryUsage
          field: memory.heap_usage
- mbean: 'nu.sitia.airgap:partition=3,type=GapDetectors'
  attributes:
  - attr: topicname_3_gaps
    field: partition_3_gaps
  - attr: topicname_3_nrWindows
    field: partition_3_nrWindows
  - attr: topicname_3_nrMissing
    field: partition_3_nrMissing
  - attr: topicname_3_mem
    field: partition_3_mem
- mbean: 'nu.sitia.airgap:partition=1,type=GapDetectors'
  attributes:
  - attr: topicname_1_gaps
    field: partition_1_gaps
  - attr: topicname_1_nrWindows
    field: partition_1_nrWindows
  - attr: topicname_1_nrMissing
    field: partition_1_nrMissing
  - attr: topicname_1_mem
    field: partition_1_mem
- mbean: 'nu.sitia.airgap:type=Props'
  attributes:
  - attr: WINDOW_SIZE
    field: window_size
  - attr: MAX_WINDOWS
    field: max_windows
  - attr: GAP_EMIT_INTERVAL_SEC
    field: gap_emit_interval_sec
  - attr: RAW_TOPICS
    field: raw_topics
  - attr: assignedRawPartitions
    field: assigned_raw_partitions
```

If one common .service-file is used to start several instances of dedup with differnt .env files, make the following changes:

- For each .env-file, add `JAVA_OPTS=-javaagent:/opt/airgap/dedup/jolokia-agent.jar=port=8778,host=0.0.0.0`. Set the port and ip so they don't collide.
- Change the .service-file ExecStart to (set the version of the deduplication jar to the one you are using):

```sh
ExecStart=/usr/bin/java $JAVA_OPTS -jar /opt/airgap/dedup/air-gap-deduplication-fat-0.1.3-SNAPSHOT.jar
```

Now, each instance of the deduplicator will have it's own listening port.

The deduplicator will report how many windows each partition has when queried, as well as an estimate of how many bytes RAM the windows are currently using. The estimation is just an estimate but can be useful to spot trends in memory consumption. The application as a whole can be monitored for RAM usage by the system metrics for that process.

### Go Application Metrics

- Upstream and downstream are Go apps that may expose Prometheus metrics. The metrics that would be interesting are current load and number of processed events. That is a small win for more complexity. The most important metrics to observe are memory and processor consumption and those are handled by the system metrics module.

For now, no metrics will be exported from the Go applications to Prometheus.

## 3. Configure Metricbeat Output

Edit `/etc/metricbeat/metricbeat.yml` to send data to Elasticsearch or Logstash.

## 4. Start and Enable Metricbeat

```sh
sudo systemctl enable --now metricbeat
```

## 5. View Metrics

- Use Kibana or your preferred dashboard to visualize metrics.

---

### Using JMX Methods from JmxSupport.java in PartitionDedupApp

The PartitionDedupApp exposes runtime information and operations via JMX, accessible through JConsole, Jolokia, or any JMX client.

#### What is Exposed?

- **GapDetectors MBean** (per partition):
  - Each partition is registered as its own MBean:
    - `nu.sitia.airgap:partition=3,type=GapDetectors`
    - `nu.sitia.airgap:partition=1,type=GapDetectors`
    - ...etc.
  - For each partition MBean, exposes:
    - `topicname_3_gaps`, `topicname_3_nrWindows`, `topicname_3_nrMissing`, `topicname_3_mem`, etc. (for partition 3)
    - `topicname_1_gaps`, `topicname_1_nrWindows`, `topicname_1_nrMissing`, `topicname_1_mem`, etc. (for partition 1)
    - Operations: `getAllGaps_topicname_3`, `purge_topicname_3`, `getAllGaps_topicname_1`, `purge_topicname_1`, etc.
- **Props MBean** (`nu.sitia.airgap:type=Props`):
  - Kafka Streams properties, runtime config, assigned partitions, topics, window size, etc.

#### How to Use

- **With JConsole:**
  1. Start your app with JMX enabled (or with Jolokia for remote HTTP access).
  2. Open JConsole and connect to the running JVM.
  3. Browse to `nu.sitia.airgap -> GapDetectors` or `Props` to view attributes and invoke operations.
- **With Jolokia (for Metricbeat):**
  - Jolokia exposes these MBeans over HTTP. Metricbeat can be configured to scrape specific attributes or call operations.

#### Example Jolokia Query (HTTP API)

To call an operation (e.g., get all gaps for partition 3):

```sh
curl -X POST http://localhost:8778/jolokia/ \
  -H 'Content-Type: application/json' \
  -d '{"type":"exec","mbean":"nu.sitia.airgap:partition=3,type=GapDetectors","operation":"getAllGaps_topicname_3"}'
```

To read an attribute (examples):

```sh
curl http://localhost:8778/jolokia/read/nu.sitia.airgap:partition=3,type=GapDetectors/topicname_3_gaps
curl http://localhost:8778/jolokia/read/nu.sitia.airgap:partition=1,type=GapDetectors/topicname_1_gaps
curl http://localhost:8778/jolokia/read/nu.sitia.airgap:type=Props/WINDOW_SIZE
curl http://localhost:8778/jolokia/list/nu.sitia.airgap
```

#### Example Metricbeat Mapping

Add to your `jolokia.yml`:

```yaml
- module: jolokia
  metricsets: [jmx]
  hosts: ["http://localhost:8778/jolokia"]
  namespace: "airgap"
  jmx.mappings:
    - mbean: 'nu.sitia.airgap:partition=3,type=GapDetectors'
      attributes:
        - attr: topicname_3_gaps
          field: partition_3_gaps
        - attr: topicname_3_nrWindows
          field: partition_3_nrWindows
        - attr: topicname_3_nrMissing
          field: partition_3_nrMissing
        - attr: topicname_3_mem
          field: partition_3_mem
    - mbean: 'nu.sitia.airgap:partition=1,type=GapDetectors'
      attributes:
        - attr: topicname_1_gaps
          field: partition_1_gaps
        - attr: topicname_1_nrWindows
          field: partition_1_nrWindows
        - attr: topicname_1_nrMissing
          field: partition_1_nrMissing
        - attr: topicname_1_mem
          field: partition_1_mem
    - mbean: 'nu.sitia.airgap:type=Props'
      attributes:
        - attr: WINDOW_SIZE
          field: window_size
        - attr: MAX_WINDOWS
          field: max_windows
        - attr: GAP_EMIT_INTERVAL_SEC
          field: gap_emit_interval_sec
        - attr: RAW_TOPICS
          field: raw_topics
        - attr: assignedRawPartitions
          field: assigned_raw_partitions
```

**Tip:** Use the Jolokia `/list` endpoint to discover available attributes and operations for your running instance. Only partitions/topics assigned to the current deduplicator instance will appear. If you get “attribute not found” errors, check the exact attribute/operation name and partition assignment in the list output.

---

**References:**

- [Metricbeat Jolokia module](https://www.elastic.co/guide/en/beats/metricbeat/current/metricbeat-module-jolokia.html)
- [Metricbeat Prometheus module](https://www.elastic.co/guide/en/beats/metricbeat/current/metricbeat-module-prometheus.html)
- [Jolokia documentation](https://jolokia.org/)
- [Prometheus client for Go](https://prometheus.io/docs/guides/go-application/)
