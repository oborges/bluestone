package posix

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/pkg/types"
)

// downObjectStore simulates an unreachable object store: every call fails
// with a non-not-found error.
type downObjectStore struct{}

var errBackendDown = errors.New("dial tcp: connection refused")

func (downObjectStore) GetObject(context.Context, string) ([]byte, error) {
	return nil, errBackendDown
}
func (downObjectStore) GetObjectRange(context.Context, string, int64, int64) ([]byte, error) {
	return nil, errBackendDown
}
func (downObjectStore) GetObjectStream(context.Context, string) (io.ReadCloser, error) {
	return nil, errBackendDown
}
func (downObjectStore) PutObject(context.Context, string, []byte, map[string]string) error {
	return errBackendDown
}
func (downObjectStore) DeleteObject(context.Context, string) error { return errBackendDown }
func (downObjectStore) HeadObject(context.Context, string) (*types.ObjectMetadata, error) {
	return nil, errBackendDown
}
func (downObjectStore) ListObjects(context.Context, string, int) ([]*types.ObjectMetadata, error) {
	return nil, errBackendDown
}
func (downObjectStore) CopyObject(context.Context, string, string) error { return errBackendDown }
func (downObjectStore) UpdateObjectMetadata(context.Context, string, map[string]string) error {
	return errBackendDown
}

// switchableObjectStore forwards to a healthy store until taken down, then
// fails every call like downObjectStore. The switch goes through a mutex
// because the handler's own goroutines may still be calling the store:
// Stat returns as soon as the file probe answers, leaving its directory
// probe running. Replacing the handler's store field instead races with it.
type switchableObjectStore struct {
	mu      sync.RWMutex
	healthy ObjectStore
	down    bool
}

func (s *switchableObjectStore) takeDown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = true
}

func (s *switchableObjectStore) current() ObjectStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.down {
		return downObjectStore{}
	}
	return s.healthy
}

func (s *switchableObjectStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	return s.current().GetObject(ctx, key)
}
func (s *switchableObjectStore) GetObjectRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	return s.current().GetObjectRange(ctx, key, offset, length)
}
func (s *switchableObjectStore) GetObjectStream(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.current().GetObjectStream(ctx, key)
}
func (s *switchableObjectStore) PutObject(ctx context.Context, key string, data []byte, metadata map[string]string) error {
	return s.current().PutObject(ctx, key, data, metadata)
}
func (s *switchableObjectStore) DeleteObject(ctx context.Context, key string) error {
	return s.current().DeleteObject(ctx, key)
}
func (s *switchableObjectStore) HeadObject(ctx context.Context, key string) (*types.ObjectMetadata, error) {
	return s.current().HeadObject(ctx, key)
}
func (s *switchableObjectStore) ListObjects(ctx context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, error) {
	return s.current().ListObjects(ctx, prefix, maxKeys)
}
func (s *switchableObjectStore) CopyObject(ctx context.Context, sourceKey, destKey string) error {
	return s.current().CopyObject(ctx, sourceKey, destKey)
}
func (s *switchableObjectStore) UpdateObjectMetadata(ctx context.Context, key string, metadata map[string]string) error {
	return s.current().UpdateObjectMetadata(ctx, key, metadata)
}

func TestStatDuringBackendOutage(t *testing.T) {
	ctx := context.Background()
	ops, _ := newRefreshTestOps(t, nil)
	// Swap in the failing store after construction.
	ops.cosClient = downObjectStore{}

	// A backend outage must surface as an error, never as false ENOENT.
	_, err := ops.Stat(ctx, "/some-file.txt")
	if err == nil {
		t.Fatal("Stat() during outage returned success")
	}
	if os.IsNotExist(err) {
		t.Fatal("Stat() during outage reported ENOENT; must report an I/O error instead")
	}

	// The export root exists by definition, even with the backend down.
	info, err := ops.Stat(ctx, "/")
	if err != nil {
		t.Fatalf("Stat(/) during outage error = %v, want synthetic root", err)
	}
	if !info.IsDir() {
		t.Fatal("Stat(/) must report a directory")
	}
}

