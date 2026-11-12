package nu.sitia.airgap.streams;

import nu.sitia.airgap.gapdetector.DetectorFixtures;
import nu.sitia.airgap.gapdetector.GapDetectorStore;
import org.apache.kafka.clients.admin.Admin;
import org.apache.kafka.clients.admin.NewTopic;
import org.apache.kafka.clients.consumer.ConsumerConfig;
import org.apache.kafka.clients.consumer.KafkaConsumer;
import org.apache.kafka.clients.producer.KafkaProducer;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.apache.kafka.common.TopicPartition;
import org.apache.kafka.common.config.ConfigResource;
import org.apache.kafka.common.serialization.ByteArrayDeserializer;
import org.apache.kafka.common.serialization.ByteArraySerializer;
import org.apache.kafka.common.serialization.StringDeserializer;
import org.apache.kafka.common.serialization.StringSerializer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.UUID;
import java.util.concurrent.TimeUnit;
import java.util.function.BooleanSupplier;

import static org.junit.jupiter.api.Assertions.*;

class DedupStateIT {
    @TempDir Path directory;
    private final String id = "dedup-state-" + UUID.randomUUID().toString().substring(0, 8);
    private final String input = id + "-input";
    private final String output = id + "-output";
    private final String gaps = id + "-gaps";
    private final String changelog = id + "-" + PartitionDedupApp.STORE_GAP + "-changelog";
    private final List<Process> processes = new ArrayList<>();
    private Admin admin;
    private String bootstrap;

    @BeforeEach
    void setup() throws Exception {
        bootstrap = System.getenv("DEDUP_TEST_BOOTSTRAP");
        assertNotNull(bootstrap, "Run via bash tests/env/verify-dedup-state.sh");
        admin = Admin.create(Map.of("bootstrap.servers", bootstrap));
        admin.createTopics(List.of(
                new NewTopic(input, 1, (short) 1),
                new NewTopic(output, 1, (short) 1),
                new NewTopic(gaps, 1, (short) 1).configs(Map.of("cleanup.policy", "compact")),
                new NewTopic(changelog, 1, (short) 1).configs(
                        Map.of("cleanup.policy", "delete", "retention.ms", "43200000"))))
                .all().get(30, TimeUnit.SECONDS);
    }

    @AfterEach
    void cleanup() throws Exception {
        for (Process process : processes) {
            if (process.isAlive()) process.destroyForcibly();
            assertTrue(process.waitFor(30, TimeUnit.SECONDS), "Child JVM did not exit");
        }
        Path logs = Path.of("target", "dedup-it-logs", id);
        Files.createDirectories(logs);
        try (java.util.stream.Stream<Path> files = Files.list(directory)) {
            for (Path file : (Iterable<Path>) files.filter(p -> p.toString().endsWith(".log"))::iterator) {
                Files.copy(file, logs.resolve(file.getFileName()));
            }
        }
        if (admin != null) {
            admin.deleteTopics(List.of(input, output, gaps, changelog)).all().get(30, TimeUnit.SECONDS);
            admin.close(Duration.ofSeconds(5));
        }
    }

    private Process start(String state, String persistMs) throws Exception {
        return start(state, persistMs, "100");
    }

