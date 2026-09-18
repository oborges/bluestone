package posix

import "sync"

// listingFence keeps a directory listing taken from COS out of the cache when
// the directory was invalidated while that listing was in flight. Without it
// a listing that missed a just-uploaded object (listed before the upload
// landed, cached after the sync completed) would hide the object until the
// listing expired.
type listingFence struct {
	mu       sync.Mutex
	inflight map[string]*listingFenceEntry
}

type listingFenceEntry struct {
	listings   int
	generation uint64
}

// begin records a listing of dir starting and returns the generation it must
// still see in end to be cacheable.
func (f *listingFence) begin(dir string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inflight == nil {
		f.inflight = make(map[string]*listingFenceEntry)
	}
	entry := f.inflight[dir]
	if entry == nil {
		entry = &listingFenceEntry{}
		f.inflight[dir] = entry
	}
	entry.listings++
	return entry.generation
}

// end records a listing of dir finishing and reports whether dir was left
// alone since begin returned generation.
func (f *listingFence) end(dir string, generation uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.inflight[dir]
	if entry == nil {
		return false
	}
	unchanged := entry.generation == generation
	entry.listings--
	if entry.listings == 0 {
		delete(f.inflight, dir)
	}
	return unchanged
}

// invalidate marks listings of dir that are in flight as stale.
func (f *listingFence) invalidate(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry := f.inflight[dir]; entry != nil {
		entry.generation++
	}
}
