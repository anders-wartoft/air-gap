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

Both UDP and TCP implementations include automatic retry logic with exponential backoff:

**Retry Strategy:**

- Maximum 3 attempts per message
- Exponential backoff: 100ms, 200ms, 400ms between attempts
- Only on transient failures (connection errors, I/O errors)

**Example log output:**

```bash
[DEBUG] Send attempt 1/3 failed for id=transfer_4_23: connection refused, retrying in 100ms
[INFO] Message id=transfer_4_23 successfully sent after 2 attempt(s)
```

**Kafka Integration:**

- If all retries fail, the Kafka message is NOT marked as consumed
- Failed messages are automatically reprocessed when transport is restored
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
payloadSize=4096   # Conservative fragmentation
# TCP handles retries automatically
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

Both UDP and TCP use the same underlying message protocol and are fully compatible at the application level.
