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

---

## Understanding Packet Drops and Loss Detection

The air-gap system provides multiple layers of monitoring to detect packet loss and performance issues. Understanding where packets can be dropped and what each metric measures is critical for proper monitoring and troubleshooting.

### The Packet Drop Problem

Packets can be dropped at multiple points in the network stack. No single metric captures all possible drop locations. A comprehensive monitoring strategy requires tracking drops at each layer and correlating the results.

### SO_RXQ_OVFL: What It Measures (and What It Doesn't)

The downstream application can optionally enable `SO_RXQ_OVFL` socket monitoring (Linux only), which exposes two statistics:

- **`SO_RXQ_OVFL`** - Socket receive queue drops since last statistics printout (resets each interval)
- **`SO_RXQ_OVFL_TOTAL`** - Total socket receive queue drops since application start

**What SO_RXQ_OVFL Measures:**

- ✅ Packets that reached the application's socket but were dropped because the socket receive buffer was full
- ✅ Application unable to read from socket fast enough
- ✅ `SO_RCVBUF` (receive buffer size) too small for the workload

**What SO_RXQ_OVFL Does NOT Measure:**

- ❌ Drops at the physical NIC (network interface card)
- ❌ Drops in the kernel's network stack before reaching the socket
- ❌ Drops at the driver level (e1000, ixgbe, virtio-net, etc.)
- ❌ Drops in VM hypervisor virtual switches (in virtualized environments)
- ❌ Drops at the host NIC when VMs share the host's physical network
- ❌ Actual network drops between physical machines
- ❌ Upstream sending fewer messages than expected

**Critical Limitation:** In virtualized environments (VMware, KVM, VirtualBox), SO_RXQ_OVFL will show **zero drops** even if packets are lost at the host NIC, virtual switch, or VM NIC driver. This can create false confidence that the air-gap will work in production when testing between VMs on the same host.

### Where Packets Can Be Dropped

Here's the complete packet path and where drops can occur:

```text

[Upstream Kafka]
    ↓
[Upstream Application]
    ├─→ messages_dropped counter (failed after retries)
    ↓
[Upstream NIC] ← TX drops (ethtool -S)
    ↓
═══ Network/Air-Gap ═══
    ↓
[Downstream NIC] ← RX drops (ip -s link / ethtool -S)  ⚠️ SO_RXQ_OVFL doesn't see these
    ↓
[Kernel Network Stack] ← Stack drops (netstat -s)  ⚠️ SO_RXQ_OVFL doesn't see these
    ↓
[Socket Receive Queue] ← SO_RXQ_OVFL measures HERE ✓
    ↓
[Downstream Application]
    ├─→ received counter (UDP packets received)
    ├─→ sent counter (Kafka messages sent)
    ↓
[Downstream Kafka]
    ↓
[Deduplicator]
    └─→ Gap detection (END-TO-END validation) ✓✓✓
```

### Multi-Layer Monitoring Strategy

To get the complete picture of packet loss, monitor at **all layers**:

#### 1. End-to-End Validation (Most Reliable)

**Deduplicator Gap Detection** is your ground truth:

- Tracks sequence IDs from upstream through the entire pipeline
- Detects missing messages regardless of where they were dropped
- Check JMX metrics: `topicname_X_nrMissing`, `topicname_X_gaps`

**Upstream Message Tracking**:

```json
{
  "received": 1000,              // Kafka messages received from source
  "messages_sent": 1000,         // Kafka messages successfully transmitted
  "messages_dropped": 0,         // Messages failed after all retries
  "sent": 3000,                  // UDP fragments sent (may be > messages_sent)
  "total_messages_sent": 38167418,    // Should match sequence numbers
  "total_messages_dropped": 42   // Cumulative drops indicate transport issues
}
```

**Key Insight:** If `total_messages_sent` doesn't match your sequence numbers, or `messages_dropped` is non-zero, upstream has transport problems.

#### 2. Application-Level Statistics

**Downstream Statistics**:

