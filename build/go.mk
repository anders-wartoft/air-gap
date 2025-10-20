# ============================================================================
# Go Binary Builds
# ============================================================================

.PHONY: build-go upstream downstream create resend

GO_BINARIES := upstream downstream create resend
GO_BUILD_DIR := $(LINUX_AMD64_DIR)

build-go: $(GO_BINARIES)
	@echo "✅ Go binaries built"

# Generate version file
src/version/version.go:
	@echo "Generating version file..."
	@mkdir -p src/version
	@echo 'package version' > $@
	@echo '' >> $@
	@echo 'var GitVersion = "$(GIT_VERSION)"' >> $@

# Individual binary targets with explicit rules
upstream: src/version/version.go
	@echo "Building upstream ($(GOARCH))..."
	@mkdir -p $(GO_BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags '$(BUILD_LDFLAGS)' \
		-o $(GO_BUILD_DIR)/upstream \
		./src/cmd/upstream
	@echo $(NEXT_BUILD_NUMBER) > $(BUILD_NUMBER_FILE)

downstream: src/version/version.go
	@echo "Building downstream ($(GOARCH))..."
	@mkdir -p $(GO_BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags '$(BUILD_LDFLAGS)' \
		-o $(GO_BUILD_DIR)/downstream \
		./src/cmd/downstream
	@echo $(NEXT_BUILD_NUMBER) > $(BUILD_NUMBER_FILE)

create: src/version/version.go
	@echo "Building create ($(GOARCH))..."
	@mkdir -p $(GO_BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags '$(BUILD_LDFLAGS)' \
		-o $(GO_BUILD_DIR)/create \
		./src/cmd/create
	@echo $(NEXT_BUILD_NUMBER) > $(BUILD_NUMBER_FILE)

resend: src/version/version.go
	@echo "Building resend ($(GOARCH))..."
	@mkdir -p $(GO_BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags '$(BUILD_LDFLAGS)' \
		-o $(GO_BUILD_DIR)/resend \
		./src/cmd/resend
	@echo $(NEXT_BUILD_NUMBER) > $(BUILD_NUMBER_FILE)
