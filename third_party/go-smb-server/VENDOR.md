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

## Known gaps to close in Bluestone

- Blocking byte-range locks: a lock that cannot be granted is refused even
  when the client did not set SMB2_LOCKFLAG_FAIL_IMMEDIATELY, instead of
  waiting for the conflicting lock to be released.

- Requests on a connection are handled one at a time.
- CREATE share access is parsed but not enforced (no sharing violations).
- The CREATE response reports only directory/archive attributes and ignores
  `vfs.FileInfo.Attributes`.
- Few QUERY_INFO classes; no leases; AES-CMAC signing and AES-128-CCM
  encryption only.
- Not yet tested against Windows clients.
