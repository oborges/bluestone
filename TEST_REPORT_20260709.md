# COS NFS Gateway Test Report - 2026-07-09

> Benchmark artifact directories referenced below (`remote-test-results-*`)
> are no longer tracked in the repository; they remain available in git
> history at commit `07a3352`.

Remote host: `vpc-backup-cos-demo` / RHEL 9.8  
Bucket: `backupvideofiles` on `s3.br-sao.cloud-object-storage.appdomain.cloud`  
Service: `nfs-gateway` active via systemd  
Mount: `/mnt/cos-nfs` mounted as NFSv4.0 from `localhost:/`  
Staging: `/dev/vdb` formatted XFS and mounted at `/var/staging/nfs-gateway-vdb`, restored to 450 GiB gateway quota after tests

## Setup And Health

| Test | Status | Notes |
|---|---:|---|
| Installed prerequisites | Pass | Installed `go`, `fio`, `git`, `bc`, build dependencies, NFS client tools already present |
| Prepared staging disk | Pass | `/dev/vdb` formatted XFS, mounted at `/var/staging/nfs-gateway-vdb`, ~497 GiB available |
| Built service binary | Pass | Built commit `753e4f873147b83266631484c13b5f4825972eee` |
| COS endpoint discovery | Pass | `us-south` returned 404; probing found bucket at `br-sao` |
| systemd service startup | Pass | `nfs-gateway.service` active and health endpoint returned `OK` |
| NFS mount | Pass | Mounted `/mnt/cos-nfs` with NFSv4.0 |
| Manual write/read/sync/delete smoke | Pass | Write/read succeeded, sync completed, delete succeeded after queue drained |

## Go Tests

| Test | Status | Notes |
|---|---:|---|
| Local `go test ./...` | Pass | All package tests passed locally |
| Remote `go test ./...` | Pass | All package tests passed on RHEL 9.8 |
| Remote `make build` | Pass | Produced `bin/nfs-gateway` |
| Remote `go test -race ./...` | Pass | Race-enabled package suite passed |

## Formal Benchmark Suite

Primary standard run artifacts: `remote-test-results-20260709/benchmark-20260709T213749Z/`

| Test | Status | KPI |
|---|---:|---|
| seq-write-100MiB-direct0 | Pass | 2083.33 MiB/s, p95 0.510 ms |
| seq-write-100MiB-direct1 | Pass | 46.00 MiB/s, p95 23.462 ms |
| seq-write-1GiB-direct0 | Pass | 823.15 MiB/s, p95 2.146 ms |
| seq-write-1GiB-direct1 | Pass | 40.54 MiB/s, p95 28.180 ms |
| sync-durable-100MiB | Pass | Sync latency 23.114 s |
| cold-read-from-COS | Pass | 14.77 MiB/s, p95 115.868 ms, cache hit ratio 0.892 |
| warm-read-local-cache | Pass | 45.18 MiB/s, p95 34.341 ms, cache hit ratio 0.969 |
| partial-range-read | Pass | 53.51 MiB/s, p95 28.180 ms, cache hit ratio 1.000 |
| random-4k-read | Pass | 99.90 IOPS, p95 13.435 ms, cache hit ratio 0.972 |
| large-sequential-read | Pass | 72.89 MiB/s, p95 64.750 ms, cache hit ratio 0.973 |
| small-files create/read/delete 100 | Fail | Reproduced `EIO` deleting a dirty staged file |
| small-files create/read/delete 1000 | Pass | Completed in 220.20 s, error count 0 |
| concurrent-readers-writers | Pass | Completed in 38.34 s, error count 0 |
| large-writes-plus-small-reads | Pass | Completed in 31.28 s, error count 0 |
| dirty-file-reads-during-sync | Pass | Completed in 1.54 s, error count 0 |

## Targeted Reruns

