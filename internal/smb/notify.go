package smb

import (
	"strings"

	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/internal/vfs"
	smbvfs "github.com/sonroyaalmerol/go-smb-server/smb/vfs"
)

var _ smbvfs.ChangeNotifier = (*Backend)(nil)

// FILE_NOTIFY_CHANGE_* bits a change to a file matches (MS-SMB2 section
// 2.2.35).
const (
	notifyAttributes  uint32 = 0x00000004
	notifySize        uint32 = 0x00000008
	notifyLastWrite   uint32 = 0x00000010
	notifyLastAccess  uint32 = 0x00000020
	notifyCreation    uint32 = 0x00000040
	notifyStreamName  uint32 = 0x00000200
	notifyStreamSize  uint32 = 0x00000400
	notifyStreamWrite uint32 = 0x00000800
)

// NotifyChanges implements smbvfs.ChangeNotifier with the filesystem's own
// change feed, which carries changes made through NFS as well as SMB. SMB
// clients watching a directory hear about them as they happen, and nothing
// lists the directory to find out.
//
// Changes made directly in the bucket, by tools that bypass the gateway,
// arrive when the object refresh scanner finds them, if it is enabled.
func (b *Backend) NotifyChanges(fn func(smbvfs.Change)) func() {
	return b.fs.SubscribeChanges(func(c vfs.Change) {
		path, ok := b.fs.ClientPath(c.Path)
		if !ok || path == "" {
			return
		}
		change := smbvfs.Change{Path: toSMBPath(path), IsDir: c.IsDir,
			External: c.Protocol != metrics.ProtocolSMB}
		switch c.Action {
		case vfs.ChangeAdded:
			change.Action = smbvfs.ChangeAdded
		case vfs.ChangeRemoved:
			change.Action = smbvfs.ChangeRemoved
		case vfs.ChangeRenamed:
			oldPath, ok := b.fs.ClientPath(c.OldPath)
			if !ok {
				// Moved in from outside the share: an addition.
				change.Action = smbvfs.ChangeAdded
				break
			}
			change.Action, change.OldPath = smbvfs.ChangeRenamed, toSMBPath(oldPath)
		case vfs.ChangeModified:
			change.Action, change.Filter = smbvfs.ChangeModified, notifyFilter(c.Kind)
		default:
			return
		}
		fn(change)
	})
}

// notifyFilter is what a modification matches in a client's filter.
func notifyFilter(kind vfs.ChangeKind) uint32 {
	var filter uint32
	if kind&vfs.ChangeData != 0 {
		filter |= notifySize | notifyLastWrite
	}
	if kind&vfs.ChangeAttributes != 0 {
		filter |= notifyAttributes
	}
	if kind&vfs.ChangeTimes != 0 {
		filter |= notifyLastWrite | notifyLastAccess | notifyCreation
	}
	if kind&vfs.ChangeStreams != 0 {
		filter |= notifyStreamName | notifyStreamSize | notifyStreamWrite
	}
	return filter
}

func toSMBPath(p string) string {
	return strings.ReplaceAll(p, "/", `\`)
}
