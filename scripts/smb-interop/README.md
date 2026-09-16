# SMB interoperability checks

Scripts for exercising a running Bluestone SMB share from real clients. They
were used to validate the server against Windows Server 2025, the Linux kernel
SMB client, and `smbclient`.

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
