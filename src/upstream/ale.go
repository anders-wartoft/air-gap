package upstream

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"sitia.nu/airgap/src/logging"
	"sitia.nu/airgap/src/protocol"
)

// lostEventPrefix marks the substitute payload that upstream forwards when the
// original Kafka record cannot be delivered. Keep this exact marker in
// airgapLostEventMessage so downstream consumers can find every lost event. In
// text mode it starts the payload; a JSON template normally puts it in body.
const lostEventPrefix = "AIRGAP LOST EVENT"

// defaultAirgapLostEventMessage is the payload delivered in place of an
// undeliverable record. Every configurable variable is available as a
// placeholder, plus the lost-event context listed in lostEventContext.
const defaultAirgapLostEventMessage = lostEventPrefix +
	": Event {topic_name} {partition}:{row} couldn't be delivered due to {reason}." +
	" The message can be found in its entirety on upstream machine {upstream_id}, in {lostEventDirectory}." +
	" Max message length is {kafkaMaxMessageBytes}."

// defaultLostEvent is the log record written when an event is lost. It is the
// delivered guard without the prefix: the log already says what it is.
const defaultLostEvent = "INFO:Event {topic_name} {partition}:{row} couldn't be delivered due to {reason}." +
	" The message can be found in its entirety on upstream machine {upstream_id}, in {lostEventDirectory}."

// defaultLostEventDirectory holds the original payload of a lost event, so the
// guard can point at something that actually exists.
const defaultLostEventDirectory = "/airgap/lost_events/"

// defaultLostMessageTruncatedMessage is emitted when an old lost-event file has
// to be removed to reclaim disk space, so operators can alert on the condition.
const defaultLostMessageTruncatedMessage = "AIRGAP LOST MESSAGE TRUNCATED: The message file {fileName} created on {fileDate} was truncated by upstream due to lack of disc space. Current disc free space is {diskFreeBytes} bytes and the truncated file was {fileSize} bytes."

// lostEventDirectoryDisabled replaces {lostEventDirectory} and {lostEventFile}
// when no directory is configured, so a message never claims the original
// payload was kept when it was not.
const lostEventDirectoryDisabled = "no directory (lostEventDirectory is not configured)"

// lostEventDirectoryValue renders the lost-event directory for templates.
func lostEventDirectoryValue(c TransferConfiguration) string {
	if c.lostEventDirectory == "" {
		return lostEventDirectoryDisabled
	}
	return c.lostEventDirectory
}

// splitMessageID splits a "<topic>_<partition>_<offset>" message id into its
// three parts. The partition and offset are always the last two fields, so the
// split is done from the right to tolerate topic names containing underscores.
func splitMessageID(id string) (topic string, partition string, offset string) {
	lastSep := strings.LastIndex(id, "_")
	if lastSep < 0 {
		return id, "", ""
	}
	offset = id[lastSep+1:]
	rest := id[:lastSep]
	prevSep := strings.LastIndex(rest, "_")
	if prevSep < 0 {
		return rest, "", offset
	}
	return rest[:prevSep], rest[prevSep+1:], offset
}

// lostEventContext returns the placeholders that describe one lost event. They
// are available in every lost-event template on top of the configuration:
//
//	{topic_name}        Kafka topic of the lost record
//	{partition}         Kafka partition
//	{row}               Kafka offset within the partition
//	{reason}            why the record could not be delivered
//	{eventBytes}        size of the original payload in bytes
//	{lostEventFile}     file holding the original payload
//
// {lostEventDirectory}, {upstreamId} and every other configurable variable come
// from the configuration and need no context.
func lostEventContext(id string, reason string, lostEventFile string, eventBytes int) map[string]string {
	topic, partition, offset := splitMessageID(id)
	if lostEventFile == "" {
		lostEventFile = lostEventDirectoryDisabled
	}
	return map[string]string{
		"topic_name":    topic,
		"partition":     partition,
		"row":           offset,
		"reason":        reason,
		"eventBytes":    strconv.Itoa(eventBytes),
		"lostEventFile": lostEventFile,
	}
}

