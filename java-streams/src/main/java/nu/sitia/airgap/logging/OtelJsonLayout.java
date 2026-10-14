package nu.sitia.airgap.logging;

import java.io.PrintWriter;
import java.io.StringWriter;
import java.nio.charset.Charset;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;

import org.apache.logging.log4j.Level;
import org.apache.logging.log4j.core.Layout;
import org.apache.logging.log4j.core.LogEvent;
import org.apache.logging.log4j.core.config.Node;
import org.apache.logging.log4j.core.config.plugins.Plugin;
import org.apache.logging.log4j.core.config.plugins.PluginAttribute;
import org.apache.logging.log4j.core.config.plugins.PluginFactory;
import org.apache.logging.log4j.core.layout.AbstractStringLayout;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;

@Plugin(name = "OtelJsonLayout", category = Node.CATEGORY, elementType = Layout.ELEMENT_TYPE, printObject = true)
public final class OtelJsonLayout extends AbstractStringLayout {
    private static final ObjectMapper MAPPER = new ObjectMapper();
    private static final TypeReference<LinkedHashMap<String, Object>> ATTR_TYPE =
            new TypeReference<LinkedHashMap<String, Object>>() { };

    private OtelJsonLayout(Charset charset) {
        super(charset);
    }

    @PluginFactory
    public static OtelJsonLayout createLayout(
            @PluginAttribute(value = "charset", defaultString = "UTF-8") Charset charset) {
        return new OtelJsonLayout(charset == null ? StandardCharsets.UTF_8 : charset);
    }

    @Override
    public String toSerializable(LogEvent event) {
        ObjectNode root = MAPPER.createObjectNode();
        ArrayNode resourceLogs = root.putArray("resourceLogs");
        ObjectNode resourceLog = resourceLogs.addObject();
        ObjectNode resource = resourceLog.putObject("resource");
        ArrayNode resourceAttrs = resource.putArray("attributes");
        addStringAttribute(resourceAttrs, "service.name", "dedup");
        addStringAttribute(resourceAttrs, "service.instance.id", environment("INSTANCE_ID", "dedup"));
        addStringAttribute(resourceAttrs, "service.version", implementationVersion());

        ObjectNode scopeLog = resourceLog.putArray("scopeLogs").addObject();
        scopeLog.putObject("scope").put("name", event.getLoggerName());
        ObjectNode record = scopeLog.putArray("logRecords").addObject();
        long timestampNanos = event.getTimeMillis() * 1_000_000L;
        record.put("timeUnixNano", Long.toString(timestampNanos));
        record.put("observedTimeUnixNano", Long.toString(timestampNanos));
        record.put("severityNumber", severityNumber(event.getLevel()));
        record.put("severityText", event.getLevel().name());
        record.putObject("body").put("stringValue", event.getMessage().getFormattedMessage());
        LinkedHashMap<String, Object> semanticAttributes = semanticAttributes(event);
        if (event.getLevel().isMoreSpecificThan(Level.ERROR)) {
            semanticAttributes.putIfAbsent("error.type", "_OTHER");
        }
        record.put("eventName", String.valueOf(semanticAttributes.get("eventName")));
        ArrayNode attributes = record.putArray("attributes");
        for (Map.Entry<String, Object> attribute : semanticAttributes.entrySet()) {
            if ("eventName".equals(attribute.getKey())) {
                continue;
            }
            addAttribute(attributes, attribute.getKey(), attribute.getValue());
        }

        try {
            return MAPPER.writeValueAsString(root) + System.lineSeparator();
        } catch (JsonProcessingException e) {
            throw new IllegalStateException("Unable to encode OTLP log record", e);
        }
    }

    private static LinkedHashMap<String, Object> semanticAttributes(LogEvent event) {
        String encoded = event.getContextData().getValue(Attrs.CONTEXT_KEY);
        LinkedHashMap<String, Object> attributes = null;
        if (encoded != null && !encoded.isEmpty()) {
            try {
                LinkedHashMap<String, Object> parsed = MAPPER.readValue(encoded, ATTR_TYPE);
                LinkedHashMap<String, Object> sanitized = new LinkedHashMap<>();
                for (Map.Entry<String, Object> attribute : parsed.entrySet()) {
                    String key = attribute.getKey();
                    if (key != null && !key.isEmpty() && !key.startsWith("airgap.log.")) {
                        sanitized.putIfAbsent(key, attribute.getValue());
                    }
                }
                Object eventName = sanitized.get("eventName");
                if (eventName != null && !String.valueOf(eventName).trim().isEmpty()) {
                    attributes = sanitized;
                }
            } catch (JsonProcessingException ignored) {
                // Invalid context cannot be trusted; use a valid classified fallback.
            }
        }

        if (attributes == null) {
            attributes = new LinkedHashMap<>();
            String loggerName = event.getLoggerName();
            attributes.put("eventName", loggerName != null && loggerName.startsWith("org.apache.kafka.")
                    ? Events.KAFKA_CLIENT
                    : Events.DIAGNOSTIC);
        }

        Throwable thrown = event.getThrown();
        if (thrown != null) {
            String eventName = String.valueOf(attributes.get("eventName"));
            if (!eventName.endsWith(".exception")) {
                attributes.put("eventName", eventName + ".exception");
            }
            attributes.putIfAbsent("error.type", thrown.getClass().getName());
            attributes.putIfAbsent("exception.type", thrown.getClass().getName());
            if (thrown.getMessage() != null) {
                attributes.putIfAbsent("exception.message", thrown.getMessage());
            }
            StringWriter stacktrace = new StringWriter();
            thrown.printStackTrace(new PrintWriter(stacktrace));
            attributes.putIfAbsent("exception.stacktrace", stacktrace.toString());
        }
        return attributes;
    }

    private static void addAttribute(ArrayNode attributes, String key, Object value) {
        ObjectNode attribute = attributes.addObject();
        attribute.put("key", key);
        ObjectNode anyValue = attribute.putObject("value");
        if (value instanceof Boolean) {
            anyValue.put("boolValue", (Boolean) value);
        } else if (value instanceof Byte || value instanceof Short || value instanceof Integer || value instanceof Long) {
            anyValue.put("intValue", String.valueOf(value));
        } else {
            anyValue.put("stringValue", String.valueOf(value));
        }
    }

    private static void addStringAttribute(ArrayNode attributes, String key, String value) {
        addAttribute(attributes, key, value);
    }

    private static int severityNumber(Level level) {
        if (level == Level.TRACE) {
            return 1;
        }
        if (level == Level.DEBUG) {
            return 5;
        }
        if (level == Level.INFO) {
            return 9;
        }
        if (level == Level.WARN) {
            return 13;
        }
        if (level == Level.ERROR) {
            return 17;
        }
        if (level == Level.FATAL) {
            return 21;
        }
        return 0;
    }

    private static String implementationVersion() {
        String version = OtelJsonLayout.class.getPackage().getImplementationVersion();
        return version == null || version.trim().isEmpty() ? "0.1.13-SNAPSHOT" : version;
    }

    private static String environment(String name, String fallback) {
        String value = System.getenv(name);
        return value == null || value.trim().isEmpty() ? fallback : value;
    }
}
