# Transport Configuration - UDP and TCP

## Overview

Air-gap supports both UDP and TCP as transport protocols for sending messages between upstream and downstream. Both protocols support encryption, compression, fragmentation, and automatic retry logic to ensure reliable delivery.

## Transport Types

### UDP (Default)

**Characteristics:**

- Connectionless protocol
- Fast, low latency
- No delivery guarantee at transport level (reliability handled by Kafka offset tracking)
- Good for hardware diodes and high-throughput scenarios
- Suitable when transient packet loss is acceptable

**When to use UDP:**

- Hardware diode environments where only UDP can pass
- High-throughput scenarios (10,000+ events/sec)
- Low-latency requirements
- Network bandwidth is limited

### TCP

**Characteristics:**

- Connection-oriented protocol
- Connection state validation before each send
- Automatic reconnection on connection loss
- Guaranteed delivery confirmation at transport level
- Better for unreliable networks with temporary disconnections

**When to use TCP:**

- Software-based connections (no hardware diode)
- Network with frequent temporary outages
- Requirement for connection state awareness
- Lower throughput scenarios (< 10,000 events/sec)

## Configuration

### Setting the Transport Type

Both upstream and downstream support a `transport` configuration parameter:

```bash
# Use TCP (default is UDP if not specified)
transport=tcp

# Or explicitly use UDP
transport=udp
```

### Environment Variable Override

Transport selection can be overridden with environment variables:

**Upstream:**

```bash
export AIRGAP_UPSTREAM_TRANSPORT=tcp
./upstream config/upstream.properties
```

**Downstream:**

```bash
export AIRGAP_DOWNSTREAM_TRANSPORT=tcp
./downstream config/downstream.properties
```

### Example Configuration Files

**upstream.properties with TCP:**

```bash
id=Upstream_1
nic=en0
targetIP=192.168.1.100
targetPort=1234
transport=tcp
source=kafka
bootstrapServers=kafka:9092
topic=source-topic
groupID=upstream-group
publicKeyFile=certs/server.pem
```

**downstream.properties with TCP:**

```bash
id=Downstream_1
nic=en0
targetIP=0.0.0.0
targetPort=1234
transport=tcp
bootstrapServers=kafka:9092
topic=destination-topic
privateKeyFiles=certs/private*.pem
```

## Reliability Features

### Message Retry Logic

Upstream retries failed sends automatically. The strategy differs by transport:

**TCP retry strategy (configurable):**

