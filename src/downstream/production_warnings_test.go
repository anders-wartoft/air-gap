package downstream

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"sitia.nu/airgap/src/internal/productiontest"
)

func TestProductionWarningsProcessHelper(t *testing.T) {
	if path := os.Getenv("AIRGAP_PRODUCTION_HELPER"); path != "" {
		os.Args = []string{"downstream", path}
		Main("test")
		os.Exit(0)
	}
}

func TestProductionWarningsLifecycle(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper",
				"id=test\nnic=lo0\ntarget=null\ntargetIP=127.0.0.1\ntargetPort=0\nmtu=1500\nlogLevel=ERROR\n", signal)
		})
	}
	t.Run("file logging", func(t *testing.T) {
		productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper",
			"id=test\nnic=lo0\ntarget=null\ntargetIP=127.0.0.1\ntargetPort=0\nmtu=1500\nlogLevel=ERROR\n", syscall.SIGTERM, true)
	})
}

func TestProductionWarningsCleanupClosesIdleTCPConnections(t *testing.T) {
	receiver := &TCPAdapter{maxConnections: 10}
	stop := make(chan struct{})
	receiver.Listen("127.0.0.1", 0, 0, func([]byte) {}, 1500, stop, 1)
	defer receiver.Close()
	conn, err := net.Dial("tcp", receiver.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for receiver.activeConns.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("TCP connection was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		receiver.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TCP cleanup did not join its idle connection")
	}
	if got := receiver.activeConns.Load(); got != 0 {
		t.Fatalf("TCP cleanup left %d active connections", got)
	}
}

func productionWarningFixture() TransferConfiguration {
	c := defaultConfiguration()
	c.id, c.nic, c.targetIP, c.targetPort = "downstream", "eth0", "0.0.0.0", 1234
	c.bootstrapServers = "broker:9093"
	c.transport, c.tcpTLSCertFile, c.tcpTLSKeyFile = "tcp", "server.crt", "server.key"
	c.tcpTLSCAFile, c.tcpTLSClientAuth = "transport-ca.crt", "require"
	c.certFile, c.keyFile, c.caFile = "client.crt", "client.key", "kafka-ca.crt"
	c.logStatistics = 30
	return c
}

func TestProductionWarnings(t *testing.T) {
	tests := []struct {
		name   string
		change func(*TransferConfiguration)
		want   map[string]string
	}{
		{"nominal", func(c *TransferConfiguration) {}, nil},
		{"console", func(c *TransferConfiguration) { c.target = "cmd" }, map[string]string{"target": "cmd"}},
		{"discard", func(c *TransferConfiguration) { c.target = "null" }, map[string]string{"target": "null"}},
		{"debug", func(c *TransferConfiguration) { c.logLevel = "DEBUG" }, map[string]string{"logLevel": "DEBUG"}},
		{"trace", func(c *TransferConfiguration) { c.logLevel = "TRACE" }, map[string]string{"logLevel": "TRACE"}},
		{"no statistics", func(c *TransferConfiguration) { c.logStatistics = 0 }, map[string]string{"logStatistics": "0"}},
		{"small queue", func(c *TransferConfiguration) { c.channelBufferSize = 16383 }, map[string]string{"channelBufferSize": "16383"}},
		{"small decompression", func(c *TransferConfiguration) { c.maximumDecompressSize = 1048575 }, map[string]string{"maximumDecompressSize": "1048575"}},
		{"decompression floor", func(c *TransferConfiguration) { c.maximumDecompressSize = 1048576 }, nil},
		{"plaintext Kafka", func(c *TransferConfiguration) { c.caFile = "" }, map[string]string{"caFile": ""}},
		{"inactive Kafka", func(c *TransferConfiguration) {
			c.target, c.caFile = "null", ""
		}, map[string]string{"target": "null"}},
		{"plaintext TCP", func(c *TransferConfiguration) {
			c.tcpTLSCertFile, c.tcpTLSKeyFile = "", ""
		}, map[string]string{"tcpTLSCertFile": ""}},
		{"no client authentication", func(c *TransferConfiguration) { c.tcpTLSClientAuth = "none" }, map[string]string{"tcpTLSClientAuth": "none"}},
		{"optional client authentication", func(c *TransferConfiguration) { c.tcpTLSClientAuth = "allow" }, map[string]string{"tcpTLSClientAuth": "allow"}},
		{"UDP buffer floor", func(c *TransferConfiguration) { c.transport = "udp" }, nil},
		{"small UDP socket buffer", func(c *TransferConfiguration) {
			c.transport, c.rcvBufSize = "udp", 4194303
		}, map[string]string{"rcvBufSize": "4194303"}},
		{"small UDP read buffer", func(c *TransferConfiguration) {
			c.transport, c.readBufferMultiplier = "udp", 15
		}, map[string]string{"readBufferMultiplier": "15"}},
		{"TCP ignores UDP buffers", func(c *TransferConfiguration) { c.rcvBufSize, c.readBufferMultiplier = 1, 1 }, nil},
		{"UDP ignores TCP auth", func(c *TransferConfiguration) {
			c.transport, c.tcpTLSClientAuth, c.tcpTLSCertFile = "udp", "none", ""
		}, nil},
		{"kernel monitoring", func(c *TransferConfiguration) { c.enableRxqOvfl = true }, nil},
		{"combined rollout", func(c *TransferConfiguration) {
			c.target, c.logLevel, c.logStatistics = "null", "DEBUG", 0
			c.transport, c.rcvBufSize, c.readBufferMultiplier = "udp", 1024, 1
			c.channelBufferSize, c.maximumDecompressSize = 1, 1024
		}, map[string]string{
			"target": "null", "logLevel": "DEBUG", "logStatistics": "0",
			"rcvBufSize": "1024", "readBufferMultiplier": "1",
			"channelBufferSize": "1", "maximumDecompressSize": "1024",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := productionWarningFixture()
			tt.change(&c)
			productiontest.Check(t, &c, tt.want)
		})
	}
}

func TestProductionWarningsAtEndOfCheckConfiguration(t *testing.T) {
	c := productionWarningFixture()
	c.channelBufferSize = 16383
	productiontest.CheckStartup(t, func() { checkConfiguration(c) }, map[string]string{"channelBufferSize": "16383"})
}
