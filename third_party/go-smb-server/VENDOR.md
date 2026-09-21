# Vendored go-smb-server

Upstream: https://github.com/sonroyaalmerol/go-smb-server
Commit: d07dc723a2926177b4bc20c806d95cb2c3b1c70a (2026-07-15)
License: MIT (see LICENSE)

Bluestone serves SMB through this library, the way `third_party/go-nfs`
serves NFS. Local changes are listed here so they can be carried forward or
proposed upstream.

## Local changes

- `go.mod`: `go 1.25.0` instead of `go 1.26`, to match Bluestone. The library
  builds and vets unchanged at 1.25.
- `smb/server/server.go`: `ListenAndServe` is split so `Serve(ctx, listener)`
  accepts on a caller-provided listener. Bluestone uses it to enforce
  `allowed_clients` at accept time and to bind test servers to port 0.
- Not vendored: `encryption.test` (a compiled test binary tracked upstream) and
  `examples/`.

- `smb/ntlmssp`: replies mirror the client's token framing. Upstream always
  wrapped the challenge in SPNEGO, so the Linux kernel SMB client (which
  sends raw NTLMSSP) failed to mount with "blob signature incorrect".

- `smb/server`: FSCTL_VALIDATE_NEGOTIATE_INFO echoes the dialect and
  capabilities this connection negotiated, not the server's configured
  defaults. A client that negotiated a lower dialect saw a mismatch and
  aborted the mount ("protocol revalidation - security settings mismatch").

- `smb/server`: answers the SMB1 multi-protocol negotiate that Windows sends
  first with a wildcard-dialect SMB2 negotiate response. Upstream could not
  parse it and stayed silent, so Windows clients hung and then failed with
  "the specified network name is no longer available".

- `smb/server` + `smb/ntlmssp`: the NEGOTIATE response advertises NTLM in an
  SPNEGO NegTokenInit. Upstream sent an empty security blob, which Windows
  answered by failing session setup with STATUS_INVALID_PARAMETER.

- `smb/ntlmssp`: the CHALLENGE_MESSAGE no longer sets NTLMSSP_NEGOTIATE_VERSION
  (the message carries no Version field, and Windows rejected the all-zero one
  with STATUS_INVALID_PARAMETER), sets TARGET_TYPE_SERVER and NEGOTIATE_56,
  drops NEGOTIATE_SEAL, and carries the AV_PAIRs Windows expects (NetBIOS
  domain and computer names, DNS names, and a timestamp), matching what
  Samba sends.

- `smb/server`: corrected the FileId offsets used to chain related compound
  requests (QUERY_INFO is at 24, SET_INFO at 16, LOCK and IOCTL at 8).
  Upstream read the wrong bytes, so the CREATE + QUERY_INFO + CLOSE compound
  the Linux client uses to mount failed with STATUS_FILE_CLOSED.

- `smb/wire` + `smb/server`: QUERY_INFO fixes. FILE_ALL_INFORMATION is now
  complete (internal, EA, access, position, mode, alignment, and name blocks);
  upstream sent a short one and the Linux client could not build the root
  inode. FileFsSizeInformation had its sector fields in the wrong place (free
  space showed as "blocks of size 0"), FileFsFullSizeInformation,
  FileFsDeviceInformation and FileFsSectorSizeInformation were missing, the
  filesystem no longer claims case-sensitive search, and file attributes now
  come from the backend instead of always being directory-or-archive.

- `smb/wire` + `smb/server`: buffer offsets in SESSION_SETUP, READ,
  QUERY_INFO and QUERY_DIRECTORY responses are relative to their own message
  header. Upstream measured them from the start of the whole output buffer,
  which is correct only for the first message in a compound reply; the
  Linux client reported "Server response too short" and failed to mount.

- `smb/wire` + `smb/server`: QUERY_DIRECTORY encodes the information class the
  client requested (directory, full, both, id-full, id-both, and names).
  Upstream encoded only two and silently fell back to the shortest layout, so
  a client asking for another class read names at the wrong offset: the Linux
  client logged "directory entry name would overflow" and showed truncated
  names.

