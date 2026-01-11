package kafka

import (
	"sync"
	"time"

	"github.com/IBM/sarama"
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
}

var (
	cache         = MessageCache{entries: make([]MessageCacheEntry, 0)}
	producer      sarama.AsyncProducer
	doneChan      chan struct{}
	wg            sync.WaitGroup
	batchSize     int
	errorCallback func(string) // Optional callback for producer errors
)

func SetProducer(p sarama.AsyncProducer, cfgBatchSize int) {
	StopBackgroundThread()
	producer = p
	batchSize = cfgBatchSize

	cache.mu.Lock()
	cache.entries = nil
	cache.mu.Unlock()
	StartBackgroundThread()
}

// SetErrorCallback sets an optional callback to be called when producer errors occur
func SetErrorCallback(callback func(string)) {
	errorCallback = callback
}

func StartBackgroundThread() {
	if producer == nil || doneChan != nil {
		return
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	doneChan = make(chan struct{})
	wg.Add(1)

	go func() {
		defer wg.Done()
		for {
			select {
			case <-ticker.C:
				sendBatch()
			case <-doneChan:
				ticker.Stop()
				sendBatch()
				return
			case err := <-producer.Errors():
				if err != nil && errorCallback != nil {
					errorCallback("Kafka producer error: " + err.Err.Error())
				}
			case success := <-producer.Successes():
				// Could track success here if needed, but we don't report per-message
				_ = success
			}
		}
	}()
}

func StopBackgroundThread() {
	if doneChan != nil {
		close(doneChan)
		wg.Wait()
		doneChan = nil
		cache.mu.Lock()
		cache.entries = nil
		cache.mu.Unlock()
	}
}

func WriteToKafka(key, topic string, partition int32, val []byte) {
	cache.mu.Lock()
	copyVal := make([]byte, len(val))
	copy(copyVal, val)
	cache.entries = append(cache.entries, MessageCacheEntry{
		key:       key,
		val:       copyVal,
		topic:     topic,
		partition: partition,
	})
	cache.mu.Unlock()
}

func sendBatch() {
	cache.mu.Lock()
	n := len(cache.entries)
	if n == 0 {
		cache.mu.Unlock()
		return
	}

	var toSend []MessageCacheEntry
	if n <= batchSize {
		toSend = cache.entries
		cache.entries = nil
	} else {
		toSend = cache.entries[:batchSize]
		cache.entries = cache.entries[batchSize:]
	}
	cache.mu.Unlock()

	for _, m := range toSend {
		if producer != nil {
			msgCopy := make([]byte, len(m.val))
			copy(msgCopy, m.val)
			msg := &sarama.ProducerMessage{
				Topic:     m.topic,
				Key:       sarama.StringEncoder(m.key),
				Value:     sarama.ByteEncoder(msgCopy),
				Partition: m.partition,
			}
			producer.Input() <- msg
		}
	}
}

func FlushCache() {
	for {
		cache.mu.RLock()
		n := len(cache.entries)
		cache.mu.RUnlock()
		if n == 0 {
			break
		}
		sendBatch()
		time.Sleep(1 * time.Millisecond)
	}
}

func CloseProducer() {
	if producer != nil {
		producer.Close()
		producer = nil
	}
}
