# IBM Power: Linux and AIX

Bluestone builds and runs on IBM Power, as a gateway on Linux and on AIX,
and serves AIX clients as well as Linux ones. This page says what was tested
and what an AIX host needs.

## What was tested

On IBM Power Virtual Server (S1022, shared processors) with RHEL 9.8
(ppc64le) and AIX 7.3 TL4, against a COS bucket in the same region. Each
cell is a 22-check file test: create, a 20 MiB copy read back and
checksummed, rename, chmod, truncate, overwrite in place, exclusive create,
setting a modification time, listing 200 files, nested directories and
removal.

| Gateway        | Client | NFSv4 | NFSv3 | SMB 3.1.1  |
| -------------- | ------ | ----- | ----- | ---------- |
| RHEL (ppc64le) | RHEL   | 22/22 | 22/22 | 20/22      |
| RHEL (ppc64le) | AIX    | 22/22 | 22/22 | not tested |
| AIX (ppc64)    | RHEL   | 22/22 | 22/22 | 20/22      |
| AIX (ppc64)    | AIX    | 22/22 | 22/22 | not tested |

The two SMB failures are the same on both gateways: `chmod` through a Linux
CIFS mount changes nothing, and renaming a directory whose files have not
yet synced to COS answers "resource busy" until they have.

Not tested on Power: SMB from an AIX client (the SMB client fileset is not
in the base AIX image), byte-range locks, load, long runs, HA, and the
systemd service installer.

## Building

`make build-all` writes `bin/bluestone-linux-ppc64le` and
`bin/bluestone-aix-ppc64` beside the other platforms. For one of them:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=ppc64le go build -o bin/bluestone-linux-ppc64le ./cmd/bluestone
CGO_ENABLED=0 GOOS=aix GOARCH=ppc64 go build -o bin/bluestone-aix-ppc64 ./cmd/bluestone
```

The gateway is pure Go and needs nothing from the host but a filesystem for
staging and cache.

## Running the gateway on AIX

There is no service installer for AIX: `scripts/install-linux-service.sh`
is for systemd. Start the binary with `--config`, under the System Resource
Controller or however the host runs its daemons.

- Give staging and cache a filesystem of their own. The root volume group
  of a stock image has little free space.
- Stop the host's own NFS server (`nfsd`, `rpc.mountd`) if it runs: the
  gateway listens on the same port, 2049.
- Startup takes several seconds longer than on Linux.

## AIX clients

### NFSv4

An AIX client needs its NFSv4 domain set and the registry daemon running
before it mounts anything over NFSv4, and resolves the server by name:

```sh
chnfsdom localdomain
startsrc -s nfsrgyd
echo "192.168.77.10 bluestone-gw" >> /etc/hosts
mount -o vers=4,proto=tcp,port=2049 bluestone-gw:/ /mnt/cos
```

Without them the mount fails with "not in hosts database" or "Verify the
NFS local domain has been set, and the nfsrgyd process is running".

### NFSv3

AIX's NFSv3 mount has no `mountport` option. It asks the gateway host's
portmapper where the MOUNT program listens, so the gateway has to be
registered there:

```yaml
server:
  nfs_version: "dual" # or "3"
  nfs_register_portmap: true
```

The host must run a portmapper (`rpcbind` on Linux, `portmap` on AIX). The
client then mounts without naming a port:

```sh
mount -o vers=3,proto=tcp bluestone-gw:/ /mnt/cos
```

Leave `nfs_register_portmap` off on a host that runs its own NFS server:
the gateway's registration would replace that server's.

### After a gateway restart

File handles do not survive a restart of the gateway, on any platform. An
AIX client keeps its old mount in place and every call on it then fails with
"Missing file or filesystem". Unmount it (`umount -f`) and mount again.
