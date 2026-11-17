package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/protocol"
)

const (
	udpBatchMTU          = 1500
	udpBatchMaxRecords   = 50_000_000
	udpBatchSourceTopic  = "stress"
	udpBatchTargetTopic  = "transfer"
	udpBatchPartitions   = 10
	udpBatchDrainTimeout = 30 * time.Second
)

type udpBatchOptions struct {
	address, brokers, topic, targetTopic string
	rate, durationSecs, cycles, size     int
	idle, maxSparseLatency, drainTimeout time.Duration
	seed                                 int64
	probeSent                            []atomic.Int64
}

type udpBatchSummary struct {
	Type               string  `json:"type"`
	Sent               uint64  `json:"sent"`
	Expected           uint64  `json:"expected"`
	Delivered          uint64  `json:"delivered"`
	SparseProbes       uint64  `json:"sparse_probes"`
	MaxSparseLatencyMS float64 `json:"max_sparse_latency_ms"`
	ElapsedSeconds     float64 `json:"elapsed_seconds"`
	Errors             string  `json:"errors,omitempty"`
}

func udpBatch(args []string) {
	flags := flag.NewFlagSet("udp-batch", flag.ExitOnError)
	address := flags.String("address", "downstream:1234", "downstream UDP address")
	brokers := flags.String("brokers", "target-kafka:9092", "comma-separated target Kafka brokers")
	topic := flags.String("topic", udpBatchSourceTopic, "source topic encoded in protocol message IDs")
	targetTopic := flags.String("target-topic", udpBatchTargetTopic, "translated topic consumed for verification")
	rate := flags.Int("rate", 10_000, "datagrams per second during each burst")
	duration := flags.Int("duration", 2, "whole seconds per burst")
	idle := flags.Duration("idle", time.Second, "idle interval between bursts")
	cycles := flags.Int("cycles", 3, "number of burst-and-sparse-probe cycles")
	size := flags.Int("size", 600, "payload bytes per datagram")
	maxSparseLatency := flags.Duration("max-sparse-latency", 2*time.Second, "maximum probe send-to-Kafka latency")
	drainTimeout := flags.Duration("drain-timeout", udpBatchDrainTimeout, "maximum wait for all expected Kafka records")
	summaryPath := flags.String("summary", "", "optional JSON summary file")
	_ = flags.Parse(args)

	o := udpBatchOptions{
		address: *address, brokers: *brokers, topic: *topic, targetTopic: *targetTopic,
		rate: *rate, durationSecs: *duration, cycles: *cycles, size: *size,
		idle: *idle, maxSparseLatency: *maxSparseLatency, drainTimeout: *drainTimeout,
	}
	expected, err := validateUDPBatchOptions(o)
	if err != nil {
		fatalf("udp-batch: %v", err)
	}
	result, err := runUDPBatch(o, expected, os.Stdout)
	if err != nil {
		result.Errors = err.Error()
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		fatalf("marshal UDP batch summary: %v", marshalErr)
	}
	if *summaryPath != "" {
		if writeErr := os.WriteFile(*summaryPath, append(data, '\n'), 0644); writeErr != nil {
			fatalf("write UDP batch summary: %v", writeErr)
		}
	}
	fmt.Println(string(data))
	if err != nil {
		fatalf("udp-batch: %v", err)
	}
}

func validateUDPBatchOptions(o udpBatchOptions) (uint64, error) {
	if o.address == "" || o.brokers == "" || o.topic == "" || o.targetTopic == "" {
		return 0, errors.New("address, brokers, topic, and target-topic are required")
	}
	if o.rate < 1 || o.durationSecs < 1 || o.cycles < 1 ||
		o.size < 24 || o.size > 1400 || o.idle < 0 ||
		o.maxSparseLatency <= 0 || o.drainTimeout <= 0 {
		return 0, errors.New("invalid rate, duration, cycles, payload size, idle, or timeout")
	}
	if uint64(o.rate) > udpBatchMaxRecords/uint64(o.durationSecs) {
		return 0, fmt.Errorf("records per burst exceed the bounded maximum of %d", udpBatchMaxRecords)
	}
	perBurst := uint64(o.rate) * uint64(o.durationSecs)
	cycles := uint64(o.cycles)
	if cycles > udpBatchMaxRecords || perBurst > (udpBatchMaxRecords-cycles)/cycles {
		return 0, fmt.Errorf("expected records exceed the bounded maximum of %d", udpBatchMaxRecords)
	}
	expected := perBurst*cycles + cycles
	if perBurst == 0 || expected > udpBatchMaxRecords {
		return 0, fmt.Errorf("expected records %d exceed the bounded maximum of %d", expected, udpBatchMaxRecords)
	}
	if o.topic != udpBatchSourceTopic || o.targetTopic != udpBatchTargetTopic {
		return 0, fmt.Errorf("this fixture requires topic mapping %q -> %q", udpBatchSourceTopic, udpBatchTargetTopic)
	}
	if _, err := protocol.FormatMessageChecked(protocol.TYPE_CLEARTEXT, "stress_0_0",
		make([]byte, o.size), udpBatchMTU); err != nil {
		return 0, fmt.Errorf("payload does not fit in one UDP datagram: %w", err)
	}
	return expected, nil
}

