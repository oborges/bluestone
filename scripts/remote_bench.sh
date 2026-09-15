#!/bin/bash
set -x

echo "Terminating old gateways..."
sudo pkill -9 -f bluestone || true
sudo umount -f /mnt/cos-nfs || true
sudo rm -rf /tmp/nfs-staging || true
sudo install -d -m 700 -o root -g root /tmp/nfs-staging

echo "Spawning Gateway daemon natively..."
cd "$(dirname "$0")/.."
sudo env BLUESTONE_STAGING_ENABLED=true BLUESTONE_STAGING_ROOT_DIR=/tmp/nfs-staging ./bin/bluestone --config configs/config.yaml > /tmp/nfs.log 2>&1 &
sleep 5

echo "Mounting natively to COS..."
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos-nfs

echo "Executing custom metrics evaluation dashboard..."
sudo ./scripts/run_stress_tests.sh
