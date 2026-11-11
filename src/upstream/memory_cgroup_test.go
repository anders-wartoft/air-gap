package upstream

import (
	"context"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

//go:noinline
func fillIdleCgroupHeap(t *testing.T, sampler memorySampler) {
	t.Helper()
	var allocations [][]byte
	for {
		sample, err := sampler.Sample()
		if err != nil {
			t.Fatal(err)
		}
		if atOrAbovePercent(sample.current, sample.limit, 83) {
			runtime.KeepAlive(allocations)
			return
		}
		allocation := make([]byte, 8*1024*1024)
		for offset := 0; offset < len(allocation); offset += 4096 {
			allocation[offset] = 1
		}
		allocations = append(allocations, allocation)
	}
}

func TestDockerMemoryThrottleReclaimsOwnHeapForAllClaims(t *testing.T) {
	if os.Getenv("TC47_CGROUP_TEST") != "1" {
		t.Skip("TC-47 runs this test inside an isolated 1 GiB Docker cgroup")
	}
	disableAutomaticGC(t)
	sampler := newSystemMemorySampler(0)
	sample, err := sampler.Sample()
	if err != nil || sample.source != "cgroup.v2" || sample.limit != 1024*1024*1024 {
		t.Fatalf("expected isolated 1 GiB cgroup.v2: sample=%+v error=%v", sample, err)
	}
	// Each claim retains a live buffer while the process also owns reclaimable
	// heap. Nothing external exits or releases pressure to make this test pass.
	buffers := make([][]byte, 60)
	for i := range buffers {
		buffers[i] = make([]byte, 256*1024)
		for offset := 0; offset < len(buffers[i]); offset += 4096 {
			buffers[i][offset] = 1
		}
	}
	fillIdleCgroupHeap(t, sampler)
	sample, err = sampler.Sample()
	if err != nil || !atOrAbovePercent(sample.current, sample.limit, 80) {
		t.Fatalf("failed to reproduce high watermark: sample=%+v error=%v", sample, err)
	}
	t.Logf("before admission: current=%d limit=%d", sample.current, sample.limit)
	config := DefaultConfiguration()
	controller := newMemoryController(sampler, config.memoryHighWatermarkPercent,
		config.memoryLowWatermarkPercent, time.Duration(config.memoryCheckIntervalMs)*time.Millisecond, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range buffers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := controller.Wait(ctx); err != nil {
				t.Errorf("claim %d stayed paused with reclaimable heap: %v", i, err)
			}
			runtime.KeepAlive(buffers[i])
		}(i)
	}
	wg.Wait()
	sample, err = sampler.Sample()
	if err != nil || !belowPercent(sample.current, sample.limit, int64(config.memoryLowWatermarkPercent)) {
		t.Errorf("cgroup did not recover below resume watermark: sample=%+v error=%v", sample, err)
	}
	t.Logf("after admission: current=%d limit=%d", sample.current, sample.limit)
	runtime.KeepAlive(buffers)
}
