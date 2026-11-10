package nu.sitia.airgap.gapdetector;

import org.apache.kafka.common.serialization.StringSerializer;
import org.apache.kafka.streams.KeyValue;
import org.apache.kafka.streams.state.KeyValueIterator;
import org.apache.kafka.streams.state.KeyValueStore;
import org.roaringbitmap.longlong.LongIterator;

import java.io.*;
import java.util.*;

/**
 * Transactional state layout: fixed detector metadata, one descriptor per window,
 * and 8 KiB bitmap blocks. No value grows with the retained history.
 */
public final class GapDetectorStore {
    public static final int BLOCK_OFFSETS = 65_536;
    public static final int BLOCK_BYTES = BLOCK_OFFSETS / 8;
    private static final String DETECTOR = "v2/d/";
    private static final String WINDOW = "v2/w/";
    private static final String BLOCK = "v2/b/";
    private static final int VERSION = 2;
    private final KeyValueStore<String, byte[]> store;
    private final Map<String, LinkedHashMap<Long, Long>> orders = new HashMap<>();
    private final Map<String, Long> nextOrders = new HashMap<>();
    private final Set<String> legacyKeys = new HashSet<>();

    public GapDetectorStore(KeyValueStore<String, byte[]> store) {
        this.store = store;
    }

    public boolean requiresMigration(String key) {
        return legacyKeys.contains(key);
    }

    public Map<String, GapDetector> restore() {
        Map<String, GapDetector> detectors = new HashMap<>();
        try (KeyValueIterator<String, byte[]> it = store.all()) {
            while (it.hasNext()) {
                KeyValue<String, byte[]> entry = it.next();
                if (entry.key.startsWith(DETECTOR)) {
                    String key = entry.key.substring(DETECTOR.length());
                    if (detectors.putIfAbsent(key, restoreDetector(key, entry.value)) != null) {
                        throw new IOException("Both legacy and versioned state exist for " + key);
                    }
                } else if (!entry.key.startsWith(WINDOW) && !entry.key.startsWith(BLOCK)) {
                    try (ObjectInputStream in = new ObjectInputStream(new ByteArrayInputStream(entry.value))) {
                        Object value = in.readObject();
                        if (!(value instanceof GapDetector)) {
                            throw new IOException("Not a GapDetector: " + entry.key);
                        }
                        if (detectors.putIfAbsent(entry.key, (GapDetector) value) != null) {
                            throw new IOException("Both legacy and versioned state exist for " + entry.key);
                        }
                        legacyKeys.add(entry.key);
                    }
                }
            }
        } catch (IOException | ClassNotFoundException e) {
            throw new IllegalStateException("Cannot restore dedup state; refusing to discard history", e);
        }
        try (KeyValueIterator<String, byte[]> it = store.all()) {
            while (it.hasNext()) {
                String key = it.next().key;
                if (key.startsWith(WINDOW) || key.startsWith(BLOCK)) {
                    String suffix = key.substring(WINDOW.length());
                    int separator = suffix.indexOf('/');
                    String owner = separator < 0 ? "" : suffix.substring(0, separator);
                    if (!orders.containsKey(owner)) {
                        throw new IllegalStateException("Orphaned dedup state without detector metadata: " + key);
                    }
                    if (key.startsWith(BLOCK)) {
                        int end = suffix.indexOf('/', separator + 1);
                        try {
                            long min = Long.parseLong(suffix.substring(separator + 1, end));
                            if (!orders.get(owner).containsKey(min)) {
                                throw new IllegalStateException("Orphaned bitmap without window metadata: " + key);
                            }
                        } catch (NumberFormatException | IndexOutOfBoundsException e) {
                            throw new IllegalStateException("Invalid bitmap key: " + key, e);
                        }
                    }
                }
            }
        }
        return detectors;
    }

