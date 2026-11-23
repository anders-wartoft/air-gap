package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/kversion"
	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/protocol"
	"sitia.nu/airgap/src/udp"
)

const calibrationFrameSize = 1400

var calibrationTopicPattern = regexp.MustCompile(`^airgap-calibration(-[a-zA-Z0-9.-]{1,80})?$`)

func calibrateUDP(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("upstream --calibrate", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	address := flags.String("address", "", "downstream UDP host:port")
	nic := flags.String("nic", "", "optional upstream network interface")
	topic := flags.String("topic", "", "dedicated downstream Kafka topic: airgap-calibration or airgap-calibration-NAME")
	brokers := flags.String("brokers", "", "comma-separated downstream Kafka bootstrap addresses")
	ca := flags.String("kafka-ca-file", "", "enable Kafka TLS with this CA PEM")
	cert := flags.String("kafka-cert-file", "", "optional Kafka client certificate PEM")
	key := flags.String("kafka-key-file", "", "optional Kafka client key PEM")
	password := flags.String("kafka-key-password-file", "", "optional Kafka client key passphrase file")
	rates := flags.String("rates", "12000,24000,40000,60000,85000", "increasing UDP datagram rates to test")
	duration := flags.Duration("duration", 10*time.Second, "send duration per trial (1s..30s)")
	drain := flags.Duration("drain", 3*time.Second, "Kafka observation time after sending (1s..30s)")
	burst := flags.Int("burst", 32, "UDP burst in datagrams (1..256)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	host, portText, err := net.SplitHostPort(*address)
	port, portErr := strconv.Atoi(portText)
	if flags.NArg() != 0 || err != nil || portErr != nil || host == "" || port < 1 || port > 65535 ||
		!calibrationTopicPattern.MatchString(*topic) || *brokers == "" || *duration < time.Second || *duration > 30*time.Second ||
		*drain < time.Second || *drain > 30*time.Second || *burst < 1 || *burst > 256 {
		return fmt.Errorf("calibration requires --address=HOST:PORT, --topic=airgap-calibration[-NAME], --brokers, valid time and burst limits, and no positional arguments")
	}
	var candidates []int
	for _, s := range strings.Split(*rates, ",") {
		rate, err := strconv.Atoi(s)
		if err != nil || rate < 1000 || rate > 200000 || len(candidates) >= 10 ||
			(len(candidates) > 0 && rate <= candidates[len(candidates)-1]) {
			return fmt.Errorf("--rates must contain up to ten strictly increasing values in 1000..200000")
		}
		candidates = append(candidates, rate)
	}
	if (*cert == "") != (*key == "") || (*password != "" && *key == "") || (*cert != "" && *ca == "") {
		return fmt.Errorf("Kafka client TLS requires --kafka-ca-file and a certificate/key pair")
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(strings.Split(*brokers, ",")...),
		kgo.ClientID("airgap-calibration"),
		kgo.MaxVersions(kversion.FromString("3.6.0")),
		kgo.DialTimeout(5 * time.Second),
		kgo.RequestTimeoutOverhead(5 * time.Second),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
	}
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return fmt.Errorf("no certificates found in Kafka CA file")
		}
		tlsConfig := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		if *cert != "" {
			pair, err := kafka.LoadTLSCertificate(*cert, *key, *password)
			if err != nil {
				return err
			}
			tlsConfig.Certificates = []tls.Certificate{pair}
		}
		opts = append(opts, kgo.DialTLSConfig(tlsConfig))
	}
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return fmt.Errorf("connect to verification Kafka: %w", err)
	}
	defer client.Close()
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("connect to verification Kafka: %w", err)
	}
	partitions, err := calibrationTopicPartitions(ctx, client, *topic)
	if err != nil {
		return fmt.Errorf("read pre-created verification topic %q: %w", *topic, err)
	}
	if len(partitions) == 0 || len(partitions) > 64 {
		return fmt.Errorf("verification topic must have 1..64 partitions")
	}
	conn, err := udp.NewUDPConnWithNIC(*address, *nic)
	if err != nil {
		return err
	}
	defer conn.Close()
	probe := calibrationTrial{topic: *topic, rate: 1000, burst: 1, duration: time.Second, drain: *drain, partitions: partitions, limit: len(partitions)}
	sent, received, _, _, err := probe.run(ctx, conn, client, opts)
	if err != nil {
		return fmt.Errorf("routing probe: %w", err)
	}
	if sent != len(partitions) || sent != received {
		return fmt.Errorf("routing probe verified %d/%d partitions; check downstream UDP readiness and topic routing", received, len(partitions))
	}
	trial := func(rate int) (bool, int, error) {
		c := calibrationTrial{topic: *topic, rate: rate, burst: *burst, duration: *duration, drain: *drain, partitions: partitions}
		sent, received, datagrams, elapsed, err := c.run(ctx, conn, client, opts)
		achieved := 0
		if elapsed > 0 {
			achieved = int(float64(datagrams) / elapsed.Seconds())
		}
		fmt.Fprintf(diagnostics, "calibration: cap=%d datagrams/s measured=%d datagrams/s sent=%d verified=%d\n",
			rate, achieved, sent, received)
		return sent > 0 && sent == received, achieved, err
	}
	var recommended int
	for _, rate := range candidates {
		passed, achieved, err := trial(rate)
		if err != nil {
			return err
		}
		if !passed {
			break
		}
		if achieved > rate {
			achieved = rate
		}
		recommended = max(recommended, achieved*80/100)
	}
	if recommended == 0 {
		return fmt.Errorf("no loss-free calibration trial; verify topic routing and try lower rates")
	}
	passed, _, err := trial(recommended)
	if err != nil {
		return err
	}
	if !passed {
		return fmt.Errorf("confirmation at %d datagrams/s lost records; no settings recommended", recommended)
	}
	_, err = fmt.Fprintf(out, "udpPacingDatagramsPerSecond=%d\nudpPacingBytesPerSecond=%d\nudpPacingBurstDatagrams=%d\n",
		recommended, recommended*calibrationFrameSize, *burst)
	return err
}

