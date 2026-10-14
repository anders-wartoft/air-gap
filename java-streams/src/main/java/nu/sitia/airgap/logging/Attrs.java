package nu.sitia.airgap.logging;

import java.io.PrintWriter;
import java.io.StringWriter;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.Map;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.ObjectMapper;

public final class Attrs {
    static final String CONTEXT_KEY = "airgap.semantic.attributes";
    private static final String RESERVED_PREFIX = "airgap.log.";
    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final LinkedHashMap<String, Object> values = new LinkedHashMap<>();

    private Attrs(String eventName) {
        with("eventName", eventName);
    }

    public static Attrs event(String eventName) {
        if (eventName == null || eventName.trim().isEmpty()) {
            throw new IllegalArgumentException("eventName must not be empty");
        }
        return new Attrs(eventName);
    }

    public Attrs with(String key, String value) {
        return put(key, value);
    }

    public Attrs with(String key, long value) {
        return put(key, value);
    }

    public Attrs with(String key, boolean value) {
        return put(key, value);
    }

    Attrs withValue(String key, Object value) {
        if (value instanceof Boolean) {
            return with(key, (Boolean) value);
        }
        if (value instanceof Byte || value instanceof Short || value instanceof Integer || value instanceof Long) {
            return with(key, ((Number) value).longValue());
        }
        return with(key, String.valueOf(value));
    }

    public Attrs failure() {
        return error("_OTHER");
    }

    public Attrs error(String errorType) {
        return put("error.type", errorType == null || errorType.trim().isEmpty() ? "_OTHER" : errorType);
    }

    public Attrs exception(Throwable exception) {
        if (exception == null) {
            return failure();
        }

        String eventName = String.valueOf(values.get("eventName"));
        if (!eventName.endsWith(".exception")) {
            values.put("eventName", eventName + ".exception");
        }
        error(exception.getClass().getName());
        put("exception.type", exception.getClass().getName());
        if (exception.getMessage() != null) {
            put("exception.message", exception.getMessage());
        }
        StringWriter stacktrace = new StringWriter();
        exception.printStackTrace(new PrintWriter(stacktrace));
        put("exception.stacktrace", stacktrace.toString());
        return this;
    }

    Map<String, Object> values() {
        return Collections.unmodifiableMap(values);
    }

    String toContextJson() {
        try {
            return MAPPER.writeValueAsString(values);
        } catch (JsonProcessingException e) {
            throw new IllegalStateException("Unable to encode log attributes", e);
        }
    }

    private Attrs put(String key, Object value) {
        if (key == null || key.isEmpty() || key.startsWith(RESERVED_PREFIX) || values.containsKey(key)) {
            return this;
        }
        values.put(key, value == null ? "" : value);
        return this;
    }
}
