package logging

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

type integrityTestVector struct {
	Schema string `json:"schema"`
	Chain  struct {
		DomainUTF8            string `json:"domain_utf8"`
		DomainSeparatorHex    string `json:"domain_separator_hex"`
		HashAlgorithm         string `json:"hash_algorithm"`
		PreviousChainHex      string `json:"previous_chain_hex"`
		Sequence              uint64 `json:"sequence"`
		TimeUnixNano          string `json:"time_unix_nano"`
		SeverityNumber        int    `json:"severity_number"`
		SeverityText          string `json:"severity_text"`
		BodyUTF8              string `json:"body_utf8"`
		BodyHex               string `json:"body_hex"`
		PayloadHashHex        string `json:"payload_hash_hex"`
		FramedSeverityTextHex string `json:"framed_severity_text_hex"`
		ChainInputHex         string `json:"chain_input_hex"`
		ChainValueHex         string `json:"chain_value_hex"`
	} `json:"chain"`
	Checkpoint struct {
		DomainUTF8             string `json:"domain_utf8"`
		DomainSeparatorHex     string `json:"domain_separator_hex"`
		IntegrityVersion       string `json:"integrity_version"`
		HashAlgorithm          string `json:"hash_algorithm"`
		SignatureAlgorithm     string `json:"signature_algorithm"`
		SignerID               string `json:"signer_id"`
		KeyID                  string `json:"key_id"`
		RunID                  string `json:"run_id"`
		SegmentID              string `json:"segment_id"`
		WindowID               string `json:"window_id"`
		ServiceName            string `json:"service_name"`
		ServiceInstanceID      string `json:"service_instance_id"`
		ProcessStartUnixNano   string `json:"process_start_unix_nano"`
		AnchorFrom             uint64 `json:"anchor_from"`
		AnchorTo               uint64 `json:"anchor_to"`
		AnchorPreviousChainHex string `json:"anchor_previous_chain_hex"`
		AnchorFinalChainHex    string `json:"anchor_final_chain_hex"`
		TestOnlyEd25519SeedHex string `json:"test_only_ed25519_seed_hex"`
		PublicKeyRawHex        string `json:"public_key_raw_hex"`
		PublicKeySPKIDERHex    string `json:"public_key_spki_der_hex"`
		CheckpointStatementHex string `json:"checkpoint_statement_hex"`
		CheckpointHashHex      string `json:"checkpoint_hash_hex"`
		SignatureHex           string `json:"signature_hex"`
		SignatureBase64        string `json:"signature_base64"`
	} `json:"checkpoint"`
}

func loadIntegrityTestVector(t *testing.T) integrityTestVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/log-integrity-v1-test-vector.json")
	if err != nil {
		t.Fatalf("read integrity test vector: %v", err)
	}
	var vector integrityTestVector
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("decode integrity test vector: %v", err)
	}
	if vector.Schema != "airgap-log-integrity-test-vector-v1" {
		t.Fatalf("unexpected integrity test vector schema %q", vector.Schema)
	}
	return vector
}

func vectorHex(t *testing.T, label, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	return decoded
}

func TestVersion1PublicChainTestVector(t *testing.T) {
	vector := loadIntegrityTestVector(t)
	chain := vector.Chain
	if chain.DomainUTF8 != chainDomain || chain.DomainSeparatorHex != "00" {
		t.Fatal("test vector chain domain does not match Version 1")
	}
	body := []byte(chain.BodyUTF8)
	if got := hex.EncodeToString(body); got != chain.BodyHex {
		t.Fatalf("body bytes = %s, want %s", got, chain.BodyHex)
	}
	payloadHash := sha256.Sum256(body)
	if got := hex.EncodeToString(payloadHash[:]); got != chain.PayloadHashHex {
		t.Fatalf("payload hash = %s, want %s", got, chain.PayloadHashHex)
	}
	timeUnixNano, err := strconv.ParseInt(chain.TimeUnixNano, 10, 64)
	if err != nil {
		t.Fatalf("parse timeUnixNano: %v", err)
	}
	previous := vectorHex(t, "previous chain", chain.PreviousChainHex)
	input := append([]byte(chainDomain), 0)
	input = append(input, previous...)
	var fixed [8]byte
	binary.BigEndian.PutUint64(fixed[:], chain.Sequence)
	input = append(input, fixed[:]...)
	binary.BigEndian.PutUint64(fixed[:], uint64(timeUnixNano))
	input = append(input, fixed[:]...)
	var severity [4]byte
	binary.BigEndian.PutUint32(severity[:], uint32(chain.SeverityNumber))
	input = append(input, severity[:]...)
	framedSeverity := frameSigningString(chain.SeverityText)
	input = append(input, framedSeverity...)
	input = append(input, payloadHash[:]...)
	if got := hex.EncodeToString(framedSeverity); got != chain.FramedSeverityTextHex {
		t.Fatalf("framed severity = %s, want %s", got, chain.FramedSeverityTextHex)
	}
	if got := hex.EncodeToString(input); got != chain.ChainInputHex {
		t.Fatalf("chain input = %s, want %s", got, chain.ChainInputHex)
	}
	state := integrityState{algorithm: supportedHashAlgorithms[chain.HashAlgorithm]}
	got := state.advanceChain(
		previous,
		chain.Sequence,
		timeUnixNano,
		chain.SeverityNumber,
		chain.SeverityText,
		payloadHash[:],
	)
	if encoded := hex.EncodeToString(got); encoded != chain.ChainValueHex {
		t.Fatalf("chain value = %s, want %s", encoded, chain.ChainValueHex)
	}
}

