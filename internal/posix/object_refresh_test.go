package posix

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/pkg/types"
)

func TestObjectRefreshInvalidatesCleanFileCaches(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("clean.txt", []byte("old"), time.Unix(100, 0))

	ops, dataCache := newRefreshTestOps(t, store)
	scanner := NewObjectRefreshScanner(ops, &config.ObjectRefreshConfig{}, nil)
	scanner.RunOnce(ctx)

	if _, err := ops.Stat(ctx, "/clean.txt"); err != nil {
		t.Fatalf("Stat(old) error = %v", err)
	}
	data, err := ops.ReadFile(ctx, "/clean.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile(old) error = %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("ReadFile(old) = %q, want old", data)
	}

	store.put("clean.txt", []byte("new-data"), time.Unix(200, 0))
	scanner.RunOnce(ctx)

	info, err := ops.Stat(ctx, "/clean.txt")
	if err != nil {
		t.Fatalf("Stat(new) error = %v", err)
	}
	if info.Size() != int64(len("new-data")) {
		t.Fatalf("Stat(new).Size = %d, want %d", info.Size(), len("new-data"))
	}

	data, err = ops.ReadFile(ctx, "/clean.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile(new) error = %v", err)
	}
	if string(data) != "new-data" {
		t.Fatalf("ReadFile(new) = %q, want new-data", data)
	}
	if _, err := dataCache.Read("/clean.txt", 0, 0); err != nil {
		t.Fatalf("data cache was not repopulated after refresh invalidation: %v", err)
	}
}

func TestObjectRefreshSkipsDirtyFileCaches(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("dirty.txt", []byte("old"), time.Unix(100, 0))

	ops, _ := newRefreshTestOps(t, store)
	scanner := NewObjectRefreshScanner(ops, &config.ObjectRefreshConfig{}, func(path string) bool {
		return path == "/dirty.txt"
	})
	scanner.RunOnce(ctx)

	if _, err := ops.Stat(ctx, "/dirty.txt"); err != nil {
		t.Fatalf("Stat(old) error = %v", err)
	}
	data, err := ops.ReadFile(ctx, "/dirty.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile(old) error = %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("ReadFile(old) = %q, want old", data)
	}

	store.put("dirty.txt", []byte("external-new"), time.Unix(200, 0))
	scanner.RunOnce(ctx)

	info, err := ops.Stat(ctx, "/dirty.txt")
	if err != nil {
		t.Fatalf("Stat(after dirty skip) error = %v", err)
	}
	if info.Size() != int64(len("old")) {
		t.Fatalf("dirty Stat size = %d, want cached old size %d", info.Size(), len("old"))
	}

	data, err = ops.ReadFile(ctx, "/dirty.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile(after dirty skip) error = %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("dirty ReadFile = %q, want cached old data", data)
	}
}

func TestObjectRefreshDirtyExternalChangeRecordsConflictAndRefreshesCOSDefault(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("dirty.txt", []byte("old"), time.Unix(100, 0))

	ops, _ := newRefreshTestOps(t, store)
	dirty := false
	var conflicts []ObjectConflict
	scanner := NewObjectRefreshScanner(
		ops,
		&config.ObjectRefreshConfig{},
		func(path string) bool {
			return dirty && path == "/dirty.txt"
		},
		func(conflict ObjectConflict) error {
			conflicts = append(conflicts, conflict)
			dirty = false
			return nil
		},
	)
	scanner.RunOnce(ctx)

	if _, err := ops.Stat(ctx, "/dirty.txt"); err != nil {
		t.Fatalf("Stat(old) error = %v", err)
	}
	data, err := ops.ReadFile(ctx, "/dirty.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile(old) error = %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("ReadFile(old) = %q, want old", data)
	}

	dirty = true
	store.put("dirty.txt", []byte("external-new"), time.Unix(200, 0))
	scanner.RunOnce(ctx)

	if len(conflicts) != 1 {
		t.Fatalf("conflicts recorded = %d, want 1", len(conflicts))
	}
	if conflicts[0].Path != "/dirty.txt" || conflicts[0].Key != "dirty.txt" {
		t.Fatalf("conflict target = path %q key %q, want /dirty.txt dirty.txt", conflicts[0].Path, conflicts[0].Key)
	}
	if conflicts[0].Deleted {
		t.Fatal("conflict should describe an updated object, not a deletion")
	}

	info, err := ops.Stat(ctx, "/dirty.txt")
	if err != nil {
		t.Fatalf("Stat(after conflict) error = %v", err)
	}
	if info.Size() != int64(len("external-new")) {
		t.Fatalf("Stat(after conflict).Size = %d, want %d", info.Size(), len("external-new"))
	}

	data, err = ops.ReadFile(ctx, "/dirty.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile(after conflict) error = %v", err)
	}
	if string(data) != "external-new" {
		t.Fatalf("ReadFile(after conflict) = %q, want external-new", data)
	}
}

