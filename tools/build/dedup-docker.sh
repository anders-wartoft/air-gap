#!/bin/bash

# Build script for the Java deduplicator application
# Uses Docker to ensure consistent build environment

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
JAVA_STREAMS_DIR="$PROJECT_ROOT/java-streams"

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if Maven Docker image is available
if ! docker images maven | grep -q "latest"; then
    log_info "Pulling Maven Docker image..."
    docker pull maven:latest
fi

log_info "Building Java deduplicator application..."
log_info "Project root: $PROJECT_ROOT"

# Run Maven build in Docker
docker run --rm \
    -v "$JAVA_STREAMS_DIR:/workspace" \
    -w /workspace \
    maven:latest \
    mvn clean package -DskipTests

# Check if build was successful
JAR_FILE="$JAVA_STREAMS_DIR/target/air-gap-deduplication-fat-0.1.5-SNAPSHOT.jar"
if [[ -f "$JAR_FILE" ]]; then
    jar_size=$(du -h "$JAR_FILE" | cut -f1)
    log_info "Build successful!"
    log_info "Fat JAR: $JAR_FILE (Size: $jar_size)"
    
    # Also create a symlink for easier access
    ln -sf "$JAR_FILE" "$PROJECT_ROOT/air-gap-deduplication-fat.jar"
    log_info "Symlink created: $PROJECT_ROOT/air-gap-deduplication-fat.jar"
else
    log_error "Build failed - JAR file not found"
    exit 1
fi

# Show build artifacts
log_info "Build artifacts:"
ls -la "$JAVA_STREAMS_DIR/target/"*.jar 2>/dev/null || true
