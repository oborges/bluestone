package posix

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/pkg/types"
)

func TestEncodeOmitsZeroTimestamps(t *testing.T) {
	got := EncodePOSIXAttributes(&types.POSIXAttributes{Mode: 0640, UID: 5, GID: 6})
	want := map[string]string{MetaKeyMode: "640", MetaKeyUID: "5", MetaKeyGID: "6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EncodePOSIXAttributes() = %v, want %v", got, want)
	}

	mtime := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	got = EncodePOSIXAttributes(&types.POSIXAttributes{Mode: 0640, Mtime: mtime})
	if got[MetaKeyMtime] != mtime.Format(time.RFC3339) || got[MetaKeyAtime] != "" {
		t.Fatalf("EncodePOSIXAttributes() with mtime = %v", got)
	}
}

func TestDecodeReadsCurrentCanonicalAndLegacyKeys(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
	}{
		{"current keys", map[string]string{"mode": "750", "uid": "42", "gid": "7"}},
		{"canonicalized keys", map[string]string{"Mode": "750", "Uid": "42", "Gid": "7"}},
		{"legacy keys as lowercased by the client", map[string]string{"x-amz-meta-mode": "750", "x-amz-meta-uid": "42", "x-amz-meta-gid": "7"}},
		{"legacy keys as canonicalized by the SDK", map[string]string{"X-Amz-Meta-Mode": "750", "X-Amz-Meta-Uid": "42", "X-Amz-Meta-Gid": "7"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := DecodePOSIXAttributes(tc.metadata, false)
			if attrs.Mode != 0750 || attrs.UID != 42 || attrs.GID != 7 {
				t.Fatalf("decoded mode %o uid %d gid %d, want 750/42/7", attrs.Mode, attrs.UID, attrs.GID)
			}
		})
	}

	attrs := DecodePOSIXAttributes(map[string]string{"x-amz-meta-mode": "755", "mode": "700"}, false)
	if attrs.Mode != 0700 {
		t.Fatalf("current key must win over legacy: mode %o, want 700", attrs.Mode)
	}
}

func TestMergePOSIXMetadataReplacesEverySpellingAndKeepsOtherKeys(t *testing.T) {
	existing := map[string]string{
		"x-amz-meta-mode": "755",
		"Uid":             "42",
		"owner-app":       "keep",
	}
	got := MergePOSIXMetadata(existing, &types.POSIXAttributes{Mode: 0600, UID: 42, GID: 7})
	want := map[string]string{"mode": "600", "uid": "42", "gid": "7", "owner-app": "keep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergePOSIXMetadata() = %v, want %v", got, want)
	}
}

func TestAttributeUpdateApply(t *testing.T) {
	base := types.POSIXAttributes{Mode: 0644, UID: 42, GID: 7}
	now := time.Unix(500, 0)

	unchanged := base
	AttributeUpdate{}.Apply(&unchanged, now)
	if unchanged != base {
		t.Fatalf("empty update changed attributes: %+v", unchanged)
	}

	mode, uid := os.FileMode(0600), 0
	updated := base
	AttributeUpdate{Mode: &mode, UID: &uid}.Apply(&updated, now)
	if updated.Mode != 0600 || updated.UID != 0 || updated.GID != 7 || !updated.Ctime.Equal(now) {
		t.Fatalf("update result = %+v, want mode 600, uid 0 (root is valid), gid 7, ctime stamped", updated)
	}
}

func newAttributeTestOps(store ObjectStore) *OperationsHandler {
	metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{
		Enabled:    true,
		TTLSeconds: 60,
		MaxEntries: 100,
	})
	return NewOperationsHandler(store, metadataCache, nil, &config.PerformanceConfig{
		MaxDirectoryEntries: 100,
		MaxFullObjectReadMB: 1,
	})
}

func setObjectMetadata(store *fakeObjectStore, key string, metadata map[string]string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	obj := store.objects[key]
	obj.metadata = copyStringMap(metadata)
	store.objects[key] = obj
}

func objectMetadata(store *fakeObjectStore, key string) (map[string]string, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	obj, ok := store.objects[key]
	return copyStringMap(obj.metadata), ok
}

