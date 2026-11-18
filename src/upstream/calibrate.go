package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/protocol"
	"sitia.nu/airgap/src/udp"
)

const calibrationFrameSize = 1400

var calibrationTopicPattern = regexp.MustCompile(`^airgap-calibration(-[a-zA-Z0-9.-]{1,80})?$`)

func calibrateUDP(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("upstream --calibrate", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	address := flags.String("address", "", "downstream UDP host:port")
	nic := flags.String("nic", "", "optional upstream network interface")
	topic := flags.String("topic", "", "dedicated downstream Kafka topic: airgap-calibration or airgap-calibration-NAME")
	brokers := flags.String("brokers", "", "comma-separated downstream Kafka bootstrap addresses")
	ca := flags.String("kafka-ca-file", "", "enable Kafka TLS with this CA PEM")
	cert := flags.String("kafka-cert-file", "", "optional Kafka client certificate PEM")
	key := flags.String("kafka-key-file", "", "optional Kafka client key PEM")
	password := flags.String("kafka-key-password-file", "", "optional Kafka client key passphrase file")
	rates := flags.String("rates", "12000,24000,40000,60000,85000", "increasing UDP datagram rates to test")
	duration := flags.Duration("duration", 10*time.Second, "send duration per trial (1s..30s)")
	drain := flags.Duration("drain", 3*time.Second, "Kafka observation time after sending (1s..30s)")
	burst := flags.Int("burst", 32, "UDP burst in datagrams (1..256)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	host, portText, err := net.SplitHostPort(*address)
	port, portErr := strconv.Atoi(portText)
	if flags.NArg() != 0 || err != nil || portErr != nil || host == "" || port < 1 || port > 65535 ||
		!calibrationTopicPattern.MatchString(*topic) || *brokers == "" || *duration < time.Second || *duration > 30*time.Second ||
		*drain < time.Second || *drain > 30*time.Second || *burst < 1 || *burst > 256 {
		return fmt.Errorf("calibration requires --address=HOST:PORT, --topic=airgap-calibration[-NAME], --brokers, valid time and burst limits, and no positional arguments")
	}
	var candidates []int
	for _, s := range strings.Split(*rates, ",") {
		rate, err := strconv.Atoi(s)
		if err != nil || rate < 1000 || rate > 200000 || len(candidates) >= 10 ||
			(len(candidates) > 0 && rate <= candidates[len(candidates)-1]) {
			return fmt.Errorf("--rates must contain up to ten strictly increasing values in 1000..200000")
		}
		candidates = append(candidates, rate)
	}
	conf := sarama.NewConfig()
	conf.Version = sarama.V3_6_0_0
	conf.ClientID = "airgap-calibration"
	conf.Metadata.AllowAutoTopicCreation = false
	conf.Metadata.Retry.Max = 1
	conf.Net.DialTimeout, conf.Net.ReadTimeout, conf.Net.WriteTimeout = 5*time.Second, 5*time.Second, 5*time.Second
	conf.Consumer.Return.Errors = true
	conf.Consumer.IsolationLevel = sarama.ReadCommitted
	if (*cert == "") != (*key == "") || (*password != "" && *key == "") || (*cert != "" && *ca == "") {
		return fmt.Errorf("Kafka client TLS requires --kafka-ca-file and a certificate/key pair")
	}
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return fmt.Errorf("no certificates found in Kafka CA file")
		}
		conf.Net.TLS.Enable = true
		conf.Net.TLS.Config = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		if *cert != "" {
			pair, err := kafka.LoadTLSCertificate(*cert, *key, *password)
			if err != nil {
				return err
			}
			conf.Net.TLS.Config.Certificates = []tls.Certificate{pair}
		}
	}
	client, err := sarama.NewClient(strings.Split(*brokers, ","), conf)
	if err != nil {
		return fmt.Errorf("connect to verification Kafka: %w", err)
	}
	defer client.Close()
	partitions, err := client.Partitions(*topic)
	if err != nil {
		return fmt.Errorf("read pre-created verification topic %q: %w", *topic, err)
	}
	if len(partitions) == 0 || len(partitions) > 64 {
		return fmt.Errorf("verification topic must have 1..64 partitions")
	}
	conn, err := udp.NewUDPConnWithNIC(*address, *nic)
	if err != nil {
		return err
	}
	defer conn.Close()
	probe := calibrationTrial{topic: *topic, rate: 1000, burst: 1, duration: time.Second, drain: *drain, partitions: partitions, limit: len(partitions)}
	sent, received, _, _, err := probe.run(ctx, conn, client)
	if err != nil {
		return fmt.Errorf("routing probe: %w", err)
	}
	if sent != len(partitions) || sent != received {
		return fmt.Errorf("routing probe verified %d/%d partitions; check downstream UDP readiness and topic routing", received, len(partitions))
	}
	trial := func(rate int) (bool, int, error) {
		c := calibrationTrial{topic: *topic, rate: rate, burst: *burst, duration: *duration, drain: *drain, partitions: partitions}
		sent, received, datagrams, elapsed, err := c.run(ctx, conn, client)
		achieved := 0
		if elapsed > 0 {
			achieved = int(float64(datagrams) / elapsed.Seconds())
		}
		fmt.Fprintf(diagnostics, "calibration: cap=%d datagrams/s measured=%d datagrams/s sent=%d verified=%d\n",
			rate, achieved, sent, received)
		return sent > 0 && sent == received, achieved, err
	}
	var recommended int
	for _, rate := range candidates {
		passed, achieved, err := trial(rate)
		if err != nil {
			return err
		}
		if !passed {
			break
		}
		if achieved > rate {
			achieved = rate
		}
		recommended = max(recommended, achieved*80/100)
	}
	if recommended == 0 {
		return fmt.Errorf("no loss-free calibration trial; verify topic routing and try lower rates")
	}
	passed, _, err := trial(recommended)
	if err != nil {
		return err
	}
	if !passed {
		return fmt.Errorf("confirmation at %d datagrams/s lost records; no settings recommended", recommended)
	}
	_, err = fmt.Fprintf(out, "udpPacingDatagramsPerSecond=%d\nudpPacingBytesPerSecond=%d\nudpPacingBurstDatagrams=%d\n",
		recommended, recommended*calibrationFrameSize, *burst)
	return err
}

