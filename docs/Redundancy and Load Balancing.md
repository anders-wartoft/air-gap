# Redundancy and Load Balancing

air-gap is created with both redundancy and load balancing in mind. If you have a Kafka cluster on both ends of the diode, the transfer software should also be able to run with high availability.

## Redundancy

The most simple case of redundancy is if you have two streams of data from Kafka upstream to Kafka downstream.

![air-gap redundancy](img/air-gap%20redundancy.png)

Here, we configure both air-gap upstreams to read the same topic: "input". They both send to a dedicated downstream. Since Kafka only allows one client at a time to write to a partition and we use partitions for the deduplication process, both downstream can't write to the same topic. The solution is that they write to different topics: "transfer1" and "transfer2". This is achieved by adding the property topicTranslations to the downstream configuration. This property is a JSON object that maps topic names to other topic names. In this case, we map "input" to "transfer1" and "input" to "transfer2" in the respective downstreams.

The downstream receivers can set the property: topicTranslations:

```bash
topicTranslations={"input": "transfer1"}
```

and

```bash
topicTranslations={"input": "transfer2"}
```

respectively. Now, all data for the input topic upstreams will be duplicated in the topics: transfer1 and transfer2. The next step is to configure the deduplicator. The one step that is different than a single stream setup is the RAW_TOPICS field. Normally, we set that to the name of the topic we should read. Now, we set it to a list of topics to read:

```bash
RAW_TOPICS=transfer1,transfer2
```

The key here is that the deduplicator will subscribe to all the topics in the RAW_TOPICS setting. All instances of the deduplicator will merge all streams, and then only accept the events that are in one of the partitions it is configured to handle. Gap state is saved periodically (COMMIT_INTERVAL_MS_CONFIG) to Kafka so if a new instance is started, the new instance will be able to continue almost where the last one stopped.

If the input topic has $n$ partitions, then both the transfer1 and transfer2 partitions need to have $n$ partitions too. The gaps topic should have at least $n$ partitions but if you start more instances of the deduplicator than the upstream instance has partitions, you should have at least that number of partitions in the dedup topic and gaps topic.

Example: say you have $n$ partitions in the upstream topic. Then, create the downstream topic with $n$ partitions. If you would like to have a couple of dedup instances as hot stand-by (say $m$ number of hot stand-by), then you should assign $n+m$ partitions to _both_ the CLEAN_TOPIC and GAP_TOPIC.

**Note:** If your events use GUIDs or other non-partitioned keys (i.e., keys that do not follow the `topic_partition_offset` scheme), these events are still supported. They will be delivered and deduplicated, but are not tied to a specific partition or deduplication instance. Instead, they are distributed among the available instances based on their key value. This allows mixed key types in the same deployment.

### Single downstream with same topic name from two clusters

