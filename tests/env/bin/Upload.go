package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/segmentio/kafka-go"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Println("Usage: producer <file> <bootstrap-servers> <topic>")
		os.Exit(1)
	}
	filename := os.Args[1]
	bootstrapServers := os.Args[2]
	topic := os.Args[3]

	// Configure writer
	writer := kafka.NewWriter(kafka.WriterConfig{
		Brokers: []string{bootstrapServers},
		Topic:   topic,
	})
	defer writer.Close()

	// Read binary file
	data, err := os.ReadFile(filename)
	if err != nil {
		log.Fatalf("failed to read file %s: %v", filename, err)
	}

	// Send raw bytes
	err = writer.WriteMessages(context.Background(),
		kafka.Message{
			Key:   []byte(filename), // filename as key (optional)
			Value: data,
		},
	)
	if err != nil {
		log.Printf("Filename: %s, Bootstrap-servers: %s, topic: %s", filename, bootstrapServers, topic)
		log.Fatalf("failed to write message: %v", err)
	}

	fmt.Printf("Sent file %s (%d bytes) to Kafka\n", filename, len(data))
}