| Test | Status | Notes |
|---|---:|---|
| small-files create/read/delete 100 rerun | Fail | Reproduced with `EIO` on `file-000076.txt`; queue drained afterward |
| crash-before-sync | Pass | Recovery completed in 4.13 s |
| crash-during-sync | Pass | Recovery completed in 6.13 s |
| crash-during-multipart | Pass | Recovery completed in 14.21 s |
| backpressure below-high-watermark | Pass | Low-watermark test config, 26.42 MiB/s, rejected writes observed |
| backpressure above-high-watermark | Pass | Low-watermark test config, 225.84 MiB/s, rejected writes observed |
| backpressure above-critical-watermark | Pass | Low-watermark test config, 77.61 MiB/s, rejected writes observed |
| backpressure sync-drain-releases-pressure | Pass | Drain completed in 6.02 s |
| backpressure block-mode/fail-fast-mode suite cases | Skip | Suite marks these as mode-specific restart scenarios |

## Final State

| Check | Status | Notes |
|---|---:|---|
| Service health | Pass | `nfs-gateway.service` active |
| Config restored | Pass | Endpoint `br-sao`, staging quota 450 GiB, backpressure mode `block`, high/critical 80/95 |
| NFS mount | Pass | `/mnt/cos-nfs` mounted |
| Staging queue | Pass | `dirty_files=0`, `sync_queue_depth=0`, staging pressure normal |
| Secret scan of copied artifacts | Pass | No API key or secret env var found under `remote-test-results-20260709/` |

## Finding

The only reproducible failure is the 100-file small-file workload. The gateway accepts writes into local staging, and immediate delete of a still-dirty staged file is rejected as documented. Over NFS this surfaced to the benchmark as `EIO`. The 1000-file workload passed because enough time elapsed for sync to drain before deletes reached the same state. This looks like a benchmark/workflow mismatch with the gateway's write-back semantics, or a UX issue in how dirty-delete rejection maps back to NFS clients.

## Fix Verification (same day)

Root cause was a POSIX semantics violation, not a benchmark mismatch: `rm` of a just-written file must succeed under write-back caching. The dirty-delete rejection (`EBUSY` from `ensureNoDirtyStagedData`, surfacing as `EIO` over NFS) was replaced with crash-safe tombstone deletes: a durable tombstone is persisted before any destructive step, staged bytes are discarded, and the COS object is deleted (deferred to the sync worker when an upload is in flight, retried until confirmed). Pending-delete paths are hidden from `Stat`/`ReadDir`/`OpenFile`; recreating a path cancels its tombstone; recovery honors tombstones so accepted deletes never resurrect. Rename of dirty paths still returns `EBUSY` (tracked as follow-up).

Rerun artifacts: `remote-test-results-20260709/benchmark-20260709T223222Z/`

| Test | Status | KPI |
|---|---:|---|
| small-files create/read/delete 100 (was Fail) | Pass | 25.77 s, error count 0 |
| small-files create/read/delete 1000 | Pass | 225.43 s, error count 0 |
| write + immediate rm smoke over NFS | Pass | Succeeds while file is dirty |
| rm then recreate same path | Pass | Tombstone canceled, new content visible |
| post-run staging state | Pass | `dirty_files=0`, `sync_queue_depth=0`, `pending_deletes=0`, tombstone dir empty |

Unit coverage added: `internal/staging/tombstone_test.go` (immediate/deferred delete, crash recovery supersedes staged data, COS delete retry, sync skip, recreate cancel) and `internal/nfs/handler_test.go` (dirty delete succeeds, namespace hiding, recreate). Full suite plus `-race` passes locally and on RHEL 9.8; fixed binary deployed to `/usr/local/bin/nfs-gateway` and service restarted.

## Dirty Rename Fix Verification (2026-07-10)

Rename of a dirty staged file previously returned `EBUSY` (breaking the write-tmp-then-rename atomic-save pattern used by editors, rsync, and databases). Implemented write-back rename (`StagingManager.RenameStagedPath`): the staged bytes and dirty bookkeeping re-key to the destination (destination sidecar persisted before the byte move, source tombstone after it — no crash window loses accepted writes), the moved bytes sync to the destination key, and the source COS object is retired by its tombstone (inline when no upload is in flight, otherwise by the sync worker after the upload lands). Clean-source rename over a destination whose staged bytes are mid-upload stays `EBUSY`; directory renames with dirty children stay blocked.

