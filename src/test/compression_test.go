package test

import (
	"os"
	"testing"

	"sitia.nu/airgap/src/protocol"
)

func compressionTest(data []byte, t *testing.T) {
	// Compress using protocol.CompressGzip
	compressed, err := protocol.CompressGzip(data)
	if err != nil {
		t.Fatalf("Compression failed: %v", err)
	}

	// Decompress and check round-trip
	decompressed, err := protocol.DecompressGzip(compressed, len(data))
	if err != nil {
		t.Fatalf("Decompression failed: %v", err)
	}
	if string(decompressed) != string(data) {
		t.Errorf("Decompressed data does not match original")
	}

	// Measure compression rate
	origLen := len(data)
	compLen := len(compressed)
	if origLen == 0 {
		t.Fatalf("Original file is empty")
	}
	compressionRate := 1.0 - float64(compLen)/float64(origLen)
	t.Logf("Original size: %d bytes, Compressed size: %d bytes, Compression rate: %.2f%%", origLen, compLen, compressionRate*100)
}

func TestGzipCompressionAndRate(t *testing.T) {
	// Read the test file
	filePath := "../../config/testcases/messages"
	original, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	compressionTest(original, t)

	// Test with small and medium data
	// read a few lines, compress each line separately
	lines := []string{
		"Short line 1\n",
		"Another short line 2\n",
		"Yet another line 3\n",
		"100 Lorem ipsum dolor sit amet, consectetur adipiscing elit. Mauris quis vestibulum felis. Fusce libero.\n",
		"200 Lorem ipsum dolor sit amet, consectetur adipiscing elit. Aliquam a libero dictum, pharetra est et, hendrerit dolor. Vestibulum tempor eleifend euismod. Etiam et vehicula neque, vel gravida nisi augue.\n",
		"400 Lorem ipsum dolor sit amet, consectetur adipiscing elit. Suspendisse ultricies vitae tortor nec laoreet. Proin eget neque varius magna aliquet condimentum eu quis metus. Etiam sodales mauris urna, et mattis dolor feugiat suscipit. Aliquam sit amet lorem id elit facilisis fermentum. Suspendisse nec purus orci. Morbi laoreet blandit lacus sed tempor. Vestibulum porttitor ligula ac nunc feugiat odio.\n",
		"800 Lorem ipsum dolor sit amet, consectetur adipiscing elit. Aliquam sem dolor, consectetur id dolor ac, viverra suscipit orci. Curabitur eget leo consequat, semper neque vitae, maximus diam. Pellentesque habitant morbi tristique senectus et netus et malesuada fames ac turpis egestas. Vestibulum ante ipsum primis in faucibus orci luctus et ultrices posuere cubilia curae; Cras porta neque non nunc ultricies, bibendum venenatis tellus volutpat. Donec tincidunt, lacus a malesuada tristique, elit elit laoreet ex, et euismod leo magna non risus. Aliquam est ligula, tempus ac arcu eu, varius pharetra neque. Nam tortor ante, accumsan ac sapien in, vulputate auctor quam. Nam quis maximus nibh. Morbi hendrerit eleifend turpis, elementum ultrices dui dictum sed. Morbi porta pharetra dui nec sodales sed.\n",
		"1200 Lorem ipsum dolor sit amet, consectetur adipiscing elit. Nullam bibendum ante id purus iaculis auctor. Sed luctus sollicitudin tortor id tincidunt. Sed dictum arcu est, non dapibus mi rhoncus id. Vestibulum hendrerit urna sed est tincidunt congue. Vestibulum nec vulputate justo. Proin neque ex, rhoncus at condimentum eget, interdum non urna. Integer nec lectus enim. Ut quis suscipit diam. Aenean malesuada, nulla in rhoncus aliquam, nunc enim dapibus urna, non ultricies mi risus eget sapien. Suspendisse vel scelerisque elit. Sed ultricies eget quam eu vulputate. Suspendisse et massa vel ante auctor aliquet in quis leo. Curabitur vehicula mauris diam, a tincidunt diam dignissim ut. Cras at rhoncus tellus. Phasellus ut imperdiet purus. Nam sagittis sed purus nec consectetur. Nullam in faucibus augue. Phasellus lacinia id orci eget ultrices. Integer imperdiet vel magna at eleifend. Mauris non lorem ante. Mauris est urna, placerat quis mollis semper, viverra quis diam. Vivamus ac odio sit amet dolor sodales efficitur sed scelerisque neque. Etiam sit amet nunc est. Vivamus mollis lacus quis lorem pharetra, eu interdum tellus gravida. Vestibulum quam erat, eleifend ut ex ac, fringilla quis.\n",
	}
	for _, line := range lines {
		compressionTest([]byte(line), t)
	}
}
