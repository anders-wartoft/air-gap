package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
)
type DATA_STATE int 
const (
	Sent DATA_STATE = iota
	Received
	Duplicated
	Unknown
	UnknownDuplicated
)
func (d DATA_STATE) String() string {
	switch d {
	case Duplicated:
		return "DUPLICATED"
	case Received:
		return "RECEIVED"
	case Sent:
		return "SENT"
	case Unknown:
		return "UNKNOWN"
	case UnknownDuplicated:
		return "UNKNOWN_DUPLICATED"
	default:
		panic(fmt.Sprintf("unexpected main.DATA_STATE: %#v", d))
	}
}
type DATA_CONTENT struct {
	State DATA_STATE
	Created time.Time
	Received time.Time
	Duplicated time.Time
	Duplicated_times uint16
	Unknown time.Time
}
type DATA_struct struct {
	Data map[[32]byte]*DATA_CONTENT
	Mutex sync.RWMutex
}
var DATA DATA_struct
var PRODUCED uint32 = 0
var RECEIVED uint32 = 0
var DUPLICATED uint32 = 0
var DROPPED uint32 = 0
var FORWARDED uint32 = 0
var UNKNOWN uint32 = 0

var MITM_PORT = 1234
var MITM_DEST_ADDR = "downstream:1235"
var MITM_DROP_PROBABILITY = 0.01

var PRODUCER_SLEEP_TIME_MS = 1
//var PRODUCER_MAX_PACKET_SIZE = 1000000
var PRODUCER_MAX_PACKET_SIZE = 100

var KAFKA_ADDR = []string{ "kafka:9092" }
var KAFKA_PRODUCE_TOPIC = "send"
var KAFKA_CONSUME_TOPIC = []string{"dedup"}
//var KAFKA_CONSUME_TOPIC = []string{"receive"}

var FILE_PATH = "/out/stats.csv"

func main() {
	DATA = DATA_struct{
		Data: make(map[[32]byte]*DATA_CONTENT),
		Mutex: sync.RWMutex{},
	}
	go mitm()
	go produce()
	go consume()
	for {
		write_stats()
		write_file()
		time.Sleep(time.Second*5)
	}
}

func randomBytes(n int) []byte  {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < n; i += 8 {
		v := r.Uint64()
		for j := 0; j < 8 && i+j < n; j++ {
			b[i+j] = byte(v)
			v >>= 8
		}
	}
	return b
}
var message_count uint64 = 0
func generate_message() string {
       current_count := atomic.AddUint64(&message_count, 1)
       length := rand.Intn(PRODUCER_MAX_PACKET_SIZE)+1
       payload := randomBytes(length)
       return fmt.Sprint(string(payload), " number is : ", current_count)
}
func produce() {
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	producer, err := sarama.NewSyncProducer(KAFKA_ADDR, config)
	if err != nil {
		log.Panicln(err)
	}
	defer producer.Close()
	for {
		msg := generate_message()
		message := &sarama.ProducerMessage{
			Topic: KAFKA_PRODUCE_TOPIC,
			Value: sarama.StringEncoder(msg),
		}
		_, _, err := producer.SendMessage(message)
		if err != nil {
			log.Println("Failed to produce message: ", err.Error())
		}
		sum := sha256.Sum256([]byte(msg))
		atomic.AddUint32(&PRODUCED, 1)
		DATA.Mutex.Lock()
		if _, exist := DATA.Data[sum]; exist {
			panic("An message was generated with already known hash")
		}
		DATA.Data[sum] = &DATA_CONTENT{
			Created:          time.Now(),
			Received:         time.Time{},
			Duplicated:       time.Time{},
			Unknown:          time.Time{},
			State:            Sent,
			Duplicated_times: 0,
		}
		DATA.Mutex.Unlock()
		time.Sleep(time.Millisecond*time.Duration(PRODUCER_SLEEP_TIME_MS))
	}
}

