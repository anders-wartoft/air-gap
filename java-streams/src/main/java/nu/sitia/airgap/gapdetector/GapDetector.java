
package nu.sitia.airgap.gapdetector;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import org.roaringbitmap.longlong.Roaring64NavigableMap;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.util.*;
import java.util.function.Consumer;
import java.util.stream.Collectors;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.annotation.JsonProperty;


@JsonIgnoreProperties(ignoreUnknown = true)
public class GapDetector implements java.io.Serializable {

    private static final Logger LOG = LoggerFactory.getLogger(GapDetector.class);
    private static final long serialVersionUID = 3L;
    /** Name of the upstream topic  */
    @JsonProperty("topic")
    private String topicName;

    /** Track the highest offset ever received */
    private long lastReceived = Long.MIN_VALUE;
    /** Track the lowest offset ever received */
    private long minReceived = Long.MAX_VALUE;

    private int windowSize; // max offsets per window
    private int maxWindows;  // max number of windows to keep
    private LinkedList<Window> windows = new LinkedList<>();




    // Getters and setters for serialization/deserialization and external access
    public String getTopicName() {
        return topicName;
    }

    public void setTopicName(String topicName) {
        this.topicName = topicName;
    }

    public long getWindowSize() {
        return windowSize;
    }

    public void setWindowSize(long windowSize) {
        // Validate that windowSize fits in an int
        if (windowSize <= 0 || windowSize > Integer.MAX_VALUE) {
            throw new IllegalArgumentException("windowSize must be a positive integer less than or equal to " + Integer.MAX_VALUE);
        }
        this.windowSize = (int) windowSize;
    }

    public int getMaxWindows() {
        return maxWindows;
    }

    public void setMaxWindows(int maxWindows) {
        this.maxWindows = maxWindows;
    }

    public void setLastReceived(long lastReceived) {
        this.lastReceived = lastReceived;
    }

    public void setMinReceived(long minReceived) {
        this.minReceived = minReceived;
    }

    public long getLastReceived() {
        return lastReceived;
    }
    
    public long getMinReceived() {
        return minReceived;
    }

    public LinkedList<Window> getWindowsList() {
        return windows;
    }

    public void setWindows(LinkedList<Window> windows) {
        this.windows = windows;
    }

    // No-arg constructor for Jackson
    public GapDetector() {
        this.topicName = "";
        this.windowSize = 10;
        this.maxWindows = 1;
        this.lastReceived = Long.MIN_VALUE;
        this.minReceived = Long.MAX_VALUE;
        this.windows = new LinkedList<>();
    }

    public GapDetector(String topicName, long windowSize, int maxWindows) {
        this.topicName = topicName;
        this.maxWindows = maxWindows;
        // Validate that windowSize fits in an int
        if (windowSize <= 0 || windowSize > Integer.MAX_VALUE) {
            throw new IllegalArgumentException("windowSize must be a positive integer less than or equal to " + Integer.MAX_VALUE);
        }
        this.windowSize = (int) windowSize;
    }

    public List<Window> getWindows() {
        return windows;
    }

