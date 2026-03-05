package nu.sitia.airgap.streams;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.lang.reflect.Method;

import org.junit.jupiter.api.Test;

class PartitionDedupAppTest {

    @Test
    void validateRuntimeConfigurationAcceptsNominalValues() {
        assertDoesNotThrow(() -> PartitionDedupApp.validateRuntimeConfigurationValues(
                5000L, 60000L, 20, true, 30000L, 10000L, 5000L));
    }

    @Test
    void validateRuntimeConfigurationRejectsNegativeRetryBackoff() {
        IllegalArgumentException ex = assertThrows(IllegalArgumentException.class,
                () -> PartitionDedupApp.validateRuntimeConfigurationValues(
                        -1L, 60000L, 20, false, 0L, 0L, 0L));
        assertTrue(ex.getMessage().contains("RETRY_BACKOFF_MS"));
    }

    @Test
    void validateRuntimeConfigurationRejectsMaxLowerThanBase() {
        IllegalArgumentException ex = assertThrows(IllegalArgumentException.class,
                () -> PartitionDedupApp.validateRuntimeConfigurationValues(
                        60000L, 5000L, 20, false, 0L, 0L, 0L));
        assertTrue(ex.getMessage().contains("RETRY_BACKOFF_MAX_MS"));
    }

    @Test
    void validateRuntimeConfigurationRejectsJitterOutOfRange() {
        IllegalArgumentException exLow = assertThrows(IllegalArgumentException.class,
                () -> PartitionDedupApp.validateRuntimeConfigurationValues(
                        5000L, 60000L, -1, false, 0L, 0L, 0L));
        assertTrue(exLow.getMessage().contains("RETRY_BACKOFF_JITTER_PCT"));

        IllegalArgumentException exHigh = assertThrows(IllegalArgumentException.class,
                () -> PartitionDedupApp.validateRuntimeConfigurationValues(
                        5000L, 60000L, 101, false, 0L, 0L, 0L));
        assertTrue(exHigh.getMessage().contains("RETRY_BACKOFF_JITTER_PCT"));
    }

    @Test
    void validateRuntimeConfigurationRejectsNonPositiveFailFastTimeouts() {
        IllegalArgumentException exStartup = assertThrows(IllegalArgumentException.class,
                () -> PartitionDedupApp.validateRuntimeConfigurationValues(
                        5000L, 60000L, 20, true, 0L, 10000L, 5000L));
        assertTrue(exStartup.getMessage().contains("FAIL_FAST_STARTUP_TIMEOUT_MS"));

        IllegalArgumentException exInterval = assertThrows(IllegalArgumentException.class,
                () -> PartitionDedupApp.validateRuntimeConfigurationValues(
                        5000L, 60000L, 20, true, 30000L, 0L, 5000L));
        assertTrue(exInterval.getMessage().contains("FAIL_FAST_CHECK_INTERVAL_MS"));

        IllegalArgumentException exCheckTimeout = assertThrows(IllegalArgumentException.class,
                () -> PartitionDedupApp.validateRuntimeConfigurationValues(
                        5000L, 60000L, 20, true, 30000L, 10000L, 0L));
        assertTrue(exCheckTimeout.getMessage().contains("FAIL_FAST_CHECK_TIMEOUT_MS"));
    }

    @Test
    void validateRuntimeConfigurationAllowsNonPositiveFailFastValuesWhenDisabled() {
        assertDoesNotThrow(() -> PartitionDedupApp.validateRuntimeConfigurationValues(
                5000L, 60000L, 20, false, 0L, 0L, 0L));
    }

    @Test
    void calculateBackoffNoJitterScalesAndCaps() throws Exception {
        Method method = PartitionDedupApp.class.getDeclaredMethod("calculateBackoffNoJitter", int.class);
        method.setAccessible(true);

        long base = Math.max(0L, PartitionDedupApp.RETRY_BACKOFF_MS);
        long max = Math.max(base, PartitionDedupApp.RETRY_BACKOFF_MAX_MS);

        long first = (Long) method.invoke(null, 1);
        assertEquals(base, first);

        long secondExpected = Math.min(max, base == 0L ? 0L : base * 2L);
        long second = (Long) method.invoke(null, 2);
        assertEquals(secondExpected, second);

        long hugeAttempt = (Long) method.invoke(null, 100);
        assertTrue(hugeAttempt <= max);
    }

    @Test
    void applyJitterStaysWithinExpectedRange() throws Exception {
        Method method = PartitionDedupApp.class.getDeclaredMethod("applyJitter", long.class);
        method.setAccessible(true);

        long value = 1000L;
        int jitterPct = Math.max(0, PartitionDedupApp.RETRY_BACKOFF_JITTER_PCT);

        if (jitterPct == 0) {
            long jittered = (Long) method.invoke(null, value);
            assertEquals(value, jittered);
            return;
        }

        long delta = Math.max(1L, Math.round(value * (jitterPct / 100.0)));
        long min = Math.max(0L, value - delta);
        long max = value + delta;

        for (int i = 0; i < 100; i++) {
            long jittered = (Long) method.invoke(null, value);
            assertTrue(jittered >= min && jittered <= max,
                    "Jittered value out of range: " + jittered + " not in [" + min + ", " + max + "]");
        }
    }

    @Test
    void extractHostFromBootstrapServerParsesCommonFormats() throws Exception {
        Method method = PartitionDedupApp.class.getDeclaredMethod("extractHostFromBootstrapServer", String.class);
        method.setAccessible(true);

        assertEquals("kafka.example.local", method.invoke(null, "kafka.example.local:9092"));
        assertEquals("my-broker", method.invoke(null, "PLAINTEXT://my-broker:9092"));
        assertEquals("2001:db8::1", method.invoke(null, "[2001:db8::1]:9092"));
        assertEquals("raw-host", method.invoke(null, "raw-host"));
    }
}
