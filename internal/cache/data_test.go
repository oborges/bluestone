package cache

import (
	"bytes"
	"testing"

	"github.com/oborges/bluestone/internal/config"
)

func TestDataCacheFullReadWithLengthZeroReturnsWholeFile(t *testing.T) {
	cache := newTestDataCache(t)

	payload := []byte("full object payload")
	if err := cache.Write("/object.bin", payload); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := cache.Read("/object.bin", 0, 0)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Read() = %q, want %q", got, payload)
	}
}

func TestDataCacheChunkReadWriteAndObjectInvalidation(t *testing.T) {
	cache := newTestDataCache(t)

	first := []byte("chunk-0")
	second := []byte("chunk-1")
	if err := cache.WriteChunk("/object.bin", 0, 1024, first); err != nil {
		t.Fatalf("WriteChunk(first) error = %v", err)
	}
	if err := cache.WriteChunk("/object.bin", 1024, 1024, second); err != nil {
		t.Fatalf("WriteChunk(second) error = %v", err)
	}

	got, err := cache.ReadChunk("/object.bin", 1024, 1024)
	if err != nil {
		t.Fatalf("ReadChunk() error = %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("ReadChunk() = %q, want %q", got, second)
	}

	if err := cache.DeleteObject("/object.bin"); err != nil {
		t.Fatalf("DeleteObject() error = %v", err)
	}
	if _, err := cache.ReadChunk("/object.bin", 0, 1024); err == nil {
		t.Fatal("ReadChunk() after DeleteObject succeeded, want cache miss")
	}
	if _, err := cache.ReadChunk("/object.bin", 1024, 1024); err == nil {
		t.Fatal("ReadChunk(second) after DeleteObject succeeded, want cache miss")
	}
}

func newTestDataCache(t *testing.T) *DataCache {
	t.Helper()

	cache, err := NewDataCache(&config.DataCacheConfig{
		Enabled:   true,
		SizeGB:    1,
		Path:      t.TempDir(),
		ChunkSize: 1,
	})
	if err != nil {
		t.Fatalf("NewDataCache() error = %v", err)
	}
	return cache
}
