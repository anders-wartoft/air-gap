package upstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cgroupV2MemoryLimitPath   = "/sys/fs/cgroup/memory.max"
	cgroupV2MemoryCurrentPath = "/sys/fs/cgroup/memory.current"
	cgroupV1MemoryLimitPath   = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
	cgroupV1MemoryCurrentPath = "/sys/fs/cgroup/memory/memory.usage_in_bytes"
	procMeminfoPath           = "/proc/meminfo"
	procSelfStatmPath         = "/proc/self/statm"
	procSelfCgroupPath        = "/proc/self/cgroup"
)

type memorySample struct {
	current int64
	limit   int64
	source  string
}

// defaultMemoryHighWatermarkLog is written when Kafka record admission is
// paused because memory use reached the high watermark.
const defaultMemoryHighWatermarkLog = "WARN:Upstream {upstreamId} entering high memory watermark. " +
	"Pausing Kafka record admission at {memoryCurrentBytes} of {memoryUsedLimitBytes} bytes " +
	"({memoryUsedPercent} percent, from {memorySource})."

// defaultMemoryLowWatermarkLog is NONE: the high watermark is the event worth
// alerting on, and a pause that resolves itself does not need a record. Set it
// to a LEVEL:message value to measure how long throttling lasted.
const defaultMemoryLowWatermarkLog = messageDisabled

// memoryWatermarkContext returns the placeholders a watermark message can use
// on top of the configuration:
//
//	{memoryCurrentBytes}    memory in use when the watermark was crossed
//	{memoryUsedLimitBytes}  limit the watermarks are measured against
//	{memoryUsedPercent}     the first as a percentage of the second
//	{memorySource}          where the limit came from, for example cgroup.v2
//
// memoryLimitBytes is deliberately not overridden here: it keeps meaning the
// configured value, while memoryUsedLimitBytes is the limit actually in force.
func memoryWatermarkContext(sample memorySample) map[string]string {
	percent := "0"
	if sample.limit > 0 {
		percent = strconv.FormatInt(sample.current*100/sample.limit, 10)
	}
	return map[string]string{
		"memoryCurrentBytes":   strconv.FormatInt(sample.current, 10),
		"memoryUsedLimitBytes": strconv.FormatInt(sample.limit, 10),
		"memoryUsedPercent":    percent,
		"memorySource":         sample.source,
	}
}

type memorySampler interface {
	Sample() (memorySample, error)
}

type systemMemorySampler struct {
	configuredLimit int64
	readFile        func(string) ([]byte, error)
	pageSize        int64
}

func newSystemMemorySampler(configuredLimit int64) *systemMemorySampler {
	return &systemMemorySampler{
		configuredLimit: configuredLimit,
		readFile:        os.ReadFile,
		pageSize:        int64(os.Getpagesize()),
	}
}

func (s *systemMemorySampler) Sample() (memorySample, error) {
	for _, candidate := range s.cgroupCandidates() {
		if sample, ok := s.sampleCgroup(candidate.limitPath, candidate.currentPath, candidate.source); ok {
			return s.applyConfiguredLimit(sample), nil
		}
	}

	limit, err := s.systemMemoryTotal()
	if err != nil && s.configuredLimit <= 0 {
		return memorySample{}, err
	}
	current, currentErr := s.processRSS()
	if currentErr != nil {
		return memorySample{}, currentErr
	}
	if s.configuredLimit > 0 && (limit <= 0 || s.configuredLimit < limit) {
		limit = s.configuredLimit
	}
	if limit <= 0 {
		return memorySample{}, errors.New("no usable memory limit found")
	}
	return memorySample{current: current, limit: limit, source: "process.rss"}, nil
}

type cgroupCandidate struct {
	limitPath   string
	currentPath string
	source      string
}

func (s *systemMemorySampler) cgroupCandidates() []cgroupCandidate {
	candidates := make([]cgroupCandidate, 0, 4)
	if raw, err := s.readFile(procSelfCgroupPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
			if len(fields) != 3 {
				continue
			}
			group := strings.TrimPrefix(path.Clean("/"+fields[2]), "/")
			if fields[0] == "0" && fields[1] == "" {
				base := path.Join("/sys/fs/cgroup", group)
				candidates = append(candidates, cgroupCandidate{
					limitPath:   path.Join(base, "memory.max"),
					currentPath: path.Join(base, "memory.current"),
					source:      "cgroup.v2",
				})
			}
			for _, controller := range strings.Split(fields[1], ",") {
				if controller == "memory" {
					base := path.Join("/sys/fs/cgroup/memory", group)
					candidates = append(candidates, cgroupCandidate{
						limitPath:   path.Join(base, "memory.limit_in_bytes"),
						currentPath: path.Join(base, "memory.usage_in_bytes"),
						source:      "cgroup.v1",
					})
					break
				}
			}
		}
	}
	candidates = append(candidates,
		cgroupCandidate{
			limitPath:   cgroupV2MemoryLimitPath,
			currentPath: cgroupV2MemoryCurrentPath,
			source:      "cgroup.v2",
		},
		cgroupCandidate{
			limitPath:   cgroupV1MemoryLimitPath,
			currentPath: cgroupV1MemoryCurrentPath,
			source:      "cgroup.v1",
		},
	)
	return candidates
}

