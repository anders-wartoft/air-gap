# air-gap Kafka to Kafka Topic Transfer over UDP with Guaranteed Delivery
This project aims to solve the problem of transferring events from a Kafka topic in near real time over an unsecure UDP connection with guaranteed once-only delivery of the events. This can be useful, e.g., for transmitting log events over a hardware diode. The information leaving the sending side may be encrypted with symmetric keys and the key exchange is automatic with use of public key encryption. 

In Kafka, events may have a key and a value. The key part of events upstreams will be lost in the transmission, since the key part downstream is used for message identification and thus detection of lost messages (gap-detection). 

For resend of lost events that gap-detection identifies, a back channel must be present but can be a manual routine with input from keyboard or from a file transfer. The information is mainly what topic, partition and id to start reading from. The easiest way to do that is to use the create-resend-bundle application that reads the Kafka Gap topic and creates a file with the missing events. The file needs to be copied to upstream and then used with the resend-missing application that reads the missing events and transmits them again to the UDP receiver.

## Quick Start

1. **Clone and build:**

```bash
git clone https://github.com/anders-wartoft/air-gap.git
cd air-gap
make all
```

This will build the upstream and downstream binaries as well as the Kafka Streams Java application for deduplication.

2\. **Run a local test (no Kafka required):**

- Edit `config/upstream.properties` and `config/downstream.properties` as needed (set `targetIP` to your local IP).
- In one terminal:

```bash
go run src/cmd/downstream/main.go config/downstream.properties
```

- In another terminal:

```bash
go run src/cmd/upstream/main.go config/upstream.properties
```

- You should see messages received in the first terminal.

3\. **Next steps:**

- Connect to real Kafka by editing the config files.
- Enable encryption by setting `publicKeyFile` and related fields.
- See the rest of this README and [Deduplication.md](doc/Deduplication.md) for advanced usage, gap detection, and production deployment.
- A short [Installation and Configuration.md](doc/Installation%20and%20Configuration.md) guide is available.
- More help for setting up encryption to Kafka can be found here: [Kafka-encryption.md](doc/Kafka-encryption.md)
- See [Monitoring.md](doc/Monitoring.md) on how to monitor the applications for resource usage etc.
- To set up reduncancy and/or load balancing, see [Redundancy and Load Balancing.md](doc/Redundancy%20and%20Load%20Balancing.md)
- Finally, there is a [Resend.md](doc/Resend.md) guide that .

## Notation

There are four executable files that together constitutes the transfer software.

- Upstream - on the sending side of the diode, also used as the name of the program that consumes Kafka events and produces UDP packets
- Downstream - on the receiving side of the diode, also used as the name of the program that receives UDP packets and produces Kafka events for the receiving Kafka
- gap-detector - The program that consumes the freshly written Kafka events from Downstream and looks for missing events. This will also deduplicate the events, so if an event is received more than once downstream, only the first of these will be delivered
- set-timestamp - This progam consumes information from the gap-detector and instructs the sending upstream process to restart at an earlier point in time in the upstream Kafka event stream so the lost events may be sent again

To use guaranteed delivery, you must be able to copy information from the gap-detector that runs downstream to the set-timestamp program that runs upstream. The information that needs to traverse the diode in the wrong direction is basically a topic id, partition id and position for the first lost event. set-timestamp is an application that uses that information, queries Kafka upstream on the last timestamp before that event and configures the upstream application to start reading at that timestamp.

![air-gap flow](doc/img/air-gap%20flow.png)

N.B., neither the configuration files nor the key files are shown in the above image.

upstream reads from a Kafka topic specified in the upstream configuration file. It then enrypts the data and sends it over the UDP connection, that might include a hardware diode. Packets that are more than MTU in size (with header) will get fragmented and sent as several UDP packets. Downstream listens to UDP packets, performs defragmentation and decryption of the content and writes to a Kafka instance and topic specified in the downstream configuration file.

