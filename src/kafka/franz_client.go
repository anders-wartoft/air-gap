package kafka

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kversion"
)

type ConsumerSettings struct {
	FetchDefaultBytes    int32
	FetchMaxBytes        int32
	ChannelBufferSize    int
	MaxProcessingTime    time.Duration
	BeforeMessage        func(context.Context) error
	LegacyDelayedRead    bool
	StartupRetryInterval time.Duration
}

func FranzClientOptions(brokers, clientID string) ([]kgo.Opt, error) {
	versions := kversion.FromString(version)
	if versions == nil {
		return nil, fmt.Errorf("invalid Kafka protocol version %q", version)
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(strings.Split(brokers, ",")...),
		kgo.ClientID(clientID),
		kgo.MaxVersions(versions),
	}
	if tlsConfigParameters != nil {
		tlsConfig, err := createTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("create Kafka TLS config: %w", err)
		}
		opts = append(opts, kgo.DialTLSConfig(tlsConfig))
	}
	return opts, nil
}
