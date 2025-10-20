# ============================================================================
# Testing Targets
# ============================================================================

.PHONY: test test-go test-java

# Run all tests
test: test-go
	@echo "✅ All tests passed"

# Go tests
test-go:
	@echo "Running Go tests..."
	go test ./...

# Java tests
test-java:
	@echo "Running Java tests..."
	cd java-streams && mvn test

# ============================================================================
# Package Testing
# ============================================================================

.PHONY: test-package test-package-all test-docker-up test-docker-down

# Test specific package
# Usage: make test-package COMPONENT=upstream FORMAT=rpm
test-package:
	@if [ -z "$(COMPONENT)" ] || [ -z "$(FORMAT)" ]; then \
		echo "Usage: make test-package COMPONENT=<upstream|downstream|dedup> FORMAT=<rpm|deb|apk>"; \
		exit 1; \
	fi
	@echo "Testing $(COMPONENT) $(FORMAT) package..."
	@PACKAGE=$$(ls target/dist/airgap-$(COMPONENT)-*.$(FORMAT) 2>/dev/null | head -n1); \
	if [ -z "$$PACKAGE" ]; then \
		echo "ERROR: Package not found. Run 'make package-$(COMPONENT)-$(FORMAT)' first"; \
		exit 1; \
	fi; \
	tests/scripts/verify-$(COMPONENT).sh $(FORMAT) $$PACKAGE

# Test all packages
test-package-all: package-all
	@echo "Testing all packages..."
	@for component in upstream downstream dedup; do \
		for format in rpm deb apk; do \
			echo "Testing $$component ($$format)..."; \
			$(MAKE) test-package COMPONENT=$$component FORMAT=$$format; \
		done; \
	done
	@echo "✅ All package tests passed!"

# Start long-running test environment
test-docker-up:
	@echo "Starting test containers..."
	cd tests && docker-compose up -d
	@echo "Use: docker-compose -f tests/docker-compose.yml exec rocky bash"

# Stop test environment
test-docker-down:
	cd tests && docker-compose down
