package upstream

import (
	"context"
	"fmt"
	"math"
	"time"

	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/protocol"
)

type memoryStallNotice struct {
	duration time.Duration
	sample   memorySample
}

func validateMemoryStallInterval(seconds int) error {
	if seconds < 0 || int64(seconds) > math.MaxInt64/int64(time.Second) {
		return fmt.Errorf("memoryStallStatusIntervalSeconds must be between 0 and %d", math.MaxInt64/int64(time.Second))
	}
	return nil
}

func (c *memoryController) stallNotice(now time.Time, interval time.Duration) (memoryStallNotice, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if interval <= 0 || !c.throttled || now.Sub(c.pausedSince) < interval ||
		(!c.lastStatus.IsZero() && now.Sub(c.lastStatus) < interval) {
		return memoryStallNotice{}, false
	}
	c.lastStatus = now
	return memoryStallNotice{duration: now.Sub(c.pausedSince), sample: c.sample}, true
}

func memoryStallMessage(c TransferConfiguration, notice memoryStallNotice) string {
	return fmt.Sprintf("Upstream %s Kafka admission stalled for %d seconds due to memory pressure: current=%d bytes, limit=%d bytes, resume_at=%d bytes (%d%%), source=%s.",
		c.id, int64(notice.duration/time.Second), notice.sample.current, notice.sample.limit,
		notice.sample.limit*int64(c.memoryLowWatermarkPercent)/100, c.memoryLowWatermarkPercent, notice.sample.source)
}

func sendMemoryStallStatus(transport TransportClient, c TransferConfiguration, message string) error {
	return transport.SendMessages(protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS", []byte(message), c.payloadSize))
}

// The monitor owns one delivery at a time, with no pending-message queue.
// A blocked transport must not prevent reclamation, local alerts or shutdown.
// The transport owner closes it on shutdown, releasing an outstanding send.
func monitorMemoryStalls(ctx context.Context, controller *memoryController, transport TransportClient,
	c TransferConfiguration, interval time.Duration, ticks <-chan time.Time) {
	result := make(chan error, 1)
	sending := false
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-result:
			sending = false
			if err != nil {
				Logger.Warnf(logging.Event(logging.EventMemoryThrottle, logging.Failure(), logging.ErrorType(err)),
					"Could not send memory-stall STATUS: %v", err)
			}
		case now := <-ticks:
			if ctx.Err() != nil {
				return
			}
			notice, ok := controller.stallNotice(now, interval)
			if !ok {
				continue
			}
			message := memoryStallMessage(c, notice)
			Logger.Warn(logging.Event(logging.EventMemoryThrottle,
				logging.Bool("airgap.memory.throttled", true),
				logging.Int("airgap.memory.pause_seconds", int64(notice.duration/time.Second)),
				logging.Int("airgap.memory.current_bytes", notice.sample.current),
				logging.Int("airgap.memory.limit_bytes", notice.sample.limit),
				logging.Str("airgap.memory.source", notice.sample.source),
			), message)
			if sending {
				Logger.Warn(logging.Event(logging.EventMemoryThrottle, logging.Failure()),
					"Memory-stall STATUS not sent: previous STATUS delivery is still in progress")
				continue
			}
			sending = true
			go func() { result <- sendMemoryStallStatus(transport, c, message) }()
		}
	}
}
