package nu.sitia.airgap.build;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashSet;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Set;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;
import java.util.zip.ZipOutputStream;

public final class NativeJarVariants {
    private NativeJarVariants() {
    }

    public static void main(String[] args) throws IOException {
        if (args.length != 1) {
            throw new IllegalArgumentException("usage: NativeJarVariants <universal-fat-jar>");
        }

        for (Path variant : generate(Paths.get(args[0]))) {
            System.out.printf("Created %s (%d bytes)%n", variant, Files.size(variant));
        }
    }

    static List<Path> generate(Path universalJar) throws IOException {
        if (!Files.isRegularFile(universalJar)) {
            throw new IOException("universal fat JAR not found: " + universalJar);
        }

        Set<String> nativeEntries = nativeEntries(universalJar);
        validateRocksPlatforms(nativeEntries);

        List<Path> variants = new ArrayList<>();
        for (Platform platform : platforms()) {
            for (String required : platform.requiredEntries) {
                if (!nativeEntries.contains(required)) {
                    throw new IOException("universal fat JAR lacks " + required
                            + " required by " + platform.name);
                }
            }

            Path output = variantPath(universalJar, platform.name);
            writeVariant(universalJar, output, new HashSet<>(platform.requiredEntries));
            variants.add(output);
        }
        return variants;
    }

    static List<Platform> platforms() {
        return Arrays.asList(
                linux("linux-amd64", "librocksdbjni-linux64.so", "amd64", "x86_64", "amd64"),
                linux("linux-amd64-musl", "librocksdbjni-linux64-musl.so", "amd64", "x86_64", "amd64"),
                linux("linux-x86", "librocksdbjni-linux32.so", "i386", "x86", "i386"),
                linux("linux-x86-musl", "librocksdbjni-linux32-musl.so", "i386", "x86", "i386"),
                linux("linux-arm64", "librocksdbjni-linux-aarch64.so", "aarch64", "aarch64", "aarch64"),
                linux("linux-arm64-musl", "librocksdbjni-linux-aarch64-musl.so", "aarch64", "aarch64",
                        "aarch64"),
                linux("linux-ppc64le", "librocksdbjni-linux-ppc64le.so", "ppc64le", "ppc64le", "ppc64le"),
                linux("linux-ppc64le-musl", "librocksdbjni-linux-ppc64le-musl.so", "ppc64le", "ppc64le",
                        "ppc64le"),
                linux("linux-s390x", "librocksdbjni-linux-s390x.so", "s390x", "s390x", "s390x"),
                linux("linux-s390x-musl", "librocksdbjni-linux-s390x-musl.so", "s390x", "s390x", "s390x"),
                new Platform("macos-amd64",
                        "librocksdbjni-osx-x86_64.jnilib",
                        "darwin/x86_64/libzstd-jni-1.5.6-4.dylib",
                        "org/xerial/snappy/native/Mac/x86_64/libsnappyjava.dylib",
                        "net/jpountz/util/darwin/x86_64/liblz4-java.dylib"),
                new Platform("macos-arm64",
                        "librocksdbjni-osx-arm64.jnilib",
                        "darwin/aarch64/libzstd-jni-1.5.6-4.dylib",
                        "org/xerial/snappy/native/Mac/aarch64/libsnappyjava.dylib",
                        "net/jpountz/util/darwin/aarch64/liblz4-java.dylib"),
                new Platform("windows-amd64",
                        "librocksdbjni-win64.dll",
                        "win/amd64/libzstd-jni-1.5.6-4.dll",
                        "org/xerial/snappy/native/Windows/x86_64/snappyjava.dll",
                        "net/jpountz/util/win32/amd64/liblz4-java.so"));
    }

    private static Platform linux(String name, String rocks, String zstdArch, String snappyArch,
            String lz4Arch) {
        return new Platform(name,
                rocks,
                "linux/" + zstdArch + "/libzstd-jni-1.5.6-4.so",
                "org/xerial/snappy/native/Linux/" + snappyArch + "/libsnappyjava.so",
                "net/jpountz/util/linux/" + lz4Arch + "/liblz4-java.so");
    }