- `smb/wire`: request parsers accept a body one byte shorter than
  `StructureSize`, which counts one byte of the variable part. Upstream
  rejected requests with an empty variable part, so QUERY_INFO for classes
  the Linux client sends bare (such as FileInternalInformation) failed with
  STATUS_INVALID_PARAMETER.
- `smb/server`: added FileInternalInformation, and file index numbers are
  computed from a normalized path so a file keeps the same index whether it
  was reached through a listing or a query (clients reported stale handles).

- `smb/server`: more QUERY_INFO classes (internal, EA, position, mode, name,
  normalized name, attribute tag, stream) and SET_INFO classes (allocation,
  position, mode, accepted as no-ops). Windows failed writes with
  STATUS_NOT_SUPPORTED without them.

- `smb/server`: FILE_FS_VOLUME_INFORMATION puts the volume label at its real
  offset (18) and sizes the buffer to match. Upstream's response was two
  bytes short with the label misplaced, and Windows answered later requests
  on that share with "the specified server cannot perform the requested
  operation".

- `smb/server`: QUERY_DIRECTORY pages through a directory across calls and
  honours SMB2_RETURN_SINGLE_ENTRY. Upstream answered the first call with
  everything it could fit and reported "no more files" afterwards, so a
  client that asks one entry at a time (Windows does) saw only the first
  entry of every directory, and large directories were silently truncated.

- `smb/server`: CREATE grants no oplock. Upstream granted whatever level the
  client asked for, including exclusive and batch, without implementing
  breaks, so Windows cached file contents and wrote stale buffers back:
  PowerShell's Set-Content appended to files instead of replacing them.
  Re-grant them only together with working oplock or lease breaks.

- `smb/server`: SET_INFO FileAllocationInformation truncates the file when
  the requested allocation is smaller than it (MS-FSCC section 2.4.4).
  Windows relies on this: PowerShell's Set-Content empties a file this way
  before writing, and treating it as a no-op silently appended to the old
  contents instead of replacing them. CreateAction also reports
  FILE_SUPERSEDED and FILE_OVERWRITTEN for the dispositions that mean them.

- `smb/server`: added FileAlternateNameInformation (the 8.3 name Windows asks
  for; the name itself is returned, since no aliases are kept).

- `smb/vfs` + `smb/server`: CREATE passes the client's DesiredAccess,
  ShareAccess and delete-on-close flag to the backend, and a backend that
  answers `vfs.ErrSharingViolation` reaches the client as
  STATUS_SHARING_VIOLATION. Upstream parsed share modes and discarded them,
  so no open ever conflicted with another.

- `smb/wire`: LOCK request elements are read from offset 24, where they
  belong (StructureSize 48 counts the 24-byte fixed part plus the first
  element). Upstream read them from offset 48, so every real client's LOCK
  was rejected with STATUS_INVALID_PARAMETER; its end-to-end test passed only
  because the test built requests in the same wrong shape, which was
  corrected too.
- `smb/vfs` + `smb/server`: byte-range locks go through a
  `vfs.ByteRangeLocker` the application can supply (`server.WithLocker`), so
  they can share a table with other protocols. The built-in table now records
  each lock's owner, so a handle's locks are released when it closes and a
  holder no longer conflicts with itself; previously locks lived per tree
  connect, were never released, and an unlock removed any matching range
  regardless of who held it. A refused lock answers STATUS_LOCK_NOT_GRANTED,
  as MS-SMB2 section 3.3.5.14 specifies (upstream answered
  STATUS_FILE_LOCK_CONFLICT, which is the status for a read or write that
  hits a lock); the upstream end-to-end test was updated to match.

- `smb/transport` + `smb/server`: a connection's responses are written by one
  writer goroutine, and the framed connection keeps separate read and write
  header buffers with a write mutex. Upstream shared one header buffer between
  reads and writes, which is safe only while a single goroutine does both.

