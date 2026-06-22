# Frequently Asked Questions

## UDP broadcast is dropping packets in VMware/virtual environments - what can I do?

UDP broadcast in virtualized environments (VMware, KVM, VirtualBox, Hyper-V) is particularly problematic because:

1. **Broadcast amplification**: One broadcast packet is replicated to every VM on the virtual switch
2. **vSwitch buffer constraints**: Virtual switches have smaller buffers than physical switches
3. **Shared bandwidth**: All VMs compete for the host's physical NIC capacity
4. **CPU overhead**: More interrupt processing on the hypervisor for broadcast replication

### Beyond Lowering EPS: 10 Optimization Strategies

#### 1. Enable Compression (High Impact)

Compress large messages to reduce bandwidth and packet count:

```properties
# upstream.properties - compress messages > 1KB
compressWhenLengthExceeds=1024
```

**Impact**: Can reduce bandwidth by 50-80% for text-heavy payloads (JSON, XML, logs). Gzip compression is CPU-cheap on modern hardware.

#### 2. Increase Downstream Receive Buffers (High Impact)

Increase OS-level socket buffer to absorb burst traffic:

```properties
# downstream.properties - increase from default 4MB to 16MB
rcvBufSize=16777216

# Also increase application-level buffers
channelBufferSize=32768          # default: 16384
readBufferMultiplier=32          # default: 16
```

**Also increase system limits:**

```bash
# Increase system-wide UDP receive buffer limits
sudo sysctl -w net.core.rmem_max=33554432      # 32MB
sudo sysctl -w net.core.rmem_default=16777216  # 16MB

# Make permanent
echo "net.core.rmem_max=33554432" | sudo tee -a /etc/sysctl.conf
echo "net.core.rmem_default=16777216" | sudo tee -a /etc/sysctl.conf
sudo sysctl -p
```

#### 3. Increase Number of Receiver Threads (Medium Impact)

More threads = better CPU utilization and faster packet processing:

```properties
# downstream.properties - increase from default 10 to 20
numReceivers=20
```

**Note**: With broadcast, you'll need firewall DNAT rules to avoid duplication (see README.md "Broadcast Duplication" section).

#### 4. Enable SO_RXQ_OVFL Monitoring (Diagnostic)

At least know where drops are happening:

```properties
# downstream.properties
enableRxqOvfl=true
logStatistics=10
```

