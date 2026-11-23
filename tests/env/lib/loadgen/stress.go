package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

var stressMagic = []byte("AGSTRESS")

type stressWindow struct {
	slots   chan struct{}
	mu      sync.Mutex
	pending map[uint64]int64
	expired map[uint64]int64
}

func newStressWindow(size int) *stressWindow {
	return &stressWindow{
		slots: make(chan struct{}, size), pending: make(map[uint64]int64), expired: make(map[uint64]int64),
	}
}

func (w *stressWindow) reserve(ctx context.Context, seq uint64, sent int64) bool {
	select {
	case w.slots <- struct{}{}:
		w.mu.Lock()
		w.pending[seq] = sent
		w.mu.Unlock()
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *stressWindow) complete(seq uint64, sent int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	expected, ok := w.pending[seq]
	if !ok {
		if expected, ok := w.expired[seq]; ok {
			if sent != expected {
				return fmt.Errorf("sequence %d timestamp changed", seq)
			}
			delete(w.expired, seq)
			return nil
		}
		return fmt.Errorf("duplicate or unexpected sequence %d", seq)
	}
	if sent != expected {
		return fmt.Errorf("sequence %d timestamp changed", seq)
	}
	delete(w.pending, seq)
	<-w.slots
	return nil
}

func (w *stressWindow) abandon(seq uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[seq]; ok {
		delete(w.pending, seq)
		<-w.slots
	}
}

func (w *stressWindow) size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) + len(w.expired)
}

func (w *stressWindow) counts() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending), len(w.expired)
}

// Keep a lossy baseline under load without forgetting or hiding missing records.
func (w *stressWindow) expire(now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for seq, sent := range w.pending {
		if now.Sub(time.Unix(0, sent)) >= 5*time.Second {
			if len(w.expired) >= 100000 {
				return errors.New("100,000 records overdue; aborting excessive-loss workload")
			}
			w.expired[seq] = sent
			delete(w.pending, seq)
			<-w.slots
		}
	}
	return nil
}

func (w *stressWindow) missingSample() []uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]uint64, 0, 20)
	for _, entries := range []map[uint64]int64{w.pending, w.expired} {
		for seq := range entries {
			if len(result) < 20 {
				result = append(result, seq)
			}
		}
	}
	return result
}

func stressPayload(seq uint64, sent int64, minSize, maxSize int) []byte {
	payload := bytes.Repeat([]byte{byte(seq % 251)}, minSize+int(seq%uint64(maxSize-minSize+1)))
	copy(payload, stressMagic)
	binary.BigEndian.PutUint64(payload[8:16], seq)
	binary.BigEndian.PutUint64(payload[16:24], uint64(sent))
	return payload
}

func validateStressPayload(payload []byte, minSize, maxSize int) (uint64, int64, error) {
	if len(payload) < 24 || !bytes.Equal(payload[:8], stressMagic) {
		return 0, 0, errors.New("missing stress payload header")
	}
	seq := binary.BigEndian.Uint64(payload[8:16])
	sent := int64(binary.BigEndian.Uint64(payload[16:24]))
	expectedSize := minSize + int(seq%uint64(maxSize-minSize+1))
	if len(payload) != expectedSize {
		return 0, 0, fmt.Errorf("sequence %d size %d, expected %d", seq, len(payload), expectedSize)
	}
	for _, b := range payload[24:] {
		if b != byte(seq%251) {
			return 0, 0, fmt.Errorf("sequence %d payload corrupted", seq)
		}
	}
	return seq, sent, nil
}

type stressOptions struct {
	source, target, group        string
	partitions, minSize, maxSize int
	window                       int
	duration, warmup, drain      time.Duration
}

