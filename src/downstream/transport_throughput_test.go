package downstream

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"sitia.nu/airgap/src/protocol"
	"sitia.nu/airgap/src/upstream"
)

// TC-51 isolates transport throughput from Kafka, rate limits and deduplication.
func TestTransportThroughput(t *testing.T) {
	if os.Getenv("AIRGAP_TRANSPORT_PERF") != "1" {
		t.Skip("run TC-51 to measure transport throughput")
	}
	const count = 20000
	const minEPS = 5000
	for _, mode := range []string{"tcp", "tls12", "tls13"} {
		t.Run(mode, func(t *testing.T) {
			receiver := &TCPAdapter{maxConnections: 1}
			var clientTLS *tls.Config
			if mode != "tcp" {
				server := httptest.NewTLSServer(nil)
				cert := server.TLS.Certificates[0]
				server.Close()
				leaf, err := x509.ParseCertificate(cert.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				roots := x509.NewCertPool()
				roots.AddCert(leaf)
				version := uint16(tls.VersionTLS13)
				if mode == "tls12" {
					version = tls.VersionTLS12
				}
				receiver.tlsConfig = &tls.Config{
					Certificates: []tls.Certificate{cert},
					MinVersion:   version, MaxVersion: version,
				}
				clientTLS = &tls.Config{
					RootCAs: roots, ServerName: "example.com",
					MinVersion: version, MaxVersion: version,
				}
			}
			payload := bytes.Repeat([]byte("x"), 256)
			cache := protocol.CreateMessageCache()
			done := make(chan struct{})
			var received int
			var validationErr error
			receiver.Listen("127.0.0.1", 0, 0, func(frame []byte) {
				_, id, value, err := protocol.ParseMessage(frame, cache)
				if validationErr == nil {
					if err != nil {
						validationErr = err
					} else if id != fmt.Sprintf("perf_0_%d", received) || !bytes.Equal(value, payload) {
						validationErr = fmt.Errorf("record %d: unexpected ID %q or payload", received, id)
					}
				}
				received++
				if received == count {
					close(done)
				}
			}, 1500, make(chan struct{}), 1)
			defer receiver.Close()
			sender, err := upstream.NewTCPAdapter(receiver.listener.Addr().String(), clientTLS)
			if err != nil {
				t.Fatal(err)
			}
			defer sender.Close()
			frames := make([][][]byte, count)
			for i := range frames {
				frames[i] = protocol.FormatMessage(protocol.TYPE_CLEARTEXT, "perf_0_"+strconv.Itoa(i), payload, 1500)
			}
			start := time.Now()
			for _, messages := range frames {
				if err := sender.SendMessages(messages); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("receiver did not deliver all records")
			}
			elapsed := time.Since(start)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			eps := float64(count) / elapsed.Seconds()
			t.Logf("%s: %d records, 256-byte payload, %s, %.0f EPS (minimum %d)",
				strings.ToUpper(mode), count, elapsed, eps, minEPS)
			if eps < minEPS {
				t.Errorf("throughput %.0f EPS is below %d; check for per-frame blocking", eps, minEPS)
			}
		})
	}
}
