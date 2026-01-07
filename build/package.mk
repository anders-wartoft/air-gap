# ============================================================================
# Package Building - Pattern-Based System
# ============================================================================

.PHONY: nfpm package-all package-upstream package-downstream package-dedup

# Install nfpm if needed
nfpm:
	@command -v $(NFPM) >/dev/null 2>&1 || { \
		echo "Installing nfpm tool..."; \
		GO111MODULE=on go install $(NFPM_MODULE); \
	}

# Create dist directory
$(DIST_DIR):
	@mkdir -p $(DIST_DIR)

# Build all packages
package-all: package-upstream package-downstream package-dedup
	@echo "✅ All packages built successfully"
	@echo ""
	@ls -lh $(DIST_DIR)/

# Aggregate targets for each component
package-upstream: package-upstream-rpm package-upstream-deb package-upstream-apk
	@echo "✅ Upstream packages built"

package-downstream: package-downstream-rpm package-downstream-deb package-downstream-apk
	@echo "✅ Downstream packages built"

package-dedup: package-dedup-rpm package-dedup-deb package-dedup-apk
	@echo "✅ Dedup packages built"

# Define build dependencies for each component
build-deps-upstream: upstream resend
build-deps-downstream: downstream
build-deps-dedup: build-java create

# Pattern rule: package-<component>-<format>
package-%-rpm package-%-deb package-%-apk: build-deps-% nfpm | $(DIST_DIR)
	$(eval COMPONENT := $*)
	$(eval FORMAT := $(lastword $(subst -, ,$@)))
	@echo "📦 Building $(COMPONENT) $(FORMAT)..."
	@$(RM) $(DIST_DIR)/airgap-$(COMPONENT)*.$(FORMAT)
	VERSION=$(PKG_VERSION) \
	RELEASE=$(PKG_RELEASE) \
	PACKAGE_NAME=airgap-$(COMPONENT) \
	GITHUB_SHA=$(GITHUB_SHA) \
	$(NFPM) package --packager $(FORMAT) \
		--config packaging/nfpm-$(COMPONENT).yaml \
		--target $(DIST_DIR)
