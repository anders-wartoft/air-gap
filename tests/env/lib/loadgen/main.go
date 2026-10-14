package main

import (
	"archive/zip"
	"bufio"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: airgap-load produce|sink|stall|pressure|seed-retention|verify-retention|verify-status [options]")
	}
	switch os.Args[1] {
	case "produce":
		produce(os.Args[2:])
	case "sink":
		sink(os.Args[2:])
	case "stall":
		stall(os.Args[2:])
	case "pressure":
		pressure(os.Args[2:])
	case "seed-retention":
		seedRetention(os.Args[2:])
	case "verify-retention":
		verifyRetention(os.Args[2:])
	case "verify-status":
		verifyStatus(os.Args[2:])
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func produce(args []string) {
	flags := flag.NewFlagSet("produce", flag.ExitOnError)
	brokers := flags.String("brokers", "kafka:9092", "comma-separated Kafka brokers")
	topic := flags.String("topic", "transfer", "Kafka topic")
	count := flags.Int("count", 12, "number of records")
	size := flags.Int("size", 4*1024*1024, "payload bytes per record")
	sizes := flags.String("sizes", "", "comma-separated payload sizes cycled across records; overrides --size")
	_ = flags.Parse(args)

	sizeList := []int{*size}
	if *sizes != "" {
		sizeList = nil
		for _, field := range strings.Split(*sizes, ",") {
			parsed, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil || parsed <= 0 {
				fatalf("invalid --sizes entry %q", field)
			}
			sizeList = append(sizeList, parsed)
		}
	}
	largest := 0
	for _, entry := range sizeList {
		if entry > largest {
			largest = entry
		}
	}

	config := sarama.NewConfig()
	config.Version = sarama.V3_6_0_0
	config.Producer.Return.Successes = true
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.MaxMessageBytes = largest + 1024
	producer, err := sarama.NewSyncProducer(strings.Split(*brokers, ","), config)
	if err != nil {
		fatalf("create producer: %v", err)
	}
	defer producer.Close()

	payload := make([]byte, largest)
	if _, err := rand.Read(payload[:min(largest, 4096)]); err != nil {
		fatalf("seed payload: %v", err)
	}
	total := 0
	for index := 0; index < *count; index++ {
		recordSize := sizeList[index%len(sizeList)]
		copy(payload, fmt.Sprintf("AIRGAP_%06d_", index))
		_, _, err := producer.SendMessage(&sarama.ProducerMessage{
			Topic: *topic,
			Key:   sarama.StringEncoder(strconv.Itoa(index)),
			Value: sarama.ByteEncoder(payload[:recordSize]),
		})
		if err != nil {
			fatalf("send record %d: %v", index, err)
		}
		total += recordSize
	}
	fmt.Printf("produced=%d sizes=%v total=%d\n", *count, sizeList, total)
}

func sink(args []string) {
	flags := flag.NewFlagSet("sink", flag.ExitOnError)
	listen := flags.String("listen", ":1234", "TCP listen address")
	output := flags.String("output", "/airgap/work/sink.bytes", "counter output file")
	capture := flags.String("capture", "", "optional file receiving every byte read from the socket")
	_ = flags.Parse(args)

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fatalf("listen: %v", err)
	}
	defer listener.Close()

	var captureFile *os.File
	var captureMutex sync.Mutex
	if *capture != "" {
		captureFile, err = os.Create(*capture)
		if err != nil {
			fatalf("create capture file: %v", err)
		}
		defer captureFile.Close()
	}

	var total atomic.Int64
	go func() {
		for {
			if err := os.WriteFile(*output, []byte(strconv.FormatInt(total.Load(), 10)+"\n"), 0600); err != nil {
				fatalf("write counter: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			fatalf("accept: %v", err)
		}
		go func() {
			defer connection.Close()
			buffer := make([]byte, 64*1024)
			for {
				n, readErr := connection.Read(buffer)
				if n > 0 {
					total.Add(int64(n))
					if captureFile != nil {
						captureMutex.Lock()
						if _, writeErr := captureFile.Write(buffer[:n]); writeErr != nil {
							fmt.Fprintf(os.Stderr, "capture: %v\n", writeErr)
						}
						captureMutex.Unlock()
					}
				}
				if readErr != nil {
					if readErr != io.EOF {
						fmt.Fprintf(os.Stderr, "read: %v\n", readErr)
					}
					return
				}
			}
		}()
	}
}

// stall accepts TCP connections and never reads from them. The kernel socket
// buffers fill up, upstream's writes block, and nothing drains the Kafka
// consumer channel. It is used by TC-31 to show what an unbounded consumer
// buffer costs when delivery stops.
func stall(args []string) {
	flags := flag.NewFlagSet("stall", flag.ExitOnError)
	listen := flags.String("listen", ":1234", "TCP listen address")
	_ = flags.Parse(args)

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fatalf("listen: %v", err)
	}
	defer listener.Close()
	connections := make([]net.Conn, 0, 8)
	for {
		connection, err := listener.Accept()
		if err != nil {
			fatalf("accept: %v", err)
		}
		fmt.Printf("stall-accepted remote=%s\n", connection.RemoteAddr())
		connections = append(connections, connection)
	}
}

func pressure(args []string) {
	flags := flag.NewFlagSet("pressure", flag.ExitOnError)
	bytes := flags.Int("bytes", 120*1024*1024, "bytes to allocate and touch")
	duration := flags.Duration("duration", 8*time.Second, "allocation lifetime")
	_ = flags.Parse(args)

	allocation := make([]byte, *bytes)
	for offset := 0; offset < len(allocation); offset += 4096 {
		allocation[offset] = byte(offset)
	}
	fmt.Printf("pressure-ready bytes=%d\n", len(allocation))
	time.Sleep(*duration)
	fmt.Printf("pressure-released bytes=%d\n", len(allocation))
}

func seedRetention(args []string) {
	flags := flag.NewFlagSet("seed-retention", flag.ExitOnError)
	dir := flags.String("dir", "/airgap/work/lost-events", "lost-event directory to seed")
	_ = flags.Parse(args)
	if err := os.MkdirAll(*dir, 0o750); err != nil {
		fatalf("create retention directory: %v", err)
	}
	now := time.Now()
	files := []struct {
		name    string
		content string
		age     time.Duration
	}{
		{"delete-me.bin", "expired payload", 70 * 24 * time.Hour},
		{"delete-me.zip", "expired archive", 70 * 24 * time.Hour},
		{`compress "quoted"\backslash.bin`, "compress payload", 10 * 24 * time.Hour},
		{"fresh.bin", "fresh payload", 24 * time.Hour},
	}
	for _, file := range files {
		path := filepath.Join(*dir, file.name)
		if err := os.WriteFile(path, []byte(file.content), 0o640); err != nil {
			fatalf("write %s: %v", file.name, err)
		}
		modTime := now.Add(-file.age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			fatalf("set age for %s: %v", file.name, err)
		}
	}
	fmt.Printf("retention-seeded dir=%s files=%d\n", *dir, len(files))
}

func verifyRetention(args []string) {
	flags := flag.NewFlagSet("verify-retention", flag.ExitOnError)
	dir := flags.String("dir", "/airgap/work/lost-events", "lost-event directory to verify")
	_ = flags.Parse(args)

	for _, name := range []string{"delete-me.bin", "delete-me.zip", `compress "quoted"\backslash.bin`} {
		if _, err := os.Stat(filepath.Join(*dir, name)); !os.IsNotExist(err) {
			fatalf("%s should have been removed, stat error=%v", name, err)
		}
	}
	fresh, err := os.ReadFile(filepath.Join(*dir, "fresh.bin"))
	if err != nil || string(fresh) != "fresh payload" {
		fatalf("fresh.bin was not retained intact: content=%q error=%v", fresh, err)
	}

	archivePath := filepath.Join(*dir, `compress "quoted"\backslash.bin.zip`)
	archiveInfo, err := os.Stat(archivePath)
	if err != nil {
		fatalf("compressed archive is missing: %v", err)
	}
	archiveAge := time.Since(archiveInfo.ModTime())
	if archiveAge < 9*24*time.Hour || archiveAge > 11*24*time.Hour {
		fatalf("archive age=%s, want approximately 10 days", archiveAge)
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		fatalf("open compressed archive: %v", err)
	}
	defer reader.Close()
	if len(reader.File) != 1 {
		fatalf("archive entries=%d, want 1", len(reader.File))
	}
	entry, err := reader.File[0].Open()
	if err != nil {
		fatalf("open archive entry: %v", err)
	}
	content, err := io.ReadAll(entry)
	entry.Close()
	if err != nil || string(content) != "compress payload" {
		fatalf("archive payload=%q error=%v", content, err)
	}
	entryAge := time.Since(reader.File[0].Modified)
	if entryAge < 9*24*time.Hour || entryAge > 11*24*time.Hour {
		fatalf("archive entry age=%s, want approximately 10 days", entryAge)
	}
	fmt.Printf("retention-ok archive_age=%s fresh=true expired=false\n", archiveAge.Round(time.Second))
}

func verifyStatus(args []string) {
	flags := flag.NewFlagSet("verify-status", flag.ExitOnError)
	path := flags.String("file", "", "Kafka console-consumer output")
	expected := flags.Int("expected", 3, "expected truncation status records")
	_ = flags.Parse(args)
	file, err := os.Open(*path)
	if err != nil {
		fatalf("open status capture: %v", err)
	}
	defer file.Close()

	type anyValue struct {
		StringValue string `json:"stringValue"`
		IntValue    string `json:"intValue"`
	}
	type attribute struct {
		Key   string   `json:"key"`
		Value anyValue `json:"value"`
	}
	type logRecord struct {
		EventName  string      `json:"eventName"`
		Attributes []attribute `json:"attributes"`
	}
	records := 0
	actions := make(map[string]int)
	names := make(map[string]int)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var record logRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil || record.EventName != "message.lost.truncated" {
			continue
		}
		records++
		for _, attribute := range record.Attributes {
			switch attribute.Key {
			case "airgap.event.lost.action":
				actions[attribute.Value.StringValue]++
			case "file.name":
				names[attribute.Value.StringValue]++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fatalf("read status capture: %v", err)
	}
	if records != *expected {
		fatalf("truncation status records=%d, want %d", records, *expected)
	}
	if actions["deleted"] != 2 || actions["compressed"] != 1 {
		fatalf("status actions=%v, want deleted=2 compressed=1", actions)
	}
	for _, name := range []string{"delete-me.bin", "delete-me.zip", `compress "quoted"\backslash.bin`} {
		if names[name] != 1 {
			fatalf("status for %q occurred %d times, want 1; names=%v", name, names[name], names)
		}
	}
	if names["fresh.bin"] != 0 {
		fatalf("fresh.bin unexpectedly produced a truncation status")
	}
	fmt.Printf("status-ok records=%d actions=%v\n", records, actions)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