type calibrationTrial struct {
	topic           string
	rate, burst     int
	duration, drain time.Duration
	partitions      []int32
	limit           int
}

func (c calibrationTrial) run(ctx context.Context, conn *udp.UDPConn, client *kgo.Client, options []kgo.Opt) (sent, received, datagrams int, elapsed time.Duration, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	startOffsets, err := calibrationOffsets(ctx, client, c.topic, c.partitions, -1)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	consumeOffsets := make(map[int32]kgo.Offset, len(c.partitions))
	partitionIndices := make(map[int32]int, len(c.partitions))
	prefixes := make([]string, len(c.partitions))
	for i, partition := range c.partitions {
		consumeOffsets[partition] = kgo.NewOffset().At(startOffsets[partition])
		partitionIndices[partition] = i
		prefixes[i] = fmt.Sprintf("%s_%d_", c.topic, partition)
	}
	consumeOptions := append([]kgo.Opt(nil), options...)
	consumeOptions = append(consumeOptions,
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{c.topic: consumeOffsets}),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
	)
	consumer, err := kgo.NewClient(consumeOptions...)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer consumer.Close()

	readCtx, stop := context.WithCancel(ctx)
	var readers sync.WaitGroup
	defer func() { stop(); readers.Wait() }()
	failures := make(chan error, 1)
	fail := func(err error) {
		select {
		case failures <- err:
			stop()
		default:
		}
	}
	var payloadTemplate [1600]byte
	if _, err := rand.Read(payloadTemplate[:]); err != nil {
		return 0, 0, 0, 0, err
	}
	base := 1_000_000_000_000_000 + binary.BigEndian.Uint64(payloadTemplate[:8])%8_000_000_000_000_000
	seen := make([]bool, c.rate*int(c.duration/time.Second+1)+c.burst+1)
	var mu sync.Mutex
	verified := 0
	highest := -1
	readers.Add(1)
	go func() {
		defer readers.Done()
		for readCtx.Err() == nil {
			fetches := consumer.PollRecords(readCtx, 256)
			for _, failure := range fetches.Errors() {
				if readCtx.Err() == nil {
					fail(failure.Err)
				}
			}
			fetches.EachRecord(func(record *kgo.Record) {
				index, ok := partitionIndices[record.Partition]
				if !ok {
					fail(fmt.Errorf("unexpected partition %d in verification topic %q", record.Partition, c.topic))
					return
				}
				offsetText, ok := strings.CutPrefix(string(record.Key), prefixes[index])
				offset, parseErr := strconv.ParseUint(offsetText, 10, 64)
				if !ok || parseErr != nil || offset < base || offset-base >= uint64(len(seen)) {
					fail(fmt.Errorf("unexpected record in verification topic %q; keep it isolated", c.topic))
					return
				}
				seq := int(offset - base)
				if seq%len(c.partitions) != index || len(record.Value) != 400+seq%1201 ||
					binary.BigEndian.Uint64(record.Value[:8]) != offset || !bytes.Equal(record.Value[8:], payloadTemplate[8:len(record.Value)]) {
					fail(fmt.Errorf("corrupt calibration record %d", seq))
					return
				}
				mu.Lock()
				if seen[seq] {
					mu.Unlock()
					fail(fmt.Errorf("duplicate calibration record %d", seq))
					return
				}
				seen[seq] = true
				verified++
				highest = max(highest, seq)
				mu.Unlock()
			})
		}
	}()

	if err := conn.ConfigurePacing(readCtx, c.rate, c.rate*calibrationFrameSize, c.burst); err != nil {
		return 0, 0, 0, 0, err
	}
	var payload = payloadTemplate
	start := time.Now()
	for readCtx.Err() == nil && time.Since(start) < c.duration && (c.limit == 0 || sent < c.limit) {
		id := prefixes[sent%len(c.partitions)] + strconv.FormatUint(base+uint64(sent), 10)
		binary.BigEndian.PutUint64(payload[:8], base+uint64(sent))
		frames, formatErr := protocol.FormatMessageChecked(protocol.TYPE_CLEARTEXT, id, payload[:400+sent%1201], calibrationFrameSize)
		if formatErr != nil {
			err = formatErr
			break
		}
		if err = conn.SendMessages(frames); err != nil {
			break
		}
		sent++
		datagrams += len(frames)
	}
	elapsed = time.Since(start)
	if err == nil && readCtx.Err() == nil {
		timer := time.NewTimer(c.drain)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-readCtx.Done():
		}
	}
	stop()
	readers.Wait()
	received = verified
	select {
	case failure := <-failures:
		err = failure
	default:
		if err == nil {
			err = ctx.Err()
		}
	}
	if err == nil && highest >= sent {
		err = fmt.Errorf("received calibration records that were not sent")
	}
	return
}

