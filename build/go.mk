# ============================================================================
# Go Binary Builds
# ============================================================================

.PHONY: build-go build-go-all build-go-linux-amd64 build-go-linux-arm64 build-go-mac-arm64 _build-binaries upstream downstream create resend

GO_BINARIES := upstream downstream create resend
GO_BUILD_DIR := $(LINUX_AMD64_DIR)

# Increment build number once per build
.build-number-incremented:
	@echo $(NEXT_BUILD_NUMBER) > $(BUILD_NUMBER_FILE)
	@touch .build-number-incremented

build-go: build-go-linux-amd64
	@echo "✅ Go binaries built (Linux AMD64)"

build-go-all: build-go-linux-amd64 build-go-linux-arm64 build-go-mac-arm64
	@echo "✅ All Go binaries built"

build-go-linux-amd64: .build-number-incremented $(GO_BINARIES)
	@echo "✅ Go binaries built (Linux AMD64)"

build-go-linux-arm64: .build-number-incremented src/version/version.go
	@echo "Building binaries (linux arm64)..."
	@mkdir -p $(LINUX_ARM64_DIR)
	@GOOS=linux GOARCH=arm64 go build -ldflags '$(BUILD_LDFLAGS)' -o $(LINUX_ARM64_DIR)/upstream ./src/cmd/upstream
	@GOOS=linux GOARCH=arm64 go build -ldflags '$(BUILD_LDFLAGS)' -o $(LINUX_ARM64_DIR)/downstream ./src/cmd/downstream
	@GOOS=linux GOARCH=arm64 go build -ldflags '$(BUILD_LDFLAGS)' -o $(LINUX_ARM64_DIR)/create ./src/cmd/create
	@GOOS=linux GOARCH=arm64 go build -ldflags '$(BUILD_LDFLAGS)' -o $(LINUX_ARM64_DIR)/resend ./src/cmd/resend
	@echo "✅ Go binaries built (Linux ARM64)"

build-go-mac-arm64: .build-number-incremented src/version/version.go
	@echo "Building binaries (darwin arm64)..."
	@mkdir -p $(MAC_ARM64_DIR)
	@GOOS=darwin GOARCH=arm64 go build -ldflags '$(BUILD_LDFLAGS)' -o $(MAC_ARM64_DIR)/upstream ./src/cmd/upstream
	@GOOS=darwin GOARCH=arm64 go build -ldflags '$(BUILD_LDFLAGS)' -o $(MAC_ARM64_DIR)/downstream ./src/cmd/downstream
	@GOOS=darwin GOARCH=arm64 go build -ldflags '$(BUILD_LDFLAGS)' -o $(MAC_ARM64_DIR)/create ./src/cmd/create
	@GOOS=darwin GOARCH=arm64 go build -ldflags '$(BUILD_LDFLAGS)' -o $(MAC_ARM64_DIR)/resend ./src/cmd/resend
	@echo "✅ Go binaries built (macOS ARM64)"

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
