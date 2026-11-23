package kafka

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
)

type fakeClient struct{ closed atomic.Bool }

func (f *fakeClient) Close() error {
	f.closed.Store(true)
	return nil
}

func TestRetryableStartupError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, err := range []error{
		refused,
		fmt.Errorf("kafka: broker unavailable: %w", kerr.BrokerNotAvailable),
		kerr.BrokerNotAvailable,
		kerr.CoordinatorNotAvailable,
		kerr.RequestTimedOut,
	} {
		if !RetryableStartupError(err) {
			t.Errorf("expected retryable: %v", err)
		}
	}
	for _, err := range []error{
		errors.New("bad config"),
		kerr.SaslAuthenticationFailed,
		kerr.TypedErrorForCode(kerr.SaslAuthenticationFailed.Code),
		kerr.GroupAuthorizationFailed,
		errors.New("unknown"),
	} {
		if RetryableStartupError(err) {
			t.Errorf("expected permanent: %v", err)
		}
	}
}

func TestStartWithRetryRetriesTransientErrorsUntilReady(t *testing.T) {
	var attempts, retries int64
	want := &fakeClient{}
	got, err := startWithRetry(context.Background(), time.Millisecond, func() (*fakeClient, error) {
		if atomic.AddInt64(&attempts, 1) < 4 {
			return nil, kerr.BrokerNotAvailable
		}
		return want, nil
	}, func(attempt int64, err error) {
		retries++
		if attempt != retries || !errors.Is(err, kerr.BrokerNotAvailable) {
			t.Errorf("onRetry(%d, %v), want attempt %d", attempt, err, retries)
		}
	})
	if err != nil || got != want {
		t.Fatalf("startWithRetry = %v, %v; want client", got, err)
	}
	if attempts != 4 || retries != 3 {
		t.Fatalf("attempts=%d retries=%d, want 4 and 3", attempts, retries)
	}
}

func TestStartWithRetryStopsOnPermanentError(t *testing.T) {
	var retries int
	_, err := startWithRetry(context.Background(), time.Millisecond, func() (*fakeClient, error) {
		return nil, kerr.SaslAuthenticationFailed
	}, func(int64, error) { retries++ })
	if !errors.Is(err, kerr.SaslAuthenticationFailed) || retries != 0 {
		t.Fatalf("err=%v retries=%d, want permanent error without retry", err, retries)
	}
}

func TestStartWithRetryCancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := startWithRetry(ctx, time.Hour, func() (*fakeClient, error) {
			return nil, kerr.BrokerNotAvailable
		}, func(int64, error) { cancel() })
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startWithRetry did not return after cancellation during the retry wait")
	}
}

func TestStartWithRetryCancelDuringAttemptClosesLateClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan struct{})
	late := &fakeClient{}
	done := make(chan error, 1)
	go func() {
		_, err := startWithRetry(ctx, time.Millisecond, func() (*fakeClient, error) {
			close(started)
			<-release
			return late, nil
		}, func(int64, error) {})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startWithRetry did not return while an attempt was in flight")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !late.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("client created after cancellation was not closed")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestReadFromKafkaWaitsForUnavailableBroker reproduces upstream starting
// before local Kafka: the consumer must keep retrying (no panic) and return
// cleanly when its context is canceled.
func TestReadFromKafkaWaitsForUnavailableBroker(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()

	var statuses atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		ReadFromKafkaWithContextSettings(ctx, "startup-test", 0, address, "topic", "group", "",
			func(string, []byte, time.Time, []byte) bool { return true },
			ConsumerSettings{StartupRetryInterval: 10 * time.Millisecond},
			func(status string) {
				if status != "running" {
					statuses.Add(1)
				}
			})
	}()

	deadline := time.Now().Add(20 * time.Second)
	for statuses.Load() < 3 {
		select {
		case p := <-done:
			t.Fatalf("consumer returned before cancellation (panic: %v)", p)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("consumer did not report repeated startup failures")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("consumer panicked: %v", p)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("consumer did not return after cancellation")
	}
}
