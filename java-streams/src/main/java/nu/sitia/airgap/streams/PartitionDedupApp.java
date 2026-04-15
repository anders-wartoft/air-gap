package nu.sitia.airgap.streams;

// File: PartitionDedupApp.java
// Build with Kafka Streams 3.9+ and Java 17+
//
// What this does
//  - Consumes from RAW_TOPICS where key = UDP offset (Long) and value = bytes (payload)
//  - Uses a per-partition state store to drop duplicates by UDP offset
//  - Invokes your gap detector (you plug in the implementation) to emit missing ranges
//  - Forwards unique records to CLEAN_TOPIC on the SAME PARTITION as input
//  - Emits gap notifications to GAP_TOPIC (also on the SAME PARTITION)
//  - State is persisted in Kafka changelog topics (one state store per partition task)
//
// How to scale
//  - Run N identical app instances (same application.id), set num.stream.threads=1 per instance
//  - Kafka Streams will assign one input partition per active task/instance
//  - Configure num.standby.replicas >= 1 for warm failover of state
//
// Notes
//  - Exactly-once processing (EoS v2) is enabled in the sample config
//  - If you want topic compaction for CLEAN_TOPIC, configure that topic in Kafka (cleanup.policy=compact)
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.apache.kafka.common.serialization.Serdes;
import org.apache.kafka.clients.admin.Admin;
import org.apache.kafka.clients.admin.AdminClientConfig;
import org.apache.kafka.streams.KafkaStreams;
import org.apache.kafka.streams.StreamsBuilder;
import org.apache.kafka.streams.StreamsConfig;
import org.apache.kafka.streams.Topology;
import org.apache.kafka.streams.errors.MissingSourceTopicException;
import org.apache.kafka.streams.errors.StreamsUncaughtExceptionHandler;
import org.apache.kafka.streams.kstream.Consumed;
import org.apache.kafka.streams.kstream.KStream;
import org.apache.kafka.streams.kstream.Named;
import org.apache.kafka.streams.state.KeyValueStore;
import org.apache.kafka.streams.state.StoreBuilder;
import org.apache.kafka.streams.state.Stores;
import org.apache.kafka.streams.state.KeyValueBytesStoreSupplier;
import org.apache.kafka.streams.processor.api.Record;
import org.apache.kafka.streams.processor.Cancellable;

import nu.sitia.airgap.gapdetector.GapDetector;

import java.time.Duration;
import java.net.InetAddress;
import java.net.URI;
import java.util.ArrayList;
import java.util.Collection;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

import com.fasterxml.jackson.databind.ObjectMapper;

public class PartitionDedupApp {
    // Configurable window size and max windows for GapDetector
    public static final long WINDOW_SIZE = Long.parseLong(System.getenv().getOrDefault("WINDOW_SIZE", "1000"));
    public static final int MAX_WINDOWS = Integer.parseInt(System.getenv().getOrDefault("MAX_WINDOWS", "5"));

    // Track partitions currently assigned to this instance for the raw topic
    private static final java.util.Set<Integer> assignedRawPartitions = java.util.Collections
            .synchronizedSet(new java.util.HashSet<>());

    // SLF4J logger for this class
    private static final Logger LOG = LoggerFactory.getLogger(PartitionDedupApp.class);
    private static final String RUN_ID = UUID.randomUUID().toString().substring(0, 8);
    // Support multiple input topics for Merge/Fan-in pattern
    public static final String RAW_TOPICS = System.getenv().getOrDefault("RAW_TOPICS", "transfer");
    public static final String CLEAN_TOPIC = System.getenv().getOrDefault("CLEAN_TOPIC", "dedup"); // deduped output
                                                                                                   // topic
    public static final String GAP_TOPIC = System.getenv().getOrDefault("GAP_TOPIC", "gaps"); // gap notifications topic
    public static final String BOOTSTRAP_SERVERS = System.getenv().getOrDefault("BOOTSTRAP_SERVERS",
            "kafka-downstream.sitia.nu:9092");
    public static final String STATE_DIR_CONFIG = System.getenv().getOrDefault("STATE_DIR_CONFIG",
            "/tmp/var/lib/kafka-streams/state");
    public static final String APPLICATION_ID = System.getenv().getOrDefault("APPLICATION_ID", "dedup-gap-app");
    // Allow commit interval to be set via env var (default 5 seconds)
    public static final long COMMIT_INTERVAL_MS = Long
            .parseLong(System.getenv().getOrDefault("COMMIT_INTERVAL_MS", "5000"));

    // public static final String STORE_SEEN = "seen-offsets-store"; // key: Long
    // offset, value: byte (marker)
    public static final String STORE_GAP = "gap-tracker-store"; // gap detector needs its own state
    public static final String GAP_EMIT_INTERVAL_SEC = System.getenv().getOrDefault("GAP_EMIT_INTERVAL_SEC", "60");

    // Persistence interval in milliseconds (configurable)
    public static final long PERSIST_INTERVAL_MS = Long
            .parseLong(System.getenv().getOrDefault("PERSIST_INTERVAL_MS", "5000"));

        // If true, terminate process when Kafka connectivity cannot be established or is lost
        public static final boolean FAIL_FAST = Boolean
            .parseBoolean(System.getenv().getOrDefault("FAIL_FAST", "false"));
        public static final long FAIL_FAST_STARTUP_TIMEOUT_MS = Long
            .parseLong(System.getenv().getOrDefault("FAIL_FAST_STARTUP_TIMEOUT_MS", "30000"));
        public static final long FAIL_FAST_CHECK_INTERVAL_MS = Long
            .parseLong(System.getenv().getOrDefault("FAIL_FAST_CHECK_INTERVAL_MS", "10000"));
        public static final long FAIL_FAST_CHECK_TIMEOUT_MS = Long
            .parseLong(System.getenv().getOrDefault("FAIL_FAST_CHECK_TIMEOUT_MS", "5000"));
        public static final long RETRY_BACKOFF_MS = Long
            .parseLong(System.getenv().getOrDefault("RETRY_BACKOFF_MS", "5000"));
        public static final long RETRY_BACKOFF_MAX_MS = Long
            .parseLong(System.getenv().getOrDefault("RETRY_BACKOFF_MAX_MS", "60000"));
        public static final int RETRY_BACKOFF_JITTER_PCT = Integer
            .parseInt(System.getenv().getOrDefault("RETRY_BACKOFF_JITTER_PCT", "20"));

    // Add environment variable for missing event reporting interval
    public static final long MISSING_REPORT_INTERVAL_SEC = Long
            .parseLong(System.getenv().getOrDefault("MISSING_REPORT_INTERVAL_SEC", "60"));

    /**
     * JMX MBean interface for exposing the Properties (props) variable
     */
    public interface PropsMBean {
        String getAllProperties();
    }

    /**
     * Implementation of the PropsMBean
     */
    public static class Props implements PropsMBean {
        private final Properties props;

        public Props(Properties props) {
            this.props = props;
        }

        @Override
        public String getAllProperties() {
            StringBuilder sb = new StringBuilder();
            for (String name : props.stringPropertyNames()) {
                sb.append(name).append("=").append(props.getProperty(name)).append("\n");
            }
            return sb.toString();
        }
    }

