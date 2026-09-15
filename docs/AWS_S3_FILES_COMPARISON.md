# Architecture Comparison: Bluestone And AWS S3 Files

This document compares this gateway with AWS's newer S3 file-access direction,
represented in public AWS documentation by Mountpoint for Amazon S3. It does
not use Amazon S3 File Gateway as the target model; S3 File Gateway is the older
AWS Storage Gateway appliance/service pattern.

The comparison is architectural, not a compatibility claim. This project is an
unofficial community IBM Cloud Object Storage NFS gateway. It is not an IBM
product, is not endorsed by IBM, and is not a fully managed service. Operators
own deployment, upgrades, local staging durability, monitoring, access control,
and recovery.

## Classification

| Classification | Meaning |
| --- | --- |
| equivalent | The behavior is broadly the same at the architectural contract level. |
| similar but self-managed | The gateway provides a comparable behavior, but the operator owns configuration, capacity, monitoring, and failure handling. |
| intentionally different | The gateway deliberately does something different from AWS S3 Files or does not try to hide that it is self-operated software. |
| missing gap | The behavior is incomplete or lacks a feature, guardrail, or automation that users might expect from AWS S3 Files. |
| not planned | The behavior is outside the current project direction unless a future design explicitly changes that. |

## Summary Matrix

| Area | Classification | Current gateway behavior | Comparison with AWS S3 Files |
| --- | --- | --- | --- |
| NFS access | intentionally different | Serves one NFSv4 export backed by one IBM COS bucket by default, with optional NFSv3 compatibility. Linux clients mount the gateway with standard NFS tooling. | AWS's newer S3 file-access model is a Linux client that mounts S3 locally and translates file operations to S3 API calls. This gateway intentionally presents a network NFS service so multiple Linux clients can use the same export through the gateway. |
| Client mount model | intentionally different | Applications talk to an NFS mount. The gateway process owns COS credentials and object translation. | Mountpoint for Amazon S3 runs on each Linux host that mounts a bucket. This project centralizes the bucket mount behind an NFS server instead of requiring every client host to run an S3-aware FUSE-style client. |
| Staging/write-back | intentionally different | Accepted writes land in local staging first. Dirty files are synced to COS asynchronously by background workers. Backpressure protects the local staging filesystem. | Mountpoint focuses on basic high-throughput file access to S3 and can create new files, but AWS documents that it cannot modify existing files. This gateway is competing by offering mutable NFS-shaped write-back behavior over IBM COS, with the honest cost that local staging is durable state until sync completes. |
| Object durability | similar but self-managed | Once a synced object is visible in COS with the expected content, durability belongs to IBM COS. Before sync completes, the accepted-write record is local staging. | AWS S3 Files ultimately stores objects in S3. This gateway follows the same object-store durability destination, but has a larger self-managed window where unsynced accepted writes depend on local staging. |
| Lazy reads | equivalent | Cold reads fetch from COS using full-object or range reads. Warm reads can hit local chunk cache. Sequential reads can use read-ahead, parallel range fetches, and singleflight deduplication. | Mountpoint translates file reads into S3 object API calls and supports read caching. The architectural idea is equivalent: avoid preloading whole buckets and fetch object data as files are accessed. |
| Object-to-file sync | similar but self-managed | Existing COS objects appear through prefix listings and metadata reads. Directory and metadata cache entries expire by TTL and are invalidated by local mutations. | AWS S3 Files interprets object keys as filesystem paths. This gateway does the same basic mapping, but the metadata/listing cache, invalidation behavior, and refresh expectations are local implementation details. |
| File-to-object sync | similar but self-managed | Dirty staged files are uploaded to COS by sync workers based on size, age, idle state, periodic scans, and backpressure pressure. Large files use multipart upload. | AWS S3 Files writes through to S3 objects within Mountpoint's supported write model. This gateway adds asynchronous write-back and queue visibility because it supports mutable NFS workflows that can outpace object upload. |
| Conflict handling | missing gap | The supported consistency model is one gateway instance owning one mounted export. Active/active multi-gateway writes to the same bucket require external coordination. Direct COS writes can be discovered later through listings/cache expiry, but there is no conflict detector or merge policy. | AWS S3 Files keeps semantics constrained and expects applications to tolerate S3/object-style behavior. This gateway exposes more mutable file behavior, so conflict detection and multi-writer coordination are a real gap. |
| Rename semantics | similar but self-managed | File rename is implemented as COS copy to the new key followed by delete of the old key, with cache invalidation. It is not equivalent to atomic local-filesystem rename across every failure mode. Directory trees are not a native object-store construct. | AWS S3 Files does not aim to be a full POSIX filesystem. Both approaches must respect that object keys are not local filesystem inodes. This gateway should document rename as object-copy semantics, not a local atomic rename contract. |
| Metadata and permissions | intentionally different | The gateway stores POSIX-like mode, UID, GID, and timestamps in object metadata. Defaults are applied when metadata is absent. `chmod`, `chown`, and timestamp changes update metadata or rewrite/copy objects depending on path state. | AWS documents Mountpoint as unsuitable for workloads that need full POSIX-style permissions. This gateway intentionally preserves a small POSIX-like metadata layer because NFS clients expect it, but it is still object metadata, not a complete POSIX authority model. |
| POSIX completeness | intentionally different | This is a filesystem-shaped interface over object storage, not a complete POSIX filesystem. Symlinks are not supported, hard links are not native object-store constructs, and path-based handles are not persistent inode allocation from COS. | AWS S3 Files also explicitly does not implement full POSIX semantics and does not support features such as symbolic links or file locking. The shared product truth is that object-backed file access must keep POSIX claims narrow. |
| Observability | similar but self-managed | Prometheus metrics, health endpoints, debug endpoints, structured logs, and benchmark tooling are provided when enabled. Key signals include dirty bytes, sync queue depth, upload latency, backpressure, cache hits/misses, NFS requests, and COS API calls. | AWS S3 Files benefits from AWS ecosystem integration around S3, IAM, metrics, logs, and client tooling. This gateway exposes raw operational signals, but operators must host, scrape, alert, and retain them. |
| Security | similar but self-managed | COS access uses configured IBM COS IAM API key or HMAC credentials. COS API traffic uses HTTPS. NFS traffic is plain NFS and should stay on trusted networks. The gateway does not implement NFS user authentication. | AWS S3 Files uses AWS credentials/IAM on the host running the mount. This gateway centralizes COS credentials in the gateway and leaves NFS access control to trusted networks, host firewalls, VPC controls, Kubernetes policy, or equivalent infrastructure. |
| High availability | missing gap | The recommended shape is one gateway instance owning one mounted export, with reliable local or attached disk for staging. Restart recovery can rebuild dirty staging state from disk. | AWS's client-side S3 file-access model avoids a shared NFS gateway as a central server, but each client host still owns its mount behavior. This gateway needs operator-designed HA around the NFS service, staging disk, failover, and client remount behavior. |
| Disaster recovery | missing gap | Synced objects are recoverable from COS according to the bucket's durability, versioning, replication, and backup configuration. Unsynced accepted writes depend on preserving the local staging directory. Restart recovery handles local process crashes when staging survives. | AWS S3 Files keeps durable state in S3 within its supported write model. This gateway must preserve local dirty staging across host loss, disk loss, redeploy, and credential rotation scenarios. |
| Operational ownership | intentionally different | The operator owns binaries, config, COS credentials, staging/cache disks, network exposure, metrics, alerting, logs, benchmarks, upgrades, and incident response. | AWS S3 Files is AWS-native client software for S3. This project competes by bringing similar object-backed file access to IBM COS through self-managed NFS, not by becoming an AWS-managed equivalent. |
| Managed-service parity | not planned | The project does not try to become an IBM managed service or present itself as an official IBM product. | Use AWS S3 Files when the requirement is AWS-native S3 file access. Use this gateway only when self-managed IBM COS-backed NFS behavior is acceptable and the operator is comfortable owning the gateway layer. |

