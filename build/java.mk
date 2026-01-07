# ============================================================================
# Java Build
# ============================================================================

.PHONY: build-java java-clean

build-java: nfpm
	@echo "Building Java deduplication application..."
	cd java-streams && \
		mvn versions:set -DnewVersion=$(PKG_VERSION) -DgenerateBackupPoms=false && \
		mvn clean package -DskipTests && \
		cp target/air-gap-deduplication-fat-$(PKG_VERSION).jar target/air-gap-deduplication-fat.jar
	@echo "✅ Java JAR built: $(JAVA_TARGET_DIR)/air-gap-deduplication-fat-*.jar"

java-clean:
	cd java-streams && mvn clean
