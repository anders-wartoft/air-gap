# Input Filtering

The air-gap system supports content-based filtering of events at the upstream side before transmission. This allows you to control which events are sent across the diode based on regex patterns matching the payload content.

## Overview

Input filtering provides:

- **Payload inspection**: Filter events based on their content, not just metadata
- **Allow/deny rules**: Create ordered lists of regex patterns with allow or deny actions
- **First match wins**: Rules are evaluated in order, the first matching rule determines the action
- **Security**: Protection against ReDoS attacks with timeout limits and dangerous pattern detection
- **Performance**: Minimal overhead with configurable regex timeout (default 100ms)

## Configuration

### Basic Setup

Add to your `config/upstream.properties`:

```properties
# File-based rules (recommended for complex filters). Use .txt suffix on the files for better detection as a rule file.
inputFilterRules=config/input-filter-rules.txt
inputFilterDefaultAction=allow
inputFilterTimeout=100

# Or inline rules (comma-separated)
inputFilterRules=allow:127\.0\.0\.1,allow:10\.10\.\d{1,3}\.\d{1,3},deny:\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}
inputFilterDefaultAction=allow
inputFilterTimeout=100
```

### Environment Variables

Override via environment variables:

```bash
export AIRGAP_UPSTREAM_INPUT_FILTER_RULES=config/input-filter-rules.txt
export AIRGAP_UPSTREAM_INPUT_FILTER_DEFAULT_ACTION=allow
export AIRGAP_UPSTREAM_INPUT_FILTER_TIMEOUT=100
```

### Rule Format

Each rule has the format: `action:regex_pattern`

- **Action**: Either `allow` or `deny`
- **Pattern**: A valid Go regex pattern (RE2 syntax)
- **Comments**: Lines starting with `#` are ignored (must be on separate lines, not inline)
- **Empty lines**: Ignored
- **Important**: Do NOT use inline comments on the same line as rules, as they will be included in the regex pattern and cause matching to fail

**Example rule file** (`config/input-filter-rules.txt`):

```bash
# Allow localhost traffic
allow:127\.0\.0\.1

# Allow internal network 10.x.x.x
allow:10\.\d{1,3}\.\d{1,3}\.\d{1,3}

# Allow internal network 192.168.x.x  
allow:192\.168\.\d{1,3}\.\d{1,3}

# Allow internal network 172.16.0.0/12
allow:172\.(1[6-9]|2[0-9]|3[0-1])\.(\d{1,3})\.(\d{1,3})

# Deny any other IP addresses
deny:\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b

# Rest is allowed by default action
```

## Default Action

When no rules match, the `inputFilterDefaultAction` determines the behavior:

- **`allow`** (default): Send the event if no rules match
- **`deny`**: Block the event if no rules match

This lets you create either:

- **Allowlist approach**: Set default to `deny`, use `allow` rules
- **Blocklist approach**: Set default to `allow`, use `deny` rules

## Use Cases

### Use Case 1: Allow Only High-Severity Events

Block all events except high/critical severity:

```bash
# Allow high and critical severity
allow:.*"severity":\s*"(high|critical)".*

# Deny everything else
deny:.*
```

With `inputFilterDefaultAction=deny`, this creates a strict allowlist.

### Use Case 2: Block PII (Personally Identifiable Information)

Allow everything except events containing PII:

```bash
# Block Social Security Numbers (US format)
deny:\b\d{3}-\d{2}-\d{4}\b

# Block email addresses
deny:(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}

# Block credit card numbers (basic pattern)
deny:\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b

# Block phone numbers (various formats)
deny:\b\d{3}[-.]?\d{3}[-.]?\d{4}\b

# Block credentials
deny:(?i)(password|passwd|pwd|secret|token|api[_-]?key)\s*[:=]\s*\S+

# Default: allow everything else
```

With `inputFilterDefaultAction=allow`, this blocks sensitive data while allowing normal traffic.

### Use Case 3: IP Address Allowlist

Network example - only allow specific internal networks:

```bash
# Allow localhost
allow:127\.0\.0\.1

# Allow 10.10.x.x network
allow:10\.10\.\d{1,3}\.\d{1,3}

# Deny any other IP addresses
deny:\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}

# Default: allow (non-IP content passes through)
```

With `inputFilterDefaultAction=allow`, this filters out unauthorized IP addresses while allowing other content.

### Use Case 4: Block Debug/Verbose Logs

Reduce traffic by filtering low-priority logs:

```bash
# Deny debug logs
deny:(?i)"level":\s*"debug"

# Deny trace logs
deny:(?i)"level":\s*"trace"

# Deny verbose logs
deny:(?i)"level":\s*"verbose"

# Allow everything else (info, warn, error)
```

### Use Case 5: Content-Based Routing Alternative

Use filtering for simple content routing (alternative to multiple upstreams):

```bash
# Only send security events
allow:(?i)"category":\s*"(security|authentication|authorization)"

# Deny everything else
deny:.*
```

