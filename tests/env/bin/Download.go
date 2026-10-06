package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/segmentio/kafka-go"
)

func main() {
       if len(os.Args) != 4 {
                fmt.Println("Usage: download <output-directory> <bootstrap-servers> <topic>")
                os.Exit(1)
        }
        bootstrapServers := os.Args[2]
        topic := os.Args[3]

	outputDir := os.Args[1]

	// Ensure directory exists
	err := os.MkdirAll(outputDir, 0755)
	if err != nil {
		log.Fatalf("failed to create output directory %s: %v", outputDir, err)
	}

	// Kafka reader config
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: []string{bootstrapServers},
		Topic:   topic,
		GroupID: "binary-consumer-group",
	})
	defer reader.Close()

	counter := 1

	fmt.Printf("Saving messages to directory: %s\n", outputDir)
	for {
		msg, err := reader.ReadMessage(context.Background())
		if err != nil {
			log.Fatalf("failed to read message: %v", err)
		}

		// Build filename like "1", "2", "3", ...
		filename := filepath.Join(outputDir, strconv.Itoa(counter))

		// Write message to file
		err = os.WriteFile(filename, msg.Value, 0644)
		if err != nil {
			log.Fatalf("failed to write file %s: %v", filename, err)
		}

		fmt.Printf("Saved message #%d (%d bytes) -> %s\n", counter, len(msg.Value), filename)
		counter++
	}
}
