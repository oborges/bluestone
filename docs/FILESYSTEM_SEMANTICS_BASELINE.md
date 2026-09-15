# Filesystem Semantics Baseline

This baseline records the current filesystem semantics that future behavior
changes must preserve. It is intentionally scoped to the write-back staging,
read, sync, cache, COS, and deployment invariants in the current codebase.

## Invariants To Preserve

1. **Accepted NFS write means local staging accepted data, not COS durability.**
   With staging enabled, `vfs.File.Write` writes bytes to the path-scoped
   `WriteSession`, marks the path dirty, and returns from the local staging
   write path. It must not imply that COS already has the final object.

2. **COS durability happens after async sync and object visibility.**
   Dirty staged files become durable in COS only after a sync worker uploads the
   staged snapshot successfully and the object is visible to the COS/S3 API.
   Until then, dirty staging state is the accepted-write record.

3. **Dirty staged reads take priority over cache/COS.**
   When a staged session exists for a path, file reads, stat, and directory
   listings must surface the staged state before consulting metadata cache,
   data cache, or COS.

4. **Staging backpressure must prevent local disk exhaustion.**
   Writes must reserve projected staging growth before writing, block or reject
   according to configured watermarks, and report ENOSPC before the local
   staging filesystem is exhausted.

5. **Multipart uploads must remain per-object synchronized and retry/abort
   safely.** Sync attempts for the same logical object must be serialized.
   Failed or stale multipart attempts must abort when safe, retry from a clean
   upload session, and avoid completing a snapshot that changed during upload.

6. **Crash recovery must preserve unsynced staged data.**
   Staging files and metadata are durable local state until uploaded. On restart,
   the gateway must recover dirty staged files, keep them visible locally, and
   resume sync without relying on in-memory multipart state.

7. **Object-store rename/delete semantics must stay explicit.**
   Rename of a clean file is copy/delete over COS keys, not atomic
   local-filesystem rename. Directory rename is recursive prefix copy/delete
   when supported, and stays blocked while dirty staged children exist. Dirty
   staged files follow POSIX write-back semantics: delete discards the staged
   bytes behind a durable, crash-safe tombstone (COS object removed after any
   in-flight upload; never resurrects across restart), and rename re-keys the
   staged bytes and bookkeeping to the destination (destination sidecar
   persisted before the byte move, source tombstone after it, so no crash
   window loses accepted writes) with the moved bytes syncing to the
   destination key.

8. **One gateway owning one export remains the default supported deployment.**
   The supported consistency model is one gateway instance owning one mounted
   export. Active/active multi-gateway writes to the same bucket require
   external coordination and are not the baseline contract.

9. **IBM COS/S3-compatible portability must remain.**
   COS access must continue to use the IBM COS SDK through S3-compatible object,
   range, listing, metadata, and multipart APIs, with IAM and HMAC credentials
   supported by configuration.

## Current Test Coverage

