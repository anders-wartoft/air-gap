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
import org.apache.kafka.streams.KafkaStreams;
import org.apache.kafka.streams.StreamsBuilder;
import org.apache.kafka.streams.StreamsConfig;
import org.apache.kafka.streams.Topology;
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
import java.util.ArrayList;
import java.util.Collection;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Properties;

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
        System.out.println("Starting PartitionDedupApp...");
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
                LOG.info("Injected Kafka property from env {} -> {}={}", k, propName, displayVal);
            }
        }

        LOG.info("Starting PartitionDedupApp with config: {}", props);
        ;
        LOG.info("BOOTSTRAP_SERVERS={}", BOOTSTRAP_SERVERS);
        LOG.info("RAW_TOPICS={}", RAW_TOPICS);
        LOG.info("CLEAN_TOPIC={}", CLEAN_TOPIC);
        LOG.info("GAP_TOPIC={}", GAP_TOPIC);
        LOG.info("STATE_DIR_CONFIG={}", STATE_DIR_CONFIG);
        LOG.info("WINDOW_SIZE={}", WINDOW_SIZE);
        LOG.info("MAX_WINDOWS={}", MAX_WINDOWS);
        LOG.info("GAP_EMIT_INTERVAL_SEC={}", GAP_EMIT_INTERVAL_SEC);
        LOG.info("PERSIST_INTERVAL_MS={}", PERSIST_INTERVAL_MS);
        LOG.info("Application ID: {}", props.get(StreamsConfig.APPLICATION_ID_CONFIG));
        LOG.info("Num Stream Threads: {}", props.get(StreamsConfig.NUM_STREAM_THREADS_CONFIG));
        LOG.info("Num Standby Replicas: {}", props.get(StreamsConfig.NUM_STANDBY_REPLICAS_CONFIG));
        LOG.info("Processing Guarantee: {}", props.get(StreamsConfig.PROCESSING_GUARANTEE_CONFIG));
        LOG.info("Commit Interval (ms): {}", props.get(StreamsConfig.COMMIT_INTERVAL_MS_CONFIG));
        LOG.info("State Dir: {}", props.get(StreamsConfig.STATE_DIR_CONFIG));
        System.out.println(props);

        // Register DynamicMBean for exposing each property as a JMX attribute
        JmxSupport.registerPropsMBean(props, RAW_TOPICS, CLEAN_TOPIC, GAP_TOPIC, APPLICATION_ID, assignedRawPartitions,
                WINDOW_SIZE, MAX_WINDOWS);

        // Build topology
        Topology topology = buildTopology();
        try (KafkaStreams streams = new KafkaStreams(topology, props)) {
            // Add shutdown hook
            Runtime.getRuntime().addShutdownHook(new Thread(() -> {
                LOG.info("Shutdown hook triggered: closing Kafka Streams...");
                streams.close(Duration.ofSeconds(10));
                LOG.info("Kafka Streams closed.");
            }));

            streams.start();
            LOG.info(
                    "PartitionDedupApp started and running. Main thread will now exit, process will be kept alive by Kafka Streams threads.");
            // Optionally, print store metadata for debugging
            Collection<org.apache.kafka.streams.StreamsMetadata> storeMetadata = streams
                    .streamsMetadataForStore(STORE_GAP);
            LOG.info("All metadata for store {}: {}", STORE_GAP, storeMetadata);

            // Block main thread until shutdown
            final java.util.concurrent.CountDownLatch latch = new java.util.concurrent.CountDownLatch(1);
            Runtime.getRuntime().addShutdownHook(new Thread("streams-shutdown-hook") {
                @Override
                public void run() {
                    latch.countDown();
                }
            });
            try {
                latch.await();
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
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
            LOG.info("Initializing DedupAndGapProcessor for partition {}", partition);
            assignedRawPartitions.add(partition);

            this.gapStore = (KeyValueStore<String, byte[]>) context.getStateStore(STORE_GAP);

            // Load gapDetectors only for this thread's partition key
            try (org.apache.kafka.streams.state.KeyValueIterator<String, byte[]> iter = gapStore.all()) {
                while (iter.hasNext()) {
                    org.apache.kafka.streams.KeyValue<String, byte[]> entry = iter.next();
                    GapDetector gd = deserializeGapDetector(entry.value);
                    if (gd != null) {
                        gapDetectors.put(entry.key, gd);
                        LOG.info("Registered GapDetector for key: {}", entry.key);
                        JmxSupport.registerProcessorMBean(this);
                    }
                }
            }

            // Persist this partition’s detector periodically
            LOG.info("Scheduling periodic gap detector persistence every {} ms", persistIntervalMs);
            this.persistSchedule = this.context.schedule(
                    java.time.Duration.ofMillis(persistIntervalMs),
                    org.apache.kafka.streams.processor.PunctuationType.WALL_CLOCK_TIME,
                    ts -> {
                        for (Map.Entry<String, GapDetector> entry : gapDetectors.entrySet()) {
                            try {
                                gapStore.put(entry.getKey(), serializeGapDetector(entry.getValue()));
                            } catch (Exception e) {
                                LOG.error("Failed to serialize GapDetector for {}", entry.getKey(), e);
                            }
                        }
                    });

            LOG.info("Emit gap: Started gap detection for partition {}", partition);
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
        }

        public int getPartition() {
            return this.partition;
        }

        /** Emit gaps for all detectors. */
        private void emitGapsForPartition(int partition) {
            // Count missing messages for this partition
            // For this GapDetector, get current gaps
            LOG.debug("[GAP-DEBUG] Partition {}: gapDetectors keys: {}", partition, gapDetectors.keySet());
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
                        } catch (Exception e) {
                            LOG.error("Failed to serialize gaps for window {}", gapKey, e);
                        }
                        window.lastEmittedNonEmpty[0] = true;
                    }
                }
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
                // Not a topic_partition_offset key, deliver with no gap detection
                LOG.info("Non-standard key format '{}', forwarding directly to deduped-sink", key);
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
                // Not a topic_partition_offset key, deliver with no gap detection
                LOG.debug("Non-standard key format '{}', forwarding directly to deduped-sink", key);
                context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                return;
            }
            LOG.debug("Processing record from topic={}, partition={}, offset={}", topic, partition, offset);
            String topicPartition = topic + "_" + partition;

            // Increment received counter
            totalReceived.merge(partition, 1L, Long::sum);

            GapDetector gapDetector = gapDetectors.get(topicPartition);
            if (gapDetector == null) {
                LOG.warn("No GapDetector found for topicPartition {}, creating a new one", topicPartition);
                gapDetector = new GapDetector(topic, windowSize, maxWindows);
                gapDetectors.put(topicPartition, gapDetector);
                LOG.info("Registered GapDetector for key: {}", topicPartition);
                // Always re-register the MBean when a new detector is added so JMX sees all
                // partitions
                JmxSupport.registerProcessorMBean(this);
                LOG.info("Re-registered Processor MBean after adding new partition: {}. Current keys: {}",
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
            if (alreadyReceived == 1) {
                LOG.debug("Key already seen {}", key);
            } else if (alreadyReceived == 0) {
                LOG.debug("Forwarding unique key {}", key);
                context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                totalEmitted.merge(partition, 1L, Long::sum);
            } else if (alreadyReceived == -1) {
                LOG.debug("Key {} fills a gap, forwarding", key);
                context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                totalEmitted.merge(partition, 1L, Long::sum);
            } else if (alreadyReceived == -2) {
                LOG.info("Key {} is below known range, forwarding", key);
                context.forward(new Record<>(key, value, record.timestamp()), "deduped-sink");
                totalEmitted.merge(partition, 1L, Long::sum);
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
            report.put("eps", deltaReceived / MISSING_REPORT_INTERVAL_SEC);

            try {
                String json = REPORT_MAPPER.writeValueAsString(List.of(report));
                LOG.info("[MISSING-REPORT] {}", json);
            } catch (Exception e) {
                LOG.error("Failed to serialize missing report JSON", e);
            }
        }

    }


    /**
     * Deserialize a GapDetector from a byte array using Java serialization.
     * Returns null if the byte array is null or deserialization fails.
     */
    private static GapDetector deserializeGapDetector(byte[] data) {
        if (data == null) {
            LOG.warn("deserializeGapDetector called with null data");
            return new GapDetector("", WINDOW_SIZE, MAX_WINDOWS);
        }
        try (java.io.ByteArrayInputStream bis = new java.io.ByteArrayInputStream(data);
                java.io.ObjectInputStream ois = new java.io.ObjectInputStream(bis)) {
            Object obj = ois.readObject();
            if (obj instanceof GapDetector) {
                LOG.debug("Successfully deserialized GapDetector");
                return (GapDetector) obj;
            } else {
                LOG.error("Deserialized object is not a recognized GapDetector: {}. Returning a new instance.",
                        obj.getClass());
            }
        } catch (Exception e) {
            LOG.error("Failed to deserialize GapDetector", e);
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
