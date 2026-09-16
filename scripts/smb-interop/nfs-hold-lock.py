#!/usr/bin/env python3
"""Hold an exclusive byte-range lock over NFS, so another protocol's client
can be checked against it."""

import fcntl
import os
import sys
import time

path = sys.argv[1] if len(sys.argv) > 1 else "/mnt/nfs/locktest.bin"
start = int(sys.argv[2]) if len(sys.argv) > 2 else 0
length = int(sys.argv[3]) if len(sys.argv) > 3 else 100
seconds = int(sys.argv[4]) if len(sys.argv) > 4 else 30

if not os.path.exists(path):
    with open(path, "wb") as handle:
        handle.write(b"\0" * 4096)

with open(path, "r+b") as handle:
    fcntl.lockf(handle, fcntl.LOCK_EX, length, start, os.SEEK_SET)
    print(f"holding [{start}, {start + length}) over NFS for {seconds}s", flush=True)
    time.sleep(seconds)
    fcntl.lockf(handle, fcntl.LOCK_UN, length, start, os.SEEK_SET)
    print("released", flush=True)