type calibrationTrial struct {
	topic           string
	rate, burst     int
	duration, drain time.Duration
	partitions      []int32
	limit           int
}

func (c calibrationTrial) run(ctx context.Context, conn *udp.UDPConn, client sarama.Client) (sent, received, datagrams int, elapsed time.Duration, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	consumer, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer consumer.Close()

	readCtx, stop := context.WithCancel(ctx)
	var readers sync.WaitGroup
	defer func() { stop(); readers.Wait() }()
	failures := make(chan error, 1)
	fail := func(err error) {
		select {
		case failures <- err:
			stop()
		default:
		}
	}
	var payloadTemplate [1600]byte
	if _, err := rand.Read(payloadTemplate[:]); err != nil {
		return 0, 0, 0, 0, err
	}
	base := 1_000_000_000_000_000 + binary.BigEndian.Uint64(payloadTemplate[:8])%8_000_000_000_000_000
	seen := make([]bool, c.rate*int(c.duration/time.Second+1)+c.burst+1)
	var mu sync.Mutex
	verified := 0
	highest := -1
	prefixes := make([]string, len(c.partitions))
	for i, partition := range c.partitions {
		prefix := fmt.Sprintf("%s_%d_", c.topic, partition)
		prefixes[i] = prefix
		startOffset, err := client.GetOffset(c.topic, partition, sarama.OffsetNewest)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		pc, err := consumer.ConsumePartition(c.topic, partition, startOffset)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case msg, ok := <-pc.Messages():
					if !ok {
						fail(fmt.Errorf("verification partition %d closed unexpectedly", partition))
						return
					}
					offsetText, ok := strings.CutPrefix(string(msg.Key), prefix)
					offset, err := strconv.ParseUint(offsetText, 10, 64)
					if !ok || err != nil || offset < base || offset-base >= uint64(len(seen)) {
						fail(fmt.Errorf("unexpected record in verification topic %q; keep it isolated", c.topic))
						return
					}
					seq := int(offset - base)
					if seq%len(c.partitions) != i || len(msg.Value) != 400+seq%1201 ||
						binary.BigEndian.Uint64(msg.Value[:8]) != offset || !bytes.Equal(msg.Value[8:], payloadTemplate[8:len(msg.Value)]) {
						fail(fmt.Errorf("corrupt calibration record %d", seq))
						return
					}
					mu.Lock()
					if seen[seq] {
						mu.Unlock()
						fail(fmt.Errorf("duplicate calibration record %d", seq))
						return
					}
					seen[seq] = true
					verified++
					highest = max(highest, seq)
					mu.Unlock()
				case failure, ok := <-pc.Errors():
					if ok {
						fail(failure)
					} else {
						fail(fmt.Errorf("verification partition %d stopped", partition))
					}
					return
				case <-readCtx.Done():
					return
				}
			}
		}()
	}

	if err := conn.ConfigurePacing(readCtx, c.rate, c.rate*calibrationFrameSize, c.burst); err != nil {
		return 0, 0, 0, 0, err
	}
	var payload = payloadTemplate
	start := time.Now()
	for readCtx.Err() == nil && time.Since(start) < c.duration && (c.limit == 0 || sent < c.limit) {
		id := prefixes[sent%len(c.partitions)] + strconv.FormatUint(base+uint64(sent), 10)
		binary.BigEndian.PutUint64(payload[:8], base+uint64(sent))
		frames, formatErr := protocol.FormatMessageChecked(protocol.TYPE_CLEARTEXT, id, payload[:400+sent%1201], calibrationFrameSize)
		if formatErr != nil {
			err = formatErr
			break
		}
		if err = conn.SendMessages(frames); err != nil {
			break
		}
		sent++
		datagrams += len(frames)
	}
	elapsed = time.Since(start)
	if err == nil && readCtx.Err() == nil {
		timer := time.NewTimer(c.drain)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-readCtx.Done():
		}
	}
	stop()
	readers.Wait()
	received = verified
	select {
	case failure := <-failures:
		err = failure
	default:
		if err == nil {
			err = ctx.Err()
		}
	}
	if err == nil && highest >= sent {
		err = fmt.Errorf("received calibration records that were not sent")
	}
	return
}
