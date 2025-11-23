package resend

import (
	"context"
	"strings"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/udp"
)

// KafkaAdapter is the concrete adapter for the real Kafka client.
type KafkaAdapter struct{}

// NewKafkaAdapter returns a usable Kafka adapter.
func NewKafkaAdapter() *KafkaAdapter {
	return &KafkaAdapter{}
}

func (k *KafkaAdapter) Read(ctx context.Context, name string, offset int,
	bootstrapServers, topic, group, from string,
	handler kafka.KafkaHandler) {
	kafka.ReadFromKafkaWithContext(ctx, name, offset, bootstrapServers, topic, group, from, handler)
}

func (k *KafkaAdapter) ReadToEndPartition(partition int, ctx context.Context, brokers string, topic string, group string, fromOffset int64,
	callbackFunction func(string, []byte, time.Time, []byte) bool) error {
	return kafka.ReadToEndPartition(partition, ctx, brokers, topic, group, fromOffset, callbackFunction)
}

func (k *KafkaAdapter) SetTLS(certFile, keyFile, caFile, keyPasswordFile string) error {
	_, err := kafka.SetTLSConfigParameters(certFile, keyFile, caFile, keyPasswordFile)
	return err
}

func (k *KafkaAdapter) GetLastOffset(servers, topic string, partition int) (int64, error) {
	config := sarama.NewConfig()
	bootstrapServers := strings.Split(servers, ",")
	client, err := sarama.NewClient(bootstrapServers, config)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	offset, err := client.GetOffset(topic, int32(partition), sarama.OffsetNewest)
	if err != nil {
		return 0, err
	}
	return offset, nil
}

// UDPAdapter is the concrete adapter for UDP.
type UDPAdapter struct {
	conn *udp.UDPConn
}

// NewUDPAdapter creates and returns a UDPAdapter wrapping a *udp.UDPConn.
func NewUDPAdapter(address string) (*UDPAdapter, error) {
	c, err := udp.NewUDPConn(address)
	if err != nil {
		return nil, err
	}
	return &UDPAdapter{conn: c}, nil
}

func (u *UDPAdapter) SendMessage(msg []byte) error {
	return u.conn.SendMessage(msg)
}

func (u *UDPAdapter) SendMessages(msgs [][]byte) error {
	return u.conn.SendMessages(msgs)
}

func (u *UDPAdapter) Close() error {
	return u.conn.Close()
}
