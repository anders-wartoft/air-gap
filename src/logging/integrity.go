package logging

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"
)

// Attribute names used for log integrity. They live in a vendor-specific
// namespace, which the OpenTelemetry log data model permits for attributes that
// are not covered by a semantic convention.
const (
	AttrIntegrityVersion  = "airgap.log.integrity.version"
	AttrHashAlg           = "airgap.log.hash.alg"
	AttrHash              = "airgap.log.hash"
	AttrSequence          = "airgap.log.seq"
	AttrChain             = "airgap.log.chain"
	AttrChainPrev         = "airgap.log.chain.prev"
	AttrAnchor            = "airgap.log.anchor"
	AttrAnchorFrom        = "airgap.log.anchor.from"
	AttrAnchorTo          = "airgap.log.anchor.to"
	AttrChainLength       = "airgap.log.chain.length"
	AttrCheckpointHash    = "airgap.log.checkpoint.hash"
	AttrSignature         = "airgap.log.signature"
	AttrSignatureAlg      = "airgap.log.signature.alg"
	AttrSignerID          = "airgap.log.signer.id"
	AttrKeyID             = "airgap.log.key.id"
	AttrCertificateSerial = "airgap.log.certificate.serial"
	AttrCertificateDER    = "airgap.log.certificate.der"
	AttrSignatureError    = "airgap.log.signature.error"
	AttrProcessStart      = "airgap.log.process.start_unix_nano"
	AttrRunID             = "airgap.log.run.id"
	AttrSegmentID         = "airgap.log.segment.id"
	AttrWindowID          = "airgap.log.window.id"
	// AttrChainRestart marks the first record of a new chain segment. A verifier
	// must be able to tell a declared restart, such as one caused by configuring
	// the chain after startup, from a silent break caused by tampering.
	AttrChainRestart = "airgap.log.chain.restart"
)

// chainDomain separates these hashes from any other use of the same algorithm,
// so a chain value can never be mistaken for a hash computed elsewhere.
const chainDomain = "airgap-log-chain-v1"

// integrityVersion names the Air-gap Log Integrity Format that produced a
// record. It is emitted on every stamped record so that a verifier can select
// the hashing, chaining and checkpoint rules from the record itself instead of
// knowing out of band which version wrote the log. It is also the version
// framed into the signed checkpoint statement, so the two can never drift.
//
// The value is deliberately not part of any hash input. Record hashes cover the
// body only, and the checkpoint statement frames this version explicitly, so
// rewriting the attribute makes a verifier apply rules that then fail rather
// than letting it silently downgrade.
const integrityVersion = "v1"

// anchorBody is the body of a generated anchor record. It is constant so that
// an anchor is recognisable without parsing attributes.
const anchorBody = "airgap log chain anchor"

// Attr is a single log attribute. A slice preserves emission order, which keeps
// output byte-stable and therefore diffable in tests.
type Attr struct {
	Key   string
	Value string
	// IsInt marks values that must be encoded as an OTLP integer rather than a
	// string.
	IsInt bool
	// IsBool marks values that must be encoded as an OTLP boolean.
	IsBool bool
}

type hashAlgorithm struct {
	name    string
	factory func() hash.Hash
}

// supportedHashAlgorithms maps a canonical name to its constructor. Only
// collision-resistant algorithms are offered; MD5 and SHA-1 are deliberately
// absent.
var supportedHashAlgorithms = map[string]hashAlgorithm{
	"SHA256":   {name: "SHA256", factory: func() hash.Hash { return sha256.New() }},
	"SHA384":   {name: "SHA384", factory: func() hash.Hash { return sha512.New384() }},
	"SHA512":   {name: "SHA512", factory: func() hash.Hash { return sha512.New() }},
	"SHA3-256": {name: "SHA3-256", factory: func() hash.Hash { return sha3.New256() }},
	"SHA3-384": {name: "SHA3-384", factory: func() hash.Hash { return sha3.New384() }},
	"SHA3-512": {name: "SHA3-512", factory: func() hash.Hash { return sha3.New512() }},
}

// SupportedHashAlgorithms returns the accepted algorithm names in a stable
// order, for error messages and documentation.
func SupportedHashAlgorithms() []string {
	return []string{"SHA256", "SHA384", "SHA512", "SHA3-256", "SHA3-384", "SHA3-512"}
}

