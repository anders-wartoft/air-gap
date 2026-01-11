# Frequently Asked Questions

## I have disabled write for /tmp. Now I get an error running the deduplication

The error occurs because RocksDB (used by Kafka Streams for state storage) extracts its native library to tmp by default, but that directory is write-protected on your machine.

You can fix this by setting the ROCKSDB_SHAREDLIB_DIR environment variable to point to a writable directory before running the Java application:

```bash
export ROCKSDB_SHAREDLIB_DIR=/var/lib/kafka-streams/rocksdb
mkdir -p /var/lib/kafka-streams/rocksdb
java -jar java-streams/target/air-gap-deduplication-fat-*.jar
```

Or if you're running it as a systemd service, add this to your service file:

```bash
[Service]
Environment="ROCKSDB_SHAREDLIB_DIR=/var/lib/kafka-streams/rocksdb"
Environment="STATE_DIR_CONFIG=/var/lib/kafka-streams/state"
```

Alternatively, you can set the Java system property:

```bash
java -Dorg.rocksdb.tmpdir=/var/lib/kafka-streams/rocksdb -jar java-streams/target/air-gap-deduplication-fat-*.jar
```

Make sure the directory you choose is writable by the user running the application. The RocksDB native library will be extracted there instead of tmp.