```json
{
  "received": 1000,              // UDP packets received by application
  "sent": 1000,                  // Kafka messages sent downstream
  "SO_RXQ_OVFL": 0,              // Socket queue drops THIS interval
  "SO_RXQ_OVFL_TOTAL": 0,        // Socket queue drops since start
  "total_received": 2155837,
  "total_sent": 2155837
}
```

**Expected Behavior:**

- `received` = `sent` → Application processing all received packets
- `SO_RXQ_OVFL` = 0 → Socket buffer not overflowing
- `total_received` ≈ `upstream.total_messages_sent` → No transport drops

**Problem Indicators:**

- `received` < `upstream.messages_sent` → Drops between applications
- `SO_RXQ_OVFL` > 0 → Application can't keep up with socket reading
- Large gap between `total_received` and `upstream.total_messages_sent` → Systematic drops

#### 3. Network Interface Statistics

**Check NIC-level drops (Linux):**

```bash
# Quick view - shows RX/TX drops
ip -s link show eth0

# Example output:
# RX: bytes  packets  errors  dropped  overrun  mcast
#     1234567 987654   0       42       0        0
#                              ^^^
#                              Drops at NIC level!

# Detailed driver statistics
ethtool -S eth0 | grep -i drop

# Example output:
# rx_dropped: 42
# rx_queue_0_drops: 15
# rx_queue_1_drops: 27
```

**Monitor these in production:**

- `dropped` in `ip -s link` output
- `rx_dropped`, `rx_queue_X_drops` in `ethtool -S` output
- These indicate kernel or driver dropped packets **before** they reach the socket

#### 4. Kernel Network Stack Statistics

```bash
# Check for UDP buffer overflows and other stack-level drops
netstat -s -u

# Look for:
# - packet receive errors
# - receive buffer errors
# - InErrors
```

#### 5. VM-Specific Monitoring (Virtualized Environments)

When testing in VMs:

**Host-Level Checks:**

```bash
# Check host NIC statistics
ip -s link show <host_bridge_interface>
ethtool -S <host_nic>

# For VMware ESXi:
esxtop  # then 'n' for network view

# For KVM/libvirt:
virsh domifstat <vm_name> <interface>
```

**Virtual Switch Statistics:**

```bash
# For Linux bridge:
brctl show
ip -s link show br0

# For Open vSwitch:
ovs-vsctl show
ovs-ofctl dump-ports br0
```

**VM NIC Driver Statistics:**

```bash
# Inside VM - check virtual NIC drops
ethtool -S eth0 | grep drop
dmesg | grep -i "network\|eth0" | tail -20
```

### Interpreting Combined Metrics

Here are common scenarios and what they mean:

#### Scenario 1: SO_RXQ_OVFL = 0, No Gaps

✅ **Healthy system** - packets flowing correctly

#### Scenario 2: SO_RXQ_OVFL > 0, Gaps Detected

❌ **Application bottleneck** - downstream can't read from socket fast enough

- **Fix:** Increase `numReceivers`, tune `SO_RCVBUF`, or reduce upstream EPS

#### Scenario 3: SO_RXQ_OVFL = 0, Gaps Detected

⚠️ **Drops before socket** - packets lost at network/NIC/kernel level

- Check `ip -s link show` for RX drops
- Check `ethtool -S` for driver drops
- Check `netstat -s -u` for kernel drops
- In VMs: Check host NIC and virtual switch

#### Scenario 4: Upstream messages_dropped > 0

❌ **Upstream can't reach downstream** - transport layer failure

- Network down or downstream application not running
- Check `transport_status` field in upstream statistics
- Verify network connectivity: `ping`, `traceroute`

#### Scenario 5: Large Gap Between Sequence and total_received

🔍 **Investigation needed:**

```bash
# Upstream sequence: 38167418
# Downstream total_received: 2155837
# Gap: ~36 million messages

# Possible causes:
# 1. Upstream restarted → counters reset, Kafka offset continued
# 2. Multiple upstream instances → only one being monitored
# 3. Dedup state corruption → false gap detection
# 4. Historical drops → check total_messages_dropped
```