func (s *systemMemorySampler) sampleCgroup(limitPath, currentPath, source string) (memorySample, bool) {
	limitRaw, err := s.readFile(limitPath)
	if err != nil {
		return memorySample{}, false
	}
	limitText := strings.TrimSpace(string(limitRaw))
	if limitText == "" {
		return memorySample{}, false
	}
	var limit int64
	if limitText == "max" {
		if s.configuredLimit <= 0 {
			return memorySample{}, false
		}
		limit = s.configuredLimit
		source += "+configured"
	} else {
		limit, err = strconv.ParseInt(limitText, 10, 64)
		if err != nil || limit <= 0 || limit >= 1<<60 {
			return memorySample{}, false
		}
	}
	currentRaw, err := s.readFile(currentPath)
	if err != nil {
		return memorySample{}, false
	}
	current, err := strconv.ParseInt(strings.TrimSpace(string(currentRaw)), 10, 64)
	if err != nil || current < 0 {
		return memorySample{}, false
	}
	return memorySample{current: current, limit: limit, source: source}, true
}

func (s *systemMemorySampler) applyConfiguredLimit(sample memorySample) memorySample {
	if s.configuredLimit > 0 && s.configuredLimit < sample.limit {
		sample.limit = s.configuredLimit
		sample.source += "+configured"
	}
	return sample
}

func (s *systemMemorySampler) systemMemoryTotal() (int64, error) {
	raw, err := s.readFile(procMeminfoPath)
	if err != nil {
		return 0, fmt.Errorf("read system memory: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if parseErr != nil {
				return 0, fmt.Errorf("parse MemTotal: %w", parseErr)
			}
			return kib * 1024, nil
		}
	}
	return 0, errors.New("MemTotal missing from /proc/meminfo")
}

func (s *systemMemorySampler) processRSS() (int64, error) {
	raw, err := s.readFile(procSelfStatmPath)
	if err != nil {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return int64(stats.Sys), nil
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0, errors.New("resident pages missing from /proc/self/statm")
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse resident pages: %w", err)
	}
	return pages * s.pageSize, nil
}

type memoryTransition func(throttled bool, sample memorySample)

type memoryController struct {
	sampler       memorySampler
	highPercent   int64
	lowPercent    int64
	checkInterval time.Duration
	onTransition  memoryTransition

	mu        sync.Mutex
	throttled bool
}

func newMemoryController(
	sampler memorySampler,
	highPercent, lowPercent int,
	checkInterval time.Duration,
	onTransition memoryTransition,
) *memoryController {
	return &memoryController{
		sampler:       sampler,
		highPercent:   int64(highPercent),
		lowPercent:    int64(lowPercent),
		checkInterval: checkInterval,
		onTransition:  onTransition,
	}
}

func (c *memoryController) Wait(ctx context.Context) error {
	for {
		sample, err := c.sampler.Sample()
		if err != nil {
			return fmt.Errorf("sample memory pressure: %w", err)
		}

		c.mu.Lock()
		wasThrottled := c.throttled
		if c.throttled {
			if belowPercent(sample.current, sample.limit, c.lowPercent) {
				c.throttled = false
			}
		} else if atOrAbovePercent(sample.current, sample.limit, c.highPercent) {
			c.throttled = true
		}
		isThrottled := c.throttled
		c.mu.Unlock()

		if wasThrottled != isThrottled && c.onTransition != nil {
			c.onTransition(isThrottled, sample)
		}
		if !isThrottled {
			return nil
		}

		timer := time.NewTimer(c.checkInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func atOrAbovePercent(current, limit, percent int64) bool {
	return limit > 0 && current >= limit*percent/100
}

func belowPercent(current, limit, percent int64) bool {
	return limit > 0 && current <= limit*percent/100
}

func applyGoMemoryLimit(sampler memorySampler, percent int) (int64, error) {
	if percent <= 0 {
		return debug.SetMemoryLimit(-1), nil
	}
	sample, err := sampler.Sample()
	if err != nil {
		return 0, err
	}
	limit := sample.limit * int64(percent) / 100
	if limit <= 0 {
		return 0, errors.New("calculated Go memory limit is not positive")
	}
	debug.SetMemoryLimit(limit)
	return limit, nil
}
