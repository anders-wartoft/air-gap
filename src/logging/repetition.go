package logging

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const defaultRepeatInterval = 10 * time.Second
const maxRepeatEntries = 1024

var repeatInterval = repeatIntervalFromEnvironment()
var repetitions = make(map[repeatKey]*repeatState)
var repeatTimer *time.Timer

type repeatKey struct {
	body  string
	level LogLevel
	attrs string
}

type repeatState struct {
	attrs       Attrs
	count       int64
	lastEmitted time.Time
	lastSeen    time.Time
}

func repeatIntervalFromEnvironment() time.Duration {
	value, set := os.LookupEnv("AIRGAP_LOG_REPEAT_INTERVAL")
	if !set {
		return defaultRepeatInterval
	}
	interval, err := parseRepeatInterval(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid AIRGAP_LOG_REPEAT_INTERVAL: %v\n", err)
		os.Exit(1)
	}
	return interval
}

func parseRepeatInterval(value string) (time.Duration, error) {
	ms, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || ms < 0 || ms > int64((1<<63-1)/time.Millisecond) {
		return 0, fmt.Errorf("expected nonnegative milliseconds within time.Duration range, got %q", value)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// SetRepeatInterval sets the summary interval. Zero disables suppression.
func (l *LoggerType) SetRepeatInterval(interval time.Duration) error {
	if interval < 0 {
		return fmt.Errorf("repeat interval must not be negative")
	}
	emitMu.Lock()
	defer emitMu.Unlock()
	resetRepetitionLocked(time.Now())
	repeatInterval = interval
	return nil
}

// FlushRepeated writes pending summaries and clears the duplicate cache.
func (l *LoggerType) FlushRepeated() {
	emitMu.Lock()
	defer emitMu.Unlock()
	resetRepetitionLocked(time.Now())
}

func resetRepetitionLocked(now time.Time) {
	if repeatTimer != nil {
		repeatTimer.Stop()
		repeatTimer = nil
	}
	for key, entry := range repetitions {
		flushRepetitionLocked(now, key, entry)
	}
	clear(repetitions)
}

func flushRepetitionLocked(now time.Time, key repeatKey, entry *repeatState) {
	if entry.count == 0 {
		return
	}
	attrs := Event(EventLogRepeat,
		Int("airgap.repeat.count", entry.count),
	)
	for _, attr := range entry.attrs {
		if attr.Key == AttrEventName {
			attrs = attrs.With(Str("airgap.repeat.event", attr.Value))
			break
		}
	}
	body := fmt.Sprintf("Message repeated %d times: %s", entry.count, key.body)
	if logFormat == FormatOTel {
		body = key.body
		attrs = entry.attrs
		if entry.count != 1 {
			attrs = Attrs{Int("airgap.repeat.count", entry.count)}.Merge(attrs)
		}
	}
	writeEventLocked(now, key.level, body, attrs)
	entry.count = 0
	entry.lastEmitted = now
}

func suppressRepeatedLocked(now time.Time, level LogLevel, attrs Attrs, body string) bool {
	if repeatInterval == 0 || level == FATAL {
		resetRepetitionLocked(now)
		return false
	}
	var identity strings.Builder
	for _, attr := range attrs {
		fmt.Fprintf(&identity, "%q:%q:%t:%t;", attr.Key, attr.Value, attr.IsInt, attr.IsBool)
	}
	key := repeatKey{body: body, level: level, attrs: identity.String()}
	entry := repetitions[key]
	if entry != nil && now.Sub(entry.lastSeen) >= repeatInterval {
		flushRepetitionLocked(now, key, entry)
		delete(repetitions, key)
		entry = nil
	}
	if entry == nil {
		if len(repetitions) >= maxRepeatEntries {
			var oldest repeatKey
			var oldestTime time.Time
			for candidate, state := range repetitions {
				if oldestTime.IsZero() || state.lastSeen.Before(oldestTime) {
					oldest, oldestTime = candidate, state.lastSeen
				}
			}
			flushRepetitionLocked(now, oldest, repetitions[oldest])
			delete(repetitions, oldest)
		}
		repetitions[key] = &repeatState{
			attrs: slices.Clone(attrs), lastEmitted: now, lastSeen: now,
		}
		scheduleRepeatTimerLocked(now)
		return false
	}
	entry.count++
	entry.lastSeen = now
	if now.Sub(entry.lastEmitted) >= repeatInterval {
		flushRepetitionLocked(now, key, entry)
	}
	scheduleRepeatTimerLocked(now)
	return true
}

func scheduleRepeatTimerLocked(now time.Time) {
	if repeatTimer != nil || len(repetitions) == 0 {
		return
	}
	delay := repeatInterval
	for _, entry := range repetitions {
		due := entry.lastSeen.Add(repeatInterval)
		if entry.count > 0 {
			due = entry.lastEmitted.Add(repeatInterval)
		}
		if remaining := due.Sub(now); remaining < delay {
			delay = remaining
		}
	}
	// Associate the callback with its timer so an expired callback cannot act
	// on a new cache after a concurrent output/configuration change.
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		emitMu.Lock()
		defer emitMu.Unlock()
		if repeatTimer != timer {
			return
		}
		repeatTimer = nil
		now := time.Now()
		for key, entry := range repetitions {
			if now.Sub(entry.lastEmitted) >= repeatInterval {
				flushRepetitionLocked(now, key, entry)
			}
			if entry.count == 0 && now.Sub(entry.lastSeen) >= repeatInterval {
				delete(repetitions, key)
			}
		}
		scheduleRepeatTimerLocked(now)
	})
	repeatTimer = timer
}