    private GapDetector restoreDetector(String key, byte[] bytes) throws IOException {
        try (DataInputStream in = input(bytes)) {
            GapDetector detector = new GapDetector(in.readUTF(), in.readInt(), in.readInt());
            detector.setLastReceived(in.readLong());
            detector.setMinReceived(in.readLong());
            long nextOrder = in.readLong();
            int expectedWindows = in.readInt();
            requireEnd(in);
            if (nextOrder < 0 || expectedWindows < 0 || detector.getMaxWindows() <= 0) {
                throw new IOException("Invalid detector metadata for " + key);
            }
            TreeMap<Long, GapDetector.Window> sorted = new TreeMap<>();
            String prefix = WINDOW + key + "/";
            try (KeyValueIterator<String, byte[]> windows = store.prefixScan(prefix, new StringSerializer())) {
                while (windows.hasNext()) {
                    KeyValue<String, byte[]> entry = windows.next();
                    try (DataInputStream win = input(entry.value)) {
                        long min = win.readLong();
                        long max = win.readLong();
                        long created = win.readLong();
                        long order = win.readLong();
                        long cardinality = win.readLong();
                        requireEnd(win);
                        if (!entry.key.equals(prefix + min) || min < 0 || min % detector.getWindowSize() != 0
                                || max - min != detector.getWindowSize() - 1
                                || order < 0 || order >= nextOrder || cardinality < 0) {
                            throw new IOException("Invalid window metadata: " + entry.key);
                        }
                        GapDetector.Window window = new GapDetector.Window(min, max, created);
                        restoreBlocks(key, window);
                        if (window.received.getLongCardinality() != cardinality
                                || sorted.put(order, window) != null) {
                            throw new IOException("Missing bitmap state or duplicate window order: " + entry.key);
                        }
                    }
                }
            }
            if (sorted.size() != expectedWindows) {
                throw new IOException("Missing window state for " + key);
            }
            LinkedHashMap<Long, Long> order = new LinkedHashMap<>();
            sorted.forEach((sequence, window) -> {
                detector.getWindowsList().add(window);
                order.put(window.minOffset, sequence);
            });
            orders.put(key, order);
            nextOrders.put(key, nextOrder);
            return detector;
        }
    }

    private void restoreBlocks(String key, GapDetector.Window window) throws IOException {
        String prefix = blockPrefix(key, window.minOffset);
        try (KeyValueIterator<String, byte[]> blocks = store.prefixScan(prefix, new StringSerializer())) {
            while (blocks.hasNext()) {
                KeyValue<String, byte[]> entry = blocks.next();
                long block;
                try {
                    block = Long.parseLong(entry.key.substring(prefix.length()));
                } catch (NumberFormatException e) {
                    throw new IOException("Invalid bitmap key: " + entry.key, e);
                }
                if (block < 0 || block > (window.maxOffset - window.minOffset) / BLOCK_OFFSETS
                        || entry.value.length != BLOCK_BYTES) {
                    throw new IOException("Invalid bitmap block: " + entry.key);
                }
                BitSet bits = BitSet.valueOf(entry.value);
                for (int bit = bits.nextSetBit(0); bit >= 0; bit = bits.nextSetBit(bit + 1)) {
                    long relative = block * BLOCK_OFFSETS + bit;
                    if (relative > window.maxOffset - window.minOffset) {
                        throw new IOException("Bitmap offset outside window: " + entry.key);
                    }
                    window.received.add(relative);
                }
            }
        }
    }

