# ============================================================================
# Variables and Configuration
# ============================================================================

# Version and Build Info
GIT_VERSION := $(shell git tag -l --sort=-version:refname | head -1 2>/dev/null || echo 0.0.0)
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo local)
BUILD_NUMBER_FILE := BUILD_NUMBER
BUILD_NUMBER := $(shell cat $(BUILD_NUMBER_FILE) 2>/dev/null || echo 1)
NEXT_BUILD_NUMBER := $(shell echo $$(($(BUILD_NUMBER)+1)))
BUILD_LDFLAGS := -X 'main.BuildNumber=$(NEXT_BUILD_NUMBER)'

# Package Configuration
PKG_VERSION ?= $(shell echo $(GIT_VERSION) | sed 's/^v//')
PKG_RELEASE ?= $(NEXT_BUILD_NUMBER)
GITHUB_SHA := $(GIT_SHA)

# Directories
TARGET_DIR := target
DIST_DIR := $(TARGET_DIR)/dist
LINUX_AMD64_DIR := $(TARGET_DIR)/linux-amd64
LINUX_ARM64_DIR := $(TARGET_DIR)/linux-arm64
MAC_ARM64_DIR := $(TARGET_DIR)/darwin-arm64
JAVA_TARGET_DIR := java-streams/target

# nfpm Configuration
NFPM_MODULE := github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.40.0
NFPM := $(shell go env GOPATH)/bin/nfpm
NFPM_PACKAGERS := rpm deb apk

# Package Names
PKG_UPSTREAM := airgap-upstream
PKG_DOWNSTREAM := airgap-downstream
PKG_DEDUP := airgap-dedup

# Build Configuration
GOOS ?= linux
GOARCH ?= amd64
