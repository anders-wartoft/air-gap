package protocol

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// ISO 8601 timestamp
func GetTimestamp() string {
	return time.Now().Format("2006-01-02T15:04:05Z07:00")
}

func NrMessages(mtu uint16, id string, message []byte) uint16 {
	// Header is
	// uint8 1 byte MessageType
	// uint16 2 bytes MessageNumber
	// uint16 2 bytes NrMessages
	// uint16 2 bytes Length of ID
	// ? bytes id
	// 4 bytes checksum
	// 2 bytes length of payload
	// payload ([] byte)
	var payloadLength = mtu - 13 - uint16(len(id))
	// len in Go returns the number of bytes, not the number of characters or runes.
	var nrMsgs = 1 + int32(len(message))/int32(payloadLength)
	if nrMsgs != int32(uint16(nrMsgs)) {
		// This message will not be delivered.
		Logger.Error("This message will not be delivered. Max 65535 parts are allowed for each message.")
	}
	return (uint16)(nrMsgs)
}

/**
* Make a MD5 hash of the message and return the last 4 characters
 */
func CalculateChecksum(message []byte) string {
	hasher := md5.New()
	hasher.Write(message)
	hash := hex.EncodeToString(hasher.Sum(nil))

	if len(hash) > 4 {
		return hash[len(hash)-4:]
	}
	return hash
}

/**
	// Header is
	// uint8 1 byte MessageType
	// uint16 2 bytes MessageNumber
	// uint16 2 bytes NrMessages
	// uint16 2 bytes Length of ID
	// ? bytes id
	// 4 bytes checksum
	// 2 bytes length of payload
	// payload ([] byte)
*
*/

// Return a message in the format that can be sent over the network
func FormatMessage(messageType uint8, id string, message []byte, mtu uint16) [][]byte {
	var result [][]byte
	// log.Printf("message length: %d", len(message))
	payloadLength := int32(len(message))

	// Special case: empty payload must still produce exactly one packet so that the
	// downstream sequence is contiguous (e.g. for filtered events that must fill gaps).
	if payloadLength == 0 {
		part := []byte{}
		b := make([]byte, 2)
		part = append(part, messageType)
		binary.BigEndian.PutUint16(b, 1)
		part = append(part, b...) // messageNumber = 1
		binary.BigEndian.PutUint16(b, 1)
		part = append(part, b...) // nrMsgs = 1
		idBytes := []byte(id)
		binary.BigEndian.PutUint16(b, uint16(len(idBytes)))
		part = append(part, b...)
		part = append(part, idBytes...)
		checksum := CalculateChecksum([]byte{})
		part = append(part, []byte(checksum)...)
		binary.BigEndian.PutUint16(b, 0)
		part = append(part, b...) // payload length = 0
		// no payload bytes
		result = append(result, part)
		return result
	}

	// This call will calculate the correct number of messages
	nrMsgs := NrMessages(mtu, id, message)
	// log.Printf("Message length: %d, MTU: %d\n", payloadLength, mtu)
	// log.Printf("Message will be sent in %d parts\n", nrMsgs)
	// Now we know the exact number of messages we need to transmit

	// Start with the first message
	var messageNumber uint16 = 1
	var position int32 = 0
	for position < payloadLength {
		part := []byte{}
		// Create a byte slice with enough space to hold a uint8
		b := make([]byte, 1)
		// Write messageType to b
		b[0] = messageType
		part = append(part, b...)

		// Now the messageNumber
		b = make([]byte, 2)
		// Write messageNumber to b
		binary.BigEndian.PutUint16(b, messageNumber)
		// Append b to part
		part = append(part, b...)

		// Now the number of messages
		binary.BigEndian.PutUint16(b, nrMsgs)
		part = append(part, b...)

		// Add the id. We need to convert the id to a byte slice
		idBytes := []byte(id)
		// Add the length of the id
		binary.BigEndian.PutUint16(b, uint16(len(idBytes)))
		part = append(part, b...)
		// Add the id
		part = append(part, idBytes...)

		// Calculate the remaining length for the payload
		// Header is
		// uint8 1 byte MessageType
		// uint16 2 bytes MessageNumber
		// uint16 2 bytes NrMessages
		// uint16 2 bytes Length of ID
		// ? bytes id
		// 4 bytes checksum
		// 2 bytes length of payload
		// payload ([] byte)
		var remainingLength int32 = int32(mtu) - 13 - int32(len(id))

		if remainingLength < 0 {
			Logger.Panicf("Error. The Header is longer than the MTU.")
		}
		var payload []byte = []byte{}
		var checksum string
		if payloadLength-position <= remainingLength {
			// The rest of the message fits in this window
			// Calculate the payload
			payload = message[position:]
			checksum = CalculateChecksum(payload)
			// Convert the checksum to a byte slice
			checksumBytes := []byte(checksum)
			// Add the checksum
			part = append(part, checksumBytes...)
			// Add the length of the payload
			binary.BigEndian.PutUint16(b, uint16(len(payload)))
			part = append(part, b...)
			// Add the payload
			part = append(part, payload...)
			// Add the message to the result
			result = append(result, part)
		} else {
			// Take a slice of the message
			payload = message[position : position+int32(remainingLength)]
			checksum = CalculateChecksum(payload)
			// Convert the checksum to a byte slice
			checksumBytes := []byte(checksum)
			// Add the checksum
			part = append(part, checksumBytes...)
			// Add the length of the payload
			binary.BigEndian.PutUint16(b, uint16(len(payload)))
			part = append(part, b...)
			// Add the payload
			part = append(part, payload...)
			// Add the message to the result
			result = append(result, part)
		}
		position += int32(remainingLength)
		messageNumber++
	}
	// log.Printf("Formatted message into %d parts\n", len(result))
	return result
}
