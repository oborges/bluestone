#!/usr/bin/env python3
"""Try to take an exclusive byte-range lock over NFS without waiting."""

import fcntl
import os
import sys

path = sys.argv[1] if len(sys.argv) > 1 else "/mnt/nfs/locktest.bin"
start = int(sys.argv[2]) if len(sys.argv) > 2 else 0
length = int(sys.argv[3]) if len(sys.argv) > 3 else 100

with open(path, "r+b") as handle:
    try:
        fcntl.lockf(handle, fcntl.LOCK_EX | fcntl.LOCK_NB, length, start, os.SEEK_SET)
    except OSError as err:
        print(f"[{start}, {start + length}) : refused ({err.strerror})")
    else:
        print(f"[{start}, {start + length}) : locked")
        fcntl.lockf(handle, fcntl.LOCK_UN, length, start, os.SEEK_SET)
