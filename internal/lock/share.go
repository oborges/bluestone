package lock

import (
	"errors"
	"sync"
)

// DefaultMaxOpensPerFile bounds how many opens the table tracks for one file,
// so a client that never closes handles cannot grow server memory without
// bound.
const DefaultMaxOpensPerFile = 1024

// ErrSharingViolation reports that an open cannot be granted because another
// open of the same file does not permit the access asked for, or does not
// tolerate the access the new open would permit. Nothing is granted.
var ErrSharingViolation = errors.New("lock: sharing violation")

// Access is a set of ways a file can be used. The same bits describe what an
// open needs for itself and what it permits other opens to do, which is how
// SMB share modes and NFSv4 share reservations are both expressed.
type Access uint8

const (
	// AccessRead is reading file data.
	AccessRead Access = 1 << iota
	// AccessWrite is writing file data, including appending and truncating.
	AccessWrite
	// AccessDelete is deleting or renaming the file.
	AccessDelete
)

// ShareOptions configures a ShareTable. Zero values select the defaults.
type ShareOptions struct {
	MaxOpensPerFile int
}

// ShareTable records open files and what each open permits others, so that an
// open which demands exclusive use conflicts with every other open of the
// same file. It is the protocol-neutral form of SMB share modes: a client
// that opens a file with no sharing blocks other clients until it closes.
//
// The table is in-memory and does not survive a restart, and it describes
// only opens made through this gateway: it says nothing about readers of the
// same object elsewhere.
type ShareTable struct {
	mu              sync.Mutex
	maxOpensPerFile int
	files           map[string][]*ShareHandle
}

// ShareHandle is one open held in the table. Release returns it, and is safe
// to call more than once.
type ShareHandle struct {
	table   *ShareTable
	path    string
	access  Access
	share   Access
	release sync.Once
}

// ShareOpen describes a held open, for tests and diagnostics.
type ShareOpen struct {
	Access Access
	Share  Access
}

// NewShareTable returns an empty table.
func NewShareTable(opts ShareOptions) *ShareTable {
	maxOpens := opts.MaxOpensPerFile
	if maxOpens <= 0 {
		maxOpens = DefaultMaxOpensPerFile
	}
	return &ShareTable{
		maxOpensPerFile: maxOpens,
		files:           make(map[string][]*ShareHandle),
	}
}

// Acquire records an open of path that needs access and permits others share.
// It returns ErrSharingViolation when an existing open of the same path and
// the new one do not permit each other, and ErrLimit when the file already
// has as many opens as the table tracks.
func (t *ShareTable) Acquire(path string, access, share Access) (*ShareHandle, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	opens := t.files[path]
	if len(opens) >= t.maxOpensPerFile {
		return nil, ErrLimit
	}
	for _, open := range opens {
		if conflicts(access, share, open.access, open.share) {
			return nil, ErrSharingViolation
		}
	}

	handle := &ShareHandle{table: t, path: path, access: access, share: share}
	t.files[path] = append(opens, handle)
	return handle, nil
}

// Release returns the open to the table.
func (h *ShareHandle) Release() {
	if h == nil {
		return
	}
	h.release.Do(func() {
		t := h.table
		t.mu.Lock()
		defer t.mu.Unlock()

		opens := t.files[h.path]
		for i, open := range opens {
			if open != h {
				continue
			}
			opens = append(opens[:i], opens[i+1:]...)
			break
		}
		if len(opens) == 0 {
			delete(t.files, h.path)
			return
		}
		t.files[h.path] = opens
	})
}

// Opens reports the opens held on path.
func (t *ShareTable) Opens(path string) []ShareOpen {
	t.mu.Lock()
	defer t.mu.Unlock()

	opens := make([]ShareOpen, 0, len(t.files[path]))
	for _, open := range t.files[path] {
		opens = append(opens, ShareOpen{Access: open.access, Share: open.share})
	}
	return opens
}

// Len reports how many opens the table holds, across every file.
func (t *ShareTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	count := 0
	for _, opens := range t.files {
		count += len(opens)
	}
	return count
}

// Rename moves the opens held on a path, so a file keeps its share state
// under its new name.
func (t *ShareTable) Rename(oldPath, newPath string) {
	if oldPath == newPath {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	opens, ok := t.files[oldPath]
	if !ok {
		return
	}
	delete(t.files, oldPath)
	for _, open := range opens {
		open.path = newPath
	}
	t.files[newPath] = append(t.files[newPath], opens...)
}

// conflicts reports whether two opens of the same file can be held at once:
// each open's access must be permitted by what the other shares.
func conflicts(access, share, otherAccess, otherShare Access) bool {
	return access&^otherShare != 0 || otherAccess&^share != 0
}
