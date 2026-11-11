package upstream

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"sitia.nu/airgap/src/internal/productiontest"
)

func observeMemory(controller *memoryController, sample memorySample, now time.Time) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.observeLocked(sample, now)
}

func TestMemoryStallIntervalConfiguration(t *testing.T) {
	c := DefaultConfiguration()
	if c.memoryStallStatusIntervalSeconds != 60 {
		t.Fatalf("default interval = %d, want 60", c.memoryStallStatusIntervalSeconds)
	}
	path := filepath.Join(t.TempDir(), "upstream.properties")
	if err := os.WriteFile(path, []byte("memoryStallStatusIntervalSeconds=120\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := ReadParameters(path, c)
	if err != nil || c.memoryStallStatusIntervalSeconds != 120 {
		t.Fatalf("properties interval = %d, error = %v", c.memoryStallStatusIntervalSeconds, err)
	}
	t.Setenv("AIRGAP_UPSTREAM_MEMORY_STALL_STATUS_INTERVAL_SECONDS", "180")
	c = overrideConfiguration(c)
	if c.memoryStallStatusIntervalSeconds != 180 {
		t.Fatalf("environment interval = %d", c.memoryStallStatusIntervalSeconds)
	}
	c = parseCommandLineOverrides([]string{"upstream", "--memoryStallStatusIntervalSeconds=0"}, c)
	if c.memoryStallStatusIntervalSeconds != 0 {
		t.Fatalf("CLI disable = %d", c.memoryStallStatusIntervalSeconds)
	}
	if err := validateMemoryStallInterval(int(math.MaxInt64 / int64(time.Second))); err != nil {
		t.Fatalf("rejected maximum supported duration: %v", err)
	}
	for _, seconds := range []int{-1, int(math.MaxInt64/int64(time.Second)) + 1} {
		if err := validateMemoryStallInterval(seconds); err == nil {
			t.Errorf("accepted invalid interval %d", seconds)
		}
		c.memoryStallStatusIntervalSeconds = seconds
		if _, err := NewKafkaAdapter(c); err == nil {
			t.Errorf("adapter accepted invalid interval %d", seconds)
		}
	}
}

func TestMemoryStallInvalidIntervalFailsAtStartup(t *testing.T) {
	for _, interval := range []string{"-1", "9223372037"} {
		t.Run(interval, func(t *testing.T) {
			nic := "lo0"
			if runtime.GOOS == "linux" {
				nic = "lo"
			}
			path := filepath.Join(t.TempDir(), "upstream.properties")
			content := "id=invalid-stall\nnic=" + nic + "\nsource=kafka\nbootstrapServers=127.0.0.1:1\n" +
				"topic=transfer\ngroupID=test\ntargetIP=127.0.0.1\ntargetPort=1234\n" +
				"memoryStallStatusIntervalSeconds=" + interval + "\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProductionWarningsProcessHelper$")
			cmd.Env = append(os.Environ(), "AIRGAP_PRODUCTION_HELPER="+path)
			output, err := cmd.CombinedOutput()
			if err == nil || ctx.Err() != nil {
				t.Fatalf("invalid interval must fail promptly: error=%v context=%v\n%s", err, ctx.Err(), output)
			}
			if !strings.Contains(string(output), "memoryStallStatusIntervalSeconds") ||
				strings.Contains(string(output), "Starting a new Sarama consumer") {
				t.Fatalf("expected actionable interval error before Kafka startup:\n%s", output)
			}
		})
	}
}

func TestMemoryStallReportsOncePerIntervalAcrossClaimsAndResets(t *testing.T) {
	controller := newMemoryController(nil, 80, 70, time.Millisecond, nil)
	start := time.Now()
	sample := memorySample{current: 85, limit: 100, source: "test"}
	observeMemory(controller, sample, start)
	if _, ok := controller.stallNotice(start.Add(59*time.Second), time.Minute); ok {
		t.Fatal("reported before first full interval")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	reports := 0
	for range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := controller.stallNotice(start.Add(time.Minute), time.Minute); ok {
				mu.Lock()
				reports++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if reports != 1 {
		t.Fatalf("60 concurrent claims reported %d times, want once", reports)
	}
	if _, ok := controller.stallNotice(start.Add(119*time.Second), time.Minute); ok {
		t.Fatal("repeated before next interval")
	}
	notice, ok := controller.stallNotice(start.Add(2*time.Minute), time.Minute)
	if !ok || notice.duration != 2*time.Minute || notice.sample != sample {
		t.Fatalf("second notice = %+v, present=%v", notice, ok)
	}
	observeMemory(controller, memorySample{current: 69, limit: 100}, start.Add(121*time.Second))
	if _, ok := controller.stallNotice(start.Add(3*time.Minute), time.Minute); ok {
		t.Fatal("reported after recovery")
	}
	observeMemory(controller, sample, start.Add(4*time.Minute))
	if _, ok := controller.stallNotice(start.Add(4*time.Minute+59*time.Second), time.Minute); ok {
		t.Fatal("new pause inherited the previous duration")
	}
	if _, ok := controller.stallNotice(start.Add(5*time.Minute), 0); ok {
		t.Fatal("disabled status interval reported")
	}
	if _, ok := controller.stallNotice(start.Add(5*time.Minute), time.Minute); !ok {
		t.Fatal("new pause failed to report after full interval")
	}
}

func TestMemoryStallStatusUsesStatusFramesAndPropagatesFailures(t *testing.T) {
	c := DefaultConfiguration()
	c.id, c.payloadSize = "stall-test", 1400
	notice := memoryStallNotice{duration: 120 * time.Second, sample: memorySample{current: 850, limit: 1000, source: "cgroup.v2"}}
	message := memoryStallMessage(c, notice)
	for _, want := range []string{"stall-test", "120", "850", "1000", "700", "cgroup.v2"} {
		if !strings.Contains(message, want) {
			t.Errorf("status missing %q: %s", want, message)
		}
	}
	for _, payloadSize := range []uint16{1400, 64} {
		c.payloadSize = payloadSize
		transport := &captureTransport{}
		if err := sendMemoryStallStatus(transport, c, message); err != nil {
			t.Fatal(err)
		}
		payloads := capturedStatusPayloads(t, transport)
		if len(payloads) != 1 || payloads[0] != message {
			t.Fatalf("STATUS payloads = %v", payloads)
		}
		if payloadSize == 64 && len(transport.messages) < 2 {
			t.Fatal("small payload size did not exercise STATUS fragmentation")
		}
	}
	transport := &captureTransport{err: errors.New("transport unavailable")}
	if err := sendMemoryStallStatus(transport, c, message); !errors.Is(err, transport.err) {
		t.Fatalf("delivery failure was hidden: %v", err)
	}
}

type blockedStallTransport struct {
	captureTransport
	started chan struct{}
	release chan struct{}
}

func (t *blockedStallTransport) SendMessages([][]byte) error {
	close(t.started)
	<-t.release
	return errors.New("closed")
}

func TestMemoryStallMonitorKeepsLoggingAndCancelsWithBlockedTransport(t *testing.T) {
	controller := newMemoryController(nil, 80, 70, time.Millisecond, nil)
	observeMemory(controller, memorySample{current: 85, limit: 100, source: "test"}, time.Now().Add(-time.Minute))
	transport := &blockedStallTransport{started: make(chan struct{}), release: make(chan struct{})}
	defer close(transport.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	c := DefaultConfiguration()
	c.payloadSize = 1400
	output := productiontest.Capture(t, "INFO", func() {
		go func() {
			defer close(done)
			monitorMemoryStalls(ctx, controller, transport, c, time.Minute, ticks)
		}()
		now := time.Now()
		ticks <- now
		select {
		case <-transport.started:
		case <-time.After(time.Second):
			t.Fatal("STATUS was not sent")
		}
		ticks <- now.Add(time.Minute)
		// Receiving this tick proves that the previous one finished logging.
		ticks <- now.Add(time.Minute)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("blocked STATUS delivery prevented shutdown")
		}
	})
	if strings.Count(output, "Kafka admission stalled for") != 2 {
		t.Fatalf("local stall alerts did not repeat while transport blocked:\n%s", output)
	}
	if !strings.Contains(output, "previous STATUS delivery is still in progress") {
		t.Fatalf("missing explicit blocked-delivery diagnostic:\n%s", output)
	}
}