Downstream Kafka contains three topics: one that downstream writes to, that is read by the deduplicator and saved as a deduplicated stream of events. Any missing events are identified by the key they should have by the gap-detector and those gaps are reported to the third Kafka topic downstream.
The first topic may contain duplicates but the one from the deduplicator should very rarely contain duplicates. Consume the gap-detector output topic, and send to your SIEM of choice. The data sent from Kafka upstream to Kafka downstream is treated as bytes and not strings, so any string encoding in upstream should be present in the final Kafka topic. Binary data, like files and images are also acceptable payloads to the downstream Kafka.

## Getting started

### Very simple use case

To enable users to get started without Kafka and without hardware diode, use the following properties files:

- upstream.properties
- downstream.properties

These properties files are configured for getting a few random strings instead of reading from Kafka and to send with UDP without encyption. Change the targetIP in upstream.properties to the one you would like to send to. The targetIP in downstream.properties is set to 0.0.0.0 so it will bind to all addresses.

In one terminal, start the server with:

```bash
go run src/cmd/downstream/main.go config/downstream.properties
```

In a new terminal, start the client (sender) with:

```bash
go run src/cmd/upstream/main.go config/upstream.properties
```

A few messages should now be sent from upstream and received by downstream. From here, add encryption and connections to Kafka to enable all features.

## Principle of Transmission

### Upstream

Upstream has two main purposes, to read events from the upstream Kafka, and to send the events to the UDP receiver. Since the UDP transmission may be insecure, we may encrypt the events using symmetric AES256 in GCM mode. The key is generated on startup, and at configurable intervals. The key is also sent over the UDP transmission to the receiver, encrypted with the receiver's public key, that we store in a file upstream. To change the public key, it should suffice to add a new key and change the configuration file to point to the new key. When the next key generation is set, the new key will be used instead of the old.

The encryption is only enabled if the `encrypted` property is set to true and the `publicKeyFile` is set to the downstream public certificate pem file.

### Downstream

Downstrem should receive UDP data (encrypted and unencrypted) and write events to the downstream Kafka on a configured topic. When upstream starts, cleartext events will be generated, as well as an encrypted key (if encryption is enabled). The cleartext messages should be forwarded to the downstream Kafka, key exchange messages should be handled internally and encrypted messages should be decrypted and then forwarded to the downstream Kafka.

On downstream startup, a file glob is read from the configuration file. When a key exchange message is received, the file glob is used to load all private key files that matches the glob. The encrypted message is decrypted with all the private keys until one succeeds. To know if an encryption was successful or not, the symmetric key that upstream sends is prepended with a known string before encryption, a so called guard. When the decrypted message starts with the guard, the rest is used as the new symmetric key. Downstream writes the events to the downstream Kafka with the upstream Kafka topic name, partition id and position as the downstream key. The name of the downstream Kafka topic downstream writes to is configurable. That topic is then consumed by gap-detection.

#### Performance

UDP receive is normally faster than Kafka writes. The downstream application tries to safeguard against lost packets by using a lightweight thread that receives events, decrypts them using AES256 (quite fast) and then adds the events to an array. Another thread consumes the array and writes to Kafka using the async protocol (that also returns immediately and processes the write in yet another thread). If the performance is not enough, first try to add nodes to the Kafka cluster and add the nodes to the bootstrapServers configuration in the downstream process. You can also try to add several events together before writing them to the upstream Kafka, since there is some overhead for each Kafka event, especially for writing. As a last resort, the upstream sender can be set to throttle (eps property), e.g., by adding a small time.Sleep after each sent event. You should be able to securely transmit tens of thousand events every second using one transmission chain, but for large installations you might have to add more sender/receiver chains, as well as upgrade the Kafka instances.

### Automatic resend

Since UDP is an unreliable protocol, you can set up air-gap to automatically resend logs at specific time intervals. In the upstream property file, add the following property:
`sendingThreads=[{"Name": seconds-delay}, ... ]`

Example:

```bash
groupID=testGroup
sendingThreads=[{"now": 0}, {"3minutes-ago": -180}]
```

For each name-dealy, a thread will be created in upstream. Each thread connects to Kafka with a group name that consists of the groupID from the property file, a "-" character, and the name from the sendingThreads property. In the example above, two threads will be created. One named "now" with 0 seconds delay and one named "3minutes-ago" with 180 seconds delay.

The thread with name "now" will connecto to Kafka with a group id of "testGroup-now" and the other thread "testGroup-3minutes-ago".

When a thread reads a message in Kafka, it will check if the Kafka timestamp - the delay (delay is a negative number) is at least equal to, or greater than, the current time. If not, it will sleep until the time is right to send.

If a message is read but not delivered (because the thread is sleeping) and the application terminates, then the

### Deduplication and Gap Detection

This is covered in the [Deduplication.md](doc/Deduplication.md) documentation

### Using Gap Detection Events to Resend from Upstream

See [Resend.md](doc/Resend.md) for details regarding the create and resend applications.

## Keys

Generate keystore with certificate or obtain otherwise.

```bash
keytool -genkey -alias keyalias -keyalg RSA -validity 365 -keystore keystore.jks -storetype JKS
```

Export the java keystore to a PKCS12 keystore:

```bash
keytool -importkeystore  -srckeystore keystore.jks  -destkeystore keystore.p12  -deststoretype PKCS12 -srcalias keyalias
```

Export certificate using openssl:

```bash
openssl pkcs12 -in keystore.p12  -nokeys -out cert.pem
```

Export unencrypted private key:

```bash
openssl pkcs12 -in keystore.p12  -nodes -nocerts -out key.pem
```

Now, add the cert.pem to upstream (public key) and the
key.pem to downstream. Downstream can have several private keys,
if all of the filenames are covered by the same file glob

## Encryption

The provided solution encrypts data in transit over the UDP connection. If needed, add TLS and authentication to all the Kafka connections. If the event generation is also secured with Kafka TLS, then no part of the event chain needs to be in clear text.

For information about Kafka and TLS, see e.g., https://dev.to/davidsbond/golang-implementing-kafka-consumers-using-sarama-cluster-4fko

## Configuration

### Upstream Settings

The upstream OS must be configured for static route and arp for the ip address used to send UDP. Upstream will need both a public key and a configuration file with the following properties:

```bash
id=Upstream_1
nic=en0
targetIP=127.0.0.1
targetPort=1234
source=kafka
bootstrapServers=192.168.153.138:9092
topic=transfer
groupID=test
# The from parameter will issue a new kafka client id
# and search from the beginning of the topic until the
# insert time for the event is at least this time, then
# start to emit the events. Handy for resending of events
# format for from=2024-01-28T10:24:55+01:00
from=
publicKeyFile=certs/server2.pem
# Every n seconds, generate a new symmetric key
generateNewSymmetricKeyEvery=500
# Read the MTU from the nic (and subtract space for IPv6 and UDP headers), or set manually
payloadSize=auto
```

All configuration can be overridden by environment variables. In the case a file is parsed that will be parsed first and may result in configuration errors. After that, any environment variables are checked and, if found, will overwrite the file configuration.

The environment variables are named as:

```bash
AIRGAP_UPSTREAM_{variable name in upper case}
```

Example:

```bash
export AIRGAP_UPSTREAM_ID=NEW-ID
export AIRGAP_UPSTREAM_NIC=ens0
export AIRGAP_UPSTREAM_TARGET_IP=255.255.255.255
...
```

### All settings for upstream