func calibrationTopicPartitions(ctx context.Context, client *kgo.Client, topic string) ([]int32, error) {
	req := kmsg.NewPtrMetadataRequest()
	req.Topics = []kmsg.MetadataRequestTopic{{Topic: &topic}}
	resp, err := req.RequestWith(ctx, client)
	if err != nil {
		return nil, err
	}
	for _, item := range resp.Topics {
		if item.Topic != nil && *item.Topic == topic {
			if item.ErrorCode != 0 {
				return nil, kerr.TypedErrorForCode(item.ErrorCode)
			}
			partitions := make([]int32, 0, len(item.Partitions))
			for _, partition := range item.Partitions {
				if partition.ErrorCode != 0 {
					return nil, kerr.TypedErrorForCode(partition.ErrorCode)
				}
				partitions = append(partitions, partition.Partition)
			}
			return partitions, nil
		}
	}
	return nil, fmt.Errorf("verification topic %q not found", topic)
}

func calibrationOffsets(ctx context.Context, client *kgo.Client, topic string, partitions []int32, timestamp int64) (map[int32]int64, error) {
	req := kmsg.NewPtrListOffsetsRequest()
	req.ReplicaID = -1
	entry := kmsg.NewListOffsetsRequestTopic()
	entry.Topic = topic
	for _, partition := range partitions {
		requestPartition := kmsg.NewListOffsetsRequestTopicPartition()
		requestPartition.Partition = partition
		requestPartition.Timestamp = timestamp
		entry.Partitions = append(entry.Partitions, requestPartition)
	}
	req.Topics = []kmsg.ListOffsetsRequestTopic{entry}
	resp, err := req.RequestWith(ctx, client)
	if err != nil {
		return nil, err
	}
	offsets := make(map[int32]int64, len(partitions))
	for _, item := range resp.Topics {
		if item.Topic != topic {
			continue
		}
		for _, partition := range item.Partitions {
			if partition.ErrorCode != 0 {
				return nil, kerr.TypedErrorForCode(partition.ErrorCode)
			}
			offsets[partition.Partition] = partition.Offset
		}
	}
	if len(offsets) != len(partitions) {
		return nil, fmt.Errorf("Kafka returned offsets for %d of %d verification partitions", len(offsets), len(partitions))
	}
	return offsets, nil
}