- Default: retry indefinitely (`tcpRetryTimes=0`) with 1-second pause between attempts
- Upstream never gives up by default — no messages are lost even if downstream is restarted
- Sending resumes automatically as soon as the downstream comes back
- All three parameters are configurable; see [TCP Retry Configuration](#tcp-retry-configuration) below

**UDP retry strategy (fixed):**

- Maximum 30 attempts with 100ms pause (3 seconds total)
- If all 30 attempts fail, the Kafka offset is NOT committed — the message is reprocessed on the next Kafka poll
- Only on transient failures (I/O errors)

**Example log output (TCP, default warn level):**

```bash
[WARN] Send attempt 1/∞ failed for id=transfer_4_23: TCP connection unavailable to 127.0.0.1:1234, retrying in 1000ms
[INFO] Message id=transfer_4_23 successfully sent after 2 attempt(s)
```

**Kafka Integration:**

- For TCP with infinite retries (`tcpRetryTimes=0`): upstream blocks until delivery succeeds — no message is ever lost
- For TCP with finite retries or UDP: if all retries fail, the Kafka message is NOT marked as consumed and will be reprocessed when transport is restored
- This prevents message loss due to temporary network issues

### Status Tracking

Transport status is continuously monitored and reported:

**Status Field:**

- `status: "running"` - Transport is operational
- `status: "<error message>"` - Transport encountered an error

**Log Messages:**

- **ERROR** level when status changes from "running" to an error state
- **INFO** level when status is restored from error to "running"
- Error status is included in periodic statistics output

**Example statistics with status:**

```json
{
  "id": "Upstream_1",
  "eps": 150,
  "status": "running",
  "total_sent": 45000,
  ...
}
```

## TCP-Specific Features

### TCP Retry Configuration

When `transport=tcp`, upstream retries failed sends according to three configurable parameters:

| Property | Env variable | Default | Description |
| --- | --- | --- | --- |
| `tcpRetryInterval` | `AIRGAP_UPSTREAM_TCP_RETRY_INTERVAL` | `1000` | Milliseconds to wait between retry attempts |
| `tcpRetryTimes` | `AIRGAP_UPSTREAM_TCP_RETRY_TIMES` | `0` | Max retries; `0` = infinite (never give up) |
| `tcpRetryErrorLevel` | `AIRGAP_UPSTREAM_TCP_RETRY_ERROR_LEVEL` | `WARN` | Log level for per-attempt retry messages (`DEBUG`/`INFO`/`WARN`/`ERROR`) |

**Default behaviour (recommended for production):**

```properties
transport=tcp
# Not required — these are the defaults:
tcpRetryInterval=1000
tcpRetryTimes=0
tcpRetryErrorLevel=WARN
```

With `tcpRetryTimes=0`, sending blocks until the downstream comes back. The upstream never asks Kafka to reprocess the message, so **no messages are lost** regardless of how long the downstream is unavailable.

**Quieter retry logging:**

```properties
tcpRetryErrorLevel=debug   # Only visible when logLevel=debug
```

**Finite retries (fall back to Kafka reprocessing after N failures):**

```properties
tcpRetryTimes=10           # Give up after 10 attempts (10 seconds at default interval)
tcpRetryInterval=1000
```

When finite retries are exhausted, upstream returns `false` to the Kafka handler, so the message is reprocessed on the next Kafka poll — no data is lost, but ordering may be affected.

### Connection Health Check

Before each send attempt, TCP validates the connection health:

**Mechanism:**

- Sets a 1ms read deadline
- Attempts a non-blocking read to detect dead connections
- Immediately closes and reconnects if connection is detected as dead
- Prevents sending to stale connections

### Automatic Reconnection

When TCP connection fails:

1. Connection is immediately closed
2. Next send attempt triggers automatic reconnection
3. Reconnection failures are logged and status is updated
4. Upstream doesn't fail on startup if downstream is unavailable (lazy connection)

**Example log output:**

```bash
[INFO] Connected to TCP server at 192.168.1.100:1234
[ERROR] Transport status changed to error: failed to reconnect to TCP server: connection refused (was: running)
[INFO] Transport status restored to running (was: failed to reconnect to TCP server: connection refused)
```

## UDP-Specific Behavior

### Transient Failure Handling

UDP is connectionless, so transient errors (ICMP connection refused) are handled specially:

**Important:** UDP "write succeeded" does not guarantee delivery. When retries eventually succeed after initial failures, a WARNING is logged:

```bash
[DEBUG] Send attempt 1/3 failed for id=transfer_3_0: connection refused, retrying in 100ms
[WARN] Message id=transfer_3_0 sent after 2 attempt(s) - UDP delivery is not guaranteed (receiver may be down)
```

**This warning indicates:**

- The message was accepted by the kernel
- But the remote receiver may not be receiving it
- Delivery confirmation depends on Kafka gap detection
- The downstream server should be checked if warnings appear frequently

## Monitoring Transport Health

### Log Levels

**ERROR:**

- Transport connection failures
- Status changes to error state
- Failed message delivery after all retries

**WARN (UDP only):**

- Messages sent after transient retries
- Indicates potential delivery uncertainty

**INFO:**

- Status restoration from error to running
- Successful delivery after retries
- Connection establishment

**DEBUG:**

- Individual retry attempts
- Connection health checks
- Detailed error information

### Statistics

Enable periodic statistics reporting to monitor transport health:

**Upstream example:**

```bash
logStatistics=60  # Report every 60 seconds
```

**Statistics include:**

- `status` field showing transport state
- `eps` (events per second) - throughput
- `total_sent` - cumulative messages sent
- `interval` - reporting interval

### Troubleshooting

**TCP Connection Refused:**

- Check if downstream is running: `netstat -an | grep 1234`
- Verify firewall allows TCP on configured port
- Check network connectivity: `ping 192.168.1.100`

**UDP Connection Refused (with eventual success):**

- Normal behavior if downstream recovers quickly
- Monitor for persistent warnings
- Check downstream logs for errors
- Verify MTU settings if using custom MTU values

**Status Always in Error:**

- Check network connectivity
- Verify target IP and port are correct
- Check firewall rules
- For TCP: verify downstream is accepting connections
- For UDP: verify downstream is listening on correct port

**Message Loss:**

- Monitor Kafka gap detection output
- Check statistics `status` field for errors
- Enable DEBUG logging to see retry details
- Verify downstream is processing messages (check Kafka consumer groups)

## Performance Considerations

### TCP vs UDP Throughput

**UDP:**

- Higher throughput (less overhead)
- Better for sustained high-rate transfers (>10,000 eps)
- Can occasionally lose messages (handled by gap detection)

**TCP:**

- Lower throughput (connection overhead)
- Better reliability for unreliable networks
- Slightly higher latency

### Configuration Tuning

**For high throughput (UDP):**

```bash
mtu=9000           # Larger packets (jumbo frames)
transport=udp
payloadSize=8000   # Optimize fragmentation
```

**For reliability (TCP):**

```bash
transport=tcp
payloadSize=4096      # Conservative fragmentation
tcpRetryInterval=1000 # 1 second between retries
tcpRetryTimes=0       # Retry indefinitely (no message loss)
tcpRetryErrorLevel=warn
```

## Migration Between Transports

To switch from UDP to TCP:

1. Stop upstream and downstream gracefully
2. Update configuration files to add `transport=tcp`
3. Verify network connectivity to new port
4. Start downstream first, then upstream
5. Monitor statistics and logs for any issues

To switch back to UDP:

1. Update configuration files to `transport=udp`
2. Restart applications
3. Gap detection will handle any missed messages

## Version Compatibility

Added in 0.1.8-SNAPSHOT

- TCP support
- Automatic reconnection
- Conditional message marking
- Retry logging improvements

Added later:

- `tcpRetryInterval`, `tcpRetryTimes`, `tcpRetryErrorLevel` — configurable TCP retry behaviour with default infinite-retry to guarantee no message loss

Both UDP and TCP use the same underlying message protocol and are fully compatible at the application level.