| Config file property name | Environment variable name | Default value | Description |
|--------------------------|--------------------------|---------------|-------------|
| id                       | AIRGAP_UPSTREAM_ID       |               | Name of the instance. Will be used in logging and when sending status messages |
| logLevel                  | AIRGAP_UPSTREAM_LOG_LEVEL  | info         | debug, info, error, warn, fatal |
| soruce                  | AIRGAP_UPSTREAM_SOURCE  | random         | Where to get the information to send. kafka or random. Random is just a string with a counter at the end, not really random data. |
| nic                      | AIRGAP_UPSTREAM_NIC      |               | What nic to use for sending to downstream |
| targetIP                 | AIRGAP_UPSTREAM_TARGET_IP  |               | Downstream air-gap ip address, if IPv6, enclose the address with brackets, like [::1] |
| targetPort               | AIRGAP_UPSTREAM_TARGET_PORT |               | Downstream air-gap ip port |
| bootstrapServers         | AIRGAP_UPSTREAM_BOOTSTRAP_SERVERS |               | Bootstrap url for Kafka, with port |
| topic                    | AIRGAP_UPSTREAM_TOPIC    |               | Topic name in Kafka to read from |
| groupID                  | AIRGAP_UPSTREAM_GROUP_ID |               | The prefix for the group to use when reading from Kafka. The complete group id will be this value, concatenated with the name from the sendingThreads. This will give each sending thread a unique group id |
| payloadSize              | AIRGAP_UPSTREAM_PAYLOAD_SIZE | 0             | 0 - ask the NIC for the MTU and subtract 68 bytes from that (IPv6 + UDP header) or set manually to the payload size (available size of UDP packets, i.e., MTU - headers) |
| from                     | AIRGAP_UPSTREAM_FROM     |               | Don't read anything from Kafka that was delivered to Kafka before this timestamp |
| enryption                | AIRGAP_UPSTREAM_ENCRYPTION | false       | true - encrypt all payload when sending to downstream air-gap. Status messages are still in clear text to help configuration and set-up |
| publicKeyFile            | AIRGAP_UPSTREAM_PUBLIC_KEY_FILE |               | If encryption is on, this is the public key of the receiver |
| source                   | AIRGAP_UPSTREAM_SOURCE.         | random        | source of the messages, kafka or random. Use random for testing only |
| generateNewSymmetricKeyEvery | AIRGAP_UPSTREAM_GENERATE_NEW_SYMMETRIC_KEY_EVERY |               | How often should we change the encryption key (symmetric key)? |
| logFileName              | AIRGAP_UPSTREAM_LOG_FILE_NAME |          | If configured, all logs will be written to this file instead of the console. This will take effekt after the configuration is read and no errors occurs |
| sendingThreads           | AIRGAP_UPSTREAM_SENDING_THREADS | {"now": 0} | How many times, and when, whould we send each event? |
| certFile                 | AIRGAP_UPSTREAM_CERT_FILE |               | For TLS to Kafka, add a certificate pem encoded file here |
| keyFile                  | AIRGAP_UPSTREAM_KEY_FILE  |               | The private key for the certFile certificate |
| caFile                   | AIRGAP_UPSTREAM_CA_FILE   |               | The CA that issued the Kafka server's certificate |
| deliverFilter            | AIRGAP_UPSTREAM_DELIVER_FILTER |               | Filter so not all events from Kafka is sent. Can be used to enable load balancing (see Load Balancing chapter below) |
| topicTranslations        | AIRGAP_UPSTREAM_TOPIC_TRANSLATIONS |              | If you need to rename a topic before sending the messages you can use this: `{"inputTopic1":"outputTopic1","inputTopic2":"outputTopic2"}`. Here, if the name of a topic upstreams is `inputTopic1` it will be sent as `outputTopic1` from upstream |
| logStatistics                   | AIRGAP_UPSTREAM_LOG_STATISTICS  | 0             | How often should a statistics event be written to the console or log file. Valus is in seconds. 0 - no logging |
| compressWhenLengthExceeds | AIRGAP_UPSTREAM_COMPRESS_WHEN_LOG_EXCEEDS | 0           | Using compression (gzip) on short events can make them longer. The break-even length is around 100 bytes for the gzip compression. Set this to a value (ideally above 1200) to gzip longer events |

### Downstream Settings