    /**
     * JMX MBean interface for PartitionDedupApp configuration
     */
    interface PartitionDedupAppConfigMBean {
        long getWindowSize();

        int getMaxWindows();

        String getRawTopics();

        String getCleanTopic();

        String getGapTopic();

        String getBootstrapServers();

        String getStateDirConfig();

        String getApplicationId();

        int getNumStreamThreads();

        int getNumStandbyReplicas();

        int getCommitIntervalMs();

        String getProcessingGuarantee();
    }

    /**
     * Implementation of the config MBean
     */
    static class PartitionDedupAppConfig implements PartitionDedupAppConfigMBean {
        public long getWindowSize() {
            return WINDOW_SIZE;
        }

        public int getMaxWindows() {
            return MAX_WINDOWS;
        }

        private final Properties props;

        PartitionDedupAppConfig(Properties props) {
            this.props = props;
        }

        public String getRawTopics() {
            return RAW_TOPICS;
        }

        public String getCleanTopic() {
            return CLEAN_TOPIC;
        }

        public String getGapTopic() {
            return GAP_TOPIC;
        }

        public String getBootstrapServers() {
            return BOOTSTRAP_SERVERS;
        }

        public String getStateDirConfig() {
            return STATE_DIR_CONFIG;
        }

        public String getApplicationId() {
            return props.getProperty(StreamsConfig.APPLICATION_ID_CONFIG, "dedup-gap-app");
        }

        public int getNumStreamThreads() {
            return Integer.parseInt(props.getProperty(StreamsConfig.NUM_STREAM_THREADS_CONFIG, "1"));
        }

        public int getNumStandbyReplicas() {
            return Integer.parseInt(props.getProperty(StreamsConfig.NUM_STANDBY_REPLICAS_CONFIG, "1"));
        }

        public int getCommitIntervalMs() {
            return Integer.parseInt(props.getProperty(StreamsConfig.COMMIT_INTERVAL_MS_CONFIG, "60000"));
        }

        public String getProcessingGuarantee() {
            return props.getProperty(StreamsConfig.PROCESSING_GUARANTEE_CONFIG, "exactly_once_v2");
        }

        public int getGapEmitIntervalSec() {
            return Integer.parseInt(GAP_EMIT_INTERVAL_SEC);
        }

        public long getPersistIntervalMs() {
            return PERSIST_INTERVAL_MS;
        }
    }

    /**
     * JMX MBean interface for GapDetector statistics
     */
    public interface GapDetectorStatsMBean {
        int getNumGapDetectors();

        String[] getGapDetectorKeys();

        String getGapDetectorWindows(String key);
    }

    /**
     * Implementation of the MBean for JMX
     */
    public static class GapDetectorStats implements GapDetectorStatsMBean {
        private final Map<String, GapDetector> gapDetectors;

        public GapDetectorStats(Map<String, GapDetector> gapDetectors) {
            this.gapDetectors = gapDetectors;
        }

        @Override
        public int getNumGapDetectors() {
            return gapDetectors.size();
        }

        @Override
        public String[] getGapDetectorKeys() {
            return gapDetectors.keySet().toArray(new String[0]);
        }

        @Override
        public String getGapDetectorWindows(String key) {
            GapDetector gd = gapDetectors.get(key);
            if (gd == null)
                return "Not found";
            StringBuilder sb = new StringBuilder();
            for (Map<String, Number> win : gd.listWindows()) {
                sb.append(win.toString()).append("\n");
            }
            return sb.toString();
        }
    }

