# Kafka Mutual Authentication and TLS
Some environments require that the communication with Kafka have to use TLS. They can also require mandatory authentication and authorizaion. This is a small guide to set up mutual authentication and autorization between air-gap and Kafka with self signed certificates.

This guid covers broker certificates, not controller certificates. If your Kafka require certificates for controllers, there are lots of guides for that on the Internet.

N.B., in this manual I don't aim to set up a working production environment, just a test setup to show how the certificates can be used to achieve TLS and mTLS. Please, do NOT use this manual as a blueprint for a production setting, since several properties are not suitable for production. For example: the key length is 2048 for the test certificates and should be at least 4096 in production. The validity length is set to almost 10 years - in a production environment you would like at most 1 year but maybe even weeks or days (if you have auto-renew on your certificates).

## TLS
To encrypt the connection between air-gap and Kafka we will need certificates on at least the Kafka machines. The clients (air-gap) need to be able to validate the certificates so they will need a truststore with the issuer of the Kafka certificates. If you don't have a certificate department or vendor, then we can create all certificates ourself. First, we need a root ca: Certificate Authority.

### Certificat Authority
We need to create a Certificat Authority that will sign all our certificates. In production environments, this is usually made on a non-networked computer that is in a locked cabinet when not used.

### Generate CA private key
```bash
openssl genrsa -out kafka-ca.key 4096
```

### Generate self-signed CA certificate
```bash
openssl req -x509 -new -nodes -key kafka-ca.key -sha256 -days 3650 \
  -out kafka-ca.crt -subj "/CN=MyKafkaCA"
```

### Create Kafka Broker Certificates
Each Kafka broker needs its own cert signed by the CA. Since some organizations use CN (mostly older) and others SAN, Subject Alternative Name, for authentication and authorization, we will should create our certificates with both and also test that we can extract the user id from both. Kafka does, however, seem to only parse the Distinguished Name field, so for this guide we will only use CN to extract a user id but we will create the certificates with the SAN attribute set.

For both SAN and CN, we first create a file for each broker, kafka-upstream and kafka-downstream. Note that the CN must be the same as the hostnames in DNS or the hosts file.

#### kafka-upstream.cnf
```ini
[ req ]
default_bits       = 2048
prompt             = no
default_md         = sha256
req_extensions     = req_ext
distinguished_name = dn

[ dn ]
CN = kafka-upstream.sitia.nu

[ req_ext ]
subjectAltName = @alt_names

[ alt_names ]
DNS.1 = kafka-upstream.sitia.nu
DNS.2 = kafka-upstream.sitia.nu
```

#### kafka-downstream.cnf
```ini
[ req ]
default_bits       = 2048
prompt             = no
default_md         = sha256
req_extensions     = req_ext
distinguished_name = dn

[ dn ]
CN = kafka-downstream.sitia.nu

[ req_ext ]
subjectAltName = @alt_names

[ alt_names ]
DNS.1 = kafka-downstream.sitia.nu
DNS.2 = kafka-downstream.sitia.nu
```


```bash
# Create the keys
openssl genrsa -out kafka-upstream.key 2048
openssl genrsa -out kafka-downstream.key 2048

# Create Signing Requests
openssl req -new -key kafka-upstream.key \
  -out kafka-upstream.csr \
  -config kafka-upstream.cnf

openssl req -new -key kafka-downstream.key \
  -out kafka-downstream.csr \
  -config kafka-downstream.cnf

# Sign with CA for 10 years (not for production)
# Make sure SAN is included by specifying extensions during signing
openssl x509 -req -in kafka-upstream.csr -CA kafka-ca.crt -CAkey kafka-ca.key \
  -CAcreateserial -out kafka-upstream.crt -days 3650 -sha256 \
  -extfile kafka-upstream.cnf -extensions req_ext

openssl x509 -req -in kafka-downstream.csr -CA kafka-ca.crt -CAkey kafka-ca.key \
  -CAcreateserial -out kafka-downstream.crt -days 3650 -sha256 \
  -extfile kafka-downstream.cnf -extensions req_ext

# Inspect the certificates
openssl x509 -in kafka-upstream.crt -text -noout
openssl x509 -in kafka-downstream.crt -text -noout
```

