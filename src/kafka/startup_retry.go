package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/logging"
)

// DefaultStartupRetryInterval is the delay after a failed Kafka client
// startup attempt when no interval is configured.
const DefaultStartupRetryInterval = 5 * time.Second

// RetryableStartupError reports whether a Kafka client startup error is
// transient (broker not running or not ready, network failure). Configuration,
// TLS certificate and authentication/authorization errors are permanent.
func RetryableStartupError(err error) bool {
	var configError sarama.ConfigurationError
	var certificateError *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	if errors.As(err, &configError) || errors.As(err, &certificateError) ||
		errors.As(err, &unknownAuthority) || errors.As(err, &hostnameError) ||
		errors.As(err, &invalidCertificate) {
		return false
	}
	for _, permanent := range []error{
		sarama.ErrSASLAuthenticationFailed, sarama.ErrUnsupportedSASLMechanism,
		sarama.ErrTopicAuthorizationFailed, sarama.ErrGroupAuthorizationFailed,
		sarama.ErrClusterAuthorizationFailed, sarama.ErrUnsupportedVersion,
		sarama.ErrInvalidRequest,
	} {
		if errors.Is(err, permanent) {
			return false
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, sarama.ErrOutOfBrokers) {
		return true
	}
	for _, transient := range []error{
		sarama.ErrBrokerNotAvailable, sarama.ErrLeaderNotAvailable,
		sarama.ErrNotLeaderForPartition, sarama.ErrRequestTimedOut,
		sarama.ErrNetworkException, sarama.ErrOffsetsLoadInProgress,
		sarama.ErrConsumerCoordinatorNotAvailable, sarama.ErrNotCoordinatorForConsumer,
		sarama.ErrReplicaNotAvailable,
	} {
		if errors.Is(err, transient) {
			return true
		}
	}
	return false
}

// startupAttempt runs create but returns as soon as ctx is canceled. Sarama's
// constructors take no context; an attempt still in flight at cancellation
// finishes on its own timeouts and closes whatever it created.
func startupAttempt[T io.Closer](ctx context.Context, create func() (T, error)) (T, error) {
	type result struct {
		client T
		err    error
	}
	completed := make(chan result)
	go func() {
		client, err := create()
		select {
		case completed <- result{client, err}:
		case <-ctx.Done():
			if err == nil {
				if closeErr := client.Close(); closeErr != nil {
					Logger.Errorf(logging.Event(logging.EventKafkaConnect).Merge(logging.Err(closeErr)),
						"Closing Kafka client after startup cancellation: %v", closeErr)
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case r := <-completed:
		return r.client, r.err
	}
}

// startWithRetry calls create until it succeeds, ctx is canceled, or it fails
// with an error that RetryableStartupError classifies as permanent. onRetry is
// called after each transient failure, before waiting interval.
func startWithRetry[T io.Closer](ctx context.Context, interval time.Duration,
	create func() (T, error), onRetry func(attempt int64, err error),
) (T, error) {
	if interval <= 0 {
		interval = DefaultStartupRetryInterval
	}
	var zero T
	for attempt := int64(1); ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		client, err := startupAttempt(ctx, create)
		if err == nil {
			return client, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !RetryableStartupError(err) {
			return zero, err
		}
		onRetry(attempt, err)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
}