Compare SO_RXQ_OVFL (socket drops) vs end-to-end message loss to identify if drops are in socket, kernel, or hypervisor. See [Monitoring.md](Monitoring.md#understanding-packet-drops-and-loss-detection) for detailed drop analysis strategies.

#### 5. Use Better Virtual NIC Drivers (Medium Impact)

Switch from legacy E1000 to paravirtualized drivers:

**VMware**: Use VMXNET3 (VM settings → Network Adapter → Adapter Type → VMXNET3)

**KVM**: Use VirtIO (best performance)

**VirtualBox**: Use Paravirtualized Network (virtio-net)

**Verify current driver:**

```bash
ethtool -i eth0 | grep driver
# Good: vmxnet3, virtio_net
# Bad: e1000, e1000e (legacy, limited queues)
```

#### 6. Pin VMs to Dedicated CPU Cores (Medium Impact)

Avoid CPU contention from other VMs:

**VMware**: VM settings → CPU → Enable CPU affinity

**KVM/libvirt**: Edit VM XML:

```xml
<vcpu placement='static' cpuset='0-3'>4</vcpu>
<cputune>
  <vcpupin vcpu='0' cpuset='0'/>
  <vcpupin vcpu='1' cpuset='1'/>
  <vcpupin vcpu='2' cpuset='2'/>
  <vcpupin vcpu='3' cpuset='3'/>
</cputune>
```

#### 7. Increase VM NIC Queue Depth (Advanced, VMware only)

Requires ESXi host SSH access:

```bash
# On ESXi host
esxcli network nic ring current get -n vmnic0
esxcli network nic ring current set -n vmnic0 -r 4096 -t rx

# For VM vNIC (requires vSphere API or host editing)
# Edit VM's .vmx file:
ethernet0.pciSlotNumber = "160"
ethernet0.virtualDev = "vmxnet3"
ethernet0.rxRingSize = "4096"
ethernet0.txRingSize = "4096"
```

#### 8. Use DSCP/TOS QoS Marking (Advanced)

Mark UDP packets for priority handling (requires network infrastructure support):

Currently **not implemented** - would require adding socket option in `src/udp/sender.go`:

```go
// Future enhancement: IP_TOS / IPV6_TCLASS socket option
syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TOS, 0xb8) // EF (expedited forwarding)
```

#### 9. Switch to TCP for Production (Alternative)

If UDP broadcast drops are unacceptable:

```properties
# upstream.properties
transport=tcp
targetIP=192.168.1.100  # Direct IP, no broadcast

# downstream.properties  
transport=tcp
```

**Tradeoffs:**

- ✅ Reliable delivery, no drops
- ✅ Works through any diode that passes TCP
- ❌ Requires knowing downstream IP (defeats broadcast purpose)
- ❌ Single connection, no load balancing

#### 10. Deploy Redundant Upstreams (High Availability)

Multiple upstream instances on different physical hosts, sending to same topic:

```properties
# upstream-1.properties (physical host A)
id=Upstream_1
targetIP=255.255.255.255
nic=eth0

# upstream-2.properties (physical host B)
id=Upstream_2
targetIP=255.255.255.255
nic=eth0
```

Deduplicator removes duplicates. Even if one upstream drops 10%, combined delivery approaches 99%+.

### Recommended Configuration for Broadcast in VMs

Start with this optimized configuration:

```properties
# upstream.properties
id=Upstream_Broadcast
transport=udp
nic=eth0
targetIP=255.255.255.255
targetPort=1234
compressWhenLengthExceeds=1024    # Enable compression
eps=3000                          # Start conservative, increase gradually
logStatistics=10

# downstream.properties
id=Downstream_Broadcast  
transport=udp
targetIP=0.0.0.0
targetPort=1234
numReceivers=20                    # More threads
channelBufferSize=32768            # Larger channel buffer
readBufferMultiplier=32            # Larger read buffer
rcvBufSize=16777216                # 16MB socket buffer
enableRxqOvfl=true                 # Monitor drops
logStatistics=10
```

**Plus system tuning:**

```bash
# Increase system UDP buffers
sudo sysctl -w net.core.rmem_max=33554432
sudo sysctl -w net.core.rmem_default=16777216
sudo sysctl -p
```

### Expected Results

With these optimizations:

- Compression: 50-80% bandwidth reduction (depends on data)
- Larger buffers: Absorb 2-3x burst traffic
- More receivers: 1.5-2x throughput improvement
- VMXNET3: 20-40% better performance vs E1000

**Realistic expectations**: In constrained VM environments with broadcast, expect 5-10% packet loss even with optimizations. The deduplicator is designed to handle this - it will detect gaps and trigger resend.

### Monitoring Success

Track these metrics to validate improvements:

```bash
# Monitor loss rate over time
./tools/dev/vmware-drop-detector.sh /var/log/airgap/upstream.log /var/log/airgap/downstream.log

# Watch for socket drops (should be 0 if hypervisor is bottleneck)
grep SO_RXQ_OVFL /var/log/airgap/downstream.log | tail -5

# Check gap detection
# JMX: topicname_X_nrMissing should be small and stable
```

**Success criteria**:

- Loss rate < 5% (down from 10-20% without optimization)
- SO_RXQ_OVFL_TOTAL = 0 (no socket drops)
- Deduplicator gap count stable or slowly growing (not rapidly increasing)
- No "Fragment cache full" warnings in downstream logs

## The deduplication only moves messages from the input topic to the output topic, but doesn't remove duplicates

This is usually explained by the third deduplication case in the algorithm:

- offset is lower than `nextExpectedId`
- but the offset is still inside a known gap

In that situation, the record is **not** treated as a duplicate. It is treated as a previously missing event that arrived late (for example due to UDP loss/reorder and later resend), so it is forwarded to the clean topic.

In other words, deduplication removes records that were already delivered before, but it intentionally accepts late gap-fill events.

Common reasons this can look like “dedup does not dedup”:

- Out-of-order delivery and resend are active, so old offsets legitimately arrive later.
- Gap state was purged (window moved on), so very old events may no longer be tracked as duplicates.
- Input key is not in expected `topic_partition_offset` format, so the record is forwarded directly.

To verify behavior, check logs/JMX for current gaps and compare with the record key (`topic_partition_offset`).

Also, see below for tuning `WINDOW_SIZE` and `MAX_WINDOWS` to better track duplicates in your expected traffic patterns.

This is also mentioned in [Deduplication.md](Deduplication.md#deduplication), but it is a common point of confusion, so it is worth reiterating here.

## How should I set WINDOW_SIZE and MAX_WINDOWS?

`WINDOW_SIZE` and `MAX_WINDOWS` control how much offset history each deduplication instance keeps in memory.

- `WINDOW_SIZE` = number of offsets tracked per window
- `MAX_WINDOWS` = number of windows kept before the oldest window is purged

Together, they define the retained range:

```text
retained offsets per partition ≈ WINDOW_SIZE * MAX_WINDOWS
```

### Practical rule of thumb

- Increase `MAX_WINDOWS` when you need to tolerate longer resend delays or longer out-of-order arrival.
- Increase `WINDOW_SIZE` when throughput is high and windows roll too quickly.
- Prefer many moderately sized windows over a few very large windows for better bitmap behavior.

### Memory tradeoff

Larger values improve duplicate detection across a longer history, but require more RAM.
If memory pressure appears, first try lowering `MAX_WINDOWS`, then tune `WINDOW_SIZE`.

### Example

If you expect late arrivals up to about 500,000 offsets behind current traffic, a starting point is:

```bash
WINDOW_SIZE=1000
MAX_WINDOWS=500
```

Then monitor memory and gap behavior, and adjust gradually.

## Deduplication gets stuck in a restart loop when FAIL_FAST is enabled

If `FAIL_FAST=true`, the deduplication process exits when it does not reach `RUNNING` within the configured startup timeout.

In environments with slower Kafka startup, DNS, TLS handshake, or topic metadata fetch, a low value for `FAIL_FAST_STARTUP_TIMEOUT_MS` can cause repeated restarts (boot loop).

Known working adjustment from field testing:

- `FAIL_FAST_STARTUP_TIMEOUT_MS=10000` caused restarts
- `FAIL_FAST_STARTUP_TIMEOUT_MS=30000` stabilized startup

Example:

```bash
export FAIL_FAST=true
export FAIL_FAST_STARTUP_TIMEOUT_MS=30000
```

If needed, increase further based on your environment and startup latency.

## I have disabled write for /tmp. Now I get an error running the deduplication

The error occurs because RocksDB (used by Kafka Streams for state storage) extracts its native library to tmp by default, but that directory is write-protected on your machine.

You can fix this by setting the ROCKSDB_SHAREDLIB_DIR environment variable to point to a writable directory before running the Java application:

```bash
export ROCKSDB_SHAREDLIB_DIR=/var/lib/kafka-streams/rocksdb
mkdir -p /var/lib/kafka-streams/rocksdb
java -jar java-streams/target/air-gap-deduplication-fat-*.jar
```

Or if you're running it as a systemd service, add this to your service file:

```bash
[Service]
Environment="ROCKSDB_SHAREDLIB_DIR=/var/lib/kafka-streams/rocksdb"
Environment="STATE_DIR_CONFIG=/var/lib/kafka-streams/state"
```

Alternatively, you can set the Java system property:

```bash
java -Dorg.rocksdb.tmpdir=/var/lib/kafka-streams/rocksdb -jar java-streams/target/air-gap-deduplication-fat-*.jar
```

Make sure the directory you choose is writable by the user running the application. The RocksDB native library will be extracted there instead of tmp.