#### Create the client certificates
Each client also needs it's own certificates. Since we would like both SAN and CN available for authentication, we need the cnf files here too:

##### airgap-upstream.cnf
```ini
[ req ]
default_bits       = 2048
prompt             = no
default_md         = sha256
req_extensions     = req_ext
distinguished_name = dn

[ dn ]
CN = airgap-upstream-client

[ req_ext ]
subjectAltName = @alt_names

[ alt_names ]
DNS.1 = airgap-upstream-client
DNS.2 = airgap-upstream-client
```


#### airgap-downstream.cnf
```ini
[ req ]
default_bits       = 2048
prompt             = no
default_md         = sha256
req_extensions     = req_ext
distinguished_name = dn

[ dn ]
CN = airgap-downstream-client

[ req_ext ]
subjectAltName = @alt_names

[ alt_names ]
DNS.1 = airgap-downstream-client
DNS.2 = airgap-downstream-client
```

```bash
# Client key (generate plaintext first)
openssl genrsa -out airgap-upstream.key 2048
openssl genrsa -out airgap-downstream.key 2048

# Encrypt the key files with a passphrase (same format as issuer-provided keys)
# You will be prompted for a passphrase. The encrypted .enc files will be used in production.
openssl rsa -aes256 -in airgap-upstream.key -out airgap-upstream.key.enc
openssl rsa -aes256 -in airgap-downstream.key -out airgap-downstream.key.enc

# Store the passphrase in a file for air-gap to use (keep this file secure!)
# You might want to disable command line history before the following commands, or better yet, use a text editor to enter the text.
echo "YourPassphrase" > airgap-upstream.key.pass
echo "YourPassphrase" > airgap-downstream.key.pass
chmod 600 airgap-upstream.key.pass airgap-downstream.key.pass

# CSR (use the plaintext key for CSR generation)
openssl req -new -key airgap-upstream.key \
  -out airgap-upstream.csr \
  -config airgap-upstream.cnf

openssl req -new -key airgap-downstream.key \
  -out airgap-downstream.csr \
  -config airgap-downstream.cnf

# Sign
openssl x509 -req -in airgap-upstream.csr -CA kafka-ca.crt -CAkey kafka-ca.key \
  -CAcreateserial -out airgap-upstream.crt -days 3650 -sha256 \
  -extfile airgap-upstream.cnf -extensions req_ext

openssl x509 -req -in airgap-downstream.csr -CA kafka-ca.crt -CAkey kafka-ca.key \
  -CAcreateserial -out airgap-downstream.crt -days 3650 -sha256 \
  -extfile airgap-downstream.cnf -extensions req_ext

# Inspect the certificates
openssl x509 -in airgap-upstream.crt -text -noout
openssl x509 -in airgap-downstream.crt -text -noout

```
All upstream and downstream certificates should contain `X509v3 Subject Alternative Name: ...` and `Subject: CN=...`

In order to be able to load the certificates into Kafka, we need to create a keystore and we also need a truststore for Kafka to trust client certificates. When generating the p12-files and keystores, you will be prompted for a password. Save the passwords for the files and keystores. When generating the keystores you will also need to supply the password for the p12 file that it imports.

#### Java Keystore and Truststore
```bash
# Kafka keystore (contains Kafka’s private key + cert)
openssl pkcs12 -export -in kafka-upstream.crt -inkey kafka-upstream.key \
  -chain -CAfile kafka-ca.crt -name kafka-upstream -out kafka-upstream.p12

keytool -importkeystore -destkeystore kafka-upstream.keystore.jks \
  -srckeystore kafka-upstream.p12 -srcstoretype PKCS12 \
  -alias kafka-upstream

# Broker truststore (contains CA to trust clients)
keytool -import -trustcacerts -alias CARoot -file kafka-ca.crt \
  -keystore kafka-upstream.truststore.jks

# Inspect the keystore:
keytool -list -v -keystore kafka-upstream.keystore.jks

# Inspect the truststore:
keytool -list -v -keystore kafka-upstream.truststore.jks
```
and for downstream:
```bash
# Kafka keystore (contains Kafka’s private key + cert)
openssl pkcs12 -export -in kafka-downstream.crt -inkey kafka-downstream.key \
  -chain -CAfile kafka-ca.crt -name kafka-downstream -out kafka-downstream.p12

keytool -importkeystore -destkeystore kafka-downstream.keystore.jks \
  -srckeystore kafka-downstream.p12 -srcstoretype PKCS12 \
  -alias kafka-downstream

# Broker truststore (contains CA to trust clients)
keytool -import -trustcacerts -alias CARoot -file kafka-ca.crt \
  -keystore kafka-downstream.truststore.jks

# Inspect the keystore:
keytool -list -v -keystore kafka-downstream.keystore.jks

# Inspect the truststore:
keytool -list -v -keystore kafka-downstream.truststore.jks
```