    public static void main(String[] args) {
        System.out.println("Starting PartitionDedupApp... runId=" + RUN_ID);
        Properties props = new Properties();
        props.put(StreamsConfig.APPLICATION_ID_CONFIG, APPLICATION_ID);
        props.put(StreamsConfig.BOOTSTRAP_SERVERS_CONFIG, BOOTSTRAP_SERVERS);
        props.put(StreamsConfig.DEFAULT_KEY_SERDE_CLASS_CONFIG, Serdes.String().getClass());
        props.put(StreamsConfig.DEFAULT_VALUE_SERDE_CLASS_CONFIG, Serdes.ByteArray().getClass());

        // One stream thread per instance so each instance maps cleanly to a partition
        // task
        props.put(StreamsConfig.NUM_STREAM_THREADS_CONFIG, 1);

        // Enable exactly-once to avoid double-emits on retried commits
        props.put(StreamsConfig.PROCESSING_GUARANTEE_CONFIG, StreamsConfig.EXACTLY_ONCE_V2);

        // Faster, cooperative rebalances (minimize disruption)
        // props.put(StreamsConfig.UPGRADE_FROM_CONFIG, null); // ensure not upgrading
        // legacy
        props.put(StreamsConfig.STATE_DIR_CONFIG, STATE_DIR_CONFIG);

        // Warm standby replicas for faster failover of state (tune as desired)
        props.put(StreamsConfig.NUM_STANDBY_REPLICAS_CONFIG, 1);

        // Optional: commit interval (EOS v2 ignores this for transactional commits)
        props.put(StreamsConfig.COMMIT_INTERVAL_MS_CONFIG, COMMIT_INTERVAL_MS);

        // Allow Kafka client security/tls/sasl configuration via environment variables.
        // Any environment variable starting with KAFKA_ will be converted to a Kafka
        // property
        // by lowercasing the name and replacing '_' with '.'. Example:
        // KAFKA_SECURITY_PROTOCOL=SSL -> security.protocol=SSL
        // KAFKA_SASL_MECHANISM=PLAIN -> sasl.mechanism=PLAIN
        // KAFKA_SASL_JAAS_CONFIG=org.apache.kafka.common.security.plain.PlainLoginModule
        // required ...
        // KAFKA_SSL_TRUSTSTORE_LOCATION=/etc/kafka/client.truststore.jks ->
        // ssl.truststore.location=...
        for (Map.Entry<String, String> e : System.getenv().entrySet()) {
            String k = e.getKey();
            if (k.startsWith("KAFKA_")) {
                String propName = k.substring("KAFKA_".length()).toLowerCase().replace('_', '.');
                String propVal = e.getValue();
                props.put(propName, propVal);
                // Redact values for any keys that contain PASSWORD (case-insensitive) when
                // logging
                String displayVal = propVal;
                if (k.toUpperCase().contains("PASSWORD") || propName.toUpperCase().contains("PASSWORD")) {
                    displayVal = "********";
                }
                LOG.debug("Injected Kafka property from env {} -> {}={}", k, propName, displayVal);
            }
        }

        validateRuntimeConfiguration();

        LOG.info("Starting PartitionDedupApp... runId={}", RUN_ID);

        LOG.info("BOOTSTRAP_SERVERS={}", BOOTSTRAP_SERVERS);
        LOG.info("RAW_TOPICS={}", RAW_TOPICS);
        LOG.info("CLEAN_TOPIC={}", CLEAN_TOPIC);
        LOG.info("GAP_TOPIC={}", GAP_TOPIC);
        LOG.info("STATE_DIR_CONFIG={}", STATE_DIR_CONFIG);
        LOG.info("WINDOW_SIZE={}", WINDOW_SIZE);
        LOG.info("MAX_WINDOWS={}", MAX_WINDOWS);
        LOG.info("GAP_EMIT_INTERVAL_SEC={}", GAP_EMIT_INTERVAL_SEC);
        LOG.info("PERSIST_INTERVAL_MS={}", PERSIST_INTERVAL_MS);
        LOG.info("FAIL_FAST={}", FAIL_FAST);
        LOG.info("FAIL_FAST_STARTUP_TIMEOUT_MS={}", FAIL_FAST_STARTUP_TIMEOUT_MS);
        LOG.info("FAIL_FAST_CHECK_INTERVAL_MS={}", FAIL_FAST_CHECK_INTERVAL_MS);
        LOG.info("FAIL_FAST_CHECK_TIMEOUT_MS={}", FAIL_FAST_CHECK_TIMEOUT_MS);
        LOG.info("RETRY_BACKOFF_MS={}", RETRY_BACKOFF_MS);
        LOG.info("RETRY_BACKOFF_MAX_MS={}", RETRY_BACKOFF_MAX_MS);
        LOG.info("RETRY_BACKOFF_JITTER_PCT={}", RETRY_BACKOFF_JITTER_PCT);
        LOG.info("Application ID: {}", props.get(StreamsConfig.APPLICATION_ID_CONFIG));
        LOG.info("Num Stream Threads: {}", props.get(StreamsConfig.NUM_STREAM_THREADS_CONFIG));
        LOG.info("Num Standby Replicas: {}", props.get(StreamsConfig.NUM_STANDBY_REPLICAS_CONFIG));
        LOG.info("Processing Guarantee: {}", props.get(StreamsConfig.PROCESSING_GUARANTEE_CONFIG));
        LOG.info("Commit Interval (ms): {}", props.get(StreamsConfig.COMMIT_INTERVAL_MS_CONFIG));
        LOG.info("State Dir: {}", props.get(StreamsConfig.STATE_DIR_CONFIG));
        LOG.info("Startup capability summary: runId={}, topicsCount={}, failFast={}, connectivityMonitor={}, kafkaSecurity={}",
            RUN_ID,
            RAW_TOPICS.split(",").length,
            FAIL_FAST,
            FAIL_FAST ? "enabled" : "disabled",
            resolveKafkaSecurityMode(props));

        // Register DynamicMBean for exposing each property as a JMX attribute
        JmxSupport.registerPropsMBean(props, RAW_TOPICS, CLEAN_TOPIC, GAP_TOPIC, APPLICATION_ID, assignedRawPartitions,
                WINDOW_SIZE, MAX_WINDOWS);

        AtomicBoolean terminateRequested = new AtomicBoolean(false);
        AtomicReference<KafkaStreams> currentStreamsRef = new AtomicReference<>();
        AtomicInteger processExitCode = new AtomicInteger(0);

        Thread shutdownHook = new Thread(() -> {
            terminateRequested.set(true);
            KafkaStreams currentStreams = currentStreamsRef.get();
            if (currentStreams != null) {
                try {
                    currentStreams.close(Duration.ofSeconds(10));
                } catch (Exception e) {
                    LOG.warn("Error while closing Kafka Streams in JVM shutdown hook", e);
                }
            }
        }, "streams-shutdown-hook");

        try {
            Runtime.getRuntime().addShutdownHook(shutdownHook);

            int restartAttempt = 0;
            while (!terminateRequested.get()) {
                restartAttempt++;

                if (!FAIL_FAST) {
                    List<String> missingSourceTopics = getMissingSourceTopics(props);
                    if (!missingSourceTopics.isEmpty()) {
                        long retryDelayMs = applyJitter(Math.max(0L, RETRY_BACKOFF_MS));
                        LOG.warn("Source topics missing: {}. Delaying startup and retrying in {} ms",
                                missingSourceTopics,
                                retryDelayMs);
                        try {
                            Thread.sleep(retryDelayMs);
                        } catch (InterruptedException e) {
                            Thread.currentThread().interrupt();
                            terminateRequested.set(true);
                            break;
                        }
                        continue;
                    }
                }

                Topology topology = buildTopology();
                KafkaStreams streams = new KafkaStreams(topology, props);
                currentStreamsRef.set(streams);

                CountDownLatch shutdownLatch = new CountDownLatch(1);
                CountDownLatch runningLatch = new CountDownLatch(1);
                AtomicBoolean shuttingDown = new AtomicBoolean(false);
                AtomicInteger exitCode = new AtomicInteger(0);
                AtomicInteger retryAttempt = new AtomicInteger(0);
                ScheduledExecutorService failFastScheduler = null;

                LOG.info("Starting KafkaStreams instance (runId={}, attempt={}, FAIL_FAST={})", RUN_ID, restartAttempt,
                    FAIL_FAST);

                try {
                    streams.setStateListener((newState, oldState) -> {
                        LOG.debug("Kafka Streams state transition: {} -> {}", oldState, newState);

                        if (newState == KafkaStreams.State.RUNNING) {
                            runningLatch.countDown();
                            retryAttempt.set(0);
                        }

                        if (newState == KafkaStreams.State.ERROR && FAIL_FAST) {
                            initiateShutdown("Kafka Streams entered ERROR state", streams, shutdownLatch, shuttingDown,
                                    exitCode, 1);
                        }

                        if (newState == KafkaStreams.State.NOT_RUNNING) {
                            shutdownLatch.countDown();
                        }
                    });

                    streams.setUncaughtExceptionHandler(exception -> {
                        LOG.error("Uncaught Streams exception", exception);
                        if (FAIL_FAST) {
                            initiateShutdown("Uncaught Streams exception", streams, shutdownLatch, shuttingDown,
                                    exitCode, 1);
                            return StreamsUncaughtExceptionHandler.StreamThreadExceptionResponse.SHUTDOWN_CLIENT;
                        }

                        if (exception instanceof MissingSourceTopicException) {
                            LOG.warn(
                                    "Source topics are not available yet (or Kafka metadata is temporarily unavailable). "
                                            + "Retrying stream thread with backoff because FAIL_FAST=false");
                        } else {
                            LOG.warn("Retrying stream thread with backoff because FAIL_FAST=false");
                        }

                        int attempt = retryAttempt.incrementAndGet();
                        long backoffNoJitter = calculateBackoffNoJitter(attempt);
                        long backoffWithJitter = applyJitter(backoffNoJitter);

                        LOG.info("Retry attempt={} sleeping {} ms (base={} ms, max={} ms, jitter={}%)",
                                attempt,
                                backoffWithJitter,
                                backoffNoJitter,
                                RETRY_BACKOFF_MAX_MS,
                                RETRY_BACKOFF_JITTER_PCT);

                        try {
                            Thread.sleep(backoffWithJitter);
                        } catch (InterruptedException interruptedException) {
                            Thread.currentThread().interrupt();
                            LOG.warn("Retry backoff interrupted; replacing stream thread immediately");
                        }

                        return StreamsUncaughtExceptionHandler.StreamThreadExceptionResponse.REPLACE_THREAD;
                    });

                    streams.start();
                    LOG.info("PartitionDedupApp started (runId={}, FAIL_FAST={})", RUN_ID, FAIL_FAST);

                    if (FAIL_FAST) {
                        boolean started = runningLatch.await(FAIL_FAST_STARTUP_TIMEOUT_MS, TimeUnit.MILLISECONDS);
                        if (!started) {
                            LOG.error("FAIL_FAST startup timeout: did not reach RUNNING state within {} ms",
                                    FAIL_FAST_STARTUP_TIMEOUT_MS);
                            initiateShutdown("FAIL_FAST startup timeout", streams, shutdownLatch, shuttingDown,
                                    exitCode, 1);
                        } else {
                            failFastScheduler = startFailFastConnectivityMonitor(props, streams, shutdownLatch,
                                    shuttingDown, exitCode);
                        }
                    }

                    if (!shuttingDown.get()) {
                        Collection<org.apache.kafka.streams.StreamsMetadata> storeMetadata = streams
                                .streamsMetadataForStore(STORE_GAP);
                        LOG.debug("All metadata for store {}: {}", STORE_GAP, storeMetadata);
                    }

                    shutdownLatch.await();
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    terminateRequested.set(true);
                    initiateShutdown("Main thread interrupted", streams, shutdownLatch, shuttingDown, exitCode, 1);
                } catch (Exception e) {
                    LOG.error("KafkaStreams lifecycle error", e);
                    if (FAIL_FAST) {
                        terminateRequested.set(true);
                        initiateShutdown("FAIL_FAST lifecycle error", streams, shutdownLatch, shuttingDown, exitCode,
                                1);
                    } else {
                        initiateShutdown("Lifecycle error with FAIL_FAST=false; restarting", streams, shutdownLatch,
                                shuttingDown, exitCode, 0);
                    }
                } finally {
                    if (failFastScheduler != null) {
                        failFastScheduler.shutdownNow();
                    }
                    if (shuttingDown.compareAndSet(false, true)) {
                        try {
                            streams.close(Duration.ofSeconds(10));
                        } catch (Exception closeError) {
                            LOG.warn("Error while closing Kafka Streams in finally block", closeError);
                        }
                    }
                    currentStreamsRef.compareAndSet(streams, null);
                }

                if (exitCode.get() != 0) {
                    processExitCode.compareAndSet(0, exitCode.get());
                    terminateRequested.set(true);
                }

                if (terminateRequested.get()) {
                    break;
                }

                if (FAIL_FAST) {
                    LOG.info("FAIL_FAST=true and KafkaStreams stopped; terminating process");
                    break;
                }

                long restartDelayMs = applyJitter(Math.max(0L, RETRY_BACKOFF_MS));
                LOG.warn("KafkaStreams reached NOT_RUNNING; restarting in {} ms (runId={}, attempt={})",
                        restartDelayMs,
                    RUN_ID,
                        restartAttempt);
                try {
                    Thread.sleep(restartDelayMs);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    terminateRequested.set(true);
                    break;
                }
            }
        } finally {
            try {
                Runtime.getRuntime().removeShutdownHook(shutdownHook);
            } catch (IllegalStateException ignored) {
                // JVM is already shutting down
            } catch (Exception removeHookError) {
                LOG.debug("Failed to remove shutdown hook", removeHookError);
            }

            if (processExitCode.get() != 0) {
                LOG.info("Exiting process with code {} due to FAIL_FAST condition (runId={})", processExitCode.get(),
                        RUN_ID);
                System.exit(processExitCode.get());
            }
        }
    }