Downstream has a similar configuration file as upstream. Note that downstream has mtu as a property and not payloadSize

```bash
id=Downstream_1
nic=en0
targetIP=127.0.0.1
targetPort=1234
bootstrapServers=192.168.153.138:9092
privateKeyFiles=certs/private*.pem
target=kafka
verbose=true
mtu=auto
clientId=downstream
```

The property privateKeyFiles should point to one or more private key files that will be tried in decrypting key exchange events from upstream.

### All settings for downstream

| Config file property name | Environment variable name | Default value | Description |
|--------------------------|--------------------------|---------------|-------------|
| id                       | AIRGAP_DOWNSTREAM_ID       |               | Name of the instance. Will be used in logging and when sending status messages |
| logLevel                  | AIRGAP_DOWNSTREAM_LOG_LEVEL  |          |  debug, info, error, warn, fatal  |
| nic                      | AIRGAP_DOWNSTREAM_NIC      |               | What nic to use for binding the UDP port |
| targetIP                 | AIRGAP_DOWNSTREAM_TARGET_IP  |               | Ip address to bind to |
| targetPort               | AIRGAP_DOWNSTREAM_TARGET_PORT |               | Port to bind to |
| bootstrapServers         | AIRGAP_DOWNSTREAM_BOOTSTRAP_SERVERS |               | Bootstrap url for Kafka, with port |
| clientId                 | AIRGAP_DOWNSTREAM_CLIENT_ID    |               | Id to use when writing to Kafka |
| mtu                      | AIRGAP_DOWNSTREAM_MTU      | 0             | 0 - ask the NIC for the MTU, else enter a positive integer |
| target                   | AIRGAP_DOWNSTREAM_TARGET   | kafka         | kafka, cmd and null are valid values. cmd will print the output to the console, null will just forget the received message but collect statistics of received events to calculate EPS when terminated. |
| privateKeyFiles          | AIRGAP_DOWNSTREAM_PRIVATE_KEY_FILES |               | Glob covering all private key files to load |
| logFileName              | AIRGAP_DOWNSTREAM_LOG_FILE_NAME |          | If configured, all logs will be written to this file instead of the console. This will take effekt after the configuration is read and no errors occurs |
| certFile                 | AIRGAP_DOWNSTREAM_CERT_FILE |               | For TLS to Kafka, add a certificate pem encoded file here |
| keyFile                  | AIRGAP_DOWNSTREAM_KEY_FILE  |               | The private key for the certFile certificate |
| caFile                   | AIRGAP_DOWNSTREAM_CA_FILE   |               | The CA that issued the Kafka server's certificate |
| internalTopic            | AIRGAP_DOWNSTREAM_INTERNAL_TOPIC    |  airgap-internal  | Topic name in Kafka to write to (internal logging). Topic name for events from the upstream topics will have the same name as the upstream topic, if not translated by the setting AIRGAP_DOWNSTREAM_TOPIC_TRANSLATIONS |
| topicTranslations        | AIRGAP_DOWNSTREAM_TOPIC_TRANSLATIONS |            | Rename topics with a specified name to another name. Used in multi downstreams setup (see Redundancy and Load Balancing.md) |
| logStatistics            | AIRGAP_DOWNSTREAM_LOG_STATISTICS  | 0             | How often should a statistics event be written to the console or log file. Valus is in seconds. 0 - no logging |
| numReceivers             | AIRGAP_DOWNSTREAM_NUM_RECEIVERS   | 1             | Number of UDP receivers to start that concurrently binds to the targetPort |
| channelBufferSize        | AIRGAP_DOWNSTREAM_CHANNEL_BUFFER_SIZE   | 16384   | Size of the buffered Go channel used between the UDP socket reader goroutines and the worker goroutines in the UDP receiver. It determines how many UDP packets can be queued in memory between being read from the socket and being processed. |
| batchSize                | AIRGAP_DOWNSTREAM_BATCH_SIZE  | 32            | How many messages are grouped together and sent to Kafka in a single batch |
| readBufferMultiplier     | AIRGAP_DOWNSTREAM_READ_BUFFER_MULTIPLIER | 16     | Size of the user-space buffer allocated for each UDP socket read operation. The buffer size is calculated as: `buffer size = mtu * readBufferMultiplier` |
| rcvBufSize               | AIRGAP_DOWNSTREAM_RCV_BUF_SIZE              | 4194304 (4MiB) | Size (in bytes) of the OS-level receive buffer for each UDP socket, set via the SO_RCVBUF socket option. It controls how much incoming UDP data the kernel can buffer for the application before packets are dropped due to the application not reading fast enough. |