### Generate certificates for the air-gap application
air-gap is written in Golang and uses pem certificates. The crt files are actually in pem format. If you open the crt file you can see that it starts with `-----BEGIN CERTIFICATE-----`. 

air-gap will use the .key and .crt files for authenticating to Kafka.

## Configure Kafka for TLS
Copy the files:
- kafka-upstream.keystore.jks
- kafka-upstream.truststore.jks

to `/opt/kafka/config/ssl/` on the upstream Kafka machine(s)

Edit `server.properties`for each Kafka broker and add (change the domain name to your domain name):
```properties
listeners=SSL://0.0.0.0:9093
advertised.listeners=SSL://kafka-upstream.sitia.nu:9093
ssl.keystore.location=/opt/kafka/config/ssl/kafka-upstream.keystore.jks
ssl.keystore.password=changeit
ssl.key.password=changeit
ssl.truststore.location=/opt/kafka/config/ssl/kafka-upstream.truststore.jks
ssl.truststore.password=changeit

# Require client authentication (mTLS)
#ssl.client.auth=required
```

In the server.properties file you need to add the SSL protocol as a broker protocol. Please don't try to set the same port for controller and broker. That will not work. The default configuration for Kafka is 9092 for PLAINTEXT and 9093 for CONTROLLER. Usually, 9093 will be used for SSL but if you do, you need to change the CONTROLLER prot to, e.g., 9080 and maybe 9093 for CONTROLLER SSL, if used. If you try to use 9093 for both CONTROLLER and BROKER SSL you might get:
```
ERROR Error processing message, terminating consumer process:  (org.apache.kafka.tools.consumer.ConsoleConsumer)
org.apache.kafka.common.errors.UnsupportedVersionException: The node does not support METADATA
```

If you want to run the Kafka console applications, you will need a producer.ssl.config and a consumer.ssl.config. For Kafka, you will need your airgap-client certificates in pkcs#12 format with a keystore and a truststore. This is highly recommended to be able to create and view topics from the command line.

```bash
# Create pkcs#12 version of airgap-upstream.crt and key
openssl pkcs12 -export -in airgap-upstream.crt -inkey airgap-upstream.key \
  -chain -CAfile kafka-ca.crt -name airgap-upstream -out airgap-upstream.p12

# Create a keystore that contains the airgap-upstream key and certificate
keytool -importkeystore -destkeystore airgap-upstream.keystore.jks \
  -srckeystore airgap-upstream.p12 -srcstoretype PKCS12 \
  -alias airgap-upstream

# Create a truststore for airgap-upstream that contains the kafka-upstream certificate issuer (our root CA)
keytool -import -trustcacerts -alias CARoot -file kafka-ca.crt \
  -keystore airgap-upstream.truststore.jks
```
Copy the files:
- airgap-upstream.keystore.jks
- airgap-upstream.truststore.jks

to `/opt/kafka/config/ssl/` on the upstream Kafka machine(s) or where you want to use the Kafka command line utilities.

Create a new file: `/opt/kafka/config/producer.ssl.properties`
```properties
security.protocol=SSL
ssl.keystore.location=/opt/kafka/config/ssl/airgap-upstream.keystore.jks
ssl.keystore.password=changeit
ssl.key.password=changeit
ssl.truststore.location=/opt/kafka/config/ssl/airgap-upstream.truststore.jks
ssl.truststore.password=changeit
```

