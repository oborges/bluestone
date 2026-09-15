# High Availability (Active/Passive)

The gateway supports an active/passive pair fenced by a lease object in the
COS bucket. Exactly one gateway serves the bucket at a time; the lease makes
violations fail loudly instead of corrupting write-back state.

Verified by a live failover drill under two-client soak load: primary killed
with `SIGKILL` (no lease release), early promotion fenced, automatic
promotion 49s after the kill (lease timeout), 25/25 file checksums intact
via the standby — including files that were dirty (unsynced) at the moment of
the kill — and the dead primary refused re-entry until the standby stepped
down.

## How fencing works

- The active gateway writes `.nfs-gateway.lease` (hidden from the NFS
  namespace) and renews it every `ha.heartbeat_interval` (default 15s).
  The key predates the Bluestone rename and is intentionally unchanged, so
  gateways from before and after the rename contend for the same lease.
- A gateway starting against a bucket with a *fresh* foreign lease exits
  fatally. Fresh means renewed within `ha.lease_timeout` (default 60s).
- A *stale* lease (holder crashed) is taken over automatically, incrementing
  the lease epoch.
- Graceful shutdown deletes the lease, so planned failover is immediate.
- Crash recovery on the same node works during a COS outage via a local
  holder marker in the staging root; a standby that never held the lease
  cannot promote blind while COS is unreachable.
- Break-glass: `BLUESTONE_HA_FORCE_TAKEOVER=true` (or
  `ha-promote.sh --force`) steals a fresh lease. Only when the holder is
  confirmed dead.

## Configuration (both nodes)

```yaml
ha:
  enabled: true
  heartbeat_interval: "15s"
  lease_timeout: "60s"   # crash-failover RTO is dominated by this value
```

`lease_timeout` must be more than twice `heartbeat_interval`.

## Standby setup

1. Install the gateway and the same `/etc/bluestone/config.yaml` (same
   bucket, credentials, `ha.enabled: true`). Keep `bluestone.service`
   disabled and stopped.
2. Replicate the primary's staging directory continuously; it is the durable
   record of accepted-but-unsynced writes and pending deletes, and its format
   is crash-consistent (safe to copy live). Example systemd units:

```ini
# /etc/systemd/system/bluestone-replicate.service
[Unit]
Description=Pull Bluestone staging state from the primary
[Service]
Type=oneshot
SuccessExitStatus=24
ExecStart=/usr/bin/rsync -a --delete --timeout=20 \
  --exclude=ha-holder-marker \
  -e "ssh -i /root/.ssh/id_ed25519" --rsync-path="sudo rsync" \
  vpcuser@PRIMARY_IP:/var/staging/bluestone/ /var/staging/bluestone/
ExecStartPost=/usr/bin/chown -R bluestone:bluestone /var/staging/bluestone

# /etc/systemd/system/bluestone-replicate.timer
[Timer]
OnBootSec=30
OnUnitActiveSec=15
```

`--exclude=ha-holder-marker` is required: the marker must never move
between nodes. `SuccessExitStatus=24` tolerates files vanishing mid-copy
under live churn. The replication interval bounds the failover RPO for data
that has not yet synced to COS (data already in COS is never at risk).

## Failover

```
standby# ha-promote.sh          # refuses while the primary's lease is fresh
standby# ha-promote.sh          # succeeds once stale (crash) or immediately
                                # after a graceful primary shutdown
client#  umount -l /mnt/cos-nfs && mount -t nfs4 -o vers=4.0 STANDBY_IP:/ /mnt/cos-nfs
```

In production, front the gateway with a DNS name (low TTL) and update it in
the promotion step so clients remount to a stable name.

## Failback

```
standby# systemctl disable --now bluestone        # releases the lease
standby# systemctl enable --now bluestone-replicate.timer
primary# systemctl start bluestone                # acquires immediately
client#  remount to the primary
```

## RPO / RTO

- RTO (crash): `ha.lease_timeout` plus a few seconds of promotion (measured
  49s with the defaults). Planned failover: seconds.
- RPO: zero for anything synced to COS; up to one replication interval
  (15s in the example) for staged-but-unsynced writes. Writes acknowledged
  in the final seconds before a crash may need the replication cycle to
  have run; applications requiring zero RPO should fsync-and-verify or wait
  for sync-visibility before depending on the data.

# Made with Bob
