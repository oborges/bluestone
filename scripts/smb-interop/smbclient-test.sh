#!/bin/bash
# Exercise the SMB share with smbclient. Reads the share password from the
# gateway config; never prints it.
set -u
PASS=$(sudo -n sed -n 's/^ *password: "\(.*\)"$/\1/p' /etc/bluestone/config.yaml)
if [ -z "$PASS" ]; then echo "could not read share password"; exit 1; fi
SERVER="${1:-127.0.0.1}"
SHARE="//$SERVER/bluestone"
run() { smbclient "$SHARE" -U "bluestone%$PASS" -m SMB3 -c "$1" 2>&1 | grep -v 'Domain=\|OS=\|Server='; }

echo "=== 1. list share root"
run 'ls'
echo "=== 2. make a directory"
run 'mkdir smbtest'
echo "=== 3. upload a file"
head -c 4096 /dev/urandom > /tmp/upload.bin
echo "hello from smbclient on $(hostname)" > /tmp/upload.txt
run 'cd smbtest; put /tmp/upload.txt report.txt; put /tmp/upload.bin blob.bin; ls'
echo "=== 4. read back and compare"
rm -f /tmp/download.txt /tmp/download.bin
run 'cd smbtest; get report.txt /tmp/download.txt; get blob.bin /tmp/download.bin' >/dev/null
diff -q /tmp/upload.txt /tmp/download.txt && echo "text file matches" || echo "TEXT MISMATCH"
cmp -s /tmp/upload.bin /tmp/download.bin && echo "binary file matches" || echo "BINARY MISMATCH"
echo "=== 5. case-insensitive open"
run 'cd SMBTEST; get REPORT.TXT /tmp/case.txt' >/dev/null
diff -q /tmp/upload.txt /tmp/case.txt && echo "case-insensitive read works" || echo "CASE-INSENSITIVE READ FAILED"
echo "=== 6. rename"
run 'cd smbtest; rename report.txt final.txt; ls'
echo "=== 7. delete"
run 'cd smbtest; del blob.bin; ls'
echo "=== 8. staging state"
sudo -n ls -R /var/staging/bluestone 2>/dev/null | head -20
