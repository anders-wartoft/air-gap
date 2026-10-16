package nu.sitia.airgap.build;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.io.OutputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Set;
import java.util.zip.ZipEntry;
import java.util.zip.ZipOutputStream;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class NativeJarVariantsTest {
    @TempDir
    Path temporaryDirectory;

    @Test
    void generatesOneJarPerRocksDbPlatform() throws Exception {
        Path universal = universalJar(false);

        List<Path> variants = NativeJarVariants.generate(universal);

        assertEquals(13, variants.size());
        for (NativeJarVariants.Platform platform : NativeJarVariants.platforms()) {
            Path variant = temporaryDirectory.resolve("dedup-fat-" + platform.name + ".jar");
            assertTrue(Files.isRegularFile(variant), "missing " + variant);
            assertEquals(new LinkedHashSet<>(platform.requiredEntries),
                    NativeJarVariants.nativeEntries(variant));
            assertTrue(zipContains(variant, "application.class"));
        }
        assertEquals(allNativeEntries(), NativeJarVariants.nativeEntries(universal),
                "the universal JAR must remain unchanged");
    }

    @Test
    void rejectsAnUnmappedRocksDbPlatform() throws Exception {
        Path universal = universalJar(true);

        IOException error = assertThrows(IOException.class,
                () -> NativeJarVariants.generate(universal));

        assertTrue(error.getMessage().contains("unassigned=[librocksdbjni-new-platform.so]"));
    }

    private Path universalJar(boolean includeUnknownRocksPlatform) throws IOException {
        Path jar = temporaryDirectory.resolve("dedup-fat.jar");
        try (OutputStream output = Files.newOutputStream(jar);
                ZipOutputStream zip = new ZipOutputStream(output)) {
            add(zip, "application.class");
            for (String entry : allNativeEntries()) {
                add(zip, entry);
            }
            if (includeUnknownRocksPlatform) {
                add(zip, "librocksdbjni-new-platform.so");
            }
        }
        return jar;
    }

    private static Set<String> allNativeEntries() {
        Set<String> entries = new LinkedHashSet<>();
        for (NativeJarVariants.Platform platform : NativeJarVariants.platforms()) {
            entries.addAll(platform.requiredEntries);
        }
        return entries;
    }

    private static void add(ZipOutputStream zip, String name) throws IOException {
        zip.putNextEntry(new ZipEntry(name));
        zip.write(name.getBytes(java.nio.charset.StandardCharsets.UTF_8));
        zip.closeEntry();
    }

    private static boolean zipContains(Path jar, String name) throws IOException {
        try (java.util.zip.ZipFile zip = new java.util.zip.ZipFile(jar.toFile())) {
            return zip.getEntry(name) != null;
        }
    }
}