func validateUDPBatchRecord(id string, payload []byte, partitions int, minSize, maxSize int, seed int64) (uint64, int64, error) {
	if partitions < 1 {
		return 0, 0, errors.New("partition count must be positive")
	}
	topic, partitionText, offsetText, err := protocol.ParseMessageId(id)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid protocol ID %q: %w", id, err)
	}
	if topic != udpBatchSourceTopic {
		return 0, 0, fmt.Errorf("unexpected source topic %q", topic)
	}
	partition, err := strconv.Atoi(partitionText)
	if err != nil || partition < 0 || partition >= partitions {
		return 0, 0, fmt.Errorf("invalid source partition %q", partitionText)
	}
	offset, err := strconv.ParseUint(offsetText, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid source offset %q", offsetText)
	}
	payloadSeq, sent, err := validateStressPayload(payload, minSize, maxSize)
	if err != nil {
		return 0, 0, err
	}
	if int(payloadSeq%uint64(partitions)) != partition || payloadSeq/uint64(partitions) != offset {
		return 0, 0, fmt.Errorf("protocol ID %q does not match payload sequence %d", id, payloadSeq)
	}
	if sent != seed+int64(payloadSeq) {
		return 0, 0, fmt.Errorf("sequence %d timestamp identity changed", payloadSeq)
	}
	if !bytes.Equal(payload, stressPayload(payloadSeq, sent, minSize, maxSize)) {
		return 0, 0, fmt.Errorf("sequence %d payload differs from the generated bytes", payloadSeq)
	}
	return payloadSeq, sent, nil
}

func udpBatchBitSet(seen []atomic.Uint64, seq uint64) bool {
	word := &seen[seq/64]
	mask := uint64(1) << (seq % 64)
	for {
		current := word.Load()
		if current&mask != 0 {
			return false
		}
		if word.CompareAndSwap(current, current|mask) {
			return true
		}
	}
}

