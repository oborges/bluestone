package posix

import (
	"context"
	"fmt"
	"strings"

	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/pkg/types"
)

// ChildLister is an ObjectStore that can list just the immediate children
// of a prefix, as S3's "/" delimiter does: the objects directly under it,
// and one prefix for each subdirectory, without enumerating subtrees.
type ChildLister interface {
	ListChildren(ctx context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, []string, error)
}

// ChildNamesWithPrefix returns the names of dir's entries that begin with
// namePrefix. A cached listing of dir answers without a COS call; otherwise
// only the keys under that prefix are listed, so the cost follows the number
// of matching names rather than the size of the directory. With a store that
// lists children, a matching subdirectory costs one entry rather than its
// whole subtree. Entries are reduced and filtered as ListDirectory does: one
// name per immediate child, and none for an object whose delete is pending.
// A subdirectory is named even if everything in it has a pending delete,
// which ListDirectory would hide; that only widens the candidates a caller
// compares names against.
func (h *OperationsHandler) ChildNamesWithPrefix(ctx context.Context, dir, namePrefix string) ([]string, error) {
	if entry, ok := h.metadataCache.Get(dir); ok && entry.ChildEntries != nil {
		var names []string
		for _, child := range entry.ChildEntries {
			if strings.HasPrefix(child.Name(), namePrefix) {
				names = append(names, child.Name())
			}
		}
		return names, nil
	}

	dirPrefix := ListPrefix(dir)
	maxEntries := h.maxDirectoryEntries()
	var names []string
	seen := make(map[string]bool)
	add := func(key string) {
		relPath := strings.TrimPrefix(key, dirPrefix)
		if relPath == "" {
			return
		}
		name := strings.Split(strings.Trim(relPath, "/"), "/")[0]
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}

	metrics.RecordCOSListObjects()
	if lister, ok := h.cosClient.(ChildLister); ok {
		objects, prefixes, err := lister.ListChildren(ctx, dirPrefix+namePrefix, maxEntries+2)
		if err != nil {
			return nil, err
		}
		for _, obj := range objects {
			if !h.isPendingDelete(obj.Key) {
				add(obj.Key)
			}
		}
		for _, prefix := range prefixes {
			add(prefix)
		}
		return h.checkChildNames(names, dir, namePrefix, maxEntries)
	}

	objects, err := h.cosClient.ListObjects(ctx, dirPrefix+namePrefix, maxEntries+2)
	if err != nil {
		return nil, err
	}
	for _, obj := range objects {
		if !h.isPendingDelete(obj.Key) {
			add(obj.Key)
		}
	}
	return h.checkChildNames(names, dir, namePrefix, maxEntries)
}

// checkChildNames enforces max_directory_entries on a prefix's names, as
// ListDirectory does on a whole directory.
func (h *OperationsHandler) checkChildNames(names []string, dir, namePrefix string, maxEntries int) ([]string, error) {
	if len(names) > maxEntries {
		return nil, fmt.Errorf("entries of %s beginning with %q exceed max_directory_entries=%d: %w",
			dir, namePrefix, maxEntries, ErrDirectoryTooLarge)
	}
	return names, nil
}
