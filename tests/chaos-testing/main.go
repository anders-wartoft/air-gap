package main

import (
	"crypto/sha1"
	"fmt"
	"log"
	"math/rand"
	"net"
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
)
type DATA_CONTENT struct {
	State DATA_STATE
	Created time.Time
	Received time.Time
	Duplicated time.Time
	Unknown time.Time
}
var DATA map[[20]byte]*DATA_CONTENT
var DATA_MUTEX sync.RWMutex
var PRODUCED uint32 = 0
var RECEIVED uint32 = 0
var DUPLICATED uint32 = 0
var DROPPED uint32 = 0
var FORWARDED uint32 = 0
var UNKNOWN uint32 = 0

var MITM_PORT = 1234
var MITM_DEST_ADDR = "downstream:1235"
var MITM_DROP_PROBABILITY = 0.22

var PRODUCER_SLEEP_TIME_MS = 10

var KAFKA_ADDR = []string{ "kafka:9092" }
var KAFKA_PRODUCE_TOPIC = "send"
var KAFKA_CONSUME_TOPIC = "dedup"
//var KAFKA_CONSUME_TOPIC = "receive"

func main() {
	DATA = make(map[[20]byte]*DATA_CONTENT)

	go mitm()
	go produce()
	go consume()
	for {
		write_stats()
		time.Sleep(time.Second*5)
	}
}

func randomBytes(n int) []byte {
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

func generate_message() []byte {
	//max_length := 1048576
	max_length := 104857
	length := rand.Intn(max_length)
	bytes := randomBytes(length)
	return bytes
}
func produce() {
	count := 0
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	producer, err := sarama.NewSyncProducer(KAFKA_ADDR, config)
	if err != nil {
		log.Panicln(err)
	}
	defer producer.Close()
	for {
		msg := string(generate_message())
		count += 1
		message := &sarama.ProducerMessage{
			Topic: KAFKA_PRODUCE_TOPIC,
			Value: sarama.StringEncoder(msg),
		}
		_, _, err := producer.SendMessage(message)
		if err != nil {
			log.Println("Failed to produce message: ", err.Error())
		}
		sum := sha1.Sum([]byte(msg))
		atomic.AddUint32(&PRODUCED, 1)
		DATA_MUTEX.Lock()
		DATA[sum] = &DATA_CONTENT{
			Created:    time.Now(),
			Received:   time.Time{},
			Duplicated: time.Time{},
			Unknown:    time.Time{},
			State:      Sent,
		}
		DATA_MUTEX.Unlock()
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

	buffer := make([]byte, 1024)
	for {
		n, _, err := listen_conn.ReadFromUDP(buffer)
		if err != nil {
			log.Println("Error reding udp: ", err.Error())
			continue
		}
		r := rand.Float64()
		if r < MITM_DROP_PROBABILITY {
			atomic.AddUint32(&DROPPED, 1)
			continue 
		}
		FORWARDED+=1
		sender_conn, err := net.DialUDP("udp", nil, sender_addr)
		if err != nil {
			log.Panicln("Unable to start mitm dialup: ", err.Error())
		}
		sender_conn.Write(buffer[:n])
		sender_conn.Close()
	}
}

func consume() {
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	consumer, err := sarama.NewConsumer(KAFKA_ADDR, config)
	if err != nil {
		log.Panicln(err)
	}
	defer consumer.Close()
	consumer_parition, err := consumer.ConsumePartition(KAFKA_CONSUME_TOPIC, 0, sarama.OffsetNewest)
	if err != nil {
		log.Panicln(err)
	}
	defer consumer_parition.Close()
	for {
		for message := range consumer_parition.Messages() {
			message_hash := sha1.Sum(message.Value)
			DATA_MUTEX.RLock()
			current_sate, exists := DATA[message_hash]
			DATA_MUTEX.RUnlock()
			if !exists {
				DATA_MUTEX.Lock()
				DATA[message_hash] = &DATA_CONTENT {
					Created:    time.Time{},
					Received:   time.Time{},
					Duplicated: time.Time{},
					Unknown:    time.Now(),
					State:      Unknown,
				}
				DATA_MUTEX.Unlock()
				continue
			}
			switch  current_sate.State {
			case Sent:
				DATA_MUTEX.Lock()
				DATA[message_hash].Received = time.Now()
				DATA_MUTEX.Unlock()
				atomic.AddUint32(&RECEIVED, 1)
			case Received:
				DATA_MUTEX.Lock()
				current_sate.Duplicated = time.Now()
				DATA_MUTEX.Unlock()
				atomic.AddUint32(&DUPLICATED, 1)
			case Duplicated:
				DATA_MUTEX.Lock()
				current_sate.Duplicated = time.Now()
				DATA_MUTEX.Unlock()
				atomic.AddUint32(&DUPLICATED, 1)
			case Unknown:
				DATA_MUTEX.Lock()
				current_sate.Unknown = time.Now()
				DATA_MUTEX.Unlock()
				atomic.AddUint32(&UNKNOWN, 1)
			default:
				DATA_MUTEX.Lock()
				current_sate.Unknown = time.Now()
				DATA_MUTEX.Unlock()
				atomic.AddUint32(&UNKNOWN, 1)
			}
		}
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
