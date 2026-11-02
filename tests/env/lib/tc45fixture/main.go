package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/IBM/sarama"
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
	config := sarama.NewConfig()
	config.Version = sarama.V3_6_0_0
	config.Net.DialTimeout = 5 * time.Second
	config.Net.ReadTimeout = 5 * time.Second
	config.Net.WriteTimeout = 5 * time.Second
	config.Producer.Return.Successes = true
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Consumer.Return.Errors = true
	if *mode == "seed" {
		admin, err := sarama.NewClusterAdmin([]string{"kafka:9092"}, config)
		if err != nil {
			return err
		}
		defer admin.Close()
		for _, topic := range []string{*source, *target} {
			if err := admin.CreateTopic(topic, &sarama.TopicDetail{
				NumPartitions: 1, ReplicationFactor: 1,
			}, false); err != nil {
				return fmt.Errorf("create %s: %w", topic, err)
			}
		}
		producer, err := sarama.NewSyncProducer([]string{"kafka:9092"}, config)
		if err != nil {
			return err
		}
		defer producer.Close()
		for i := 0; i < 6; i++ {
			partition, offset, err := producer.SendMessage(&sarama.ProducerMessage{
				Topic: *source, Key: sarama.StringEncoder(fmt.Sprint(i)),
				Value: sarama.ByteEncoder(payload(i)),
			})
			if err != nil {
				return err
			}
			if partition != 0 || offset != int64(i) {
				return fmt.Errorf("unexpected source identity %d:%d", partition, offset)
			}
		}
		fmt.Println("Seeded six records, including empty, binary and fragmented payloads")
		return nil
	}
	consumer, err := sarama.NewConsumer([]string{"kafka:9092"}, config)
	if err != nil {
		return err
	}
	defer consumer.Close()
	partition, err := consumer.ConsumePartition(*target, 0, sarama.OffsetOldest)
	if err != nil {
		return err
	}
	defer partition.Close()
	deadline := time.NewTimer(*wait)
	defer deadline.Stop()
	seen := make(map[string]bool)
	for {
		select {
		case err, ok := <-partition.Errors():
			if !ok {
				return fmt.Errorf("destination consumer error channel closed unexpectedly")
			}
			return fmt.Errorf("destination consumer: %w", err)
		case message, ok := <-partition.Messages():
			if !ok {
				return fmt.Errorf("destination consumer closed unexpectedly")
			}
			key := string(message.Key)
			if seen[key] {
				return fmt.Errorf("duplicate destination identity %q", key)
			}
			index := -1
			for i := 0; i < 6; i++ {
				if key == fmt.Sprintf("%s_0_%d", *source, i) {
					index = i
				}
			}
			if index < 0 || !bytes.Equal(message.Value, payload(index)) {
				return fmt.Errorf("unexpected identity or altered payload: key=%q size=%d", key, len(message.Value))
			}
			seen[key] = true
			if len(seen) == 6 {
				if !deadline.Stop() {
					return fmt.Errorf("delivery deadline exceeded")
				}
				deadline.Reset(2 * time.Second)
			}
		case <-deadline.C:
			if len(seen) != 6 {
				return fmt.Errorf("delivered %d/6 expected records", len(seen))
			}
			fmt.Println("Verified six exact Kafka identities and payloads, with no duplicates")
			return nil
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