    /** Called on the stream thread, before forwarding the corresponding input record. */
    public void persist(String key, GapDetector detector, long offset) {
        try {
            LinkedHashMap<Long, Long> order = orders.computeIfAbsent(key, ignored -> new LinkedHashMap<>());
            Set<Long> retained = new HashSet<>();
            for (GapDetector.Window window : detector.getWindows()) retained.add(window.minOffset);
            for (long min : new ArrayList<>(order.keySet())) {
                if (!retained.contains(min)) {
                    deleteWindow(key, min);
                    order.remove(min);
                }
            }
            boolean migrate = legacyKeys.contains(key);
            long nextOrder = nextOrders.getOrDefault(key, 0L);
            for (GapDetector.Window window : detector.getWindows()) {
                boolean fresh = !order.containsKey(window.minOffset);
                if (fresh) order.put(window.minOffset, nextOrder++);
                if (migrate || fresh) {
                    writeAllBlocks(key, window);
                } else if (window.contains(offset)) {
                    long relative = offset - window.minOffset;
                    String blockKey = blockPrefix(key, window.minOffset) + relative / BLOCK_OFFSETS;
                    byte[] previous = store.get(blockKey);
                    byte[] block = previous == null ? new byte[BLOCK_BYTES] : previous.clone();
                    int bit = (int) (relative % BLOCK_OFFSETS);
                    block[bit / 8] |= (byte) (1 << (bit % 8));
                    store.put(blockKey, block);
                }
                if (migrate || fresh || window.contains(offset)) {
                    store.put(WINDOW + key + "/" + window.minOffset, encode(out -> {
                        out.writeLong(window.minOffset);
                        out.writeLong(window.maxOffset);
                        out.writeLong(window.createdAt);
                        out.writeLong(order.get(window.minOffset));
                        out.writeLong(window.received.getLongCardinality());
                    }));
                }
            }
            nextOrders.put(key, nextOrder);
            final long sequence = nextOrder;
            store.put(DETECTOR + key, encode(out -> {
                out.writeUTF(detector.getTopicName());
                out.writeInt((int) detector.getWindowSize());
                out.writeInt(detector.getMaxWindows());
                out.writeLong(detector.getLastReceived());
                out.writeLong(detector.getMinReceived());
                out.writeLong(sequence);
                out.writeInt(detector.getWindows().size());
            }));
            // Migration and its legacy tombstone are committed in the same Kafka transaction.
            if (migrate) {
                store.delete(key);
                legacyKeys.remove(key);
            }
        } catch (IOException e) {
            throw new IllegalStateException("Cannot persist dedup state for " + key, e);
        }
    }

    private void writeAllBlocks(String key, GapDetector.Window window) {
        LongIterator it = window.received.getLongIterator();
        long current = -1;
        byte[] block = new byte[BLOCK_BYTES];
        while (it.hasNext()) {
            long relative = it.next();
            long index = relative / BLOCK_OFFSETS;
            if (current != index) {
                if (current >= 0) store.put(blockPrefix(key, window.minOffset) + current, block);
                block = new byte[BLOCK_BYTES];
                current = index;
            }
            int bit = (int) (relative % BLOCK_OFFSETS);
            block[bit / 8] |= (byte) (1 << (bit % 8));
        }
        if (current >= 0) store.put(blockPrefix(key, window.minOffset) + current, block);
    }

    private void deleteWindow(String key, long min) {
        List<String> keys = new ArrayList<>();
        try (KeyValueIterator<String, byte[]> it = store.prefixScan(blockPrefix(key, min), new StringSerializer())) {
            while (it.hasNext()) keys.add(it.next().key);
        }
        keys.forEach(store::delete);
        store.delete(WINDOW + key + "/" + min);
    }

    private static String blockPrefix(String key, long min) {
        return BLOCK + key + "/" + min + "/";
    }

    private interface Writer { void write(DataOutputStream out) throws IOException; }

    private static byte[] encode(Writer writer) throws IOException {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        try (DataOutputStream out = new DataOutputStream(bytes)) {
            out.writeInt(VERSION);
            writer.write(out);
        }
        return bytes.toByteArray();
    }

    private static DataInputStream input(byte[] bytes) throws IOException {
        DataInputStream in = new DataInputStream(new ByteArrayInputStream(bytes));
        if (in.readInt() != VERSION) throw new IOException("Unsupported dedup state version");
        return in;
    }

    private static void requireEnd(DataInputStream in) throws IOException {
        if (in.read() != -1) throw new IOException("Unexpected trailing dedup state data");
    }
}
