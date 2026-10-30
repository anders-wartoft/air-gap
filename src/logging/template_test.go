package logging

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRenderTemplate(t *testing.T) {
	attrs := Event(EventDiagnostic, Str("messaging.destination.name", "transfer"))
	params := []Attr{Str("airgap.message.id", "k1"), Str("messaging.destination.name", "ignored")}
	got := renderTemplate("topic={messaging.destination.name} key={airgap.message.id} {missing} {open", attrs, params)
	if want := "topic=transfer key=k1 {missing} {open"; got != want {
		t.Fatalf("renderTemplate = %q, want %q", got, want)
	}
	if got := renderTemplate("100% {}", nil, nil); got != "100% {}" {
		t.Fatalf("literal template changed: %q", got)
	}
}

func TestTemplateGroupsByIdentityAndShowsLatestParams(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	const template = "Failed to send message: topic={messaging.destination.name} key={airgap.message.id} error={error.message}"
	outOfBrokers := Event(EventDiagnostic, Str("messaging.destination.name", "transfer"),
		ErrMessage(errors.New("out of brokers")))
	breakerOpen := Event(EventDiagnostic, Str("messaging.destination.name", "transfer"),
		ErrMessage(errors.New("circuit breaker is open")))
	for i, attrs := range []Attrs{outOfBrokers, outOfBrokers, breakerOpen, outOfBrokers, breakerOpen, breakerOpen} {
		Logger.ErrorT(attrs, template, Str("airgap.message.id", "k"+string(rune('0'+i))))
	}
	Logger.FlushRepeated()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{
		"[ERROR] Failed to send message: topic=transfer key=k0 error=out of brokers",
		"[ERROR] Failed to send message: topic=transfer key=k2 error=circuit breaker is open",
	}
	if len(lines) != 4 || !strings.HasSuffix(lines[0], want[0]) || !strings.HasSuffix(lines[1], want[1]) {
		t.Fatalf("unexpected output:\n%s", buf.String())
	}
	summaries := strings.Join(lines[2:], "\n")
	for _, summary := range []string{
		"[ERROR] Message repeated 2 times: Failed to send message: topic=transfer key=k3 error=out of brokers",
		"[ERROR] Message repeated 2 times: Failed to send message: topic=transfer key=k5 error=circuit breaker is open",
	} {
		if !strings.Contains(summaries, summary) {
			t.Fatalf("missing summary %q in:\n%s", summary, buf.String())
		}
	}
}

func TestTemplateAndPlainMessagesDoNotShareGroups(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	attrs := Event(EventDiagnostic)
	Logger.Warn(attrs, "retry {airgap.retry.count}")
	Logger.WarnT(attrs, "retry {airgap.retry.count}", Int("airgap.retry.count", 1))
	Logger.WarnT(attrs, "retry {airgap.retry.count}", Int("airgap.retry.count", 2))
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Fatalf("got %d records, want plain and templated first records:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "[WARN] retry 1") {
		t.Fatalf("template not rendered:\n%s", buf.String())
	}
}

func TestTemplateDisabledRepeatWritesEveryRecord(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetRepeatInterval(0); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		Logger.WarnT(Event(EventDiagnostic), "attempt {airgap.retry.count}", Int("airgap.retry.count", int64(i)))
	}
	if got := buf.String(); strings.Count(got, "\n") != 3 || !strings.Contains(got, "[WARN] attempt 2") {
		t.Fatalf("unexpected output:\n%s", got)
	}
}

func TestTemplateOTLPAttributes(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatal(err)
	}
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	const template = "Error from consumer (attempt {airgap.retry.count}): {error.message}"
	attrs := Event(EventDiagnostic, ErrMessage(errors.New("refused")))
	Logger.ErrorT(attrs, template, Int("airgap.retry.count", 1))
	Logger.ErrorT(attrs, template, Int("airgap.retry.count", 2))
	Logger.ErrorT(attrs, template, Int("airgap.retry.count", 3))
	Logger.FlushRepeated()
	records := decodeOTLPLines(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("got %d records, want original and repeat", len(records))
	}
	first, repeated := records[0], records[1]
	if first.body != "Error from consumer (attempt 1): refused" ||
		repeated.body != "Error from consumer (attempt 3): refused" {
		t.Fatalf("bodies = %q, %q", first.body, repeated.body)
	}
	for _, record := range records {
		if record.attributes[AttrMessageTemplate] != template || record.attributes["error.message"] != "refused" {
			t.Fatalf("missing template/error attributes: %+v", record.attributes)
		}
	}
	if first.attributes["airgap.retry.count"] != "1" || repeated.attributes["airgap.retry.count"] != "3" ||
		repeated.attributes["airgap.repeat.count"] != "2" {
		t.Fatalf("unexpected parameter/count attributes: first=%+v repeated=%+v", first.attributes, repeated.attributes)
	}
}
