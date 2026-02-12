package protocol

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

func CompressGzip(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	_, err := writer.Write(payload)
	if err != nil {
		return nil, err
	}
	err = writer.Close()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decompress payload with a max decompressed size of maxReturnSize bytes
// Put maxReturnSize < 0 to disable the size check
func DecompressGzip(payload []byte, maxReturnSize int) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if maxReturnSize < 0 {
		decompressed, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		return decompressed, nil
	}
	decompressed, err := io.ReadAll(io.LimitReader(reader, int64(maxReturnSize)+1))
	if err != nil {
		return nil, err
	}
	if len(decompressed) > maxReturnSize {
		return nil, fmt.Errorf("Decompressed size is larger than maximum size")
	}
	return decompressed, nil
}