func runUDPBatch(o udpBatchOptions, expected uint64, out io.Writer) (result udpBatchSummary, retErr error) {
	result.Type = "summary"
	result.Expected = expected
	started := time.Now()
	o.seed = started.UnixNano()
	o.probeSent = make([]atomic.Int64, o.cycles)
	defer func() { result.ElapsedSeconds = time.Since(started).Seconds() }()

	config := sarama.NewConfig()
	config.Version = sarama.V3_6_0_0
	config.ClientID = "tc53-udp-verifier"
	config.Consumer.Return.Errors = true
	config.Consumer.Fetch.Default = 1024 * 1024
	config.Net.ReadTimeout = 10 * time.Second
	client, err := sarama.NewClient(strings.Split(o.brokers, ","), config)
	if err != nil {
		return result, fmt.Errorf("create target Kafka client: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, client.Close()) }()
	consumer, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		return result, fmt.Errorf("create target Kafka consumer: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, consumer.Close()) }()

	partitions, err := consumer.Partitions(o.targetTopic)
	if err != nil {
		return result, fmt.Errorf("list target topic partitions: %w", err)
	}
	if len(partitions) != udpBatchPartitions {
		return result, fmt.Errorf("target topic has %d partitions, want %d", len(partitions), udpBatchPartitions)
	}

	seen := make([]atomic.Uint64, (expected+63)/64)
	probeSequences := make(map[uint64]struct{}, o.cycles)
	perBurst := uint64(o.rate) * uint64(o.durationSecs)
	for cycle := range o.cycles {
		probeSequences[uint64(cycle)*(perBurst+1)+perBurst] = struct{}{}
	}
	var delivered, probes, maxProbeLatencyNS atomic.Uint64
	ctx, cancel := context.WithCancel(context.Background())
	var receivers sync.WaitGroup
	failures := make(chan error, 1)
	fail := func(err error) {
		select {
		case failures <- err:
			cancel()
		default:
		}
	}
	for _, partition := range partitions {
		pc, consumeErr := consumer.ConsumePartition(o.targetTopic, partition, sarama.OffsetOldest)
		if consumeErr != nil {
			cancel()
			receivers.Wait()
			return result, fmt.Errorf("consume target partition %d: %w", partition, consumeErr)
		}
		receivers.Add(1)
		go func(partition int32, pc sarama.PartitionConsumer) {
			defer receivers.Done()
			defer func() {
				if closeErr := pc.Close(); closeErr != nil {
					fail(fmt.Errorf("close target partition consumer %d: %w", partition, closeErr))
				}
			}()
			messages, errorsCh := pc.Messages(), pc.Errors()
			for messages != nil || errorsCh != nil {
				select {
				case msg, ok := <-messages:
					if !ok {
						messages = nil
						continue
					}
					seq, _, verifyErr := validateUDPBatchRecord(string(msg.Key), msg.Value, len(partitions), o.size, o.size, o.seed)
					if verifyErr != nil {
						fail(fmt.Errorf("validate target record: %w", verifyErr))
						return
					}
					if seq >= expected {
						fail(fmt.Errorf("unexpected sequence %d; expected range [0,%d)", seq, expected))
						return
					}
					if int32(seq%uint64(len(partitions))) != partition {
						fail(fmt.Errorf("sequence %d delivered to target partition %d", seq, partition))
						return
					}
					if !udpBatchBitSet(seen, seq) {
						fail(fmt.Errorf("duplicate sequence %d", seq))
						return
					}
					if _, isProbe := probeSequences[seq]; isProbe {
						sent := o.probeSent[seq/(perBurst+1)].Load()
						latency := time.Since(time.Unix(0, sent))
						if latency < 0 {
							fail(fmt.Errorf("probe sequence %d has a future timestamp", seq))
							return
						}
						for {
							current := maxProbeLatencyNS.Load()
							if uint64(latency) <= current || maxProbeLatencyNS.CompareAndSwap(current, uint64(latency)) {
								break
							}
						}
						probes.Add(1)
					}
					delivered.Add(1)
				case consumeErr, ok := <-errorsCh:
					if !ok {
						errorsCh = nil
					} else {
						fail(fmt.Errorf("consume target partition %d: %w", partition, consumeErr))
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}(partition, pc)
	}

	udpAddress, err := net.ResolveUDPAddr("udp", o.address)
	if err != nil {
		cancel()
		receivers.Wait()
		return result, fmt.Errorf("resolve downstream UDP address: %w", err)
	}
	conn, err := net.DialUDP("udp", nil, udpAddress)
	if err != nil {
		cancel()
		receivers.Wait()
		return result, fmt.Errorf("open UDP sender socket: %w", err)
	}
	sent, sendErr := sendUDPBatchWorkload(conn, o, perBurst)
	closeErr := conn.Close()
	result.Sent = sent
	retErr = errors.Join(sendErr, closeErr)
	if retErr == nil {
		deadline := time.NewTimer(o.drainTimeout)
		ticker := time.NewTicker(10 * time.Millisecond)
		for delivered.Load() < expected {
			select {
			case err := <-failures:
				retErr = err
			case <-deadline.C:
				retErr = fmt.Errorf("Kafka delivery timeout: delivered=%d expected=%d", delivered.Load(), expected)
			case <-ticker.C:
			}
			if retErr != nil {
				break
			}
		}
		deadline.Stop()
		ticker.Stop()
	}
	if retErr == nil {
		settle := time.NewTimer(time.Second)
		select {
		case err := <-failures:
			retErr = err
		case <-settle.C:
		}
		settle.Stop()
	}
	cancel()
	receivers.Wait()
	select {
	case err := <-failures:
		retErr = errors.Join(retErr, err)
	default:
	}
	if retErr == nil {
		retErr = verifyUDPBatchTopicOffsets(client, o.targetTopic, expected, len(partitions))
	}
	result.Delivered = delivered.Load()
	result.SparseProbes = probes.Load()
	result.MaxSparseLatencyMS = float64(maxProbeLatencyNS.Load()) / float64(time.Millisecond)
	if retErr == nil && result.SparseProbes != uint64(o.cycles) {
		retErr = fmt.Errorf("sparse probes delivered=%d expected=%d", result.SparseProbes, o.cycles)
	}
	if retErr == nil && maxProbeLatencyNS.Load() > uint64(o.maxSparseLatency) {
		retErr = fmt.Errorf("sparse probe Kafka latency %.3fs exceeds %s",
			time.Duration(maxProbeLatencyNS.Load()).Seconds(), o.maxSparseLatency)
	}
	if retErr == nil && result.Delivered != expected {
		retErr = fmt.Errorf("delivered=%d expected=%d", result.Delivered, expected)
	}
	if retErr == nil && result.Sent != expected {
		retErr = fmt.Errorf("sent=%d expected=%d", result.Sent, expected)
	}
	if retErr == nil {
		if _, writeErr := fmt.Fprintf(out, "udp-batch-ok sent=%d delivered=%d sparse_probes=%d max_sparse_latency=%.3fms\n",
			result.Sent, result.Delivered, result.SparseProbes, result.MaxSparseLatencyMS); writeErr != nil {
			retErr = writeErr
		}
	}
	return result, retErr
}

func verifyUDPBatchTopicOffsets(client sarama.Client, topic string, expected uint64, partitions int) error {
	for partition := range partitions {
		highWatermark, err := client.GetOffset(topic, int32(partition), sarama.OffsetNewest)
		if err != nil {
			return fmt.Errorf("read target partition %d high-water mark: %w", partition, err)
		}
		want := expected / uint64(partitions)
		if uint64(partition) < expected%uint64(partitions) {
			want++
		}
		if highWatermark < 0 || uint64(highWatermark) != want {
			return fmt.Errorf("target partition %d records=%d, want exactly %d",
				partition, highWatermark, want)
		}
	}
	return nil
}

func sendUDPBatchWorkload(conn *net.UDPConn, o udpBatchOptions, perBurst uint64) (uint64, error) {
	var sent uint64
	for cycle := range o.cycles {
		burstStart := time.Now()
		chunkSize := max(1, o.rate/1000)
		for base := 0; base < int(perBurst); base += chunkSize {
			due := burstStart.Add(time.Duration(base) * time.Second / time.Duration(o.rate))
			if wait := time.Until(due); wait > 0 {
				time.Sleep(wait)
			}
			end := min(base+chunkSize, int(perBurst))
			for index := base; index < end; index++ {
				seq := uint64(cycle)*(perBurst+1) + uint64(index)
				if err := sendUDPBatchRecord(conn, o, seq); err != nil {
					return sent, fmt.Errorf("send sequence %d: %w", seq, err)
				}
				sent++
			}
		}
		burstDuration := time.Since(burstStart)
		plannedDuration := time.Duration(o.durationSecs) * time.Second
		fmt.Printf("udp-burst cycle=%d datagrams=%d elapsed=%.6fs actual_eps=%.0f\n",
			cycle+1, perBurst, burstDuration.Seconds(), float64(perBurst)/burstDuration.Seconds())
		if burstDuration > plannedDuration+max(100*time.Millisecond, plannedDuration/10) {
			return sent, fmt.Errorf("cycle %d sender could not sustain requested rate %d: elapsed=%s planned=%s",
				cycle+1, o.rate, burstDuration, plannedDuration)
		}
		if wait := time.Until(burstStart.Add(plannedDuration)); wait > 0 {
			time.Sleep(wait)
		}
		time.Sleep(max(o.idle, 100*time.Millisecond))
		probeSeq := uint64(cycle)*(perBurst+1) + perBurst
		o.probeSent[cycle].Store(time.Now().UnixNano())
		if err := sendUDPBatchRecord(conn, o, probeSeq); err != nil {
			return sent, fmt.Errorf("send sparse probe %d: %w", probeSeq, err)
		}
		sent++
		time.Sleep(o.maxSparseLatency + 100*time.Millisecond)
	}
	return sent, nil
}

func sendUDPBatchRecord(conn *net.UDPConn, o udpBatchOptions, seq uint64) error {
	sentAt := o.seed + int64(seq)
	payload := stressPayload(seq, sentAt, o.size, o.size)
	id := fmt.Sprintf("%s_%d_%d", o.topic, seq%udpBatchPartitions, seq/udpBatchPartitions)
	frames, err := protocol.FormatMessageChecked(protocol.TYPE_CLEARTEXT, id, payload, udpBatchMTU)
	if err != nil {
		return err
	}
	if len(frames) != 1 {
		return fmt.Errorf("sequence %d encoded into %d datagrams, want one", seq, len(frames))
	}
	n, err := conn.Write(frames[0])
	if err != nil {
		return err
	}
	if n != len(frames[0]) {
		return fmt.Errorf("short UDP write: %d of %d bytes", n, len(frames[0]))
	}
	return nil
}
