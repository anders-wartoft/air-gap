#!/bin/bash

# Air Gap Deduplicator Instance Setup Script
# This script sets up multiple deduplicator instances with unique configurations

set -euo pipefail

# Configuration
DEDUP_JAR_PATH="${1:-/opt/airgap/java-streams/air-gap-deduplication-fat-0.1.5-SNAPSHOT.jar}"
CONFIG_DIR="${2:-/opt/airgap/config}"
NUM_INSTANCES="${3:-2}"
STATE_BASE_DIR="${4:-/opt/airgap/state}"
LOG_DIR="${5:-/var/log/airgap}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if running as root
if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root"
    exit 1
fi

# Check if deduplicator JAR exists
if [[ ! -f "$DEDUP_JAR_PATH" ]]; then
    log_error "Deduplicator JAR not found at: $DEDUP_JAR_PATH"
    exit 1
fi

# Create necessary directories
log_info "Creating necessary directories..."
mkdir -p "$CONFIG_DIR" "$STATE_BASE_DIR" "$LOG_DIR"

# Create airgap user if it doesn't exist
if ! id "airgap" &>/dev/null; then
    log_info "Creating airgap user..."
    useradd -r -s /bin/false -d /opt/airgap airgap
fi

# Set ownership
chown -R airgap:airgap /opt/airgap "$STATE_BASE_DIR" "$LOG_DIR"
chmod 755 "$CONFIG_DIR" "$STATE_BASE_DIR" "$LOG_DIR"

# Create configuration files for each instance
log_info "Creating configuration files for $NUM_INSTANCES instances..."

for i in $(seq 1 $NUM_INSTANCES); do
    CONFIG_FILE="$CONFIG_DIR/dedup-$i.env"
    STATE_DIR="$STATE_BASE_DIR/dedup-$i"
    
    # Create state directory for this instance
    mkdir -p "$STATE_DIR"
    chown airgap:airgap "$STATE_DIR"
    
    # Create configuration file
    cat > "$CONFIG_FILE" << EOF
# Deduplicator Instance $i Configuration
# Core Topics
RAW_TOPICS=transfer
CLEAN_TOPIC=dedup
GAP_TOPIC=gaps

# Kafka Connection
BOOTSTRAP_SERVERS=localhost:9092

# State Management (unique per instance)
STATE_DIR_CONFIG=$STATE_DIR/
WINDOW_SIZE=5000
MAX_WINDOWS=100

# Timing Configuration
GAP_EMIT_INTERVAL_SEC=300
PERSIST_INTERVAL_MS=1000

# Application Configuration
APPLICATION_ID=dedup-gap-app-$i

# Java Configuration
JAVA_OPTS="-Xms512m -Xmx2g -Dlog4j2.configurationFile=/opt/airgap/config/log4j2.xml"

# Instance-specific settings
INSTANCE_ID=$i
logLevel=INFO
EOF

    chown airgap:airgap "$CONFIG_FILE"
    chmod 640 "$CONFIG_FILE"
    
    log_info "Created configuration: $CONFIG_FILE"
done

# Install systemd service template
SERVICE_FILE="/etc/systemd/system/airgap-dedup@.service"
CURRENT_DIR="$(dirname "$(readlink -f "$0")")"
SOURCE_SERVICE="$CURRENT_DIR/../../packaging/systemd/airgap-dedup@.service"

if [[ -f "$SOURCE_SERVICE" ]]; then
    log_info "Installing systemd service template..."
    cp "$SOURCE_SERVICE" "$SERVICE_FILE"
    systemctl daemon-reload
    log_info "Systemd service template installed: $SERVICE_FILE"
else
    log_error "Service template not found at: $SOURCE_SERVICE"
    exit 1
fi

# Create log4j2 configuration for structured logging
LOG4J2_FILE="$CONFIG_DIR/log4j2.xml"
cat > "$LOG4J2_FILE" << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<Configuration status="WARN">
  <Appenders>
    <RollingFile name="FileAppender"
                 fileName="/var/log/airgap/dedup-${env:INSTANCE_ID:-default}.log"
                 filePattern="/var/log/airgap/dedup-${env:INSTANCE_ID:-default}-%d{yyyy-MM-dd}.log.gz">
      <PatternLayout pattern="%d{yyyy-MM-dd HH:mm:ss} %-5p [%t] %c{1}:%L - %m%n"/>
      <Policies>
        <TimeBasedTriggeringPolicy interval="1" modulate="true"/>
      </Policies>
    </RollingFile>
    <Console name="Console" target="SYSTEM_OUT">
      <PatternLayout pattern="%d{yyyy-MM-dd HH:mm:ss} %-5p [%t] %c{1}:%L - %m%n"/>
    </Console>
  </Appenders>
  <Loggers>
    <Root level="${env:logLevel:-INFO}">
      <AppenderRef ref="FileAppender"/>
      <AppenderRef ref="Console"/>
    </Root>
  </Loggers>
</Configuration>
EOF

chown airgap:airgap "$LOG4J2_FILE"
chmod 640 "$LOG4J2_FILE"

log_info "Setup completed successfully!"
log_info "To start instances, run:"
for i in $(seq 1 $NUM_INSTANCES); do
    echo "  systemctl start airgap-dedup@$i"
    echo "  systemctl enable airgap-dedup@$i"
done

log_info "To check status:"
echo "  systemctl status airgap-dedup@1"
echo "  journalctl -u airgap-dedup@1 -f"

log_info "Configuration files created in: $CONFIG_DIR"
log_info "State directories created in: $STATE_BASE_DIR"
