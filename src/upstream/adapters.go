package upstream

import (
	"context"

	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/udp"
)

type KafkaAdapter struct{}

func (k *KafkaAdapter) Read(ctx context.Context, name string, offset int,
	bootstrapServers, topic, group, from string,
	handler kafka.KafkaHandler) {
	kafka.ReadFromKafkaWithContext(ctx, name, offset, bootstrapServers, topic, group, from, handler)
}

func (k *KafkaAdapter) SetTLS(certFile, keyFile, caFile, keyPasswordFile string) error {
	_, err := kafka.SetTLSConfigParameters(certFile, keyFile, caFile, keyPasswordFile)
	return err
}

type UDPAdapter struct {
	conn *udp.UDPConn
}

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
