package nu.sitia.airgap.logging;

import org.apache.logging.log4j.ThreadContext;
import org.slf4j.Logger;

public final class Log {
    private Log() {
    }

    public static void trace(Logger logger, Attrs attrs, String message, Object... arguments) {
        with(enrich(logger, attrs, message, arguments, false), () -> logger.trace(message, arguments));
    }

    public static void debug(Logger logger, Attrs attrs, String message, Object... arguments) {
        with(enrich(logger, attrs, message, arguments, false), () -> logger.debug(message, arguments));
    }

    public static void info(Logger logger, Attrs attrs, String message, Object... arguments) {
        with(enrich(logger, attrs, message, arguments, false), () -> logger.info(message, arguments));
    }

    public static void warn(Logger logger, Attrs attrs, String message, Object... arguments) {
        with(enrich(logger, attrs, message, arguments, false), () -> logger.warn(message, arguments));
    }

    public static void warn(org.apache.logging.log4j.core.Logger logger, Attrs attrs, String message) {
        attrs.with("code.namespace", logger.getName())
                .with("log.message.template", message);
        with(attrs, () -> logger.logMessage(
                logger.getName(),
                org.apache.logging.log4j.Level.WARN,
                null,
                new org.apache.logging.log4j.message.SimpleMessage(message),
                null));
    }

    public static void error(Logger logger, Attrs attrs, String message, Object... arguments) {
        with(enrich(logger, attrs, message, arguments, true), () -> logger.error(message, arguments));
    }

    private static Attrs enrich(
            Logger logger, Attrs attrs, String message, Object[] arguments, boolean failed) {
        attrs.with("code.namespace", logger.getName())
                .with("log.message.template", message);
        for (int i = 0; i < arguments.length; i++) {
            Object argument = arguments[i];
            if (argument instanceof Throwable) {
                attrs.exception((Throwable) argument);
            } else {
                attrs.withValue("airgap.event.argument." + i, argument);
            }
        }
        if (failed) {
            attrs.failure();
        }
        return attrs;
    }

    private static void with(Attrs attrs, Runnable action) {
        if (attrs == null) {
            throw new IllegalArgumentException("attrs must not be null");
        }

        String previous = ThreadContext.get(Attrs.CONTEXT_KEY);
        ThreadContext.put(Attrs.CONTEXT_KEY, attrs.toContextJson());
        try {
            action.run();
        } finally {
            if (previous == null) {
                ThreadContext.remove(Attrs.CONTEXT_KEY);
            } else {
                ThreadContext.put(Attrs.CONTEXT_KEY, previous);
            }
        }
    }
}
