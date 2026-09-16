#!/bin/bash
# Mount the share with the Linux kernel SMB client and exercise it.
set -u
SERVER="${1:-127.0.0.1}"
PASS=$(sudo -n sed -n 's/^ *password: "\(.*\)"$/\1/p' /etc/bluestone/config.yaml)
MNT=/mnt/bluestone
sudo -n mkdir -p "$MNT"
sudo -n umount "$MNT" 2>/dev/null
echo "=== root listing via smbclient (now that a directory exists)"
smbclient "//${SERVER:-127.0.0.1}/bluestone" -U "bluestone%$PASS" -m SMB3 -c 'ls' 2>&1 | grep -v 'Domain=\|OS=\|Server='
echo "=== mount with kernel cifs client"
if ! sudo -n mount -t cifs "//${SERVER:-127.0.0.1}/bluestone" "$MNT" -o "username=bluestone,password=$PASS,vers=3.0,uid=$(id -u),gid=$(id -g)" 2>/tmp/mounterr; then
  echo "MOUNT FAILED:"; cat /tmp/mounterr; exit 1
fi
echo "mounted"
echo "=== ls -la mount root"
ls -la "$MNT" | head
echo "=== ls empty vs populated dir"
ls -la "$MNT/smbtest"
echo "=== write a file through the mount"
echo "written by the linux kernel smb client" > "$MNT/smbtest/kernel.txt" && echo "write ok" || echo "WRITE FAILED"
cat "$MNT/smbtest/kernel.txt"
echo "=== copy a 2 MiB file"
head -c 2097152 /dev/urandom > /tmp/big.bin
cp /tmp/big.bin "$MNT/smbtest/big.bin" && echo "copy ok" || echo "COPY FAILED"
cmp -s /tmp/big.bin "$MNT/smbtest/big.bin" && echo "2 MiB round trip matches" || echo "2 MiB MISMATCH"
echo "=== rename and stat"
mv "$MNT/smbtest/kernel.txt" "$MNT/smbtest/renamed.txt" && echo "rename ok" || echo "RENAME FAILED"
stat -c '%n size=%s mode=%A mtime=%y' "$MNT/smbtest/renamed.txt"
echo "=== df (free space reporting)"
df -h "$MNT" | tail -1
echo "=== unmount"
sudo -n umount "$MNT" && echo "unmounted"