func TestVersion1PublicCheckpointTestVector(t *testing.T) {
	vector := loadIntegrityTestVector(t)
	checkpoint := vector.Checkpoint
	if checkpoint.DomainUTF8 != checkpointDomain ||
		checkpoint.DomainSeparatorHex != "00" {
		t.Fatal("test vector checkpoint domain does not match Version 1")
	}
	processStart, err := strconv.ParseUint(checkpoint.ProcessStartUnixNano, 10, 64)
	if err != nil {
		t.Fatalf("parse process start: %v", err)
	}
	statement := buildCheckpointStatement(checkpointStatementFields{
		version:          checkpoint.IntegrityVersion,
		hashAlgorithm:    checkpoint.HashAlgorithm,
		signatureAlg:     checkpoint.SignatureAlgorithm,
		signerID:         checkpoint.SignerID,
		keyID:            checkpoint.KeyID,
		runID:            checkpoint.RunID,
		segmentID:        checkpoint.SegmentID,
		windowID:         checkpoint.WindowID,
		serviceName:      checkpoint.ServiceName,
		serviceInstance:  checkpoint.ServiceInstanceID,
		processStartNano: processStart,
		anchorFrom:       checkpoint.AnchorFrom,
		anchorTo:         checkpoint.AnchorTo,
		previousChain: vectorHex(
			t, "checkpoint previous chain", checkpoint.AnchorPreviousChainHex,
		),
		finalChain: vectorHex(
			t, "checkpoint final chain", checkpoint.AnchorFinalChainHex,
		),
	})
	if got := hex.EncodeToString(statement); got != checkpoint.CheckpointStatementHex {
		t.Fatalf("checkpoint statement = %s, want %s", got, checkpoint.CheckpointStatementHex)
	}
	digest := sha256.Sum256(statement)
	if got := hex.EncodeToString(digest[:]); got != checkpoint.CheckpointHashHex {
		t.Fatalf("checkpoint hash = %s, want %s", got, checkpoint.CheckpointHashHex)
	}
	seed := vectorHex(t, "Ed25519 seed", checkpoint.TestOnlyEd25519SeedHex)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if got := hex.EncodeToString(publicKey); got != checkpoint.PublicKeyRawHex {
		t.Fatalf("raw public key = %s, want %s", got, checkpoint.PublicKeyRawHex)
	}
	spki, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	if got := hex.EncodeToString(spki); got != checkpoint.PublicKeySPKIDERHex {
		t.Fatalf("SPKI = %s, want %s", got, checkpoint.PublicKeySPKIDERHex)
	}
	keyFingerprint := sha256.Sum256(spki)
	if got := "sha256:" + hex.EncodeToString(keyFingerprint[:]); got != checkpoint.KeyID {
		t.Fatalf("key ID = %s, want %s", got, checkpoint.KeyID)
	}
	signature := ed25519.Sign(privateKey, digest[:])
	if got := hex.EncodeToString(signature); got != checkpoint.SignatureHex {
		t.Fatalf("signature = %s, want %s", got, checkpoint.SignatureHex)
	}
	if got := base64.StdEncoding.EncodeToString(signature); got != checkpoint.SignatureBase64 {
		t.Fatalf("base64 signature = %s, want %s", got, checkpoint.SignatureBase64)
	}
	if !ed25519.Verify(publicKey, digest[:], signature) {
		t.Fatal("test-vector signature did not verify")
	}
}
