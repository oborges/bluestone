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

Re-run on 2026-09-20 against a two-node pair to exercise runtime fencing as
well (`ha.on_lease_lost: "stop"`):

- A standby forcing a takeover while the primary was healthy fenced the
  primary about 5s later: it logged the loss, stopped without draining, and
  exited 3. It stopped listening on 2049 and 445, and each restart was
  refused because the standby held the lease.
- `SIGKILL` on the primary: promotion refused while the lease was fresh,
  granted 63s after the kill, and 25/25 checksums verified through the
  standby. The dead primary refused re-entry while the standby served.
- Writes issued seconds before the kill, before they had synced to COS or
  been replicated, were lost: the files kept their previous contents. That
  is the documented RPO, and the reason the RPO section below says to wait
  for sync before depending on data.

## How fencing works

- The active gateway writes `.nfs-gateway.lease` (hidden from the NFS
  namespace) and renews it every `ha.heartbeat_interval` (default 15s).
  The key predates the Bluestone rename and is intentionally unchanged, so
  gateways from before and after the rename contend for the same lease.
- A gateway starting against a bucket with a *fresh* foreign lease exits
  fatally. Fresh means renewed within `ha.lease_timeout` (default 60s).
- A *stale* lease (holder crashed) is taken over automatically, incrementing
  the lease epoch.
- An active gateway that loses the lease stops serving and exits 3. It loses
  the lease when another gateway holds it (a takeover, forced or after this
  one went silent), or when the lease cannot be renewed for longer than
  `ha.lease_timeout`, after which a standby is entitled to promote. It stops
  without draining, since finishing a client's copy would mean writing to a
  bucket it no longer holds, and it does not delete the lease: that belongs
  to the gateway that took it. A supervisor restarting the process is
  harmless, because a gateway that finds a fresh foreign lease at startup
  refuses to serve.
- `ha.on_lease_lost: "warn"` keeps the older behaviour of logging and
  serving on. It risks two gateways writing one bucket, which is what the
  lease exists to prevent, and is only sensible while diagnosing the lease
  itself.
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
  on_lease_lost: "stop"  # stop serving and exit 3; "warn" logs and serves on
```

Exit status 3 means the gateway stopped because it lost the lease, as
opposed to a clean stop (0) or a failure to start (1).

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

The gateway must run as the user that owns the staging directory. The unit
in `deployments/systemd/` runs as `bluestone`, which is why the replication
unit and `ha-promote.sh` chown the staged state to `bluestone`. A unit
edited to run as root instead will fail to read that state: the unit keeps
only `CAP_NET_BIND_SERVICE`, so root has no `CAP_DAC_OVERRIDE` and is
refused like any other user. The symptom is a gateway that serves reads from
COS but fails every write with a permission error.

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

Replication is one-way, so anything the standby staged while it was active
exists only on the standby. Re-enabling replication runs `rsync --delete`
against the primary's staging and deletes exactly those files, so the
standby must finish syncing them to COS before it steps down.
`ha-stepdown.sh` waits for that, then stops the gateway and resumes
replication:

```
standby# ha-stepdown.sh                           # drains, then releases the lease
primary# systemctl start bluestone                # acquires immediately
client#  remount to the primary
```

Stepping down by hand loses those writes unless the staging area is drained
first:

```
standby# find /var/staging/bluestone/active -name '*.data' | wc -l   # must be 0
standby# systemctl disable --now bluestone        # releases the lease
standby# systemctl enable --now bluestone-replicate.timer
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
