# SMB Roadmap

The SMB server works: Windows Server 2025, macOS, the Linux kernel client and
`smbclient` all pass the suites in `scripts/smb-interop/` against a real COS
bucket, with Windows naming, DOS attributes, share modes, byte-range locks
shared with NFS, and concurrent reads and writes. It is still marked
experimental because of what surrounds the protocol work rather than the
protocol work itself: operational limits, the features Windows applications
expect a file server to answer, and the depth of testing.

This document records what is missing, in the order worth building it, and
what "production grade" is taken to mean.

## Where the line is

Dropping "experimental" needs phases 1 and 2, the change-notification work
from phase 3, and a scale and soak pass. That yields a server that behaves
correctly under the failures that actually happen, can be monitored and
bounded, and is honest about the two things it still does not do: hand
clients any cached state, and survive a dropped connection with handles
intact.

Leases and durable handles are what separate a solid team share from a
drop-in Windows file server. Each is a substantial project of its own, and
each risks the consistency that not caching currently buys us, so they come
after the gateway is trustworthy to operate.

## Phase 1: safe to run

Operability and abuse resistance. Nothing here changes the protocol surface,
so it carries the least risk of regressing interop.

- Credentials: store the NTLMv2 hash (NTOWFv2) rather than the plaintext
  password in the config file, with a tool to generate one. Plaintext stays
  supported for one release, and warns.
- Limits: maximum connections per server and per client address, sessions per
  connection, opens per session, and a cap on granted credits. A rejected
  request answers the right status rather than growing memory.
- Authentication backoff: increasing delay and a temporary block after
  repeated failures from one address.
- Health: an SMB check registered with the health checker, so the listener
  and session count show up in `/health`.
- Metrics: sessions, opens, locks, requests by command, errors by status,
  bytes moved, and per-command latency, labelled `protocol="smb"`.
- Error mapping: cover the statuses clients act on, including a full staging
  disk as `STATUS_DISK_FULL`, rather than mapping the unrecognised to
  `STATUS_ACCESS_DENIED`.
- Shutdown: drain connections instead of closing them from under clients.
- HA: stop serving when the lease is lost, not only refuse to start without
  it. Applies to NFS as well, and so belongs with the gateway rather than
  with SMB alone.

**Done when:** limits are enforced and covered by tests, `/health` and the
metrics endpoint describe the SMB server, a full staging disk surfaces as a
disk-full error on Windows, and losing the HA lease stops both servers.

## Phase 2: behaves like a file server

The visible "this is not a real file server" failures.

- Security descriptors: answer `QUERY_INFO` for security with a synthetic
  descriptor (owner is the authenticated user, everyone full control), and
  accept sets without storing them. Explorer's Security tab, and the
  installers and Office paths that ask for an owner on open, currently get
  `STATUS_NOT_SUPPORTED`.
- Server-side copy (`FSCTL_SRV_COPYCHUNK`): copy within the share without the
  bytes leaving the gateway, using a COS server-side copy where the source is
  clean. Today Explorer and robocopy pull every byte to the client and push
  it back.
- Disk full and quota reporting, so Windows warns before a write fails.
  Done: the share reports the staging area's size and the room below its
  high watermark, and a write past it is `STATUS_DISK_FULL`. A bucket over
  its hard quota is detected from COS's `BucketQuotaExceeded` (checked
  against a real bucket with a 1 MiB quota). Writes then fail as disk full
  at once over SMB and NFS, the share shows no free space, and the gateway
  keeps its HA lease instead of fencing itself.
- Delete-on-close and delete-pending semantics, and the rename and delete
  edge cases around open handles.
  Done: matches Windows' own server scenario for scenario
  (`windows-delete-test.ps1`), apart from one query the Windows client
  answers from its cache without leases.
- Alternate data streams, at least enough that macOS stops writing
  AppleDouble `._` files into the bucket and Windows can keep its
  "downloaded from the internet" mark.
  Done: streams are kept in object metadata, capped by
  `smb.max_stream_bytes`. They match Windows' own server
  (`windows-streams-test.ps1`) apart from the cap, and macOS keeps
  Finder information, tags and extended attributes with no `._` files
  (`macos-streams-test.sh`). Large resource forks do not fit.

**Done when:** the Security tab opens, a large copy inside the share runs at
COS-to-COS speed rather than client round-trip speed, a full bucket or
staging area reports as disk full, and macOS no longer litters `._` files.

## Phase 3: fast

