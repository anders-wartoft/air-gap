package test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"

	"sitia.nu/airgap/src/protocol"
)

func TestFragmentBoundaryRoundTrip(t *testing.T) {
	const mtu = 1400
	const id = "stress_0_12"
	const capacity = mtu - 13 - len(id)
	cache := protocol.CreateMessageCache()
	for _, size := range []int{0, capacity - 1, capacity, capacity + 1, 2 * capacity} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), size)
			packets := protocol.FormatMessage(protocol.TYPE_CLEARTEXT, id, payload, mtu)
			expected := 1
			if size > 0 {
				expected = (size + capacity - 1) / capacity
			}
			if count := protocol.NrMessages(mtu, id, payload); int(count) != expected {
				t.Fatalf("advertised %d fragments; expected %d", count, expected)
			}
			for index, packet := range packets {
				if len(packet) > mtu || int(binary.BigEndian.Uint16(packet[3:5])) != len(packets) {
					t.Fatalf("fragment %d: size=%d, advertised=%d, emitted=%d",
						index, len(packet), binary.BigEndian.Uint16(packet[3:5]), len(packets))
				}
				kind, gotID, got, err := protocol.ParseMessage(packet, cache)
				if err != nil {
					t.Fatal(err)
				}
				if index == len(packets)-1 {
					if kind != protocol.TYPE_CLEARTEXT || gotID != id || !bytes.Equal(got, payload) {
						t.Fatal("last emitted fragment did not deliver the complete original record")
					}
				} else if kind != protocol.TYPE_MULTIPART {
					t.Fatal("record completed before its last fragment")
				}
			}
			if cache.GetCacheSize() != 0 {
				t.Fatal("completed record retained in fragment cache")
			}
		})
	}
}

func TestFormatPreservesWireChecksum(t *testing.T) {
	for _, fixture := range []struct{ payload, checksum string }{{"", "427e"}, {"abc", "7f72"}} {
		packet := protocol.FormatMessage(protocol.TYPE_CLEARTEXT, "id", []byte(fixture.payload), 100)[0]
		if got := string(packet[9:13]); got != fixture.checksum {
			t.Fatalf("wire checksum for %q changed: %q", fixture.payload, got)
		}
	}
}

func TestFormatMessage(t *testing.T) {
	tests := []struct {
		name     string
		mtu      uint16
		id       string
		message  string
		expected uint16
	}{
		{
			name:     "Test 1",
			mtu:      100,
			id:       "testID",
			message:  "This is a test message",
			expected: 1,
		},
		{
			name:     "Test 2",
			mtu:      25,
			id:       "testID",
			message:  "This is a longer test message that should be split into multiple parts. abcdefghijklmnopqrstuvwxyzåäö",
			expected: 18,
		},
		// add more test cases as needed
	}
	// func FormatMessage(id string, message string, mtu int) []string {
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := protocol.FormatMessage(protocol.TYPE_MESSAGE, tt.id, []byte(tt.message), tt.mtu)
			if !reflect.DeepEqual(uint16(len(actual)), tt.expected) {
				t.Errorf("FormatMessage(%d, %s, %s) = %v; expected %v", tt.mtu, tt.id, tt.message, actual, tt.expected)
			}
		})
	}
}

func TestNrMessages(t *testing.T) {
	tests := []struct {
		name     string
		mtu      uint16
		id       string
		message  string
		nrMsgs   uint16
		expected uint16
	}{
		{
			name:     "Test 1",
			mtu:      100,
			id:       "testID",
			message:  "This is a test message",
			nrMsgs:   1,
			expected: 1,
		},
		{
			name:     "Test 2",
			mtu:      25,
			id:       "testID",
			message:  "This is a longer test message that should be split into multiple parts. abcdefghijklmnopqrstuvwxyzåäö",
			nrMsgs:   1,
			expected: 18, // adjust this based on your expected result
		},
		// add more test cases as needed
	}

	// ...

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := protocol.NrMessages(tt.mtu, tt.id, []byte(tt.message)) // Use the fully qualified function name
			if actual != tt.expected {
				t.Errorf("nrMessages(%d, %s, %s, %d) = %d; expected %d", tt.mtu, tt.id, tt.message, tt.nrMsgs, actual, tt.expected)
			}
		})
	}
}

func TestParseMessage(t *testing.T) {
	tests := []struct {
		name     string
		mtu      int
		id       string
		message  string
		expected int
	}{
		{
			name:     "Test 1",
			mtu:      100,
			id:       "testID",
			message:  "This is a test message",
			expected: 1,
		},
		{
			name:     "Test 2",
			mtu:      25,
			id:       "testID",
			message:  "This is a longer test message that should be split into multiple parts. abcdefghijklmnopqrstuvwxyzåäö",
			expected: 33,
		},
		// add more test cases as needed
	}
	// func FormatMessage(id string, message string, mtu int) []string {
	cache := protocol.CreateMessageCache()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := protocol.FormatMessage(protocol.TYPE_MESSAGE, tt.id, []byte(tt.message), uint16(tt.mtu))
			var combinedResult string
			for _, str := range actual {
				_, _, result, ok := protocol.ParseMessage([]byte(str), cache) // Pass the dereferenced value of cache
				fmt.Println("Received: " + string(result))
				if ok == nil {
					combinedResult += string(result)
				}
			}
			// check the result
			if !reflect.DeepEqual(combinedResult, tt.message) {
				t.Errorf("FormatMessage(%d, %s, %s) = %v; expected %v", tt.mtu, tt.id, tt.message, actual, tt.expected)
			}
		})
	}
}

func TestParseMessageEncrypted(t *testing.T) {
	tests := []struct {
		name     string
		mtu      uint16
		id       string
		message  string
		expected uint16
	}{
		{
			name:     "Test 1",
			mtu:      100,
			id:       "testID",
			message:  "This is a test message",
			expected: 1,
		},
		{
			name:     "Test 2",
			mtu:      25,
			id:       "testID",
			message:  "This is a longer test message that should be split into multiple parts. abcdefghijklmnopqrstuvwxyzåäö",
			expected: 33,
		},
		// add more test cases as needed
	}
	// func FormatMessage(id string, message string, mtu int) []string {
	cache := protocol.CreateMessageCache()
	key := []byte("your-32-byte-key-here!abcdefghij") // replace with your 32-byte key

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ciphertext, err := protocol.Encrypt([]byte(tt.message), key)
			if err != nil {
				fmt.Println(err)
				t.Fail()
			}
			actual := protocol.FormatMessage(protocol.TYPE_MESSAGE, tt.id, ciphertext, tt.mtu)
			for _, str := range actual {
				msgType, _, encrypted, ok := protocol.ParseMessage(str, cache) // Pass the dereferenced value of cache
				if !protocol.IsMessageType(msgType, protocol.TYPE_MULTIPART) {
					// Complete message
					decrypted, _ := protocol.Decrypt(encrypted, key)
					fmt.Println("Received: " + string(decrypted))
					fmt.Println(ok)
					if ok == nil {
						// check the result
						if !reflect.DeepEqual(string(decrypted), tt.message) {
							t.Errorf("FormatMessage(%d, %s, %s) = %v; expected %v", tt.mtu, tt.id, tt.message, actual, tt.expected)
						}
					}
				} // else the message is empty (part of a larger message)
			}
		})
	}
	// Everything is ok
}