// normalizeHashName accepts the common spellings of an algorithm name so that
// sha-256, SHA256 and sha3_256 all resolve.
func normalizeHashName(name string) string {
	upper := strings.ToUpper(strings.TrimSpace(name))
	upper = strings.ReplaceAll(upper, "_", "-")

	if strings.HasPrefix(upper, "SHA3-") {
		return upper
	}

	// Collapse SHA-256 to SHA256 without disturbing the SHA3- family.
	return strings.ReplaceAll(upper, "SHA-", "SHA")
}

// integrityState carries the running hash chain. All access happens under the
// emit mutex in logging.go, so the state needs no lock of its own; serialising
// on the single writer is what keeps sequence numbers and chain values
// consistent with the order records actually reach the output.
type integrityState struct {
	enabled   bool
	algorithm hashAlgorithm
	chainLen  int

	// seq is the sequence number assigned to the next record.
	seq uint64
	// chain is the current cumulative chain value.
	chain []byte
	// sinceAnchor counts records emitted since the last anchor.
	sinceAnchor int
	// anchorFrom is the first sequence number covered by the next anchor.
	anchorFrom uint64
	// restartPending marks that the next chained record opens a new segment.
	restartPending bool
	// signer is optional. When configured, completed anchors carry a signature.
	signer    *logSigner
	runID     string
	segmentID string
	windowID  string
}

// configureIntegrity validates and installs a hash algorithm. An empty or
// "none" name disables hashing and, with it, the chain.
func (s *integrityState) configureIntegrity(algName string) error {
	trimmed := strings.TrimSpace(algName)
	if trimmed == "" || strings.EqualFold(trimmed, "none") || strings.EqualFold(trimmed, "off") {
		s.enabled = false
		s.algorithm = hashAlgorithm{}
		return nil
	}

	algorithm, ok := supportedHashAlgorithms[normalizeHashName(trimmed)]
	if !ok {
		return fmt.Errorf("unsupported hash algorithm %q, supported values are %s and none",
			algName, strings.Join(SupportedHashAlgorithms(), ", "))
	}

	s.enabled = true
	s.algorithm = algorithm
	s.resetChain()
	return nil
}

// configureChainLength sets the anchor interval. Zero disables chaining, so
// records carry only their own payload hash.
func (s *integrityState) configureChainLength(length int) error {
	if length < 0 {
		return fmt.Errorf("hash-chain must be 0 or a positive integer, got %d", length)
	}

	s.chainLen = length
	s.resetChain()
	return nil
}

// resetChain restarts the chain from a genesis value bound to this process.
// Binding to the service identity and start time stops a chain segment from one
// process being spliced into the log of another.
//
// The sequence counter is deliberately left alone. Reconfiguration can happen
// after records have already been written, and renumbering from zero would put
// duplicate sequence numbers in one stream, which a verifier cannot resolve.
// Sequence numbers are therefore unique for the lifetime of the process, and a
// restart is announced with AttrChainRestart instead.
func (s *integrityState) resetChain() {
	s.sinceAnchor = 0
	s.anchorFrom = s.seq
	s.restartPending = true
	if s.runID == "" {
		s.runID = newIntegrityID()
	}
	s.segmentID = newIntegrityID()
	s.windowID = newIntegrityID()

	if !s.enabled {
		s.chain = nil
		return
	}

	h := s.algorithm.factory()
	h.Write([]byte(chainDomain))
	h.Write([]byte{0})
	h.Write([]byte("genesis"))
	h.Write([]byte{0})
	h.Write([]byte(resourceServiceName()))
	h.Write([]byte{0})
	h.Write([]byte(resourceInstanceID()))
	h.Write([]byte{0})

	start := make([]byte, 8)
	binary.BigEndian.PutUint64(start, uint64(processStartUnixNano()))
	h.Write(start)

	s.chain = h.Sum(nil)
}

// chainingEnabled reports whether a chain is maintained in addition to the
// per-record hash.
func (s *integrityState) chainingEnabled() bool {
	return s.enabled && s.chainLen > 0
}

