package server

import (
	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// ShareAccess is what a user may do on a share.
type ShareAccess int

const (
	// ShareDenied refuses the user's TREE_CONNECT with ACCESS_DENIED.
	ShareDenied ShareAccess = iota
	// ShareReadOnly lets the user read but change nothing.
	ShareReadOnly
	// ShareReadWrite gives the user full access.
	ShareReadWrite
)

// WithShareAccess decides, when a session connects to a share, what its
// user may do there. Without it every authenticated user has full access
// to every share. IPC$ is asked about too.
func WithShareAccess(fn func(id *auth.Identity, share string) ShareAccess) Option {
	return func(s *Server) { s.shareAccess = fn }
}

// Access mask bits (MS-SMB2 section 2.2.13.1).
const (
	accessWriteData       uint32 = 0x00000002
	accessAppendData      uint32 = 0x00000004
	accessWriteEA         uint32 = 0x00000010
	accessDeleteChild     uint32 = 0x00000040
	accessWriteAttributes uint32 = 0x00000100
	accessWriteDAC        uint32 = 0x00040000
	accessWriteOwner      uint32 = 0x00080000
	accessGenericWrite    uint32 = 0x40000000

	// accessChanges are the rights that change a file or its metadata.
	accessChanges = accessWriteData | accessAppendData | accessWriteEA | accessDeleteChild |
		accessWriteAttributes | accessDelete | accessWriteDAC | accessWriteOwner |
		accessGenericWrite | accessGenericAll

	// maximalAccessFull is every right on a file, and maximalAccessRead
	// the rights a read-only share grants: FILE_GENERIC_READ and
	// FILE_GENERIC_EXECUTE.
	maximalAccessFull uint32 = 0x001F01FF
	maximalAccessRead uint32 = 0x001200A9
)

// treeAccess asks the share access hook what the session may do on share.
func (s *Server) treeAccess(sess *session, share string) ShareAccess {
	if s.shareAccess == nil {
		return ShareReadWrite
	}
	var id *auth.Identity
	if sess != nil {
		id = sess.identity
	}
	return s.shareAccess(id, share)
}

// readOnlyCreate checks a CREATE on a read-only share. It returns
// ACCESS_DENIED for one that asks to change anything, which is what a
// Windows server answers for a share its user may only read. Opening a
// file that exists, for reading, is all that is allowed; OPEN_IF is left to
// the caller, which can only tell once it knows whether the file exists.
func readOnlyCreate(req *wire.CreateRequest, deleteOnClose bool) uint32 {
	if req.DesiredAccess&accessChanges != 0 || deleteOnClose {
		return wire.StatusAccessDenied
	}
	switch req.CreateDisposition {
	case vfs.DispositionOpen, vfs.DispositionOpenIf:
		return wire.StatusSuccess
	}
	return wire.StatusAccessDenied
}
