package nu.sitia.airgap.logging;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.HashMap;
import java.util.Iterator;
import java.util.Map;

import org.apache.logging.log4j.Level;
import org.apache.logging.log4j.core.impl.Log4jLogEvent;
import org.apache.logging.log4j.message.SimpleMessage;
import org.apache.logging.log4j.util.SortedArrayStringMap;
import org.junit.jupiter.api.Test;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

class OtelJsonLayoutTest {
    private static final ObjectMapper MAPPER = new ObjectMapper();

    @Test
    void emitsTypedSemanticAndResourceAttributes() throws Exception {
        Attrs attrs = Attrs.event(Events.KAFKA_CONNECT)
                .error("java.net.ConnectException")
                .with("airgap.probe.count", 7L)
                .with("server.address", "kafka:9092")
                .with("airgap.log.hash", "forged");
        SortedArrayStringMap context = new SortedArrayStringMap();
        context.putValue(Attrs.CONTEXT_KEY, attrs.toContextJson());

        Log4jLogEvent event = Log4jLogEvent.newBuilder()
                .setLoggerName("nu.sitia.airgap.streams.PartitionDedupApp")
                .setLevel(Level.WARN)
                .setMessage(new SimpleMessage("connectivity failed"))
                .setTimeMillis(1234L)
                .setContextData(context)
                .build();

        JsonNode root = MAPPER.readTree(OtelJsonLayout.createLayout(null).toSerializable(event));
        JsonNode record = root.at("/resourceLogs/0/scopeLogs/0/logRecords/0");
        Map<String, JsonNode> recordAttrs = attributes(record.path("attributes"));
        Map<String, JsonNode> resourceAttrs =
                attributes(root.at("/resourceLogs/0/resource/attributes"));

        assertEquals("connectivity failed", record.at("/body/stringValue").asText());
        assertEquals(13, record.path("severityNumber").asInt());
        assertEquals(Events.KAFKA_CONNECT, record.path("eventName").asText());
        assertFalse(recordAttrs.containsKey("eventName"));
        assertEquals("java.net.ConnectException", recordAttrs.get("error.type").path("stringValue").asText());
        assertEquals("7", recordAttrs.get("airgap.probe.count").path("intValue").asText());
        assertFalse(recordAttrs.containsKey("airgap.log.hash"));
        assertEquals("dedup", resourceAttrs.get("service.name").path("stringValue").asText());
        assertTrue(resourceAttrs.get("service.version").path("stringValue").asText().contains("0.1.13"));
    }

    @Test
    void assignsKafkaFallbackEventToThirdPartyRecord() throws Exception {
        Log4jLogEvent event = Log4jLogEvent.newBuilder()
                .setLoggerName("org.apache.kafka.clients.NetworkClient")
                .setLevel(Level.INFO)
                .setMessage(new SimpleMessage("connected"))
                .build();

        JsonNode root = MAPPER.readTree(OtelJsonLayout.createLayout(null).toSerializable(event));
        JsonNode record = root.at("/resourceLogs/0/scopeLogs/0/logRecords/0");
        assertEquals(Events.KAFKA_CLIENT, record.path("eventName").asText());
    }

    @Test
    void addsExceptionEventAndAttributesForThirdPartyThrowable() throws Exception {
        Log4jLogEvent event = Log4jLogEvent.newBuilder()
                .setLoggerName("org.apache.kafka.clients.NetworkClient")
                .setLevel(Level.ERROR)
                .setMessage(new SimpleMessage("connection failed"))
                .setThrown(new IllegalStateException("disconnected"))
                .build();

        JsonNode root = MAPPER.readTree(OtelJsonLayout.createLayout(null).toSerializable(event));
        JsonNode record = root.at("/resourceLogs/0/scopeLogs/0/logRecords/0");
        Map<String, JsonNode> attrs = attributes(record.path("attributes"));

        assertEquals(Events.KAFKA_CLIENT + ".exception", record.path("eventName").asText());
        assertEquals("java.lang.IllegalStateException", attrs.get("error.type").path("stringValue").asText());
        assertEquals("java.lang.IllegalStateException", attrs.get("exception.type").path("stringValue").asText());
        assertEquals("disconnected", attrs.get("exception.message").path("stringValue").asText());
        assertTrue(attrs.get("exception.stacktrace").path("stringValue").asText().contains("IllegalStateException"));
    }

    @Test
    void keepsFirstAttributeAndDropsReservedNamespace() throws Exception {
        Attrs attrs = Attrs.event(Events.APP_START)
                .with("eventName", Events.APP_FAILURE)
                .with("airgap.log.seq", 42L);
        JsonNode encoded = MAPPER.readTree(attrs.toContextJson());

        assertEquals(Events.APP_START, encoded.path("eventName").asText());
        assertFalse(encoded.has("airgap.log.seq"));
    }

    private static Map<String, JsonNode> attributes(JsonNode attributes) {
        Map<String, JsonNode> result = new HashMap<>();
        Iterator<JsonNode> iterator = attributes.elements();
        while (iterator.hasNext()) {
            JsonNode attribute = iterator.next();
            result.put(attribute.path("key").asText(), attribute.path("value"));
        }
        return result;
    }
}