Live verification also exposed and fixed a pre-existing staleness bug: the sync worker mutates COS objects without invalidating the operations-handler caches, so metadata cached during the dirty window (e.g. the zero-byte object created by the O_TRUNC immediate sync) outlived the sync and served empty reads. The worker now fires an object-mutated callback (wired to `InvalidateFileMutation`) after every upload and deferred delete.

Rerun artifacts: `remote-test-results-20260709/benchmark-20260710T005733Z/`

| Test | Status | KPI |
|---|---:|---|
| small-files create/read/delete 100 | Pass | 23.48 s, error count 0 |
| small-files create/read/delete 1000 | Pass | 221.07 s, error count 0 |
| mixed concurrent-readers-writers | Pass | 39.12 s, error count 0 |
| mixed large-writes-plus-small-reads | Pass | 30.72 s, error count 0 |
| mixed dirty-file-reads-during-sync | Pass | 4.15 s, error count 0 |
| atomic-write pattern (write tmp, mv over dirty target) | Pass | Correct content after mv, after sync, and from a cold remounted client |
| mv dirty file to new name | Pass | Source gone, destination correct, source object tombstoned |
| post-run staging state | Pass | `dirty_files=0`, `pending_deletes=0`, `sync_queue_depth=0` |

Unit coverage added: `internal/staging/rename_test.go` (staged re-key, in-flight sync claim preserved and deferred tombstone ordering, destination replacement, restart recovery, sync-to-new-key, object-mutated callback) and `internal/nfs/handler_test.go` (dirty rename succeeds with namespace checks, clean rename over dirty destination, syncing-destination busy). Full suite plus `-race` passes locally and on RHEL 9.8; binary deployed and service restarted.

## Read-Path Performance Fixes (2026-07-10)

CPU profiling of warm reads found the gateway allocation-bound (27% buffer zeroing, ~30% GC) and serialized. Three fixes:

1. **Warm reads no longer re-trigger read-ahead**: every read used to synchronously re-read ~8 MiB of already-cached chunks via `io.ReadAll`. Warm requests are now served with one ranged `pread` per cached chunk returning exactly the requested bytes (`readWarmChunks`, `DataCache.ReadChunkRange`); read-ahead engages only on cache miss and is clamped to the known object size (eliminating past-EOF 416 requests on small files).
2. **Concurrent NFS request handling**: `go-nfs` processed all requests on a connection serially, and Linux clients multiplex the whole mount over one TCP connection. Request bodies are now buffered up front and dispatched to a bounded per-connection handler pool (default 64). Replies are XID-matched (out-of-order is legal); NFSv4.0 clients serialize seqid-ordered state ops per owner (RFC 7530 §9.1.7). Oversized record-marking fragments (>16 MiB) are rejected.
3. **Exact-size buffers** replace `io.ReadAll` grow-loops in the data cache (~3x less CPU per byte, confirmed by re-profile).

Results (standard read benchmark, artifacts `remote-test-results-20260710-readfix/`):

| Test | Baseline 2026-07-09 | After fixes | Change |
|---|---:|---:|---:|
| random-4k-read | 99.9 IOPS, p95 13.4 ms | 4,370.6 IOPS, p95 0.34 ms | 44x |
| partial-range-read | 53.5 MiB/s | 247.1 MiB/s | 4.6x |
| large-sequential-read | 72.9 MiB/s | 209.1 MiB/s | 2.9x |
| cold-read-from-COS | 14.8 MiB/s | 57.1 MiB/s | 3.9x |
| warm-read-local-cache | 45.2 MiB/s | 49.6 MiB/s | +10% |

The remaining warm-read ceiling is environmental: fio measured the VM's volumes at ~75 MiB/s sequential / ~3,400 IOPS (direct=1), and the instance's storage bandwidth caps single-stream cache reads; page-cache-served patterns now reach 200+ MiB/s.

Regression: full standard `frontend-write`, `sync`, `small-files`, and `mixed` categories all pass with 0 errors after the changes (write throughput unchanged within variance). `go test -race` passes for all packages including the vendored `go-nfs`. Final state: service active, `dirty_files=0`, `pending_deletes=0`, `sync_queue_depth=0`, pressure normal.
