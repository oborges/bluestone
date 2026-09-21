package nfs

import (
	"hash/fnv"
	"os"
	"testing"
	"time"
)

type ownedInfo struct{ uid, gid uint32 }

func (ownedInfo) Name() string                 { return "f" }
func (ownedInfo) Size() int64                  { return 1 }
func (ownedInfo) Mode() os.FileMode            { return 0o644 }
func (ownedInfo) ModTime() time.Time           { return time.Unix(0, 0) }
func (ownedInfo) IsDir() bool                  { return false }
func (ownedInfo) Sys() interface{}             { return nil }
func (o ownedInfo) NFSOwner() (uint32, uint32) { return o.uid, o.gid }

// A file that reports its owner through file.Owner is shown with that owner,
// and keeps the file id derived from its path.
func TestToFileAttributeReportsOwner(t *testing.T) {
	attr := ToFileAttribute(ownedInfo{uid: 1000, gid: 1001}, "/dir/f")
	if attr.UID != 1000 || attr.GID != 1001 {
		t.Errorf("owner %d:%d, want 1000:1001", attr.UID, attr.GID)
	}
	h := fnv.New64()
	_, _ = h.Write([]byte("/dir/f"))
	if attr.Fileid != h.Sum64() {
		t.Errorf("file id %d, want the path's hash %d", attr.Fileid, h.Sum64())
	}
}