- `smb/server`: TREE_CONNECT matches share names case-insensitively, as
  Windows does. macOS sends the share name upper-cased and was refused with
  STATUS_BAD_NETWORK_NAME.

- `smb/server`: credits are granted as the client requests them, up to the
  maximum, instead of topping the client up to the maximum in whichever
  response comes first. That was the NEGOTIATE response, whose grant macOS
  does not count, so macOS believed it had a single credit: it waited two
  seconds before every compound request and eventually hung. Every request,
  including one with a CreditCharge of zero, now uses at least one credit.

- `smb/server`: each response in a compound reply is signed after the next
  one is appended, so the signature covers its padding and NextCommand.
  Signing them as they were built left every response but the last with a
  signature that did not verify.

- `smb/server`: a QUERY_DIRECTORY that matches nothing answers
  STATUS_NO_SUCH_FILE; STATUS_NO_MORE_FILES is for a listing that has
  returned every match (MS-FSA 2.1.5.6.3). macOS looks up single names this
  way.

- `smb/vfs` (LocalBackend, which Bluestone does not use): paths convert SMB
  backslashes to slashes, so a file in a subdirectory no longer lands in the
  share root with a backslash in its name outside Windows; a rename takes the
  new path from the share root, so files can move between directories; and
  creating a directory with FILE_CREATE no longer fails with
  STATUS_OBJECT_NAME_COLLISION after making it. Setting attributes makes a
  file read-only only for FILE_ATTRIBUTE_READONLY (0x01); it did so for
  HIDDEN (0x02), and macOS hides the AppleDouble files it writes, which then
  could not be deleted.

- `smb/server`: `WithObserver` reports connections, authenticated sessions,
  and every completed request (command, status, duration), so an application
  can count what the server does without wrapping the protocol. A compound
  request reports each of its commands.

- `smb/server`: `WithLimits` caps the sessions a connection may hold, the
  trees a session may connect, and the files it may hold open; a request past
  a limit is refused with STATUS_INSUFFICIENT_RESOURCES. `WithAuthGate` lets
  the application refuse or slow down authentication attempts per client
  address, and is told the outcome of each attempt. Both default to off, so a
  server without them behaves as before.

- `smb/ntlmssp`: `NTHash` and `NTOWFv2FromHash` split the NT hash of a
  password from the per-login NTLMv2 key, so a server can store the hash
  rather than the password and still accept any user and domain. `NTOWFv2`
  is now the two called together.

- `smb/server` + `smb/wire`: backend errors map to the status a client acts
  on rather than mostly to STATUS_ACCESS_DENIED: ENOSPC becomes
  STATUS_DISK_FULL, EROFS a write-protected disk, ENOTEMPTY, EISDIR and
  ENOTDIR their own statuses, EBUSY a sharing violation, EMFILE and ENOMEM
  insufficient resources, a deadline or ETIMEDOUT an I/O timeout, and
  anything unrecognised STATUS_UNEXPECTED_IO_ERROR. The errno cases are
  tested before the io/fs sentinels, because Go reports some errnos as those
  sentinels (ENOTEMPTY reads as fs.ErrExist).

- `smb/server`: `Drain(ctx, idleFor)` shuts a server down gracefully, the way
  `http.Server.Shutdown` does: it stops accepting, then closes each
  connection once it has no request in flight, no reply waiting to be
  written, and has been quiet for `idleFor`. The quiet period matters
  because a client copying a file sends requests back to back, so closing in
  a gap between two of them fails the copy. Connections still busy when ctx
  is done are closed anyway. `Shutdown` remains the abrupt form.

- `smb/server` + `smb/wire`: QUERY_INFO answers a request for a file's
  security descriptor with a self-relative descriptor (owner
  BUILTIN\Administrators, group BUILTIN\Users, and a DACL granting everyone
  full access, inheritable on a directory), honouring the parts named in
  AdditionalInformation, and answers a buffer that is too small with
  STATUS_BUFFER_TOO_SMALL and the size to ask for. SET_INFO for security is
  refused rather than accepted and dropped. Upstream answered
  STATUS_NOT_SUPPORTED, which Windows reports as being unable to read the
  file's security information.