### Special Case: VMware and Hypervisor Packet Loss

#### Critical: SO_RXQ_OVFL Cannot Detect VMware/Hypervisor Drops

When running in virtualized environments (VMware, KVM, VirtualBox, Hyper-V), packet drops can occur in the **hypervisor virtual networking layer** that are **completely invisible** to the guest operating system. This creates a dangerous false-positive situation where all your Linux-based metrics show zero drops, but packets are still being lost.

#### How to Detect VMware/Hypervisor Drops

##### Step 1: Compare Upstream vs Downstream Totals

The only reliable way to detect hypervisor drops is to compare end-to-end message counts:

```bash
# Get upstream total messages sent (from latest statistics)
grep "STATISTICS" /var/log/airgap/upstream.log | tail -1 | jq '.total_messages_sent'
# Output: 270531

# Get downstream total received
grep "STATISTICS" /var/log/airgap/downstream.log | tail -1 | jq '.total_received'
# Output: 251576

# Calculate loss
# Loss = 270531 - 251576 = 18955 messages (7% loss)
```

##### Step 2: Verify Linux Stack is Clean

If you see message loss but all Linux metrics are clean, it's hypervisor drops:

```bash
# On downstream machine:

# Check SO_RXQ_OVFL (should be 0 if hypervisor drops)
grep "SO_RXQ_OVFL_TOTAL" /var/log/airgap/downstream.log | tail -1
# Output: "SO_RXQ_OVFL_TOTAL": 0

# Check NIC drops (should be 0 if hypervisor drops)
ethtool -S eth0 | grep -i drop
# Output: tx_dropped: 0, rx_dropped: 0

# Check kernel UDP stack (should be clean if hypervisor drops)
netstat -s | grep -E "packet receive errors|receive buffer errors"
# Output: 0 packet receive errors, 0 receive buffer errors

# Check socket-level drops (should be 0 if hypervisor drops)
cat /proc/net/udp | awk '{print $13}' | awk '{sum+=$1} END {print sum}'
# Output: 0
```

If all metrics above show ZERO but upstream sent more than downstream received, the drops are occurring in the hypervisor layer.

#### Diagnostic Checklist for VMware

```bash
# 1. Verify you're in a VM
hostnamectl | grep Virtualization
lspci | grep -i vmware
cat /sys/class/dmi/id/product_name
# Look for: VMware, VirtualBox, KVM, etc.

# 2. Check virtual NIC type (VMware)
ethtool -i eth0
# driver: vmxnet3 (best), e1000 (legacy, limited), etc.

# 3. Check shared folders (indicates VM with shared host filesystem)
mount | grep hgfs  # VMware
mount | grep vboxsf  # VirtualBox
# If found: You're definitely in a VM

# 4. Compare message rates
# Upstream: 5600 messages/sec × 1500 bytes = 8.4 MB/sec = 67 Mbps
# If you see 10%+ loss at this rate, likely hitting VM network limits
```

#### Why VMware Drops Are Invisible to Linux

```text
┌─────────────────────────────────────────────────┐
│           [Hypervisor Host OS]                  │
│                                                 │
│  ┌───────────────────────────────────────┐     │
│  │        Virtual Switch (vSwitch)       │ ◄─── DROPS HERE (invisible to guest)
│  │        - Limited TX/RX queues         │
│  │        - Shared with other VMs        │
│  │        - Buffer overflows in bursts   │
│  └───────────────┬───────────────────────┘     │
│                  │                              │
│  ┌───────────────┴───────────────────────┐     │
│  │    [VM Guest - Downstream Linux]      │     │
│  │                                        │     │
│  │   ethtool -S ────► Shows 0 drops  ✗   │ ◄─── Cannot see hypervisor drops
│  │   netstat -s ────► Shows 0 drops  ✗   │
│  │   SO_RXQ_OVFL ───► Shows 0 drops  ✗   │
│  │   /proc/net/udp ─► Shows 0 drops  ✗   │
│  │                                        │
│  │   Only total_received shows truth! ✓  │
│  └────────────────────────────────────────┘     │
└─────────────────────────────────────────────────┘
```