    private static String resolveKafkaSecurityMode(Properties props) {
        String securityProtocol = props.getProperty("security.protocol", "PLAINTEXT");
        String saslMechanism = props.getProperty("sasl.mechanism");
        if (saslMechanism != null && !saslMechanism.isBlank()) {
            return securityProtocol + "+SASL(" + saslMechanism + ")";
        }
        return securityProtocol;
    }

    private static long calculateBackoffNoJitter(int attempt) {
        int normalizedAttempt = Math.max(1, attempt);
        long baseBackoff = Math.max(0L, RETRY_BACKOFF_MS);
        long maxBackoff = Math.max(baseBackoff, RETRY_BACKOFF_MAX_MS);

        if (normalizedAttempt <= 1) {
            return baseBackoff;
        }

        int exponent = Math.min(normalizedAttempt - 1, 30);
        long multiplier = 1L << exponent;

        if (baseBackoff == 0L) {
            return 0L;
        }

        if (multiplier > Long.MAX_VALUE / baseBackoff) {
            return maxBackoff;
        }

        long candidate = baseBackoff * multiplier;
        return Math.min(maxBackoff, candidate);
    }

    private static long applyJitter(long backoffMs) {
        if (backoffMs <= 0L) {
            return 0L;
        }

        int jitterPct = Math.max(0, RETRY_BACKOFF_JITTER_PCT);
        if (jitterPct == 0) {
            return backoffMs;
        }

        double jitterFraction = jitterPct / 100.0;
        long delta = Math.max(1L, Math.round(backoffMs * jitterFraction));
        long minDelay = Math.max(0L, backoffMs - delta);
        long maxDelay = Math.max(minDelay, backoffMs + delta);

        return ThreadLocalRandom.current().nextLong(minDelay, maxDelay + 1);
    }

    private static void validateRuntimeConfiguration() {
        validateRuntimeConfigurationValues(
                RETRY_BACKOFF_MS,
                RETRY_BACKOFF_MAX_MS,
                RETRY_BACKOFF_JITTER_PCT,
                FAIL_FAST,
                FAIL_FAST_STARTUP_TIMEOUT_MS,
                FAIL_FAST_CHECK_INTERVAL_MS,
                FAIL_FAST_CHECK_TIMEOUT_MS);
    }

    static void validateRuntimeConfigurationValues(
            long retryBackoffMs,
            long retryBackoffMaxMs,
            int retryBackoffJitterPct,
            boolean failFast,
            long failFastStartupTimeoutMs,
            long failFastCheckIntervalMs,
            long failFastCheckTimeoutMs) {
        if (retryBackoffMs < 0L) {
            throw new IllegalArgumentException("RETRY_BACKOFF_MS must be >= 0");
        }
        if (retryBackoffMaxMs < retryBackoffMs) {
            throw new IllegalArgumentException("RETRY_BACKOFF_MAX_MS must be >= RETRY_BACKOFF_MS");
        }
        if (retryBackoffJitterPct < 0 || retryBackoffJitterPct > 100) {
            throw new IllegalArgumentException("RETRY_BACKOFF_JITTER_PCT must be between 0 and 100");
        }

        if (failFast) {
            if (failFastStartupTimeoutMs <= 0L) {
                throw new IllegalArgumentException("FAIL_FAST_STARTUP_TIMEOUT_MS must be > 0 when FAIL_FAST=true");
            }
            if (failFastCheckIntervalMs <= 0L) {
                throw new IllegalArgumentException("FAIL_FAST_CHECK_INTERVAL_MS must be > 0 when FAIL_FAST=true");
            }
            if (failFastCheckTimeoutMs <= 0L) {
                throw new IllegalArgumentException("FAIL_FAST_CHECK_TIMEOUT_MS must be > 0 when FAIL_FAST=true");
            }
        }
    }

