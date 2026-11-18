package udp

import (
	"context"
	"fmt"
	"time"
)

const pacingByteBurst = 64 * 1024

type pacer struct {
	ctx     context.Context
	gate    chan struct{}
	packets pacingCredit
	bytes   pacingCredit
	last    time.Time
}

type pacingCredit struct {
	rate, burst, available float64
}

func (c *pacingCredit) refill(elapsed float64) {
	c.available = min(c.burst, c.available+elapsed*c.rate)
}

func (c *pacingCredit) wait(cost float64) float64 {
	if c.available >= cost {
		return 0
	}
	// Refill up to a millisecond of spare credit so subsequent small sends
	// can share one timer wakeup without saving an unbounded burst.
	return (min(c.burst, cost+c.rate/1000) - c.available) / c.rate
}

// ConfigurePacing must be called before sharing the connection with senders.
// Both rates must be positive. Bytes count the encoded UDP payload.
func (u *UDPConn) ConfigurePacing(ctx context.Context, datagramsPerSecond, bytesPerSecond, burst int) error {
	if datagramsPerSecond < 1 || bytesPerSecond < 1 || burst < 1 || burst > 256 {
		return fmt.Errorf("UDP pacing requires positive rates and a burst of 1..256 datagrams")
	}
	u.pacer = &pacer{
		ctx: ctx, gate: make(chan struct{}, 1), last: time.Now(),
		packets: pacingCredit{float64(datagramsPerSecond), float64(burst), float64(burst)},
		bytes:   pacingCredit{float64(bytesPerSecond), pacingByteBurst, pacingByteBurst},
	}
	return nil
}

func (p *pacer) send(ctx context.Context, messages [][]byte, write func([]byte) error) error {
	for len(messages) > 0 {
		n, err := p.sendChunk(ctx, messages, write)
		if err != nil {
			return err
		}
		messages = messages[n:]
	}
	return nil
}

func (p *pacer) sendChunk(ctx context.Context, messages [][]byte, write func([]byte) error) (int, error) {
	select {
	case p.gate <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-p.gate }()

	first := float64(len(messages[0]))
	if first > p.bytes.burst {
		return 0, fmt.Errorf("UDP datagram exceeds pacing byte burst: %d bytes", len(messages[0]))
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		now := time.Now()
		elapsed := now.Sub(p.last).Seconds()
		p.last = now
		p.packets.refill(elapsed)
		p.bytes.refill(elapsed)
		delay := max(p.packets.wait(1), p.bytes.wait(first))
		if delay == 0 {
			break
		}
		timer := time.NewTimer(max(time.Nanosecond, time.Duration(delay*float64(time.Second))))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		}
	}
	n := 0
	for _, msg := range messages {
		size := float64(len(msg))
		if p.packets.available < 1 || p.bytes.available < size {
			break
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		p.packets.available--
		p.bytes.available -= size
		if err := write(msg); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
