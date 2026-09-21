package vfs

import (
	"os"
	"strings"
	"sync"

	"github.com/oborges/bluestone/internal/posix"
)

// ChangeAction says what happened to a path.
type ChangeAction int

const (
	// ChangeAdded is a file or directory created.
	ChangeAdded ChangeAction = iota + 1
	// ChangeRemoved is a file or directory deleted.
	ChangeRemoved
	// ChangeModified is a file or directory changed in place; Kind says how.
	ChangeModified
	// ChangeRenamed is a file or directory moved from OldPath to Path.
	ChangeRenamed
)

// ChangeKind says what a ChangeModified changed.
type ChangeKind uint32

const (
	// ChangeData is the file's contents, and with them its size and
	// modification time.
	ChangeData ChangeKind = 1 << iota
	// ChangeAttributes is its mode, owner or Windows attribute flags.
	ChangeAttributes
	// ChangeTimes is its access, modification or creation time.
	ChangeTimes
	// ChangeStreams is its named data streams.
	ChangeStreams
)

// Change is one change to the namespace or to a file, made through any
// protocol. Paths are key paths, as stored: slash-separated from the bucket
// root.
type Change struct {
	Action  ChangeAction
	Path    string
	OldPath string // for ChangeRenamed
	IsDir   bool
	Kind    ChangeKind // for ChangeModified
}

// changeFeed passes every change made through the filesystem to its
// subscribers, which is how SMB clients watching a directory learn of
// changes made by NFS clients, other SMB clients, or themselves, without
// anything listing the directory to find out.
type changeFeed struct {
	mu   sync.RWMutex
	subs map[int]func(Change)
	next int
}

// SubscribeChanges calls fn with every change made through the filesystem,
// from any view, until the returned function is called. fn runs on the
// goroutine making the change, so it must not block or call back into the
// filesystem.
func (fs *Filesystem) SubscribeChanges(fn func(Change)) (cancel func()) {
	feed := fs.changes
	if feed == nil {
		return func() {}
	}
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if feed.subs == nil {
		feed.subs = make(map[int]func(Change))
	}
	id := feed.next
	feed.next++
	feed.subs[id] = fn
	return func() {
		feed.mu.Lock()
		defer feed.mu.Unlock()
		delete(feed.subs, id)
	}
}

// publish passes a change to the subscribers. With none, it costs a read
// lock.
func (feed *changeFeed) publish(c Change) {
	if feed == nil {
		return
	}
	feed.mu.RLock()
	defer feed.mu.RUnlock()
	for _, fn := range feed.subs {
		fn(c)
	}
}

func (fs *Filesystem) changed(c Change) { fs.changes.publish(c) }

// active reports whether anything is subscribed, so callers can skip work
// done only to describe a change.
func (feed *changeFeed) active() bool {
	if feed == nil {
		return false
	}
	feed.mu.RLock()
	defer feed.mu.RUnlock()
	return len(feed.subs) > 0
}

// isDirPath reports whether the key path is a directory, for describing a
// change. It is asked only when something is subscribed.
func (fs *Filesystem) isDirPath(fullPath string) bool {
	info, err := fs.ops.Stat(fs.requestContext(), fullPath)
	return err == nil && info.IsDir()
}

// existsPath reports whether the key path exists, for describing a change.
func (fs *Filesystem) existsPath(fullPath string) bool {
	_, err := fs.statPath(fullPath, fullPath)
	return err == nil
}

// Remove deletes the named file or empty directory.
func (fs *Filesystem) Remove(filename string) error {
	fullPath := fs.keyPath(filename)
	isDir := fs.changes.active() && fs.isDirPath(fullPath)
	if err := fs.remove(filename); err != nil {
		return err
	}
	fs.changed(Change{Action: ChangeRemoved, Path: fullPath, IsDir: isDir})
	return nil
}

// Rename moves a file or directory.
func (fs *Filesystem) Rename(oldpath, newpath string) error {
	oldFull := fs.keyPath(oldpath)
	if err := fs.rename(oldpath, newpath); err != nil {
		return err
	}
	if fs.changes.active() {
		newFull := fs.keyPath(newpath)
		fs.changed(Change{Action: ChangeRenamed, Path: newFull, OldPath: oldFull, IsDir: fs.isDirPath(newFull)})
	}
	return nil
}