For the same setup in the main configuration guide, see [README.md](../README.md#example-use-case-two-clusters-same-topic-name-no-topic-rename-possible).

If you have two upstream Kafka clusters with the same topic name (for example `transfer`) and want to merge both streams into one downstream topic with one deduplicator, you can use `partitionStartValue` in one upstream.

Example:

```bash
# Upstream A
topic=transfer
partitionStartValue=0

# Upstream B
topic=transfer
partitionStartValue=100
```

Result:

- Upstream A keeps partitions `0..99`
- Upstream B is remapped to partitions `100..199`
- Both streams can be written to the same downstream topic name while deduplication still works independently per partition

Partition sizing rule:

- The downstream topic must have at least the highest mapped partition index + 1 partitions.
- In the example above, that means at least `200` partitions.
- If a source has `10` partitions and you set `partitionStartValue=100`, the downstream topic must have at least `110` partitions.

Three-cluster example (same topic name in all clusters):

| Cluster | Source partitions | `partitionStartValue` | Downstream partitions |
| ------- | ----------------- | --------------------- | --------------------- |
| A       | `0..4`            | `0`                   | `0..4`                |
| B       | `0..4`            | `10`                  | `10..14`              |
| C       | `0..4`            | `20`                  | `20..24`              |

Matching resend setup per cluster:

- Cluster A resend: `partitionStartValue=0`, `partitionStopValue=5`
- Cluster B resend: `partitionStartValue=10`, `partitionStopValue=5`
- Cluster C resend: `partitionStartValue=20`, `partitionStopValue=5`

This pattern scales to any number of clusters as long as downstream partition windows are disjoint.

Empty partitions are not a deduplication problem, but they still count toward required topic partition capacity.

## Load balancing

There is a filter option in the upstream application so you can choose to just send some events to the UDP receiver. This can be used in a similar setup as the redundancy scheme above:

![air-gap load balancing](img/air-gap%20redundancy.png)

### Filters

First we need to look at filters. Filters are a mechanism in upstream that enable the upstream application to filter out events that do not adhere to a numbering scheme. The scheme is described in three groups of one or more numbers.

If we want to send just odd numbers, we will use the filter: `1,3,5`. Here we have three groups with contents: `1`, `3` and `5`. The first two groups are actually enough to write the filter so the third group is used to check that the numbers in that group will be delivered.

A more complex example can be that we want to deliver just 20% of the events. We can do this in many ways but this is one:

```bash
Environment="AIRGAP_UPSTREAM_DELIVER_FILTER=0,5,10" 
```

Now, offsets that ends with a 0 or 5 will be delivered, no other.

The groups can contain more than one number too. Say we want to deliver everything but events ending in 0 or 5:

```bash
Environment="AIRGAP_UPSTREAM_DELIVER_FILTER=1,2,3,4,6,7,8,9,11,12,13,14" 
```

Here, the groups are `1,2,3,4`, `6,7,8,9` and the control group: `11,12,13,14`, this will deliver every event where the offset ends in 1, 2, 3, 4, 6, 7, 8 or 9.

Now, we are ready to write the load balancing configuration

### Load balancing filter

The setup looks the same, but we configure upstream1 to filter:

```bash
Environment="AIRGAP_UPSTREAM_DELIVER_FILTER=1,3,5" 
```

and the upstream2 to filter:

```bash
Environment="AIRGAP_UPSTREAM_DELIVER_FILTER=2,4,6" 
```

Now, upstream1 will only send odd events from each partition and upstream2 will only send even events. The downstream configuration can be the same as the Redundancy example above, since both downstream will receive events for all the partitions, but only half of the events that are in the upstream input topic.

## Redundancy and Load Balancing at the same time

If we combine the methods above, we can adjust the level of redundancy and load balancing at the same time. In the next example, we send all data over three diodes twice. If one node fails, the other two will still work and send all data at least once, but one third of the data will be delivered twice.

![air-gap redundancy load balancing](img/air-gap%20redundancy-loadbalancing.png)

Here, we configure the upstream filters to:

```bash
Environment="AIRGAP_UPSTREAM_DELIVER_FILTER=1,2,4,5,7,8"
Environment="AIRGAP_UPSTREAM_DELIVER_FILTER=2,3,5,6,8,9"
Environment="AIRGAP_UPSTREAM_DELIVER_FILTER=3,4,6,7,9,10"
```

Here, each event will be delivered by two upstreams, so only two need to be running at the same time. Also, each node only needs to send 2/3 of the data. The deduplicator can now merge the three topics downstream and deduplicate each partition at a time.

You can now configure hardware and air-gap to achieve your preferred level of redundancy and load balancing at the same time.

There's a nice util, contributed by a Tobias Wennberg, to calculate the filters for you: `utils/loadbalance.py`. Just run it and specify the number of upstreams and the level of redundancy you want. It will print the filters for you.

## Drawbacks with Redundancy and Load Balancing

The main drawback of using multiple streams for redundancy and/or load balancing is the increased need for compute resources. Writing to Kafka is resource-intensive, so writing the same data multiple times will increase CPU usage, memory consumption, and network utilization. However, the algorithm scales well, so adding more hardware can resolve performance issues—provided the right components are upgraded. Keep in mind that performance bottlenecks can occur in the Kafka cluster, the network layer, due to insufficient memory (leading to swapping), CPU congestion, and other areas. A section on performance tuning of air-gap may be added in the future.
