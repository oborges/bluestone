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
- Not vendored: `encryption.test` (a compiled test binary tracked upstream) and
  `examples/`.

## Known gaps to close in Bluestone

- Requests on a connection are handled one at a time.
- CREATE share access is parsed but not enforced (no sharing violations).
- The CREATE response reports only directory/archive attributes and ignores
  `vfs.FileInfo.Attributes`.
- Few QUERY_INFO classes; no leases; AES-CMAC signing and AES-128-CCM
  encryption only.
- Not yet tested against Windows clients.
