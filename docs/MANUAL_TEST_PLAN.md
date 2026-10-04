# Manual Test Plan

Ten checks to run by hand against a gateway, from basic file operations to
killing the process mid-write. They need a running gateway and a mount of
it:

```bash
sudo mount -t nfs4 -o vers=4.0,tcp,soft,timeo=30,retrans=2,port=2049 localhost:/ /mnt/cos-nfs
```

The `soft` mount matters for test 9: it makes the client give up with an
error instead of waiting for ever when the gateway stops answering.

Several tests read the gateway's log. The commands below assume the systemd
service; if the gateway logs to a file, tail the file named by
`logging.output` instead.

---

## Part 1: Basic file operations

### 1. Write and read

1. `echo "Hello IBM COS NFS" > /mnt/cos-nfs/test_basic.txt`
2. `cat /mnt/cos-nfs/test_basic.txt`

**Expected:** `cat` prints `Hello IBM COS NFS`.

### 2. Directories and permissions

1. `mkdir -p /mnt/cos-nfs/deep/nested/folder`
2. `touch /mnt/cos-nfs/deep/nested/folder/empty.bin`
3. `chmod 777 /mnt/cos-nfs/deep/nested/folder/empty.bin`
4. `ls -lah /mnt/cos-nfs/deep/nested/folder`

**Expected:** `ls` returns promptly and shows `empty.bin` with mode
`-rwxrwxrwx`.

### 3. Append and truncate

Object storage cannot change part of an object, so these exercise the staging
layer.

1. `echo "Line 1" > /mnt/cos-nfs/modify.txt`
2. `echo "Line 2" >> /mnt/cos-nfs/modify.txt`
3. `truncate -s 5 /mnt/cos-nfs/modify.txt`
4. `cat /mnt/cos-nfs/modify.txt`

**Expected:** `cat` prints `Line ` (five bytes), with no error.

---

## Part 2: Size and concurrency

### 4. A large sequential write

`dd if=/dev/urandom of=/mnt/cos-nfs/scale_1gb.blob bs=1M count=1000`

**Expected:** `dd` completes at about the speed of the staging disk, since
writes are accepted into local staging. The upload to COS happens afterwards
in the background and does not hold `dd` up.

### 5. Concurrent mixed reads and writes

`fio --name=randrw --directory=/mnt/cos-nfs --rw=randrw --bs=4k --size=100M --numjobs=10 --time_based --runtime=30`

**Expected:** `fio` finishes with no errors. Neither `fio` nor the gateway
log reports an `Input/output error`, and the gateway does not hang.

---

## Part 3: Staging and write-back

### 6. Reading back what was just written

1. `dd if=/dev/urandom of=/mnt/cos-nfs/test_cache.bin bs=1M count=250`
2. Straight away: `time cat /mnt/cos-nfs/test_cache.bin > /dev/null`

**Expected:** the read is served from local staging, so it takes well under
the time a 250 MB download from COS would.

### 7. Multipart upload of a large file

1. In one terminal: `sudo journalctl -u bluestone -f | grep -i "multipart"`
2. In another: `dd if=/dev/urandom of=/mnt/cos-nfs/massive.blob bs=100M count=50`

**Expected:** once the file starts syncing, the log shows
`Multipart upload lifecycle event` entries for it. `dd` is not slowed by the
upload. Progress is also visible at
`http://127.0.0.1:8082/debug/staging/sync` when `server.debug_enabled` is on.

### 8. Staging quota

1. Set `staging.max_staging_size_gb: 1` in the configuration and restart the
   gateway.
2. `dd if=/dev/zero of=/mnt/cos-nfs/quota.bin bs=1M count=3000`

**Expected:** `dd` slows as staging fills and then fails with
`No space left on device`, before the staging filesystem itself is full.
With `staging.backpressure_mode: "block"`, the default, it may wait up to
`backpressure_wait_timeout` for sync to free space before failing.

---

## Part 4: Failures

### 9. A gateway that stops answering

1. `while true; do ls /mnt/cos-nfs; sleep 1; done &`
2. Freeze the gateway: `sudo pkill -STOP -f bluestone`

**Expected:** after the mount's timeout and retries, `ls` fails with
`Input/output error` instead of hanging, and the shell stays usable.
Resume the gateway with `sudo pkill -CONT -f bluestone`; `ls` works again.

### 10. A crash with unsynced data

1. `dd if=/dev/urandom of=/mnt/cos-nfs/disaster.bin bs=1M count=100 &`
2. Kill the gateway without warning: `sudo pkill -9 -f bluestone`
3. Start it again with the same configuration.
4. Read its log from startup: `sudo journalctl -u bluestone -b | grep -i recover`

**Expected:** the log shows
`Recovered staging files automatically after daemon crash`. What `dd` had
written before the kill is still in staging, and the gateway resumes
uploading it to COS. Bytes `dd` had not yet written are, of course, not
there.