    private static void validateRocksPlatforms(Set<String> nativeEntries) throws IOException {
        Set<String> discovered = new LinkedHashSet<>();
        for (String entry : nativeEntries) {
            if (entry.startsWith("librocksdbjni-")) {
                discovered.add(entry);
            }
        }

        Set<String> configured = new LinkedHashSet<>();
        for (Platform platform : platforms()) {
            configured.add(platform.requiredEntries.get(0));
        }
        if (!discovered.equals(configured)) {
            Set<String> missing = new LinkedHashSet<>(configured);
            missing.removeAll(discovered);
            Set<String> unassigned = new LinkedHashSet<>(discovered);
            unassigned.removeAll(configured);
            throw new IOException("RocksDB platform mapping mismatch; missing=" + missing
                    + ", unassigned=" + unassigned);
        }
    }

    private static Path variantPath(Path universalJar, String platform) {
        String fileName = universalJar.getFileName().toString();
        if (!fileName.endsWith(".jar")) {
            throw new IllegalArgumentException("fat JAR must end with .jar: " + universalJar);
        }
        String variantName = fileName.substring(0, fileName.length() - 4) + "-" + platform + ".jar";
        return universalJar.resolveSibling(variantName);
    }

    private static void writeVariant(Path universalJar, Path output, Set<String> selectedNatives)
            throws IOException {
        Path temporary = output.resolveSibling(output.getFileName().toString() + ".tmp");
        Files.deleteIfExists(temporary);
        try (ZipFile source = new ZipFile(universalJar.toFile());
                OutputStream fileOutput = Files.newOutputStream(temporary);
                ZipOutputStream destination = new ZipOutputStream(fileOutput)) {
            java.util.Enumeration<? extends ZipEntry> entries = source.entries();
            while (entries.hasMoreElements()) {
                ZipEntry entry = entries.nextElement();
                if (isNativeLibrary(entry.getName()) && !selectedNatives.contains(entry.getName())) {
                    continue;
                }

                destination.putNextEntry(new ZipEntry(entry));
                if (!entry.isDirectory()) {
                    try (InputStream input = source.getInputStream(entry)) {
                        copy(input, destination);
                    }
                }
                destination.closeEntry();
            }
        } catch (IOException e) {
            Files.deleteIfExists(temporary);
            throw e;
        }

        try {
            Files.move(temporary, output, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
        } catch (AtomicMoveNotSupportedException e) {
            Files.move(temporary, output, StandardCopyOption.REPLACE_EXISTING);
        }
    }

    static Set<String> nativeEntries(Path jar) throws IOException {
        Set<String> result = new LinkedHashSet<>();
        try (ZipFile zip = new ZipFile(jar.toFile())) {
            java.util.Enumeration<? extends ZipEntry> entries = zip.entries();
            while (entries.hasMoreElements()) {
                ZipEntry entry = entries.nextElement();
                if (!entry.isDirectory() && isNativeLibrary(entry.getName())) {
                    result.add(entry.getName());
                }
            }
        }
        return result;
    }

    private static boolean isNativeLibrary(String name) {
        return name.startsWith("librocksdbjni-")
                || (name.startsWith("linux/") && name.contains("/libzstd-jni-"))
                || (name.startsWith("darwin/") && name.contains("/libzstd-jni-"))
                || (name.startsWith("freebsd/") && name.contains("/libzstd-jni-"))
                || (name.startsWith("win/") && name.contains("/libzstd-jni-"))
                || (name.startsWith("org/xerial/snappy/native/") && !name.endsWith("/README"))
                || (name.startsWith("net/jpountz/util/") && name.contains("/liblz4-java."));
    }

    private static void copy(InputStream input, OutputStream output) throws IOException {
        byte[] buffer = new byte[8192];
        int read;
        while ((read = input.read(buffer)) != -1) {
            output.write(buffer, 0, read);
        }
    }

    static final class Platform {
        final String name;
        final List<String> requiredEntries;

        Platform(String name, String... requiredEntries) {
            this.name = name;
            this.requiredEntries = Collections.unmodifiableList(Arrays.asList(requiredEntries));
        }
    }
}
