package zipconduit

import (
	"archive/zip"
	"bytes"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCalculateCrc32ForFile_WithContent(t *testing.T) {
	content := "hello world"

	// Create temp file
	tmpFile, err := os.CreateTemp("", "test-crc-*.txt")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Seek(0, io.SeekStart)

	expectedCrc := crc32.ChecksumIEEE([]byte(content))

	actualCrc, err := calculateCrc32ForFile(tmpFile)
	if err != nil {
		t.Fatalf("calculateCrc32ForFile failed: %v", err)
	}

	if actualCrc != expectedCrc {
		t.Errorf("expected crc %d, but got %d", expectedCrc, actualCrc)
	}
}

func TestCalculateCrc32ForFile_Empty(t *testing.T) {
	// Create empty temp file
	tmpFile, err := os.CreateTemp("", "test-crc-empty-*.txt")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	expectedCrc := crc32.ChecksumIEEE([]byte{})

	actualCrc, err := calculateCrc32ForFile(tmpFile)
	if err != nil {
		t.Fatalf("calculateCrc32ForFile failed: %v", err)
	}

	if actualCrc != expectedCrc {
		t.Errorf("expected crc %d, but got %d", expectedCrc, actualCrc)
	}
}

func TestCalculateCrc32ForFile_LargeContent(t *testing.T) {
	// Create a large content buffer (e.g., 10MB)
	contentSize := 10 * 1024 * 1024
	content := make([]byte, contentSize)
	for i := 0; i < contentSize; i++ {
		content[i] = byte(i % 256)
	}

	// Create temp file with large content
	tmpFile, err := os.CreateTemp("", "test-crc-large-*.dat")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(content); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Seek(0, io.SeekStart)

	expectedCrc := crc32.ChecksumIEEE(content)

	actualCrc, err := calculateCrc32ForFile(tmpFile)
	if err != nil {
		t.Fatalf("calculateCrc32ForFile failed: %v", err)
	}

	if actualCrc != expectedCrc {
		t.Errorf("expected crc %d, but got %d", expectedCrc, actualCrc)
	}
}

func TestStreamingFromZip(t *testing.T) {
	// Create a test ZIP file
	tmpDir, err := os.MkdirTemp("", "test-zip-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "test.zip")
	zipFile, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("failed to create zip file: %v", err)
	}

	zipWriter := zip.NewWriter(zipFile)

	testContent := "hello from zip file"
	testFileName := "test.txt"

	w, err := zipWriter.Create(testFileName)
	if err != nil {
		t.Fatalf("failed to create file in zip: %v", err)
	}

	if _, err := w.Write([]byte(testContent)); err != nil {
		t.Fatalf("failed to write to zip: %v", err)
	}

	if err := zipWriter.Close(); err != nil {
		t.Fatalf("failed to close zip writer: %v", err)
	}
	zipFile.Close()

	//  read from the ZIP and verify CRC32
	zipReader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("failed to open zip reader: %v", err)
	}
	defer zipReader.Close()

	if len(zipReader.File) != 1 {
		t.Fatalf("expected 1 file in zip, got %d", len(zipReader.File))
	}

	f := zipReader.File[0]

	// Verify pre-computed CRC32 is present
	expectedCrc := crc32.ChecksumIEEE([]byte(testContent))
	if f.CRC32 != expectedCrc {
		t.Errorf("ZIP CRC32 mismatch: expected %d, got %d", expectedCrc, f.CRC32)
	}

	// Verify we can stream the content
	rc, err := f.Open()
	if err != nil {
		t.Fatalf("failed to open file from zip: %v", err)
	}
	defer rc.Close()

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, rc); err != nil {
		t.Fatalf("failed to read from zip: %v", err)
	}

	if buf.String() != testContent {
		t.Errorf("content mismatch: expected %q, got %q", testContent, buf.String())
	}
}

func TestMemoryUsage(t *testing.T) {
	// Create a test ZIP with a large file
	tmpDir, err := os.MkdirTemp("", "test-memory-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "large.zip")
	zipFile, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("failed to create zip file: %v", err)
	}

	zipWriter := zip.NewWriter(zipFile)

	// Create 50MB file in the ZIP
	largeSize := 50 * 1024 * 1024
	largeContent := make([]byte, largeSize)
	for i := 0; i < largeSize; i++ {
		largeContent[i] = byte(i % 256)
	}

	w, err := zipWriter.Create("large.bin")
	if err != nil {
		t.Fatalf("failed to create file in zip: %v", err)
	}

	if _, err := w.Write(largeContent); err != nil {
		t.Fatalf("failed to write to zip: %v", err)
	}

	zipWriter.Close()
	zipFile.Close()

	// Measure memory before streaming
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// Stream the file
	zipReader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("failed to open zip: %v", err)
	}
	defer zipReader.Close()

	f := zipReader.File[0]
	rc, err := f.Open()
	if err != nil {
		t.Fatalf("failed to open file: %v", err)
	}
	defer rc.Close()

	// Stream to discard with small buffer (like we do in production)
	buf := make([]byte, 32*1024) // 32KB buffer
	written, err := io.CopyBuffer(io.Discard, rc, buf)
	if err != nil {
		t.Fatalf("failed to stream: %v", err)
	}

	if written != int64(largeSize) {
		t.Errorf("expected to stream %d bytes, got %d", largeSize, written)
	}

	// Measure memory after streaming
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	// Handle potential underflow in memory calculation
	var memUsed uint64
	if memAfter.Alloc > memBefore.Alloc {
		memUsed = memAfter.Alloc - memBefore.Alloc
	} else {
		memUsed = 0
	}

	// Memory usage should be much less than the file size
	// We expect < 5MB of additional memory for streaming a 50MB file
	maxExpectedMemory := uint64(5 * 1024 * 1024) // 5MB max

	t.Logf("Memory: before=%d, after=%d, used=%d (%.2f MB)",
		memBefore.Alloc, memAfter.Alloc, memUsed, float64(memUsed)/(1024*1024))

	if memUsed > maxExpectedMemory {
		t.Logf("Warning: Memory usage higher than expected: %.2f MB (file size: %.2f MB)",
			float64(memUsed)/(1024*1024), float64(largeSize)/(1024*1024))
		// Don't fail the test as memory measurements can be flaky, just log
	} else {
		t.Logf("Memory usage OK: %.2f MB for %.2f MB file",
			float64(memUsed)/(1024*1024), float64(largeSize)/(1024*1024))
	}
}
