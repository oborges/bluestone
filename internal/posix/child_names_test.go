package posix

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oborges/bluestone/pkg/types"
)

// childListingStore adds S3's delimiter listing to the fake store, and
// counts the keys and prefixes it returns.
type childListingStore struct {
	*fakeObjectStore
	returned int
}

func (s *childListingStore) ListChildren(ctx context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, []string, error) {
	all, err := s.fakeObjectStore.ListObjects(ctx, prefix, 0)
	if err != nil {
		return nil, nil, err
	}
	var objects []*types.ObjectMetadata
	var prefixes []string
	for _, obj := range all {
		rest := strings.TrimPrefix(obj.Key, prefix)
		if slash := strings.Index(rest, "/"); slash >= 0 {
			common := prefix + rest[:slash+1]
			if !slices.Contains(prefixes, common) {
				prefixes = append(prefixes, common)
			}
			continue
		}
		objects = append(objects, obj)
	}
	s.returned += len(objects) + len(prefixes)
	return objects, prefixes, nil
}

// ChildNamesWithPrefix names the immediate children beginning with a prefix,
// the same with or without a store that lists children, and leaves out a
// file whose delete is pending.
func TestChildNamesWithPrefix(t *testing.T) {
	populate := func() *fakeObjectStore {
		store := newFakeObjectStore()
		now := time.Now()
		for _, key := range []string{
			"dir/", "dir/Alpha", "dir/alpine", "dir/beta", "dir/alpha-pending",
			"dir/Alps/", "dir/Alps/deep/1", "dir/Alps/deep/2", "dir/Alps/deep/3",
			"dirt/Alpha",
		} {
			store.put(key, []byte("x"), now)
		}
		return store
	}
	check := func(t *testing.T, store ObjectStore) {
		t.Helper()
		ops, _ := newRefreshTestOps(t, nil)
		ops.cosClient = store
		ops.SetPendingDeleteCheck(func(path string) bool { return path == "/dir/alpha-pending" })
		for _, tc := range []struct {
			prefix string
			want   []string
		}{
			{"Al", []string{"Alpha", "Alps"}},
			{"al", []string{"alpine"}},
			{"alpha", nil},
			{"b", []string{"beta"}},
			{"Alps", []string{"Alps"}},
		} {
			got, err := ops.ChildNamesWithPrefix(context.Background(), "/dir", tc.prefix)
			if err != nil {
				t.Fatalf("ChildNamesWithPrefix(%q): %v", tc.prefix, err)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ChildNamesWithPrefix(/dir, %q) = %q, want %q", tc.prefix, got, tc.want)
			}
		}
	}

	t.Run("store that only lists subtrees", func(t *testing.T) {
		check(t, populate())
	})
	t.Run("store that lists children", func(t *testing.T) {
		store := &childListingStore{fakeObjectStore: populate()}
		check(t, store)
		// "Al" names Alpha and the Alps directory, not the three objects
		// under Alps.
		store.returned = 0
		ops, _ := newRefreshTestOps(t, nil)
		ops.cosClient = store
		if _, err := ops.ChildNamesWithPrefix(context.Background(), "/dir", "Al"); err != nil {
			t.Fatal(err)
		}
		if store.returned != 2 {
			t.Errorf("listing children beginning with Al returned %d keys and prefixes, want 2", store.returned)
		}
	})
}
