# ============================================================================
# Go Binary Builds
# ============================================================================

.PHONY: build-go upstream downstream create resend

GO_BINARIES := upstream downstream create resend
GO_BUILD_DIR := $(LINUX_AMD64_DIR)

# Increment build number once per build
.build-number-incremented:
	@echo $(NEXT_BUILD_NUMBER) > $(BUILD_NUMBER_FILE)
	@touch .build-number-incremented

build-go: .build-number-incremented $(GO_BINARIES)
	@echo "✅ Go binaries built"

# Generate version file
src/version/version.go:
	@echo "Generating version file..."
	@mkdir -p src/version
	@echo 'package version' > $@
	@echo '' >> $@
	@echo 'var GitVersion = "$(GIT_VERSION)"' >> $@

# Individual binary targets with explicit rules
upstream: src/version/version.go .build-number-incremented
	@echo "Building upstream ($(GOARCH))..."
	@mkdir -p $(GO_BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags '$(BUILD_LDFLAGS)' \
		-o $(GO_BUILD_DIR)/upstream \
		./src/cmd/upstream

downstream: src/version/version.go .build-number-incremented
	@echo "Building downstream ($(GOARCH))..."
	@mkdir -p $(GO_BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags '$(BUILD_LDFLAGS)' \
		-o $(GO_BUILD_DIR)/downstream \
		./src/cmd/downstream

create: src/version/version.go .build-number-incremented
	@echo "Building create ($(GOARCH))..."
	@mkdir -p $(GO_BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags '$(BUILD_LDFLAGS)' \
		-o $(GO_BUILD_DIR)/create \
		./src/cmd/create

resend: src/version/version.go .build-number-incremented
	@echo "Building resend ($(GOARCH))..."
	@mkdir -p $(GO_BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags '$(BUILD_LDFLAGS)' \
		-o $(GO_BUILD_DIR)/resend \
		./src/cmd/resend
