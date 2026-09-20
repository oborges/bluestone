# SMB interoperability checks

Scripts for exercising a running Bluestone SMB share from real clients. They
were used to validate the server against Windows Server 2025, macOS, the Linux
kernel SMB client, and `smbclient`.

Each script reads the share password rather than taking it on a command line:
the shell scripts read it from `/etc/bluestone/config.yaml` on the gateway
host, and the PowerShell scripts read it from standard input, so it never
appears in a process list or a shell history.

## On the gateway host (Linux)

```bash
sudo ./smbclient-test.sh                 # upload, download, rename, delete
sudo ./linux-kernel-client-test.sh       # mount -t cifs, then copy and stat
```

Both default to `127.0.0.1`; pass another address as the first argument.

## From a macOS client

```bash
ssh gateway 'sudo sed -n "s/^ *password: \"\(.*\)\"$/\1/p" /etc/bluestone/config.yaml' \
  | ./macos-client-test.sh 10.0.0.4
```

`macos-client-test.sh` mounts the share with `mount_smbfs` and covers listing,
reading, writing, making a folder and moving a file into it, overwriting, a
5 MB copy compared byte for byte, rename, and delete, in a `macos-test` folder
it removes afterwards. Every step runs under a watchdog, so a server that
leaves the client waiting shows up as `HUNG` rather than hanging the script.
Pass `SERVER:PORT` to reach the share through a tunnel.

## From a Windows client

Pipe the password in from the gateway host, so it is never typed on Windows:

```bash
ssh gateway 'sudo sed -n "s/^ *password: \"\(.*\)\"$/\1/p" /etc/bluestone/config.yaml' \
  | ssh windows 'powershell -NoProfile -ExecutionPolicy Bypass -File C:\Windows\Temp\windows-client-test.ps1 -Server 10.0.0.4'
```

- `windows-client-test.ps1` covers mapping the share, listing, reading and
  writing, a 5 MiB copy compared by hash, case-insensitive access, attributes
  and timestamps, the write-temp-then-rename save pattern, append, rename,
  delete, and free space.
- `windows-overwrite-test.ps1` checks that overwriting an existing file
  replaces its contents rather than appending to them, through several
  different Windows write paths.
- `windows-lock-test.ps1` checks byte-range locking: locking free bytes,
  an overlapping lock from a second handle being refused, an adjacent lock
  being granted, and the bytes freeing up on release. With `-HoldOffset` it
  instead holds a lock and waits, which is how the cross-protocol check below
  keeps a lock held.
- `nfs-hold-lock.py` and `nfs-try-lock.py` do the same from an NFS mount, so
  the two protocols can be checked against each other:

  ```bash
  # On the gateway host, with the export mounted at /mnt/nfs:
  sudo python3 nfs-hold-lock.py /mnt/nfs/locktest.bin 0 100 40 &
  # Then from Windows, a lock on the same bytes must be refused:
  echo "$password" | ssh windows 'powershell -File windows-lock-test.ps1 -Server 10.0.0.4'
  ```

- `windows-acl-test.ps1` checks what Windows reads from a file's security
  descriptor: `Get-Acl` on a file and a directory (the Security tab reads the
  same thing), the inherit flags on a directory's entry, and that `Set-Acl`
  is refused cleanly rather than appearing to save permissions the gateway
  does not keep.
- `windows-sharemode-test.ps1` checks share modes: a file held with
  `FileShare.None` blocks other opens, one held with `FileShare.Read` admits
  readers but not writers, and listing a directory still works while a file
  in it is held exclusively.
