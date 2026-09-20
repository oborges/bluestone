#!/usr/bin/env bash
# Step this active gateway down so another node can take over, without
# losing writes it has accepted but not yet synced to COS.
#
# Staging replication is one-way (primary to standby), so anything this node
# staged while active exists only here. Stopping and then re-enabling
# replication would delete it: rsync --delete makes this node match the
# other one. Wait for the sync queue to drain first.
set -euo pipefail

STAGING="${STAGING:-/var/staging/bluestone}"
TIMEOUT="${TIMEOUT:-300}"

dirty() { find "${STAGING}/active" -name '*.data' 2>/dev/null | wc -l; }

echo "==> Waiting for staged writes to reach COS (timeout ${TIMEOUT}s)"
deadline=$(( $(date +%s) + TIMEOUT ))
while [ "$(dirty)" -gt 0 ]; do
  if [ "$(date +%s)" -ge "${deadline}" ]; then
    echo "!! $(dirty) staged file(s) still unsynced after ${TIMEOUT}s."
    echo "   Stepping down now would lose them once replication resumes."
    echo "   Investigate the sync queue (/debug/staging/sync) before continuing."
    exit 1
  fi
  sleep 2
done
echo "    staging is drained"

echo "==> Stopping the gateway (releases the lease, so the other node can promote at once)"
systemctl disable --now bluestone

echo "==> Resuming staging replication from the other node"
systemctl enable --now bluestone-replicate.timer 2>/dev/null || \
  echo "    (no replication timer on this node; skipping)"

echo "==> Step-down complete. This node is a standby again."