type stressSummary struct {
	Type               string   `json:"type"`
	MeasurementSecs    float64  `json:"measurement_seconds"`
	DeliveredEPS       float64  `json:"delivered_eps"`
	DeliveredMiBPS     float64  `json:"delivered_mib_per_second"`
	MeasuredRecords    uint64   `json:"measured_records"`
	SourceAcknowledged uint64   `json:"source_acknowledged"`
	TotalDelivered     uint64   `json:"total_delivered"`
	Outstanding        int      `json:"outstanding"`
	MeanLatencyMS      float64  `json:"mean_latency_ms"`
	P99LatencyMS       *int     `json:"p99_latency_ms,omitempty"`
	MissingSample      []uint64 `json:"missing_sequence_sample,omitempty"`
	Errors             string   `json:"errors,omitempty"`
}

// Millisecond upper bounds, with the final bucket reserved for overflow.
type stressLatencyHistogram [60002]atomic.Uint64

func (h *stressLatencyHistogram) record(latency time.Duration) {
	ms := (max(0, latency) + time.Millisecond - 1) / time.Millisecond
	h[min(int(ms), len(h)-1)].Add(1)
}

func (h *stressLatencyHistogram) p99(count uint64) *int {
	if count == 0 {
		return nil
	}
	rank := count - count/100
	var cumulative uint64
	for ms := 0; ms < len(h)-1; ms++ {
		cumulative += h[ms].Load()
		if cumulative >= rank {
			return &ms
		}
	}
	return nil
}

func stress(args []string) {
	flags := flag.NewFlagSet("stress", flag.ExitOnError)
	source := flags.String("source-brokers", "source-kafka:9092", "source Kafka brokers")
	target := flags.String("target-brokers", "target-kafka:9092", "downstream Kafka brokers")
	group := flags.String("source-group", "tc52-now", "upstream Kafka consumer group")
	partitions := flags.Int("partitions", 10, "partition count on both topics")
	minSize := flags.Int("min-size", 400, "smallest payload")
	maxSize := flags.Int("max-size", 1600, "largest payload")
	window := flags.Int("window", 10000, "maximum outstanding records; bounds benchmark backlog")
	duration := flags.Duration("duration", 5*time.Minute, "measured interval after warmup")
	warmup := flags.Duration("warmup", 20*time.Second, "unmeasured warmup")
	drain := flags.Duration("drain", 15*time.Second, "maximum final drain")
	summaryPath := flags.String("summary", "", "optional JSON summary file")
	_ = flags.Parse(args)
	if *partitions < 1 || *minSize < 24 || *maxSize < *minSize || *maxSize > 65535 ||
		*window < 1 || *duration <= 0 || *warmup < 0 || *drain <= 0 {
		fatalf("invalid stress parameters")
	}
	result, err := runStress(stressOptions{
		source: *source, target: *target, group: *group,
		partitions: *partitions, minSize: *minSize, maxSize: *maxSize,
		window: *window, duration: *duration, warmup: *warmup, drain: *drain,
	}, os.Stdout)
	if err != nil {
		result.Errors = err.Error()
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		fatalf("marshal stress summary: %v", marshalErr)
	}
	if *summaryPath != "" {
		if err := os.WriteFile(*summaryPath, append(data, '\n'), 0644); err != nil {
			fatalf("write stress summary: %v", err)
		}
	}
	fmt.Println(string(data))
	if err != nil {
		fatalf("stress: %v", err)
	}
}

