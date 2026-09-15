# Linux Service Installation

This guide installs Bluestone as a `systemd` service for a
Linux server operator.

## What The Installer Creates

- `/usr/local/bin/bluestone`
- `/etc/bluestone/config.yaml`
- `/etc/default/bluestone`
- `/etc/systemd/system/bluestone.service`
- `bluestone` system user and group
- `/var/cache/bluestone`
- `/var/staging/bluestone`
- `/var/log/bluestone`

The service runs as the unprivileged `bluestone` user and receives only
`CAP_NET_BIND_SERVICE`, which lets it bind the default NFS port `2049`.

## Install From Source

```bash
git clone https://github.com/oborges/bluestone.git
cd bluestone
sudo ./scripts/install-linux-service.sh --build
```

To install a prebuilt binary instead:

```bash
sudo ./scripts/install-linux-service.sh --binary /path/to/bluestone
```

The installer does not overwrite an existing `/etc/bluestone/config.yaml`
unless `--force-config` is passed.

## Configure

Edit the service config:

```bash
sudoedit /etc/bluestone/config.yaml
```

Set at least:

```yaml
cos:
  endpoint: "s3.us-south.cloud-object-storage.appdomain.cloud"
  bucket: "my-nfs-bucket"
  region: "us-south"
  auth_type: "iam"
  api_key: "your-ibm-cloud-api-key"
```

Secrets may also be placed in `/etc/default/bluestone`:

```bash
BLUESTONE_COS_API_KEY=your-ibm-cloud-api-key
```

The installer sets config and environment file permissions to `0640` with group
`bluestone`.

## Start And Inspect

```bash
sudo systemctl enable --now bluestone
sudo systemctl status bluestone
sudo journalctl -u bluestone -f
```

To install and start in one command:

```bash
sudo ./scripts/install-linux-service.sh --build --enable --start
```

## Mount From The Gateway Host

```bash
sudo mkdir -p /mnt/cos-nfs
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos-nfs
```

## Custom Cache Or Staging Paths

The service unit uses `ProtectSystem=strict`, so only the default writable paths
are open:

- `/var/cache/bluestone`
- `/var/staging/bluestone`
- `/var/log/bluestone`

If you move `cache.data.path`, `staging.root_dir`, or file logging to another
directory, add a systemd drop-in:

```bash
sudo systemctl edit bluestone
```

Example:

```ini
[Service]
ReadWritePaths=/mnt/bluestone-cache /mnt/bluestone-staging
```

Then create and assign the directories:

```bash
sudo install -d -m 0750 -o bluestone -g bluestone /mnt/bluestone-cache
sudo install -d -m 0750 -o bluestone -g bluestone /mnt/bluestone-staging
sudo systemctl daemon-reload
sudo systemctl restart bluestone
```

## Upgrade

From a fresh checkout or release directory:

```bash
sudo ./scripts/install-linux-service.sh --build
sudo systemctl restart bluestone
```

Existing config and environment files are preserved by default.

## Migrating From nfs-gateway

Installations made before the rename use the `nfs-gateway` service, user,
and paths. The installer refuses to install `bluestone.service` next to a
legacy `nfs-gateway.service`, because both would serve the same port and
bucket.

Moving the staging directory is required, not cosmetic: it holds accepted
writes that may not have synced to COS yet, pending delete tombstones, and
the HA holder marker. The new unit only allows writes under
`/var/staging/bluestone` (`ProtectSystem=strict`).

1. Stop the legacy service. A graceful stop releases the HA lease; staged
   data stays on disk and resumes syncing after the move.

   ```bash
   sudo systemctl disable --now nfs-gateway
   ```

2. Move configuration and state to the new paths, and point the config at
   them. The read cache is disposable and can be left behind.

   ```bash
   sudo mv /etc/nfs-gateway /etc/bluestone
   sudo mv /etc/default/nfs-gateway /etc/default/bluestone
   sudo mv /var/staging/nfs-gateway /var/staging/bluestone
   sudo sed -i 's|/var/staging/nfs-gateway|/var/staging/bluestone|; s|/var/cache/nfs-gateway|/var/cache/bluestone|' /etc/bluestone/config.yaml
   sudo sed -i 's/NFS_GATEWAY_/BLUESTONE_/g' /etc/default/bluestone
   ```

3. Remove the legacy unit and binary.

   ```bash
   sudo rm /etc/systemd/system/nfs-gateway.service /usr/local/bin/nfs-gateway
   sudo systemctl daemon-reload
   ```

4. Install Bluestone, hand the moved state to the new service user, and start.

   ```bash
   sudo ./scripts/install-linux-service.sh --build --enable
   sudo chown -R bluestone:bluestone /var/staging/bluestone
   sudo chgrp -R bluestone /etc/bluestone /etc/default/bluestone
   sudo systemctl start bluestone
   sudo journalctl -u bluestone -f
   ```

   Confirm the log shows the staged files being recovered and no deprecation
   notices remain.

5. Optionally remove the old account and cache: `sudo userdel nfs-gateway`
   and `sudo rm -rf /var/cache/nfs-gateway`.

For HA pairs, migrate the standby first, then fail over and migrate the old
primary. The lease object keeps its original name, so gateways from before
and after the rename still fence each other. Update the replication units
and their rsync paths to the new names on both nodes.