- `smb/wire`: QUERY_INFO reads AdditionalInformation from offset 16 and
  Flags from 20 (MS-SMB2 2.2.37). Upstream read them from 24 and 28, which
  are the first bytes of the FileId, so both fields were whatever the handle
  happened to contain.

- `smb/server` + `smb/wire` + `smb/vfs`: server-side copy.
  FSCTL_SRV_REQUEST_RESUME_KEY hands out a token naming an open, and
  FSCTL_SRV_COPYCHUNK copies ranges from the file that token names into
  another open, so a copy within a share never travels to the client and
  back. Requests past the limits (16 chunks, 1 MiB each, 16 MiB total) are
  answered with those limits, as MS-SMB2 3.3.5.15.6 specifies, and an
  unknown resume key with STATUS_OBJECT_NAME_NOT_FOUND so the client falls
  back. A backend that implements `vfs.ChunkCopier` copies the range
  itself, which over object storage can mean no bytes moving at all;
  returning errors.ErrUnsupported for a particular copy falls back to the
  server moving them. Upstream answered STATUS_NOT_SUPPORTED to both
  controls, so every copy went out to the client and back.
- Filesystem size queries ask the backend. FILE_FS_SIZE_INFORMATION and
  FILE_FS_FULL_SIZE_INFORMATION report what a backend implementing
  `vfs.SpaceReporter` returns, keeping free space within the total, and an
  error from it as the matching status. Upstream always reported 1 TiB with
  half free, so Explorer started copies that could not fit and they failed
  partway with STATUS_DISK_FULL. A backend without the interface still gets
  the nominal size.
- Delete semantics follow the file, not the handle (MS-FSA 2.1.5.4 and
  2.1.5.14.3). A table of open files across every connection holds each
  file's delete-pending state: the file is removed when its last handle
  closes, every handle reports it pending, and new opens get
  STATUS_DELETE_PENDING. A disposition needs delete access and is refused
  for a read-only file (STATUS_CANNOT_DELETE) and a non-empty directory
  (STATUS_DIRECTORY_NOT_EMPTY); FILE_DELETE_ON_CLOSE needs delete access,
  and a non-empty directory opened with it is left in place. Delete-on-close
  also applies when a client disconnects. Renames update every handle to
  the file, and are refused for a delete-pending file, a directory with
  anything open inside it, and a target someone has open. Upstream kept the
  disposition per handle and deleted on that handle's close, even with other
  handles open, and never updated a handle's path on rename: deleting
  through a handle after renaming its file removed whatever had the old
  name, which lost the new version of a file saved by renaming the old one
  aside.

## Known gaps to close in Bluestone

Tracked with the rest of the SMB work in `docs/SMB_ROADMAP.md`.

- Blocking byte-range locks: a lock that cannot be granted is refused even
  when the client did not set SMB2_LOCKFLAG_FAIL_IMMEDIATELY, instead of
  waiting for the conflicting lock to be released.
- No oplocks or leases, so clients cache nothing and every read crosses the
  wire. Granting them needs working breaks, including breaks caused by writes
  arriving over NFS.
- CHANGE_NOTIFY polls the directory every 500ms per watch and compares
  listings, which is expensive against object storage.
- No durable or persistent handles and no multichannel, so a dropped
  connection loses open handles.
- The dialect is fixed at 3.0.2: no SMB 3.1.1, so no pre-auth integrity and
  no AES-GCM. Signing is AES-CMAC and encryption AES-128-CCM.
- Byte-range locks are keyed by path in the lock table, and are not moved
  when their file is renamed. They are still released when the handle
  closes, but a lock taken before a rename does not conflict with NFS locks
  on the new name.
- No alternate data streams, so macOS writes AppleDouble files into the
  bucket.
