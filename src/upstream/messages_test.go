package upstream

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sitia.nu/airgap/src/internal/pinnedtls"
	"sitia.nu/airgap/src/logging"
)

func TestParseMessageSpec(t *testing.T) {
	tests := []struct {
		name         string
		value        string
		defaultLevel logging.LogLevel
		enabled      bool
		level        logging.LogLevel
		template     string
	}{
		{"none disables", "NONE", logging.WARN, false, 0, ""},
		{"none is case insensitive", "none", logging.WARN, false, 0, ""},
		{"empty disables", "  ", logging.WARN, false, 0, ""},
		{"level and message", "WARN:high watermark", logging.INFO, true, logging.WARN, "high watermark"},
		{"level is case insensitive", "error:boom", logging.INFO, true, logging.ERROR, "boom"},
		{"message may contain colons", "INFO:Event {partition}:{row} lost", logging.WARN, true, logging.INFO,
			"Event {partition}:{row} lost"},
		{"no level uses the default", "just a message", logging.ERROR, true, logging.ERROR, "just a message"},
		{"unknown prefix is part of the message", "Note: something", logging.INFO, true, logging.INFO,
			"Note: something"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMessageSpec(tc.value, tc.defaultLevel)
			if got.enabled != tc.enabled {
				t.Fatalf("enabled = %t, want %t", got.enabled, tc.enabled)
			}
			if !tc.enabled {
				return
			}
			if got.level != tc.level {
				t.Errorf("level = %v, want %v", got.level, tc.level)
			}
			if got.template != tc.template {
				t.Errorf("template = %q, want %q", got.template, tc.template)
			}
		})
	}
}

func TestLooksLikeLevel(t *testing.T) {
	tests := map[string]bool{
		"WARNING:oops":     true,
		"WARN:fine":        true,
		"Note: something":  false,
		"no colon at all":  false,
		":leading colon":   false,
		"VERYLONGLEVEL:no": false,
	}
	for value, want := range tests {
		if got := looksLikeLevel(value); got != want {
			t.Errorf("looksLikeLevel(%q) = %t, want %t", value, got, want)
		}
	}
}

func TestRenderTemplateAcceptsEitherNotation(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.id = "Upstream_1"
	config.lostEventDirectory = "/lost"

	for _, template := range []string{
		"{upstreamId} {lostEventDirectory}",
		"{upstream_id} {lost_event_directory}",
		"{UPSTREAM_ID} {LostEventDirectory}",
	} {
		if got := renderTemplate(template, nil); got != "Upstream_1 /lost" {
			t.Errorf("renderTemplate(%q) = %q, want %q", template, got, "Upstream_1 /lost")
		}
	}
}

func TestRenderTemplateContextOverridesConfiguration(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.topic = "configured"

	got := renderTemplate("{topic}", map[string]string{"topic": "actual"})
	if got != "actual" {
		t.Errorf("renderTemplate() = %q, want %q", got, "actual")
	}
}

