package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
	"github.com/sonroyaalmerol/go-smb-server/smb/encryption"
	"github.com/sonroyaalmerol/go-smb-server/smb/signing"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

func (c *request) dispatch(ctx context.Context, msg []byte, hdr *wire.Header, lastFileId *[16]byte, related bool) uint32 {
	var sess *session
	var tr *tree
	if requiresSession(hdr.Command) {
		sess = c.getSession(hdr.SessionId)
		if sess == nil || !sess.authenticated {
			if hdr.Command == wire.CmdSessionSetup {
			} else {
				return c.errBody(wire.StatusUserSessionDeleted)
			}
		}
		if sess != nil {
			tr = sess.getTree(hdr.TreeId)
			if requiresTree(hdr.Command) && tr == nil {
				return c.errBody(wire.StatusNetworkNameDeleted)
			}
		}
	}

	switch hdr.Command {
	case wire.CmdNegotiate:
		return c.handleNegotiate(msg, hdr)
	case wire.CmdSessionSetup:
		return c.handleSessionSetup(ctx, msg, hdr)
	case wire.CmdLogoff:
		return c.handleLogoff(ctx, hdr, sess)
	case wire.CmdTreeConnect:
		return c.handleTreeConnect(msg, hdr, sess)
	case wire.CmdTreeDisconnect:
		return c.handleTreeDisconnect(ctx, hdr, sess, tr)
	case wire.CmdCreate:
		return c.handleCreate(ctx, msg, hdr, sess, tr, lastFileId)
	case wire.CmdClose:
		return c.handleClose(ctx, msg, tr)
	case wire.CmdRead:
		return c.handleRead(ctx, msg, tr)
	case wire.CmdWrite:
		if tr.readOnly {
			return c.errBody(wire.StatusAccessDenied)
		}
		return c.handleWrite(ctx, msg, tr)
	case wire.CmdQueryDirectory:
		return c.handleQueryDirectory(ctx, msg, tr)
	case wire.CmdQueryInfo:
		return c.handleQueryInfo(ctx, msg, tr)
	case wire.CmdSetInfo:
		if tr.readOnly {
			return c.errBody(wire.StatusAccessDenied)
		}
		return c.handleSetInfo(ctx, msg, tr)
	case wire.CmdFlush:
		return c.handleFlush(ctx, msg, tr)
	case wire.CmdLock:
		return c.handleLock(ctx, msg, hdr, tr)
	case wire.CmdIoctl:
		return c.handleIoctl(ctx, msg, tr)
	case wire.CmdEcho:
		return c.handleEcho(hdr)
	case wire.CmdOplockBreak:
		return c.handleOplockBreak(msg)
	default:
		return c.errBody(wire.StatusNotImplemented)
	}
}

func requiresSession(cmd uint16) bool {
	switch cmd {
	case wire.CmdNegotiate:
		return false
	default:
		return true
	}
}

func requiresTree(cmd uint16) bool {
	switch cmd {
	case wire.CmdTreeConnect, wire.CmdSessionSetup, wire.CmdLogoff, wire.CmdEcho, wire.CmdNegotiate:
		return false
	default:
		return true
	}
}

func (c *request) errBody(status uint32) uint32 {
	var e wire.ErrorResponse
	c.out = e.Append(c.out)
	return status
}

func (c *request) handleNegotiate(msg []byte, hdr *wire.Header) uint32 {
	var req wire.NegotiateRequest
	if err := req.Parse(msg[wire.HeaderSize:]); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	dialect := pickDialect(req.Dialects, c.srv.dialect)
	if dialect == 0 {
		return c.errBody(wire.StatusNotSupported)
	}

	caps := c.negotiateCapabilities(dialect)
	c.clientGUID = req.ClientGuid
	c.negDialect = dialect
	c.negCaps = caps

	resp := wire.NegotiateResponse{
		SecurityMode:    wire.SigningEnabled,
		DialectRevision: dialect,
		ServerGuid:      c.srv.guid,
		Capabilities:    caps,
		MaxTransactSize: c.srv.maxTransact,
		MaxReadSize:     c.srv.maxRead,
		MaxWriteSize:    c.srv.maxWrite,
		SecurityBuffer:  c.srv.negotiateHintToken(),
	}
	if dialect == wire.DialectSMB311 {
		contexts, status := c.negotiate311(&req, msg)
		if status != wire.StatusSuccess {
			return c.errBody(status)
		}
		resp.Contexts = contexts
	}
	c.out = resp.Append(c.out)
	return wire.StatusSuccess
}

