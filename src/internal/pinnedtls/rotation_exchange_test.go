package pinnedtls

import (
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"
)

func runRotationFixture(t *testing.T, handler func(*tls.Conn), manager *Manager, old, next *Snapshot) error {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", manager.ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	serverErrors := make(chan error, 1)
	go func() {
		defer close(done)
		raw, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer raw.Close()
		conn := raw.(*tls.Conn)
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if err := conn.Handshake(); err != nil {
			serverErrors <- err
			return
		}
		handler(conn)
	}()
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: old.config.Certificates,
		InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 || Fingerprint(state.PeerCertificates[0].RawSubjectPublicKeyInfo) != old.Identity() {
				return fmt.Errorf("unexpected fixture server key")
			}
			return nil
		},
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", listener.Addr().String(), config)
	if err != nil {
		listener.Close()
		<-done
		t.Fatal(err)
	}
	_, requestErr := RequestRotation(conn, config, next)
	conn.Close()
	<-done
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
	return requestErr
}

func TestRotationChallengeTimeBoundaries(t *testing.T) {
	for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(delta.String(), func(t *testing.T) {
			manager, dir, _, old, next := rotationFixture(t)
			serverFailure := make(chan error, 1)
			err := runRotationFixture(t, func(conn *tls.Conn) {
				start := time.Unix(1800000000, 0)
				now := start
				session := RotationSession{now: func() time.Time { return now }}
				begin, err := ReadRotation(conn)
				if err != nil {
					serverFailure <- err
					return
				}
				challenge := manager.HandleRotation(&session, conn, begin, dir, true)
				if err := WriteRotation(conn, challenge); err != nil {
					serverFailure <- err
					return
				}
				request, err := ReadRotation(conn)
				if err != nil {
					serverFailure <- err
					return
				}
				now = start.Add(30*time.Second + delta)
				response := manager.HandleRotation(&session, conn, request, dir, true)
				if err := WriteRotation(conn, response); err != nil {
					serverFailure <- err
				}
			}, manager, old, next)
			select {
			case err := <-serverFailure:
				t.Fatal(err)
			default:
			}
			if (err != nil) != (delta > 0) {
				t.Fatalf("delta=%v rotation error=%v", delta, err)
			}
		})
	}
}

func TestRotationClientRejectsUnboundAcknowledgments(t *testing.T) {
	for _, name := range []string{"id", "new", "peer", "version", "public", "unsolicited"} {
		t.Run(name, func(t *testing.T) {
			manager, dir, _, old, next := rotationFixture(t)
			serverFailure := make(chan error, 1)
			err := runRotationFixture(t, func(conn *tls.Conn) {
				var session RotationSession
				begin, err := ReadRotation(conn)
				if err != nil {
					serverFailure <- err
					return
				}
				challenge := manager.HandleRotation(&session, conn, begin, dir, true)
				if name == "unsolicited" {
					challenge.Kind, challenge.Nonce = "accepted", ""
					if err := WriteRotation(conn, challenge); err != nil {
						serverFailure <- err
					}
					return
				}
				if err := WriteRotation(conn, challenge); err != nil {
					serverFailure <- err
					return
				}
				request, err := ReadRotation(conn)
				if err != nil {
					serverFailure <- err
					return
				}
				response := manager.HandleRotation(&session, conn, request, dir, true)
				switch name {
				case "id":
					response.ID = "unrelated"
				case "new":
					response.New = old.Identity()
				case "peer":
					response.Peer = "another-peer"
				case "version":
					response.Version++
				case "public":
					response.Public = request.Public
				}
				if err := WriteRotation(conn, response); err != nil {
					serverFailure <- err
				}
			}, manager, old, next)
			select {
			case err := <-serverFailure:
				t.Fatal(err)
			default:
			}
			if err == nil {
				t.Fatal("unbound acknowledgment accepted by client")
			}
		})
	}
}
