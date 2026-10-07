package nu.sitia.airgap.streams;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.fail;

import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;

import org.apache.logging.log4j.Level;
import org.apache.logging.log4j.LogManager;
import org.apache.logging.log4j.core.LogEvent;
import org.apache.logging.log4j.core.Logger;
import org.apache.logging.log4j.core.appender.AbstractAppender;
import org.apache.logging.log4j.core.config.Property;
import org.junit.jupiter.api.Test;

class ProductionConfigurationWarningsTest {
    private static Properties nominal() {
        Properties p = new Properties();
        p.setProperty("WINDOW_SIZE", "1000");
        p.setProperty("MAX_WINDOWS", "500");
        p.setProperty("state.dir", "/var/lib/airgap/dedup");
        p.setProperty("num.standby.replicas", "1");
        p.setProperty("security.protocol", "SSL");
        return p;
    }

    private static Map<String, String> warning(String setting, String value) {
        return Collections.singletonMap(setting, value);
    }

    private static final class Capture extends AbstractAppender {
        final List<LogEvent> events = new ArrayList<>();

        Capture() {
            super("production-warning-test", null, null, false, Property.EMPTY_ARRAY);
        }

        @Override
        public void append(LogEvent event) {
            events.add(event.toImmutable());
        }
    }

    private static void check(Properties effective, Map<String, String> expected) throws Exception {
        Method method;
        try {
            method = PartitionDedupApp.class.getDeclaredMethod(
                    "warnProductionConfiguration", Properties.class, String.class);
        } catch (NoSuchMethodException missing) {
            fail("REQ-50 pending: implement warnProductionConfiguration(Properties effective, String phase)");
            return;
        }
        method.setAccessible(true);
        Properties before = new Properties();
        before.putAll(effective);
        Logger logger = (Logger) LogManager.getLogger(PartitionDedupApp.class);
        Level originalLevel = logger.getLevel();
        Capture capture = new Capture();
        capture.start();
        logger.addAppender(capture);
        try {
            for (String phase : new String[] { "startup", "shutdown" }) {
                for (Level threshold : new Level[] { Level.INFO, Level.ERROR, Level.FATAL }) {
                    logger.setLevel(threshold);
                    capture.events.clear();
                    method.invoke(null, effective, phase);
                    assertEquals(threshold, logger.getLevel(), "warning emission must preserve log level");
                    assertEquals(before, effective, "warning evaluation must not mutate settings");
                    assertEquals(expected.size(), capture.events.size(), "one event per risky setting");
                    Map<String, Integer> seen = new HashMap<>();
                    for (LogEvent event : capture.events) {
                        assertEquals(Level.WARN, event.getLevel());
                        String text = event.getMessage().getFormattedMessage();
                        String prefix = "[PRODUCTION-CONFIG] phase=" + phase + " setting=";
                        assertTrue(text.startsWith(prefix), text);
                        String[] settingAndValue = text.substring(prefix.length()).split(" value=", 2);
                        assertEquals(2, settingAndValue.length, text);
                        String[] valueAndRisk = settingAndValue[1].split(" risk=", 2);
                        assertEquals(2, valueAndRisk.length, text);
                        String setting = settingAndValue[0];
                        assertTrue(expected.containsKey(setting), "unexpected warning: " + text);
                        assertEquals(expected.get(setting), valueAndRisk[0]);
                        assertTrue(valueAndRisk[1].trim().length() >= 10, "risk needs an explanation");
                        seen.put(setting, seen.getOrDefault(setting, 0) + 1);
                        if (setting.equals("WINDOW_SIZE") || setting.equals("MAX_WINDOWS")) {
                            long retained = Long.parseLong(effective.getProperty("WINDOW_SIZE"))
                                    * Long.parseLong(effective.getProperty("MAX_WINDOWS"));
                            assertTrue(valueAndRisk[1].contains(Long.toString(retained)),
                                    "dedup warning must explain retained offset capacity without int overflow");
                        }
                        assertFalse(text.contains("DO-NOT-LOG-THIS-SECRET"));
                    }
                    for (String setting : expected.keySet()) {
                        assertEquals(Integer.valueOf(1), seen.get(setting), "missing or repeated " + setting);
                    }
                }
            }
        } finally {
            logger.removeAppender(capture);
            capture.stop();
            logger.setLevel(originalLevel);
        }
    }

