# ============================================================================
# Java Build
# ============================================================================

.PHONY: build-java java-clean

build-java:
	@echo "Building Java deduplication application..."
	cd java-streams && mvn clean package -DskipTests
	@echo "✅ Java JAR built: $(JAVA_TARGET_DIR)/air-gap-deduplication-fat-*.jar"

java-clean:
	cd java-streams && mvn clean
