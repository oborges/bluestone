package server

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

var errEOF = io.EOF

// osErrToStatus maps a backend error to the status a client acts on. Getting
// this right matters to the person using the client: Windows shows a full
// disk, a read-only share and a timeout differently, and reporting all of
// them as "access denied" sends them looking at permissions.
func osErrToStatus(err error) uint32 {
	switch {
	case err == nil:
		return wire.StatusSuccess

	// Specific errnos come first: Go treats some of them as the general
	// sentinels below (ENOTEMPTY reads as fs.ErrExist, for one), so testing
	// those first would lose the detail the client acts on.
	case errors.Is(err, syscall.ENOSPC):
		return wire.StatusDiskFull
	case errors.Is(err, syscall.EROFS):
		return wire.StatusMediaWriteProtected
	case errors.Is(err, syscall.ENOTEMPTY):
		return wire.StatusDirectoryNotEmpty
	case errors.Is(err, syscall.EISDIR):
		return wire.StatusFileIsADirectory
	case errors.Is(err, syscall.ENOTDIR):
		return wire.StatusNotADirectory
	case errors.Is(err, syscall.ENAMETOOLONG):
		return wire.StatusObjectNameInvalid

	// The file is in use, which a client retries rather than reporting.
	case errors.Is(err, syscall.EBUSY), errors.Is(err, syscall.ETXTBSY):
		return wire.StatusSharingViolation

	// The server is out of capacity, which a client backs off from.
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE),
		errors.Is(err, syscall.ENOMEM):
		return wire.StatusInsufficientResources

	// Took too long, or was given up on.
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, syscall.ETIMEDOUT):
		return wire.StatusIOTimeout
	case errors.Is(err, context.Canceled):
		return wire.StatusCancelled
	case errors.Is(err, syscall.EIO):
		return wire.StatusUnexpectedIOError

	case errors.Is(err, fs.ErrNotExist):
		return wire.StatusObjectNameNotFound
	case errors.Is(err, fs.ErrExist):
		return wire.StatusObjectNameCollision
	case errors.Is(err, fs.ErrPermission):
		return wire.StatusAccessDenied
	case errors.Is(err, os.ErrClosed):
		return wire.StatusInvalidHandle
	case errors.Is(err, io.EOF):
		return wire.StatusEndOfFile
	case errors.Is(err, vfs.ErrSharingViolation):
		return wire.StatusSharingViolation
	case errors.Is(err, fs.ErrInvalid):
		return wire.StatusInvalidParameter
	case errors.Is(err, errors.ErrUnsupported):
		return wire.StatusNotSupported
	}

	// A backend that failed for a reason of its own: report an I/O error
	// rather than blaming the client's permissions.
	return wire.StatusUnexpectedIOError
}

// makeFileID builds an SMB2_FILEID (MS-SMB2 section 2.2.14.1): a persistent
// half unique to the open across the server, which is what a client names
// to reclaim a durable open after reconnecting, and a volatile half from
// the tree and its open counter.
func makeFileID(openID uint64, treeID uint32, counter uint64) [16]byte {
	var fid [16]byte
	binary.LittleEndian.PutUint64(fid[0:8], openID)
	binary.LittleEndian.PutUint32(fid[8:12], treeID)
	binary.LittleEndian.PutUint32(fid[12:16], uint32(counter))
	return fid
}

// persistentID is the persistent half of a file id.
func persistentID(fid [16]byte) uint64 {
	return binary.LittleEndian.Uint64(fid[0:8])
}