    @Test
    void productionWarningsNominalAndUpperBoundaryControls() throws Exception {
        check(nominal(), Collections.emptyMap());
        Properties p = nominal();
        p.setProperty("WINDOW_SIZE", "1001");
        p.setProperty("MAX_WINDOWS", "501");
        p.setProperty("security.protocol", "SASL_SSL");
        p.setProperty("state.dir", "/var/lib/tmp-state");
        check(p, Collections.emptyMap());
    }

    @Test
    void productionWarningsEachSmallWindowDimension() throws Exception {
        Properties smallWindow = nominal();
        smallWindow.setProperty("WINDOW_SIZE", "999");
        check(smallWindow, warning("WINDOW_SIZE", "999"));
        Properties smallCount = nominal();
        smallCount.setProperty("MAX_WINDOWS", "499");
        check(smallCount, warning("MAX_WINDOWS", "499"));
    }

    @Test
    void productionWarningsLargeOtherDimensionDoesNotHideSmallOneOrOverflow() throws Exception {
        Properties p = nominal();
        p.setProperty("WINDOW_SIZE", "10000000");
        p.setProperty("MAX_WINDOWS", "499");
        check(p, warning("MAX_WINDOWS", "499"));
        p = nominal();
        p.setProperty("WINDOW_SIZE", "999");
        p.setProperty("MAX_WINDOWS", "1000000");
        check(p, warning("WINDOW_SIZE", "999"));
    }

    @Test
    void productionWarningsTemporaryStateDirectories() throws Exception {
        for (String path : new String[] { "/tmp", "/tmp/dedup", "/var/tmp", "/var/tmp/dedup" }) {
            Properties p = nominal();
            p.setProperty("state.dir", path);
            check(p, warning("state.dir", path));
        }
        for (String path : new String[] { "/tmp-production", "/var/tmp-production" }) {
            Properties p = nominal();
            p.setProperty("state.dir", path);
            check(p, Collections.emptyMap());
        }
    }

    @Test
    void productionWarningsNoStandby() throws Exception {
        Properties p = nominal();
        p.setProperty("num.standby.replicas", "0");
        check(p, warning("num.standby.replicas", "0"));
    }

    @Test
    void productionWarningsAcceptResolvedNumericKafkaProperties() throws Exception {
        Properties p = nominal();
        p.put("num.standby.replicas", Integer.valueOf(1));
        check(p, Collections.emptyMap());
        p.put("num.standby.replicas", Integer.valueOf(0));
        check(p, warning("num.standby.replicas", "0"));
    }

    @Test
    void productionWarningsBothPlaintextSecurityModes() throws Exception {
        for (String protocol : new String[] { "PLAINTEXT", "SASL_PLAINTEXT" }) {
            Properties p = nominal();
            p.setProperty("security.protocol", protocol);
            p.setProperty("sasl.jaas.config", "DO-NOT-LOG-THIS-SECRET");
            check(p, warning("security.protocol", protocol));
        }
    }

    @Test
    void productionWarningsCombinedTestEnvironmentRollout() throws Exception {
        Properties p = nominal();
        p.setProperty("WINDOW_SIZE", "10");
        p.setProperty("MAX_WINDOWS", "5");
        p.setProperty("state.dir", "/tmp/var/lib/kafka-streams/state");
        p.setProperty("num.standby.replicas", "0");
        p.setProperty("security.protocol", "PLAINTEXT");
        Map<String, String> expected = new HashMap<>();
        for (String key : p.stringPropertyNames()) {
            expected.put(key, p.getProperty(key));
        }
        check(p, expected);
    }

    @Test
    void productionWarningsLifecycleFailFast() throws Exception {
        checkLifecycle(true);
    }

    @Test
    void productionWarningsLifecycleTerminationSignal() throws Exception {
        checkLifecycle(false);
    }

