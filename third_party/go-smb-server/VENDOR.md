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

## Known gaps to close in Bluestone

- Requests on a connection are handled one at a time.
- CREATE share access is parsed but not enforced (no sharing violations).
- The CREATE response reports only directory/archive attributes and ignores
  `vfs.FileInfo.Attributes`.
- Few QUERY_INFO classes; no leases; AES-CMAC signing and AES-128-CCM
  encryption only.
- Not yet tested against Windows clients.
