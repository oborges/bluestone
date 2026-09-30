package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/oborges/bluestone/internal/posix"
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

// KeyPath is the key path a caller's name refers to, resolved the way the
// view resolves names when opening files.
func (fs *Filesystem) KeyPath(name string) string { return fs.keyPath(name) }

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
//
// Most names a client sends exist exactly as spelled, and confirming that
// takes one lookup. Only a name that does not exist as spelled needs the
// directory searched for another spelling, and then only the entries that
// begin with a case variant of its first characters are listed. Listing the
// whole directory for every name made each operation cost as much as the
// directory is large: over SMB a directory of 50,000 files could not be
// created or deleted in six hours.
//
// A cached listing of dir answers in memory, as it always did: that is
// cheaper than the lookup when the name is spelled differently.
func (fs *Filesystem) matchChild(dir, key string) string {
	candidates, cached := fs.ops.CachedChildNames(dir)
	if cached {
		candidates = append(candidates, fs.stagedChildNames(dir)...)
	} else {
		if _, err := fs.statPath(fs.Join(dir, key), key); err == nil {
			return key
		}
		candidates = fs.foldCandidates(dir, key)
	}
	match := ""
	for _, name := range candidates {
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

// maxFoldPrefixes bounds the case variants of a name's start that
// foldCandidates lists, and so the listings one lookup makes.
const maxFoldPrefixes = 8

// foldCandidates returns the entries of dir that could match key
// case-insensitively: listed entries beginning with a case variant of key's
// first characters, and staged entries.
func (fs *Filesystem) foldCandidates(dir, key string) []string {
	if !utf8.ValidString(key) {
		// EqualFold reads invalid bytes as U+FFFD, which no prefix of
		// the stored bytes captures: search the whole directory.
		return fs.childNames(dir)
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		names []string
	)
	for _, prefix := range foldPrefixes(key, maxFoldPrefixes) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A listing that fails contributes nothing, as in childNames.
			found, err := fs.ops.ChildNamesWithPrefix(fs.requestContext(), dir, prefix)
			if err != nil {
				return
			}
			mu.Lock()
			names = append(names, found...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return append(names, fs.stagedChildNames(dir)...)
}

// foldPrefixes returns every case variant of the start of key, taking as
// much of key as keeps the variants to at most limit (and always its first
// character). strings.EqualFold matches character by character through
// simple case folding, so any name equal to key under it begins with one of
// these.
func foldPrefixes(key string, limit int) []string {
	prefixes := []string{""}
	for i, r := range key {
		orbit := foldOrbit(r)
		if i > 0 && len(prefixes)*len(orbit) > limit {
			break
		}
		next := make([]string, 0, len(prefixes)*len(orbit))
		for _, prefix := range prefixes {
			for _, variant := range orbit {
				next = append(next, prefix+string(variant))
			}
		}
		prefixes = next
	}
	return prefixes
}

// foldOrbit returns the runes that simple case folding treats as equal to
// r, r first: a letter's cases, and for some letters more than two (k, K and
// the Kelvin sign).
func foldOrbit(r rune) []rune {
	orbit := []rune{r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		orbit = append(orbit, f)
	}
	return orbit
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
	return append(names, fs.stagedChildNames(dir)...)
}

// stagedChildNames lists the entry names of a directory key path that exist
// only in staging so far: staged files, and directories implied by staged
// files below it. Entries with a pending delete are left out.
func (fs *Filesystem) stagedChildNames(dir string) []string {
	var names []string
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

// NFSOwner keeps the renamed entry's owner visible over NFS.
func (e namedEntry) NFSOwner() (uid, gid uint32) { return posix.OwnerIDs(e.Attributes()) }