// lostEventPayload renders the Airgap Lost Event (ALE) guard that replaces a
// record which could not be delivered. The guard keeps the offset sequence
// continuous so the gap detector does not pull the event into resend bundles,
// while still making the loss explicit and auditable downstream.
func lostEventPayload(context map[string]string) ([]byte, error) {
	template := config.airgapLostEventMessage
	if template == "" {
		template = defaultAirgapLostEventMessage
	}
	switch config.airgapLostEventMessageFormat {
	case messageFormatJSON:
		rendered, err := renderJSONTemplate(template, context)
		if err != nil {
			return nil, err
		}
		return []byte(rendered), nil
	case messageFormatText:
		return []byte(renderTemplate(template, context)), nil
	default:
		return nil, fmt.Errorf("illegal airgapLostEventMessageFormat %q", config.airgapLostEventMessageFormat)
	}
}

// lostEventFileName turns a message id into a file name that is safe on any
// file system by replacing everything outside [A-Za-z0-9._-] with an underscore.
func lostEventFileName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func fileAgeDays(now time.Time, modTime time.Time) float64 {
	return now.Sub(modTime).Hours() / 24
}

func lostMessageTruncatedContext(fileName string, fileDate time.Time, diskFreeBytes int64, fileSize int64, action string) map[string]string {
	return map[string]string{
		"fileName":           filepath.Base(fileName),
		"fileDate":           fileDate.Format(time.RFC3339),
		"diskFreeBytes":      strconv.FormatInt(diskFreeBytes, 10),
		"fileSize":           strconv.FormatInt(fileSize, 10),
		"action":             action,
		"upstreamId":         config.id,
		"lostEventDirectory": config.lostEventDirectory,
	}
}

func emitLostMessageTruncated(transport TransportClient, fileName string, fileDate time.Time, diskFreeBytes int64, fileSize int64, action string) {
	spec := parseMessageSpec(config.lostMessageTruncatedMessage, logging.WARN)
	if !spec.enabled {
		return
	}
	attrs := logging.Event(logging.EventMessageTruncated,
		logging.Failure(),
		logging.Str("airgap.event.lost.file", fileName),
		logging.Int("airgap.event.lost.size", fileSize),
		logging.Int("airgap.event.lost.disk_free_bytes", diskFreeBytes),
		logging.Str("airgap.event.lost.action", action),
	)
	context := lostMessageTruncatedContext(fileName, fileDate, diskFreeBytes, fileSize, action)
	var rendered string
	if config.lostMessageTruncatedMessageFormat == messageFormatJSON {
		var err error
		rendered, err = renderJSONTemplate(spec.template, context)
		if err != nil {
			Logger.Warnf(attrs.With(logging.ErrorType(err)), "Could not render lostMessageTruncatedMessage for file %s: %v", fileName, err)
			return
		}
		Logger.Log(spec.level, attrs, rendered)
	} else {
		rendered = emitConfiguredMessage(spec, attrs, context)
	}
	if transport == nil || rendered == "" {
		return
	}
	messages := protocol.FormatMessage(protocol.TYPE_STATUS, "STATUS", []byte(rendered), config.payloadSize)
	if err := transport.SendMessages(messages); err != nil {
		Logger.Errorf(attrs.With(
			logging.Str("airgap.transport.kind", config.transport),
			logging.ErrorType(err),
		), "Could not send lost-message-truncated status for file %s: %v", fileName, err)
	}
}

func dirFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func removeLostEventFile(transport TransportClient, path string, info os.FileInfo, action string) bool {
	if err := os.Remove(path); err != nil {
		Logger.Warnf(logging.Event(logging.EventMessageTruncated,
			logging.Failure(),
			logging.Str("airgap.event.lost.file", path),
			logging.Str("airgap.event.lost.action", action),
			logging.ErrorType(err),
		), "Could not remove lost-event file %s: %v", path, err)
		return false
	}
	free, err := dirFreeBytes(filepath.Dir(path))
	if err != nil {
		free = -1
		Logger.Warnf(logging.Event(logging.EventMessageTruncated,
			logging.Failure(),
			logging.Str("airgap.event.lost.file", path),
			logging.ErrorType(err),
		), "Could not determine free disk space after removing lost-event file %s: %v", path, err)
	}
	emitLostMessageTruncated(transport, path, info.ModTime(), free, info.Size(), action)
	Logger.Infof(logging.Event(logging.EventMessageLost,
		logging.Str("airgap.event.lost.file", path),
		logging.Str("airgap.event.lost.action", action),
		logging.Int("airgap.event.lost.size", info.Size()),
		logging.Int("airgap.event.lost.disk_free_bytes", free),
	), "Removed lost-event file %s (%s)", path, action)
	return true
}