### Configuration Examples

numReceivers can be set to 10 or more to bind several threads to the same port. The kernel will distribute the events evenly (at least if the numReceivers is divisable by 5). If two downstream are started with the same configuration, the second started will act as a hot stand-by. Use 1 numReceiver for every free CPU core.

For high throughput on high-end servers, set the numReceivers, channelBufferSize, batchSize, readBufferMultiplier and rcvBufSize to:

#### High-volume UDP-Kafka receiver configuration example

```bash
id=Downstream_HighPerf
nic=enp1s0                # Use a high-performance NIC if available
targetIP=0.0.0.0          # Bind to all interfaces
targetPort=1234           # Use your desired UDP port
bootstrapServers=192.168.153.138:9092
privateKeyFiles=certs/private*.pem
target=kafka
mtu=auto
clientId=downstream
logLevel=INFO
logFileName=downstream.log
```

#### High-performance tuning parameters

```bash
numReceivers=10               # Number of UDP receiver goroutines (match to CPU cores)
channelBufferSize=65536       # Size of Go channel buffer between UDP and Kafka. Large buffer to absorb UDP bursts.
batchSize=128                 # Number of messages to batch per Kafka write. Larger batches improve Kafka throughput.
readBufferMultiplier=32       # User-space UDP read buffer (mtu * multiplier). Larger user-space buffer for big UDP packets.
rcvBufSize=16777216           # OS UDP socket buffer (16 MiB). Large OS buffer to minimize packet loss under load.
logStatistics=5               # Log stats every 5 seconds
```

To test the UDP sender/receiver, use the normal binaries with the following configuration:

```bash
downstream config/downstreap-perf.properties

upstream config/upstream-perf.properties
```

This will generate events automatically in upstream, with a sequence number, send those to downstream that will discard the events but count how many was received and time from first receive to Ctrl-C. Terminate downstream when you don't want to measure any longer and the eps and total number of received events will be displayed. Terminate upstream after downstream.

### Deduplicator and Gap Detector

Deduplicator and Gap Detector is covered in the [Deduplication.md](doc/Deduplication.md) documentation

### Logging

All code uses the log logging packets. To be able to write the most important logs to a file, a new variable, Logger, is introduced, that can accept a file name from the configuration. Important events that can be delivered in the output of the code (event stream) are also sent into the event stream.

## Possible Problems

Some problems that may arise are:

- The UDP sending fails. Check that you have static arp (arp -s) and route (ip route add) enabled.
- If the UDP connection is very unstable, then condider using two instances of upstream/downstream sending the same information over two different hardware diodes to the same Kafka and the same topic. Duplicates should be removed by the gap-detection so the result should be a more stable connection.

## Service

The applications responds to os signals and can be installed as a service in, e.g., Linux. 
See https://fabianlee.org/2022/10/29/golang-running-a-go-binary-as-a-systemd-service-on-ubuntu-22-04/

## Compile

There is a Makefile that will get the latest tag from git and save in version.go, then build upstream and downstream.

```bash
make            # builds both upstream and downstream as well as building the deduplication Java project
make upstream   # builds only upstream
make downstream # builds only downstream
make clean      # removes binaries and version.go, then performs a make
```

To build manually, change directory to the application you would like to build (./src/upstream, ...). 
Compile the applications with `go build {filename}`.

Example:

