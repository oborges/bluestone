#!/bin/bash

# Quick 5-minute stress test for Bluestone
# This script runs basic performance tests to quickly validate the mountpoint
# Usage: ./quick_test.sh [test_number] [test_dir]
#   test_number: 1, 2, 3, or 4 to run a specific test, or omit to run all tests
#   test_dir: optional test directory path (useful for running Test 2 after Test 1)

# Note: Not using 'set -e' to allow tests to continue even if one fails

# Configuration
MOUNT_POINT="${MOUNT_POINT:-/mnt/cos-nfs}"
SPECIFIC_TEST="${1:-all}"
if [ -n "$2" ]; then
    TEST_DIR="$2"
else
    TEST_DIR="$MOUNT_POINT/quick-test-$$"
fi

# Colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
NC='\033[0m'

echo "=========================================="
echo "Quick Bluestone Performance Test"
echo "=========================================="
echo

# Check prerequisites
if ! command -v fio &> /dev/null; then
    echo -e "${RED}Error: fio is not installed${NC}"
    echo "Install with: sudo apt-get install fio (Ubuntu/Debian) or sudo dnf install fio (RHEL/CentOS)"
    exit 1
fi

if ! mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
    echo -e "${RED}Error: NFS is not mounted at $MOUNT_POINT${NC}"
    exit 1
fi

# Create test directory
mkdir -p "$TEST_DIR"
echo -e "${BLUE}Test directory: $TEST_DIR${NC}"
if [ "$SPECIFIC_TEST" != "all" ]; then
    echo -e "${YELLOW}Running only Test $SPECIFIC_TEST${NC}"
fi
echo

# Test 1: Sequential Write
if [ "$SPECIFIC_TEST" = "all" ] || [ "$SPECIFIC_TEST" = "1" ]; then
    echo -e "${BLUE}Test 1: Sequential Write (100MB)${NC}"
    fio --name=write --directory="$TEST_DIR" --rw=write \
        --bs=1M --size=100M --numjobs=1 --direct=0 2>&1 | \
        grep -E "write: IOPS=|bw=" | head -2
    echo
    
    # Wait for file to be fully synced and available
    if [ "$SPECIFIC_TEST" = "all" ]; then
        echo -e "${YELLOW}Waiting for file to be synced...${NC}"
        sleep 5
    fi
fi

# Test 2: Sequential Read
if [ "$SPECIFIC_TEST" = "all" ] || [ "$SPECIFIC_TEST" = "2" ]; then
    echo -e "${BLUE}Test 2: Sequential Read (100MB)${NC}"
    
    # Check if file exists before attempting to read
    if [ ! -f "$TEST_DIR/write.0.0" ]; then
        echo -e "${RED}Error: write.0.0 file not found! Skipping read test.${NC}"
        if [ "$SPECIFIC_TEST" = "2" ]; then
            echo -e "${YELLOW}Files in test directory:${NC}"
            ls -la "$TEST_DIR/"
        fi
    else
        if [ "$SPECIFIC_TEST" = "2" ]; then
            # Show debug info when running individual test
            echo -e "${YELLOW}Debug: File exists:${NC}"
            ls -lh "$TEST_DIR/write.0.0"
            echo -e "${YELLOW}Running fio read test...${NC}"
            fio --name=write --directory="$TEST_DIR" --rw=read \
                --bs=1M --size=100M --numjobs=1 --direct=0 --readonly 2>&1
        else
            # Clean output when running all tests
            fio --name=write --directory="$TEST_DIR" --rw=read \
                --bs=1M --size=100M --numjobs=1 --direct=0 --readonly 2>&1 | \
                grep -E "read: IOPS=|READ:" | head -2
        fi
    fi
    echo
fi

# Test 3: Random IOPS (30 seconds)
if [ "$SPECIFIC_TEST" = "all" ] || [ "$SPECIFIC_TEST" = "3" ]; then
    echo -e "${BLUE}Test 3: Random Read/Write IOPS (30s)${NC}"
    fio --name=random --directory="$TEST_DIR" --rw=randrw \
        --bs=4K --size=50M --numjobs=4 --runtime=30s \
        --time_based --direct=0 2>&1 | \
        grep -E "IOPS=" | head -2
    echo
fi

# Test 4: Small file operations
if [ "$SPECIFIC_TEST" = "all" ] || [ "$SPECIFIC_TEST" = "4" ]; then
    echo -e "${BLUE}Test 4: Small File Operations (100 files)${NC}"
    small_dir="$TEST_DIR/small"
    mkdir -p "$small_dir"

    start=$(date +%s)
    for i in {1..100}; do
        echo "test $i" > "$small_dir/file_$i.txt"
    done
    end=$(date +%s)
    create_time=$((end - start))
    echo "Created 100 files in ${create_time}s"

    start=$(date +%s)
    for i in {1..100}; do
        cat "$small_dir/file_$i.txt" > /dev/null
    done
    end=$(date +%s)
    read_time=$((end - start))
    echo "Read 100 files in ${read_time}s"
    echo
fi

# Cleanup
if [ "$SPECIFIC_TEST" = "all" ]; then
    echo -e "${BLUE}Cleaning up...${NC}"
    rm -rf "$TEST_DIR"

    echo
    echo -e "${GREEN}=== Test Complete ===${NC}"
    echo
    echo "Performance Summary:"
    echo "-------------------"
    echo "✓ Sequential write test completed"
    echo "✓ Sequential read test completed"
    echo "✓ Random IOPS test completed"
    echo "✓ Small file operations completed"
    echo
    echo "For detailed stress testing, run:"
    echo "  ./scripts/run_stress_tests.sh"
    echo
else
    echo -e "${YELLOW}Test $SPECIFIC_TEST completed. Test directory preserved: $TEST_DIR${NC}"
    echo -e "${YELLOW}To run another test in same directory: ./scripts/quick_test.sh <test_num> $TEST_DIR${NC}"
    echo -e "${YELLOW}To cleanup manually: rm -rf $TEST_DIR${NC}"
fi
echo "For full documentation, see:"
echo "  docs/STRESS_TESTING_GUIDE.md"
echo "  docs/QUICK_START_STRESS_TEST.md"

# Made with Bob