    /**
     * Estimate the memory usage (in bytes) for all loaded windows.
     * This is a rough estimate: each window has a small overhead, and each offset in the bitmap costs memory.
     * RoaringBitmap is compact, but we estimate 16 bytes per window (object overhead) and 16 bytes per offset (bitmap + object refs).
     */
    public long estimateMemoryBytes() {
        long total = 0;
        for (Window w : windows) {
            // Estimate: 16 bytes for Window object, 16 bytes per offset in bitmap
            long cardinality = w.received.getLongCardinality();
            total += 16; // window object overhead
            total += 16 * cardinality; // each offset in bitmap
        }
        return total;
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) return true;
        if (o == null || getClass() != o.getClass()) return false;
        GapDetector that = (GapDetector) o;
        return that.asJson().equals(this.asJson());
    }

    @Override
    public int hashCode() {
        return Objects.hash(this.asJson());
    }

    public String asJson() {
        try {
            ObjectMapper mapper = new ObjectMapper();
            return mapper.writeValueAsString(this);
        } catch (Exception e) {
            throw new RuntimeException("Failed to serialize GapDetector to JSON", e);
        }
    }

    public void fromJson(String json) {
        try {
            ObjectMapper mapper = new ObjectMapper();
            GapDetector other = mapper.readValue(json, GapDetector.class);
            // Copy all fields from 'other' to 'this'
            this.topicName = other.topicName;
            this.windowSize = other.windowSize;
            this.maxWindows = other.maxWindows;
            this.lastReceived = other.lastReceived;
            this.minReceived = other.minReceived;
            this.windows = other.windows;
        } catch (Exception e) {
            throw new RuntimeException("Failed to parse GapDetector JSON", e);
        }
    }

    // Expose a public view for purged windows
    public interface WindowView {
    long getMinOffset();
    long getMaxOffset();
    long getCreatedAt();
    List<List<Long>> findGaps();
    }

    /**
     * Mark a number as received. Optionally emit missing offsets from purged windows.
     *
     * @param number  the offset received
     * @param onPurge callback invoked before a window is purged, to process missing offsets
     * @return 1 if this number was already received, 0 if it’s newly received and -1 if it fills a gap.
     * If the number is less than the lowest known offset, it is considered unseen and -2 is returned.
     */
    public int check(long number, Consumer<WindowView> onPurge) throws OutOfMemoryError {
        if (number > lastReceived) {
            LOG.debug("[GapDetector] New lastReceived for topic {}: {} (was {})", topicName, number, lastReceived);
            lastReceived = number;
        }

        Window w = findWindow(number);
        if (w == null) {
            // special case: If the number is less than the lowest known offset,
            // then we deliver it as unseen, but do not create a new window
            if (!windows.isEmpty() && number < windows.getFirst().minOffset) {
                LOG.debug("[GapDetector] Offset {} is below current known window range (firstWindowMin={}) for topic {}",
                        number,
                        windows.getFirst().minOffset,
                        topicName);
                return -2;
            }

            // purge oldest if exceeding maxWindows
            if (windows.size() >= maxWindows) {
                Window old = windows.removeFirst();
                List<List<Long>> purgedWindowGaps = old.findGapsFiltered(minReceived);
                LOG.debug(
                        "[GapDetector] Purging oldest window {}-{} for topic {} (currentWindows={}, maxWindows={}, purgedGapRanges={})",
                        old.minOffset,
                        old.maxOffset,
                        topicName,
                        windows.size(),
                        maxWindows,
                        purgedWindowGaps.size());
                if (onPurge != null) {
                    onPurge.accept(new WindowView() {
                        public long getMinOffset() { return old.minOffset; }
                        public long getMaxOffset() { return old.maxOffset; }
                        public long getCreatedAt() { return old.createdAt; }
                        public List<List<Long>> findGaps() { return purgedWindowGaps; }
                    });
                }
            }
            long start = number - (number % windowSize);
            long end = start + windowSize - 1;
            try {
                w = new Window(start, end);
                windows.add(w);
                LOG.debug("[GapDetector] Created new window {}-{} for topic {} (windowsNow={}, maxWindows={})",
                        start,
                        end,
                        topicName,
                        windows.size(),
                        maxWindows);
            } catch (OutOfMemoryError oom) {
                LOG.error("[GapDetector] OutOfMemoryError: Failed to allocate new window for offsets " + start + "-" + end + ": " + oom);
                // Prevent further window growth
                this.maxWindows = windows.size();
                LOG.error("[GapDetector] Reducing maxWindows to " + this.maxWindows);
                // Retry once (guard against infinite recursion)
                if (windows.size() == maxWindows) {
                    return check(number, onPurge);
                }
                throw new OutOfMemoryError("Failed to allocate new window for offsets " + start + "-" + end + ": " + oom);
            }
        }

        boolean alreadyReceived = w.received.contains(number - w.minOffset);
        LOG.debug("[GapDetector] Marking offset {} as received in window {}-{} (alreadyReceived={})", number, w.minOffset, w.maxOffset, alreadyReceived);
        w.markReceived(number - w.minOffset);
        // Only update minReceived if this number was actually marked as received
        if (!alreadyReceived && number < minReceived) {
            minReceived = number;
        }
        return alreadyReceived ? 1 : (lastReceived == number ? 0 : -1);
    }

    private Window findWindow(long number) {
        for (Window w : windows) {
            if (w.contains(number)) {
                LOG.debug("[GapDetector] Found window {}-{} for offset {}", w.minOffset, w.maxOffset, number);
                return w;
            }
        }
        return null;
    }

    /**
     * Extract all missing offsets from windows, filtering out gaps below minReceived, in compact format.
     * @return List<List<Long>> compact gap format
     */
    public List<List<Long>> getAllCompactGaps() {
        List<List<Long>> compactGaps = new ArrayList<>();
        for (Window w : windows) {
            compactGaps.addAll(w.findGapsFiltered(minReceived));
        }
        return compactGaps;
    }

    public long getMissingCounts() {
        long missingCount = 0;
        for (List<Long> gap : getAllCompactGaps()) {
            if (gap.size() == 1) {
                missingCount += 1;
            } else if (gap.size() == 2) {
                missingCount += gap.get(1) - gap.get(0) + 1;
            }
        }
        return missingCount;
    }
                        
    public void purge() {
        windows.clear();
        minReceived = Long.MAX_VALUE;
    }

    /** Purge all windows that are full and contain no gaps */
    public void purgeFullWindows() {
        windows.removeIf(Window::isFull);
    }

    /** Represents a single window */
    public static class Window implements java.io.Serializable {
    // Track sticky empty emission state (not serialized)
    public transient boolean[] lastEmittedNonEmpty = null;
        // No-arg constructor for Jackson
        public Window() {
            this.minOffset = 0L;
            this.maxOffset = 0L;
            this.createdAt = System.currentTimeMillis();
        }
        final long minOffset;
        final long maxOffset;
        final long createdAt;
        final Roaring64NavigableMap received = new Roaring64NavigableMap();

        Window(long minOffset, long maxOffset) {
            this.minOffset = minOffset;
            this.maxOffset = maxOffset;
            this.createdAt = System.currentTimeMillis();
        }

        public long getMinOffset() {
            return minOffset;
        }
        
        public long getMaxOffset() {
            return maxOffset;
        }

        boolean contains(long number) {
            return number >= minOffset && number <= maxOffset;
        }

        void markReceived(long number) {
            received.add(number);
        }

        /**
         * Find gaps, but only return those where gap.from >= minAllowed.
         */
        /**
         * Find gaps, but only return those where gap.from >= minAllowed, in compact format.
         * The last gap (tail gap to maxOffset) is excluded.
         * The gap id:s is relative to the window_min, so add window_min to get the absolute offset.
         * @param minAllowed minimum offset to include in gaps
         * @return List<List<Long>>: [from] for single, [from, to] for range.
         */
        public List<List<Long>> findGapsFiltered(long minAllowed) {
            List<List<Long>> compactGaps = new ArrayList<>();

            org.roaringbitmap.longlong.LongIterator it = received.getLongIterator();
            long prev = -1;

            // Collect all gaps as Gap objects for easier tail exclusion
            List<Gap> allGaps = new ArrayList<>();

            while (it.hasNext()) {
                long val = it.next();
                if (prev == -1 && val > 0) {
                    // Gap at the beginning
                    long gapFrom = minOffset;
                    long gapTo = minOffset + val - 1;
                    if (gapFrom >= minAllowed) {
                        allGaps.add(new Gap(gapFrom, gapTo));
                    } else if (gapTo >= minAllowed) {
                        allGaps.add(new Gap(minAllowed, gapTo));
                    }
                } else if (prev != -1 && val > prev + 1) {
                    // Gap between prev and val
                    long gapFrom = minOffset + prev + 1;
                    long gapTo = minOffset + val - 1;
                    if (gapFrom >= minAllowed) {
                        allGaps.add(new Gap(gapFrom, gapTo));
                    } else if (gapTo >= minAllowed) {
                        allGaps.add(new Gap(minAllowed, gapTo));
                    }
                }
                prev = val;
            }

            // Only exclude the true tail gap (from last received + 1 to maxOffset)
            int n = allGaps.size();
            for (int i = 0; i < n; i++) {
                Gap g = allGaps.get(i);
                // Exclude only if this is the last gap and it is a true tail gap
                boolean isLast = (i == n - 1);
                boolean isTailGap = isLast && (g.getTo() == maxOffset) && (g.getFrom() == (prev == -1 ? minOffset : minOffset + prev + 1));
                if (isTailGap) {
                    continue;
                }
                long relFrom = g.getFrom() - minOffset;
                long relTo = g.getTo() - minOffset;
                if (g.getFrom() == g.getTo()) {
                    compactGaps.add(java.util.Collections.singletonList(relFrom));
                } else {
                    compactGaps.add(java.util.Arrays.asList(relFrom, relTo));
                }
            }
            return compactGaps;
        }

        boolean isFull() {
            return received.getLongCardinality() == (maxOffset - minOffset + 1);
        }
    }

    /** Optional: expose all windows for external inspection */
    public List<Map<String, Number>> listWindows() {
        return windows.stream()
            .map(w -> Map.of(
                "minOffset", (Number) w.minOffset,
                "maxOffset", (Number) w.maxOffset,
                "createdAt", (Number) w.createdAt,
                "receivedCardinality", (Number) w.received.getLongCardinality(),
                "lastReceived", (Number) lastReceived
            ))
            .collect(Collectors.toList());
    }



}