- Change notification: the vendored implementation polls every 500 ms and
  re-enumerates the whole directory per watch, and Explorer opens a watch per
  window. Drive it from staging and metadata events instead, with one watcher
  per directory shared by its watchers.
  Done: the filesystem layer publishes every change made through NFS or SMB,
  and the object refresh scanner publishes changes found in the bucket. The
  server routes them to watches with no listing at all. Checked with 21
  Windows watchers: changes from SMB and NFS arrive, and there are no
  directory listings while idle (`windows-notify-test.ps1`).
- Leases: read and read-handle leases with working breaks, including breaks
  caused by NFS writes and by changes found in the bucket. This is where
  client-side caching comes from, and where cross-protocol consistency is
  easiest to lose. Oplocks and leases stay ungranted until breaks are
  implemented and tested from both protocols.
  Done: read and read-handle leases, and level II oplocks. Write caching is
  never granted. On Windows (`windows-lease-test.ps1`), an NFS write broke a
  read-handle lease and the next read through the same handle saw the new
  data. 50 reads of a file went from 400 READs to 1. A macOS write to a file
  Windows had cached a handle to succeeded after the handle break, where it
  would have failed as "in use".

**Done when:** a directory with many watchers costs no listings while idle,
and a lease held by a Windows client is broken by an NFS write to the same
file before that write is acknowledged.

## Phase 4: survives

- SMB 3.1.1: negotiate contexts, pre-auth integrity, and AES-GCM.
  Done: Windows Server 2025 and macOS negotiate 3.1.1, and Windows encrypts
  with AES-128-GCM when encryption is required. A 256 MB copy ran at about
  265 MB/s up and 480 MB/s down, against 150 and 195 MB/s with AES-128-CCM
  over 3.0.2. `smb.max_dialect: "3.0.2"` caps the dialect. Encryption over
  3.0.2 had never worked with Windows, and now does.
- Durable handles v2, so a dropped connection pauses a client rather than
  failing its open files.
  Done (`windows-durable-test.ps1`). Windows held a file open with a
  byte-range lock and its connection was cut. An NFS lock on the same bytes
  was refused during the outage. Windows then reconnected, reclaimed the
  handle, wrote through it, and released its lock. With durable handles off,
  every operation on the handle failed and the lock was lost.
- Multichannel, optionally, once durable handles exist.

**Done when:** Windows negotiates 3.1.1 with GCM, and a client survives a
network interruption with its handles and locks intact.

## Phase 5: identity

- Kerberos and Active Directory: the vendored library already has Kerberos
  support that the gateway does not wire up.
- Per-share access control and read-only shares.
- Mapping Windows identities to the metadata the gateway stores.

Done (`windows-domain-test.ps1`), against a Windows Server 2025 domain
controller with a gateway that had no local accounts:
- A domain user reached a share only their group was admitted to, with a
  Kerberos ticket, both with credentials given and signed in at the
  machine with none asked for.
- A user outside the group was refused it. On a read-only share they could
  read but not write, delete or create, while a user on its write list
  could.
- Files record their creator: Windows showed the domain account as owner
  after a gateway restart, from the uid kept in the object's metadata.
  NFS clients see the same owner as a uid and gid.
- Local accounts keep working alongside over NTLM, and Windows, macOS, the
  Linux kernel client and `smbclient` pass their suites with Kerberos
  advertised.

**Done when:** a domain-joined Windows client mounts with its own credentials
and no local user exists in the config.

## Continuous

- Scale and soak: directories of 100k entries, files over 4 GB, many
  concurrent clients, and multi-day runs.
- Fuzzing of the wire parsers, which today are covered only by the cases real
  clients produced.
- A client matrix: Windows 10 and 11, Server 2019 through 2025, several macOS
  releases and `cifs` versions, Office, and robocopy.
- Upstream: `third_party/go-smb-server/VENDOR.md` lists every local change.
  Six pull requests are open against the upstream project; keeping them moving
  keeps the vendored diff from drifting.

## Known limitations that stay

These are consequences of serving object storage, not gaps to close:

- A cold directory listing reports the object's last-modified time, and the
  default owner and mode, until something stats the file, because COS
  listings carry no user metadata. `ls -l` over NFSv4 shows what the listing
  says; `stat` always shows what is stored.
- NFS does not take part in the share-mode table, so an NFS client can open a
  file an SMB client holds exclusively.
- Byte-range locks are refused rather than queued: a client that asked to
  wait is told no.
- Locks and open state are in memory and do not survive a gateway restart.