```bash
cd src/cmd/upstream
go build -o upstream main.go
```

Now we have a compiled file called `upstream`. We can run the application with `./upstream`, but you will still need a configuration file.

## Run the upstream and downstream applications as Linux services (systemd)

### Upstream Service

To turn the application into a service we need to create a service file: `/etc/systemd/system/upstream.service`
Change the paths to where you will install the service binary and comfiguration file

```ini
[Unit]
Description=Upstream AirGap service
ConditionPathExists=/opt/airgap/upstream/bin
After=network.target
StartLimitIntervalSec=60

[Service]
Type=simple
User=root
Group=root
LimitNOFILE=1024
StandardOutput=append:/var/log/airgap/upstream/stdout.log
StandardError=append:/var/log/airgap/upstream/stderr.log

# Set min and max memory (in bytes, e.g., 256M min, 1G max)
MemoryMin=256M
MemoryMax=1G

Restart=on-failure
RestartSec=10

WorkingDirectory=/opt/airgap/upstream
ExecStart=/opt/airgap/upstream/bin/fedora-upstream /opt/airgap/upstream/upstream.properties

# make sure log directory exists and owned by syslog
PermissionsStartOnly=true
ExecStartPre=/bin/mkdir -p /var/log/airgap/upstream
ExecStartPre=/bin/chown root:root /var/log/airgap/upstream
ExecStartPre=/bin/chmod 755 /var/log/airgap/upstream
SyslogIdentifier=root

[Install]
WantedBy=multi-user.target
```

You can also use the environment variables for configuration.
Example systemd service file (`/etc/systemd/system/upstream.service`):

```ini
[Unit]
Description=Upstream AirGap service (env config)
ConditionPathExists=/opt/airgap/upstream/bin
After=network.target
StartLimitIntervalSec=60

[Service]
Type=simple
User=root
Group=root
LimitNOFILE=1024
StandardOutput=append:/var/log/airgap/upstream/stdout.log
StandardError=append:/var/log/airgap/upstream/stderr.log


# Set environment variables for configuration
Environment="AIRGAP_UPSTREAM_ID=Upstream_10"
Environment="AIRGAP_UPSTREAM_NIC=ens160"
Environment="AIRGAP_UPSTREAM_TARGET_IP=192.168.153.14"
Environment="AIRGAP_UPSTREAM_TARGET_PORT=1234"
Environment="AIRGAP_UPSTREAM_BOOTSTRAP_SERVERS=192.168.153.145:9092"
Environment="AIRGAP_UPSTREAM_TOPIC=transfer"
Environment="AIRGAP_UPSTREAM_GROUP_ID=test"
Environment="AIRGAP_UPSTREAM_PAYLOAD_SIZE=auto"
Environment="AIRGAP_UPSTREAM_LOG_FILE_NAME=/var/log/airgap/upstream/upstream.log"
Environment="AIRGAP_UPSTREAM_VERBOSE=true"
#Environment='AIRGAP_UPSTREAM_SENDING_THREADS={"now": 0}'
#Environment="AIRGAP_UPSTREAM_GENERATE_NEW_SYMMETRIC_KEY_EVERY=3600"
#Environment="AIRGAP_UPSTREAM_PUBLIC_KEY_FILE=/opt/kafka/certs/server2.pem"
#Environment="AIRGAP_UPSTREAM_CERT_FILE="
#Environment="AIRGAP_UPSTREAM_KEY_FILE="
#Environment="AIRGAP_UPSTREAM_CA_FILE="
#Environment="AIRGAP_UPSTREAM_DELIVER_FILTER="  

# Set min and max memory (in bytes, e.g., 256M min, 1G max)
MemoryMin=256M
MemoryMax=1G

Restart=on-failure
RestartSec=10

WorkingDirectory=/opt/airgap/upstream
ExecStart=/opt/airgap/upstream/bin/fedora-upstream

# make sure log directory exists and owned by syslog
PermissionsStartOnly=true
ExecStartPre=/bin/mkdir -p /var/log/airgap/upstream
ExecStartPre=/bin/chown root:root /var/log/airgap/upstream
ExecStartPre=/bin/chmod 755 /var/log/airgap/upstream
SyslogIdentifier=root

[Install]
WantedBy=multi-user.target
```

