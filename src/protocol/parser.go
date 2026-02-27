package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"sitia.nu/airgap/src/logging"
)

var Logger = logging.Logger

// Try to assemble a message from the supplied message fragment and
// earlier received fragments, stored in the message cache
func assembleMessage(msg MessageCacheEntry) ([]byte, error) {
	result := []byte{}
	var i uint16
	for i = 1; i <= msg.len; i++ {
		part, exists := msg.val[i]
		// log.Printf("Assembling message part %d of %d %s\n", i, msg.len, part)
		if exists {
			result = append(result, part...)
		} else {
			val := fmt.Sprintf("Missing part %d of message", i)
			return []byte{}, errors.New(val)
		}
	}
	return result, nil
}

// Parse the message id and return topic, partition and offset
func ParseMessageId(id string) (string, string, string, error) {
	const delimiter = "_"
	parts := strings.Split(id, delimiter)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("invalid message id format")
	}
	return parts[0], parts[1], parts[2], nil
}

// Parse a message from a byte array. The message contains a header and a payload
// Header is
// uint8 1 byte MessageType (see protocol/message_types.go)
// uint16 2 bytes MessageNumber
// uint16 2 bytes NrMessages
// uint16 2 bytes Length of message ID
// ? bytes message ID
// 4 bytes checksum
// After the header, we have the payload
// 2 bytes length of payload
// payload ([] byte)
func ParseMessage(message []byte, cache *MessageCache) (uint8, string, []byte, error) {
	const minimumPackageLength = 13
	length := uint16(len(message))
	sofar := uint16(0)
	if length < minimumPackageLength {
		// Cannot possibly be a valid message
		return TYPE_ERROR, "", nil, fmt.Errorf("too short message. won't parse. length is: %d", len(message))
	}
	// First byte is the message type
	messageType := message[0]

	// Next is 2 bytes for the message number
	messageNumberBytes := message[1:3]
	messageNumber := binary.BigEndian.Uint16(messageNumberBytes)
	sofar = 3

	// Next 2 bytes are the number of messagesparse messaparse messa
	nrMessagesBytes := message[3:5]
	nrMessages := binary.BigEndian.Uint16(nrMessagesBytes)
	sofar = 5

	// Next 2 bytes are the length of the id
	idLengthBytes := message[5:7]
	idLength := binary.BigEndian.Uint16(idLengthBytes)
	sofar = 7

	if idLength > length-minimumPackageLength {
		// Cannot possibly be a valid message
		return TYPE_ERROR, "", nil, fmt.Errorf("too long message id length. Won't parse. Length is: %d", idLength)
	}

	// Next idLength bytes are the id
	id := string(message[7 : 7+idLength])
	sofar = 7 + idLength

	// Next 4 bytes are the checksum
	checksumBytes := message[7+idLength : 11+idLength]
	checksum := string(checksumBytes)
	sofar += 4

	if 2+sofar > length {
		// Cannot possibly be a valid message
		return TYPE_ERROR, "", nil, fmt.Errorf("reading payloadLengthBytes will proceed outside of the message")
	}
	// Next 2 bytes are the length of the payload
	payloadLengthBytes := message[11+idLength : 13+idLength]
	payloadLength := binary.BigEndian.Uint16(payloadLengthBytes)
	sofar += 2
	if payloadLength > length-minimumPackageLength-idLength {
		// Cannot possibly be a valid message
		return TYPE_ERROR, "", nil, fmt.Errorf("reading payloadLength bytes will proceed outside of the message. Message length: %d, max pointer: %d",
			length,
			length-minimumPackageLength)
	}

	// Next payloadLength bytes are the payload
	payload := message[minimumPackageLength+idLength : minimumPackageLength+idLength+payloadLength]
	sofar += payloadLength

	if length != sofar {
		return TYPE_ERROR, "", nil, fmt.Errorf("message length is: %d, but parsed data is: %d",
			length,
			sofar)
	}

	// verify the checksum
	calculatedChecksum := CalculateChecksum(payload)
	if calculatedChecksum != checksum && shouldValidateChecksum() {
		// The message was not transmitted properly.
		return TYPE_ERROR, "", nil, errors.New("invalid checksum")
	}

	if nrMessages > 1 {
		// Multi part message
		snapshot, complete := cache.AddPartAndSnapshot(id, messageNumber, nrMessages, payload)
		if complete {
			assembled, err := assembleMessage(snapshot)
			if err == nil {
				cache.RemoveEntry(id)
				return messageType, id, assembled, nil
			}
		}
		// Discard the part (but keep in the cache). The completed message is
		// returned only when all parts have arrived and assembled successfully.
		return TYPE_MULTIPART, "", nil, nil
	} else {
		// Only single message
		return messageType, id, payload, nil
	}
}