// negotiate311 answers an SMB 3.1.1 NEGOTIATE's contexts (MS-SMB2 section
// 3.3.5.4): SHA-512 pre-authentication integrity, which it starts the
// connection's hash with, the first cipher in the client's list the server
// supports, and AES-CMAC signing.
func (c *request) negotiate311(req *wire.NegotiateRequest, msg []byte) ([]wire.NegotiateContext, uint32) {
	data, ok := req.Context(wire.CtxPreauthIntegrity)
	if !ok {
		return nil, wire.StatusInvalidParameter
	}
	hashes, err := wire.ParsePreauthCapabilities(data)
	if err != nil || !slices.Contains(hashes, wire.HashSHA512) {
		return nil, statusSMBNoPreauthIntegrityHashOverlap
	}
	salt := make([]byte, wire.PreauthSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return nil, wire.StatusInsufficientResources
	}
	// The hash covers the NEGOTIATE request as received; the response
	// joins it once it is built.
	c.preauthHash = preauthUpdate(wire.ZeroPreauthHash(), msg)
	contexts := []wire.NegotiateContext{wire.PreauthContext(salt)}

	c.cipher = 0
	if data, ok := req.Context(wire.CtxEncryption); ok {
		if ciphers, err := wire.ParseAlgorithmList(data); err == nil {
			for _, id := range ciphers {
				if encryption.Supported(id) {
					c.cipher = id
					break
				}
			}
		}
		contexts = append(contexts, wire.SingleAlgorithmContext(wire.CtxEncryption, c.cipher))
	}
	if data, ok := req.Context(wire.CtxSigning); ok {
		if algs, err := wire.ParseAlgorithmList(data); err == nil && slices.Contains(algs, wire.SigningAESCMAC) {
			contexts = append(contexts, wire.SingleAlgorithmContext(wire.CtxSigning, wire.SigningAESCMAC))
		}
	}
	c.log.Debug("negotiated SMB 3.1.1", "cipher", encryption.CipherName(c.cipher))
	return contexts, wire.StatusSuccess
}

// statusSMBNoPreauthIntegrityHashOverlap: the client offers no hash the
// server supports.
const statusSMBNoPreauthIntegrityHashOverlap uint32 = 0xC05D0000

func pickDialect(offered []uint16, maxDialect uint16) uint16 {
	var best uint16
	for _, d := range offered {
		if d > maxDialect {
			continue
		}
		if d == wire.DialectSMB202 || d == wire.DialectSMB21 ||
			d == wire.DialectSMB30 || d == wire.DialectSMB302 || d == wire.DialectSMB311 {
			if d > best {
				best = d
			}
		}
	}
	return best
}

func (c *request) handleSessionSetup(ctx context.Context, msg []byte, hdr *wire.Header) uint32 {
	var req wire.SessionSetupRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	remote := c.remoteAddr()
	if !c.srv.gate().Allow(remote) {
		// Answered as a wrong password, so a caller cannot tell a blocked
		// client from a bad credential.
		return c.errBody(wire.StatusLogonFailure)
	}

	sess := c.getSession(hdr.SessionId)
	if sess == nil {
		if max := c.srv.limits.SessionsPerConnection; max > 0 && c.sessionCount() >= max {
			c.log.Debug("session limit reached", "remote", remote, "limit", max)
			return c.errBody(wire.StatusInsufficientResources)
		}
		sess = &session{
			auth:  c.srv.authFactory(),
			trees: make(map[uint32]*tree),
		}
		// Unique across the server, not only this connection (MS-SMB2
		// section 3.3.5.5.1): file ids and lock owners are built from it,
		// and with every connection counting from 1 two clients' opens got
		// the same ids, so their byte-range locks did not conflict.
		sessID := c.srv.nextSessionID.Add(1)
		c.putSession(sessID, sess)
		hdr.SessionId = sessID
	}

	if c.negDialect == wire.DialectSMB311 {
		// The session's pre-authentication hash starts from the
		// connection's and takes in each of its SESSION_SETUP requests.
		if sess.preauth == nil {
			sess.preauth = append([]byte(nil), c.preauthHash...)
		}
		sess.preauth = preauthUpdate(sess.preauth, msg)
	}

	started := time.Now()
	result, err := sess.auth.Accept(ctx, req.SecurityBuffer)
	if err != nil {
		c.srv.gate().Attempted(remote, false, time.Since(started))
		if errors.Is(err, auth.ErrLogonFailed) {
			return c.errBody(wire.StatusLogonFailure)
		}
		return c.errBody(wire.StatusAccessDenied)
	}

	ssr := wire.SessionSetupResponse{
		SecurityBuffer: result.OutputToken,
	}
	if c.srv.requireEnc {
		ssr.SessionFlags |= wire.SessionFlagEncryptData
	}
	if result.Identity != nil {
		if status := c.establishKeys(sess, result.SessionKey); status != wire.StatusSuccess {
			c.dropSession(hdr.SessionId)
			return c.errBody(status)
		}
		sess.identity = result.Identity
		sess.authenticated = true
		c.log.Debug("session established", "user", result.Identity.Username, "domain", result.Identity.Domain,
			"mechanism", result.Identity.Mechanism, "sid", result.Identity.SID, "groups", len(result.Identity.Groups))
	}
	c.out = ssr.Append(c.out)
	if sess.authenticated {
		if sess.counted.CompareAndSwap(false, true) {
			c.srv.gate().Attempted(remote, true, time.Since(started))
			c.srv.obs().SessionOpened()
		}
		return wire.StatusSuccess
	}
	return wire.StatusMoreProcessingRequired
}

