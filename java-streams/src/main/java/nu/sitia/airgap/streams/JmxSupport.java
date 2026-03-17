package nu.sitia.airgap.streams;

import nu.sitia.airgap.gapdetector.GapDetector;
import nu.sitia.airgap.streams.PartitionDedupApp.DedupAndGapProcessor;

import javax.management.*;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.lang.management.ManagementFactory;
import java.util.*;

public class JmxSupport {
    private static final Logger LOG = LoggerFactory.getLogger(JmxSupport.class);
    
    public static class GapDetectorsDynamicMBean implements DynamicMBean {
        private final DedupAndGapProcessor processor;
        public GapDetectorsDynamicMBean(DedupAndGapProcessor processor) {
            this.processor = processor;
        }
        @Override
        public Object invoke(String actionName, Object[] params, String[] signature) throws MBeanException, ReflectionException {
            Map<String, GapDetector> gapDetectors = processor.getGapDetectors();
            // Partition-specific operations: getAllGaps_<partition> and purge_<partition>
            if (actionName.startsWith("getAllGaps_")) {
                String key = actionName.substring("getAllGaps_".length());
                GapDetector gd = gapDetectors.get(key);                
                if (gd == null) return "Not found";
                try {
                    java.util.List<?> gaps = gd.getAllCompactGaps();
                    return gaps.toString();
                } catch (Exception e) {
                    throw new MBeanException(e, "Failed to getAllGaps for key: " + key);
                }
            }
            if (actionName.startsWith("purge_")) {
                String key = actionName.substring("purge_".length());
                GapDetector gd = gapDetectors.get(key);
                if (gd == null) return "Not found";
                try {
                    gd.listWindows().clear();
                    return "Purged for key: " + key;
                } catch (Exception e) {
                    throw new MBeanException(e, "Failed to purge for key: " + key);
                }
            }
            return null;
        }
        @Override
        public Object getAttribute(String attribute) throws AttributeNotFoundException {
            Map<String, GapDetector> gapDetectors = processor.getGapDetectors();

            // Support per-partition info, per-partition gaps, and per-partition memory as attributes
            if (attribute.endsWith("_gaps")) {
                String base = attribute.substring(0, attribute.length() - 5);
                GapDetector gd = gapDetectors.get(base);
                if (gd == null) throw new AttributeNotFoundException(attribute);
                String result = gd.getAllCompactGaps().toString();
                LOG.debug("Gaps for {}: {}", base, result);
                return result;
            }
            if (attribute.endsWith("_mem")) {
                String base = attribute.substring(0, attribute.length() - 4);
                GapDetector gd = gapDetectors.get(base);
                if (gd == null) throw new AttributeNotFoundException(attribute);
                return gd.estimateMemoryBytes();
            }
            if (attribute.endsWith("_nrMissing")) {
                String base = attribute.substring(0, attribute.length() - 10);
                GapDetector gd = gapDetectors.get(base);
                if (gd == null) throw new AttributeNotFoundException(attribute);
                return gd.getMissingCounts();
            }
            if (attribute.endsWith("_nrWindows")) {
                String base = attribute.substring(0, attribute.length() - 10);
                GapDetector gd = gapDetectors.get(base);
                if (gd == null) throw new AttributeNotFoundException(attribute);
                return gd.listWindows().size();
            }
            GapDetector gd = gapDetectors.get(attribute);
            if (gd == null) throw new AttributeNotFoundException(attribute);
            StringBuilder sb = new StringBuilder();
            sb.append("size=").append(gd.listWindows().size());
            if (!gd.listWindows().isEmpty()) {
                Map<String, Number> first = gd.listWindows().get(0);
                Map<String, Number> last = gd.listWindows().get(gd.listWindows().size()-1);
                sb.append(", start=").append(first.get("minOffset"));
                sb.append(", end=").append(last.get("maxOffset"));
            }
            sb.append(", windows=[");
            for (Map<String, Number> win : gd.listWindows()) {
                sb.append(win.toString()).append(", ");
            }
            sb.append("]");
            return sb.toString();
        }
        @Override
        public void setAttribute(Attribute attribute) throws AttributeNotFoundException, InvalidAttributeValueException, MBeanException, ReflectionException {
            throw new ReflectionException(new UnsupportedOperationException("Read-only"));
        }
        @Override
        public AttributeList getAttributes(String[] attributes) {
            AttributeList list = new AttributeList();
            for (String attr : attributes) {
                try {
                    Object value = getAttribute(attr);
                    list.add(new Attribute(attr, value));
                } catch (AttributeNotFoundException e) {
                    // skip
                }
            }
            return list;
        }
        @Override
        public AttributeList setAttributes(AttributeList attributes) {
            return new AttributeList(); // read-only
        }
        @Override
        public MBeanInfo getMBeanInfo() {
            // Always regenerate the attribute list so JMX console refresh shows new gapdetectors
            Map<String, GapDetector> gapDetectors = processor.getGapDetectors();
            java.util.List<MBeanAttributeInfo> attrList = new java.util.ArrayList<>();
            if (gapDetectors.isEmpty()) {
                attrList.add(new MBeanAttributeInfo("NoGapDetectors", "java.lang.String", "No GapDetectors registered yet", true, false, false));
            } else {
                for (String name : gapDetectors.keySet()) {
                    String[] parts = name.split("_");
                    String detectorPartition = parts[1];
                    long partition = Long.parseLong(detectorPartition);
                    if (processor.getPartition() != partition) {
                        continue;
                    }
                    attrList.add(new MBeanAttributeInfo(name, "java.lang.String", "GapDetector info for " + name, true, false, false));
                    attrList.add(new MBeanAttributeInfo(name + "_gaps", "java.lang.String", "Gaps for " + name, true, false, false));
                    attrList.add(new MBeanAttributeInfo(name + "_mem", "java.lang.Long", "Estimated memory usage (bytes) for all loaded windows in " + name, true, false, false));
                    attrList.add(new MBeanAttributeInfo(name + "_nrMissing", "java.lang.Long", "Number of missing offsets detected in " + name, true, false, false));
                    attrList.add(new MBeanAttributeInfo(name + "_nrWindows", "java.lang.Long", "Number of windows in " + name, true, false, false));
                }
            }
            // Dynamically create one operation per partition for getAllGaps and purge
            java.util.List<MBeanOperationInfo> opList = new java.util.ArrayList<>();
            for (String name : gapDetectors.keySet()) {
                String detectorPartition = name.split("_")[1];
                long partition = Long.parseLong(detectorPartition);
                if (processor.getPartition() != partition) {
                    continue;
                }
                opList.add(new MBeanOperationInfo(
                    "getAllGaps_" + name,
                    "Show gaps for " + name,
                    new MBeanParameterInfo[] {},
                    "java.lang.String",
                    MBeanOperationInfo.INFO
                ));
                opList.add(new MBeanOperationInfo(
                    "purge_" + name,
                    "Purge all full windows for " + name,
                    new MBeanParameterInfo[] {},
                    "java.lang.String",
                    MBeanOperationInfo.ACTION
                ));
            }
            return new MBeanInfo(
                this.getClass().getName(),
                "Dynamic MBean for all GapDetectors",
                attrList.toArray(new MBeanAttributeInfo[0]),
                null, // constructors
                opList.toArray(new MBeanOperationInfo[0]),
                null  // notifications
            );
        }
    }

