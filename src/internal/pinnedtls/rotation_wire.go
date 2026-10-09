package pinnedtls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"sitia.nu/airgap/src/protocol"
)

const RotationMessageID = "PIN_TLS_V1"
const MaxRotationPayload = 8192

func validFingerprint(value string) bool {
	if !strings.HasPrefix(value, "SHA256:") {
		return false
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(value, "SHA256:"))
	return err == nil && len(decoded) == 32 && value == "SHA256:"+base64.StdEncoding.EncodeToString(decoded)
}

type RotationMessage struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Old     string `json:"old"`
	New     string `json:"new"`
	Peer    string `json:"peer"`
	Nonce   string `json:"nonce"`
	Public  string `json:"public"`
	Reason  string `json:"reason"`
	Proof   string `json:"proof"`
}

func rotationID(old, next string) string {
	sum := sha256.Sum256([]byte("airgap/pinned-rotation/id/v1\n" + old + "\n" + next))
	return hex.EncodeToString(sum[:])
}

func strictJSON(data []byte, output any) error {
	// Reject duplicate fields as well as unknown fields, avoiding parser ambiguity.
	d := json.NewDecoder(bytes.NewReader(data))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("expected JSON object")
	}
	seen := make(map[string]bool)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return fmt.Errorf("duplicate/invalid JSON field")
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(output); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func DecodeRotationFrame(frame []byte) (RotationMessage, error) {
	var message RotationMessage
	if len(frame) < 13 || frame[0] != protocol.TYPE_KEY_EXCHANGE ||
		binary.BigEndian.Uint16(frame[1:3]) != 1 || binary.BigEndian.Uint16(frame[3:5]) != 1 {
		return message, fmt.Errorf("invalid/multipart pinned rotation frame")
	}
	n := int(binary.BigEndian.Uint16(frame[5:7]))
	if n != len(RotationMessageID) || len(frame) < 13+n || string(frame[7:7+n]) != RotationMessageID {
		return message, fmt.Errorf("invalid pinned rotation discriminator")
	}
	size := int(binary.BigEndian.Uint16(frame[11+n : 13+n]))
	if size > MaxRotationPayload || len(frame) != 13+n+size {
		return message, fmt.Errorf("invalid pinned rotation payload length")
	}
	payload := frame[13+n:]
	if string(frame[7+n:11+n]) != protocol.CalculateChecksum(payload) {
		return message, fmt.Errorf("invalid pinned rotation checksum")
	}
	if err := strictJSON(payload, &message); err != nil {
		return message, err
	}
	if message.Version != 1 || len(message.Peer) > 128 || strings.ContainsAny(message.Peer, "\r\n\t") ||
		len(message.Reason) > 256 || !validFingerprint(message.Old) || !validFingerprint(message.New) ||
		message.Old == message.New || message.ID != rotationID(message.Old, message.New) {
		return message, fmt.Errorf("invalid pinned rotation version/binding")
	}
	switch message.Kind {
	case "begin":
		if message.Peer != "" || message.Nonce != "" || message.Public != "" || message.Reason != "" || message.Proof != "" {
			return message, fmt.Errorf("unexpected begin fields")
		}
	case "challenge":
		nonce, err := base64.StdEncoding.DecodeString(message.Nonce)
		if message.Peer == "" || err != nil || len(nonce) != 32 || message.Public != "" || message.Reason != "" || message.Proof != "" {
			return message, fmt.Errorf("invalid challenge fields")
		}
	case "request":
		nonce, err := base64.StdEncoding.DecodeString(message.Nonce)
		if message.Peer == "" || err != nil || len(nonce) != 32 || message.Public == "" || message.Proof == "" || message.Reason != "" {
			return message, fmt.Errorf("invalid request fields")
		}
	case "accepted":
		if message.Peer == "" || message.Nonce != "" || message.Public != "" || message.Reason != "" || message.Proof != "" {
			return message, fmt.Errorf("invalid acceptance fields")
		}
	case "rejected":
		if message.Reason == "" || message.Nonce != "" || message.Public != "" || message.Proof != "" {
			return message, fmt.Errorf("invalid rejection fields")
		}
	default:
		return message, fmt.Errorf("unknown pinned rotation kind")
	}
	return message, nil
}

func WriteRotation(w io.Writer, message RotationMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(payload) > MaxRotationPayload {
		return fmt.Errorf("rotation message exceeds payload bound")
	}
	frames := protocol.FormatMessage(protocol.TYPE_KEY_EXCHANGE, RotationMessageID, payload, 16384)
	if len(frames) != 1 {
		return fmt.Errorf("rotation must fit one frame")
	}
	_, err = io.Copy(w, bytes.NewReader(frames[0]))
	return err
}

func ReadRotation(r io.Reader) (RotationMessage, error) {
	var header [7]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return RotationMessage{}, err
	}
	if int(binary.BigEndian.Uint16(header[5:7])) != len(RotationMessageID) {
		return RotationMessage{}, fmt.Errorf("unexpected rotation frame ID length")
	}
	tail := make([]byte, len(RotationMessageID)+6)
	if _, err := io.ReadFull(r, tail); err != nil {
		return RotationMessage{}, err
	}
	size := int(binary.BigEndian.Uint16(tail[len(tail)-2:]))
	if size > MaxRotationPayload {
		return RotationMessage{}, fmt.Errorf("rotation payload too large")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return RotationMessage{}, err
	}
	return DecodeRotationFrame(append(append(header[:], tail...), payload...))
}

func IsRotationFrame(frame []byte) bool {
	if len(frame) < 7 {
		return false
	}
	n := int(binary.BigEndian.Uint16(frame[5:7]))
	return len(frame) >= 7+n && strings.HasPrefix(string(frame[7:7+n]), "PIN_TLS")
}

func proofDigest(message RotationMessage) [32]byte {
	message.Proof = ""
	data, _ := json.Marshal(message) // Fixed string/int fields cannot fail.
	return sha256.Sum256(append([]byte("airgap/pinned-rotation/v1\n"), data...))
}

func SignRotation(message *RotationMessage, key *ecdsa.PrivateKey) error {
	digest := proofDigest(*message)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return err
	}
	message.Proof = base64.StdEncoding.EncodeToString(signature)
	return nil
}

func verifyRotation(message RotationMessage) (Entry, error) {
	if message.Kind != "request" || message.Reason != "" || message.Peer == "" {
		return Entry{}, fmt.Errorf("invalid rotation request fields")
	}
	spki, err := base64.StdEncoding.DecodeString(message.Public)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid rotation public key")
	}
	public, err := x509.ParsePKIXPublicKey(spki)
	if err != nil || ValidatePublicKey(public) != nil || Fingerprint(spki) != message.New {
		return Entry{}, fmt.Errorf("rotation public-key fingerprint/policy mismatch")
	}
	proof, err := base64.StdEncoding.DecodeString(message.Proof)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid new-key proof")
	}
	digest := proofDigest(message)
	if !ecdsa.VerifyASN1(public.(*ecdsa.PublicKey), digest[:], proof) {
		return Entry{}, fmt.Errorf("invalid new-key possession proof")
	}
	return Entry{Version: 1, Peer: message.Peer, Role: "client", Fingerprint: message.New,
		PublicKey: string(pemPublic(spki))}, nil
}
