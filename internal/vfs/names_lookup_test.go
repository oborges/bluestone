package vfs

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/IBM/ibm-cos-sdk-go/service/s3"
	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/feature"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/staging"
	"github.com/oborges/bluestone/pkg/types"
	"go.uber.org/zap"
)

// countingStore counts the keys listings return, which is what a directory
// listing costs against COS.
type countingStore struct {
	*fakeObjectStore
	mu     sync.Mutex
	listed int
}

func (s *countingStore) ListObjects(ctx context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, error) {
	objects, err := s.fakeObjectStore.ListObjects(ctx, prefix, maxKeys)
	s.mu.Lock()
	s.listed += len(objects)
	s.mu.Unlock()
	return objects, err
}

// ListChildren lists as COS does with a "/" delimiter: objects directly
// under prefix, and one prefix per subdirectory.
func (s *countingStore) ListChildren(ctx context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, []string, error) {
	all, err := s.fakeObjectStore.ListObjects(ctx, prefix, 0)
	if err != nil {
		return nil, nil, err
	}
	var objects []*types.ObjectMetadata
	var prefixes []string
	seen := make(map[string]bool)
	for _, obj := range all {
		rest := strings.TrimPrefix(obj.Key, prefix)
		if slash := strings.Index(rest, "/"); slash >= 0 {
			if common := prefix + rest[:slash+1]; !seen[common] {
				seen[common] = true
				prefixes = append(prefixes, common)
			}
			continue
		}
		objects = append(objects, obj)
	}
	s.mu.Lock()
	s.listed += len(objects) + len(prefixes)
	s.mu.Unlock()
	return objects, prefixes, nil
}

func (s *countingStore) keysListed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listed
}

func (s *countingStore) PutObjectStream(ctx context.Context, key string, body io.ReadSeeker, metadata map[string]string) error {
	return (&plainUploadStore{fakeObjectStore: s.fakeObjectStore}).PutObjectStream(ctx, key, body, metadata)
}

func (s *countingStore) CreateMultipartUpload(ctx context.Context, key string, metadata map[string]string) (string, error) {
	return "", errNoMultipart
}

func (s *countingStore) UploadPart(ctx context.Context, key, uploadID string, partNumber int64, body io.ReadSeeker) (string, error) {
	return "", errNoMultipart
}

func (s *countingStore) CompleteMultipartUpload(ctx context.Context, key, uploadID string, completedParts []*s3.CompletedPart) error {
	return errNoMultipart
}

func (s *countingStore) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	return errNoMultipart
}

// newCountingWindowsView builds an SMB-style (Windows naming) view over a
// counting store, with staging and a sync worker as the gateway runs.
func newCountingWindowsView(t *testing.T, store *countingStore) (*Filesystem, *staging.StagingManager, *staging.SyncWorker) {
	t.Helper()
	cfg := testStagingConfig(t)
	cfg.SyncInterval = "20ms"
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Shutdown() })

	perfConfig := &config.PerformanceConfig{
		WriteBufferKB:       4096,
		MaxBufferedWriteMB:  config.DefaultMaxBufferedWriteMB,
		MaxDirectoryEntries: config.DefaultMaxDirectoryEntries,
	}
	metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{Enabled: true, MaxEntries: 10000, TTLSeconds: 60})
	ops := posix.NewOperationsHandler(store, metadataCache, nil, perfConfig)
	ops.SetPendingDeleteCheck(manager.HasPendingDelete)
	fs := NewFilesystem(ops, logging.NewKVLogger(zap.NewNop()), "/", perfConfig, manager, nil,
		&feature.FeatureFlags{UseStagingPath: true})

	worker := staging.NewSyncWorker(manager, store, cfg)
	worker.SetObjectMutatedCallback(ops.InvalidateFileMutation)
	worker.SetObjectSyncedCallback(ops.InvalidateObjectAfterSync)
	worker.Start()
	t.Cleanup(worker.Stop)
	return fs.WithWindowsNames(), manager, worker
}