func mitm() {
	receiver_addr := net.UDPAddr {
		Port: MITM_PORT,
		IP: net.ParseIP("0.0.0.0"),
	}
	listen_conn, err := net.ListenUDP("udp", &receiver_addr)
	if err != nil {
		log.Panicln("Unable to start mitm listener: ", err.Error())
	}
	defer listen_conn.Close()
	sender_addr, err := net.ResolveUDPAddr("udp", MITM_DEST_ADDR)
	if err != nil {
		log.Panicln("Unable to start mitm resolver: ", err.Error())
	}
	sender_conn, err := net.DialUDP("udp", nil, sender_addr)
	if err != nil {
		log.Panicln("Unable to start mitm dial: ", err.Error())
	}

	buffer := make([]byte, 1024)
	for {
		n, _, err := listen_conn.ReadFromUDP(buffer)
		if err != nil {
			log.Println("Error reading udp: ", err.Error())
			continue
		}
		r := rand.Float64()
		if r < MITM_DROP_PROBABILITY {
			atomic.AddUint32(&DROPPED, 1)
			continue 
		}
		FORWARDED+=1
		sender_conn.Write(buffer[:n])
		sender_conn.Close()
	}
}

func consume() {
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	consumer_group, err := sarama.NewConsumerGroup(KAFKA_ADDR, "chaos", config)
	if err != nil {
		log.Panicln(err)
	}
	defer consumer_group.Close()
	cw := context.Background()
	for {
		err := consumer_group.Consume(cw, KAFKA_CONSUME_TOPIC, &handler{})
		if err != nil {
			log.Println("COULDN'T CONSUME!!")
		}
	}
}
type handler struct{}

func (h *handler) Setup(s sarama.ConsumerGroupSession) error   { return nil }
func (h *handler) Cleanup(s sarama.ConsumerGroupSession) error { return nil }

func (h *handler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
    for message := range claim.Messages() {
	message_hash := sha256.Sum256(message.Value)
	consume_message(message_hash)
	session.MarkMessage(message, "")
    }
    return nil
}
func consume_message(message_hash [32]byte) {
	DATA.Mutex.Lock()
	defer DATA.Mutex.Unlock()
	current_sate, exists := DATA.Data[message_hash]
	if !exists {
		DATA.Data[message_hash] = &DATA_CONTENT {
			Created:          time.Time{},
			Received:         time.Time{},
			Duplicated:       time.Time{},
			Unknown:          time.Now(),
			State:            Unknown,
			Duplicated_times: 0,
		}
		return
	}
	switch  current_sate.State {
	case Sent:
		current_sate.State = Received
		DATA.Data[message_hash].Received = time.Now()
		atomic.AddUint32(&RECEIVED, 1)
	case Received:
		current_sate.State = Duplicated
		current_sate.Duplicated = time.Now()
		current_sate.Duplicated_times += 1
		atomic.AddUint32(&DUPLICATED, 1)
	case Duplicated:
		current_sate.State = Duplicated
		current_sate.Duplicated = time.Now()
		current_sate.Duplicated_times += 1
		atomic.AddUint32(&DUPLICATED, 1)
	case Unknown, UnknownDuplicated:
		current_sate.State = UnknownDuplicated
		current_sate.Duplicated_times += 1
	default:
		panic(fmt.Sprintf("unexpected main.DATA_STATE: %#v", current_sate.State))
	}
}

func write_stats() {
	produced := atomic.LoadUint32(&PRODUCED)
	received := atomic.LoadUint32(&RECEIVED)
	duplicated := atomic.LoadUint32(&DUPLICATED)
	dropped := atomic.LoadUint32(&DROPPED)
	forwarded := atomic.LoadUint32(&FORWARDED)
	unknown := atomic.LoadUint32(&UNKNOWN)
	fmt.Println("PRODUCED: ", produced, " | RECEIVED: ", received, " | DUPLICATED: ", duplicated, " | DROPPED: ", dropped, " | FORWARDED: ", forwarded, " | UNKNOWN: ", unknown)
}
func write_file() {
	DATA.Mutex.RLock()
	defer DATA.Mutex.RUnlock()
	start_time := time.Now()
	defer fmt.Println("It took: ", (time.Now().Second() - start_time.Second()), "s to create the statistics file" )

	file, err := os.Create(FILE_PATH)
	if err != nil {
		log.Println("ERROR CREATING STATISTICS FILE: ", err.Error())
		return
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	fmt.Fprintln(writer, "ID,STATE,CREATED,RECEIVED,DUPLICATED,DUPLICATED_TIMES, UNKNOWN")
	for hash, packet := range(DATA.Data) {
		fmt.Fprintf(writer, "%x,%s,%v,%v,%v,%d,%v\n", 
			hash, 
			packet.State, 
			packet.Created, 
			packet.Received,
			packet.Duplicated, 
			packet.Duplicated_times, 
			packet.Unknown)
	}
	err = writer.Flush()
	if err != nil {
		log.Println("Error flushing statistics file: ", err.Error())
	}
}
