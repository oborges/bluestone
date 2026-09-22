package vfs

import (
	"os"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/pkg/types"
)

// maxTrackedDirs bounds dirTimes. Past it, the oldest tenth is forgotten.
const maxTrackedDirs = 100_000

// dirTimes remembers when the gateway last added, removed or renamed an
// entry in each directory. A directory in COS is a marker object, or no object
// at all, whose stored modification time does not change when entries come
// and go. Without this record, a directory would report the same modification
// time, and so the same NFSv4 change attribute, before and after a create.
// Clients key their cached listings on that attribute, and would keep serving
// a listing that lacks the new file.
//
// The record lives in memory, shared by all views. After a restart,
// directories report their stored times again.
type dirTimes struct {
	mu   sync.RWMutex
	last time.Time
	dirs map[string]time.Time // key path -> modification time
}

// touch records a change to the entries of the directory at key path dir.
// Times only move forward, even when two changes land within the clock's
// resolution, so each change gives the directory a new time.
func (d *dirTimes) touch(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if !now.After(d.last) {
		now = d.last.Add(time.Nanosecond)
	}
	d.last = now
	if d.dirs == nil {
		d.dirs = make(map[string]time.Time)
	}
	d.dirs[dir] = now
	if len(d.dirs) > maxTrackedDirs {
		d.evictOldestLocked()
	}
}

// forget drops what is recorded for the path, whose own times were set
// explicitly or which no longer exists.
func (d *dirTimes) forget(p string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.dirs, p)
}

// move carries a renamed directory's record to its new path.
func (d *dirTimes) move(oldPath, newPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.dirs[oldPath]; ok {
		delete(d.dirs, oldPath)
		d.dirs[newPath] = t
	}
}

// modTime returns the recorded modification time of the directory at key
// path dir, if any.
func (d *dirTimes) modTime(dir string) (time.Time, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t, ok := d.dirs[dir]
	return t, ok
}

func (d *dirTimes) evictOldestLocked() {
	times := make([]time.Time, 0, len(d.dirs))
	for _, t := range d.dirs {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	cutoff := times[len(times)/10]
	for p, t := range d.dirs {
		if !t.After(cutoff) {
			delete(d.dirs, p)
		}
	}
}

// record updates directory times for a change. It runs for every change,
// whether or not anything subscribes to the feed.
func (d *dirTimes) record(c Change) {
	p := cleanKey(c.Path)
	switch c.Action {
	case ChangeAdded:
		d.touch(path.Dir(p))
	case ChangeRemoved:
		d.forget(p)
		d.touch(path.Dir(p))
	case ChangeRenamed:
		old := cleanKey(c.OldPath)
		d.move(old, p)
		d.touch(path.Dir(old))
		d.touch(path.Dir(p))
	case ChangeModified:
		// Times set explicitly, as by touch or an SMB client, replace the
		// recorded one.
		if c.Kind&ChangeTimes != 0 {
			d.forget(p)
		}
	}
}

// cleanKey gives a key path one spelling: rooted, with no trailing slash.
func cleanKey(p string) string {
	return path.Clean("/" + p)
}

// withDirTime reports a directory's recorded modification time when it is
// later than the stored one. Other entries pass through.
func (fs *Filesystem) withDirTime(fullPath string, info os.FileInfo) os.FileInfo {
	if info == nil || !info.IsDir() || fs.changes == nil {
		return info
	}
	t, ok := fs.changes.dirTimes.modTime(cleanKey(fullPath))
	if !ok || !t.After(info.ModTime()) {
		return info
	}
	return touchedDir{FileInfo: info, modTime: t}
}

// touchedDir is a directory whose entries changed after its stored
// modification time.
type touchedDir struct {
	os.FileInfo
	modTime time.Time
}

func (d touchedDir) ModTime() time.Time { return d.modTime }

// Attributes reports the stored attributes with the later modification
// time, which is also a change to the directory's status time.
func (d touchedDir) Attributes() types.POSIXAttributes {
	attrs := FileAttributes(d.FileInfo)
	attrs.Mtime = d.modTime
	if d.modTime.After(attrs.Ctime) {
		attrs.Ctime = d.modTime
	}
	return attrs
}

// NFSOwner keeps the directory's owner visible over NFS.
func (d touchedDir) NFSOwner() (uid, gid uint32) { return posix.OwnerIDs(d.Attributes()) }