func TestObjectRefreshInvalidatesDirectoryListing(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("dir/a.txt", []byte("a"), time.Unix(100, 0))

	ops, _ := newRefreshTestOps(t, store)
	scanner := NewObjectRefreshScanner(ops, &config.ObjectRefreshConfig{}, nil)
	scanner.RunOnce(ctx)

	entries, err := ops.ListDirectory(ctx, "/dir")
	if err != nil {
		t.Fatalf("ListDirectory(initial) error = %v", err)
	}
	if names := entryNames(entries); strings.Join(names, ",") != "a.txt" {
		t.Fatalf("initial names = %v, want [a.txt]", names)
	}

	store.put("dir/b.txt", []byte("b"), time.Unix(200, 0))
	scanner.RunOnce(ctx)

	entries, err = ops.ListDirectory(ctx, "/dir")
	if err != nil {
		t.Fatalf("ListDirectory(refreshed) error = %v", err)
	}
	if names := entryNames(entries); strings.Join(names, ",") != "a.txt,b.txt" {
		t.Fatalf("refreshed names = %v, want [a.txt b.txt]", names)
	}
}

func TestObjectRefreshInvalidatesAncestorDirectoryListing(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()

	ops, _ := newRefreshTestOps(t, store)
	scanner := NewObjectRefreshScanner(ops, &config.ObjectRefreshConfig{}, nil)
	scanner.RunOnce(ctx)

	entries, err := ops.ListDirectory(ctx, "/dir")
	if err != nil {
		t.Fatalf("ListDirectory(empty) error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty names = %v, want none", entryNames(entries))
	}

	store.put("dir/sub/file.txt", []byte("nested"), time.Unix(200, 0))
	scanner.RunOnce(ctx)

	entries, err = ops.ListDirectory(ctx, "/dir")
	if err != nil {
		t.Fatalf("ListDirectory(refreshed ancestor) error = %v", err)
	}
	if names := entryNames(entries); strings.Join(names, ",") != "sub" {
		t.Fatalf("ancestor names = %v, want [sub]", names)
	}
}

func newRefreshTestOps(t *testing.T, store *fakeObjectStore) (*OperationsHandler, *cache.DataCache) {
	t.Helper()

	metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{
		Enabled:    true,
		TTLSeconds: 3600,
		MaxEntries: 100,
	})
	dataCache, err := cache.NewDataCache(&config.DataCacheConfig{
		Enabled:   true,
		SizeGB:    1,
		Path:      t.TempDir(),
		ChunkSize: 1024,
	})
	if err != nil {
		t.Fatalf("NewDataCache() error = %v", err)
	}
	perf := &config.PerformanceConfig{
		ReadAheadKB:          config.DefaultReadAheadKB,
		WriteBufferKB:        4096,
		MultipartThresholdMB: 100,
		MultipartChunkMB:     10,
		WorkerPoolSize:       1,
		MaxConcurrentReads:   1,
		MaxConcurrentWrites:  1,
		MaxFullObjectReadMB:  1,
		MaxBufferedWriteMB:   1,
		MaxDirectoryEntries:  100,
	}
	return NewOperationsHandler(store, metadataCache, dataCache, perf), dataCache
}

func entryNames(entries []*FileInfo) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	sort.Strings(names)
	return names
}

type fakeObjectStore struct {
	mu           sync.RWMutex
	objects      map[string]fakeObject
	copyErrors   map[string]error
	deleteErrors map[string]error
	// omitListMetadata drops user metadata from listings, as COS does.
	omitListMetadata bool
	rangeCalls       int
	headCalls        int
	// afterList runs once a listing has been taken, before it is returned,
	// to simulate an object changing while the listing is in flight.
	afterList func()
}

type fakeObject struct {
	data         []byte
	lastModified time.Time
	etag         string
	metadata     map[string]string
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{
		objects:      make(map[string]fakeObject),
		copyErrors:   make(map[string]error),
		deleteErrors: make(map[string]error),
	}
}

func (s *fakeObjectStore) put(key string, data []byte, lastModified time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := append([]byte(nil), data...)
	s.objects[key] = fakeObject{
		data:         copied,
		lastModified: lastModified,
		etag:         fmt.Sprintf("%q", string(copied)),
		metadata:     map[string]string{},
	}
}

