package downstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"sitia.nu/airgap/src/kafka"
)

func TestKafkaClientIDEnvironmentOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "downstream.properties")
	if err := os.WriteFile(path, []byte("clientId=from-properties\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := readConfigurationFile(path, defaultConfiguration(), true)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("AIRGAP_DOWNSTREAM_CLIENT_ID", "")
	if got := overrideConfiguration(config).clientID; got != "from-properties" {
		t.Fatalf("with empty environment: clientId = %q, want from-properties", got)
	}
	t.Setenv("AIRGAP_DOWNSTREAM_CLIENT_ID", "from-environment")
	if got := overrideConfiguration(config).clientID; got != "from-environment" {
		t.Fatalf("with environment override: clientId = %q, want from-environment", got)
	}
}

func TestRetryableKafkaStartupError(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"connection_refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"broker_unavailable", kerr.BrokerNotAvailable, true},
		{"leader_not_ready", kerr.LeaderNotAvailable, true},
		{"coordinator_not_ready", kerr.CoordinatorNotAvailable, true},
		{"metadata_timeout", kerr.RequestTimedOut, true},
		{"authentication", kerr.SaslAuthenticationFailed, false},
		{"authorization", kerr.ClusterAuthorizationFailed, false},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false},
		{"wrapped_authentication", errors.Join(kerr.BrokerNotAvailable, kerr.SaslAuthenticationFailed), false},
		{"wrapped_certificate", errors.Join(kerr.BrokerNotAvailable, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), false},
		{"wrapped_network", fmt.Errorf("broker: %w", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		{"unknown", errors.New("unrecognized failure"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableKafkaStartupError(fmt.Errorf("producer: %w", test.err)); got != test.retry {
				t.Fatalf("retryable = %t, want %t", got, test.retry)
			}
		})
	}
}

type startupTestProducer struct {
	pingErr error
	closed  chan struct{}
	once    sync.Once
}

func (p *startupTestProducer) Ping(context.Context) error                                   { return p.pingErr }
func (*startupTestProducer) Produce(context.Context, *kgo.Record, func(*kgo.Record, error)) {}
func (*startupTestProducer) Flush(context.Context) error                                    { return nil }
func (p *startupTestProducer) Close() {
	if p.closed != nil {
		p.once.Do(func() { close(p.closed) })
	}
}

func TestKafkaProducerStartupRetriesUntilReady(t *testing.T) {
	expected := &startupTestProducer{}
	var attempts []time.Time
	failures := []error{kerr.BrokerNotAvailable, kerr.LeaderNotAvailable, kerr.BrokerNotAvailable}
	interval := 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	producer, err := newKafkaProducerWithRetry(ctx, "test", interval, func() (kafka.Producer, error) {
		attempts = append(attempts, time.Now())
		if len(attempts) <= len(failures) {
			return nil, failures[len(attempts)-1]
		}
		return expected, nil
	})
	if err != nil || producer != expected || len(attempts) != 4 {
		t.Fatalf("producer=%v err=%v attempts=%d", producer, err, len(attempts))
	}
	for i := 1; i < len(attempts); i++ {
		if delay := attempts[i].Sub(attempts[i-1]); delay < interval {
			t.Fatalf("retry delay %s < configured %s", delay, interval)
		}
	}
}

func TestKafkaProducerStartupCancellationAndPermanentErrors(t *testing.T) {
	t.Run("cancel_inflight_creation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, release := make(chan struct{}), make(chan struct{})
		producer := &startupTestProducer{closed: make(chan struct{})}
		finished := make(chan error, 1)
		go func() {
			_, err := newKafkaProducerWithRetry(ctx, "test", time.Hour, func() (kafka.Producer, error) {
				close(started)
				<-release
				return producer, nil
			})
			finished <- err
		}()
		<-started
		cancel()
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("unexpected error: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("shutdown blocked on producer creation")
		}
		close(release)
		select {
		case <-producer.closed:
		case <-time.After(time.Second):
			t.Fatal("producer created after cancellation was not closed")
		}
	})
	t.Run("cancel_backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		attempted := make(chan struct{})
		finished := make(chan error, 1)
		go func() {
			_, err := newKafkaProducerWithRetry(ctx, "test", time.Hour, func() (kafka.Producer, error) {
				close(attempted)
				return nil, kerr.BrokerNotAvailable
			})
			finished <- err
		}()
		<-attempted
		cancel()
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown did not interrupt retry wait")
		}
	})
	t.Run("already_canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newKafkaProducerWithRetry(ctx, "test", time.Hour, func() (kafka.Producer, error) {
			t.Fatal("attempt made after cancellation")
			return nil, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("permanent_ping_error", func(t *testing.T) {
		attempts := 0
		producer := &startupTestProducer{pingErr: kerr.TypedErrorForCode(kerr.SaslAuthenticationFailed.Code), closed: make(chan struct{})}
		_, err := newKafkaProducerWithRetry(context.Background(), "test", time.Hour, func() (kafka.Producer, error) {
			attempts++
			return producer, nil
		})
		var kafkaError *kerr.Error
		if !errors.As(err, &kafkaError) || kafkaError.Code != kerr.SaslAuthenticationFailed.Code || attempts != 1 {
			t.Fatalf("err=%v attempts=%d", err, attempts)
		}
		select {
		case <-producer.closed:
		default:
			t.Fatal("producer was not closed after permanent ping error")
		}
	})
}