// stamp consumes the next sequence number and returns the integrity attributes
// for a record. The hash input is exactly the UTF-8 bytes of the fully formatted
// body string before JSON escaping. No OTLP envelope serialization or JSON
// canonicalization is involved. The chain separately commits to the original
// event timestamp, severity number and severity text.
func (s *integrityState) stamp(timestamp time.Time, level LogLevel, body string) []Attr {
	if !s.enabled {
		return nil
	}

	h := s.algorithm.factory()
	h.Write([]byte(body))
	payloadHash := h.Sum(nil)

	seq := s.seq
	s.seq++

	attrs := []Attr{
		{Key: AttrIntegrityVersion, Value: integrityVersion},
		{Key: AttrHashAlg, Value: s.algorithm.name},
		{Key: AttrHash, Value: hex.EncodeToString(payloadHash)},
		{Key: AttrSequence, Value: strconv.FormatUint(seq, 10), IsInt: true},
		{Key: AttrRunID, Value: s.runID},
	}

	if !s.chainingEnabled() {
		return attrs
	}

	previous := s.chain
	s.chain = s.advanceChain(
		previous,
		seq,
		timestamp.UnixNano(),
		severityNumber(level),
		severityText(level),
		payloadHash,
	)
	s.sinceAnchor++

	// chain.prev makes a record verifiable against its predecessor without
	// replaying the whole stream from the genesis value.
	attrs = append(attrs,
		Attr{Key: AttrSegmentID, Value: s.segmentID},
		Attr{Key: AttrWindowID, Value: s.windowID},
		Attr{Key: AttrChainPrev, Value: hex.EncodeToString(previous)},
		Attr{Key: AttrChain, Value: hex.EncodeToString(s.chain)},
	)

	if s.restartPending {
		attrs = append(attrs, Attr{Key: AttrChainRestart, Value: "true", IsBool: true})
		s.restartPending = false
	}

	return attrs
}

// advanceChain folds one record into the chain. Sequence, timestamp and severity
// are part of the input, so reordering or editing operator-visible metadata
// invalidates the chain even when two payloads are identical.
func (s *integrityState) advanceChain(
	previous []byte,
	seq uint64,
	timeUnixNano int64,
	severityNo int,
	severity string,
	payloadHash []byte,
) []byte {
	h := s.algorithm.factory()
	h.Write([]byte(chainDomain))
	h.Write([]byte{0})
	h.Write(previous)

	var fixed [8]byte
	binary.BigEndian.PutUint64(fixed[:], seq)
	h.Write(fixed[:])
	binary.BigEndian.PutUint64(fixed[:], uint64(timeUnixNano))
	h.Write(fixed[:])

	var severityBytes [4]byte
	binary.BigEndian.PutUint32(severityBytes[:], uint32(severityNo))
	h.Write(severityBytes[:])
	writeFramedHashString(h, severity)
	h.Write(payloadHash)

	return h.Sum(nil)
}

func writeFramedHashString(h hash.Hash, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	h.Write(length[:])
	h.Write([]byte(value))
}

// anchorDue reports whether an anchor record should follow the record that was
// just emitted.
func (s *integrityState) anchorDue() bool {
	return s.chainingEnabled() && s.sinceAnchor >= s.chainLen
}

// anchorAttributes returns the attributes describing the range an anchor
// covers, and arms the next window. It must be called before the anchor record
// itself is stamped, because the anchor is chained like any other record and its
// own sequence number closes the range. Call finishAnchor once the anchor has
// been written.
func (s *integrityState) anchorAttributes() []Attr {
	from := s.anchorFrom
	to := s.seq

	// The anchor consumes one sequence number, so the next window starts after
	// it.
	s.anchorFrom = to + 1

	return []Attr{
		{Key: AttrAnchor, Value: "true", IsBool: true},
		{Key: AttrAnchorFrom, Value: strconv.FormatUint(from, 10), IsInt: true},
		{Key: AttrAnchorTo, Value: strconv.FormatUint(to, 10), IsInt: true},
		{Key: AttrChainLength, Value: strconv.Itoa(s.chainLen), IsInt: true},
	}
}

// finishAnchor opens a new window after an anchor record has been written.
// Stamping the anchor itself incremented the counter, so it is cleared here
// rather than in anchorAttributes; otherwise the anchor would consume a slot in
// the window it just closed.
func (s *integrityState) finishAnchor() {
	s.sinceAnchor = 0
	s.windowID = newIntegrityID()
}

func newIntegrityID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("generate log integrity identity: " + err.Error())
	}
	return hex.EncodeToString(value[:])
}