    private Process start(String state, String persistMs, String commitMs) throws Exception {
        Path log = directory.resolve(state + ".log");
        // RocksDB extracts JNI here; hardened hosts may mount /tmp with noexec.
        Path nativeTmp = Files.createDirectories(
                Path.of("target", "dedup-it-logs", id, state + "-tmp").toAbsolutePath());
        ProcessBuilder builder = new ProcessBuilder(
                Path.of(System.getProperty("java.home"), "bin", "java").toString(),
                "-Djava.io.tmpdir=" + nativeTmp,
                "-Dlog4j.configurationFile=" + getClass().getResource("/log4j2-dedup-it.xml"),
                "-cp", System.getProperty("surefire.test.class.path", System.getProperty("java.class.path")),
                PartitionDedupApp.class.getName());
        Map<String, String> env = builder.environment();
        // The child must not inherit deployment credentials or Kafka overrides.
        env.keySet().removeIf(k -> k.startsWith("KAFKA_"));
        env.putAll(Map.of(
                "BOOTSTRAP_SERVERS", bootstrap, "RAW_TOPICS", input, "CLEAN_TOPIC", output,
                "GAP_TOPIC", gaps, "APPLICATION_ID", id, "STATE_DIR_CONFIG", directory.resolve(state).toString(),
                "WINDOW_SIZE", "5000000", "MAX_WINDOWS", "5200", "PERSIST_INTERVAL_MS", persistMs,
                "COMMIT_INTERVAL_MS", commitMs));
        env.put("INSTANCE_ID", "");
        env.put("NUM_STANDBY_REPLICAS", "0");
        env.put("FAIL_FAST", "true");
        env.put("KAFKA_CONSUMER_SESSION_TIMEOUT_MS", "6000");
        env.put("KAFKA_CONSUMER_HEARTBEAT_INTERVAL_MS", "1000");
        env.put("KAFKA_CONSUMER_AUTO_OFFSET_RESET", "earliest");
        builder.redirectErrorStream(true).redirectOutput(log.toFile());
        Process process = builder.start();
        processes.add(process);
        await(() -> {
            assertTrue(process.isAlive(), () -> read(log));
            return read(log).contains("Kafka Streams reached RUNNING");
        }, "Dedup startup; log: " + log);
        return process;
    }

    private static String read(Path log) {
        try {
            return Files.readString(log);
        } catch (java.io.IOException e) {
            throw new java.io.UncheckedIOException(e);
        }
    }

    private static void await(BooleanSupplier condition, String description) throws Exception {
        long deadline = System.nanoTime() + Duration.ofSeconds(90).toNanos();
        while (System.nanoTime() < deadline) {
            if (condition.getAsBoolean()) return;
            Thread.sleep(100);
        }
        fail("Timed out: " + description);
    }

    private void send(String topic, String key, byte[] value) throws Exception {
        Properties props = new Properties();
        props.put("bootstrap.servers", bootstrap);
        props.put("max.request.size", "4194304");
        try (KafkaProducer<String, byte[]> producer = new KafkaProducer<>(
                props, new StringSerializer(), new ByteArraySerializer())) {
            producer.send(new ProducerRecord<>(topic, 0, key, value)).get(30, TimeUnit.SECONDS);
        }
    }

    private KafkaConsumer<String, byte[]> consumer(String topic) {
        Properties props = new Properties();
        props.put(ConsumerConfig.BOOTSTRAP_SERVERS_CONFIG, bootstrap);
        props.put(ConsumerConfig.ISOLATION_LEVEL_CONFIG, "read_committed");
        KafkaConsumer<String, byte[]> consumer = new KafkaConsumer<>(
                props, new StringDeserializer(), new ByteArrayDeserializer());
        consumer.assign(List.of(new TopicPartition(topic, 0)));
        consumer.seekToBeginning(consumer.assignment());
        return consumer;
    }

    private List<String> readThrough(String marker) throws Exception {
        List<String> keys = new ArrayList<>();
        try (KafkaConsumer<String, byte[]> consumer = consumer(output)) {
            await(() -> {
                consumer.poll(Duration.ofMillis(200)).forEach(r -> keys.add(r.key()));
                return keys.contains(marker);
            }, "committed output " + marker);
        }
        return keys;
    }

    @Test
    void committedOutputSurvivesHardCrashAndColdRestoreWithoutDuplicate() throws Exception {
        Process first = start("first", "600000");
        send(input, "export_0_1", new byte[] {1});
        assertEquals(List.of("export_0_1"), readThrough("export_0_1"));
        first.destroyForcibly();
        assertTrue(first.waitFor(30, TimeUnit.SECONDS));
        // Fresh local directory forces a read_committed changelog restoration.
        start("restored", "600000");
        send(input, "export_0_1", new byte[] {1});
        send(input, "export_0_2", new byte[] {2});
        assertEquals(List.of("export_0_1", "export_0_2"), readThrough("export_0_2"));
    }