func compressOldLostEventFiles(transport TransportClient, dir string, now time.Time) int {
	if config.lostEventDirectoryCompressAfterDays <= 0 {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		Logger.Warnf(logging.Event(logging.EventMessageTruncated,
			logging.Failure(),
			logging.Str("airgap.event.lost.directory", dir),
			logging.ErrorType(err),
		), "Could not scan lost-event directory for compression: %v", err)
		return 0
	}
	compressed := 0
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if fileAgeDays(now, info.ModTime()) <= float64(config.lostEventDirectoryCompressAfterDays) {
			continue
		}
		zipPath := path + ".zip"
		if _, err := os.Lstat(zipPath); err == nil {
			Logger.Warnf(logging.Event(logging.EventMessageTruncated,
				logging.Failure(),
				logging.Str("airgap.event.lost.file", path),
				logging.Str("airgap.event.lost.archive", zipPath),
			), "Will not overwrite existing lost-event archive %s", zipPath)
			continue
		} else if !os.IsNotExist(err) {
			continue
		}
		source, err := os.Open(path)
		if err != nil {
			continue
		}
		tempArchive, err := os.CreateTemp(dir, "."+entry.Name()+".*.zip.tmp")
		if err != nil {
			source.Close()
			continue
		}
		tempPath := tempArchive.Name()
		zw := zip.NewWriter(tempArchive)
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			source.Close()
			zw.Close()
			tempArchive.Close()
			os.Remove(tempPath)
			continue
		}
		header.Name = filepath.Base(path)
		header.Method = zip.Deflate
		header.SetModTime(info.ModTime())
		writer, err := zw.CreateHeader(header)
		if err != nil {
			source.Close()
			zw.Close()
			tempArchive.Close()
			os.Remove(tempPath)
			continue
		}
		if _, err := io.Copy(writer, source); err != nil {
			source.Close()
			zw.Close()
			tempArchive.Close()
			os.Remove(tempPath)
			continue
		}
		if err := source.Close(); err != nil {
			zw.Close()
			tempArchive.Close()
			os.Remove(tempPath)
			continue
		}
		if err := zw.Close(); err != nil {
			tempArchive.Close()
			os.Remove(tempPath)
			continue
		}
		if err := tempArchive.Close(); err != nil {
			os.Remove(tempPath)
			continue
		}
		if err := os.Chmod(tempPath, info.Mode().Perm()); err != nil {
			os.Remove(tempPath)
			continue
		}
		if err := os.Chtimes(tempPath, info.ModTime(), info.ModTime()); err != nil {
			os.Remove(tempPath)
			continue
		}
		if err := os.Rename(tempPath, zipPath); err != nil {
			os.Remove(tempPath)
			continue
		}
		if !removeLostEventFile(transport, path, info, "compressed") {
			continue
		}
		compressed++
		Logger.Infof(logging.Event(logging.EventMessageLost,
			logging.Str("airgap.event.lost.file", path),
			logging.Str("airgap.event.lost.archive", zipPath),
		), "Compressed stale lost-event file %s to %s", path, zipPath)
	}
	return compressed
}

func deleteOldLostEventFiles(transport TransportClient, dir string, now time.Time) int {
	if config.lostEventDirectoryDeleteAfterDays <= 0 {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		Logger.Warnf(logging.Event(logging.EventMessageTruncated,
			logging.Failure(),
			logging.Str("airgap.event.lost.directory", dir),
			logging.ErrorType(err),
		), "Could not scan lost-event directory for deletion: %v", err)
		return 0
	}
	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if fileAgeDays(now, info.ModTime()) <= float64(config.lostEventDirectoryDeleteAfterDays) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !removeLostEventFile(transport, path, info, "deleted") {
			continue
		}
		deleted++
	}
	return deleted
}

type lostEventFileCandidate struct {
	path string
	info os.FileInfo
}

func oldestLostEventFiles(dir string) ([]lostEventFileCandidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	candidates := make([]lostEventFileCandidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		candidates = append(candidates, lostEventFileCandidate{
			path: filepath.Join(dir, entry.Name()),
			info: info,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].info.ModTime().Equal(candidates[j].info.ModTime()) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].info.ModTime().Before(candidates[j].info.ModTime())
	})
	return candidates, nil
}

func reclaimLostEventDirectorySpace(transport TransportClient, dir string, required int64, now time.Time) {
	reclaimLostEventDirectorySpaceWithFree(transport, dir, required, now, dirFreeBytes)
}

