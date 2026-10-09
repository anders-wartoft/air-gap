package pinnedtls

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"time"
)

type RotationSession struct {
	challenge RotationMessage
	expires   time.Time
	now       func() time.Time
}

func (s *RotationSession) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (m *Manager) HandleRotation(session *RotationSession, conn *tls.Conn, message RotationMessage, dir string, automatic bool) RotationMessage {
	response := RotationMessage{Version: 1, Kind: "rejected", ID: message.ID,
		Old: message.Old, New: message.New}
	reject := func(reason string) RotationMessage {
		response.Reason = reason
		if len(response.Reason) > 256 {
			response.Reason = response.Reason[:256]
		}
		return response
	}
	m.mu.Lock()
	fingerprint, peer, err := m.active.authorize(conn.ConnectionState())
	m.mu.Unlock()
	if err != nil || message.Old != fingerprint {
		return reject("old key not authenticated/authorized")
	}
	response.Peer = peer.Peer
	if message.Kind == "begin" {
		if !session.expires.IsZero() {
			return reject("one challenge permitted per connection")
		}
		if message.Proof != "" || message.Public != "" || message.Peer != "" ||
			message.Nonce != "" || message.Reason != "" {
			return reject("invalid begin fields")
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return reject("challenge randomness failed")
		}
		response.Kind, response.Peer = "challenge", peer.Peer
		response.Nonce = base64.StdEncoding.EncodeToString(nonce[:])
		session.challenge = response
		session.expires = session.currentTime().Add(30 * time.Second)
		return response
	}
	if message.Kind != "request" || session.challenge.Kind != "challenge" ||
		session.currentTime().After(session.expires) || message.ID != session.challenge.ID ||
		message.New != session.challenge.New || message.Peer != peer.Peer ||
		message.Nonce != session.challenge.Nonce {
		return reject("invalid/expired connection challenge")
	}
	session.challenge.Kind = "consumed"
	if err := m.CommitRotation(message, dir, automatic); err != nil {
		return reject(err.Error())
	}
	response.Kind, response.Peer = "accepted", peer.Peer
	return response
}

func RequestRotation(conn *tls.Conn, oldConfig *tls.Config, candidate *Snapshot) (string, error) {
	leaf := oldConfig.Certificates[0].Leaf
	if leaf == nil {
		var err error
		leaf, err = x509.ParseCertificate(oldConfig.Certificates[0].Certificate[0])
		if err != nil {
			return "", err
		}
	}
	old, next := Fingerprint(leaf.RawSubjectPublicKeyInfo),
		Fingerprint(candidate.config.Certificates[0].Leaf.RawSubjectPublicKeyInfo)
	begin := RotationMessage{Version: 1, Kind: "begin", Old: old, New: next, ID: rotationID(old, next)}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return begin.ID, err
	}
	if err := WriteRotation(conn, begin); err != nil {
		return begin.ID, err
	}
	challenge, err := ReadRotation(conn)
	if err != nil {
		return begin.ID, err
	}
	if challenge.ID != begin.ID || challenge.Old != old || challenge.New != next ||
		challenge.Kind != "challenge" || challenge.Peer == "" || len(challenge.Nonce) != 44 {
		return begin.ID, fmt.Errorf("rotation rejected: %s", challenge.Reason)
	}
	request := challenge
	request.Kind = "request"
	request.Public = base64.StdEncoding.EncodeToString(candidate.config.Certificates[0].Leaf.RawSubjectPublicKeyInfo)
	key, ok := candidate.config.Certificates[0].PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return begin.ID, fmt.Errorf("unsupported rotation private key")
	}
	if err := SignRotation(&request, key); err != nil {
		return begin.ID, err
	}
	if err := WriteRotation(conn, request); err != nil {
		return begin.ID, err
	}
	response, err := ReadRotation(conn)
	if err != nil {
		return begin.ID, err
	}
	if response.Kind != "accepted" || response.ID != begin.ID || response.Old != old ||
		response.New != next || response.Peer != challenge.Peer || response.Reason != "" {
		return begin.ID, fmt.Errorf("rotation rejected: %s", response.Reason)
	}
	return begin.ID, nil
}