func TestUpdateAttributesMergesIntoObjectMetadata(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("f.txt", []byte("data"), time.Unix(100, 0))
	// Attributes as earlier gateway versions stored them, plus a key another
	// application owns.
	setObjectMetadata(store, "f.txt", map[string]string{
		"X-Amz-Meta-Mode": "644",
		"X-Amz-Meta-Uid":  "42",
		"X-Amz-Meta-Gid":  "7",
		"owner-app":       "keep",
	})
	ops := newAttributeTestOps(store)

	mode := os.FileMode(0600)
	if err := ops.UpdateAttributes(ctx, "/f.txt", AttributeUpdate{Mode: &mode}); err != nil {
		t.Fatalf("UpdateAttributes() error = %v", err)
	}

	got, _ := objectMetadata(store, "f.txt")
	if got[MetaKeyCtime] == "" {
		t.Fatalf("ctime not stamped: %v", got)
	}
	delete(got, MetaKeyCtime)
	for k := range got {
		if strings.HasPrefix(strings.ToLower(k), legacyMetaKeyPrefix) {
			t.Fatalf("legacy key %q written back: %v", k, got)
		}
	}
	delete(got, MetaKeyAtime)
	delete(got, MetaKeyMtime)
	want := map[string]string{"mode": "600", "uid": "42", "gid": "7", "owner-app": "keep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata after chmod = %v, want %v", got, want)
	}

	data, err := store.GetObject(ctx, "f.txt")
	if err != nil || string(data) != "data" {
		t.Fatalf("object data = %q, %v; must be unchanged", data, err)
	}
}

func TestUpdateAttributesOnImplicitDirectoryCreatesMarker(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("dir/child.txt", []byte("c"), time.Unix(100, 0))
	ops := newAttributeTestOps(store)

	mode := os.FileMode(0700)
	if err := ops.UpdateAttributes(ctx, "/dir", AttributeUpdate{Mode: &mode}); err != nil {
		t.Fatalf("UpdateAttributes(implicit dir) error = %v", err)
	}
	marker, ok := objectMetadata(store, "dir/")
	if !ok {
		t.Fatal("directory marker was not created")
	}
	if marker[MetaKeyMode] != "700" {
		t.Fatalf("marker metadata = %v, want mode 700", marker)
	}
}

func TestStatAndListingExposeAttributes(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("f.txt", []byte("data"), time.Unix(100, 0))
	setObjectMetadata(store, "f.txt", map[string]string{"mode": "750", "uid": "42", "gid": "7"})
	store.put("other.txt", []byte("x"), time.Unix(100, 0))
	ops := newAttributeTestOps(store)

	info, err := ops.Stat(ctx, "/f.txt")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if a := info.Attributes(); a.Mode != 0750 || a.UID != 42 || a.GID != 7 {
		t.Fatalf("Stat attributes = %+v, want 750/42/7", a)
	}

	listed := func() map[string]*FileInfo {
		t.Helper()
		entries, err := ops.ListDirectory(ctx, "/")
		if err != nil {
			t.Fatalf("ListDirectory() error = %v", err)
		}
		byName := make(map[string]*FileInfo)
		for _, entry := range entries {
			byName[entry.Name()] = entry
		}
		return byName
	}

	entries := listed()
	if a := entries["f.txt"].Attributes(); a.UID != 42 || entries["f.txt"].Mode() != 0750 {
		t.Fatalf("listing reused no cached attributes: mode %o uid %d", entries["f.txt"].Mode(), a.UID)
	}
	if a := entries["other.txt"].Attributes(); a.UID != DefaultUID {
		t.Fatalf("uncached listing entry uid = %d, want default %d", a.UID, DefaultUID)
	}

	// Once the object changes, the cached attributes no longer describe it.
	store.put("f.txt", []byte("changed content"), time.Unix(200, 0))
	ops.metadataCache.InvalidatePath("/")
	entries = listed()
	if a := entries["f.txt"].Attributes(); a.UID != DefaultUID {
		t.Fatalf("listing reused stale attributes for a changed object: uid %d", a.UID)
	}
}
