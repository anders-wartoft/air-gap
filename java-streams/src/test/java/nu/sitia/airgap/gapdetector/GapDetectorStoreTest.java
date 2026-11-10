package nu.sitia.airgap.gapdetector;

import org.apache.kafka.common.serialization.Serdes;
import org.apache.kafka.streams.processor.api.MockProcessorContext;
import org.apache.kafka.streams.state.KeyValueStore;
import org.apache.kafka.streams.state.Stores;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.io.ObjectStreamClass;
import java.util.List;

import static org.junit.jupiter.api.Assertions.*;

class GapDetectorStoreTest {
    private KeyValueStore<String, byte[]> state;
    private GapDetectorStore persistence;

    @BeforeEach
    void setup() {
        state = Stores.keyValueStoreBuilder(Stores.inMemoryKeyValueStore("state"),
                Serdes.String(), Serdes.ByteArray()).withLoggingDisabled().build();
        state.init(new MockProcessorContext<String, byte[]>().getStateStoreContext(), state);
        persistence = new GapDetectorStore(state);
    }

    @AfterEach
    void close() {
        state.close();
    }

    private void receive(GapDetector detector, long offset) {
        detector.check(offset, null);
        persistence.persist("export_0", detector, offset);
    }

    @Test
    void blockBoundaryAndGapFillSurviveRestoration() {
        GapDetector detector = new GapDetector("export", 5_000_000, 2);
        receive(detector, 65_535);
        receive(detector, 65_537);
        assertEquals(8192, state.get("v2/b/export_0/0/0").length);
        assertEquals(8192, state.get("v2/b/export_0/0/1").length);
        GapDetector restored = new GapDetectorStore(state).restore().get("export_0");
        assertEquals(1, restored.check(65_535, null));
        assertEquals(List.of(List.of(0L, 65_534L), List.of(65_536L)),
                restored.getAllCompactGaps());
        assertEquals(-1, restored.check(65_536, null));
        GapDetectorStore second = new GapDetectorStore(state);
        restored = second.restore().get("export_0");
        restored.check(65_536, null);
        second.persist("export_0", restored, 65_536);
        assertEquals(1, new GapDetectorStore(state).restore().get("export_0").check(65_536, null));
    }

    @Test
    void purgedWindowsDeleteAllBlocksAndKeepSurvivingDuplicateHistory() {
        GapDetector detector = new GapDetector("export", 100_000, 2);
        receive(detector, 0);
        receive(detector, 99_999);
        receive(detector, 100_000);
        receive(detector, 200_000);
        assertNull(state.get("v2/w/export_0/0"));
        assertNull(state.get("v2/b/export_0/0/0"));
        assertNull(state.get("v2/b/export_0/0/1"));
        GapDetector restored = new GapDetectorStore(state).restore().get("export_0");
        assertEquals(2, restored.getWindows().size());
        assertEquals(1, restored.check(100_000, null));
        assertEquals(-2, restored.check(0, null));
    }

    @Test
    void completeWindowsRemainDeduplicated() {
        GapDetector detector = new GapDetector("export", 4, 2);
        for (long offset = 0; offset < 8; offset++) receive(detector, offset);
        GapDetector restored = new GapDetectorStore(state).restore().get("export_0");
        for (long offset = 0; offset < 8; offset++) assertEquals(1, restored.check(offset, null));
        assertEquals(0, restored.getMissingCounts());
    }

    @Test
    void windowInsertionOrderIsNotChangedByLexicographicStoreKeyOrdering() {
        GapDetector detector = new GapDetector("export", 10, 4);
        receive(detector, 0);
        receive(detector, 100);
        receive(detector, 20);
        GapDetector restored = new GapDetectorStore(state).restore().get("export_0");
        assertEquals(100L, restored.getWindows().get(1).getMinOffset());
        assertEquals(20L, restored.getWindows().get(2).getMinOffset());
    }

    @Test
    void missingBlocksAndMissingDescriptorsAreFatal() {
        GapDetector detector = new GapDetector("export", 1000, 2);
        receive(detector, 1);
        byte[] block = state.delete("v2/b/export_0/0/0");
        assertThrows(IllegalStateException.class, () -> new GapDetectorStore(state).restore());
        state.put("v2/b/export_0/0/0", block);
        byte[] window = state.delete("v2/w/export_0/0");
        assertThrows(IllegalStateException.class, () -> new GapDetectorStore(state).restore());
        state.put("v2/w/export_0/0", window);
        state.delete("v2/d/export_0");
        assertThrows(IllegalStateException.class, () -> new GapDetectorStore(state).restore());
    }

    @Test
    void legacyWindowSerializationIdentityIsPreserved() {
        assertEquals(-7186946582250807937L, ObjectStreamClass.lookup(GapDetector.Window.class).getSerialVersionUID());
        assertEquals(3L, ObjectStreamClass.lookup(GapDetector.class).getSerialVersionUID());
    }
}