func TestStatServesStaleMetadataDuringOutage(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("stale.txt", []byte("payload-12"), time.Unix(100, 0))
	store.put("dir/child.txt", []byte("c"), time.Unix(100, 0))
	backend := &switchableObjectStore{healthy: store}

	metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{
		Enabled:    true,
		TTLSeconds: 1, // expire quickly so the outage hits stale entries
		MaxEntries: 100,
	})
	ops := NewOperationsHandler(backend, metadataCache, nil, &config.PerformanceConfig{
		MaxDirectoryEntries: 100,
		MaxFullObjectReadMB: 1,
	})

	// Warm the caches while the backend is healthy.
	info, err := ops.Stat(ctx, "/stale.txt")
	if err != nil {
		t.Fatalf("Stat(warm) error = %v", err)
	}
	wantSize := info.Size()
	entries, err := ops.ListDirectory(ctx, "/dir")
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListDirectory(warm) = %v entries, err %v", len(entries), err)
	}

	// Let the TTL lapse, then take the backend down.
	time.Sleep(1100 * time.Millisecond)
	backend.takeDown()

	info, err = ops.Stat(ctx, "/stale.txt")
	if err != nil {
		t.Fatalf("Stat(stale during outage) error = %v, want stale answer", err)
	}
	if info.Size() != wantSize {
		t.Fatalf("stale Stat size = %d, want %d", info.Size(), wantSize)
	}

	entries, err = ops.ListDirectory(ctx, "/dir")
	if err != nil {
		t.Fatalf("ListDirectory(stale during outage) error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "child.txt" {
		t.Fatalf("stale listing = %v, want [child.txt]", entryNames(entries))
	}

	// A name absent from the parent's stale listing answers as a stale
	// negative (ENOENT), which is what keeps creates working in an outage.
	if _, err := ops.Stat(ctx, "/dir/brand-new-name.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat(new name under stale-listed dir) error = %v, want ENOENT", err)
	}
	// A name present in the parent's stale listing answers positively.
	info, err = ops.Stat(ctx, "/dir/child.txt")
	if err != nil {
		t.Fatalf("Stat(child via stale parent listing) error = %v", err)
	}
	if info.Name() != "child.txt" {
		t.Fatalf("stale child name = %q", info.Name())
	}

	// Paths with no stale entry AND no stale parent listing still fail with
	// an I/O error, not ENOENT.
	if _, err := ops.Stat(ctx, "/unlisted-dir/never-seen.txt"); err == nil || os.IsNotExist(err) {
		t.Fatalf("Stat(uncached during outage) error = %v, want I/O error", err)
	}
}

func TestStatMissingFileStillENOENT(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	ops, _ := newRefreshTestOps(t, store)

	// With a healthy backend, a genuinely missing path stays ENOENT.
	if _, err := ops.Stat(ctx, "/definitely-missing.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat(missing) error = %v, want ENOENT", err)
	}
}

func TestStatNegativeCachingStopsProbeStorms(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	ops, _ := newRefreshTestOps(t, store)

	if _, err := ops.Stat(ctx, "/missing.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat(first miss) error = %v, want ENOENT", err)
	}
	probesAfterFirst := store.headCallCount()
	if probesAfterFirst == 0 {
		t.Fatal("first miss should probe the object store")
	}

	// Repeated stats within the negative TTL must answer from cache.
	for i := 0; i < 5; i++ {
		if _, err := ops.Stat(ctx, "/missing.txt"); !os.IsNotExist(err) {
			t.Fatalf("Stat(repeat miss) error = %v, want ENOENT", err)
		}
	}
	if got := store.headCallCount(); got != probesAfterFirst {
		t.Fatalf("repeat misses probed the store %d extra times, want 0", got-probesAfterFirst)
	}

	// Once the object appears and the negative entry is invalidated (as all
	// mutation paths do), Stat sees it.
	store.put("missing.txt", []byte("now exists"), time.Unix(200, 0))
	ops.InvalidateFileMutation("/missing.txt")
	info, err := ops.Stat(ctx, "/missing.txt")
	if err != nil {
		t.Fatalf("Stat(after create) error = %v", err)
	}
	if info.Size() != int64(len("now exists")) {
		t.Fatalf("Stat(after create).Size = %d", info.Size())
	}
}

// Made with Bob
