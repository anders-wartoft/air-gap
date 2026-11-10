package nu.sitia.airgap.gapdetector;

public final class DetectorFixtures {
    private DetectorFixtures() {}

    public static GapDetector largeDetector() {
        GapDetector detector = new GapDetector("export", 5_000_000, 5200);
        for (long start = 0; start < 10_000_000; start += 5_000_000) {
            detector.check(start, null);
            GapDetector.Window window = detector.getWindowsList().getLast();
            // Alternating offsets cannot be reduced to a few Roaring runs.
            for (long offset = 0; offset < 5_000_000; offset += 2) {
                window.received.add(offset);
            }
        }
        detector.setLastReceived(9_999_998);
        return detector;
    }
}
