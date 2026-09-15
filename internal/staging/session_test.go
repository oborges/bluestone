package staging

import (
	"bytes"
	"os"
	"sync"
	"testing"
	"time"
)

func TestWriteSession_New(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	if session.Path != path {
		t.Errorf("Expected path %s, got %s", path, session.Path)
	}

	if session.StagingPath != stagingPath {
		t.Errorf("Expected staging path %s, got %s", stagingPath, session.StagingPath)
	}

	if session.RefCount != 1 {
		t.Errorf("Expected initial RefCount 1, got %d", session.RefCount)
	}

	if session.Size != 0 {
		t.Errorf("Expected initial size 0, got %d", session.Size)
	}
}

func TestWriteSession_Write(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-write.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	// Write data
	data := []byte("Hello, World!")
	n, err := session.Write(data, 0)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if n != len(data) {
		t.Errorf("Expected to write %d bytes, wrote %d", len(data), n)
	}

	if session.Size != int64(len(data)) {
		t.Errorf("Expected size %d, got %d", len(data), session.Size)
	}

	if !session.Dirty {
		t.Error("Session should be marked as dirty after write")
	}
}

func TestWriteSession_WriteAtOffset(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-offset.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	// Write at offset 0
	data1 := []byte("Hello")
	session.Write(data1, 0)

	// Write at offset 5
	data2 := []byte(", World!")
	session.Write(data2, 5)

	// Sync to flush
	if err := session.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Read back and verify
	expected := []byte("Hello, World!")
	result := make([]byte, len(expected))
	n, err := session.Read(result, 0)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if n != len(expected) {
		t.Errorf("Expected to read %d bytes, read %d", len(expected), n)
	}

	if !bytes.Equal(result, expected) {
		t.Errorf("Expected %s, got %s", expected, result)
	}
}

func TestWriteSession_Read(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-read.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	// Write data
	data := []byte("Hello, World!")
	session.Write(data, 0)
	session.Sync()

	// Read data
	buffer := make([]byte, len(data))
	n, err := session.Read(buffer, 0)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if n != len(data) {
		t.Errorf("Expected to read %d bytes, read %d", len(data), n)
	}

	if !bytes.Equal(buffer, data) {
		t.Errorf("Expected %s, got %s", data, buffer)
	}
}

func TestWriteSession_ReadPartial(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-partial.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	// Write data
	data := []byte("Hello, World!")
	session.Write(data, 0)
	session.Sync()

	// Read partial data
	buffer := make([]byte, 5)
	n, err := session.Read(buffer, 0)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if n != 5 {
		t.Errorf("Expected to read 5 bytes, read %d", n)
	}

	expected := []byte("Hello")
	if !bytes.Equal(buffer, expected) {
		t.Errorf("Expected %s, got %s", expected, buffer)
	}

	// Read from offset
	buffer2 := make([]byte, 7)
	n, err = session.Read(buffer2, 7)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	expected2 := []byte("World!")
	if !bytes.Equal(buffer2[:n], expected2) {
		t.Errorf("Expected %s, got %s", expected2, buffer2[:n])
	}
}

func TestWriteSession_Sync(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-sync.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	// Write data
	data := []byte("Hello, World!")
	session.Write(data, 0)

	// Sync
	if err := session.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Verify file exists and has correct size
	stat, err := os.Stat(stagingPath)
	if err != nil {
		t.Fatalf("Failed to stat staging file: %v", err)
	}

	if stat.Size() != int64(len(data)) {
		t.Errorf("Expected file size %d, got %d", len(data), stat.Size())
	}
}

func TestWriteSession_RefCount(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-refcount.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	// Initial ref count should be 1
	if session.GetRefCount() != 1 {
		t.Errorf("Expected initial RefCount 1, got %d", session.GetRefCount())
	}

	// Increment
	session.IncrementRefCount()
	if session.GetRefCount() != 2 {
		t.Errorf("Expected RefCount 2, got %d", session.GetRefCount())
	}

	// Decrement
	session.DecrementRefCount()
	if session.GetRefCount() != 1 {
		t.Errorf("Expected RefCount 1, got %d", session.GetRefCount())
	}

	// Decrement to 0
	session.DecrementRefCount()
	if session.GetRefCount() != 0 {
		t.Errorf("Expected RefCount 0, got %d", session.GetRefCount())
	}

	// Should not go negative
	session.DecrementRefCount()
	if session.GetRefCount() != 0 {
		t.Errorf("RefCount should not go negative, got %d", session.GetRefCount())
	}
}

