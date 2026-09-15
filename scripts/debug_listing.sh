#!/bin/bash

# Debug script for directory listing issues
# This script helps track what's happening during ls operations

set -e

echo "=== Bluestone Directory Listing Debug ==="
echo ""

# Check if mount point is provided
MOUNT_POINT=${1:-"/mnt/nfs"}
TEST_DIR=${2:-"quick-test-27156/small"}

echo "Mount point: $MOUNT_POINT"
echo "Test directory: $TEST_DIR"
echo ""

# Function to count log messages
count_logs() {
    local pattern=$1
    local count=$(grep -c "$pattern" /tmp/nfs_debug.log 2>/dev/null || echo "0")
    echo "$count"
}

# Clear previous debug log
> /tmp/nfs_debug.log

echo "Step 1: Starting log capture in background..."
# Capture Bluestone logs (adjust this based on how you run the gateway)
# This assumes logs go to stdout/stderr
echo "  (Make sure Bluestone is running and logging to a file or use journalctl)"
echo ""

echo "Step 2: Performing ls operation..."
echo "  Running: ls -la $MOUNT_POINT/$TEST_DIR"
echo ""

# Run ls and capture timing
START_TIME=$(date +%s)
timeout 30s ls -la "$MOUNT_POINT/$TEST_DIR" 2>&1 | head -20 || {
    EXIT_CODE=$?
    if [ $EXIT_CODE -eq 124 ]; then
        echo "ERROR: ls command timed out after 30 seconds!"
    else
        echo "ERROR: ls command failed with exit code $EXIT_CODE"
    fi
}
END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

echo ""
echo "Step 3: Analysis"
echo "  Duration: ${DURATION}s"
echo ""

echo "Expected log patterns to look for in the Bluestone logs:"
echo "  1. 'ListDirectory cache miss' - Should appear once"
echo "  2. 'Got objects from COS' - Should appear once with object count"
echo "  3. 'Directory listed and cached' - Should appear once"
echo "  4. 'Stat cache miss' - Should be minimal (not hundreds)"
echo "  5. 'Implicit directory detected' - Should not repeat"
echo ""

echo "If you see:"
echo "  - Hundreds of 'Stat' calls: NFS client is stuck in validation loop"
echo "  - 'Implicit directory detected' repeating: Cache not working properly"
echo "  - Long 'cos_duration': COS ListObjects is slow"
echo "  - No 'ListDirectory' logs: NFS client never calls ReadDir"
echo ""

echo "Next steps:"
echo "  1. Check the Bluestone logs for the patterns above"
echo "  2. Look for the 'ListDirectory completed' message with duration"
echo "  3. If duration > 5s for 100 files, there's a performance issue"
echo "  4. Count how many 'Stat cache miss' messages appear"
echo ""

echo "To monitor logs in real-time, run:"
echo "  tail -f <your-bluestone-log-file> | grep -E 'ListDirectory|Stat|Implicit'"

# Made with Bob