    @Test
    void productionWarningsMonitorCleanupJoinsWorkerAndPreservesInterrupt() throws Exception {
        ScheduledExecutorService monitor = Executors.newSingleThreadScheduledExecutor();
        CountDownLatch started = new CountDownLatch(1);
        AtomicBoolean workerFinished = new AtomicBoolean(false);
        monitor.execute(() -> {
            started.countDown();
            try {
                Thread.sleep(10000);
            } catch (InterruptedException e) {
                workerFinished.set(true);
            }
        });
        try {
            assertTrue(started.await(5, TimeUnit.SECONDS));
            Thread.currentThread().interrupt();
            PartitionDedupApp.stopMonitor(monitor);
            assertTrue(Thread.currentThread().isInterrupted());
            assertTrue(workerFinished.get());
            assertTrue(monitor.isTerminated());
        } finally {
            Thread.interrupted();
            monitor.shutdownNow();
        }
    }

    private static void checkLifecycle(boolean failFast) throws Exception {
        Path directory = Files.createTempDirectory("airgap-warning-lifecycle-");
        Path output = directory.resolve("output.log");
        Process process = null;
        try {
            ProcessBuilder builder = new ProcessBuilder(
                    System.getProperty("java.home") + "/bin/java", "-cp",
                    System.getProperty("java.class.path"), PartitionDedupApp.class.getName());
            builder.redirectErrorStream(true).redirectOutput(output.toFile());
            Map<String, String> env = builder.environment();
            env.put("WINDOW_SIZE", "10");
            env.put("MAX_WINDOWS", "5");
            env.put("STATE_DIR_CONFIG", directory.resolve("state").toString());
            env.put("BOOTSTRAP_SERVERS", "127.0.0.1:1");
            env.put("FAIL_FAST", Boolean.toString(failFast));
            env.put("FAIL_FAST_STARTUP_TIMEOUT_MS", "1000");
            env.put("RETRY_BACKOFF_MS", "1000");
            env.put("RETRY_BACKOFF_MAX_MS", "1000");
            env.put("KAFKA_REQUEST_TIMEOUT_MS", "1000");
            env.put("KAFKA_DEFAULT_API_TIMEOUT_MS", "1000");
            process = builder.start();
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(10);
            while (!Files.readString(output).contains("[PRODUCTION-CONFIG] phase=startup ")) {
                assertTrue(process.isAlive(), Files.readString(output));
                assertTrue(System.nanoTime() < deadline, "startup warning timeout");
                Thread.sleep(20);
            }
            if (!failFast) {
                process.destroy();
            }
            assertTrue(process.waitFor(20, TimeUnit.SECONDS),
                    "dedup shutdown timeout\n" + Files.readString(output));
            if (failFast) {
                assertEquals(1, process.exitValue(), "fail-fast must retain its failure exit code");
            }
            List<String> startup = new ArrayList<>();
            List<String> shutdown = new ArrayList<>();
            boolean finalBlock = false;
            for (String line : Files.readAllLines(output, StandardCharsets.UTF_8)) {
                if (line.contains("[PRODUCTION-CONFIG] phase=startup ")) {
                    startup.add(line.substring(line.indexOf(" setting=")));
                }
                if (line.contains("[PRODUCTION-CONFIG] phase=shutdown ")) {
                    finalBlock = true;
                    shutdown.add(line.substring(line.indexOf(" setting=")));
                } else if (finalBlock && !line.trim().isEmpty()) {
                    fail("application log after shutdown warnings: " + line);
                }
            }
            assertFalse(startup.isEmpty());
            assertEquals(startup, shutdown, Files.readString(output));
        } finally {
            if (process != null && process.isAlive()) {
                process.destroyForcibly();
                process.waitFor(5, TimeUnit.SECONDS);
            }
            try (java.util.stream.Stream<Path> paths = Files.walk(directory)) {
                for (Path path : (Iterable<Path>) paths.sorted(java.util.Comparator.reverseOrder())::iterator) {
                    Files.delete(path);
                }
            }
        }
    }
}
