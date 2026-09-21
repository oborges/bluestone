package server

import (
	"context"
	"strings"
	"sync"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
)

// Delete semantics belong to the file, not to the handle that asked for them
// (MS-FSA sections 2.1.5.4 and 2.1.5.14.3). Setting a file's disposition
// marks it delete-pending for every open of it; new opens are refused with
// STATUS_DELETE_PENDING; and the file is removed when its last handle
// closes, not when the handle that set the disposition does. An open made
// with FILE_DELETE_ON_CLOSE marks the file pending when that open closes.
//
// Opens are tracked across every connection, since two clients can hold the
// same file open, and they follow renames: the handle that renamed a file
// deletes it under its new name, never whatever now has the old one.

// fileKey names a file on a share. Paths compare case-insensitively, which
// is how the server tells clients its shares behave.
type fileKey struct {
	share string
	path  string
}

func keyFor(share, path string) fileKey {
	return fileKey{share: strings.ToLower(share), path: normalizeOpenPath(path)}
}

// normalizeOpenPath reduces a client path to one form: backslash-separated,
// no leading or trailing separator, lower case.
func normalizeOpenPath(p string) string {
	return strings.ToLower(strings.Trim(strings.ReplaceAll(p, "/", "\\"), "\\"))
}

// openFile is one file with at least one open.
type openFile struct {
	opens         map[*openHandle]struct{}
	deletePending bool
}

type openFiles struct {
	mu    sync.Mutex
	files map[fileKey]*openFile
}

// fileTable returns the server's table, creating it for a Server built
// without New.
func (s *Server) fileTable() *openFiles {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = &openFiles{files: make(map[fileKey]*openFile)}
	}
	return s.files
}

// deletePending reports whether the file at key is waiting to be deleted,
// which refuses new opens of it.
func (t *openFiles) deletePending(key fileKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.files[key]
	return ok && f.deletePending
}

// add records an open of the file at key.
func (t *openFiles) add(key fileKey, oh *openHandle) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.files[key]
	if !ok {
		f = &openFile{opens: make(map[*openHandle]struct{})}
		t.files[key] = f
	}
	f.opens[oh] = struct{}{}
	oh.key = key
}

// setDeletePending sets or clears the file's disposition on behalf of oh.
func (t *openFiles) setDeletePending(oh *openHandle, pending bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f, ok := t.files[oh.key]; ok {
		f.deletePending = pending
	}
}

// isDeletePending reports the disposition of the file oh has open.
func (t *openFiles) isDeletePending(oh *openHandle) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.files[oh.key]
	return ok && f.deletePending
}

// release forgets oh. It reports whether this was the file's last open and
// the file is to be deleted, with the path to delete it under.
func (t *openFiles) release(oh *openHandle) (remove bool, path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.files[oh.key]
	if !ok {
		return false, ""
	}
	if oh.deleteOnClose {
		f.deletePending = true
	}
	delete(f.opens, oh)
	if len(f.opens) > 0 {
		return false, ""
	}
	delete(t.files, oh.key)
	return f.deletePending, oh.currentPath()
}

// checkRename reports whether oh's file cannot be renamed to newPath:
// pending when it is waiting to be deleted, busy when it is a directory with
// anything open inside it, a file with one of its named streams open, or
// the rename would replace a file someone has open. Windows refuses the
// first, second and last; open streams are refused here because their
// handles would go on naming the old path.
func (t *openFiles) checkRename(oh *openHandle, newPath string) (pending, busy bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f, ok := t.files[oh.key]; ok && f.deletePending {
		return true, false
	}
	children := oh.key.path + "\\"
	// A handle to one of the file's named streams names it by its old path.
	streams := oh.key.path + ":"
	target := fileKey{share: oh.key.share, path: normalizeOpenPath(newPath)}
	for key := range t.files {
		if key.share != oh.key.share {
			continue
		}
		if strings.HasPrefix(key.path, children) || strings.HasPrefix(key.path, streams) {
			return false, true
		}
		if key == target && key != oh.key {
			return false, true
		}
	}
	return false, false
}

// renamed moves every open of oh's file to newPath, so later operations on
// any of those handles, a delete among them, act on the file where it now
// is.
func (t *openFiles) renamed(oh *openHandle, newPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	oldKey := oh.key
	f, ok := t.files[oldKey]
	if !ok {
		return
	}
	newKey := fileKey{share: oldKey.share, path: normalizeOpenPath(newPath)}
	clientPath := strings.Trim(strings.ReplaceAll(newPath, "/", "\\"), "\\")
	delete(t.files, oldKey)
	t.files[newKey] = f
	for other := range f.opens {
		other.key = newKey
		other.setPath(clientPath)
	}
}

// dirHasEntries reports whether the directory open as h lists anything,
// which a directory must not when it is deleted.
func dirHasEntries(ctx context.Context, h vfs.Handle) (bool, error) {
	for fi, err := range h.Enumerate(ctx, "*") {
		if err != nil {
			return false, err
		}
		if fi.Name != "." && fi.Name != ".." {
			return true, nil
		}
	}
	return false, nil
}