    private static void initiateShutdown(String reason, KafkaStreams streams, CountDownLatch shutdownLatch,
            AtomicBoolean shuttingDown, AtomicInteger exitCode, int newExitCode) {
        if (!shuttingDown.compareAndSet(false, true)) {
            return;
        }

        if (newExitCode != 0) {
            exitCode.compareAndSet(0, newExitCode);
        }

        LOG.info("Initiating shutdown: {}", reason);
        try {
            streams.close(Duration.ofSeconds(10));
        } catch (Exception e) {
            LOG.warn("Error while closing Kafka Streams during shutdown", e);
        } finally {
            shutdownLatch.countDown();
        }
    }

    private static ScheduledExecutorService startFailFastConnectivityMonitor(Properties streamProps, KafkaStreams streams,
            CountDownLatch shutdownLatch, AtomicBoolean shuttingDown, AtomicInteger exitCode) {
        Properties adminProps = createAdminProps(streamProps);
        AtomicInteger probeCounter = new AtomicInteger(0);

        ScheduledExecutorService scheduler = Executors.newSingleThreadScheduledExecutor(r -> {
            Thread t = new Thread(r, "fail-fast-kafka-monitor");
            t.setDaemon(true);
            return t;
        });

        scheduler.scheduleAtFixedRate(() -> {
            if (shuttingDown.get()) {
                return;
            }

            List<String> unresolvedHosts = getUnresolvedBootstrapHosts(streamProps);
            if (!unresolvedHosts.isEmpty()) {
                LOG.error("FAIL_FAST bootstrap DNS resolution failed for hosts {}; terminating process",
                        unresolvedHosts);
                initiateShutdown("FAIL_FAST bootstrap DNS resolution failed", streams, shutdownLatch, shuttingDown,
                        exitCode, 1);
                return;
            }

            try (Admin admin = Admin.create(adminProps)) {
                long start = System.nanoTime();
                admin.describeCluster().nodes().get(FAIL_FAST_CHECK_TIMEOUT_MS, TimeUnit.MILLISECONDS);
                int count = probeCounter.incrementAndGet();
                if (LOG.isDebugEnabled() && (count == 1 || count % 6 == 0)) {
                    long durationMs = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - start);
                    LOG.debug("FAIL_FAST connectivity probe ok (runId={}, probeCount={}, durationMs={})",
                            RUN_ID,
                            count,
                            durationMs);
                }
            } catch (Exception e) {
                LOG.error("FAIL_FAST connectivity check failed; terminating process", e);
                initiateShutdown("FAIL_FAST Kafka connectivity check failed", streams, shutdownLatch, shuttingDown,
                        exitCode, 1);
            }
        }, 0, FAIL_FAST_CHECK_INTERVAL_MS, TimeUnit.MILLISECONDS);

