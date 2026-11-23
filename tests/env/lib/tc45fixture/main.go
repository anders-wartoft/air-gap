package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/kversion"
)

func payload(index int) []byte {
	sizes := []int{0, 1, 128, 1400, 8192, 32768}
	value := make([]byte, sizes[index])
	for i := range value {
		value[i] = byte((i*37 + index) % 256)
	}
	return value
}

func run() error {
	mode := flag.String("mode", "seed", "seed or verify")
	source := flag.String("source", "", "source topic")
	target := flag.String("target", "", "destination topic")
	wait := flag.Duration("wait", 40*time.Second, "verification deadline")
	flag.Parse()
	if (*mode != "seed" && *mode != "verify") || *source == "" || *target == "" || *wait <= 0 {
		return fmt.Errorf("require --mode=seed|verify, --source, --target and positive --wait")
	}
	clientOptions := []kgo.Opt{
		kgo.SeedBrokers("kafka:9092"),
		kgo.ClientID("tc45-fixture"),
		kgo.MaxVersions(kversion.FromString("3.6.0")),
		kgo.DialTimeout(5 * time.Second),
		kgo.RequestTimeoutOverhead(5 * time.Second),
		kgo.RequiredAcks(kgo.AllISRAcks()),
	}
	if *mode == "seed" {
		admin, err := kgo.NewClient(clientOptions...)
		if err != nil {
			return err
		}
		defer admin.Close()
		create := kmsg.NewPtrCreateTopicsRequest()
		create.TimeoutMillis = 5000
		for _, topic := range []string{*source, *target} {
			create.Topics = append(create.Topics, kmsg.CreateTopicsRequestTopic{
				Topic: topic, NumPartitions: 1, ReplicationFactor: 1,
			})
		}
		response, err := create.RequestWith(context.Background(), admin)
		if err != nil {
			return err
		}
		for _, topic := range response.Topics {
			if topic.ErrorCode != 0 && topic.ErrorCode != kerr.TopicAlreadyExists.Code {
				return fmt.Errorf("create %s: %w", topic.Topic, kerr.TypedErrorForCode(topic.ErrorCode))
			}
		}
		producerOptions := append(clientOptions, kgo.RecordPartitioner(kgo.ManualPartitioner()))
		producer, err := kgo.NewClient(producerOptions...)
		if err != nil {
			return err
		}
		defer producer.Close()
		for i := 0; i < 6; i++ {
			record := &kgo.Record{
				Topic: *source, Partition: 0, Key: []byte(fmt.Sprint(i)),
				Value: payload(i),
			}
			if err := producer.ProduceSync(context.Background(), record).FirstErr(); err != nil {
				return err
			}
			if record.Partition != 0 || record.Offset != int64(i) {
				return fmt.Errorf("unexpected source identity %d:%d", record.Partition, record.Offset)
			}
		}
		fmt.Println("Seeded six records, including empty, binary and fragmented payloads")
		return nil
	}
	consumerOptions := append(clientOptions,
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{
			*target: {0: kgo.NewOffset().AtStart()},
		}),
		kgo.FetchMaxPartitionBytes(1024*1024),
	)
	consumer, err := kgo.NewClient(consumerOptions...)
	if err != nil {
		return err
	}
	defer consumer.Close()
	deadline := time.Now().Add(*wait)
	seen := make(map[string]bool)
	for {
		if time.Until(deadline) <= 0 {
			if len(seen) != 6 {
				return fmt.Errorf("delivered %d/6 expected records", len(seen))
			}
			fmt.Println("Verified six exact Kafka identities and payloads, with no duplicates")
			return nil
		}
		pollCtx, cancel := context.WithTimeout(context.Background(), min(100*time.Millisecond, time.Until(deadline)))
		fetches := consumer.PollRecords(pollCtx, 1)
		cancel()
		for _, fetchErr := range fetches.Errors() {
			if !errors.Is(fetchErr.Err, context.DeadlineExceeded) && !errors.Is(fetchErr.Err, context.Canceled) {
				return fmt.Errorf("destination consumer: %w", fetchErr.Err)
			}
		}
		var recordErr error
		fetches.EachRecord(func(record *kgo.Record) {
			key := string(record.Key)
			if seen[key] {
				recordErr = fmt.Errorf("duplicate destination identity %q", key)
				return
			}
			index := -1
			for i := 0; i < 6; i++ {
				if key == fmt.Sprintf("%s_0_%d", *source, i) {
					index = i
				}
			}
			if index < 0 || !bytes.Equal(record.Value, payload(index)) {
				recordErr = fmt.Errorf("unexpected identity or altered payload: key=%q size=%d", key, len(record.Value))
				return
			}
			seen[key] = true
			if len(seen) == 6 {
				deadline = time.Now().Add(2 * time.Second)
			}
		})
		if recordErr != nil {
			return recordErr
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