    public static class PropsDynamicMBean implements DynamicMBean {
        private final Properties props;
        private final String rawTopic;
        private final String cleanTopic;
        private final String gapTopic;
        private final String applicationId;
        private final java.util.Set<Integer> assignedRawPartitions;
        private final long windowSize;
        private final int maxWindows;
        public PropsDynamicMBean(Properties props, String rawTopic, String cleanTopic, String gapTopic, String applicationId, java.util.Set<Integer> assignedRawPartitions, long windowSize, int maxWindows) {
            this.props = props;
            this.rawTopic = rawTopic;
            this.cleanTopic = cleanTopic;
            this.gapTopic = gapTopic;
            this.applicationId = applicationId;
            this.assignedRawPartitions = assignedRawPartitions;
            this.windowSize = windowSize;
            this.maxWindows = maxWindows;
        }
        @Override
        public Object getAttribute(String attribute) throws AttributeNotFoundException {
            String value = props.getProperty(attribute);
            if (value != null) return value;
            if ("topics".equals(attribute)) {
                return rawTopic + "," + cleanTopic + "," + gapTopic;
            }
            if ("WINDOW_SIZE".equals(attribute)) {
                return String.valueOf(windowSize);
            }
            if ("MAX_WINDOWS".equals(attribute)) {
                return String.valueOf(maxWindows);
            }
            if ("assignedRawPartitions".equals(attribute)) {
                synchronized (assignedRawPartitions) {
                    return assignedRawPartitions.toString();
                }
            }
            if ("RAW_TOPICS".equals(attribute)) {
                return rawTopic;
            }
            if ("CLEAN_TOPIC".equals(attribute)) {
                return cleanTopic;
            }
            if ("GAP_TOPIC".equals(attribute)) {
                return gapTopic;
            }
            if ("BOOTSTRAP_SERVERS".equals(attribute)) {
                return props.getProperty("bootstrap.servers");
            }
            if ("STATE_DIR_CONFIG".equals(attribute)) {
                return props.getProperty("state.dir");
            }
            if ("PERSIST_INTERVAL_MS".equals(attribute)) {
                return String.valueOf(PartitionDedupApp.PERSIST_INTERVAL_MS);
            }
            if ("GAP_EMIT_INTERVAL_SEC".equals(attribute)) {
                return String.valueOf(PartitionDedupApp.GAP_EMIT_INTERVAL_SEC);
            }
            if ("APPLICATION_ID".equals(attribute)) {
                return applicationId;
            }
            throw new AttributeNotFoundException(attribute);
        }
        @Override
        public void setAttribute(Attribute attribute) throws AttributeNotFoundException, InvalidAttributeValueException, MBeanException, ReflectionException {
            throw new ReflectionException(new UnsupportedOperationException("Read-only"));
        }
        @Override
        public AttributeList getAttributes(String[] attributes) {
            AttributeList list = new AttributeList();
            for (String attr : attributes) {
                String value = props.getProperty(attr);
                if (value != null) {
                    list.add(new Attribute(attr, value));
                }
            }
            return list;
        }
        @Override
        public AttributeList setAttributes(AttributeList attributes) {
            return new AttributeList(); // read-only
        }
        @Override
        public Object invoke(String actionName, Object[] params, String[] signature) {
            return null;
        }
        @Override
        public MBeanInfo getMBeanInfo() {
            java.util.List<MBeanAttributeInfo> attrList = new java.util.ArrayList<>();
            for (String name : props.stringPropertyNames()) {
                attrList.add(new MBeanAttributeInfo(name, "java.lang.String", "Kafka Streams property", true, false, false));
            }
            attrList.add(new MBeanAttributeInfo("topics", "java.lang.String", "Topics handled by this app", true, false, false));
            attrList.add(new MBeanAttributeInfo("WINDOW_SIZE", "java.lang.String", "GapDetector window size (from env)", true, false, false));
            attrList.add(new MBeanAttributeInfo("MAX_WINDOWS", "java.lang.String", "GapDetector max windows (from env)", true, false, false));
            attrList.add(new MBeanAttributeInfo("RAW_TOPICS", "java.lang.String", "Raw topic name", true, false, false));
            attrList.add(new MBeanAttributeInfo("CLEAN_TOPIC", "java.lang.String", "Clean topic name", true, false, false));
            attrList.add(new MBeanAttributeInfo("GAP_TOPIC", "java.lang.String", "Gap topic name", true, false, false));
            attrList.add(new MBeanAttributeInfo("BOOTSTRAP_SERVERS", "java.lang.String", "Kafka bootstrap servers", true, false, false));
            attrList.add(new MBeanAttributeInfo("STATE_DIR_CONFIG", "java.lang.String", "Kafka Streams state directory", true, false, false));
            attrList.add(new MBeanAttributeInfo("PERSIST_INTERVAL_MS", "java.lang.String", "GapDetector persist interval (ms)", true, false, false));
            attrList.add(new MBeanAttributeInfo("GAP_EMIT_INTERVAL_SEC", "java.lang.String", "Gap emit interval (seconds)", true, false, false));
            attrList.add(new MBeanAttributeInfo("assignedRawPartitions", "java.lang.String", "Partitions assigned to this instance for the raw topic", true, false, false));
            attrList.add(new MBeanAttributeInfo("APPLICATION_ID", "java.lang.String", "Kafka Streams application.id", true, false, false));

            return new MBeanInfo(
                this.getClass().getName(),
                "Dynamic MBean for Kafka Streams Properties and runtime info",
                attrList.toArray(new MBeanAttributeInfo[0]),
                null, // constructors
                null, // operations
                null  // notifications
            );
        }
    }


