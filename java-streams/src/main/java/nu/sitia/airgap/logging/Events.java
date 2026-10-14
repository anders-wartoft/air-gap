package nu.sitia.airgap.logging;

public final class Events {
    public static final String APP_START = "app.lifecycle.start";
    public static final String APP_READY = "app.lifecycle.ready";
    public static final String APP_SHUTDOWN = "app.lifecycle.shutdown";
    public static final String APP_FAILURE = "app.lifecycle.failure";
    public static final String CONFIG_LOAD = "config.load";
    public static final String CONFIG_APPLIED = "config.applied";
    public static final String CONFIG_INVALID = "config.invalid";
    public static final String CONFIG_PRODUCTION_WARNING = "config.production.warning";
    public static final String KAFKA_CONNECT = "kafka.connect";
    public static final String KAFKA_CONNECTION_FAILED = "kafka.connection.failed";
    public static final String KAFKA_CONNECTION_RESTORED = "kafka.connection.restored";
    public static final String KAFKA_CLIENT = "kafka.client";
    public static final String KAFKA_CONSUME = "kafka.consume";
    public static final String KAFKA_PRODUCE = "kafka.produce";
    public static final String KAFKA_REBALANCE = "kafka.rebalance";
    public static final String MESSAGE_FILTER = "message.filter";
    public static final String MESSAGE_DELIVER = "message.deliver";
    public static final String GAP_DETECTED = "gap.detected";
    public static final String GAP_CLOSED = "gap.closed";
    public static final String GAP_REPORT = "gap.report";
    public static final String STATE_STORE = "state.store";
    public static final String STATS_REPORT = "stats.report";
    public static final String DIAGNOSTIC = "app.diagnostic";

    private Events() {
    }
}
