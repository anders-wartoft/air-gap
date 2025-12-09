# ============================================================================
# Java Build
# ============================================================================

.PHONY: build-java java-clean

build-java: nfpm
	@echo "Building Java deduplication application..."
	cd java-streams && \
		mvn versions:set -DnewVersion=$(PKG_VERSION) -DgenerateBackupPoms=false && \
		mvn clean package -DskipTests
	@echo "✅ Java JAR built: $(JAVA_TARGET_DIR)/air-gap-deduplication-fat-*.jar"

java-clean:
	cd java-streams && mvn clean
