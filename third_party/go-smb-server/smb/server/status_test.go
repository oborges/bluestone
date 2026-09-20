package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// The status a backend error becomes is what the person at the client sees,
// so a full disk, a read-only share and a timeout must not all arrive as
// "access denied".
func TestOSErrToStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want uint32
	}{
		{"nil", nil, wire.StatusSuccess},
		{"missing", fs.ErrNotExist, wire.StatusObjectNameNotFound},
		{"exists", fs.ErrExist, wire.StatusObjectNameCollision},
		{"permission", fs.ErrPermission, wire.StatusAccessDenied},
		{"closed", os.ErrClosed, wire.StatusInvalidHandle},
		{"end of file", io.EOF, wire.StatusEndOfFile},
		{"sharing violation", vfs.ErrSharingViolation, wire.StatusSharingViolation},
		{"invalid", fs.ErrInvalid, wire.StatusInvalidParameter},
		{"out of space", syscall.ENOSPC, wire.StatusDiskFull},
		{"read-only", syscall.EROFS, wire.StatusMediaWriteProtected},
		{"is a directory", syscall.EISDIR, wire.StatusFileIsADirectory},
		{"not a directory", syscall.ENOTDIR, wire.StatusNotADirectory},
		{"directory not empty", syscall.ENOTEMPTY, wire.StatusDirectoryNotEmpty},
		{"name too long", syscall.ENAMETOOLONG, wire.StatusObjectNameInvalid},
		{"busy", syscall.EBUSY, wire.StatusSharingViolation},
		{"too many files", syscall.EMFILE, wire.StatusInsufficientResources},
		{"out of memory", syscall.ENOMEM, wire.StatusInsufficientResources},
		{"deadline", context.DeadlineExceeded, wire.StatusIOTimeout},
		{"timed out", syscall.ETIMEDOUT, wire.StatusIOTimeout},
		{"cancelled", context.Canceled, wire.StatusCancelled},
		{"device error", syscall.EIO, wire.StatusUnexpectedIOError},
		{"unsupported", errors.ErrUnsupported, wire.StatusNotSupported},
		// A backend failure of its own making is an I/O error, not a
		// permissions problem: "access denied" sends people to check ACLs
		// that have nothing to do with it.
		{"unknown", errors.New("the object store said no"), wire.StatusUnexpectedIOError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := osErrToStatus(tt.err); got != tt.want {
				t.Errorf("osErrToStatus(%v) = %#x, want %#x", tt.err, got, tt.want)
			}
			if tt.err == nil {
				return
			}
			// Backends wrap their errors; the mapping has to see through it.
			wrapped := fmt.Errorf("writing %q: %w", "report.txt", tt.err)
			if got := osErrToStatus(wrapped); got != tt.want {
				t.Errorf("osErrToStatus(wrapped %v) = %#x, want %#x", tt.err, got, tt.want)
			}
		})
	}
}