## Competitive Position

The useful comparison is not "can this clone AWS's old File Gateway?" It should
be "can this make IBM COS feel useful to Linux file workloads in the same broad
problem space that AWS is now addressing for S3?"

This gateway's competitive bet is different from AWS's newer client-side model:

- provide a standard NFS export instead of requiring each application host to
  run an S3-specific mount client.
- support mutable, write-back file workflows over object storage.
- make staging pressure, sync lag, and durability boundaries visible instead of
  pretending object storage is a local filesystem.
- stay honest that this is self-managed infrastructure, not an IBM product or a
  managed service.

## Behavioral Notes

### Write Acknowledgement Is Not COS Durability

For this gateway:

- `WRITE` accepted means local staging accepted the bytes.
- dirty staged data remains the local source of truth until upload completes.
- COS durability begins after the sync worker uploads the staged snapshot and
  the object is visible in COS.
- local staging must be treated as durable state, not disposable cache, while
  files are dirty.

That is the main operational cost of supporting mutable NFS-style writes over
object storage.

### One Gateway Owns The Export

The current supported consistency model is one gateway instance owning one
mounted export. Multiple readers are normal, but multiple independent gateway
instances writing the same bucket/prefix are not a supported active/active
mode. If a deployment needs multiple writers, add external coordination or use
a storage system designed for that consistency model.

### Object Store Semantics Leak Through

AWS S3 Files and this gateway both expose files while storing objects. That
means some file operations become object operations:

- file rename is copy then delete.
- metadata changes can require object metadata replacement or rewrite.
- append or partial modification can create new object versions if versioning is
  enabled.
- directories are represented by prefixes and optional marker objects, not by a
  native object-store directory inode.

This project documents those leaks because they matter for cost, failure modes,
and recovery.

## References

- Mount an Amazon S3 bucket as a local file system:
  <https://docs.aws.amazon.com/AmazonS3/latest/userguide/mountpoint.html>
- Configuring and using Mountpoint for Amazon S3:
  <https://docs.aws.amazon.com/AmazonS3/latest/userguide/mountpoint-usage.html>
