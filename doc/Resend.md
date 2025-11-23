# Resend

Even if we use several sending threads and streams over multiple diodes, eventually some events will be marked as missing. Missing events will be written to the topic defined by PartitionDedupApp's GAP_TOPIC. From that topic, we can extract the information needed to resend all missing events.

## Resend Bundle and the applications to resend events

A resend of events consists of three steps:

1. Create a resend bundle (JSON file) with `create` from the downstream Kafka.
2. Move or copy that bundle to a machine with network access to the upstream Kafka.
3. Run the `resend` app with the name and location of the resource bundle as a parameter to read the missing events from Kafka and send them to the receiver with UDP once again.

When all events have been resent, the gaps downstream should disappear, or at least decrease. You can check by inspecting the GAP_TOPIC or with Jolokia.

### create

This application will get all data from the GAP_TOPIC topic and create a JSON file with the topic name upstream, all partitions, and their respective missing offsets. In order for this to work, we must make sure the GAP_TOPIC only stores one version of our gaps, so we always get the latest one.

If the GAP_TOPIC topic has the property `cleanup.policy=compact`, then only the latest record for each key will be retained after compaction. Newer records with the same key will overwrite older ones.

To check the property value:

```sh
kafka-configs.sh --bootstrap-server <broker> --entity-type topics --entity-name gaps --describe
```

To add the property to an existing topic:

```sh
kafka-configs.sh --bootstrap-server <broker> --entity-type topics --entity-name <topic-name> --alter --add-config cleanup.policy=compact
```

You can also remove and recreate the gaps topic

```sh
kafka-topics.sh --delete --topic <topic-name> --bootstrap-server <broker>

kafka-topics.sh --create --topic <topic-name> --bootstrap-server <broker> --partitions 5 --config cleanup.policy=compact
```

The application `create` takes the following parameters:

- bootstrapServers={Kafka connection url}
- topic={topic in Kafka to read from}
- groupID={Group name in Kafka}
- logLevel={DEBUG, INFO, WARN, ERROR or FATAL}
- logFileName={Log to file instead of console}
- limit={'first' or 'all'. If 'first' then only the first missing offset is reported in the result}
- resendFileName={Name of a file to receive the JSON result. If empty the result will be printed to the console}
- certFile={path to crt file if using TLS with Kafka}
- keyFile={path to key file for the certFile}
- keyPasswordFile={Path to a file containing the password to decrypt an encrypted keyFile}
- caFile={path to crt file of the issuer of the server certificate}

The parameters can be supplied in a properties file, as environment variables or as command line overrides.

Example:

```sh
./create create.properties --topic=gaps --bootstrap-servers=localhost:9092
```

The output will be written to stdout, so if you want to save the result to a file, you can use:

```sh
./create --topic=gaps --bootstrap-servers=localhost:9092 --resendFileName=filename.json
```

After creating the JSON file, inspect it to ensure it contains valid JSON.

Now, copy or move the file to the upstream network. Any computer upstream that has access to the diode and the Kafka cluster will suffice, but in most cases the best choice will be the same machine where the upstream service is installed.

Here, we will run another application that will read and parse the JSON file and query the Kafka cluster for the missing events. Then the events will be sent, just as the upstream application does. Collisions should not be a problem, since most organization uses switches. Since the upstream side of the diode will eventually send the events without collisions, the downstream side will also receive the same events.

### resend

The `resend` application takes similar parameters as the `create` application:

- id={id for some logs}
- bootstrapServers={Kafka connection url}
- topic={topic in Kafka to read from}
- groupID={Group name in Kafka}
- nic={Network Interface Card. What card to use for resend}
- targetIP={Where to send the logs (ipv4 or ipv6 or hostname)}
- targetPort={Port number to send the logs to (UDP)}
- payloadSize={Max amount of payload to include in each UDP packet}
- from={If present, specify a timestamp for the first log to read. Format: 1970-01-01T00:00:00Z (RFC3339)}
- to={If present, specify a timestamp for the last log to read.}
- encryption={true/false, default is false}
- publicKeyFile={file with the public key of the receiver (downstream)}
- generateNewSymmetricKeyEvery={seconds between symmetric key generation}
- eps={events per second limiter. Leave out or set to -1 for no limit}
- logLevel={DEBUG, INFO, WARN, ERROR or FATAL}
- logFileName={Log to file instead of console}
- resendFileName={Name of a file to read the JSON result from}
- compressWhenLengthExceeds={compress messages when length exceeds this value, 0 means no compression. default is 0}
- certFile={path to crt file if using TLS with Kafka}
- keyFile={path to key file for the certFile}
- keyPasswordFile={Path to a file containing the password to decrypt an encrypted keyFile}
- caFile={path to crt file of the issuer of the server certificate}
- limit={limit the created file to just the first missing (`first`) or include all gaps (`all`)}
- compressWhenLengthExceeds={compress messages when length in bytes exceeds this value, 0 means no compression}
- partition={partition to read from}
- offsetFrom={offset to start reading from}
- offsetTo={offset to stop reading at}

When started, the `resend` will read and emit the events as fast as possible, without throttling if the eps is not set. When all events have been delivered, the application terminates.

The setting `limit` can be used if a lot of gaps are present. Instead of writing all the gaps to the resend file, only the first gap for each partition is recorded and the `resend` application will resend that gap and all events after that for each partition.

In case file copy to upstream is not feasable, the command line overrides can be used to resend events.

#### Command line overrides

When using `create` with limit=first, only the first offset for each partition will be saved in the file. The file will basically contain {topic_name, [partition, offset]*}. In case of a few partitions, it's easy to manually run commands to resend the data from that offset and forward. The deduplication should take care of any duplicates, as log as the events are in working memory of the deduplicator.

The configuration for `create` is basically the same as for `upstream`. You can use that configuration and just override a few arguments to do manual resend.

##### To resend with partition and offsetFrom

To resend for a topic: topicName, partition: 1 and offset: 12345678:

```bash
./resend upstream.properties --topic=topicName --partition=1 --offsetFrom=12345678
```

The resend will not write any log files during sending. If you have a lot of events in the normal event stream, you can slow down the resend by adding an argument:

```bash
--eps=500
```

You can also add an argument for the resend to stop at a specified offset with `offsetTo` and you can also add a timestamp filter.

```bash
--from=yyyy-MM-ddTHH:mm:ss.sssZ --to=yyyy-MM-ddTHH:mm:ss.sssZ
```

The command line overrides ensures that you don't need a resend bundle file to be able to resend events, but to utilize the bandwith best, you should use a full resend bundle.