#### Solutions for VMware Environments

##### 1. Reduce Send Rate (Test if it's a bottleneck)

```properties
# upstream.properties
eps=3000  # Start at half your current rate
```

If loss drops to 0%, you've confirmed VM networking is the bottleneck.

##### 2. Use Better Virtual NIC Drivers

```bash
# Check current driver
ethtool -i eth0 | grep driver

# VMware: Use VMXNET3 (paravirtualized, best performance)
# Avoid: E1000, E1000E (legacy, limited buffers)

# Change in VM settings → Network Adapter → Adapter Type → VMXNET3
```

##### 3. Tune Virtual NIC Settings (requires ESXi/host access)

```bash
# On ESXi host (requires SSH access):
esxcli network nic ring current get -n vmnic0
esxcli network nic ring current set -n vmnic0 -r 4096 -t rx

# Increase VM vNIC receive queues
```

##### 4. Pin VM to Dedicated Resources

- Give VM dedicated CPU cores (avoid overcommit)
- Set CPU reservations in VMware
- Enable CPU pinning / affinity

##### 5. Test on Physical Hardware

The **only** way to eliminate VM networking as a variable:

- Deploy both upstream and downstream on bare metal servers
- Use actual network air-gap hardware (data diode)
- Measure true production capacity

##### 6. Accept and Monitor

If you must run in VMs:

- **Accept 5-10% loss as normal** for VM networking
- **Monitor via upstream vs downstream totals** (not SO_RXQ_OVFL)
- **Rely on deduplicator gap detection** (this is why the deduplicator was built!)
- **Alert on loss rate > 15%** (indicates worsening conditions)

#### VMware Monitoring Strategy

```yaml
# DON'T trust in VMs:
- SO_RXQ_OVFL: 0          # ✗ Meaningless in VMs
- ethtool drops: 0        # ✗ Meaningless in VMs
- netstat UDP errors: 0   # ✗ Meaningless in VMs

# DO monitor in VMs:
- upstream.total_messages_sent vs downstream.total_received  # ✓ Ground truth
- loss_rate = (sent - received) / sent                       # ✓ Real loss
- deduplicator gap counts                                    # ✓ End-to-end validation
- transport_status field                                     # ✓ Application health
```

#### Example: Detecting VMware Drops

```bash
#!/bin/bash
# vmware-drop-detector.sh - Compare upstream vs downstream

# Get totals from last statistics
UP_SENT=$(grep "STATISTICS" /var/log/airgap/upstream.log | tail -1 | jq -r '.total_messages_sent')
DOWN_RECV=$(grep "STATISTICS" /var/log/airgap/downstream.log | tail -1 | jq -r '.total_received')
SO_RXQ=$(grep "STATISTICS" /var/log/airgap/downstream.log | tail -1 | jq -r '.SO_RXQ_OVFL_TOTAL')

# Calculate loss
LOSS=$((UP_SENT - DOWN_RECV))
LOSS_PERCENT=$(awk "BEGIN {printf \"%.2f\", ($LOSS / $UP_SENT) * 100}")

echo "Upstream sent:     $UP_SENT"
echo "Downstream recv:   $DOWN_RECV"
echo "Loss:              $LOSS ($LOSS_PERCENT%)"
echo "SO_RXQ_OVFL_TOTAL: $SO_RXQ"

# Determine drop location
if [ "$LOSS" -gt 1000 ] && [ "$SO_RXQ" -eq 0 ]; then
    echo ""
    echo "⚠️  HYPERVISOR DROPS DETECTED!"
    echo "   - Linux stack shows 0 drops (SO_RXQ_OVFL=$SO_RXQ)"
    echo "   - But $LOSS messages lost ($LOSS_PERCENT%)"
    echo "   - Drops occurring in VMware/hypervisor layer"
    echo ""
    echo "Verify with:"
    echo "  - ethtool -S eth0 | grep drop  (should be 0)"
    echo "  - netstat -s | grep 'receive buffer errors'  (should be 0)"
    echo ""
    echo "Solutions:"
    echo "  1. Test on physical hardware"
    echo "  2. Reduce upstream EPS"
    echo "  3. Use VMXNET3 virtual NIC"
    echo "  4. Add CPU/memory resources to VM"
fi
```