Now you should be able to restart Kafka and run (we only have one kafka so the producer.ssl.properties will work as a consumer.ssl.properties too)
```bash
bin/kafka-console-consumer.sh --topic downstream --bootstrap-server kafka-upstream.sitia.nu:9093 --consumer.config ./config/producer.ssl.properties --from-beginning
```
The command should give the same output as if you ran it on the plaintext port:
```bash
bin/kafka-console-consumer.sh --topic downstream --bootstrap-server kafka-upstream.sitia.nu:9092  --from-beginning
```

Now, we up the difficulty a bit. We add authorization so clients not only need to have a certificate from a valid issues but also need to be present in an allow-list.

## Authorization
When we created the airgap-* certificates, we added a name to them: `airgap-upstream-client` and `airgap-downstream-client`. We will now use those as identifiers in Kafka for authorization.

Kafka seems to only use DN for principal mappings, so we will use the CN instead of SAN for that.
From https://kafka.apache.org/24/generated/kafka_config.html:
For SSL authentication, the principal will be derived using the rules defined by `ssl.principal.mapping.rules` applied on the distinguished name from the client certificate if one is provided;...


First we must require mTLS in the Kafka config:
In the above `server.properties` file, add the following:

```properties
# Require client authentication (mTLS)
ssl.client.auth=required

# Enable authorization (Kafka ≥ 3.x KRaft or ZK)
authorizer.class.name=org.apache.kafka.metadata.authorizer.StandardAuthorizer
# Default deny everyone
allow.everyone.if.no.acl.found=false

# Make our kafka-upstream.sitia.nu user (the user in the Kafka upstream certificate) admin
super.users=User:kafka-upstream.sitia.nu

# And map the CN name to the user name from the certificate.
# This regex should be able to extract names from both the Kafka and upstream/downstream certificates:
ssl.principal.mapping.rules=RULE:^CN=([a-zA-Z0-9.-]+),?.*$/$1/
```

Also, change this line

```properties
listeners=PLAINTEXT://0.0.0.0:9092,SSL://0.0.0.0:9093,CONTROLLER://:9083
```
to
```properties
listeners=SSL://0.0.0.0:9093,CONTROLLER://:9083
```
and

```properties
advertised.listeners=PLAINTEXT://192.168.153.144:9092,SSL://kafka-upstream.sitia.nu:9093
```
to
```properties
advertised.listeners=SSL://kafka-upstream.sitia.nu:9093
```

If you have a cluster, the `inter.broker.listener.name` property must also be set to `SSL` and all the Kafka cluster certificate's CN added as admin with the property `super.users`.

Now, the only way for a broker to access Kafka is with TLS.

Save and restart your Kafka

### Test without certificate
First, try to access your Kafka without TLS

```bash
bin/kafka-console-consumer.sh --topic downstream --bootstrap-server kafka-upstream.sitia.nu:9092  --from-beginning
```

The connection should fail.

### Test with kafka-upstream certificate
Copy the following files to `/opt/kafka/ssl/.`.
* kafka-upstream.p12
* kafka-upstream.truststore.jks
* kafka-upstream.keystore.jks

Create a file config/consumer.mtls.properties
```properties
security.protocol=SSL
ssl.truststore.location=/opt/kafka/ssl/kafka-upstream.truststore.jks
ssl.truststore.password=changeit
ssl.keystore.location=/opt/kafka/ssl/kafka-upstream.keystore.jks
ssl.keystore.password=changeit
ssl.key.password=changeit
```

Now, this command should succeed. 

```bash
bin/kafka-console-consumer.sh --topic downstream --bootstrap-server kafka-upstream.sitia.nu:9093 --consumer.config config/consumer.mtls.properties --from-beginning
```

There might not be any data in Kafka but in that case, the console consumer should be waiting for data to print.

### AirGap certificate to Kafka
Now it's time to add the client certificates to AirGap and connect to Kafka with mTLS.

First, add the topic to Kafka if it doesn't exists.
```bash
bin/kafka-topics.sh --create --topic upstream --bootstrap-server kafka-upstream.sitia.nu:9093 --command-config config/consumer.mtls.properties
```

