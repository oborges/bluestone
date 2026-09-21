package file

import "os"

type FileInfo struct {
	Nlink  uint32
	UID    uint32
	GID    uint32
	Major  uint32
	Minor  uint32
	Fileid uint64
}

// Owner is implemented by an os.FileInfo that knows its file's owner
// without a platform stat structure from Sys(), such as a file whose owner
// is kept in object metadata. ToFileAttribute reports what it returns.
type Owner interface {
	NFSOwner() (uid, gid uint32)
}

// GetInfo extracts some non-standardized items from the result of a Stat call.
func GetInfo(fi os.FileInfo) *FileInfo {
	sys := fi.Sys()
	switch v := sys.(type) {
	case FileInfo:
		return &v
	case *FileInfo:
		return v
	default:
		return getOSFileInfo(fi)
	}
}