**Key Takeaway:** In virtualized environments, **SO_RXQ_OVFL=0 does NOT mean "no packet loss"**. Always compare `upstream.total_messages_sent` vs `downstream.total_received` as your ground truth for packet loss detection.

### Configuration for Comprehensive Monitoring

**Enable SO_RXQ_OVFL in Downstream** (optional, has small performance overhead):

```properties
# config/downstream.properties
enableRxqOvfl=true
logStatistics=10
```

**Enable Statistics in Upstream:**

```properties
# config/upstream.properties
logStatistics=10
```

**Monitor NIC Statistics with systemd timer:**

```bash
# Create /usr/local/bin/log-nic-stats.sh
#!/bin/bash
echo "=== $(date) ===" >> /var/log/airgap-nic-stats.log
ip -s link show eth0 >> /var/log/airgap-nic-stats.log
ethtool -S eth0 | grep -i drop >> /var/log/airgap-nic-stats.log

# Create systemd timer to run every minute
sudo systemctl enable --now nic-stats.timer
```

**Parse Statistics in Monitoring System:**

```bash
# Extract key metrics from JSON logs
jq -r '[.time, .id, .total_received, .total_sent, .SO_RXQ_OVFL_TOTAL, .messages_dropped] | @csv' \
   < /var/log/airgap-downstream.log

# Alert on gaps
jq 'select(.SO_RXQ_OVFL > 100 or .messages_dropped > 0)' \
   < /var/log/airgap-*.log
```

### Production Monitoring Checklist

- [ ] **Deduplicator JMX metrics** - `nrMissing`, `gaps` per partition (END-TO-END validation)
- [ ] **Upstream statistics** - `messages_sent`, `messages_dropped`, `total_messages_dropped`
- [ ] **Downstream statistics** - `received`, `sent`, `SO_RXQ_OVFL_TOTAL`
- [ ] **NIC statistics** - `ip -s link` RX/TX drops every minute
- [ ] **Driver statistics** - `ethtool -S` drop counters
- [ ] **Kernel statistics** - `netstat -s -u` UDP errors
- [ ] **Transport status** - `transport_status` field = "running"
- [ ] **Kafka status** - `kafka_status` field = "running"
- [ ] **VM host metrics** (if applicable) - Host NIC and vSwitch drops

### Recommended Alert Thresholds

```yaml
# Alert if any of these conditions occur:
- SO_RXQ_OVFL_TOTAL increases by > 100 in 1 minute
- messages_dropped > 0 in upstream
- NIC RX drops increasing (ip -s link)
- Deduplicator nrMissing > 0 for any partition
- transport_status != "running"
- Gap between upstream.total_messages_sent and downstream.total_received growing > 1000/min
```

### Testing Recommendations

**Don't rely solely on SO_RXQ_OVFL when testing:**

1. **Test with real network separation** - VMs on different physical hosts
2. **Add artificial network stress:**

   ```bash
   # Simulate network delay/loss in downstream VM
   sudo tc qdisc add dev eth0 root netem loss 0.1% delay 10ms
   ```

3. **Monitor ALL layers** - NIC, kernel, socket, application, end-to-end
4. **Generate sustained high load** - Use upstream `eps` setting to test capacity
5. **Verify deduplicator gap detection** - This is your ground truth for packet loss

**Remember:** A clean test in VMs on the same host does NOT guarantee the air-gap hardware will work. Physical network testing with multi-layer monitoring is essential.

---

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
