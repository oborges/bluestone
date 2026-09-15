#!/bin/bash

# Script to measure the missing 85 seconds in directory listing
# This will help us understand where the time is being spent

set -e

MOUNT_POINT=${1:-"/mnt/nfs"}
TEST_DIR="quick-test-27156/small"
SERVER_URL="http://localhost:8080"

echo "========================================="
echo "NFS Directory Listing Performance Test"
echo "========================================="
echo ""
echo "Target: $MOUNT_POINT/$TEST_DIR"
echo "Server: $SERVER_URL"
echo ""

# Step 1: Reset counters
echo "Step 1: Resetting performance counters..."
curl -s -X POST "$SERVER_URL/debug/perf/reset" > /dev/null
echo "✓ Counters reset"
echo ""

# Step 2: Run the test
echo "Step 2: Running 'ls -la' command..."
echo "Command: time ls -la $MOUNT_POINT/$TEST_DIR"
echo ""
echo "--- START TIMING ---"
START_TIME=$(date +%s.%N)
time ls -la "$MOUNT_POINT/$TEST_DIR" > /tmp/ls_output.txt 2>&1 || true
END_TIME=$(date +%s.%N)
WALL_TIME=$(echo "$END_TIME - $START_TIME" | bc)
echo "--- END TIMING ---"
echo ""
echo "Wall clock time: ${WALL_TIME}s"
echo ""

# Step 3: Get metrics
echo "Step 3: Collecting performance metrics..."
echo ""

echo "=== Overall Metrics ==="
curl -s "$SERVER_URL/debug/perf" | jq '.'
echo ""

echo "=== Per-Path Statistics ==="
curl -s "$SERVER_URL/debug/perf/paths" | jq '.'
echo ""

# Step 4: Analysis
echo "========================================="
echo "ANALYSIS"
echo "========================================="
echo ""

# Get the metrics as variables
METRICS=$(curl -s "$SERVER_URL/debug/perf")
READDIR_CALLS=$(echo "$METRICS" | jq -r '.readdir.total_calls')
READDIR_TIME=$(echo "$METRICS" | jq -r '.readdir.total_time_ms')
LISTDIR_TIME=$(echo "$METRICS" | jq -r '.listdirectory.total_time_ms')
CACHE_HITS=$(echo "$METRICS" | jq -r '.listdirectory.cache_hits')
CACHE_MISSES=$(echo "$METRICS" | jq -r '.listdirectory.cache_misses')
COS_LIST=$(echo "$METRICS" | jq -r '.cos_operations.list_objects')

WALL_TIME_MS=$(echo "$WALL_TIME * 1000" | bc)
READDIR_TIME_S=$(echo "$READDIR_TIME / 1000" | bc)
LISTDIR_TIME_S=$(echo "$LISTDIR_TIME / 1000" | bc)
MISSING_TIME=$(echo "$WALL_TIME_MS - $READDIR_TIME" | bc)
MISSING_TIME_S=$(echo "$MISSING_TIME / 1000" | bc)

echo "Wall clock time:        ${WALL_TIME}s (${WALL_TIME_MS}ms)"
echo "Time in ReadDir():      ${READDIR_TIME_S}s (${READDIR_TIME}ms)"
echo "Time in ListDirectory(): ${LISTDIR_TIME_S}s (${LISTDIR_TIME}ms)"
echo "MISSING TIME:           ${MISSING_TIME_S}s (${MISSING_TIME}ms)"
echo ""
echo "ReadDir() calls:        $READDIR_CALLS"
echo "Cache hits:             $CACHE_HITS"
echo "Cache misses:           $CACHE_MISSES"
echo "COS ListObjects calls:  $COS_LIST"
echo ""

if [ $(echo "$MISSING_TIME_S > 10" | bc) -eq 1 ]; then
    echo "⚠️  WARNING: ${MISSING_TIME_S}s is unaccounted for!"
    echo ""
    echo "This time is spent OUTSIDE our instrumented code."
    echo "Possible causes:"
    echo "  1. Network delays between NFS client and server"
    echo "  2. Time in go-nfs library before/after calling our code"
    echo "  3. NFS protocol overhead (marshaling/unmarshaling)"
    echo "  4. Client-side delays or retries"
    echo ""
    echo "Check server logs for:"
    echo "  - 'ReadDir loop detected' messages"
    echo "  - Average gap between calls"
    echo "  - Calls per second"
fi

echo ""
echo "========================================="
echo "Next steps:"
echo "1. Check server logs: journalctl -u bluestone -n 500"
echo "2. Look for 'ReadDir loop detected' or 'ReadDir timing' messages"
echo "3. Share this output for analysis"
echo "========================================="

# Made with Bob
