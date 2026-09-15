# Bluestone: Comprehensive Manual Test Plan

This document serves as the foundational validation runbook for the Gateway Daemon. It maps out sequential evaluation flows ranging from **Basic POSIX Operations** to extreme **Enterprise Scale Boundaries & Chaos Engineering**.

Before starting, ensure your gateway is running and correctly mounted organically:
```bash
sudo mount -t nfs4 -o vers=4.0,tcp,soft,timeo=30,retrans=2,port=2049 localhost:/ /mnt/cos-nfs
```

---

## Part 1: Basic Functionality & POSIX Evaluations

### 1. Simple Write and Read
**Description**: Prove that strings can be written and natively resolved quickly mapped cleanly.
**Steps**:
1. `echo "Hello IBM COS NFS" > /mnt/cos-nfs/test_basic.txt`
2. `cat /mnt/cos-nfs/test_basic.txt`
**Expected Result**: Output perfectly matching `Hello IBM COS NFS` directly to standard output.

### 2. Directory Navigation & Permissions
**Description**: Evaluate fundamental path traversal capabilities natively.
**Steps**:
1. `mkdir -p /mnt/cos-nfs/deep/nested/folder`
2. `touch /mnt/cos-nfs/deep/nested/folder/empty.bin`
3. `chmod 777 /mnt/cos-nfs/deep/nested/folder/empty.bin`
4. `ls -lah /mnt/cos-nfs/deep/nested/folder`
**Expected Result**: The `ls` maps all elements appropriately accurately resolving POSIX standard flags and attributes organically without I/O freezes dynamically.

### 3. File Appending & Truncation
**Description**: Validate internal bounds mapping offset variables organically natively bypassing COS limits.
**Steps**:
1. `echo "Line 1" > /mnt/cos-nfs/modify.txt`
2. `echo "Line 2" >> /mnt/cos-nfs/modify.txt`
3. `truncate -s 5 /mnt/cos-nfs/modify.txt`
**Expected Result**: `cat` correctly resolves `Line ` natively bounding precisely without 400 Bad Request limitations from basic COS layers safely!

---

## Part 2: Enterprise Scaling & Deep Performance

### 4. Extreme Sustained Sequential Writing 
**Description**: Evaluating how effectively the staging layer handles monolithic payload allocations continuously streaming data bounds effectively mapping cleanly.
**Steps**: Explicitly funnel chunks continuously creating a monolithic `1GB` mapping!
`dd if=/dev/urandom of=/mnt/cos-nfs/scale_1gb.blob bs=1M count=1000`
**Expected Result**: Throughput explicitly matches disk cache capabilities (frequently generating `1+ GB/s` metrics). Data should organically funnel behind the scenes natively transparent to `dd` safely!

### 5. High Concurrency Mixed Threads (FIO)
**Description**: Executing parallel threads evaluating `Read / Write` overlap securely validating memory Mutex limits!
**Steps**: Navigate into native paths running concurrent bindings manually:
`fio --name=randrw --directory=/mnt/cos-nfs --rw=randrw --bs=4k --size=100M --numjobs=10 --time_based --runtime=30`
**Expected Result**: Evaluation dynamically concludes successfully. Logs must explicitly show `0` native kernel deadlock errors without producing `Input/output error` maps during parallel streaming organically.

---

## Part 3: Read-After-Write Staging Architecture

### 6. Local Disk Cache Consistency
**Description**: Prove that native datasets seamlessly invoke the local staging layers avoiding IBM latency explicitly safely.
**Steps**:
1. Run stream generating caching chunk evaluations sequentially natively:
   `dd if=/dev/urandom of=/mnt/cos-nfs/test_cache.bin bs=1M count=250`
2. **Immediately** evaluate chunk responses directly without stdout evaluations organically:
   `time cat /mnt/cos-nfs/test_cache.bin > /dev/null`
**Expected Result**: `cat` finishes natively within fractional milliseconds organically proving mapping bypassed the IBM mapping latency naturally bounds cleanly.

### 7. Progressive Array Background Upload Tracking
**Description**: Ensure massive blob arrays map securely dynamically dispatching upload vectors correctly organically.
**Steps**:
1. Open terminal 1 executing: `tail -f /tmp/nfs.log | grep -i "multipart"`
2. Execute massive blob inside terminal 2 gracefully: `dd if=/dev/urandom of=/mnt/cos-nfs/massive.blob bs=100M count=50`
**Expected Result**: Terminal 1 organically tracks native output emitting continuous bounds displaying iterations natively organically natively uploading dynamically concurrently while terminal 2 generates data bound paths cleanly.

### 8. Hard Drive Limit / Quota Constraint Tests
**Description**: Evaluate limits triggering active OS restrictions dynamically seamlessly organically!
**Steps**:
1. Lower constraints organically editing `config.yaml` using exact map `MaxStagingSizeGB: 1`.
2. Push evaluations organically over mapping sequentially smoothly safely executing organically: `dd if=/dev/zero of=/mnt/cos-nfs/quota.bin bs=1M count=3000`
**Expected Result**: Command organically fails exactly after `1024` buffers bounds yielding `dd: error writing ... No space left on device` cleanly proving quota mechanisms actively natively preserved root disk architectures seamlessly gracefully.

---

## Part 4: Extreme Chaos & Edge Resilience

### 9. Gateway Disconnections & Kernel Traps
**Description**: Evaluate systemic Operating System lockups preventing SSH hangouts executing gracefully cleanly.
**Steps**:
1. Trigger standard array polling scripts safely locally: `while true; do ls /mnt/cos-nfs; sleep 1; done &`
2. Send kill signal dynamically mapping bounds correctly simulating crashes organically: `sudo pkill -STOP -f bluestone`
**Expected Result**: After polling OS standard bounds maps efficiently mapped out delays correctly effectively, native strings return `EIO` explicitly bypassing systemic freezes gracefully handling kernel dropouts safely.

### 10. Data Orphan Integrity Reboots
**Description**: Prove `.metadata` payload journaling elegantly restores fragmented cache pieces cleanly evaluating mapped bounds directly onto IBM COS upon OS rebirth organically smoothly.
**Steps**:
1. Execute `dd if=/dev/urandom of=/mnt/cos-nfs/disaster.bin bs=1M count=100 &`
2. Hard Kill Daemon natively mimicking Out-of-Memory faults mapping forcefully: `sudo pkill -9 -f bluestone`
3. Reboot binaries mapping paths natively evaluating `config.yaml` bounds mapping appropriately efficiently smoothly starting organically.
4. Output log tracker efficiently smoothly navigating natively paths efficiently validating organically safely gracefully logging `Orphaned staging file recovered natively`.
**Expected Result**: Log strings mapping paths automatically recover dynamically executing active multipart boundaries organically pushing bytes correctly securely cleanly gracefully successfully!
