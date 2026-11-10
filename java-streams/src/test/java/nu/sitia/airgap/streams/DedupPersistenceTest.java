package nu.sitia.airgap.streams;

import nu.sitia.airgap.gapdetector.DetectorFixtures;
import nu.sitia.airgap.gapdetector.GapDetector;
import nu.sitia.airgap.gapdetector.GapDetectorStore;
import org.apache.kafka.common.serialization.Serdes;
import org.apache.kafka.streams.processor.api.MockProcessorContext;
import org.apache.kafka.streams.processor.api.Record;
import org.apache.kafka.streams.state.KeyValueStore;
import org.apache.kafka.streams.state.Stores;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

import java.io.ByteArrayOutputStream;
import java.io.ObjectOutputStream;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.*;

class DedupPersistenceTest {
    private final java.util.List<KeyValueStore<String, byte[]>> stores = new ArrayList<>();

    private KeyValueStore<String, byte[]> store(Map<String, byte[]> contents) {
        KeyValueStore<String, byte[]> store = Stores.keyValueStoreBuilder(
                Stores.inMemoryKeyValueStore(PartitionDedupApp.STORE_GAP),
                Serdes.String(), Serdes.ByteArray()).withLoggingDisabled().build();
        store.init(new MockProcessorContext<String, byte[]>().getStateStoreContext(), store);
        contents.forEach(store::put);
        stores.add(store);
        return store;
    }

    private MockProcessorContext<String, byte[]> context(KeyValueStore<String, byte[]> store) {
        MockProcessorContext<String, byte[]> context = new MockProcessorContext<>();
        context.addStateStore(store);
        return context;
    }

    private Map<String, byte[]> snapshot(KeyValueStore<String, byte[]> store) {
        Map<String, byte[]> result = new HashMap<>();
        try (org.apache.kafka.streams.state.KeyValueIterator<String, byte[]> it = store.all()) {
            while (it.hasNext()) {
                org.apache.kafka.streams.KeyValue<String, byte[]> entry = it.next();
                result.put(entry.key, entry.value.clone());
            }
        }
        return result;
    }

    static byte[] legacy(GapDetector detector) throws Exception {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        try (ObjectOutputStream out = new ObjectOutputStream(bytes)) {
            out.writeObject(detector);
        }
        return bytes.toByteArray();
    }

    @AfterEach
    void closeStores() {
        stores.forEach(KeyValueStore::close);
    }

    @Test
    void recordIsDurableBeforePunctuationAndDelayedCopyIsDroppedAfterRestore() {
        KeyValueStore<String, byte[]> firstStore = store(Map.of());
        MockProcessorContext<String, byte[]> firstContext = context(firstStore);
        PartitionDedupApp.DedupAndGapProcessor first = new PartitionDedupApp.DedupAndGapProcessor();
        first.init(firstContext);
        first.process(new Record<>("export_0_1", new byte[] {1}, 0));
        assertEquals(1, firstContext.forwarded().size());
        Map<String, byte[]> committed = snapshot(firstStore);
        assertFalse(committed.isEmpty(), "Output must never commit without its dedup state");
        first.close();

        MockProcessorContext<String, byte[]> restoredContext = context(store(committed));
        PartitionDedupApp.DedupAndGapProcessor restored = new PartitionDedupApp.DedupAndGapProcessor();
        restored.init(restoredContext);
        restored.process(new Record<>("export_0_1", new byte[] {1}, 120_000));
        assertTrue(restoredContext.forwarded().isEmpty(), "Delayed copy must be suppressed");
        restored.close();
    }

    @Test
    void largeFragmentedLegacyStateMigratesToBoundedRecordsWithoutLosingHistory() throws Exception {
        GapDetector legacy = DetectorFixtures.largeDetector();
        byte[] oldValue = legacy(legacy);
        assertTrue(oldValue.length > 1_048_576, "Fixture must reproduce staging-sized state");
        KeyValueStore<String, byte[]> state = store(Map.of("export_0", oldValue));
        MockProcessorContext<String, byte[]> ctx = context(state);
        PartitionDedupApp.DedupAndGapProcessor processor = new PartitionDedupApp.DedupAndGapProcessor();
        processor.init(ctx);
        processor.process(new Record<>("export_0_9999999", new byte[] {1}, 0));
        // Old implementation only persists on its wall-clock punctuator.
        ctx.scheduledPunctuators().stream()
                .filter(p -> p.getInterval().toMillis() == PartitionDedupApp.PERSIST_INTERVAL_MS)
                .forEach(p -> p.getPunctuator().punctuate(60_000));
        Map<String, byte[]> persisted = snapshot(state);
        assertTrue(state.get("export_0") == null, "Legacy monolithic key must be tombstoned");
        assertTrue(persisted.values().stream().allMatch(v -> v.length <= GapDetectorStore.BLOCK_BYTES),
                "Even uncompressible history must consist of small records");
        processor.close();

        MockProcessorContext<String, byte[]> restoredContext = context(store(persisted));
        PartitionDedupApp.DedupAndGapProcessor restored = new PartitionDedupApp.DedupAndGapProcessor();
        restored.init(restoredContext);
        restored.process(new Record<>("export_0_0", new byte[] {1}, 0));
        restored.process(new Record<>("export_0_9999999", new byte[] {1}, 0));
        assertTrue(restoredContext.forwarded().isEmpty());
        assertEquals(1, restored.getGapDetectors().get("export_0").check(2, null));
        assertEquals(-1, restored.getGapDetectors().get("export_0").check(3, null));
        restored.close();
    }

    @Test
    void corruptStateFailsRatherThanSilentlyForgettingHistory() {
        PartitionDedupApp.DedupAndGapProcessor processor = new PartitionDedupApp.DedupAndGapProcessor();
        assertThrows(RuntimeException.class,
                () -> processor.init(context(store(Map.of("export_0", new byte[] {1, 2, 3})))));
    }

    @Test
    void filteredOffsetsAreDurableButNotForwarded() {
        KeyValueStore<String, byte[]> state = store(Map.of());
        MockProcessorContext<String, byte[]> ctx = context(state);
        PartitionDedupApp.DedupAndGapProcessor processor = new PartitionDedupApp.DedupAndGapProcessor();
        processor.init(ctx);
        processor.process(new Record<>("export_0_5", new byte[0], 0));
        assertTrue(ctx.forwarded().isEmpty());
        processor.close();
        MockProcessorContext<String, byte[]> restoredContext = context(store(snapshot(state)));
        PartitionDedupApp.DedupAndGapProcessor restored = new PartitionDedupApp.DedupAndGapProcessor();
        restored.init(restoredContext);
        restored.process(new Record<>("export_0_5", new byte[] {1}, 0));
        assertTrue(restoredContext.forwarded().isEmpty());
        restored.close();
    }

    @Test
    void firstDuplicateAlsoMigratesLegacyStateWithoutForwarding() throws Exception {
        GapDetector legacy = new GapDetector("export", 1000, 5);
        legacy.check(5, null);
        KeyValueStore<String, byte[]> state = store(Map.of("export_0", legacy(legacy)));
        MockProcessorContext<String, byte[]> ctx = context(state);
        PartitionDedupApp.DedupAndGapProcessor processor = new PartitionDedupApp.DedupAndGapProcessor();
        processor.init(ctx);
        processor.process(new Record<>("export_0_5", new byte[] {1}, 0));
        assertTrue(ctx.forwarded().isEmpty());
        assertNull(state.get("export_0"));
        assertNotNull(state.get("v2/d/export_0"));
        processor.close();
    }
}