        return scheduler;
    }

    private static List<String> getUnresolvedBootstrapHosts(Properties streamProps) {
        String bootstrapServers = streamProps.getProperty(StreamsConfig.BOOTSTRAP_SERVERS_CONFIG, "");
        List<String> unresolvedHosts = new ArrayList<>();

        for (String server : bootstrapServers.split(",")) {
            String trimmed = server.trim();
            if (trimmed.isEmpty()) {
                continue;
            }

            String host = extractHostFromBootstrapServer(trimmed);
            if (host == null || host.isEmpty()) {
                unresolvedHosts.add(trimmed);
                continue;
            }

            try {
                InetAddress.getAllByName(host);
            } catch (Exception e) {
                unresolvedHosts.add(host);
            }
        }

        return unresolvedHosts;
    }

    private static String extractHostFromBootstrapServer(String bootstrapServer) {
        String candidate = bootstrapServer.trim();
        if (candidate.isEmpty()) {
            return "";
        }

        try {
            if (candidate.contains("://")) {
                URI uri = URI.create(candidate);
                return uri.getHost();
            }

            if (candidate.startsWith("[")) {
                int endBracket = candidate.indexOf(']');
                if (endBracket > 1) {
                    return candidate.substring(1, endBracket);
                }
                return candidate;
            }

            int lastColon = candidate.lastIndexOf(':');
            if (lastColon > 0) {
                return candidate.substring(0, lastColon);
            }

            return candidate;
        } catch (Exception e) {
            LOG.debug("Unable to parse bootstrap server entry '{}': {}", bootstrapServer, e.getMessage());
            return candidate;
        }
    }

    private static Properties createAdminProps(Properties streamProps) {
        Properties adminProps = new Properties();
        adminProps.put(AdminClientConfig.BOOTSTRAP_SERVERS_CONFIG,
                streamProps.getProperty(StreamsConfig.BOOTSTRAP_SERVERS_CONFIG));

        for (String key : streamProps.stringPropertyNames()) {
            if (key.startsWith("security.") || key.startsWith("ssl.") || key.startsWith("sasl.")) {
                adminProps.put(key, streamProps.getProperty(key));
            }
        }

        return adminProps;
    }

    private static List<String> getMissingSourceTopics(Properties streamProps) {
        List<String> configuredTopics = new ArrayList<>();
        for (String topic : RAW_TOPICS.split(",")) {
            String trimmed = topic.trim();
            if (!trimmed.isEmpty()) {
                configuredTopics.add(trimmed);
            }
        }

        if (configuredTopics.isEmpty()) {
            return List.of("<none configured in RAW_TOPICS>");
        }

        Properties adminProps = createAdminProps(streamProps);
        try (Admin admin = Admin.create(adminProps)) {
            java.util.Set<String> existingTopics = admin.listTopics().names()
                    .get(FAIL_FAST_CHECK_TIMEOUT_MS, TimeUnit.MILLISECONDS);

            List<String> missingTopics = new ArrayList<>();
            for (String configuredTopic : configuredTopics) {
                if (!existingTopics.contains(configuredTopic)) {
                    missingTopics.add(configuredTopic);
                }
            }
            return missingTopics;
        } catch (Exception e) {
            LOG.warn("Unable to verify source topics before startup; treating as unavailable and retrying", e);
            return configuredTopics;
        }
    }

    public static Topology buildTopology() {
        StreamsBuilder builder = new StreamsBuilder();

        // Persistent store for our gap detector
        KeyValueBytesStoreSupplier gapSupplier = Stores.persistentKeyValueStore(STORE_GAP);
        StoreBuilder<KeyValueStore<String, byte[]>> gapStoreBuilder = Stores
                .keyValueStoreBuilder(gapSupplier, Serdes.String(), Serdes.ByteArray())
                .withCachingEnabled()
                .withLoggingEnabled(new HashMap<>());
        builder.addStateStore(gapStoreBuilder);

        // Parse RAW_TOPICS env
        String[] inputTopics = RAW_TOPICS.split(",");
        List<String> topicList = new ArrayList<>();
        for (String t : inputTopics) {
            String trimmed = t.trim();
            if (!trimmed.isEmpty())
                topicList.add(trimmed);
        }
        if (topicList.isEmpty()) {
            throw new IllegalArgumentException("No input topics specified in RAW_TOPICS");
        }

        // One stream per topic, then merge them
        List<KStream<String, byte[]>> streams = new ArrayList<>();
        for (String topic : topicList) {
            streams.add(builder.stream(topic, Consumed.with(Serdes.String(), Serdes.ByteArray())));
        }

        KStream<String, byte[]> mergedSource = streams.get(0);
        for (int i = 1; i < streams.size(); i++) {
            mergedSource = mergedSource.merge(streams.get(i));
        }

        // Send merged stream to the processor
        mergedSource.process(
                () -> new DedupAndGapProcessor(),
                Named.as("dedup-gap"),
                STORE_GAP);

        Topology topology = builder.build();

        // Explicit sinks
        topology.addSink("deduped-sink", CLEAN_TOPIC,
                Serdes.String().serializer(), Serdes.ByteArray().serializer(), "dedup-gap");
        topology.addSink("gaps-sink", GAP_TOPIC,
                Serdes.String().serializer(), Serdes.ByteArray().serializer(), "dedup-gap");

        return topology;
    }

    /**
     * Transformer that performs per-partition dedup via a state store and consults
     * a GapDetector.
     * It forwards unique records to CLEAN_TOPIC and gap signals to GAP_TOPIC
     * as the input record using ProcessorContext.
     */
    public static class DedupAndGapProcessor
            implements org.apache.kafka.streams.processor.api.Processor<String, byte[], String, byte[]> {
        private int partition = -1;
        private final long persistIntervalMs;
        private static final Logger LOG = LoggerFactory.getLogger(DedupAndGapProcessor.class);
        private final long windowSize;
        private final int maxWindows;
        private ObjectMapper MAPPER = new ObjectMapper();

        private org.apache.kafka.streams.processor.api.ProcessorContext<String, byte[]> context;
        private KeyValueStore<String, byte[]> gapStore;
        private Map<String, GapDetector> gapDetectors = new HashMap<>();
        private Cancellable persistSchedule;
        private Cancellable emitSchedule;

        private static final Map<Integer, Long> lastTotalMissing = new java.util.concurrent.ConcurrentHashMap<>();
        private static final Map<Integer, Long> lastReceived = new java.util.concurrent.ConcurrentHashMap<>();
        private static final Map<Integer, Long> lastEmitted = new java.util.concurrent.ConcurrentHashMap<>();
        private static final Map<Integer, Long> totalReceived = new java.util.concurrent.ConcurrentHashMap<>();
        private static final Map<Integer, Long> totalEmitted = new java.util.concurrent.ConcurrentHashMap<>();
        private static final Map<Integer, Long> totalDuplicates = new java.util.concurrent.ConcurrentHashMap<>();
        private static final Map<Integer, Long> totalGapFill = new java.util.concurrent.ConcurrentHashMap<>();
        private static final Map<Integer, Long> totalBelowRange = new java.util.concurrent.ConcurrentHashMap<>();
        private static final Map<Integer, Long> totalNonStandardKeyPassthrough = new java.util.concurrent.ConcurrentHashMap<>();
        // Removed unused reportScheduled field
        // Use the shared ObjectMapper instance for reporting
        private static final ObjectMapper REPORT_MAPPER = new ObjectMapper();

        public DedupAndGapProcessor() {
            this.persistIntervalMs = PERSIST_INTERVAL_MS;
            this.windowSize = WINDOW_SIZE;
            this.maxWindows = MAX_WINDOWS;
            // Register DynamicMBean for gapDetectors
            LOG.info("Registering Processor MBean");
            JmxSupport.registerProcessorMBean(this);
        }

        public Map<String, GapDetector> getGapDetectors() {
            return gapDetectors;
        }

        @Override
        @SuppressWarnings("unchecked")
        public void init(org.apache.kafka.streams.processor.api.ProcessorContext<String, byte[]> context) {
            this.context = context;
            this.partition = context.taskId().partition();
            LOG.debug("Initializing DedupAndGapProcessor for partition {}", partition);
            assignedRawPartitions.add(partition);
            LOG.info("Task assigned partition {} (runId={}); active partitions now {}",
                    partition,
                    RUN_ID,
                    assignedRawPartitions);

            this.gapStore = (KeyValueStore<String, byte[]>) context.getStateStore(STORE_GAP);

            // Load gapDetectors only for this thread's partition key
            try (org.apache.kafka.streams.state.KeyValueIterator<String, byte[]> iter = gapStore.all()) {
                while (iter.hasNext()) {
                    org.apache.kafka.streams.KeyValue<String, byte[]> entry = iter.next();
                    GapDetector gd = deserializeGapDetector(entry.value, entry.key);
                    if (gd != null) {
                        gapDetectors.put(entry.key, gd);
                        LOG.debug("Registered GapDetector for key: {}", entry.key);
                        JmxSupport.registerProcessorMBean(this);
                    }
                }
            }

            // Persist this partition’s detector periodically
            LOG.debug("Scheduling periodic gap detector persistence every {} ms", persistIntervalMs);
            this.persistSchedule = this.context.schedule(
                    java.time.Duration.ofMillis(persistIntervalMs),
                    org.apache.kafka.streams.processor.PunctuationType.WALL_CLOCK_TIME,
                    ts -> {
                        long startNs = System.nanoTime();
                        int persisted = 0;
                        long totalBytes = 0;
                        for (Map.Entry<String, GapDetector> entry : gapDetectors.entrySet()) {
                            try {
                                byte[] data = serializeGapDetector(entry.getValue());
                                if (data != null) {
                                    totalBytes += data.length;
                                }
                                gapStore.put(entry.getKey(), data);
                                persisted++;
                            } catch (Exception e) {
                                LOG.error("Failed to serialize GapDetector for {}", entry.getKey(), e);
                            }
                        }
                        if (LOG.isDebugEnabled()) {
                            long durationMs = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - startNs);
                            LOG.debug("Persisted gap detectors (partition={}, count={}, totalBytes={}, durationMs={})",
                                    partition,
                                    persisted,
                                    totalBytes,
                                    durationMs);
                        }
                    });

            LOG.debug("Emit gap: Started gap detection for partition {}", partition);
            // Emit gaps only for this partition’s detector
            long emitIntervalMs = Long.parseLong(GAP_EMIT_INTERVAL_SEC) * 1000;
            this.emitSchedule = this.context.schedule(
                    java.time.Duration.ofMillis(emitIntervalMs),
                    org.apache.kafka.streams.processor.PunctuationType.WALL_CLOCK_TIME,
                    ts -> emitGapsForPartition(partition));

            // Schedule missing-report once per partition
            context.schedule(
                    java.time.Duration.ofSeconds(MISSING_REPORT_INTERVAL_SEC),
                    org.apache.kafka.streams.processor.PunctuationType.WALL_CLOCK_TIME,
                    ts -> logMissingEventsForThisPartition());
        }

        @Override
        /** Close the processor and release resources */
        public void close() {
            if (emitSchedule != null) {
                emitSchedule.cancel();
                emitSchedule = null;
            }
            if (persistSchedule != null) {
                persistSchedule.cancel();
                persistSchedule = null;
            }
            if (partition >= 0) {
                assignedRawPartitions.remove(partition);
                LOG.info("Task released partition {} (runId={}); active partitions now {}",
                        partition,
                        RUN_ID,
                        assignedRawPartitions);
            }
        }

        public int getPartition() {
            return this.partition;
        }

        /** Emit gaps for all detectors. */
        private void emitGapsForPartition(int partition) {
            // Count missing messages for this partition
            // For this GapDetector, get current gaps
            LOG.debug("[GAP-DEBUG] Partition {}: gapDetectors keys: {}", partition, gapDetectors.keySet());
            int windowsScanned = 0;
            int windowsEmitted = 0;
            long rangesEmitted = 0;
            long missingOffsetsInRanges = 0;
            for (Map.Entry<String, GapDetector> entry : gapDetectors.entrySet()) {
                LOG.debug("[GAP-DEBUG] Checking detector key {} for partition {}", entry.getKey(), partition);
                String key = entry.getKey(); // key is topic_partition
                if (!key.endsWith("_" + partition)) {
                    continue; // Skip detectors not for this partition
                }
                // Extract topic from key (always use topic from event key, not RAW_TOPICS)
                int lastUnderscore = key.lastIndexOf('_');
                String topic = (lastUnderscore > 0) ? key.substring(0, lastUnderscore) : key;
                LOG.debug("[GAP-DEBUG] Key accepted for partition {}: {} (topic: {})", partition, key, topic);
                GapDetector detector = entry.getValue();
                LOG.debug("GapDetector address for key {}: {}", key, System.identityHashCode(detector));
                // Log total missing count for this partition
                LOG.debug("Partition {}: Total missing messages detected: {}", partition, detector.getMissingCounts());

                List<GapDetector.Window> windows = detector.getWindows();
                // For all windows, find gaps
                // Sticky empty emission logic: emit empty gaps once when gaps become empty,
                // reset if non-empty
                for (GapDetector.Window window : windows) {
                    windowsScanned++;
                    List<List<Long>> gaps = window.findGapsFiltered(detector.getMinReceived());
                    long windowMin = window.getMinOffset();
                    String gapKey = topic + "_" + partition + ":" + windowMin;
                    if (window.lastEmittedNonEmpty == null)
                        window.lastEmittedNonEmpty = new boolean[] { true };
                    boolean wasNonEmpty = window.lastEmittedNonEmpty[0];
                    if (!gaps.isEmpty() || wasNonEmpty) {
                        try {
                            Map<String, Object> gapsMap = new HashMap<>();
                            gapsMap.put("topic", topic);
                            gapsMap.put("partition", partition);
                            gapsMap.put("window_min", windowMin);
                            gapsMap.put("window_max", window.getMaxOffset());
                            gapsMap.put("gaps", gaps);
                            String json = MAPPER.writeValueAsString(gapsMap);
                            context.forward(new Record<>(gapKey, json.getBytes(), System.currentTimeMillis()),
                                    "gaps-sink");
                            LOG.debug("Emitted gaps for window {}: {}", gapKey, json);
                            windowsEmitted++;
                            rangesEmitted += gaps.size();
                            for (List<Long> gap : gaps) {
                                if (gap.size() == 1) {
                                    missingOffsetsInRanges += 1;
                                } else if (gap.size() == 2) {
                                    missingOffsetsInRanges += (gap.get(1) - gap.get(0) + 1);
                                }
                            }
                        } catch (Exception e) {
                            LOG.error("Failed to serialize gaps for window {}", gapKey, e);
                        }
                        window.lastEmittedNonEmpty[0] = true;
                    }
                }
            }
            if (LOG.isDebugEnabled()) {
                LOG.debug(
                        "Gap emission cycle summary (partition={}, windowsScanned={}, windowsEmitted={}, rangesEmitted={}, missingOffsetsInRanges={})",
                        partition,
                        windowsScanned,
                        windowsEmitted,
                        rangesEmitted,
                        missingOffsetsInRanges);
            }
        }

        @Override
        public void process(org.apache.kafka.streams.processor.api.Record<String, byte[]> record) {
            String key = record.key();
            byte[] value = record.value();
            if (key == null)
                return; // nothing we can do
            String[] parts = key.split("_");
            if (parts.length != 3) {
                // Non-standard keys are expected for internal passthrough records
                LOG.trace("Non-standard key passthrough key='{}', partsCount={}",
                        key,
                        parts.length);
                totalNonStandardKeyPassthrough.merge(this.partition, 1L, Long::sum);
                context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                return;
            }
            String topic = parts[0];
            int partition;
            long offset;
            try {
                partition = Integer.parseInt(parts[1]);
                offset = Long.parseLong(parts[2]);
            } catch (Exception e) {
                // Non-standard keys are expected for internal passthrough records
                LOG.trace(
                        "Non-standard key passthrough key='{}' (partitionPart='{}', offsetPart='{}', reason='{}')",
                        key,
                        parts[1],
                        parts[2],
                        e.getMessage());
                totalNonStandardKeyPassthrough.merge(this.partition, 1L, Long::sum);
                context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                return;
            }
            LOG.trace("Processing record from topic={}, partition={}, offset={}", topic, partition, offset);
            String topicPartition = topic + "_" + partition;

            // Increment received counter
            totalReceived.merge(partition, 1L, Long::sum);

            GapDetector gapDetector = gapDetectors.get(topicPartition);
            if (gapDetector == null) {
                LOG.warn("No GapDetector found for topicPartition {}, creating a new one", topicPartition);
                gapDetector = new GapDetector(topic, windowSize, maxWindows);
                gapDetectors.put(topicPartition, gapDetector);
                LOG.debug("Registered GapDetector for key: {}", topicPartition);
                // Always re-register the MBean when a new detector is added so JMX sees all
                // partitions
                JmxSupport.registerProcessorMBean(this);
                LOG.debug("Re-registered Processor MBean after adding new partition: {}. Current keys: {}",
                        topicPartition, gapDetectors.keySet());
            }

            int alreadyReceived = gapDetector.check(offset, window -> {
                try {
                    List<List<Long>> gaps = window.findGaps();
                    long windowMin = window.getMinOffset();
                    if (!gaps.isEmpty()) {
                        String gapKey = topic + "_" + partition + ":" + windowMin;
                        Map<String, Object> gapsMap = new HashMap<>();
                        gapsMap.put("topic", topic);
                        gapsMap.put("partition", partition);
                        gapsMap.put("window_min", windowMin);
                        gapsMap.put("window_max", window.getMaxOffset());
                        gapsMap.put("gaps", gaps);
                        String json = MAPPER.writeValueAsString(gapsMap);
                        context.forward(new Record<>(gapKey, json.getBytes(), record.timestamp()), "gaps-sink");
                        LOG.debug("Emitted gaps for window {}: {}", gapKey, json);
                    }
                } catch (OutOfMemoryError oom) {
                    String errorMessage = "OutOfMemoryError while emitting gaps for purged window. Consider increasing MAX_WINDOWS or reducing WINDOW_SIZE. "
                            + oom;
                    LOG.error(errorMessage);
                    String dedupedKey = key + ":oom";
                    context.forward(new Record<>(dedupedKey, errorMessage.getBytes(), record.timestamp()),
                            "deduped-sink");
                } catch (Exception e) {
                    LOG.error("Failed to emit gaps for purged window", e);
                }
            });

            // * alreadyReceived is 1 if this number was already received, 0 if it’s newly
            // received and -1 if it fills a gap.
            // * If the number is less than the lowest known offset, it is considered unseen
            // and -2 is returned.
            //
            // Events with a zero-length payload were sent by upstream with their content
            // intentionally cleared by the input filter. They must still be registered with
            // the gap detector (done above via gapDetector.check) so no gap is reported, but
            // they must NOT be forwarded to the clean topic.
            boolean emptyPayload = (value == null || value.length == 0);
            if (alreadyReceived == 1) {
                LOG.trace("Key already seen {}", key);
                totalDuplicates.merge(partition, 1L, Long::sum);
            } else if (alreadyReceived == 0) {
                if (emptyPayload) {
                    LOG.trace("Dropping empty-payload (filtered) key {}", key);
                } else {
                    LOG.trace("Forwarding unique key {}", key);
                    context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                    totalEmitted.merge(partition, 1L, Long::sum);
                }
            } else if (alreadyReceived == -1) {
                if (emptyPayload) {
                    LOG.trace("Dropping empty-payload (filtered) gap-fill key {}", key);
                } else {
                    LOG.trace("Key {} fills a gap, forwarding", key);
                    context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                    totalEmitted.merge(partition, 1L, Long::sum);
                    totalGapFill.merge(partition, 1L, Long::sum);
                }
            } else if (alreadyReceived == -2) {
                if (emptyPayload) {
                    LOG.trace("Dropping empty-payload (filtered) below-range key {}", key);
                } else {
                    LOG.trace("Key {} is below known range, forwarding", key);
                    context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                    totalEmitted.merge(partition, 1L, Long::sum);
                    totalBelowRange.merge(partition, 1L, Long::sum);
                }
            } else {
                LOG.error("Unexpected return value {} from gapDetector.check for key {}", alreadyReceived, key);
            }
        }

        /**
         * Log missing event stats for all handled partitions in this instance as JSON.
         */
        private void logMissingEventsForThisPartition() {
            int part = this.partition;
            Map<String, Object> report = new LinkedHashMap<>();
            report.put("report_time", System.currentTimeMillis());
            report.put("partition", part);

            long totalMissing = 0;
            for (GapDetector gd : gapDetectors.values()) {
                totalMissing += gd.getMissingCounts();
            }

            long deltaMissing = totalMissing - lastTotalMissing.getOrDefault(part, 0L);
            lastTotalMissing.put(part, totalMissing);

            long received = totalReceived.getOrDefault(part, 0L);
            long deltaReceived = received - lastReceived.getOrDefault(part, 0L);
            lastReceived.put(part, received);

            long emitted = totalEmitted.getOrDefault(part, 0L);
            long deltaEmitted = emitted - lastEmitted.getOrDefault(part, 0L);
            lastEmitted.put(part, emitted);

            report.put("total_missing", totalMissing);
            report.put("delta_missing", deltaMissing);
            report.put("total_received", received);
            report.put("delta_received", deltaReceived);
            report.put("total_emitted", emitted);
            report.put("delta_emitted", deltaEmitted);
            report.put("total_duplicates", totalDuplicates.getOrDefault(part, 0L));
            report.put("total_gap_fill", totalGapFill.getOrDefault(part, 0L));
            report.put("total_below_range", totalBelowRange.getOrDefault(part, 0L));
            report.put("total_non_standard_key_passthrough", totalNonStandardKeyPassthrough.getOrDefault(part, 0L));
            report.put("eps", deltaReceived / MISSING_REPORT_INTERVAL_SEC);

            try {
                String json = REPORT_MAPPER.writeValueAsString(List.of(report));
                LOG.info("[MISSING-REPORT] {}", json);
                if (LOG.isDebugEnabled()) {
                    LOG.debug(
                            "Partition flow summary (partition={}, deltaReceived={}, deltaEmitted={}, duplicates={}, gapFill={}, belowRange={}, nonStandardKeyPassthrough={})",
                            part,
                            deltaReceived,
                            deltaEmitted,
                            totalDuplicates.getOrDefault(part, 0L),
                            totalGapFill.getOrDefault(part, 0L),
                            totalBelowRange.getOrDefault(part, 0L),
                            totalNonStandardKeyPassthrough.getOrDefault(part, 0L));
                }
            } catch (Exception e) {
                LOG.error("Failed to serialize missing report JSON", e);
            }
        }

    }


    /**
     * Deserialize a GapDetector from a byte array using Java serialization.
     * Returns null if the byte array is null or deserialization fails.
     */
    private static GapDetector deserializeGapDetector(byte[] data, String detectorKey) {
        if (data == null) {
            LOG.warn("deserializeGapDetector called with null data (detectorKey={}), creating empty detector",
                    detectorKey);
            return new GapDetector("", WINDOW_SIZE, MAX_WINDOWS);
        }
        try (java.io.ByteArrayInputStream bis = new java.io.ByteArrayInputStream(data);
                java.io.ObjectInputStream ois = new java.io.ObjectInputStream(bis)) {
            Object obj = ois.readObject();
            if (obj instanceof GapDetector) {
                LOG.debug("Successfully deserialized GapDetector (detectorKey={})", detectorKey);
                return (GapDetector) obj;
            } else {
                LOG.error(
                        "Deserialized object is not a recognized GapDetector for detectorKey {}: {}. Returning a new instance.",
                        detectorKey,
                        obj.getClass());
            }
        } catch (Exception e) {
            LOG.error("Failed to deserialize GapDetector for detectorKey {}; creating empty detector", detectorKey,
                    e);
        }
        return new GapDetector("", WINDOW_SIZE, MAX_WINDOWS);
    }

    /**
     * Serialize a GapDetector to a byte array using Java serialization.
     */
    private static byte[] serializeGapDetector(GapDetector detector) throws java.io.IOException {
        if (detector == null)
            return null;
        try (java.io.ByteArrayOutputStream bos = new java.io.ByteArrayOutputStream();
                java.io.ObjectOutputStream oos = new java.io.ObjectOutputStream(bos)) {
            oos.writeObject(detector);
            oos.flush();
            return bos.toByteArray();
        }
    }
}

/*
 * =============================
 * HOW TO RUN (example)
 * =============================
 * 1) Ensure topics exist with the same partition count:
 * kafka-topics.sh --create --topic topic1-raw --partitions 6
 * --replication-factor 3 --bootstrap-server <bs>
 * kafka-topics.sh --create --topic topic2-clean --partitions 6
 * --replication-factor 3 --bootstrap-server <bs>
 * kafka-topics.sh --create --topic topic1-gaps --partitions 6
 * --replication-factor 3 --bootstrap-server <bs>
 *
 * 2) Build and run N app instances with the SAME application.id
 * (udp-dedupe-gap-app) and num.stream.threads=1
 * Each instance will take ownership of a subset of partitions; if a node dies,
 * tasks move automatically.
 *
 * 3) For faster failover, set StreamsConfig.NUM_STANDBY_REPLICAS_CONFIG>=1 so
 * state is replicated to standby tasks.
 *
 * 4) If you want compaction on topic2-clean: set cleanup.policy=compact on the
 * broker for that topic.
 */
