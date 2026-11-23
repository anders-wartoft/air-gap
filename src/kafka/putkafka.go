package kafka

import (
	"context"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"sitia.nu/airgap/src/logging"
)

type Producer interface {
	Ping(context.Context) error
	Produce(context.Context, *kgo.Record, func(*kgo.Record, error))
	Flush(context.Context) error
	Close()
}

type MessageCacheEntry struct {
	key       string
	val       []byte
	topic     string
	partition int32
}

type MessageCache struct {
	mu      sync.RWMutex
	entries []MessageCacheEntry
	sending int
	running bool
}

var (
	cache       = MessageCache{entries: make([]MessageCacheEntry, 0)}
	producer    Producer
	status      func(string)
	doneChan    chan struct{}
	wg          sync.WaitGroup
	batchSize   int
	lifecycleMu sync.Mutex
	batchReady  chan struct{}
	cacheSpace  = sync.NewCond(&cache.mu)
)

func SetProducer(p Producer, cfgBatchSize int, updateStatus ...func(string)) {
	if cfgBatchSize <= 0 {
		Logger.Panicf(logging.Event(logging.EventConfigInvalid), "Kafka batch size must be positive: %d", cfgBatchSize)
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	stopBackgroundThread()
	cache.mu.Lock()
	producer = p
	status = nil
	if len(updateStatus) > 0 {
		status = updateStatus[0]
	}
	batchSize = cfgBatchSize
	cache.entries = nil
	cache.mu.Unlock()
	startBackgroundThread()
}

func StartBackgroundThread() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	startBackgroundThread()
}

func startBackgroundThread() {
	cache.mu.Lock()
	if producer == nil || doneChan != nil {
		cache.mu.Unlock()
		return
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	doneChan = make(chan struct{})
	batchReady = make(chan struct{}, 1)
	done, ready := doneChan, batchReady
	cache.running = true
	cache.mu.Unlock()
	wg.Add(1)

	go func() {
		defer wg.Done()
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sendBatch()
			case <-ready:
				sendBatch()
			case <-done:
				sendBatch()
				return
			}
		}
	}()
}

func StopBackgroundThread() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	stopBackgroundThread()
}

func stopBackgroundThread() {
	cache.mu.Lock()
	if doneChan != nil {
		cache.running = false
		close(doneChan)
		cacheSpace.Broadcast()
		cache.mu.Unlock()
		wg.Wait()
		cache.mu.Lock()
		doneChan = nil
		batchReady = nil
		cache.entries = nil
	}
	cache.mu.Unlock()
}

func WriteToKafka(key, topic string, partition int32, val []byte) {
	cache.mu.Lock()
	for cache.running && len(cache.entries) >= batchSize {
		cacheSpace.Wait()
	}
	if !cache.running {
		cache.mu.Unlock()
		Logger.Errorf(logging.Event(logging.EventKafkaProduce,
			logging.Failure(),
			logging.Str("airgap.message.id", key),
			logging.Str("messaging.destination.name", topic),
		), "Kafka dispatcher is stopped; message %s was not queued", key)
		return
	}
	copyVal := make([]byte, len(val))
	copy(copyVal, val)
	cache.entries = append(cache.entries, MessageCacheEntry{
		key:       key,
		val:       copyVal,
		topic:     topic,
		partition: partition,
	})
	if len(cache.entries) == batchSize {
		signalBatch()
	}
	cache.mu.Unlock()
}

func signalBatch() {
	select {
	case batchReady <- struct{}{}:
	default:
	}
}

func sendBatch() {
	cache.mu.Lock()
	n := len(cache.entries)
	if n == 0 {
		cache.mu.Unlock()
		return
	}
	toSend := cache.entries
	cache.entries = nil
	cache.sending += n
	p := producer
	cacheSpace.Broadcast()
	cache.mu.Unlock()

	for _, message := range toSend {
		if p != nil {
			record := &kgo.Record{
				Topic:     message.topic,
				Key:       []byte(message.key),
				Value:     message.val,
				Partition: message.partition,
			}
			p.Produce(context.Background(), record, reportKafkaDelivery)
		}
	}
	cache.mu.Lock()
	cache.sending -= n
	cacheSpace.Broadcast()
	cache.mu.Unlock()
}

func reportKafkaDelivery(record *kgo.Record, err error) {
	if record == nil {
		updateKafkaStatus("Kafka producer error: unknown producer error")
		Logger.Errorf(logging.Event(logging.EventKafkaProduce, logging.Failure()),
			"Failed to send message: unknown producer error")
		return
	}
	attrs := logging.Event(logging.EventKafkaProduce,
		logging.Str("messaging.destination.name", record.Topic),
		logging.Decimal("messaging.destination.partition.id", int64(record.Partition)),
		logging.Str("airgap.message.id", string(record.Key)),
		logging.Int("airgap.message.size", int64(len(record.Value))),
	)
	if err != nil {
		updateKafkaStatus("Kafka producer error: " + err.Error())
		Logger.ErrorT(attrs.With(logging.Failure(), logging.ErrorType(err), logging.ErrMessage(err)),
			"Failed to send message: topic={messaging.destination.name} partition={messaging.destination.partition.id} key={airgap.message.id} error={error.message}")
		return
	}
	updateKafkaStatus("running")
	if Logger.CanLog(logging.DEBUG) {
		preview := string(record.Value)
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		Logger.Debugf(attrs, "Message sent: key=%s partition=%d %s", string(record.Key), record.Partition, preview)
	}
}

func updateKafkaStatus(message string) {
	cache.mu.RLock()
	update := status
	cache.mu.RUnlock()
	if update != nil {
		update(message)
	}
}

func FlushCache() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for cache.running && (len(cache.entries) > 0 || cache.sending > 0) {
		signalBatch()
		cacheSpace.Wait()
	}
}

func CloseProducer() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	stopBackgroundThread()
	cache.mu.Lock()
	p := producer
	producer = nil
	status = nil
	cache.mu.Unlock()
	if p != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.Flush(ctx); err != nil {
			Logger.Errorf(logging.Event(logging.EventKafkaProduce).Merge(logging.Err(err)), "Failed to flush Kafka producer: %v", err)
		}
		p.Close()
	}
}
