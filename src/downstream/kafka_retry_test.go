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
	"syscall"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

func TestRetryableKafkaStartupError(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"connection_refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"broker_unavailable", sarama.ErrOutOfBrokers, true},
		{"leader_not_ready", sarama.ErrLeaderNotAvailable, true},
		{"coordinator_not_ready", sarama.ErrConsumerCoordinatorNotAvailable, true},
		{"metadata_timeout", sarama.ErrRequestTimedOut, true},
		{"configuration", sarama.ConfigurationError("invalid"), false},
		{"authentication", sarama.ErrSASLAuthenticationFailed, false},
		{"authorization", sarama.ErrClusterAuthorizationFailed, false},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false},
		{"wrapped_authentication", sarama.Wrap(sarama.ErrOutOfBrokers, sarama.ErrSASLAuthenticationFailed), false},
		{"wrapped_certificate", sarama.Wrap(sarama.ErrOutOfBrokers, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), false},
		{"wrapped_network", sarama.Wrap(sarama.ErrOutOfBrokers, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		{"unknown", errors.New("unrecognized failure"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableKafkaStartupError(fmt.Errorf("producer: %w", test.err)); got != test.retry {
				t.Fatalf("retryable = %t, want %t", got, test.retry)
			}
		})
	}
}

type startupTestProducer struct{ sarama.AsyncProducer }

type canceledStartupProducer struct {
	sarama.AsyncProducer
	closed chan struct{}
}

func (p *canceledStartupProducer) Close() error {
	close(p.closed)
	return nil
}

func TestKafkaProducerStartupRetriesUntilReady(t *testing.T) {
	expected := &startupTestProducer{}
	var attempts []time.Time
	failures := []error{sarama.ErrOutOfBrokers, sarama.ErrLeaderNotAvailable, sarama.ErrBrokerNotAvailable}
	interval := 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	producer, err := newKafkaProducerWithRetry(ctx, "test", interval, func() (sarama.AsyncProducer, error) {
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
		producer := &canceledStartupProducer{closed: make(chan struct{})}
		finished := make(chan error, 1)
		go func() {
			_, err := newKafkaProducerWithRetry(ctx, "test", time.Hour, func() (sarama.AsyncProducer, error) {
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
			_, err := newKafkaProducerWithRetry(ctx, "test", time.Hour, func() (sarama.AsyncProducer, error) {
				close(attempted)
				return nil, sarama.ErrOutOfBrokers
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
		_, err := newKafkaProducerWithRetry(ctx, "test", time.Hour, func() (sarama.AsyncProducer, error) {
			t.Fatal("attempt made after cancellation")
			return nil, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("permanent", func(t *testing.T) {
		attempts := 0
		_, err := newKafkaProducerWithRetry(context.Background(), "test", time.Hour, func() (sarama.AsyncProducer, error) {
			attempts++
			return nil, sarama.ErrSASLAuthenticationFailed
		})
		if !errors.Is(err, sarama.ErrSASLAuthenticationFailed) || attempts != 1 {
			t.Fatalf("err=%v attempts=%d", err, attempts)
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