and the downstream topic too:
```bash
bin/kafka-topics.sh --create --topic downstream --bootstrap-server kafka-upstream.sitia.nu:9093 --command-config config/consumer.mtls.properties
```


Then, set the permissions so that `airgap-upstream-client` can read from and write to the `upstream` topic.
```bash
bin/kafka-acls.sh \
  --bootstrap-server kafka-upstream.sitia.nu:9093 \
  --command-config config/consumer.mtls.properties \
  --add --allow-principal "User:airgap-upstream-client" \
  --operation Write --operation Read --topic upstream
```

the `airgap-downstream-client` must be able to write to the `downstream` topic.
```bash
bin/kafka-acls.sh \
  --bootstrap-server kafka-upstream.sitia.nu:9093 \
  --command-config config/consumer.mtls.properties \
  --add --allow-principal "User:airgap-downstream-client" \
  --operation Write --topic downstream
```


and check the permissions with:
```bash
bin/kafka-acls.sh --bootstrap-server kafka-upstream.sitia.nu:9093 --command-config config/consumer.mtls.properties --list --topic upstream
```

Now we should be able to use the command line tools to create events in Kafka. Note that if you are using LogGenerator to generate logs in Kafka, that utility doesn't support TLS so you will have to open a plaintext port for that.

```bash
bin/kafka-console-consumer.sh --topic upstream --bootstrap-server kafka-upstream.sitia.nu:9093 --consumer.config config/consumer.mtls.properties --from-beginning
```

If the command line tool can access the topic without errors, you can proceed to adding the certificates to air-gap.

air-gap uses consumer groups and Kafka will neeed ACL:s for those too. The consumer groups are named `group`-`name` from the configuration. The `group` parameter is set with the configuration `groupID`, or `AIRGAP_UPSTREAM_GROUP_ID` environment variable. The `name` parameter is the thread name from the `sendingThreads` array. If no `sendingThreads` is given, a default `{"now": 0}`is used so in that case, the name is `now`.

The easiest way here is to add an ACL that accepts all groups:
```bash
bin/kafka-acls.sh --bootstrap-server kafka-upstream.sitia.nu:9093 --command-config config/consumer.mtls.properties --add --allow-principal User:airgap-upstream-client --operation Read --group '*'
```

Or, if you want to be more restrictive, you can use:
```bash
bin/kafka-acls.sh --bootstrap-server kafka-upstream.sitia.nu:9093 --command-config config/consumer.mtls.properties --add --allow-principal User:airgap-upstream-client --operation Read --group test-No-delay
```

Start the upstream air-gap application:
```bash
go run src/upstream/upstream.go config/testcases/upstream-airgap-4.properties
```

If the upstream topic is empty, you can manually add events with the following command.
```bash
bin/kafka-acls.sh --bootstrap-server kafka-upstream.sitia.nu:9093 --command-config config/consumer.mtls.properties --add --allow-principal User:airgap-upstream-client --operation Read --group '*'
```

Add events and press enter to send them to Kafka. Ctrl-C to stop.

The upstream air-gap application should log that values are received and handled.

Congratulations. You can now read from Kafka. To configure downstream for Kafka mTLS, add the 
```properties
# Certificate file
certFile=certs/tmp/airgap-downstream.crt
# Encrypted key file (password-protected)
keyFile=certs/tmp/airgap-downstream.key.enc
# Passphrase file for the encrypted key
keyPassFile=certs/tmp/airgap-downstream.key.pass
# CA file
caFile=certs/tmp/kafka-ca.crt
```

properties to the downstream property file, or set them as environment variables.

**Note:** If your key file is **not** encrypted (plaintext `.key` file), omit the `keyPassFile` property and use `keyFile=certs/tmp/airgap-downstream.key` directly. The air-gap application will detect whether the key is encrypted and only use the passphrase file when needed.

For upstream, the equivalent configuration would be:
```properties
# Certificate file
certFile=certs/tmp/airgap-upstream.crt
# Encrypted key file (password-protected)
keyFile=certs/tmp/airgap-upstream.key.enc
# Passphrase file for the encrypted key
keyPassFile=certs/tmp/airgap-upstream.key.pass
# CA file
caFile=certs/tmp/kafka-ca.crt
``` 

-End