func TestKafkaRetryIntervalConfiguration(t *testing.T) {
	if got := defaultConfiguration().kafkaRetryInterval; got != 5000 {
		t.Fatalf("default interval=%d", got)
	}
	for _, value := range []string{"0", "-1", "bad", "9223372036855"} {
		if _, err := parseKafkaRetryInterval(value); err == nil {
			t.Errorf("accepted invalid interval %q", value)
		}
	}
	path := filepath.Join(t.TempDir(), "downstream.properties")
	if err := os.WriteFile(path, []byte("kafkaRetryInterval=200\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := readConfigurationFile(path, defaultConfiguration(), true)
	if err != nil || config.kafkaRetryInterval != 200 {
		t.Fatalf("properties config=%d err=%v", config.kafkaRetryInterval, err)
	}
	if err := os.WriteFile(path, []byte("kafkaRetryInterval=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigurationFile(path, defaultConfiguration(), true); err == nil {
		t.Error("properties accepted invalid interval")
	}
	t.Setenv("AIRGAP_DOWNSTREAM_KAFKA_RETRY_INTERVAL", "300")
	config = overrideConfiguration(config)
	if config.kafkaRetryInterval != 300 {
		t.Fatalf("environment interval=%d", config.kafkaRetryInterval)
	}
	config = parseCommandLineOverrides([]string{"--kafkaRetryInterval=400"}, config)
	if config.kafkaRetryInterval != 400 {
		t.Fatalf("CLI interval=%d", config.kafkaRetryInterval)
	}
}

func TestProducerCompressionConfiguration(t *testing.T) {
	if got := defaultConfiguration().producerCompression; got != "none" {
		t.Fatalf("default producerCompression=%q", got)
	}
	for _, value := range []string{"", "snappy2", "producer"} {
		if _, err := parseProducerCompression(value); err == nil {
			t.Errorf("accepted invalid producerCompression %q", value)
		}
	}
	if codec, err := parseProducerCompression("Snappy"); err != nil || codec != "snappy" {
		t.Fatalf("mixed-case codec=%q err=%v", codec, err)
	}
	path := filepath.Join(t.TempDir(), "downstream.properties")
	if err := os.WriteFile(path, []byte("producerCompression=gzip\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := readConfigurationFile(path, defaultConfiguration(), true)
	if err != nil || config.producerCompression != "gzip" {
		t.Fatalf("properties codec=%q err=%v", config.producerCompression, err)
	}
	if err := os.WriteFile(path, []byte("producerCompression=brotli\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigurationFile(path, defaultConfiguration(), true); err == nil {
		t.Error("properties accepted invalid producerCompression")
	}
	t.Setenv("AIRGAP_DOWNSTREAM_PRODUCER_COMPRESSION", "zstd")
	config = overrideConfiguration(config)
	if config.producerCompression != "zstd" {
		t.Fatalf("environment codec=%q", config.producerCompression)
	}
	config = parseCommandLineOverrides([]string{"--producerCompression=none"}, config)
	if config.producerCompression != "none" {
		t.Fatalf("CLI codec=%q", config.producerCompression)
	}
}