func reclaimLostEventDirectorySpaceWithFree(transport TransportClient, dir string, required int64, now time.Time, freeBytes func(string) (int64, error)) {
	if dir == "" || required <= 0 {
		return
	}
	// Delete before compressing so a file already beyond the deletion age does
	// not receive a fresh archive timestamp and escape retention.
	deleteOldLostEventFiles(transport, dir, now)
	compressOldLostEventFiles(transport, dir, now)

	free, err := freeBytes(dir)
	if err != nil {
		Logger.Warnf(logging.Event(logging.EventMessageTruncated,
			logging.Failure(),
			logging.Str("airgap.event.lost.directory", dir),
			logging.ErrorType(err),
		), "Could not determine free disk space in lost-event directory; emergency eviction skipped: %v", err)
		return
	}
	if free >= required {
		return
	}
	candidates, err := oldestLostEventFiles(dir)
	if err != nil {
		Logger.Warnf(logging.Event(logging.EventMessageTruncated,
			logging.Failure(),
			logging.Str("airgap.event.lost.directory", dir),
			logging.ErrorType(err),
		), "Could not scan lost-event directory for emergency eviction: %v", err)
		return
	}
	for _, candidate := range candidates {
		if !removeLostEventFile(transport, candidate.path, candidate.info, "evicted") {
			continue
		}
		free, err = freeBytes(dir)
		if err != nil || free >= required {
			return
		}
	}
}

// writeLostEvent stores the original payload of a lost event so it can be
// inspected afterwards. It returns the path that was written, or an empty
// string when the feature is disabled or the write failed. A failed write is
// never fatal: the guard is still delivered.
func writeLostEvent(transport TransportClient, id string, payload []byte) string {
	if config.lostEventDirectory == "" {
		return ""
	}
	if err := os.MkdirAll(config.lostEventDirectory, 0o750); err != nil {
		Logger.Warnf(logging.Event(logging.EventMessageLost,
			logging.Failure(),
			logging.Str("airgap.message.id", id),
			logging.Str("airgap.event.lost.directory", config.lostEventDirectory),
			logging.ErrorType(err),
		), "Could not create lost-event directory %s: %v", config.lostEventDirectory, err)
		return ""
	}
	reclaimLostEventDirectorySpace(transport, config.lostEventDirectory, int64(len(payload))+1024, time.Now())
	path := filepath.Join(config.lostEventDirectory,
		fmt.Sprintf("%s-%d.bin", lostEventFileName(id), time.Now().UnixNano()))
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		if info, statErr := os.Stat(path); statErr == nil {
			removeLostEventFile(transport, path, info, "write_failed")
		}
		Logger.Warnf(logging.Event(logging.EventMessageLost,
			logging.Failure(),
			logging.Str("airgap.message.id", id),
			logging.Str("airgap.event.lost.file", path),
			logging.ErrorType(err),
		), "Could not write lost-event file %s: %v", path, err)
		return ""
	}
	return path
}

// recordLostEvent accounts for one undeliverable record, optionally writes the
// original payload to the lost-event directory, writes the configurable
// lostEvent log record and returns the guard payload to forward in its place.
// It returns nil when lostEventGuard is disabled, meaning the record is
// discarded.
func recordLostEvent(transport TransportClient, id string, reason string, original []byte) []byte {
	atomic.AddInt64(&lostEvents, 1)
	atomic.AddInt64(&totalLostEvents, 1)

	lostEventFile := writeLostEvent(transport, id, original)
	context := lostEventContext(id, reason, lostEventFile, len(original))
	topic, partition, offset := splitMessageID(id)

	var guard []byte
	if config.lostEventGuard {
		var err error
		guard, err = lostEventPayload(context)
		if err != nil {
			Logger.Errorf(logging.Event(logging.EventMessageLost,
				logging.Failure(),
				logging.Str("airgap.message.id", id),
				logging.ErrorType(err),
			), "Could not render lost-event guard for id=%s: %v", id, err)
		}
	}

	attrs := logging.Event(logging.EventMessageLost,
		logging.Failure(),
		logging.Error("airgap.event.lost"),
		logging.Str("airgap.message.id", id),
		logging.Str("messaging.destination.name", topic),
		logging.Str("messaging.destination.partition.id", partition),
		logging.Str("messaging.kafka.offset", offset),
		logging.Int("airgap.message.size", int64(len(original))),
		logging.Str("airgap.event.lost.reason", reason),
		logging.Bool("airgap.event.lost.guard_sent", len(guard) > 0),
		logging.Str("airgap.event.lost.file", lostEventFile),
	)
	emitConfiguredMessage(parseMessageSpec(config.lostEvent, logging.INFO), attrs, context)
	return guard
}
