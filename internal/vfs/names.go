package vfs

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/oborges/bluestone/pkg/types"
)

// Object keys may contain characters Windows cannot use in names. Views with
// Windows naming present them as Unicode private-use characters using the
// Services for Macintosh (SFM) mapping, the scheme macOS SMB clients, the
// Linux CIFS client, and Samba's vfs_catia/vfs_fruit share, and map them back
// to the stored key.
const (
	// sfmControlOffset maps U+0001..U+001F to U+F001..U+F01F.
	sfmControlOffset  = 0xF000
	sfmTrailingSpace  = ''
	sfmTrailingPeriod = ''
	sfmFirst          = ''
	sfmLast           = ''
)

var sfmReserved = map[rune]rune{
	'"':  '',
	'*':  '',
	':':  '',
	'<':  '',
	'>':  '',
	'?':  '',
	'\\': '',
	'|':  '',
}

var sfmReservedReverse = func() map[rune]rune {
	reverse := make(map[rune]rune, len(sfmReserved))
	for char, mapped := range sfmReserved {
		reverse[mapped] = char
	}
	return reverse
}()

// windowsName presents a stored key name to Windows naming clients.
func windowsName(key string) string {
	if key == "." || key == ".." {
		return key
	}
	runes := []rune(key)
	changed := false
	for i, r := range runes {
		if r >= 0x01 && r <= 0x1F {
			runes[i] = sfmControlOffset + r
			changed = true
		} else if mapped, ok := sfmReserved[r]; ok {
			runes[i] = mapped
			changed = true
		}
	}
	// Windows drops trailing spaces and periods, so those are mapped too.
trailing:
	for i := len(runes) - 1; i >= 0; i-- {
		switch runes[i] {
		case ' ':
			runes[i] = sfmTrailingSpace
		case '.':
			runes[i] = sfmTrailingPeriod
		default:
			break trailing
		}
		changed = true
	}
	if !changed {
		return key
	}
	return string(runes)
}

// keyName maps a Windows naming client's name back to the stored key name.
func keyName(name string) string {
	if !strings.ContainsFunc(name, func(r rune) bool { return r >= sfmFirst && r <= sfmLast }) {
		return name
	}
	runes := []rune(name)
	for i, r := range runes {
		switch {
		case r >= sfmFirst && r <= sfmControlOffset+0x1F:
			runes[i] = r - sfmControlOffset
		case r == sfmTrailingSpace:
			runes[i] = ' '
		case r == sfmTrailingPeriod:
			runes[i] = '.'
		default:
			if char, ok := sfmReservedReverse[r]; ok {
				runes[i] = char
			}
		}
	}
	return string(runes)
}

// keyPath maps a caller's name to the key path it refers to. Views without
// Windows naming use the name exactly as given. With Windows naming, each
// component is unmapped and then matched case-insensitively against its
// parent's entries.
func (fs *Filesystem) keyPath(name string) string {
	if !fs.windowsNames {
		return fs.Join(fs.root, name)
	}
	current := fs.Join(fs.root)
	for _, component := range strings.Split(filepath.Clean("/"+name), "/") {
		if component == "" {
			continue
		}
		current = fs.Join(current, fs.matchChild(current, keyName(component)))
	}
	return current
}

// matchChild returns the entry of dir that key names: an exact match, else
// the first case-insensitive match in byte order, else key itself (a name
// that does not exist yet).
func (fs *Filesystem) matchChild(dir, key string) string {
	match := ""
	for _, name := range fs.childNames(dir) {
		if name == key {
			return name
		}
		if strings.EqualFold(name, key) && (match == "" || name < match) {
			match = name
		}
	}
	if match != "" {
		return match
	}
	return key
}

// childNames lists the entry names of a directory key path: listed objects,
// staged files, and directories implied by staged files below it. Entries
// with a pending delete are left out.
func (fs *Filesystem) childNames(dir string) []string {
	var names []string
	if entries, err := fs.ops.ListDirectory(fs.requestContext(), dir); err == nil {
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
	}
	if fs.featureFlags == nil || !fs.featureFlags.IsStagingEnabled() || fs.stagingManager == nil {
		return names
	}
	for _, session := range fs.stagingManager.GetSessionsInDirectory(dir) {
		if !fs.stagingManager.HasPendingDelete(session.Path) {
			names = append(names, filepath.Base(session.Path))
		}
	}
	// Staged files in directories the object store does not list yet.
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for _, dirty := range fs.stagingManager.DirtyPathsUnder(dir) {
		rest := strings.TrimPrefix(dirty, prefix)
		if slash := strings.Index(rest, "/"); rest != dirty && slash > 0 {
			names = append(names, rest[:slash])
		}
	}
	return names
}

// renameTargetPath resolves a rename destination. A destination that matches
// the source itself only case-insensitively is a case change of the source,
// so the requested spelling is used rather than the source's.
func (fs *Filesystem) renameTargetPath(oldFull, newpath string) string {
	target := fs.keyPath(newpath)
	if !fs.windowsNames || target != oldFull {
		return target
	}
	return fs.Join(filepath.Dir(target), keyName(filepath.Base(filepath.Clean("/"+newpath))))
}

// presentEntry shows an entry under the name the view's clients use.
func (fs *Filesystem) presentEntry(info os.FileInfo) os.FileInfo {
	if !fs.windowsNames || info == nil {
		return info
	}
	if name := windowsName(info.Name()); name != info.Name() {
		return namedEntry{FileInfo: info, name: name}
	}
	return info
}

// presentEntries shows directory entries under the names the view's clients
// use.
func (fs *Filesystem) presentEntries(entries []os.FileInfo) []os.FileInfo {
	if !fs.windowsNames {
		return entries
	}
	for i, entry := range entries {
		entries[i] = fs.presentEntry(entry)
	}
	return entries
}

// namedEntry is an entry presented under a different name.
type namedEntry struct {
	os.FileInfo
	name string
}

func (e namedEntry) Name() string { return e.name }

// Attributes keeps the renamed entry's attributes visible.
func (e namedEntry) Attributes() types.POSIXAttributes { return FileAttributes(e.FileInfo) }