func TestWriteSession_ThreadSafety(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-threadsafe.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	var wg sync.WaitGroup
	numGoroutines := 100

	// Concurrent writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			data := []byte{byte(id)}
			session.Write(data, int64(id))
		}(i)
	}

	// Concurrent ref count operations
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session.IncrementRefCount()
			time.Sleep(time.Microsecond)
			session.DecrementRefCount()
		}()
	}

	// Concurrent reads
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buffer := make([]byte, 1)
			session.Read(buffer, 0)
		}()
	}

	wg.Wait()

	// Test should not panic (race detector will catch issues)
}

func TestWriteSession_LastWrite(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-lastwrite.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	before := time.Now()
	time.Sleep(10 * time.Millisecond)

	// Write data
	data := []byte("Hello")
	session.Write(data, 0)

	time.Sleep(10 * time.Millisecond)
	after := time.Now()

	if session.LastWrite.Before(before) || session.LastWrite.After(after) {
		t.Errorf("LastWrite timestamp %v not in expected range [%v, %v]",
			session.LastWrite, before, after)
	}
}

func TestWriteSession_Close(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-close.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Write some data
	data := []byte("Hello, World!")
	session.Write(data, 0)

	// Close
	if err := session.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// File should still exist (not deleted by Close)
	if _, err := os.Stat(stagingPath); os.IsNotExist(err) {
		t.Error("Staging file should exist after Close")
	}
}

func TestWriteSession_MultipleWrites(t *testing.T) {
	path := "/test/file.txt"
	stagingPath := "/tmp/staging-test-multiple.dat"
	defer os.Remove(stagingPath)

	session, err := NewWriteSession(nil, path, stagingPath)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	// Multiple writes at different offsets
	writes := []struct {
		data   []byte
		offset int64
	}{
		{[]byte("Hello"), 0},
		{[]byte("World"), 10},
		{[]byte(", "), 5},
		{[]byte("!"), 15},
	}

	for _, w := range writes {
		if _, err := session.Write(w.data, w.offset); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	session.Sync()

	// Expected result: "Hello, World!"
	// Positions: 0-4: "Hello", 5-6: ", ", 10-14: "World", 15: "!"
	expected := make([]byte, 16)
	copy(expected[0:], "Hello")
	copy(expected[5:], ", ")
	copy(expected[10:], "World")
	copy(expected[15:], "!")

	result := make([]byte, 16)
	n, err := session.Read(result, 0)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if n != 16 {
		t.Errorf("Expected to read 16 bytes, read %d", n)
	}

	if !bytes.Equal(result, expected) {
		t.Errorf("Expected %v, got %v", expected, result)
	}
}

// Made with Bob

func TestWriteSession_TruncateIntoUploadedPartIsNotSequential(t *testing.T) {
	ws, err := NewWriteSession(nil, "/big.bin", t.TempDir()+"/big.data")
	if err != nil {
		t.Fatalf("NewWriteSession() error = %v", err)
	}
	defer ws.Close()

	if _, err := ws.Write(make([]byte, 64), 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// Simulate a progressive multipart upload that already sent bytes 0-31.
	ws.Multipart.PartSize = 32
	ws.Multipart.AddCompletedPart(1, "etag-1")
	if !ws.Multipart.IsSequential() {
		t.Fatal("precondition: state should be sequential after a part upload")
	}

	if err := ws.Truncate(16); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}
	if ws.Multipart.IsSequential() {
		t.Fatal("truncating into an uploaded part must invalidate the progressive upload")
	}
	if got := ws.GetSize(); got != 16 {
		t.Fatalf("GetSize() = %d, want 16", got)
	}
}
