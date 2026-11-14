package kafka

import (
	"sync"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/logging"
)

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
	producer    sarama.AsyncProducer
	doneChan    chan struct{}
	wg          sync.WaitGroup
	batchSize   int
	lifecycleMu sync.Mutex
	batchReady  chan struct{}
	cacheSpace  = sync.NewCond(&cache.mu)
)

func SetProducer(p sarama.AsyncProducer, cfgBatchSize int) {
	if cfgBatchSize <= 0 {
		Logger.Panicf(logging.Event(logging.EventConfigInvalid), "Kafka batch size must be positive: %d", cfgBatchSize)
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	stopBackgroundThread()
	cache.mu.Lock()
	producer = p
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

// Called with cache.mu held; a full batch need not wait for the partial-batch timer.
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

	for _, m := range toSend {
		if p != nil {
			msg := &sarama.ProducerMessage{
				Topic:     m.topic,
				Key:       sarama.StringEncoder(m.key),
				Value:     sarama.ByteEncoder(m.val),
				Partition: m.partition,
			}
			p.Input() <- msg
		}
	}
	cache.mu.Lock()
	cache.sending -= n
	cacheSpace.Broadcast()
	cache.mu.Unlock()
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
	cache.mu.Unlock()
	if p != nil {
		if err := p.Close(); err != nil {
			Logger.Errorf(logging.Event(logging.EventKafkaProduce).Merge(logging.Err(err)), "Failed to close Kafka producer: %v", err)
		}
	}
}
