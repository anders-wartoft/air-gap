package upstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"testing"
	"time"
)

func TestReadParametersParsesMemoryAndKafkaBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.properties")
	content := "kafkaFetchDefaultBytes=1048576\n" +
		"kafkaMaxMessageBytes=6291456\n" +
		"kafkaChannelBufferSize=2\n" +
		"memoryLimitBytes=201326592\n" +
		"memoryHighWatermarkPercent=75\n" +
		"memoryLowWatermarkPercent=60\n" +
		"memoryCheckIntervalMs=25\n" +
		"goMemoryLimitPercent=65\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	config, err := ReadParameters(path, DefaultConfiguration())
	if err != nil {
		t.Fatalf("ReadParameters: %v", err)
	}
	if config.kafkaFetchDefaultBytes != 1048576 ||
		config.kafkaMaxMessageBytes != 6291456 ||
		config.kafkaChannelBufferSize != 2 ||
		config.memoryLimitBytes != 201326592 ||
		config.memoryHighWatermarkPercent != 75 ||
		config.memoryLowWatermarkPercent != 60 ||
		config.memoryCheckIntervalMs != 25 ||
		config.goMemoryLimitPercent != 65 {
		t.Fatalf("unexpected memory configuration: %+v", config)
	}
}

type sequenceMemorySampler struct {
	mu      sync.Mutex
	samples []memorySample
	index   int
}

func (s *sequenceMemorySampler) Sample() (memorySample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) == 0 {
		return memorySample{}, errors.New("no samples")
	}
	index := s.index
	if index >= len(s.samples) {
		index = len(s.samples) - 1
	} else {
		s.index++
	}
	return s.samples[index], nil
}

func TestSystemMemorySamplerPrefersCgroupV2AndCapsWithConfiguredLimit(t *testing.T) {
	files := map[string]string{
		cgroupV2MemoryLimitPath:   "104857600\n",
		cgroupV2MemoryCurrentPath: "52428800\n",
	}
	sampler := &systemMemorySampler{
		configuredLimit: 80 * 1024 * 1024,
		pageSize:        4096,
		readFile: func(path string) ([]byte, error) {
			value, ok := files[path]
			if !ok {
				return nil, errors.New("not found")
			}
			return []byte(value), nil
		},
	}

	sample, err := sampler.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if sample.current != 50*1024*1024 {
		t.Fatalf("current=%d, want %d", sample.current, 50*1024*1024)
	}
	if sample.limit != 80*1024*1024 {
		t.Fatalf("limit=%d, want %d", sample.limit, 80*1024*1024)
	}
	if sample.source != "cgroup.v2+configured" {
		t.Fatalf("source=%q", sample.source)
	}
}

func TestSystemMemorySamplerResolvesSystemdCgroupPath(t *testing.T) {
	const group = "/system.slice/airgap-upstream@test.service"
	files := map[string]string{
		procSelfCgroupPath:                           "0::" + group + "\n",
		"/sys/fs/cgroup" + group + "/memory.max":     "1073741824\n",
		"/sys/fs/cgroup" + group + "/memory.current": "268435456\n",
	}
	sampler := &systemMemorySampler{
		pageSize: 4096,
		readFile: func(path string) ([]byte, error) {
			value, ok := files[path]
			if !ok {
				return nil, errors.New("not found")
			}
			return []byte(value), nil
		},
	}

	sample, err := sampler.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if sample.current != 256*1024*1024 || sample.limit != 1024*1024*1024 {
		t.Fatalf("sample=%+v", sample)
	}
	if sample.source != "cgroup.v2" {
		t.Fatalf("source=%q", sample.source)
	}
}

func TestSystemMemorySamplerFallsBackToProcessRSS(t *testing.T) {
	files := map[string]string{
		cgroupV2MemoryLimitPath: "max\n",
		procMeminfoPath:         "MemTotal:       262144 kB\n",
		procSelfStatmPath:       "1000 100 0 0 0 0 0\n",
	}
	sampler := &systemMemorySampler{
		pageSize: 4096,
		readFile: func(path string) ([]byte, error) {
			value, ok := files[path]
			if !ok {
				return nil, errors.New("not found")
			}
			return []byte(value), nil
		},
	}

	sample, err := sampler.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if sample.current != 409600 {
		t.Fatalf("current=%d, want 409600", sample.current)
	}
	if sample.limit != 256*1024*1024 {
		t.Fatalf("limit=%d, want %d", sample.limit, 256*1024*1024)
	}
	if sample.source != "process.rss" {
		t.Fatalf("source=%q", sample.source)
	}
}

func TestSystemMemorySamplerUsesConfiguredLimitWithUnlimitedCgroup(t *testing.T) {
	files := map[string]string{
		cgroupV2MemoryLimitPath:   "max\n",
		cgroupV2MemoryCurrentPath: "73400320\n",
	}
	sampler := &systemMemorySampler{
		configuredLimit: 128 * 1024 * 1024,
		pageSize:        4096,
		readFile: func(path string) ([]byte, error) {
			value, ok := files[path]
			if !ok {
				return nil, errors.New("not found")
			}
			return []byte(value), nil
		},
	}

	sample, err := sampler.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if sample.current != 70*1024*1024 || sample.limit != 128*1024*1024 {
		t.Fatalf("sample=%+v", sample)
	}
	if sample.source != "cgroup.v2+configured" {
		t.Fatalf("source=%q", sample.source)
	}
}

func TestMemoryControllerThrottlesAndResumesWithHysteresis(t *testing.T) {
	sampler := &sequenceMemorySampler{samples: []memorySample{
		{current: 81, limit: 100, source: "test"},
		{current: 75, limit: 100, source: "test"},
		{current: 69, limit: 100, source: "test"},
	}}
	var transitions []bool
	controller := newMemoryController(sampler, 80, 70, time.Millisecond, func(throttled bool, _ memorySample) {
		transitions = append(transitions, throttled)
	})

	if err := controller.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(transitions) != 2 || !transitions[0] || transitions[1] {
		t.Fatalf("transitions=%v, want [true false]", transitions)
	}
}

func TestMemoryControllerCancellationStopsThrottleWait(t *testing.T) {
	sampler := &sequenceMemorySampler{samples: []memorySample{
		{current: 90, limit: 100, source: "test"},
	}}
	controller := newMemoryController(sampler, 80, 70, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := controller.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error=%v, want context.Canceled", err)
	}
}

func TestApplyGoMemoryLimitUsesEffectiveLimit(t *testing.T) {
	previous := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })
	sampler := &sequenceMemorySampler{samples: []memorySample{
		{current: 10, limit: 1000, source: "test"},
	}}

	limit, err := applyGoMemoryLimit(sampler, 70)
	if err != nil {
		t.Fatalf("applyGoMemoryLimit: %v", err)
	}
	if limit != 700 {
		t.Fatalf("limit=%d, want 700", limit)
	}
	if got := debug.SetMemoryLimit(-1); got != 700 {
		t.Fatalf("runtime limit=%d, want 700", got)
	}
}