// Resolving a name over SMB must not cost a listing of every directory on
// its path. The soak's 50,000-file directory never finished over SMB: every
// open and delete listed the parent (and, through ancestor invalidation, the
// root, which lists the whole bucket), so N operations in a directory of N
// files listed on the order of N^2 keys. NFS deleted the same directory in
// under an hour.
func TestWindowsNamesDoNotListWholeDirectories(t *testing.T) {
	const files = 3000
	const ops = 200
	store := &countingStore{fakeObjectStore: newFakeObjectStore()}
	store.put("big/", nil)
	for i := 0; i < files; i++ {
		store.put(fmt.Sprintf("big/f%05d", i), []byte("x"))
	}
	win, manager, worker := newCountingWindowsView(t, store)

	t.Run("opening and deleting existing files", func(t *testing.T) {
		before := store.keysListed()
		for i := 0; i < ops; i++ {
			name := fmt.Sprintf("big/f%05d", i)
			if _, err := win.Stat(name); err != nil {
				t.Fatalf("Stat(%s): %v", name, err)
			}
			if err := win.Remove(name); err != nil {
				t.Fatalf("Remove(%s): %v", name, err)
			}
		}
		per := (store.keysListed() - before) / ops
		t.Logf("keys listed per stat and delete: %d", per)
		if per > 50 {
			t.Fatalf("each stat and delete of an existing file listed %d keys on average in a directory of %d files; want a handful", per, files)
		}
	})

	// A copy into a directory writes one file after another, and each lands
	// in COS before long; the upload invalidates the parent's listing, so the
	// next file's name lookup finds no cached listing to scan.
	t.Run("creating new files", func(t *testing.T) {
		const creates = 100
		before := store.keysListed()
		for i := 0; i < creates; i++ {
			name := fmt.Sprintf("big/new-%05d", i)
			f, err := win.Create(name)
			if err != nil {
				t.Fatalf("Create(%s): %v", name, err)
			}
			if _, err := f.Write([]byte("y")); err != nil {
				t.Fatalf("Write(%s): %v", name, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("Close(%s): %v", name, err)
			}
			// Upload it now rather than after the worker's idle delay.
			key := "/" + name
			if !manager.TryLockSync(key) {
				t.Fatalf("could not claim %s for sync", key)
			}
			err = worker.TriggerSync(key)
			manager.UnlockSync(key)
			if err != nil || store.get(name) == nil {
				t.Fatalf("%s did not reach the object store: %v", name, err)
			}
		}
		per := (store.keysListed() - before) / creates
		t.Logf("keys listed per create: %d", per)
		if per > 50 {
			t.Fatalf("each create of a new file listed %d keys on average in a directory of %d files; want a handful", per, files)
		}
	})
}

// A name that exists only in another spelling is still found in a large
// directory whose listing is not cached, through the case variants of its
// first characters rather than a listing of every entry.
func TestWindowsNamesMatchCaseInsensitivelyInLargeDirectories(t *testing.T) {
	store := &countingStore{fakeObjectStore: newFakeObjectStore()}
	store.put("big/", nil)
	for i := 0; i < 3000; i++ {
		store.put(fmt.Sprintf("big/f%05d", i), []byte("x"))
	}
	store.put("big/Report.TXT", []byte("report"))
	store.put("big/File", []byte("upper"))
	store.put("big/file", []byte("lower"))
	win, manager, _ := newCountingWindowsView(t, store)

	// Staged only: not in the object store yet.
	session, err := manager.GetOrCreateSession("/big/Staged.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Write([]byte("staged"), 0); err != nil {
		t.Fatal(err)
	}
	manager.MarkDirty("/big/Staged.txt", session.Size)
	manager.ReleaseSession("/big/Staged.txt")

	for _, tc := range []struct{ name, want, content string }{
		{"big/report.txt", "/big/Report.TXT", "report"},
		{"BIG/REPORT.TXT", "/big/Report.TXT", "report"},
		// Two spellings exist: the exact one wins, else the first in byte
		// order.
		{"big/file", "/big/file", "lower"},
		{"big/FILE", "/big/File", "upper"},
		{"big/staged.TXT", "/big/Staged.txt", "staged"},
		{"big/missing.txt", "/big/missing.txt", ""},
	} {
		before := store.keysListed()
		if got := win.KeyPath(tc.name); got != tc.want {
			t.Errorf("KeyPath(%q) = %q, want %q", tc.name, got, tc.want)
			continue
		}
		if listed := store.keysListed() - before; listed > 50 {
			t.Errorf("resolving %q listed %d keys", tc.name, listed)
		}
		if tc.content == "" {
			continue
		}
		f, err := win.Open(tc.name)
		if err != nil {
			t.Errorf("Open(%q): %v", tc.name, err)
			continue
		}
		got, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil || string(got) != tc.content {
			t.Errorf("Open(%q) read %q, %v; want %q", tc.name, got, err, tc.content)
		}
	}
}

// Every spelling that strings.EqualFold accepts as the same name begins
// with one of the prefixes foldPrefixes returns, whatever the letters.
func TestFoldPrefixesCoverEveryEqualFoldSpelling(t *testing.T) {
	for _, key := range []string{
		"f012345", "IMG_20260101_120000.jpg", "file-00001.bin", "Straße", "kelvin",
		"ΣΊΣΥΦΟΣ", "123", "a", "ǅungla",
	} {
		prefixes := foldPrefixes(key, maxFoldPrefixes)
		if len(prefixes) == 0 || len(prefixes) > maxFoldPrefixes {
			t.Errorf("foldPrefixes(%q) returned %d prefixes", key, len(prefixes))
			continue
		}
		// Every spelling: each character replaced by every member of its
		// fold orbit, up to a bound on the combinations.
		spellings := []string{""}
		for _, r := range key {
			var next []string
			for _, s := range spellings {
				for _, v := range foldOrbit(r) {
					if len(next) < 4096 {
						next = append(next, s+string(v))
					}
				}
			}
			spellings = next
		}
		for _, spelling := range spellings {
			if !strings.EqualFold(spelling, key) {
				t.Fatalf("test bug: %q is not EqualFold to %q", spelling, key)
			}
			covered := false
			for _, p := range prefixes {
				if strings.HasPrefix(spelling, p) {
					covered = true
					break
				}
			}
			if !covered {
				t.Errorf("%q matches %q case-insensitively but begins with none of %q", spelling, key, prefixes)
			}
		}
	}
	if got := foldPrefixes("f012345", maxFoldPrefixes); len(got) != 2 || got[0] != "f012345" || got[1] != "F012345" {
		t.Errorf("foldPrefixes(f012345) = %q, want the whole name in both cases", got)
	}
}