    public static synchronized void registerProcessorMBean(DedupAndGapProcessor processor) {
        int partition = processor.getPartition();
        if (partition == -1) {
            LOG.info("Skipping registration of GapDetectors MBean for uninitialized partition (-1)");
            return;
        }
        try {
            MBeanServer mbs = ManagementFactory.getPlatformMBeanServer();
            ObjectName name = new ObjectName("nu.sitia.airgap:type=GapDetectors,partition=" + partition);
            // Always unregister first if already registered for this partition
            if (mbs.isRegistered(name)) {
                try {
                    mbs.unregisterMBean(name);
                } catch (Exception ignored) {}
            }
            GapDetectorsDynamicMBean mbean = new GapDetectorsDynamicMBean(processor);
            mbs.registerMBean(mbean, name);
            LOG.debug("Registered GapDetectors MBean for partition {} as {}", partition, name);
        } catch (Exception e) {
            e.printStackTrace();
        }
    }

    // // Call this whenever a new gap detector is added
    // public static synchronized void refreshGapDetectorsMBean(Map<String, GapDetector> gapDetectors) {
    //     registerGapDetectorsMBean(gapDetectors);
    // }

    public static void registerPropsMBean(Properties props, String rawTopic, String cleanTopic, String gapTopic, String applicationId, java.util.Set<Integer> assignedRawPartitions, long windowSize, int maxWindows) {
        try {
            MBeanServer mbs = ManagementFactory.getPlatformMBeanServer();
            ObjectName propsName = new ObjectName("nu.sitia.airgap:type=Props");
            PropsDynamicMBean propsMBean = new PropsDynamicMBean(props, rawTopic, cleanTopic, gapTopic, applicationId, assignedRawPartitions, windowSize, maxWindows);
            if (!mbs.isRegistered(propsName)) {
                mbs.registerMBean(propsMBean, propsName);
            }
        } catch (Exception e) {
            e.printStackTrace();
        }
    }
}
