# ============================================================================
# Java Build
# ============================================================================

.PHONY: build-java java-clean

build-java: nfpm
	@echo "Building Java deduplication application..."
	cd java-streams && \
		mvn versions:set -DnewVersion=$(PKG_VERSION) -DgenerateBackupPoms=false && \
		mvn clean package -DskipTests -Dbuild.number=$(NEXT_BUILD_NUMBER) && \
		cp target/air-gap-deduplication-fat-$(PKG_VERSION)-linux-amd64.jar \
			target/air-gap-deduplication-fat.jar
	tests/scripts/verify-dedup-native-libraries.sh \
		java-streams/target/air-gap-deduplication-fat-$(PKG_VERSION)-linux-amd64.jar
	@echo "✅ Java JAR built: $(JAVA_TARGET_DIR)/air-gap-deduplication-fat-*.jar"

java-clean:
	cd java-streams && mvn clean
