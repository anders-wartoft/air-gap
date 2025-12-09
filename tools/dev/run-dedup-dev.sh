#!/bin/bash

# Development script to run the deduplicator with Docker
# Usage: ./tools/dev/run-dedup-dev.sh [instance-id]

set -euo pipefail

INSTANCE_ID="${1:-1}"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CONFIG_FILE="$PROJECT_ROOT/config/dedup-dev.env"
JAR_FILE="$PROJECT_ROOT/java-streams/target/air-gap-deduplication-fat-0.1.5-SNAPSHOT.jar"

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

# Check if JAR exists
if [[ ! -f "$JAR_FILE" ]]; then
    log_warn "JAR file not found at: $JAR_FILE"
    log_info "Building the application first..."
    "$PROJECT_ROOT/tools/build/dedup-docker.sh"
fi

# Create state directory if it doesn't exist
STATE_DIR="$PROJECT_ROOT/tmp/dedup_state_dev_$INSTANCE_ID"
mkdir -p "$STATE_DIR"

log_info "Starting deduplicator instance $INSTANCE_ID..."
log_info "JAR: $JAR_FILE"
log_info "Config: $CONFIG_FILE"
log_info "State: $STATE_DIR"

# Override state directory in environment
export STATE_DIR_CONFIG="$STATE_DIR"
export INSTANCE_ID="$INSTANCE_ID"

# Run the deduplicator with Docker
docker run --rm \
    -v "$PROJECT_ROOT/java-streams/target:/app" \
    -v "$PROJECT_ROOT/config:/config:ro" \
    -v "$STATE_DIR:/state" \
    --env-file "$CONFIG_FILE" \
    -e STATE_DIR_CONFIG="/state" \
    -e INSTANCE_ID="$INSTANCE_ID" \
    -p 999$INSTANCE_ID:9999 \
    --name "airgap-dedup-dev-$INSTANCE_ID" \
    openjdk:17 \
    java ${JAVA_OPTS:-} -jar /app/air-gap-deduplication-fat-0.1.5-SNAPSHOT.jar

log_info "Deduplicator stopped."
