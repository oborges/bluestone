# Bluestone

**A file gateway for IBM Cloud Object Storage.**

> Bluestone was previously named *IBM Cloud COS NFS Gateway*. Existing
> `NFS_GATEWAY_*` environment variables and `/etc/nfs-gateway` config paths
> still work and log a deprecation notice. To move a systemd installation to
> the new names, see
> [Migrating From nfs-gateway](docs/LINUX_SERVICE.md#migrating-from-nfs-gateway).

Bluestone exposes an IBM Cloud Object Storage bucket through an
NFSv4 mount by default, with optional NFSv3 compatibility. It is intended for
Linux workloads that need a filesystem-shaped interface while storing file data
in COS.

This is an unofficial community project. It is not an IBM product, is not
endorsed by IBM, and is provided as-is without warranty or official support.
Test carefully with your own workload before relying on it.

## What This Gateway Does

- Serves an NFSv4 export backed by one IBM Cloud COS bucket, with optional
  NFSv3 or dual-protocol serving.
- Accepts POSIX-style file operations from Linux NFS clients.
- Optionally serves the same bucket over SMB 3 (experimental) to Windows,
  macOS, and Linux clients, with NTLM users and Windows naming. See
  [SMB](#smb).
- Uses a local staging layer for writes.
- Syncs staged dirty files to COS asynchronously in background workers.
- Uses multipart upload for large staged objects.
- Provides staging backpressure to prevent the local staging filesystem from
  filling unexpectedly.
- Provides metadata and chunk/range data caching for reads.
- Supports advisory byte-range file locking over NFSv4 (`fcntl` and `flock`)
  and SMB, with POSIX conflict semantics, capped at 512 locks per file and
  8,192 per client. The two protocols share one lock table, so a range locked
  by an NFS client is refused to an SMB client and the other way round. Locks
  are advisory only and do not survive a gateway restart.
- Tolerates object-store outages: starts degraded when COS is unreachable,
  keeps accepting staged writes, serves staged/cached reads and staging-backed
  or stale metadata, accepts deletes via durable tombstones, and reconciles
  when the backend heals. Set `staging.clean_after_sync: false` to retain
  staged copies as a local tier for maximum outage coverage.
- Supports active/passive high availability fenced by a bucket lease: a
  standby with replicated staging state promotes automatically once a crashed
  primary's lease goes stale (planned failovers are immediate), and a second
  active gateway is refused. See [docs/HA.md](docs/HA.md).
- Uses read-ahead, parallel range fetches, and singleflight deduplication to
  reduce repeated COS reads.
- Exposes Prometheus metrics, health endpoints, and debug endpoints when
  enabled.
- Includes a repeatable benchmark suite for write, sync, read, backpressure,
  small-file, crash-safety, and mixed workload validation.

## The Most Important Write Semantics

The gateway uses write-back asynchronous sync.

When an NFS write is accepted by the gateway, the data has been accepted into
local staging. That does not mean the object is already durable in COS.

Durability in COS happens later, when the background sync worker uploads the
staged file and the object becomes visible in the target bucket. Until that
sync completes, the gateway must preserve the staged dirty data locally. If you
need to know whether data is durable in COS, monitor the sync queue, dirty
bytes, upload metrics, logs, or the debug staging endpoint.

In short:

- "Write accepted" means local staging accepted the write.
- "Sync complete" means the staged file was uploaded to COS.
- "Durable in COS" means the uploaded object is visible in COS with the
  expected size/checksum for your validation process.

## Filesystem Operation Limits Over COS

COS/S3 is an object store, not a local filesystem. The gateway keeps these
operations explicit:

- File rename is copy to the new key followed by delete of the old key. It is
  not atomic. If copy fails, the old object remains and a destination object may
  or may not have been created by COS. If delete fails after copy succeeds, both
  old and new objects may exist; the gateway does not try to delete the new
  copy as rollback.
- Directory rename is a best-effort recursive prefix operation. The gateway
  lists keys under the old directory prefix, copies every listed key to the new
  prefix, and only then deletes old keys. It is not atomic. If a copy fails,
  source keys are not deleted and already copied destination keys are left in
  place. If a delete fails, both prefixes may contain objects.
- Directory rename to an existing destination path is rejected to avoid
  implicit merges. Renaming a directory into its own subtree is rejected.
- Delete of a dirty staged file succeeds with POSIX write-back semantics: the
  gateway persists a durable tombstone, discards the staged bytes, and removes
  the COS object (after any in-flight upload completes, retried until
  confirmed). Tombstones survive restarts so an accepted delete cannot
  resurrect the file; recreating the path cancels the pending delete.
- Rename of a dirty staged file succeeds with POSIX write-back semantics: the
  staged bytes and dirty bookkeeping move to the destination name, a durable
  tombstone retires the source COS object (after any in-flight upload), and
  the moved bytes sync to the destination key. This supports the
  write-tmp-then-rename atomic-save pattern used by editors and sync tools.
  Renaming a clean file over a destination whose staged bytes are mid-upload
  returns busy (retryable); directory rename is rejected when any dirty staged
  child exists under the source or destination tree. This avoids losing
  accepted writes that are still only in local staging.
- `mkdir` creates a trailing-slash directory marker object. `rmdir` removes the
  marker only when the gateway's current listing sees the directory as empty.
  Implicit directories still come from object key prefixes and may converge
  through cache expiry or refresh scans.
- Mutating operations invalidate the target metadata, affected parent and
  ancestor directory listings, and affected data cache entries. Directory
  rename/delete invalidates cached data under the old and new prefixes.

## Prerequisites

Before running the gateway, the operator must create and provide:

- An IBM Cloud Object Storage service.
- A COS bucket.
- An API key or HMAC credentials with the required permissions for that bucket.
- A Linux host with NFS client utilities.
- Local disk capacity for staging and read cache.
- Go 1.25 or newer if building from source.

The NFS export itself does not implement user authentication. Deploy it only on
trusted hosts or trusted networks, and control access with operating-system,
firewall, VPC, security group, or Kubernetes network policy boundaries.

## Quick Start

Clone and build:

```bash
git clone https://github.com/oborges/bluestone.git
cd bluestone
make build
```

Create a configuration file:

```bash
cp configs/config.example.yaml configs/config.yaml
```

Edit `configs/config.yaml` with your COS settings:

```yaml
cos:
  endpoint: "s3.us-south.cloud-object-storage.appdomain.cloud"
  bucket: "my-nfs-bucket"
  region: "us-south"
  auth_type: "iam"
  api_key: "your-ibm-cloud-api-key"
  service_id: "ServiceId-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
```

Run the gateway:

```bash
sudo ./bin/bluestone --config configs/config.yaml
```

Or install it as a Linux `systemd` service:

```bash
sudo ./scripts/install-linux-service.sh --build
sudoedit /etc/bluestone/config.yaml
sudo systemctl enable --now bluestone
```

The installer creates a dedicated `bluestone` system user, installs the unit
file, preserves existing config by default, and prepares cache and staging
directories. See [Linux Service Installation](docs/LINUX_SERVICE.md) for the
operator runbook.

Mount it from the same Linux host:

```bash
sudo mkdir -p /mnt/cos-nfs
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos-nfs
```

Unmount when finished:

```bash
sudo umount /mnt/cos-nfs -f
```

## Configuration Areas

The full example lives in `configs/config.example.yaml`. All nested settings can
also be overridden with environment variables using the `BLUESTONE_` prefix.
For example, `cos.api_key` becomes `BLUESTONE_COS_API_KEY`.

### Server

```yaml
server:
  nfs_port: 2049
  nfs_version: "4" # "4" by default; use "3" for NFSv3 or "dual" for both
  metrics_enabled: true
  metrics_port: 8080
  health_enabled: true
  health_port: 8081
  debug_enabled: true
  debug_port: 8082
  allowed_clients: [] # CIDRs/IPs allowed to connect; empty allows all
  nfs_concurrent_handlers: 0 # per-connection parallelism; 0 = default (64), 1 = serial
```

Metrics, health, and debug HTTP servers bind to localhost. Enable only the
endpoints you need.

`allowed_clients` restricts NFS connections to the listed CIDRs or IPs, the
same model as cloud security groups: rejected connections are dropped at TCP
accept before any RPC parsing and logged for auditing. The export still has no
user authentication, so combine the allowlist with OS/VPC firewalling and
trusted networks. `nfs_concurrent_handlers` is an operational escape hatch for
the per-connection request parallelism: set it to `1` to restore fully serial
handling if a client misbehaves with concurrent replies.

### SMB

```yaml
smb:
  enabled: true
  port: 445
  share_name: "bluestone"
  domain: "BLUESTONE"
  encryption_required: false
  max_dialect: "3.1.1"   # or "3.0.2" for clients that mishandle 3.1.1
  concurrent_requests: 0 # reads/writes at once per connection; 0 = default (64), 1 = serial
  drain_timeout: "30s"   # how long a shutdown waits for clients to finish
  max_stream_bytes: 2048 # cap on a file's named streams, kept in object metadata
  leases: true           # let clients cache the files they read
  durable_handles: true  # keep open files through a dropped connection
  limits:
    max_connections: 256
    max_connections_per_client: 64
    max_sessions_per_connection: 32
    max_trees_per_session: 64
    max_opens_per_session: 4096
    auth_failures: 5
    auth_window: "5m"
    auth_block: "30s"
    auth_max_block: "15m"
  users:
    - username: "alice"
      # The account's NT hash, from: bluestone -smb-hash
      ntlm_hash: "<32 hex characters>"
      uid: 2001            # owner recorded for files alice creates (optional)
      gid: 2001
      groups: ["staff"]    # for "@staff" in share access lists (optional)
  kerberos:
    keytab: "/etc/bluestone/smb.keytab"  # lets Active Directory users sign in
  id_map:
    domain_sid: "S-1-5-21-<n>-<n>-<n>"   # map domain accounts to uids by RID
    base: 100000
  shares:                  # optional; without it, share_name serves the bucket
    - name: "projects"
      path: "/projects"
      valid_users: ["S-1-5-21-<n>-<n>-<n>-1105", "@staff"]
      read_list: ['CORP\contractor']
    - name: "public"
      path: "/public"
      read_only: true
      write_list: ["alice"]
```

The SMB server is experimental and disabled by default. It serves the same
bucket and staging layer as NFS, so both protocols see the same files. What
"experimental" still means, and the work to remove it, is in
[`docs/SMB_ROADMAP.md`](docs/SMB_ROADMAP.md). SMB
clients get Windows naming: names match case-insensitively, and characters
Windows cannot use in names are shown as Unicode private-use characters and
mapped back to the stored key. Creation time and the read-only, hidden,
system, and archive attributes are stored in object metadata.

`smb.limits` bounds what clients can make the gateway hold, so one client
cannot exhaust it: connections to the server and from a single address,
sessions per connection, share connections per session, and files a session
holds open. A request past a limit is refused with
`STATUS_INSUFFICIENT_RESOURCES`, and a connection past one is closed at
accept. `0` turns a limit off. The defaults are generous enough that no
ordinary client meets them.

Repeated login failures from one address are slowed down: after
`auth_failures` failures within `auth_window`, that address is refused for
`auth_block`, and each further failure doubles the wait up to
`auth_max_block`. A refused attempt is answered as a wrong password, so a
client cannot tell a block from a bad credential, and a successful login
clears the record, so a mistyped password costs a user nothing. Blocking is
by client address, which is blunt behind NAT; the defaults are set so
ordinary retries never reach the threshold. Watch `smb_auth_failures_total`,
`smb_auth_blocked_total` and `smb_auth_blocked_clients` for a client that is
guessing.

Users authenticate with NTLM against the accounts listed under `users`. Give
each account an `ntlm_hash` rather than a `password`:

```bash
bluestone -smb-hash
```

It reads the password from standard input, so it never reaches the process
list or the shell history, and prints the NT hash to put in the file. The
hash authenticates exactly as the password does, so it is still a secret to
protect: what it avoids is writing down a password that its owner may have
reused elsewhere. It is the same hash Windows and Samba store, so an existing
one can be pasted in. `password` still works and logs a warning at startup;
set one or the other, not both.

Keep the configuration file readable only by the gateway's service account.
`server.allowed_clients` also applies to the SMB port. Binding port 445 on
Linux needs root or `CAP_NET_BIND_SERVICE`. `smb.enabled`, `smb.port`,
`smb.share_name`, `smb.domain`, `smb.encryption_required`,
`smb.kerberos.keytab` and `smb.id_map.*` can be overridden with
`BLUESTONE_SMB_*` environment variables; users and shares are read from the
file only.

#### Active Directory

With `smb.kerberos.keytab` set, users of an Active Directory domain sign in
with their own credentials over Kerberos, and a domain-joined Windows machine
connects without asking for a password. The gateway needs no local accounts
and does not join the domain: it only needs a service account whose key it
holds.

1. Give the gateway a DNS name clients resolve, such as
   `gw.corp.example.com`. Kerberos is only used when clients connect by
   name; a client that connects by IP address falls back to NTLM, which only
   local accounts can use.
2. Create a service account for it, with AES-256, and write its keytab:

   ```powershell
   New-ADUser bluestone-gw -AccountPassword (Read-Host -AsSecureString) -Enabled $true -PasswordNeverExpires $true
   Set-ADUser bluestone-gw -KerberosEncryptionType AES256
   ktpass -princ cifs/gw.corp.example.com@CORP.EXAMPLE.COM -mapuser CORP\bluestone-gw `
       -crypto AES256-SHA1 -ptype KRB5_NT_PRINCIPAL -pass * -out gw.keytab
   ```

3. Copy `gw.keytab` to the gateway, readable only by its service account,
   and point `smb.kerberos.keytab` at it. The gateway logs the principals it
   answers to at startup. Clocks must agree to within
   `smb.kerberos.max_clock_skew` (five minutes).

The ticket's PAC tells the gateway who the user is: their SID, their groups,
and their domain's NetBIOS name. Local accounts under `users` keep working
alongside, over NTLM.

#### Shares and access

Without `smb.shares`, one share named `share_name` serves the whole bucket to
every user who can sign in. `smb.shares` replaces it with shares of their own
directories, created when missing. Shares may not overlap. Each share can
limit who uses it and how:

- `valid_users`: the only users admitted; empty admits everyone.
- `read_only`: nobody may change anything, except users in `write_list`.
- `read_list`: users who may only read, even on a writable share.
  `write_list` wins for a user on both.

Users are named as Windows names them: `CORP\alice` or `alice` for an
account, `@staff` for a group of local accounts, or a SID for a domain
account or group. Domain groups can only be named by SID, as
`Get-ADGroup Engineering | select SID` shows it. In YAML, write
`DOMAIN\user` in single quotes: in double quotes, `\` starts an escape. A
local account matches `DOMAIN\user` only for the gateway's own `smb.domain`,
since an NTLM client names whatever domain it likes.

A user a share does not admit is refused when connecting. On a read-only
share, opening a file to change it, creating, deleting or renaming is refused
with "access denied", as a Windows server does, and Windows shows the share's
files as readable only.

#### Owners

Files record who created them. With `smb.id_map.domain_sid` set, a domain
account's uid is `id_map.base` plus its relative ID (the last part of its
SID), and its gid comes from its primary group the same way, as Samba's
`idmap_rid` does. A local account with a `uid` and `gid` gets those. Windows
shows the owner on a file's Security tab: the domain account, or a Unix SID
(`S-1-22-1-<uid>`) for a local one. Without an id map, files keep the
default owner, as before.

Tested against Windows Server 2025 (SMB 3.1.1 with signing, or encryption), macOS
(`mount_smbfs`), the Linux kernel client (`mount -t cifs`), and `smbclient`:
mapping a drive, listing, reading
and writing, copying multi-megabyte files, case-insensitive access, DOS
attributes and creation times, the write-temp-then-rename pattern that Office
and many editors use, renames, and deletes. Scripts for repeating these checks
are in `scripts/smb-interop/`.

Reads and writes are handled concurrently, up to 64 at a time per connection
by default: a client waits on the object store far more than on the gateway,
so handling requests in turn would cost it a round trip each. Requests that
create or destroy state, such as opening and closing files, stay ordered.
`concurrent_requests: 1` restores the older serial behaviour.

Shutting the gateway down drains the SMB server: it stops accepting clients,
lets the requests already in flight finish, and closes each connection once
its client has been quiet for a moment, so a copy in progress completes
instead of failing. A client still busy after `smb.drain_timeout` has its
connection closed anyway, so one that has stopped responding cannot hold up a
restart; the gateway logs how long it waited and how many clients were still
busy.

Failures are reported as the status a client acts on, rather than as a
permissions error: a full staging area reaches Windows as "there is not
enough space on the disk", a read-only backend as a
write-protected disk, an operation that timed out as an I/O timeout, and a
backend failure the gateway does not recognise as an I/O device error.
`smb_requests_total{status="..."}` counts them, so a rise in
`STATUS_DISK_FULL` is visible before users report it.

The share reports the size of the staging area, the same capacity NFS
clients see: every write lands there before it is uploaded, so it bounds
what a copy can hold. Free space is the room left below the staging high
watermark, where writes start waiting on uploads, and it shrinks as unsynced
data piles up. Explorer checks it before a copy and refuses one that will
not fit, rather than failing partway through. The flip side is that a
single copy larger than the free staging space is refused up front even
though uploads would drain staging while it ran; size staging for the
largest copy users make, or copy in parts.

A bucket with a hard quota is reported as full once it refuses writes. IBM
COS counts a bucket's usage a few minutes behind, so some writes past the
quota succeed, and the gateway learns the bucket is full when COS first
refuses one (`BucketQuotaExceeded`). From then on, new writes fail at once
with "not enough space on the disk" (SMB) or "No space left on device"
(NFS), instead of being staged and then failing to upload, and the share
shows no free space. Reads and deletes still work, so users can make room.
Files staged before the bucket filled stay in staging and upload once there
is room. The gateway notices when an upload succeeds again, or retries
writes two minutes after the last refusal. `cos_bucket_quota_exceeded` is 1
while the bucket is full. After a quota is raised, COS can take half a
minute to apply it everywhere, and writes may be refused now and then
meanwhile.

Clients watching a directory, as Explorer and Finder windows do, are told of
changes as they happen, whether made over SMB or NFS. The gateway reports
changes from its own filesystem layer rather than by listing directories, so
open windows cost nothing while nothing changes, however many there are.
Changes made directly in the bucket, by tools that bypass the gateway, reach
them only when the object refresh scanner (`object_refresh`) finds them, if
it is enabled.

Byte-range locks taken over SMB go into the same table as NFS locks, so the
two protocols conflict with each other on the same bytes. A lock belongs to
the handle that took it and is released when that handle closes or its session
ends. Locks that cannot be granted are refused rather than queued: a client
that asked to wait for a lock is told no instead of blocking.

Share modes are enforced: a client that opens a file without sharing it, as
editors and Office do while a document is open, makes other clients' opens
fail with a sharing violation until it closes. The table of open files lives
in the gateway, so opens conflict across all SMB clients; NFS does not take
part in it yet.

Deletes follow Windows semantics. A file marked for deletion stays until
the last handle to it closes, not only the handle that marked it; every
handle reports the delete as pending, and new opens are refused meanwhile.
A read-only file and a directory with anything in it refuse to be deleted,
and a directory opened delete-on-close while it has entries is left in
place. Handles follow renames, so deleting through a handle after renaming
its file deletes the renamed file, never a new file that has taken the old
name, which is how applications that save by renaming the old version
aside behave. A directory with a file open inside it, a file someone has
open as a rename target, and a file waiting to be deleted all refuse
renames, as on Windows.

Copying a file inside the share is done by the gateway: the client asks for
a server-side copy and the bytes never travel to it and back. When the whole
file is being copied, the gateway goes further and copies the object inside
the bucket, so no bytes move at all. That includes the usual Windows copy,
which creates the destination and sets its length before copying: nothing
has been written to it, so the gateway drops the empty staged file and
copies the object in its place. A copy from a file with staged changes, or
into one something has written to, is copied by the gateway instead.

Windows asks for a file's security descriptor to show its Security tab, and
some applications ask on open. The gateway answers with everyone having full
access, owned by `BUILTIN\Administrators`: it stores no Windows owners or
ACLs, and says so plainly rather than inventing detail. Changing permissions
from Windows is refused, because accepting a change the gateway cannot store
would show permissions that nothing enforces. Access is controlled by the
accounts in `smb.users` and by `server.allowed_clients`.

Clients may cache the files they read (`smb.leases`, on by default).
Windows and macOS get read and read-handle leases, and older clients level II
oplocks. Reads of a cached file are served from the client, and a handle
closed and reopened is reused. In one Windows test, 50 reads of a file took
70 ms instead of 969, with 1 read reaching the gateway instead of 400.

Clients never cache writes: every write reaches the gateway. When a file
changes through anyone else, the gateway breaks the leases on it before
acknowledging the change, and the clients drop what they cached. That covers
another SMB client, an NFS client, and changes the object refresh scanner
finds made directly in the bucket. Without the scanner, a client can keep
serving its cached copy of a file changed behind the gateway's back, until
it closes the file. Turn the scanner on, or leases off, if other tools write
to the bucket.

Files a client has open survive its connection dropping
(`smb.durable_handles`, on by default). When a connection is lost rather
than closed, the gateway keeps the client's open files for up to a minute
(or the time the client asks for, at most five), with their byte-range
locks, and the client reclaims them when it reconnects: an application in
the middle of writing a file carries on, and nobody else can take its locks
meanwhile. A client that does not come back loses them when the time runs
out, and another client that needs one of those files sooner gets it. Open
files do not survive the gateway itself restarting or failing over.

A client caching a handle keeps the file open after the application closes
it. When another client's open would conflict with that handle, the gateway
asks the client to let go and waits for it, up to 35 seconds, instead of
failing the open as "in use".

The gateway speaks SMB 2.0.2 through 3.1.1. With 3.1.1, the handshake is
protected by pre-authentication integrity (SHA-512), sessions are signed with
AES-CMAC, and `encryption_required` encrypts them with AES-128-GCM, or
whichever of AES-128/256-GCM/CCM the client prefers. SMB 3.0 clients encrypt
with AES-128-CCM. GCM is markedly faster: Windows Server 2025 copied a
256 MB file at about 265 MB/s up and 480 MB/s down with GCM, against 150 and
195 MB/s with CCM. `max_dialect: "3.0.2"` caps the dialect for a client that
mishandles 3.1.1. Leases are not granted on sessions that require
encryption.

Named data streams (alternate data streams) are supported, and kept in the
object's metadata rather than as objects of their own. macOS stores Finder
information, tags and extended attributes this way instead of writing an
AppleDouble `._` file beside every file, and Windows keeps a download's
"downloaded from the internet" mark. Streams move with renames and copies,
go with the file when it is deleted, and are dropped when a file is
replaced, as on Windows. Object metadata is small, so a file's streams are
capped at `smb.max_stream_bytes` (2048 bytes, names and contents together,
by default). A write past it fails as disk full, and macOS then reports that
it could not copy a file's extended attributes. That rules out large
resource forks and long extended attributes. IBM COS holds about 4 KB of
metadata per object, which allows up to 2560. For an object store with
Amazon S3's 2 KB limit, set it to 1024.

### Staging And Async Sync

```yaml
staging:
  enabled: true
  root_dir: "/var/staging/bluestone"
  sync_interval: "30s"
  sync_threshold_mb: 10
  max_dirty_age: "5m"
  sync_on_close: false
  max_staging_size_gb: 10
  max_dirty_files: 1000
  sync_worker_count: 4
  sync_queue_size: 100
  max_sync_retries: 3
  retry_backoff_initial: "1s"
  retry_backoff_max: "60s"
  clean_after_sync: true
```

Dirty files are kept under `root_dir` until they are synced. On restart, the
gateway scans staging metadata and resumes syncing dirty data. For production
use, put staging on reliable local storage with enough free capacity for your
largest expected dirty working set.

### Backpressure

```yaml
staging:
  backpressure_enabled: true
  backpressure_mode: "block"
  backpressure_high_watermark_percent: 80
  backpressure_critical_watermark_percent: 95
  backpressure_wait_timeout: "30s"
  backpressure_check_interval: "250ms"
```

Backpressure protects staging before it is full.

- `block` waits for sync workers to drain dirty data until the timeout.
- `fail_fast` rejects writes immediately at or above the critical watermark.
- Above the critical watermark, writes receive deterministic errors instead of
  being allowed to run until the filesystem is full.
- Sync workers continue uploading and cleaning dirty files while pressure is
  active.

Every backpressure decision is logged with the path, requested bytes, available
bytes, pressure level, and decision.

### Read Cache And Read-Ahead

```yaml
cache:
  metadata:
    enabled: true
    size_mb: 256
    ttl_seconds: 60
    max_entries: 10000
  data:
    enabled: true
    size_gb: 10
    path: "/var/cache/bluestone"
    chunk_size_kb: 1024

performance:
  read_ahead_kb: 8192
  max_concurrent_reads: 50
```

Reads can be served from local chunk cache when available. Cold reads fetch
object ranges from COS, warm reads can hit local cache, and read-ahead can fetch
nearby chunks in parallel for sequential access patterns.

### Object-Side Change Refresh

By default, object-side changes made directly in COS converge through normal
cache expiry. Metadata entries and directory listings expire after
`cache.metadata.ttl_seconds`; the NFS directory wrapper has its own short
listing TTL.

You can also enable a periodic prefix scan:

```yaml
object_refresh:
  enabled: true
  interval: "5m"
  prefix: ""
```

When enabled, the gateway lists the configured COS prefix and compares each
object's list signature: key, size, ETag, and last-modified time. Created,
updated, and deleted objects invalidate the affected metadata cache entry, its
parent directory listing, and the object's data cache. The scanner keeps only an
in-memory previous-scan map; it does not require a database.

Dirty staged files stay authoritative until the scanner observes that the same
COS key changed after the scanner's base observation. At that point the gateway
records a conflict: the local dirty staging file is copied under
`staging.root_dir/lost+found/` with a JSON metadata sidecar, the original path is
removed from the sync queue, and the refreshed COS object becomes the default
visible version. If conflict recording fails, refresh keeps the older dirty-path
skip behavior and does not overwrite local staged bytes. Parent directory
listings may still be invalidated, and staged files are re-added to listings by
the staging layer when no conflict exists. The supported write model remains
one gateway owning writes for the mounted export; multi-gateway active/active
writes still require external coordination.

The exact consistency model is:

- Local writes are visible through staging immediately on the same gateway.
- Direct COS creates, updates, and deletes become visible after metadata/NFS
  listing cache expiry, or after a successful refresh scan plus any remaining
  NFS wrapper listing TTL.
- Clean cached object data is invalidated when the refresh scan sees a changed
  object list signature.
- Dirty staged files are never overwritten by refresh. When a dirty path also
  has an external COS change, the local staged bytes are preserved in
  `lost+found`, sync is blocked for the stale staged copy, and COS is the
  default winner for the original path.
- Deletions are detected after the scanner has seen the object in a previous
  scan; otherwise they converge through normal cache expiry and COS stat/list
  misses.
- User metadata changes are detected when COS list results expose a changed
  list signature, usually last-modified time for copy-to-self updates; otherwise
  they converge on metadata cache expiry.

### Multipart Upload

```yaml
performance:
  multipart_threshold_mb: 100
  multipart_chunk_mb: 10
  max_concurrent_writes: 25
```

Files larger than the threshold use multipart upload during background sync.
Multipart upload lifecycle is protected by per-object synchronization so
workers do not race the same object.

## Observability

Prometheus metrics are available when `server.metrics_enabled` is true:

```bash
curl http://127.0.0.1:8080/metrics
```

Important metrics include:

- `staging_used_bytes`
- `staging_available_bytes`
- `staging_pressure_level`
- `writes_blocked_total`
- `writes_rejected_total`
- `backpressure_wait_seconds`
- `sync_queue_bytes`
- `staging_sync_queue_depth`
- `staging_sync_queue_bytes`
- `staging_cos_visibility_latency_seconds`
- `staging_upload_duration_seconds`
- `staging_upload_throughput_mib_per_second`
- `staging_conflict_count`
- `staging_conflicts_total`
- `staging_last_conflict_timestamp_seconds`
- `cache_hits_total`
- `cache_misses_total`
- `object_refresh_scans_total`
- `object_refresh_duration_seconds`
- `object_refresh_objects_changed_total`
- `object_refresh_cache_invalidations_total`
- `object_refresh_skipped_dirty_paths_total`
- `object_refresh_conflicts_total`
- `filesystem_requests_total` (labels `protocol`, `operation`, `status`)
- `filesystem_request_duration_seconds` (labels `protocol`, `operation`)
- `nfs_requests_total` (deprecated: the same requests without a `protocol`
  label; use `filesystem_requests_total`)
- `cos_api_calls_total`
- `smb_requests_total` (labels `command`, `status`), and
  `smb_request_duration_seconds` (label `command`): one entry per SMB2
  command, so a compound request counts once per command in it, and `status`
  is the NT status the client saw, such as `STATUS_SHARING_VIOLATION`
- `smb_connections`, `smb_sessions`, `smb_open_files`
- `cos_bucket_quota_exceeded`: 1 while the bucket refuses writes for its
  hard quota
- `smb_leases_granted_total` (labels `kind`: lease or oplock, `state`),
  `smb_lease_breaks_total` (labels `kind`, `from`, `to`), and
  `smb_lease_break_timeouts_total`: client caching granted, and taken back
  when files change
- `smb_connections_refused_total` (label `reason`: which limit refused them)
- `smb_auth_failures_total`, `smb_auth_blocked_total`,
  `smb_auth_blocked_clients`

Health endpoints are available when `server.health_enabled` is true:

```bash
curl http://127.0.0.1:8081/health/live
curl http://127.0.0.1:8081/health/ready
curl http://127.0.0.1:8081/health
```

`/health` reports a check per subsystem. The `smb` check is healthy while the
server is serving, and reports its connection, session, and open-file counts;
it is healthy and says so when SMB is disabled, and unhealthy if the server
stopped serving while the gateway kept running.

Debug endpoints are available when `server.debug_enabled` is true:

```bash
curl http://127.0.0.1:8082/debug/staging/sync
curl http://127.0.0.1:8082/debug/perf
```

Use `/debug/staging/sync` to check dirty files, sync queue depth, queue bytes,
staging pressure, conflict count, conflicted paths, last conflict time, last
sync timing, and upload throughput.

## Benchmarking

The benchmark suite is in `scripts/benchmark_suite.py` and
`scripts/run_benchmark_suite.sh`. It writes timestamped results under
`benchmark-results/`.

Run a standard profile against a running and mounted gateway:

```bash
PROFILE=standard ./scripts/run_benchmark_suite.sh
```

Run selected categories:

```bash
./scripts/run_benchmark_suite.sh --categories frontend-write sync read
```

Backpressure and crash-safety tests are opt-in because they intentionally stress
staging capacity or kill the gateway:

```bash
./scripts/run_benchmark_suite.sh \
  --categories backpressure \
  --allow-backpressure
```

```bash
./scripts/run_benchmark_suite.sh \
  --categories crash-safety \
  --allow-crash \
  --gateway-command 'cd ~/bluestone && sudo nohup ./bin/bluestone --config configs/config.yaml >/tmp/bluestone-benchmark.log 2>&1 &' \
  --post-restart-command 'sudo umount /mnt/cos-nfs -f || true; sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos-nfs'
```

Each benchmark run produces:

- `SUMMARY.md`
- `results.json`
- `results.csv`
- `baseline.json`
- `environment.json`
- `monitor_samples.csv`
- raw fio output when fio-backed tests are used

See `docs/BENCHMARK_SUITE.md` for the benchmark categories and output format.

## Docker And Kubernetes

Docker and Kubernetes manifests are provided under `deployments/`.

Build the image:

```bash
docker build -t cos-bluestone -f deployments/docker/Dockerfile .
```

Run with Docker Compose:

```bash
cd deployments/docker
COS_ENDPOINT=... COS_BUCKET=... IBM_CLOUD_API_KEY=... docker compose up -d
```

The optional monitoring profile starts Prometheus and Grafana. Grafana requires
`GRAFANA_PASSWORD` to be set before enabling that profile.

Kubernetes manifests are examples and should be reviewed for your cluster,
secret management, storage, network policy, and operational requirements before
use.

## Operational Notes

- Keep staging and cache paths outside ephemeral directories for real workloads.
- Size staging for the largest expected unsynced dirty working set.
- Monitor `sync_queue_bytes`, `staging_used_bytes`, and upload latency.
- Treat a growing sync queue as a durability delay, not just a performance
  issue.
- Use private COS endpoints or private networking where possible.
- Keep COS credentials out of source control.
- Restrict access to the NFS port at the host or network layer.
- Prefer benchmark validation on the same VM shape, disk type, COS region, and
  mount options used in deployment.

## Troubleshooting

Gateway fails to start:

```bash
sudo ss -tlnp | grep 2049
sudo ./bin/bluestone --config configs/config.yaml
```

Mount fails:

```bash
sudo umount /mnt/cos-nfs -f
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos-nfs
```

Writes succeed but objects are not visible in COS yet:

```bash
curl http://127.0.0.1:8082/debug/staging/sync
curl http://127.0.0.1:8080/metrics | grep -E 'sync_queue|staging_|writes_'
```

Remember that accepted writes are staged locally first. Check whether dirty
files or queue bytes are still present before concluding that COS has the final
object.

Backpressure rejects or blocks writes:

```bash
df -h /var/staging/bluestone
curl http://127.0.0.1:8082/debug/staging/sync
curl http://127.0.0.1:8080/metrics | grep -E 'staging_pressure|writes_blocked|writes_rejected|backpressure'
```

## Development

```bash
make build
make test
make benchmark-suite
```

Useful local documentation:

- `docs/BENCHMARK_SUITE.md`
- `docs/BENCHMARKING.md`
- `ARCHITECTURE.md`
- `docs/AWS_S3_FILES_COMPARISON.md`
- `docs/STAGING_ARCHITECTURE.md`

## License

This project is licensed under the MIT License. See `LICENSE` for details.

## Support

There is no official support channel. Issues and pull requests are handled on a
best-effort basis.
