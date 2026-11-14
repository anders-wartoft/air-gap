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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
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
	MissingSample      []uint64 `json:"missing_sequence_sample,omitempty"`
	Errors             string   `json:"errors,omitempty"`
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
	conf := sarama.NewConfig()
	conf.Version = sarama.V3_6_0_0
	conf.ClientID = "tc52-driver"
	conf.Consumer.Return.Errors = true
	conf.Consumer.IsolationLevel = sarama.ReadCommitted
	conf.Consumer.Fetch.Default = 1024 * 1024
	conf.Producer.Partitioner = sarama.NewManualPartitioner
	conf.Producer.RequiredAcks = sarama.WaitForAll
	conf.Producer.Return.Successes = true
	conf.Producer.Flush.Frequency = time.Millisecond
	conf.Producer.Flush.Messages = 1000
	conf.Net.ReadTimeout = 10 * time.Second
	conf.Net.WriteTimeout = 10 * time.Second
	sourceClient, err := sarama.NewClient(strings.Split(o.source, ","), conf)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, sourceClient.Close()) }()
	admin, err := sarama.NewClusterAdminFromClient(sourceClient)
	if err != nil {
		return result, err
	}
	targetClient, err := sarama.NewClient(strings.Split(o.target, ","), conf)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, targetClient.Close()) }()
	consumer, err := sarama.NewConsumerFromClient(targetClient)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, consumer.Close()) }()
	producer, err := sarama.NewAsyncProducerFromClient(sourceClient)
	if err != nil {
		return result, err
	}
	producerStarted := false
	defer func() {
		if !producerStarted {
			retErr = errors.Join(retErr, producer.Close())
		}
	}()

	window := newStressWindow(o.window)
	sendCtx, stopSending := context.WithCancel(context.Background())
	defer stopSending()
	receiveCtx, stopReceiving := context.WithCancel(context.Background())
	var receivers sync.WaitGroup
	defer func() { stopReceiving(); receivers.Wait() }()
	var acknowledged, delivered, measured, measuredBytes, latencyNS atomic.Uint64
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
	for p := range o.partitions {
		partitionIDs[p] = int32(p)
		pc, err := consumer.ConsumePartition("transfer", int32(p), sarama.OffsetOldest)
		if err != nil {
			return result, err
		}
		receivers.Add(1)
		go func(p int, pc sarama.PartitionConsumer) {
			defer receivers.Done()
			defer func() {
				if err := pc.Close(); err != nil {
					fail(err)
				}
			}()
			messages, errs := pc.Messages(), pc.Errors()
			for messages != nil || errs != nil {
				select {
				case msg, ok := <-messages:
					if !ok {
						messages = nil
						continue
					}
					seq, sent, err := validateStressPayload(msg.Value, o.minSize, o.maxSize)
					if err == nil && int(seq%uint64(o.partitions)) != p {
						err = fmt.Errorf("sequence %d delivered to wrong partition %d", seq, p)
					}
					if err == nil {
						err = window.complete(seq, sent)
					}
					if err != nil {
						fail(err)
						return
					}
					offsets[p].Store(msg.Offset + 1)
					delivered.Add(1)
					now := time.Now().UnixNano()
					if from := measureFrom.Load(); from > 0 && now >= from && now < measureUntil.Load() {
						measured.Add(1)
						measuredBytes.Add(uint64(len(msg.Value)))
						latencyNS.Add(uint64(time.Since(time.Unix(0, sent))))
					}
				case err, ok := <-errs:
					if !ok {
						errs = nil
					} else {
						fail(err)
						return
					}
				case <-receiveCtx.Done():
					return
				}
			}
		}(p, pc)
	}
	senderDone, producerDone := make(chan struct{}), make(chan struct{})
	producerStarted = true
	go func() {
		defer close(senderDone)
		defer producer.AsyncClose()
		for seq := uint64(0); ; seq++ {
			sent := time.Now().UnixNano()
			if !window.reserve(sendCtx, seq, sent) {
				return
			}
			msg := &sarama.ProducerMessage{
				Topic: "stress", Partition: int32(seq % uint64(o.partitions)),
				Value:    sarama.ByteEncoder(stressPayload(seq, sent, o.minSize, o.maxSize)),
				Metadata: seq,
			}
			select {
			case producer.Input() <- msg:
			case <-sendCtx.Done():
				window.abandon(seq)
				return
			}
		}
	}()
	go func() {
		defer close(producerDone)
		successes, errs := producer.Successes(), producer.Errors()
		for successes != nil || errs != nil {
			select {
			case _, ok := <-successes:
				if !ok {
					successes = nil
				} else {
					acknowledged.Add(1)
				}
			case err, ok := <-errs:
				if !ok {
					errs = nil
				} else {
					fail(err)
				}
			}
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
			response, err := admin.ListConsumerGroupOffsets(o.group, map[string][]int32{"stress": partitionIDs})
			if err != nil {
				retErr = err
				waiting = false
				continue
			}
			sourceLag, targetLag = 0, 0
			for p := range o.partitions {
				block := response.GetBlock("stress", int32(p))
				if block == nil || block.Err != sarama.ErrNoError {
					retErr = fmt.Errorf("source group offsets unavailable for partition %d", p)
					waiting = false
					break
				}
				sourceEnd, err := sourceClient.GetOffset("stress", int32(p), sarama.OffsetNewest)
				if err != nil {
					retErr = err
					waiting = false
					break
				}
				targetEnd, err := targetClient.GetOffset("transfer", int32(p), sarama.OffsetNewest)
				if err != nil {
					retErr = err
					waiting = false
					break
				}
				// A fresh group has offset -1 until its first successful commit.
				sourceLag += max(0, sourceEnd-max(0, block.Offset))
				targetLag += max(0, targetEnd-offsets[p].Load())
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
