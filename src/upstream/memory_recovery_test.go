package upstream

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDefaultMemoryLimitLeavesRecoveryHeadroom(t *testing.T) {
	config := DefaultConfiguration()
	if config.goMemoryLimitPercent >= config.memoryLowWatermarkPercent {
		t.Fatalf("Go memory target %d%% must be below resume watermark %d%% to leave room for non-Go memory",
			config.goMemoryLimitPercent, config.memoryLowWatermarkPercent)
	}
}

func TestUnsafeGoMemoryTargetsFailAtStartup(t *testing.T) {
	for _, target := range []string{"70", "80", "100"} {
		t.Run(target, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upstream.properties")
			content := "id=invalid-memory\nnic=lo0\nsource=kafka\nbootstrapServers=127.0.0.1:1\n" +
				"topic=transfer\ngroupID=test\ntargetIP=127.0.0.1\ntargetPort=1234\n" +
				"memoryLowWatermarkPercent=70\ngoMemoryLimitPercent=" + target + "\n"
			if runtime.GOOS == "linux" {
				content = strings.ReplaceAll(content, "nic=lo0", "nic=lo")
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProductionWarningsProcessHelper$")
			cmd.Env = append(os.Environ(), "AIRGAP_PRODUCTION_HELPER="+path)
			output, err := cmd.CombinedOutput()
			if err == nil || ctx.Err() != nil {
				t.Fatalf("unsafe target must fail promptly at startup: error=%v context=%v\n%s", err, ctx.Err(), output)
			}
			if !strings.Contains(string(output), "must be below memoryLowWatermarkPercent") {
				t.Fatalf("missing actionable headroom error:\n%s", output)
			}
			if strings.Contains(string(output), "Starting a new Sarama consumer") {
				t.Fatalf("invalid memory configuration reached Kafka startup:\n%s", output)
			}
		})
	}
}

func TestGoMemoryHeadroomValidation(t *testing.T) {
	for _, tt := range []struct {
		percent int
		low     int
		valid   bool
	}{
		{60, 70, true},
		{69, 70, true},
		{0, 70, true},
		{70, 70, false},
		{80, 70, false},
		{-1, 70, false},
		{101, 70, false},
	} {
		err := validateGoMemoryHeadroom(tt.percent, tt.low)
		if (err == nil) != tt.valid {
			t.Errorf("Go target %d, low watermark %d: error=%v, want valid=%v", tt.percent, tt.low, err, tt.valid)
		}
	}
	config := DefaultConfiguration()
	config.goMemoryLimitPercent = config.memoryLowWatermarkPercent
	if _, err := NewKafkaAdapter(config); err == nil {
		t.Fatal("adapter accepted a Go memory target equal to the resume watermark")
	}
}

type heapMemorySampler struct{ limit int64 }

func (s heapMemorySampler) Sample() (memorySample, error) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return memorySample{current: int64(stats.HeapAlloc), limit: s.limit, source: "test.heap"}, nil
}

//go:noinline
func allocateIdleHeap(bytes int) {
	allocation := make([]byte, bytes)
	for offset := 0; offset < len(allocation); offset += 4096 {
		allocation[offset] = 1
	}
	runtime.KeepAlive(allocation)
}

func disableAutomaticGC(t *testing.T) {
	t.Helper()
	previousLimit := debug.SetMemoryLimit(math.MaxInt64)
	previousGC := debug.SetGCPercent(-1)
	t.Cleanup(func() {
		debug.SetGCPercent(previousGC)
		debug.SetMemoryLimit(previousLimit)
		debug.FreeOSMemory()
	})
	debug.FreeOSMemory()
}

func TestMemoryControllerReclaimsIdleHeapBeforeResuming(t *testing.T) {
	disableAutomaticGC(t)
	const limit = 80 * 1024 * 1024
	allocateIdleHeap(72 * 1024 * 1024)
	controller := newMemoryController(heapMemorySampler{limit: limit}, 80, 70, time.Millisecond, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := controller.Wait(ctx); err != nil {
		t.Fatalf("idle heap kept admission paused instead of being reclaimed: %v", err)
	}
	sample, err := controller.sampler.Sample()
	if err != nil || !belowPercent(sample.current, sample.limit, 70) {
		t.Fatalf("resumed without reaching low watermark: sample=%+v error=%v", sample, err)
	}
}

func TestMemoryControllerSharesReclamationAcrossClaims(t *testing.T) {
	disableAutomaticGC(t)
	sampler := &sequenceMemorySampler{samples: []memorySample{{current: 90, limit: 100, source: "external"}}}
	controller := newMemoryController(sampler, 80, 70, time.Millisecond, nil)
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := controller.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("unreclaimable pressure must stay paused until cancellation, got %v", err)
			}
		}()
	}
	wg.Wait()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if collections := after.NumForcedGC - before.NumForcedGC; collections != 1 {
		t.Fatalf("60 concurrent claims performed %d forced collections, want one shared, rate-limited reclamation", collections)
	}
}