**Notes:**

- You can set any configuration parameter using the `AIRGAP_UPSTREAM_*` environment variables.
- `MemoryMin` and `MemoryMax` are systemd ResourceControl options (see `man systemd.resource-control`). For Go binaries, these set cgroup memory limits (like Java's `-Xms`/`-Xmx`).
- Remove the `configfile.properties` argument from `ExecStart` when using only environment variables.

### Enable serice and start

```bash
sudo systemctl enable upstream.service
sudo systemctl start upstream
```

### Downstream Service

Downstream can be run as a service in the same manner as upstream.

### Create

The `create` application is used to create resend bundle files that contain information of missing events. The bundle should be transferred to the upstream network and consumed by the `resend` application. This application can not be run as a service. The application will terminate when the gaps are exported to the file.

### Resend

The `resend` application is used to either consume resend bundles created by the `create` application, or resend everything from a specified timestamp. The application can not be run as a service. The application will terminate when the information has been resent for all partitions.

## Dependencies

air-gap uses IBM/sarama for the Kafka read/write. For other dependencies, check the go.mod file.

## License

See LICENSE file

## Release Notes

### 0.1.7-SNAPSHOT

- Fixed the following issues:
  - Support encrypted key files for communication with Kafka #5

### 0.1.6-SNAPSHOT

- Removed `topic` configuration from downstream. Downstream uses upstream's topic name, or a translation of that name.
- Fixed the following issues:
  - resend will not accept payloadSize=auto #1
  - Separate internal logging from event stream from Upstream Kafka #2
  - Dedup can't use TLS to connect to Kafka #3

### 0.1.5-SNAPSHOT

- Multiple sockets with SO_REUSEPORT for faster and more reliable UDP receive in Linux and Mac for downstream. Fallback to single thread in Windows.
- `create` application to create resend bundle files downstream
- `resend` application to resend missing events from the resend bundle created by `create`
- `compressWhenLengthExceeds` setting for upstream and resend to compress messages when length exceeds this value. As of now gzip is the only supported algorithm.
- More configuration for upstream and downstream for buffer size optimizations
- Upstream and downstream can translate topic names to other names. Useful in multi source and/or target setups.
- Statistics logging in upstream, downstream and dedup

### 0.1.4-SNAPSHOT

- Changed the logging for the go applications to include log levels. Monitoring and log updates. 
- Documented redundancy and load balancing (see doc folder)
- Documented resend (future updates will implement the new resend algorithm)

### 0.1.3-SNAPSHOT

- Added a Kafka Streams Java Application for deduplication and gap detection. Gap detection not finished.
- Added upstreams filter to filter on the offset number for each partition (used in redundancy an load balancing setups)
- Added a topic name mapping in downstream so a topic with a specified name upstream can be written to another topic downstream (used in redundancy an load balancing setups)
- Added documentation for the new features.
- Added JMX monitoring of the deduplication application. Added system monitoring documentation

### 0.1.2-SNAPSHOT

- All configuration from files can be overridden by environment variables. See Configuration Upstream
- UDP sending have been made more robust
- Transfer of binary data from upstream to downstream is now supported
- Sending a sighup to upstream or downstream will now force a re-write of the log file, so you can rotate the log file and then sighup the application to make it log to a new file with the name specified in the upstream or downstream configuration.
- air-gap now supports TLS and mTLS to Kafka upstream and downstream. 

### 0.1.1-SNAPSHOT

#### Several sending threads

air-gap now supports several sending threads that all have a specified time offset, so you can start one thread that consumes everything from Kafka as soon as it's available, one that inspects Kafka content that was added for an hour ago and so on. See Automatic resend above.