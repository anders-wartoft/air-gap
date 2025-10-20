# ============================================================================
# Air-Gap Multi-Package Build System
# ============================================================================

# Include modular components
include build/variables.mk
include build/go.mk
include build/java.mk
include build/package.mk
include build/test.mk

# ============================================================================
# Main Targets
# ============================================================================

.PHONY: all clean help

all: build-go build-java
	@echo "✅ All components built successfully"

help:
	@echo "Air-Gap Build System"
	@echo "===================="
	@echo ""
	@echo "Build Targets:"
	@echo "  make all                    - Build everything (Go + Java)"
	@echo "  make build-go               - Build all Go binaries"
	@echo "  make build-java             - Build Java deduplication JAR"
	@echo ""
	@echo "Package Targets:"
	@echo "  make package-all            - Build all packages (all formats)"
	@echo "  make package-upstream       - Build upstream packages (rpm+deb+apk)"
	@echo "  make package-downstream     - Build downstream packages"
	@echo "  make package-dedup          - Build dedup packages"
	@echo ""
	@echo "Individual Package Format:"
	@echo "  make package-upstream-rpm   - Build upstream RPM only"
	@echo "  make package-upstream-deb   - Build upstream DEB only"
	@echo "  (Similar for downstream and dedup)"
	@echo ""
	@echo "Testing:"
	@echo "  make test                   - Run Go tests"
	@echo ""
	@echo "Cleanup:"
	@echo "  make clean                  - Remove all build artifacts"

clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(TARGET_DIR)
	rm -f src/version/version.go
	cd java-streams && mvn clean || true
	@echo "✅ Clean complete"

.PHONY: clean-packages
clean-packages:
	@echo "Cleaning package artifacts..."
	rm -rf $(DIST_DIR)
	@echo "✅ Package artifacts cleaned"
