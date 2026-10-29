package nu.sitia.airgap.gapdetector;

import org.junit.jupiter.api.Test;
import java.util.List;
import static org.junit.jupiter.api.Assertions.*;

public class GapDetectorTest {
    @Test
    public void testNoGapsWhenSequential() {
        GapDetector gd = new GapDetector("test", 10, 2);
        for (long i = 0; i < 10; i++) {
            int dup = gd.check(i, null);
            //  1 if this number was already received, 0 if it’s newly received and -1 if it fills a gap.
            //  If the number is less than the lowest known offset, it is considered unseen and -2 is returned.
            assertEquals(dup,0, "Should not be duplicate for new offset " + i);
        }
        assertTrue(gd.getAllCompactGaps().isEmpty(), "No gaps expected for sequential input");
    }

    @Test
    public void testDetectSingleGap() {
    GapDetector gd = new GapDetector("test", 10, 2);
    // Fill 0, 2, 3, 4, 5, 6, 7, 8, 9 (missing 1)
    gd.check(0, null);
    gd.check(2, null);
    gd.check(3, null);

    List<List<Long>> gaps = gd.getAllCompactGaps();
    assertEquals(1, gaps.size(), "Should detect one gap");
    List<Long> gap = gaps.get(0);
    assertEquals(1, gap.size());
    assertEquals(1L, gap.get(0));
    }

    @Test
    public void testDetectMultipleGaps() {
    GapDetector gd = new GapDetector("test", 10, 2);
    // Fill 0, 2, 4, 5, 6, 7, 8, 9 (missing 1 and 3)
    gd.check(0, null);
    gd.check(2, null);
    gd.check(4, null);
    gd.check(5, null);
    gd.check(6, null);

    List<List<Long>> gaps = gd.getAllCompactGaps();
    assertEquals(2, gaps.size(), "Should detect two gaps");
    assertEquals(1, gaps.get(0).size());
    assertEquals(1L, gaps.get(0).get(0));
    assertEquals(1, gaps.get(1).size());
    assertEquals(3L, gaps.get(1).get(0));
    }

    @Test
    public void testDuplicateDetection() {
        GapDetector gd = new GapDetector("test", 10, 2);
        //  1 if this number was already received, 0 if it’s newly received and -1 if it fills a gap.
        //  If the number is less than the lowest known offset, it is considered unseen and -2 is returned.
        assertEquals(gd.check(0, null), 0);
        assertEquals(gd.check(0, null), 1, "Second check for same offset should be duplicate");
    }

    @Test
    public void testGapClosedByLateArrival() {
    GapDetector gd = new GapDetector("test", 10, 2);
    // Fill 0, 2, 3, 4, 5, 6, 7, 8, 9 (missing 1)
    gd.check(0, null);
    gd.check(2, null);
    gd.check(3, null);
    gd.check(4, null);
    gd.check(5, null);
    gd.check(6, null);
    gd.check(7, null);
    gd.check(8, null);
    gd.check(9, null);
    assertEquals(1, gd.getAllCompactGaps().size());
    gd.check(1, null); // fill the gap
    assertTrue(gd.getAllCompactGaps().isEmpty(), "Gap should be closed after late arrival");
    }

    @Test
    public void testWindowPurgeCallback() {
        GapDetector gd = new GapDetector("test", 1, 2); // small window for easy purging
        final boolean[] purged = {false};
        gd.check(0, w -> purged[0] = true);
        gd.check(1, w -> purged[0] = true);
        gd.check(2, w -> purged[0] = true); // triggers purge of first window
        assertTrue(purged[0], "Purge callback should be called");
    }

    @Test
    public void testPurgeCallbackRunsForGapFreeAndGappedWindows() {
        GapDetector gd = new GapDetector("test", 10, 2);
        java.util.List<Long> purgedMins = new java.util.ArrayList<>();
        java.util.List<Integer> purgedGapCounts = new java.util.ArrayList<>();
        java.util.function.Consumer<GapDetector.WindowView> onPurge = w -> {
            purgedMins.add(w.getMinOffset());
            purgedGapCounts.add(w.findGaps().size());
        };
        for (long i = 0; i < 40; i++) {
            if (i != 15) {
                gd.check(i, onPurge);
            }
        }
        // Windows 0 and 10 leave the range of two windows; 0 is gap-free, 10 misses 15.
        assertEquals(java.util.List.of(0L, 10L), purgedMins);
        assertEquals(java.util.List.of(0, 1), purgedGapCounts);
    }
}