// MkdirAll creates a directory and any missing parents.
func (fs *Filesystem) MkdirAll(filename string, perm os.FileMode) error {
	fullPath := fs.keyPath(filename)
	existed := fs.changes.active() && fs.existsPath(fullPath)
	if err := fs.mkdirAll(filename, perm); err != nil {
		return err
	}
	if !existed {
		fs.changed(Change{Action: ChangeAdded, Path: fullPath, IsDir: true})
	}
	return nil
}

// SetAttributes applies an attribute change to the named file or directory.
// A staged file records the change for its next sync (access and
// modification times are not staged); anything else gets a metadata-only
// update in COS that does not rewrite the object's bytes.
func (fs *Filesystem) SetAttributes(name string, update posix.AttributeUpdate) error {
	if err := fs.setAttributes(name, update); err != nil {
		return err
	}
	var kind ChangeKind
	if update.Mode != nil || update.UID != nil || update.GID != nil || update.WindowsAttributes != nil {
		kind |= ChangeAttributes
	}
	if update.Atime != nil || update.Mtime != nil || update.Btime != nil {
		kind |= ChangeTimes
	}
	if update.Streams != nil {
		kind |= ChangeStreams
	}
	if kind != 0 && fs.changes.active() {
		fullPath := fs.keyPath(name)
		fs.changed(Change{Action: ChangeModified, Path: fullPath, IsDir: fs.isDirPath(fullPath), Kind: kind})
	}
	return nil
}

// CopyFile copies src over dst inside the bucket; see copyFile.
func (fs *Filesystem) CopyFile(src, dst string, onlyUnwritten bool) error {
	dstFull := fs.keyPath(dst)
	existed := fs.changes.active() && fs.existsPath(dstFull)
	if err := fs.copyFile(src, dst, onlyUnwritten); err != nil {
		return err
	}
	if existed {
		fs.changed(Change{Action: ChangeModified, Path: dstFull, Kind: ChangeData})
	} else {
		fs.changed(Change{Action: ChangeAdded, Path: dstFull})
	}
	return nil
}

// noteWrite publishes a handle's first write; Close publishes the rest.
// Called with f.mu held.
func (f *File) noteWrite(n int) {
	if n <= 0 || f.changes == nil {
		return
	}
	if !f.wrote {
		f.changes.publish(Change{Action: ChangeModified, Path: f.path, Kind: ChangeData})
	}
	f.wrote = true
}

// Truncate changes the file size, shrinking or zero-extending it.
func (f *File) Truncate(size int64) error {
	if err := f.truncate(size); err != nil {
		return err
	}
	f.changes.publish(Change{Action: ChangeModified, Path: f.path, Kind: ChangeData})
	return nil
}

// ClientPath is how this view's clients name a key path: relative to the
// view's root, slash-separated, in the view's naming. It reports false for a
// path outside the root, or one clients cannot see.
func (fs *Filesystem) ClientPath(keyPath string) (string, bool) {
	if isReservedPath(keyPath) {
		return "", false
	}
	root := strings.TrimSuffix(fs.root, "/")
	rel := keyPath
	if root != "" {
		if keyPath != root && !strings.HasPrefix(keyPath, root+"/") {
			return "", false
		}
		rel = strings.TrimPrefix(keyPath, root)
	}
	rel = strings.Trim(rel, "/")
	if rel == "" || !fs.windowsNames {
		return rel, true
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		parts[i] = windowsName(part)
	}
	return strings.Join(parts, "/"), true
}

// ReportExternalChange publishes a change made directly in the bucket, as
// the object refresh scanner finds them, so watchers hear of changes that
// did not pass through the gateway.
func (fs *Filesystem) ReportExternalChange(c posix.ObjectChange) {
	change := Change{Path: c.Path, IsDir: c.IsDir}
	switch c.Kind {
	case posix.ObjectAdded:
		change.Action = ChangeAdded
	case posix.ObjectRemoved:
		change.Action = ChangeRemoved
	case posix.ObjectModified:
		change.Action, change.Kind = ChangeModified, ChangeData
	default:
		return
	}
	fs.changed(change)
}