func (c *request) handleLogoff(ctx context.Context, hdr *wire.Header, sess *session) uint32 {
	if sess != nil {
		for _, t := range sess.allTrees() {
			c.closeAllOpens(ctx, t, false)
		}
		c.dropSession(hdr.SessionId)
	}
	var r wire.LogoffResponse
	c.out = r.Append(c.out)
	return wire.StatusSuccess
}

func (c *request) handleTreeConnect(msg []byte, hdr *wire.Header, sess *session) uint32 {
	var req wire.TreeConnectRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	shareName := parseShareName(wire.UTF16FromBytes(req.Path))
	sh, ok := c.srv.shareNamed(shareName)
	if !ok {
		return c.errBody(wire.StatusBadNetworkName)
	}

	if max := c.srv.limits.TreesPerSession; max > 0 && sess.treeCount() >= max {
		return c.errBody(wire.StatusInsufficientResources)
	}
	access := c.srv.treeAccess(sess, sh.Name())
	if access == ShareDenied {
		c.log.Debug("share access denied", "share", sh.Name(), "user", sessionUser(sess))
		return c.errBody(wire.StatusAccessDenied)
	}
	readOnly := access == ShareReadOnly

	treeID := sess.nextTreeID
	sess.nextTreeID++
	sess.addTree(treeID, &tree{
		share:    sh,
		readOnly: readOnly,
		opens:    make(map[[16]byte]*openHandle),
	})
	hdr.TreeId = treeID

	resp := wire.TreeConnectResponse{
		ShareType:     wire.ShareTypeDisk,
		ShareFlags:    0x00000030,
		Capabilities:  0,
		MaximalAccess: maximalAccessFull,
	}
	if readOnly {
		// Windows shows the share as read-only from this, and does not
		// offer to change what it cannot.
		resp.MaximalAccess = maximalAccessRead
	}
	c.out = resp.Append(c.out)
	return wire.StatusSuccess
}

// shareNamed finds a share by name. Share names are case-insensitive, as on
// Windows: macOS clients send them upper-cased.
func (s *Server) shareNamed(name string) (vfs.Share, bool) {
	if sh, ok := s.shareByName[name]; ok {
		return sh, true
	}
	for n, sh := range s.shareByName {
		if strings.EqualFold(n, name) {
			return sh, true
		}
	}
	return nil, false
}