func TestRenderJSONTemplateEscapesPlaceholderValues(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.id = "upstream \"north\""
	config.lostEventDirectory = "/lost\\events"
	template := `{
		"eventName":"message.lost",
		"body":{"stringValue":"AIRGAP LOST EVENT: {reason}"},
		"attributes":[
			{"key":"service.instance.id","value":{"stringValue":"{upstreamId}"}},
			{"key":"airgap.event.lost.file","value":{"stringValue":"{lostEventDirectory}"}}
		]
	}`
	rendered, err := renderJSONTemplate(template, map[string]string{
		"reason": "couldn't open \"payload\"\nline two\tend",
	})
	if err != nil {
		t.Fatalf("renderJSONTemplate() error = %v", err)
	}
	if !json.Valid([]byte(rendered)) {
		t.Fatalf("renderJSONTemplate() produced invalid JSON: %s", rendered)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(rendered), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	body := got["body"].(map[string]any)["stringValue"]
	if body != "AIRGAP LOST EVENT: couldn't open \"payload\"\nline two\tend" {
		t.Errorf("body.stringValue = %q", body)
	}
	attributes := got["attributes"].([]any)
	if value := attributes[0].(map[string]any)["value"].(map[string]any)["stringValue"]; value != config.id {
		t.Errorf("service.instance.id = %q, want %q", value, config.id)
	}
	if value := attributes[1].(map[string]any)["value"].(map[string]any)["stringValue"]; value != config.lostEventDirectory {
		t.Errorf("airgap.event.lost.file = %q, want %q", value, config.lostEventDirectory)
	}
}

func TestRenderJSONTemplateRejectsInvalidTemplates(t *testing.T) {
	tests := []struct {
		name     string
		template string
	}{
		{"invalid JSON", `{"reason":"{reason}"`},
		{"placeholder outside string", `{"row":{row}}`},
		{"non-object root", `["{reason}"]`},
		{"trailing value", `{"reason":"{reason}"} {}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := renderJSONTemplate(tc.template, map[string]string{"reason": "failure", "row": "1"}); err == nil {
				t.Fatalf("renderJSONTemplate(%q) unexpectedly succeeded", tc.template)
			}
		})
	}
}

func TestValidatePayloadTemplate(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		template string
		wantErr  bool
	}{
		{"text", messageFormatText, "not JSON", false},
		{"JSON object", messageFormatJSON, `{"body":"{reason}"}`, false},
		{"malformed JSON", messageFormatJSON, `{"body":"{reason}"`, true},
		{"placeholder outside string", messageFormatJSON, `{"row":{row}}`, true},
		{"unknown format", "yaml", `{"body":"lost"}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePayloadTemplate(tc.format, tc.template)
			if (err != nil) != tc.wantErr {
				t.Errorf("validatePayloadTemplate() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestLostEventMessageFormatConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.properties")
	if err := os.WriteFile(path, []byte("airgapLostEventMessageFormat=JSON\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	fromFile, err := ReadParameters(path, DefaultConfiguration())
	if err != nil {
		t.Fatalf("ReadParameters() error = %v", err)
	}
	if fromFile.airgapLostEventMessageFormat != messageFormatJSON {
		t.Errorf("file format = %q, want %q", fromFile.airgapLostEventMessageFormat, messageFormatJSON)
	}

	t.Setenv("AIRGAP_UPSTREAM_AIRGAP_LOST_EVENT_MESSAGE_FORMAT", "json")
	fromEnvironment := overrideConfiguration(DefaultConfiguration())
	if fromEnvironment.airgapLostEventMessageFormat != messageFormatJSON {
		t.Errorf("environment format = %q, want %q", fromEnvironment.airgapLostEventMessageFormat, messageFormatJSON)
	}

	fromCommandLine := parseCommandLineOverrides(
		[]string{"upstream", "--airgapLostEventMessageFormat=json"},
		DefaultConfiguration(),
	)
	if fromCommandLine.airgapLostEventMessageFormat != messageFormatJSON {
		t.Errorf("command-line format = %q, want %q", fromCommandLine.airgapLostEventMessageFormat, messageFormatJSON)
	}
}

func TestReadParametersRejectsUnknownLostEventMessageFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.properties")
	if err := os.WriteFile(path, []byte("airgapLostEventMessageFormat=yaml\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := ReadParameters(path, DefaultConfiguration()); err == nil {
		t.Fatal("ReadParameters() unexpectedly accepted airgapLostEventMessageFormat=yaml")
	}
}

func TestLostEventRetentionConfigurationOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.properties")
	content := "lostEventDirectoryCompressAfterDays=5\n" +
		"lostEventDirectoryDeleteAfterDays=45\n" +
		"lostMessageTruncatedMessage=ERROR:removed {fileName} {action}\n" +
		"lostMessageTruncatedMessageFormat=JSON\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	fromFile, err := ReadParameters(path, DefaultConfiguration())
	if err != nil {
		t.Fatalf("ReadParameters() error = %v", err)
	}
	if fromFile.lostEventDirectoryCompressAfterDays != 5 ||
		fromFile.lostEventDirectoryDeleteAfterDays != 45 ||
		fromFile.lostMessageTruncatedMessage != "ERROR:removed {fileName} {action}" ||
		fromFile.lostMessageTruncatedMessageFormat != messageFormatJSON {
		t.Fatalf("unexpected file retention configuration: %+v", fromFile)
	}

	t.Setenv("AIRGAP_UPSTREAM_LOST_EVENT_DIRECTORY_COMPRESS_AFTER_DAYS", "6")
	t.Setenv("AIRGAP_UPSTREAM_LOST_EVENT_DIRECTORY_DELETE_AFTER_DAYS", "46")
	t.Setenv("AIRGAP_UPSTREAM_LOST_MESSAGE_TRUNCATED_MESSAGE", "WARN:env {fileName}")
	t.Setenv("AIRGAP_UPSTREAM_LOST_MESSAGE_TRUNCATED_MESSAGE_FORMAT", "json")
	fromEnvironment := overrideConfiguration(DefaultConfiguration())
	if fromEnvironment.lostEventDirectoryCompressAfterDays != 6 ||
		fromEnvironment.lostEventDirectoryDeleteAfterDays != 46 ||
		fromEnvironment.lostMessageTruncatedMessage != "WARN:env {fileName}" ||
		fromEnvironment.lostMessageTruncatedMessageFormat != messageFormatJSON {
		t.Fatalf("unexpected environment retention configuration: %+v", fromEnvironment)
	}

	fromCommandLine := parseCommandLineOverrides([]string{
		"upstream",
		"--lostEventDirectoryCompressAfterDays=8",
		"--lostEventDirectoryDeleteAfterDays=48",
		"--lostMessageTruncatedMessage=INFO:cli {fileName}",
		"--lostMessageTruncatedMessageFormat=json",
	}, DefaultConfiguration())
	if fromCommandLine.lostEventDirectoryCompressAfterDays != 8 ||
		fromCommandLine.lostEventDirectoryDeleteAfterDays != 48 ||
		fromCommandLine.lostMessageTruncatedMessage != "INFO:cli {fileName}" ||
		fromCommandLine.lostMessageTruncatedMessageFormat != messageFormatJSON {
		t.Fatalf("unexpected command-line retention configuration: %+v", fromCommandLine)
	}
}

func TestJSONTruncationMessageAllowsLevelPrefixAndNone(t *testing.T) {
	jsonSpec := parseMessageSpec(`WARN:{"eventName":"message.lost.truncated","body":{"stringValue":"{fileName}"}}`, logging.WARN)
	if !jsonSpec.enabled {
		t.Fatal("JSON truncation message unexpectedly disabled")
	}
	if err := validatePayloadTemplate(messageFormatJSON, jsonSpec.template); err != nil {
		t.Fatalf("level-prefixed JSON template is invalid: %v", err)
	}
	if spec := parseMessageSpec("NONE", logging.WARN); spec.enabled {
		t.Fatal("NONE should disable the truncation message")
	}
}

func TestRenderTemplateExposesEveryConfigurationValue(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.kafkaMaxMessageBytes = 6291456
	config.transport = "tcp"

	got := renderTemplate("{kafkaMaxMessageBytes} {transport} {tcpRetryTimes}", nil)
	if got != "6291456 tcp 0" {
		t.Errorf("renderTemplate() = %q, want %q", got, "6291456 tcp 0")
	}
}

// TestTemplateValuesCoversEveryConfigurationField fails when a configuration
// field is added without being made available to templates. There is no
// exclusion list: every field must resolve to something, so a template
// placeholder is never silently unavailable.
func TestTemplateValuesCoversEveryConfigurationField(t *testing.T) {
	values := templateValues(DefaultConfiguration())
	available := make(map[string]bool, len(values))
	for name := range values {
		available[normalizePlaceholder(name)] = true
	}

	fields := reflect.TypeOf(TransferConfiguration{})
	for i := 0; i < fields.NumField(); i++ {
		name := fields.Field(i).Name
		if !available[normalizePlaceholder(name)] {
			t.Errorf("configuration field %q is not available as a template placeholder", name)
		}
	}
}

// TestTemplateValuesHoldNoSecrets guards the one exception to exposing
// everything: a rendered template is written to the log and delivered to
// downstream, and the symmetric key is what protects that delivery.
func TestTemplateValuesHoldNoSecrets(t *testing.T) {
	c := DefaultConfiguration()
	c.key = []byte("symmetric-key-material")
	c.newkey = []byte("next-symmetric-key")
	c.keyPasswordFile = "/secret/password-file"

	values := templateValues(c)
	for name, value := range values {
		if strings.Contains(value, "symmetric-key") {
			t.Errorf("template value %q leaks key material: %q", name, value)
		}
	}
	if values["key"] != valueRedacted || values["newkey"] != valueRedacted {
		t.Errorf("key material in use must render as %q, got %q and %q",
			valueRedacted, values["key"], values["newkey"])
	}
	if values["keyPasswordFile"] != valueRedacted {
		t.Errorf("keyPasswordFile must render as %q, got %q", valueRedacted, values["keyPasswordFile"])
	}

	empty := templateValues(DefaultConfiguration())
	if empty["key"] != valueNone || empty["newkey"] != valueNone {
		t.Errorf("absent key material must render as %q, got %q and %q",
			valueNone, empty["key"], empty["newkey"])
	}
	if empty["keyPasswordFile"] != valueNone {
		t.Errorf("absent keyPasswordFile must render as %q, got %q", valueNone, empty["keyPasswordFile"])
	}
}

func TestTemplateValuesRenderPublicKeyAsFingerprint(t *testing.T) {
	c := DefaultConfiguration()
	if got := templateValues(c)["publicKey"]; got != valueNone {
		t.Errorf("publicKey without a key = %q, want %q", got, valueNone)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey() error = %v", err)
	}
	c.publicKey = &key.PublicKey

	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("x509.MarshalPKIXPublicKey() error = %v", err)
	}
	want := pinnedtls.Fingerprint(spki)
	if got := templateValues(c)["publicKey"]; got != want {
		t.Errorf("publicKey = %q, want %q", got, want)
	}
}

func TestTemplateValuesRenderDerivedObjects(t *testing.T) {
	c := DefaultConfiguration()
	c.translations = map[string]string{"in-b": "out-b", "in-a": "out-a"}

	values := templateValues(c)
	if values["translations"] != "in-a=out-a,in-b=out-b" {
		t.Errorf("translations = %q, want %q", values["translations"], "in-a=out-a,in-b=out-b")
	}
	if values["filter"] != valueNone || values["inputFilter"] != valueNone {
		t.Errorf("absent filters must render as %q, got %q and %q",
			valueNone, values["filter"], values["inputFilter"])
	}

	empty := templateValues(DefaultConfiguration())
	if empty["translations"] != valueNone {
		t.Errorf("absent translations = %q, want %q", empty["translations"], valueNone)
	}
}

func TestMemoryWatermarkContext(t *testing.T) {
	context := memoryWatermarkContext(memorySample{current: 150, limit: 200, source: "cgroup.v2"})
	want := map[string]string{
		"memoryCurrentBytes":   "150",
		"memoryUsedLimitBytes": "200",
		"memoryUsedPercent":    "75",
		"memorySource":         "cgroup.v2",
	}
	for name, value := range want {
		if context[name] != value {
			t.Errorf("context[%q] = %q, want %q", name, context[name], value)
		}
	}
}

func TestMemoryWatermarkContextWithoutLimit(t *testing.T) {
	context := memoryWatermarkContext(memorySample{current: 150, limit: 0, source: "none"})
	if context["memoryUsedPercent"] != "0" {
		t.Errorf("percent = %q, want 0 when no limit is known", context["memoryUsedPercent"])
	}
}

func TestDefaultMemoryHighWatermarkMessageRenders(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.id = "Upstream_1"

	spec := parseMessageSpec(config.memoryHighWatermarkLog, logging.WARN)
	if !spec.enabled || spec.level != logging.WARN {
		t.Fatalf("default high watermark message should be enabled at WARN, got %+v", spec)
	}
	got := renderTemplate(spec.template,
		memoryWatermarkContext(memorySample{current: 150, limit: 200, source: "cgroup.v2"}))
	if strings.Contains(got, "{") {
		t.Errorf("default high watermark message left placeholders unexpanded: %q", got)
	}
	for _, want := range []string{"Upstream_1", "150", "200", "75", "cgroup.v2"} {
		if !strings.Contains(got, want) {
			t.Errorf("default high watermark message %q does not contain %q", got, want)
		}
	}
}

func TestDefaultMemoryLowWatermarkMessageIsDisabled(t *testing.T) {
	spec := parseMessageSpec(DefaultConfiguration().memoryLowWatermarkLog, logging.INFO)
	if spec.enabled {
		t.Errorf("default low watermark message should be NONE, got %+v", spec)
	}
}

func TestEmitConfiguredMessageReturnsEmptyWhenDisabled(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	got := emitConfiguredMessage(parseMessageSpec("NONE", logging.WARN),
		logging.Event(logging.EventMemoryThrottle), nil)
	if got != "" {
		t.Errorf("emitConfiguredMessage() = %q, want an empty string", got)
	}
}