## Security Considerations

### ReDoS Protection

The input filter includes multiple layers of protection against Regular Expression Denial of Service (ReDoS) attacks:

1. **Pattern validation**: Dangerous nested quantifiers are detected at startup:
   - `(\w+)*`, `(a+)+`, `(.*)*`, `(\w*)*`, `(\d+)*`, `(.+)+`, `(\S+)*`
2. **Timeout protection**: Each regex match has a configurable timeout (default 100ms via `inputFilterTimeout`)

3. **Panic recovery**: Regex panics are caught and treated as non-matches

4. **Length limits**: Regex patterns are limited to 1000 characters

### Configuration Options

- **`inputFilterRules`**: Path to rule file or inline comma-separated rules
- **`inputFilterDefaultAction`**: Default action (`allow` or `deny`) when no rules match
- **`inputFilterTimeout`**: Regex match timeout in milliseconds (default: 100)

### Best Practices

1. **Test patterns**: Validate regex patterns before deployment

   ```bash
   echo "test content" | grep -P "your_pattern"
   ```

2. **Start simple**: Begin with simple patterns, add complexity as needed

3. **Fail open**: On filter errors, events are allowed (not blocked)

4. **Monitor statistics**: Track `filtered` and `total_filtered` in statistics logs

5. **Use anchors**: Add `^` and `$` when matching entire payloads

6. **Escape special characters**: Remember to escape `.` `*` `+` `?` `[` `]` `(` `)` `{` `}` `|` `\`

## Performance Impact

At typical throughput levels:

- **Low throughput** (<1,000 eps): Negligible impact (<1% CPU)
- **Medium throughput** (1,000-10,000 eps): ~5-10% CPU overhead
- **High throughput** (>10,000 eps): ~10-20% CPU overhead

**Optimization tips**:

- Use simple patterns when possible
- Avoid complex alternations with many options
- Place most common matches first in the rule list
- Use anchors to avoid unnecessary backtracking

## Statistics

Filtered events are tracked in the statistics output:

```json
STATISTICS: {
  "id": "Upstream_1",
  "time": 1735776000,
  "time_start": 1735772400,
  "interval": 60,
  "received": 3600,
  "sent": 3200,
  "filtered": 400,
  "unfiltered": 3200,
  "filter_timeouts": 2,
  "eps": 60,
  "total_received": 72000,
  "total_sent": 64000,
  "total_filtered": 8000,
  "total_unfiltered": 64000,
  "total_filter_timeouts": 15
}
```

Fields:

- **`filtered`**: Events filtered (blocked) during the last interval
- **`unfiltered`**: Events that passed through the input filter during the last interval
- **`total_filtered`**: Total events filtered (blocked) since startup
- **`total_unfiltered`**: Total events that passed through the input filter since startup
- **`filter_timeouts`**: Regex timeout errors during the last interval
- **`total_filter_timeouts`**: Total regex timeout errors since startup

## Troubleshooting

### No events are being sent

Check if your filter rules are too restrictive:

1. Set `inputFilterDefaultAction=allow` temporarily
2. Check logs for `"Input filter blocked message"` (debug level)
3. Verify your regex patterns match what you expect

### Filter not working

1. Verify the rule file path is correct
2. Check for syntax errors in rules (look for startup errors)
3. Enable debug logging: `logLevel=DEBUG`
4. Verify patterns with a regex tester

### Performance issues

1. Check for complex regex patterns (nested quantifiers)
2. Monitor CPU usage during high load
3. Simplify patterns or reduce rule count

## Example Filter Files

### Security-focused Filter

`config/security-filter.txt`:

```bash
# Allow security-relevant events
allow:(?i)"category":\s*"(security|authentication|authorization|audit)"
allow:(?i)"severity":\s*"(high|critical|error)"
allow:(?i)"event":\s*"(login|logout|access_denied|permission_change)"

# Block everything else
deny:.*
```

### Privacy-protection Filter

`config/privacy-filter.txt`:

```bash
# Block common PII patterns
deny:\b\d{3}-\d{2}-\d{4}\b                                    # SSN
deny:(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b            # Email
deny:\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b               # Credit cards
deny:(?i)(password|passwd|pwd|secret|token|api_key)\s*[:=]    # Credentials

# Allow everything else
```

### Network Filter

`config/network-filter.txt`:

```bash
# Allow private networks only
allow:10\.\d{1,3}\.\d{1,3}\.\d{1,3}
allow:172\.(1[6-9]|2[0-9]|3[01])\.\d{1,3}\.\d{1,3}
allow:192\.168\.\d{1,3}\.\d{1,3}
allow:127\.0\.0\.1
allow:::1

# Block all other IPs
deny:\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}
deny:[0-9a-fA-F:]+

# Allow non-IP content
```

## Related Documentation

- [Installation and Configuration](Installation%20and%20Configuration.md) - General configuration guide
- [Monitoring](Monitoring.md) - Tracking filter statistics