func parseShareName(unc string) string {
	s := strings.TrimLeft(unc, `\`)
	if i := strings.IndexByte(s, '\\'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '\\'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (c *request) handleTreeDisconnect(ctx context.Context, hdr *wire.Header, sess *session, tr *tree) uint32 {
	if tr != nil {
		c.closeAllOpens(ctx, tr, false)
		sess.dropTree(hdr.TreeId)
	}
	var r wire.TreeDisconnectResponse
	c.out = r.Append(c.out)
	return wire.StatusSuccess
}

// closeAllOpens closes a tree's opens. When the connection was lost rather
// than closed, durable opens are kept for their client to reconnect to
// instead, with their locks, lease and share modes.
func (c *conn) closeAllOpens(ctx context.Context, tr *tree, connectionLost bool) {
	for _, oh := range tr.allOpens() {
		if connectionLost && oh.durable != nil && !c.srv.stopping.Load() {
			c.srv.resumeKeyTable().release(oh)
			c.srv.endWatch(oh)
			c.srv.preserve(tr.share, oh)
			continue
		}
		c.srv.lockerFor(tr.share).ReleaseOwner(lockOwner(oh.sessionID, oh.fileId))
		c.srv.resumeKeyTable().release(oh)
		c.srv.leaseTable().release(oh)
		c.srv.endWatch(oh)
		_ = oh.h.Close(ctx)
		// A client that disconnects still gets its delete-on-close files
		// deleted, as when a process holding a temporary file exits.
		if err := c.releaseOpen(ctx, tr, oh); err != nil {
			c.log.Debug("delete on disconnect failed", "path", oh.currentPath(), "err", err)
		}
	}
	tr.mu.Lock()
	tr.opens = make(map[[16]byte]*openHandle)
	tr.mu.Unlock()
}

// releaseOpen forgets a closed handle and, when it was the file's last open
// and the file is delete-pending, deletes the file under its current name.
func (c *conn) releaseOpen(ctx context.Context, tr *tree, oh *openHandle) error {
	return c.srv.releaseOpen(ctx, tr.share, oh)
}

// releaseOpen is releaseOpen for an open no longer on any tree, as a
// durable open whose client never came back.
func (s *Server) releaseOpen(ctx context.Context, share vfs.Share, oh *openHandle) error {
	remove, path := s.fileTable().release(oh)
	if !remove {
		return nil
	}
	rm, ok := share.Backend().(vfs.Remover)
	if !ok {
		return nil
	}
	if oh.isDir {
		// A directory is only deleted once it is empty. One opened
		// delete-on-close while it had entries stays, and its handle
		// closes without error, as on Windows.
		if nonEmpty, err := dirNonEmpty(ctx, share, path); err != nil || nonEmpty {
			s.log.Debug("not deleting a non-empty directory", "path", path, "err", err)
			return nil
		}
	}
	if err := rm.Remove(ctx, path); err != nil {
		return err
	}
	if !oh.stream {
		s.selfNotifyShare(share, vfs.Change{Action: vfs.ChangeRemoved, Path: path, IsDir: oh.isDir})
	}
	return nil
}

// dirNonEmpty reports whether the directory at path lists anything.
func dirNonEmpty(ctx context.Context, share vfs.Share, path string) (bool, error) {
	h, err := share.Backend().Open(ctx, vfs.OpenOptions{Path: path, Disposition: vfs.DispositionOpen, CreateDir: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = h.Close(ctx) }()
	return dirHasEntries(ctx, h)
}

func (c *request) handleCreate(ctx context.Context, msg []byte, hdr *wire.Header, sess *session, tr *tree, lastFileId *[16]byte) uint32 {
	var req wire.CreateRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	if max := c.srv.limits.OpensPerSession; max > 0 && sess != nil && sess.openCount() >= max {
		c.log.Debug("open limit reached", "remote", c.remoteAddr(), "limit", max)
		return c.errBody(wire.StatusInsufficientResources)
	}
	if rc, ok := durableReconnect(&req); ok {
		return c.reconnectDurable(ctx, &req, rc, sess, tr, lastFileId)
	}
	name := wire.UTF16FromBytes(req.Name)
	base, stream, err := splitStream(name)
	if err != nil {
		return c.errBody(wire.StatusObjectNameInvalid)
	}
	backend := tr.share.Backend()
	streams, hasStreams := backend.(vfs.StreamOpener)
	if stream != "" && !hasStreams {
		// Passing "file:stream" through would create a file of that name.
		return c.errBody(wire.StatusObjectNameInvalid)
	}
	// "file::$DATA" is the file itself.
	name = streamPath(base, stream)
	deleteOnClose := req.CreateOptions&wire.FileDeleteOnClose != 0
	if deleteOnClose && req.DesiredAccess&(accessDelete|accessGenericAll|accessMaximumAllowed) == 0 {
		// Deleting on close is deleting, which the open has to ask for
		// (MS-SMB2 section 3.3.5.9).
		return c.errBody(wire.StatusAccessDenied)
	}
	// On a read-only share, only opens that change nothing get through.
	// OPEN_IF becomes OPEN, since creating the file would change the
	// share: a file that is not there is refused rather than created.
	readOnlyOpenIf := false
	if tr.readOnly {
		if status := readOnlyCreate(&req, deleteOnClose); status != wire.StatusSuccess {
			return c.errBody(status)
		}
		if req.CreateDisposition == vfs.DispositionOpenIf {
			req.CreateDisposition, readOnlyOpenIf = vfs.DispositionOpen, true
		}
	}
	key := keyFor(tr.share.Name(), name)
	files := c.srv.fileTable()
	if files.deletePending(key) || (stream != "" && files.deletePending(keyFor(tr.share.Name(), base))) {
		// The file is waiting for its last handle to close before it goes;
		// Windows refuses every open of it meanwhile, including ones that
		// would overwrite it. Checked before the backend opens it, so an
		// overwrite does not truncate a file on its way out.
		return c.errBody(wire.StatusDeletePending)
	}
	// The lease the request asks for, if any: its key may not already name
	// another file.
	var asked leaseID
	askedLease := false
	if c.srv.leasesEnabled && req.RequestedOplockLevel == wire.OplockLevelLease {
		if data, ok := req.Context(wire.CreateContextLease); ok {
			if l, err := wire.ParseLeaseRequest(data); err == nil {
				asked, askedLease = leaseID{client: c.clientGUID, key: l.Key}, true
				if c.srv.leaseTable().leaseConflict(asked, key) {
					return c.errBody(wire.StatusInvalidParameter)
				}
			}
		}
	}
	opts := vfs.OpenOptions{
		Path:          base,
		Disposition:   req.CreateDisposition,
		CreateDir:     req.CreateOptions&wire.FileDirectoryFile != 0,
		DesiredAccess: req.DesiredAccess,
		ShareAccess:   req.ShareAccess,
		DeleteOnClose: deleteOnClose,
		User:          sessionIdentity(sess),
	}
	// A backend that does not report changes has them reported for it,
	// which needs to know whether this open creates the file.
	selfNotify := stream == "" && c.srv.wantsSelfNotify(tr)
	existed := false
	if selfNotify {
		if probe, err := backend.Open(ctx, vfs.OpenOptions{Path: base, Disposition: vfs.DispositionOpen}); err == nil {
			existed = true
			_ = probe.Close(ctx)
		}
	}
	if tr.readOnly && opts.CreateDir {
		// Not every backend refuses to create a directory it is asked to
		// open, so a read-only share checks that it exists first.
		probe, err := backend.Open(ctx, vfs.OpenOptions{Path: base, Disposition: vfs.DispositionOpen, User: opts.User})
		if err != nil {
			status := osErrToStatus(err)
			if readOnlyOpenIf && status == wire.StatusObjectNameNotFound {
				status = wire.StatusAccessDenied
			}
			return c.errBody(status)
		}
		_ = probe.Close(ctx)
	}
	var h vfs.Handle
	if stream != "" {
		h, err = streams.OpenStream(ctx, opts, stream)
	} else {
		h, err = backend.Open(ctx, opts)
	}
	if errors.Is(err, vfs.ErrSharingViolation) && c.srv.leasesEnabled && !c.retryingCreate && !c.compound {
		// Another client may only be holding the file open because it
		// caches the handle. Told to stop, it closes it, and the open
		// may then succeed: wait for that before answering.
		except := asked
		if !askedLease {
			except = leaseID{}
		}
		if waits := c.srv.leaseTable().breakHandle(key, except); len(waits) > 0 {
			return c.createAfterBreaks(ctx, msg, hdr, sess, tr, waits)
		}
	}
	if err != nil {
		status := osErrToStatus(err)
		if readOnlyOpenIf && status == wire.StatusObjectNameNotFound {
			status = wire.StatusAccessDenied
		}
		return c.errBody(status)
	}
	fi, err := h.Stat(ctx)
	if err != nil {
		_ = h.Close(ctx)
		return c.errBody(osErrToStatus(err))
	}
	if deleteOnClose && !fi.IsDir && fi.Attributes&attrReadOnly != 0 {
		// A read-only file cannot be deleted until the attribute is
		// cleared (MS-FSA section 2.1.5.1.2.1).
		_ = h.Close(ctx)
		return c.errBody(wire.StatusCannotDelete)
	}

	fid := makeFileID(c.srv.nextOpenID.Add(1), hdr.TreeId, tr.nextFileID())
	c.log.Debug("create", "path", name, "disposition", req.CreateDisposition,
		"desired_access", req.DesiredAccess, "options", req.CreateOptions,
		"oplock", req.RequestedOplockLevel, "contexts", contextNames(&req))
	oh := &openHandle{h: h, fileId: fid, sessionID: hdr.SessionId, path: name,
		access: req.DesiredAccess, deleteOnClose: deleteOnClose, isDir: fi.IsDir, stream: stream != ""}
	files.add(key, oh)
	tr.addOpen(oh)
	switch req.CreateDisposition {
	case wire.FileOverwrite, wire.FileOverwriteIf, wire.FileSupersede:
		// Replacing the file's data: what others cached is stale.
		c.srv.dataChanged(oh)
	}
	if selfNotify {
		switch {
		case !existed:
			c.srv.selfNotify(tr, vfs.Change{Action: vfs.ChangeAdded, Path: name, IsDir: fi.IsDir})
		case req.CreateDisposition == wire.FileOverwrite || req.CreateDisposition == wire.FileOverwriteIf || req.CreateDisposition == wire.FileSupersede:
			c.srv.selfNotify(tr, vfs.Change{Action: vfs.ChangeModified, Path: name,
				Filter: FileNotifyChangeSize | FileNotifyChangeLastWrite})
		}
	}
	*lastFileId = fid

	// CreateAction tells the client what the open actually did (MS-SMB2
	// section 2.2.14).
	action := wire.FileOpened
	switch req.CreateDisposition {
	case wire.FileCreate:
		action = wire.FileCreated
	case wire.FileSupersede:
		action = wire.FileSuperseded
	case wire.FileOverwrite:
		action = wire.FileOverwritten
	case wire.FileOverwriteIf:
		action = wire.FileOverwritten
	}

	// Read caching at most: a client never caches writes, which upstream
	// let it do with exclusive and batch oplocks, and which made Windows
	// PowerShell's Set-Content flush stale cached content and silently
	// append to files instead of replacing them.
	oplock, contexts, leaseState := c.grantCaching(&req, sess, tr, oh, fi)
	contexts = append(contexts, c.grantDurable(&req, sess, tr, oh, leaseState)...)

	resp := wire.CreateResponse{
		Contexts:       contexts,
		OplockLevel:    oplock,
		CreateAction:   action,
		CreationTime:   wire.TimeToFiletime(fi.CreationTime),
		LastAccessTime: wire.TimeToFiletime(fi.LastAccess),
		LastWriteTime:  wire.TimeToFiletime(fi.LastWrite),
		ChangeTime:     wire.TimeToFiletime(fi.ChangeTime),
		AllocationSize: uint64(fi.Size),
		EndOfFile:      uint64(fi.Size),
		FileAttributes: toFileAttributes(fi),
		FileId:         fid,
	}
	c.out = resp.Append(c.out)
	return wire.StatusSuccess
}

// Access bits an open asks for that decide whether it may delete (MS-SMB2
// section 2.2.13.1).
const (
	accessDelete         uint32 = 0x00010000
	accessMaximumAllowed uint32 = 0x02000000
	accessGenericAll     uint32 = 0x10000000
)

// DOS attribute bits (MS-FSCC section 2.6).
const (
	attrReadOnly  uint32 = 0x00000001
	attrHidden    uint32 = 0x00000002
	attrSystem    uint32 = 0x00000004
	attrDirectory uint32 = 0x00000010
	attrArchive   uint32 = 0x00000020
	attrNormal    uint32 = 0x00000080
)

// toFileAttributes reports the backend's attributes, with the directory bit
// forced to match the entry itself. A file with no stored attributes is
// reported as FILE_ATTRIBUTE_ARCHIVE, which is what Windows servers and
// Samba report for an ordinary file.
func toFileAttributes(fi vfs.FileInfo) uint32 {
	attrs := fi.Attributes & (attrReadOnly | attrHidden | attrSystem | attrArchive)
	if fi.IsDir {
		return attrs | attrDirectory
	}
	if attrs == 0 {
		return attrArchive
	}
	return attrs
}

func (c *request) handleClose(ctx context.Context, msg []byte, tr *tree) uint32 {
	var req wire.CloseRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	oh, ok := tr.open(req.FileId)
	if !ok {
		return c.errBody(wire.StatusInvalidHandle)
	}
	fi, statErr := oh.h.Stat(ctx)
	if err := oh.h.Close(ctx); err != nil && statErr == nil {
		return c.errBody(osErrToStatus(err))
	}
	tr.removeOpen(req.FileId)
	// Closing an open releases the byte-range locks it holds (MS-SMB2
	// section 3.3.5.10); they used to stay until the session went.
	c.srv.lockerFor(tr.share).ReleaseOwner(lockOwner(oh.sessionID, oh.fileId))
	c.srv.leaseTable().release(oh)
	c.srv.resumeKeyTable().release(oh)
	c.srv.endWatch(oh)

	if rmErr := c.releaseOpen(ctx, tr, oh); rmErr != nil {
		return c.errBody(osErrToStatus(rmErr))
	}

	resp := wire.CloseResponse{Flags: req.Flags & wire.CloseFlagPostQueryAttrib}
	if statErr == nil {
		resp.CreationTime = wire.TimeToFiletime(fi.CreationTime)
		resp.LastAccessTime = wire.TimeToFiletime(fi.LastAccess)
		resp.LastWriteTime = wire.TimeToFiletime(fi.LastWrite)
		resp.ChangeTime = wire.TimeToFiletime(fi.ChangeTime)
		resp.AllocationSize = uint64(fi.Size)
		resp.EndOfFile = uint64(fi.Size)
		resp.FileAttributes = toFileAttributes(fi)
	}
	c.out = resp.Append(c.out)
	return wire.StatusSuccess
}

func (c *request) handleRead(ctx context.Context, msg []byte, tr *tree) uint32 {
	var req wire.ReadRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	oh, ok := tr.open(req.FileId)
	if !ok {
		return c.errBody(wire.StatusInvalidHandle)
	}
	respStart := len(c.out)
	c.out = wire.ReadResponseAlloc(c.out, int(req.Length))
	n, err := oh.h.Read(ctx, int64(req.Offset), wire.ReadResponseData(c.out, respStart))
	if err != nil && !errors.Is(err, errEOF) {
		c.out = c.out[:respStart]
		return c.errBody(osErrToStatus(err))
	}
	if n == 0 {
		c.out = c.out[:respStart]
		return c.errBody(wire.StatusEndOfFile)
	}
	c.out = wire.ReadResponseSetCount(c.out, respStart, n)
	return wire.StatusSuccess
}

func (c *request) handleWrite(ctx context.Context, msg []byte, tr *tree) uint32 {
	var req wire.WriteRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	oh, ok := tr.open(req.FileId)
	if !ok {
		return c.errBody(wire.StatusInvalidHandle)
	}
	c.log.Debug("write", "path", oh.currentPath(), "offset", req.Offset, "len", len(req.Data))
	n, err := oh.h.Write(ctx, int64(req.Offset), req.Data)
	if err != nil {
		return c.errBody(osErrToStatus(err))
	}
	if n > 0 {
		c.srv.dataChanged(oh)
	}
	if n > 0 && !oh.stream && !oh.wrote.Swap(true) {
		// Reported once per handle: a client watching does not need a
		// notification for every piece of a file being written.
		c.srv.selfNotify(tr, vfs.Change{Action: vfs.ChangeModified, Path: oh.currentPath(),
			Filter: FileNotifyChangeSize | FileNotifyChangeLastWrite})
	}
	c.out = wire.WriteResponseAppend(c.out, uint32(n))
	return wire.StatusSuccess
}

func (c *request) handleQueryDirectory(ctx context.Context, msg []byte, tr *tree) uint32 {
	var req wire.QueryDirectoryRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	oh, ok := tr.open(req.FileId)
	if !ok {
		return c.errBody(wire.StatusInvalidHandle)
	}

	oh.enumMu.Lock()
	defer oh.enumMu.Unlock()

	if req.Flags&(wire.QueryDirRestartScans|wire.QueryDirReopen) != 0 {
		oh.enumDone = false
		oh.enumStarted = false
	}
	if oh.enumDone {
		return c.errBody(wire.StatusNoMoreFiles)
	}

	pattern := wire.UTF16FromBytes(req.FileName)
	if pattern == "" {
		pattern = "*"
	}

	// Capture the listing on the first call, then serve it in batches: a
	// client that asked for a single entry comes back for the rest.
	if !oh.enumStarted {
		oh.enumEntries = oh.enumEntries[:0]
		oh.enumIndex = 0
		for fi, err := range oh.h.Enumerate(ctx, pattern) {
			if err != nil {
				return c.errBody(osErrToStatus(err))
			}
			oh.enumEntries = append(oh.enumEntries, fi)
		}
		oh.enumStarted = true
		if len(oh.enumEntries) == 0 {
			// Nothing matched at all, which is a different answer from having
			// returned every match (MS-FSA 2.1.5.6.3). macOS looks names up
			// this way and expects STATUS_NO_SUCH_FILE for a missing one.
			oh.enumDone = true
			return c.errBody(wire.StatusNoSuchFile)
		}
	}
	if oh.enumIndex >= len(oh.enumEntries) {
		oh.enumDone = true
		return c.errBody(wire.StatusNoMoreFiles)
	}

	// Encode the class the client asked for: each has a different fixed part
	// before the name, so answering in the wrong one gives clients garbled
	// names ("directory entry name would overflow", on Linux).
	infoClass := req.FileInformationClass
	entryMinSize, ok := wire.DirInfoFixedSize(infoClass)
	if !ok {
		return c.errBody(wire.StatusInvalidInfoClass)
	}

	const bodyFixed = 8
	bodyStart := len(c.out)
	c.out = append(c.out, make([]byte, bodyFixed)...)
	bufStart := len(c.out)

	var prevEntryStart = -1
	empty := true
	for ; oh.enumIndex < len(oh.enumEntries); oh.enumIndex++ {
		fi := oh.enumEntries[oh.enumIndex]
		if len(c.out)-bufStart+entryMinSize+len(fi.Name)*2 > int(req.OutputBufferLength) {
			if empty {
				// Not even one entry fits in the buffer the client offered.
				c.out = c.out[:bodyStart]
				return c.errBody(wire.StatusInfoLengthMismatch)
			}
			break
		}
		encFi := wire.FileInfo{
			Name:   fi.Name,
			FileId: pathIndexNumber(oh.currentPath() + "/" + fi.Name),
			// (pathIndexNumber normalizes separators, so this matches the
			// index a QUERY_INFO on the same file reports.)
			EndOfFile:      uint64(fi.Size),
			AllocationSize: uint64(fi.Size),
			FileAttributes: toFileAttributes(fi),
			CreationTime:   wire.TimeToFiletime(fi.CreationTime),
			LastAccessTime: wire.TimeToFiletime(fi.LastAccess),
			LastWriteTime:  wire.TimeToFiletime(fi.LastWrite),
			ChangeTime:     wire.TimeToFiletime(fi.ChangeTime),
		}
		var entryStart int
		c.out, entryStart = wire.AppendDirInfo(c.out, encFi, infoClass)
		if prevEntryStart >= 0 {
			wire.SetNextEntryOffset(c.out, prevEntryStart, entryStart)
		}
		prevEntryStart = entryStart
		empty = false
		if req.Flags&wire.QueryDirReturnSingle != 0 {
			// The client asked for one entry and will come back for more.
			oh.enumIndex++
			break
		}
	}

	if empty {
		c.out = c.out[:bodyStart]
		oh.enumDone = true
		return c.errBody(wire.StatusNoMoreFiles)
	}

	bufLen := len(c.out) - bufStart
	binary.LittleEndian.PutUint16(c.out[bodyStart:bodyStart+2], 9)
	// OutputBufferOffset is relative to this message's header, which is 64
	// bytes before the body, plus the 8-byte fixed part.
	binary.LittleEndian.PutUint16(c.out[bodyStart+2:bodyStart+4], wire.HeaderSize+8)
	binary.LittleEndian.PutUint32(c.out[bodyStart+4:bodyStart+8], uint32(bufLen))
	return wire.StatusSuccess
}

func (c *request) handleEcho(hdr *wire.Header) uint32 {
	c.out = append(c.out, 0x04, 0x00, 0x00, 0x00)
	return wire.StatusSuccess
}

// createAfterBreaks answers a CREATE that met a sharing violation while
// other clients cache handles to the file: it goes async, and once they have
// acknowledged the break (or not, in time) it is tried again, succeeding if
// the clients closed the handles they were caching (MS-SMB2 section
// 3.3.5.9, the oplock break wait).
func (c *request) createAfterBreaks(ctx context.Context, msg []byte, hdr *wire.Header, sess *session, tr *tree, waits []chan struct{}) uint32 {
	op := c.registerPending(hdr.MessageId)
	c.asyncID = op.asyncID
	retry := append([]byte(nil), msg...)
	reqHdr := *hdr
	timeout := c.srv.leaseTable().timeout + time.Second
	c.after = append(c.after, func() {
		go func() {
			deadline := time.NewTimer(timeout)
			defer deadline.Stop()
			for _, wait := range waits {
				select {
				case <-wait:
				case <-deadline.C:
				case <-op.done:
					c.unregister(op)
					c.sendAsyncFinal(reqHdr, op.asyncID, wire.StatusCancelled, errorBody())
					return
				}
			}
			r := &request{conn: c.conn, retryingCreate: true}
			var lastFileId [16]byte
			status := r.handleCreate(ctx, retry, &reqHdr, sess, tr, &lastFileId)
			c.unregister(op)
			c.sendAsyncFinal(reqHdr, op.asyncID, status, r.out)
		}()
	})
	c.out = append(c.out, errorBody()...)
	return wire.StatusPending
}

// errorBody is an SMB2 ERROR response body with no data, which is also what
// an interim response carries.
func errorBody() []byte {
	return []byte{9, 0, 0, 0, 0, 0, 0, 0, 0}
}

// contextNames lists a request's create contexts, for the debug log.
func contextNames(req *wire.CreateRequest) []string {
	names := make([]string, 0, len(req.Contexts))
	for _, c := range req.Contexts {
		names = append(names, c.Name)
	}
	return names
}

// establishKeys derives an authenticated session's signing and encryption
// keys: from the session key alone for SMB 3.0 and 3.0.2, and from it and the
// session's pre-authentication integrity hash for 3.1.1, with the cipher the
// connection negotiated. A server that requires encryption refuses a client
// that cannot encrypt.
func (c *request) establishKeys(sess *session, sessionKey []byte) uint32 {
	var signKey, outKey, inKey []byte
	cipherID := encryption.CipherAES128CCM
	if c.negDialect == wire.DialectSMB311 {
		cipherID = c.cipher
		keyLen := 16
		if cipherID != 0 {
			keyLen = encryption.KeyLen(cipherID)
		}
		signKey, outKey, inKey = encryption.Keys311(sessionKey, sess.preauth, keyLen)
	} else {
		key := encryption.SessionKey(sessionKey)
		signKey = signing.DeriveSigningKey(key)
		outKey = encryption.DeriveServerEncryptionKey(key)
		inKey = encryption.DeriveServerDecryptionKey(key)
		if c.negCaps&wire.CapEncryption == 0 {
			cipherID = 0
		}
	}
	if c.srv.requireEnc && cipherID == 0 {
		c.log.Debug("refusing a session that cannot encrypt", "dialect", c.negDialect)
		return wire.StatusAccessDenied
	}
	sess.signer, _ = signing.NewSigner(signKey)
	sess.encryptionKey, sess.decryptionKey = outKey, inKey
	if cipherID != 0 {
		enc, err := encryption.NewCipher(cipherID, outKey)
		if err != nil {
			return wire.StatusInternalError
		}
		dec, err := encryption.NewCipher(cipherID, inKey)
		if err != nil {
			return wire.StatusInternalError
		}
		sess.encCipher, sess.decCipher = enc, dec
	}
	sess.requireEncrypt = c.srv.requireEnc
	return wire.StatusSuccess
}