    @Test
    void fragmentedLegacyHistoryMigratesAndCommitsUnderDefaultProducerLimit() throws Exception {
        byte[] fixture = DedupPersistenceTest.legacy(DetectorFixtures.largeDetector());
        assertTrue(fixture.length > 1_048_576);
        send(changelog, "export_0", fixture);
        Process process = start("migration", "200");
        send(input, "export_0_9999999", new byte[] {1});
        assertEquals(List.of("export_0_9999999"), readThrough("export_0_9999999"));
        assertTrue(process.isAlive(), "Oversized state must not kill Streams");
        process.destroyForcibly();
        assertTrue(process.waitFor(30, TimeUnit.SECONDS));
        start("migration-restored", "200");
        send(input, "export_0_0", new byte[] {1});
        send(input, "export_0_9999999", new byte[] {1});
        send(input, "export_0_10000000", new byte[] {2});
        assertEquals(List.of("export_0_9999999", "export_0_10000000"),
                readThrough("export_0_10000000"));
        try (KafkaConsumer<String, byte[]> consumer = consumer(changelog)) {
            long end = consumer.endOffsets(consumer.assignment()).values().iterator().next();
            List<String> migrated = new ArrayList<>();
            await(() -> {
                consumer.poll(Duration.ofMillis(200)).forEach(r -> {
                    if (!"export_0".equals(r.key()) && r.value() != null) {
                        assertTrue(r.value().length <= GapDetectorStore.BLOCK_BYTES, "Unbounded changelog value");
                        migrated.add(r.key());
                    }
                });
                return consumer.position(new TopicPartition(changelog, 0)) >= end;
            }, "bounded committed changelog");
            assertFalse(migrated.isEmpty(), "Migration must write versioned state records");
        }
    }

    @Test
    void existingDeleteOnlyChangelogIsRepairedBeforeProcessing() throws Exception {
        start("policy", "600000");
        ConfigResource resource = new ConfigResource(ConfigResource.Type.TOPIC, changelog);
        String policy = admin.describeConfigs(List.of(resource)).all().get(30, TimeUnit.SECONDS)
                .get(resource).get("cleanup.policy").value();
        assertEquals("compact", policy, "A delete-only changelog can erase the last durable state");
    }

    @Test
    void newChangelogUsesCompactionDespiteDeleteOnlyBrokerDefault() throws Exception {
        admin.deleteTopics(List.of(changelog)).all().get(30, TimeUnit.SECONDS);
        start("new-topic", "600000");
        ConfigResource resource = new ConfigResource(ConfigResource.Type.TOPIC, changelog);
        assertEquals("compact", admin.describeConfigs(List.of(resource)).all().get(30, TimeUnit.SECONDS)
                .get(resource).get("cleanup.policy").value());
    }

    @Test
    void uncommittedOutputIsAbortedAndReplayedExactlyOnceAfterCrash() throws Exception {
        Process first = start("uncommitted", "600000", "5000");
        send(input, "export_0_1", new byte[] {1});
        try (KafkaConsumer<String, byte[]> observer = uncommittedConsumer()) {
            await(() -> !observer.poll(Duration.ofMillis(200)).isEmpty(), "uncommitted first output");
        }
        try (KafkaConsumer<String, byte[]> committed = consumer(output)) {
            assertTrue(committed.poll(Duration.ofMillis(100)).isEmpty(),
                    "Crash must occur before the first transaction commits");
        }
        first.destroyForcibly();
        assertTrue(first.waitFor(30, TimeUnit.SECONDS));
        start("replayed", "600000");
        send(input, "export_0_1", new byte[] {1});
        send(input, "export_0_2", new byte[] {2});
        assertEquals(List.of("export_0_1", "export_0_2"), readThrough("export_0_2"));
    }

    private KafkaConsumer<String, byte[]> uncommittedConsumer() {
        KafkaConsumer<String, byte[]> observer = new KafkaConsumer<>(
                Map.of("bootstrap.servers", bootstrap, "isolation.level", "read_uncommitted"),
                new StringDeserializer(), new ByteArrayDeserializer());
        observer.assign(List.of(new TopicPartition(output, 0)));
        observer.seekToBeginning(observer.assignment());
        return observer;
    }
}