| Invariant | Covered by current tests | Missing coverage |
| --- | --- | --- |
| Accepted NFS write means local staging accepted data, not COS durability | `internal/staging/session_test.go`: `TestWriteSession_Write`, `TestWriteSession_WriteAtOffset`, `TestWriteSession_MultipleWrites`; `internal/staging/metadata_test.go`: dirty-index tests; `internal/staging/sync_worker_test.go`: sync is exercised separately from staging writes. | No direct `vfs.File.Write` test proving a staging-enabled NFS write returns after local staging and does not call COS. No end-to-end NFS client test distinguishes write acknowledgement from COS durability. |
| COS durability after async sync and object visibility | `internal/staging/sync_worker_test.go`: `TestSyncWorker_SyncFile_Success`, `TestSyncWorker_SyncFile_Retry`, `TestSyncWorker_TriggerSync`, `TestSyncWorker_CleanupAfterSync`, `TestSyncWorker_MultipleFiles`; `internal/staging/sync_worker_multipart_test.go`: `TestIntegrationMultipartSync1GiBSucceeds`. | Tests use mocks and record upload completion, but do not verify real IBM COS/S3 object visibility, checksums, consistency delay, or debug/metrics durability signals against a live bucket. |
| Dirty staged reads before cache/COS | `internal/staging/session_test.go`: `TestWriteSession_Read`, `TestWriteSession_ReadPartial`; `internal/vfs/filesystem_test.go`: `TestFilesystemChrootPreservesRecoveredStagingSessions` covers recovered staged `Stat` and `ReadDir`; `internal/cache/data_test.go`: cache invalidation behavior. | No direct test that `vfs.File.Read` or `ReadAt` returns dirty staged bytes while stale data exists in cache/COS. No direct test for dirty staged directory entries overriding conflicting cached directory entries. |
| Backpressure prevents local disk exhaustion | `internal/staging/backpressure_test.go`: `TestStagingBackpressureBelowWatermarkSucceeds`, `TestStagingBackpressureAboveHighWatermarkBlocksThenFails`, `TestStagingBackpressureAboveCriticalWatermarkFailsEarly`, `TestStagingBackpressureSyncDrainReleasesPressure`. | No integration test that NFS WRITE maps pressure to client-visible ENOSPC. No filesystem-stat test for staging-aware available space. No test forces actual `statfs` low-disk conditions; current tests cover configured quota pressure. |
| Multipart per-object synchronization and safe retry/abort | `internal/staging/sync_worker_multipart_test.go`: `TestMultipartConcurrentSyncAttemptsDoNotRace`, `TestMultipartNoSuchUploadRestartsCleanly`, `TestMultipartSnapshotChangeBeforeCompleteKeepsDirtyAndDoesNotPublishPartialObject`, `TestMultipartChangedAfterCompleteKeepsDirtyForRetry`, `TestMultipartCleanupDoesNotAbortActiveUpload`, `TestMultipartRestartDuringUploadRestartsCleanly`, plus `TestIntegrationMultipartSync1GiBSucceeds`. | No live COS multipart integration test. No test for abort failures beyond mock error paths. No test of multipart limits against provider-specific edge cases such as max part count or minimum non-final part size. |
| Crash recovery preserves unsynced staged data | `internal/staging/manager_test.go`: `TestStagingManager_RecoverFromDisk`; `internal/vfs/filesystem_test.go`: `TestFilesystemChrootPreservesRecoveredStagingSessions`; `internal/staging/sync_worker_test.go`: `TestSyncWorker_OrphanDirtyWithStagingIsRecoveredAndSynced`, `TestSyncWorker_OrphanDirtyWithoutStagingIsForgotten`; `internal/staging/sync_worker_multipart_test.go`: `TestMultipartRestartDuringUploadRestartsCleanly`. | No process-level crash/kill test in `go test`. No test for torn or corrupt staging metadata. No test for crash during an in-flight monolithic upload with a still-dirty staged file. |
| Object-store rename/delete semantics stay explicit | `internal/posix/operations_test.go`: file rename, directory rename, copy failure, and delete failure coverage; `internal/vfs/filesystem_test.go`: dirty staged file delete and rename success, rename-over-dirty-destination, syncing-destination busy, pending-delete namespace hiding, recreate-cancels-tombstone, dirty child directory blocks; `internal/staging/tombstone_test.go`: immediate/deferred delete, crash-safe tombstone recovery, COS delete retry, sync skip, recreate cancel; `internal/staging/rename_test.go`: staged re-key, in-flight sync claim preserved, destination replacement, restart recovery, sync-to-new-key, object-mutation cache callback. | No live COS test for provider-specific copy/delete acknowledgement edge cases, versioned buckets, lifecycle rules, or object-lock behavior. |
| One gateway owning one export default | Documented in `ARCHITECTURE.md` deployment and consistency sections. | No automated test can currently enforce deployment topology. No guardrail test or configuration validation rejects active/active multi-gateway ownership. |
| IBM COS/S3-compatible portability | Code path is in `internal/cos/client.go` and `internal/cos/multipart.go`, using S3-compatible APIs and IAM/HMAC configuration. | No `internal/cos` unit tests currently cover client configuration, auth mode selection, S3-compatible endpoint behavior, metadata portability, or multipart API request construction. |

## Baseline Review Notes

- Existing README and architecture docs already describe the main write-back
  contract; this file is the compact test-and-invariant checklist for future
  changes.
- The strongest automated coverage today is in staging backpressure, sync
  worker recovery, and multipart lifecycle tests.
- The largest gaps are end-to-end NFS behavior, live COS visibility semantics,
  direct dirty-read precedence over stale cache/COS, and deployment/portability
  guardrails.