func runStress(o stressOptions, out io.Writer) (result stressSummary, retErr error) {
	result.Type = "summary"
	sourceClient, err := newKafkaClient(o.source, "tc52-driver",
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(time.Millisecond),
		kgo.MaxBufferedRecords(o.window),
	)
	if err != nil {
		return result, err
	}
	defer sourceClient.Close()
	targetClient, err := newKafkaClient(o.target, "tc52-target-consumer",
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxPartitionBytes(1024*1024),
	)
	if err != nil {
		return result, err
	}
	defer targetClient.Close()

	window := newStressWindow(o.window)
	sendCtx, stopSending := context.WithCancel(context.Background())
	defer stopSending()
	receiveCtx, stopReceiving := context.WithCancel(context.Background())
	var receivers sync.WaitGroup
	defer func() { stopReceiving(); receivers.Wait() }()
	var acknowledged, delivered, measured, measuredBytes, latencyNS atomic.Uint64
	var latencyHistogram stressLatencyHistogram
	var measureFrom, measureUntil atomic.Int64
	failure := make(chan error, 1)
	fail := func(err error) {
		select {
		case failure <- err:
		default:
		}
		stopSending()
	}
	offsets := make([]atomic.Int64, o.partitions)
	partitionIDs := make([]int32, o.partitions)
	consumeOffsets := make(map[int32]kgo.Offset, o.partitions)
	for p := range o.partitions {
		partitionIDs[p] = int32(p)
		consumeOffsets[int32(p)] = kgo.NewOffset().AtStart()
	}
	targetClient.AddConsumePartitions(map[string]map[int32]kgo.Offset{"transfer": consumeOffsets})
	receivers.Add(1)
	go func() {
		defer receivers.Done()
		for receiveCtx.Err() == nil {
			fetches := targetClient.PollRecords(receiveCtx, 256)
			for _, fetchErr := range fetches.Errors() {
				if receiveCtx.Err() == nil {
					fail(fmt.Errorf("consume transfer topic: %w", fetchErr.Err))
				}
			}
			fetches.EachRecord(func(record *kgo.Record) {
				partition := int(record.Partition)
				seq, sent, err := validateStressPayload(record.Value, o.minSize, o.maxSize)
				if err == nil && int(seq%uint64(o.partitions)) != partition {
					err = fmt.Errorf("sequence %d delivered to wrong partition %d", seq, partition)
				}
				if err == nil {
					err = window.complete(seq, sent)
				}
				if err != nil {
					fail(err)
					return
				}
				offsets[partition].Store(record.Offset + 1)
				delivered.Add(1)
				now := time.Now().UnixNano()
				if from := measureFrom.Load(); from > 0 && now >= from && now < measureUntil.Load() {
					measured.Add(1)
					measuredBytes.Add(uint64(len(record.Value)))
					latency := time.Duration(now - sent)
					latencyNS.Add(uint64(max(0, latency)))
					latencyHistogram.record(latency)
				}
			})
		}
	}()
	senderDone, producerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(senderDone)
		for seq := uint64(0); ; seq++ {
			sent := time.Now().UnixNano()
			if !window.reserve(sendCtx, seq, sent) {
				return
			}
			recordSeq := seq
			record := &kgo.Record{
				Topic: "stress", Partition: int32(recordSeq % uint64(o.partitions)),
				Value: stressPayload(recordSeq, sent, o.minSize, o.maxSize),
			}
			sourceClient.Produce(context.Background(), record, func(_ *kgo.Record, err error) {
				if err != nil {
					window.abandon(recordSeq)
					fail(fmt.Errorf("produce stress record %d: %w", recordSeq, err))
					return
				}
				acknowledged.Add(1)
			})
		}
	}()
	go func() {
		defer close(producerDone)
		<-senderDone
		flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sourceClient.Flush(flushCtx); err != nil {
			fail(fmt.Errorf("flush stress producer: %w", err))
		}
	}()
	encoder := json.NewEncoder(out)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	phase := "warmup"
	phaseTimer := time.NewTimer(o.warmup)
	defer phaseTimer.Stop()
	var start, end time.Time
	var previousMeasured uint64
	previousSample := time.Now()
	var sourceLag, targetLag int64
	waiting := true
	for waiting {
		select {
		case <-phaseTimer.C:
			if phase == "warmup" {
				phase = "measure"
				start = time.Now()
				measureUntil.Store(start.Add(o.duration).UnixNano())
				measureFrom.Store(start.UnixNano())
				phaseTimer.Reset(o.duration)
			} else {
				end = start.Add(o.duration)
				waiting = false
			}
		case <-tick.C:
			if err := window.expire(time.Now()); err != nil {
				retErr = err
				waiting = false
				continue
			}
			committed, err := kafkaCommittedOffsets(context.Background(), sourceClient, o.group, "stress", partitionIDs)
			if err != nil {
				retErr = err
				waiting = false
				continue
			}
			sourceEnds, err := kafkaOffsets(context.Background(), sourceClient, "stress", partitionIDs, -1)
			if err != nil {
				retErr = err
				waiting = false
				continue
			}
			targetEnds, err := kafkaOffsets(context.Background(), targetClient, "transfer", partitionIDs, -1)
			if err != nil {
				retErr = err
				waiting = false
				continue
			}
			sourceLag, targetLag = 0, 0
			for p := range o.partitions {
				// A fresh group has offset -1 until its first successful commit.
				sourceLag += max(0, sourceEnds[int32(p)]-max(0, committed[int32(p)]))
				targetLag += max(0, targetEnds[int32(p)]-offsets[p].Load())
			}
			current := measured.Load()
			now := time.Now()
			active, overdue := window.counts()
			if err := encoder.Encode(map[string]any{
				"type": "sample", "time": now.UTC(), "phase": phase,
				"source_acknowledged": acknowledged.Load(), "delivered": delivered.Load(),
				"measured_records": current, "interval_records": current - previousMeasured,
				"interval_seconds": now.Sub(previousSample).Seconds(),
				"delivered_eps":    float64(current-previousMeasured) / now.Sub(previousSample).Seconds(),
				"source_kafka_lag": sourceLag, "target_kafka_lag": targetLag,
				"outstanding":        window.size(),
				"active_outstanding": active, "overdue": overdue,
			}); err != nil {
				retErr = err
				waiting = false
			}
			previousMeasured = current
			previousSample = now
		case err := <-failure:
			retErr = err
			waiting = false
		}
	}
	if !start.IsZero() && end.IsZero() {
		end = time.Now()
		if planned := start.Add(o.duration); end.After(planned) {
			end = planned
		}
		measureUntil.Store(end.UnixNano())
	}
	stopSending()
	<-senderDone
	<-producerDone
	drainUntil := time.Now().Add(o.drain)
	for window.size() > 0 && time.Now().Before(drainUntil) {
		select {
		case err := <-failure:
			retErr = errors.Join(retErr, err)
			drainUntil = time.Now()
		case <-tick.C:
		}
	}
	stopReceiving()
	receivers.Wait()
	select {
	case err := <-failure:
		retErr = errors.Join(retErr, err)
	default:
	}
	result.SourceAcknowledged = acknowledged.Load()
	result.TotalDelivered = delivered.Load()
	result.Outstanding = window.size()
	result.MissingSample = window.missingSample()
	result.MeasuredRecords = measured.Load()
	if !start.IsZero() {
		result.MeasurementSecs = end.Sub(start).Seconds()
		result.DeliveredEPS = float64(result.MeasuredRecords) / result.MeasurementSecs
		result.DeliveredMiBPS = float64(measuredBytes.Load()) / (1024 * 1024) / result.MeasurementSecs
	}
	if result.MeasuredRecords > 0 {
		result.MeanLatencyMS = float64(latencyNS.Load()) / float64(result.MeasuredRecords) / 1e6
		result.P99LatencyMS = latencyHistogram.p99(result.MeasuredRecords)
	}
	if result.Outstanding != 0 || result.SourceAcknowledged != result.TotalDelivered {
		retErr = errors.Join(retErr, fmt.Errorf("delivery mismatch: source acknowledged=%d delivered=%d outstanding=%d",
			result.SourceAcknowledged, result.TotalDelivered, result.Outstanding))
	}
	if result.MeasuredRecords == 0 {
		retErr = errors.Join(retErr, errors.New("no records consumed from downstream Kafka during measurement"))
	}
	return result, retErr
}