func (s *fakeObjectStore) GetObject(_ context.Context, key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	obj, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), obj.data...), nil
}

func (s *fakeObjectStore) rangeCallCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rangeCalls
}

func (s *fakeObjectStore) GetObjectRange(_ context.Context, key string, offset, length int64) ([]byte, error) {
	s.mu.Lock()
	s.rangeCalls++
	s.mu.Unlock()

	s.mu.RLock()
	defer s.mu.RUnlock()

	obj, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if offset >= int64(len(obj.data)) {
		return nil, io.EOF
	}
	end := offset + length
	if end > int64(len(obj.data)) {
		end = int64(len(obj.data))
	}
	return append([]byte(nil), obj.data[offset:end]...), nil
}

func (s *fakeObjectStore) GetObjectStream(_ context.Context, key string) (io.ReadCloser, error) {
	data, err := s.GetObject(context.Background(), key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeObjectStore) PutObject(_ context.Context, key string, data []byte, metadata map[string]string) error {
	s.put(key, data, time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	obj := s.objects[key]
	obj.metadata = copyStringMap(metadata)
	s.objects[key] = obj
	return nil
}

func (s *fakeObjectStore) DeleteObject(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deleteErrors[key]; err != nil {
		return err
	}
	delete(s.objects, key)
	return nil
}

func (s *fakeObjectStore) headCallCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.headCalls
}

func (s *fakeObjectStore) HeadObject(_ context.Context, key string) (*types.ObjectMetadata, error) {
	s.mu.Lock()
	s.headCalls++
	s.mu.Unlock()

	s.mu.RLock()
	defer s.mu.RUnlock()

	obj, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &types.ObjectMetadata{
		Key:          key,
		Size:         int64(len(obj.data)),
		LastModified: obj.lastModified,
		ETag:         obj.etag,
		Metadata:     copyStringMap(obj.metadata),
	}, nil
}

// omitListMetadata makes listings carry no user metadata, the way COS
// ListObjectsV2 behaves. Tests that care about that path set it.
func (s *fakeObjectStore) omitListingMetadata() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.omitListMetadata = true
}

func (s *fakeObjectStore) ListObjects(_ context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, error) {
	out, afterList := s.listObjects(prefix, maxKeys)
	if afterList != nil {
		afterList()
	}
	return out, nil
}

func (s *fakeObjectStore) listObjects(prefix string, maxKeys int) ([]*types.ObjectMetadata, func()) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if maxKeys > 0 && len(keys) > maxKeys {
		keys = keys[:maxKeys]
	}

	out := make([]*types.ObjectMetadata, 0, len(keys))
	for _, key := range keys {
		obj := s.objects[key]
		listed := &types.ObjectMetadata{
			Key:          key,
			Size:         int64(len(obj.data)),
			LastModified: obj.lastModified,
			ETag:         obj.etag,
			Metadata:     copyStringMap(obj.metadata),
		}
		if s.omitListMetadata {
			listed.Metadata = nil
		}
		out = append(out, listed)
	}
	return out, s.afterList
}

func (s *fakeObjectStore) CopyObject(_ context.Context, sourceKey, destKey string) error {
	s.mu.RLock()
	err := s.copyErrors[copyErrorKey(sourceKey, destKey)]
	s.mu.RUnlock()
	if err != nil {
		return err
	}

	data, err := s.GetObject(context.Background(), sourceKey)
	if err != nil {
		return err
	}
	s.put(destKey, data, time.Now())
	return nil
}

func (s *fakeObjectStore) CopyObjectWithMetadata(ctx context.Context, sourceKey, destKey string, metadata map[string]string) error {
	if err := s.CopyObject(ctx, sourceKey, destKey); err != nil {
		return err
	}
	return s.UpdateObjectMetadata(ctx, destKey, metadata)
}

func (s *fakeObjectStore) failCopy(sourceKey, destKey string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.copyErrors[copyErrorKey(sourceKey, destKey)] = err
}

func (s *fakeObjectStore) failDelete(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteErrors[key] = err
}

func copyErrorKey(sourceKey, destKey string) string {
	return sourceKey + "\x00" + destKey
}

func (s *fakeObjectStore) UpdateObjectMetadata(_ context.Context, key string, metadata map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	obj, ok := s.objects[key]
	if !ok {
		return os.ErrNotExist
	}
	obj.metadata = copyStringMap(metadata)
	obj.lastModified = time.Now()
	s.objects[key] = obj
	return nil
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
