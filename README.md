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
  concurrent_requests: 0 # reads/writes at once per connection; 0 = default (64), 1 = serial
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
      ntlm_hash: "8846f7eaee8fb117ad06bdd830b7586c"
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
`smb.share_name`, `smb.domain`, and `smb.encryption_required` can be
overridden with `BLUESTONE_SMB_*` environment variables; users are read from
the file only.

Tested against Windows Server 2025 (SMB 3.0.2 with signing), macOS
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

No oplocks or leases are granted, so clients do not cache file contents
locally and write through to the gateway. That keeps SMB clients consistent
with NFS clients and with changes made directly in the bucket, at the cost of
some client-side caching performance. Also not supported yet: alternate data
streams and security descriptors. Signing uses AES-CMAC and encryption
AES-128-CCM.

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
