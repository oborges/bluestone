#!/bin/bash

# Mountpoint Stress Test Script
# Automated performance testing for Bluestone

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
MOUNT_PATH="${MOUNT_PATH:-/mnt/cos-nfs}"
TEST_DIR="${MOUNT_PATH}/stress-test-$(date +%Y%m%d-%H%M%S)"
RESULTS_DIR="./stress-test-results-$(date +%Y%m%d-%H%M%S)"
QUICK_MODE="${QUICK_MODE:-false}"

# Test sizes (reduced for quick mode)
if [ "$QUICK_MODE" = "true" ]; then
    LARGE_FILE_SIZE="256M"
    MEDIUM_FILE_SIZE="64M"
    SMALL_FILE_SIZE="16M"
    TEST_DURATION="30"
    NUM_FILES="1000"
else
    LARGE_FILE_SIZE="1G"
    MEDIUM_FILE_SIZE="256M"
    SMALL_FILE_SIZE="64M"
    TEST_DURATION="60"
    NUM_FILES="10000"
fi

# Functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

check_prerequisites() {
    log_info "Checking prerequisites..."
    
    # Check if mount exists
    if ! mountpoint -q "$MOUNT_PATH"; then
        log_error "Mount point $MOUNT_PATH is not mounted"
        exit 1
    fi
    
    # Check required tools
    local missing_tools=()
    for tool in fio jq bc; do
        if ! command -v $tool &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -gt 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_info "Install with: sudo apt-get install -y ${missing_tools[*]}"
        exit 1
    fi
    
    log_success "All prerequisites met"
}

setup_test_environment() {
    log_info "Setting up test environment..."
    
    # Create test directory
    mkdir -p "$TEST_DIR"
    mkdir -p "$RESULTS_DIR"
    
    # Verify write access
    if ! touch "$TEST_DIR/test.txt" 2>/dev/null; then
        log_error "Cannot write to $TEST_DIR"
        exit 1
    fi
    rm -f "$TEST_DIR/test.txt"
    
    log_success "Test environment ready"
    log_info "Test directory: $TEST_DIR"
    log_info "Results directory: $RESULTS_DIR"
}

run_sequential_write_tests() {
    log_info "Running sequential write tests..."
    
    # Test 1: Large file sequential write
    log_info "Test 1/3: Large file sequential write (${LARGE_FILE_SIZE})"
    fio --name=seq-write-large \
        --directory="$TEST_DIR" \
        --rw=write \
        --bs=1M \
        --size=$LARGE_FILE_SIZE \
        --numjobs=1 \
        --direct=1 \
        --group_reporting \
        --output-format=json \
        --output="$RESULTS_DIR/seq-write-large.json" \
        2>&1 | tee "$RESULTS_DIR/seq-write-large.log"
    
    # Test 2: Multiple concurrent writes
    log_info "Test 2/3: Concurrent writes (4 x ${MEDIUM_FILE_SIZE})"
    fio --name=seq-write-concurrent \
        --directory="$TEST_DIR" \
        --rw=write \
        --bs=1M \
        --size=$MEDIUM_FILE_SIZE \
        --numjobs=4 \
        --direct=1 \
        --group_reporting \
        --output-format=json \
        --output="$RESULTS_DIR/seq-write-concurrent.json" \
        2>&1 | tee "$RESULTS_DIR/seq-write-concurrent.log"
    
    # Test 3: Variable block sizes
    log_info "Test 3/3: Variable block sizes"
    for bs in 4k 64k 256k 1m; do
        log_info "  Testing block size: $bs"
        fio --name=seq-write-${bs} \
            --directory="$TEST_DIR" \
            --rw=write \
            --bs=$bs \
            --size=$SMALL_FILE_SIZE \
            --numjobs=1 \
            --direct=1 \
            --group_reporting \
            --output-format=json \
            --output="$RESULTS_DIR/seq-write-${bs}.json" \
            2>&1 | tee "$RESULTS_DIR/seq-write-${bs}.log"
    done
    
    log_success "Sequential write tests completed"
}

run_sequential_read_tests() {
    log_info "Running sequential read tests..."
    
    # Create test file
    log_info "Creating test file for read tests..."
    dd if=/dev/zero of="$TEST_DIR/read-test.dat" bs=1M count=512 2>&1 | tail -1
    
    # Test 1: Cold cache read
    log_info "Test 1/2: Cold cache read"
    sync
    echo 3 | sudo tee /proc/sys/vm/drop_caches > /dev/null 2>&1 || log_warning "Cannot drop caches (need sudo)"
    
    fio --name=seq-read-cold \
        --filename="$TEST_DIR/read-test.dat" \
        --rw=read \
        --bs=1M \
        --size=512M \
        --numjobs=1 \
        --direct=0 \
        --group_reporting \
        --output-format=json \
        --output="$RESULTS_DIR/seq-read-cold.json" \
        2>&1 | tee "$RESULTS_DIR/seq-read-cold.log"
    
    # Test 2: Warm cache read
    log_info "Test 2/2: Warm cache read"
    fio --name=seq-read-warm \
        --filename="$TEST_DIR/read-test.dat" \
        --rw=read \
        --bs=1M \
        --size=512M \
        --numjobs=1 \
        --direct=0 \
        --group_reporting \
        --output-format=json \
        --output="$RESULTS_DIR/seq-read-warm.json" \
        2>&1 | tee "$RESULTS_DIR/seq-read-warm.log"
    
    log_success "Sequential read tests completed"
}

run_random_io_tests() {
    log_info "Running random I/O tests..."
    
    # Test 1: Random write
    log_info "Test 1/3: Random write (4K blocks, ${TEST_DURATION}s)"
    fio --name=rand-write \
        --directory="$TEST_DIR" \
        --rw=randwrite \
        --bs=4k \
        --size=$MEDIUM_FILE_SIZE \
        --numjobs=4 \
        --direct=1 \
        --group_reporting \
        --runtime=$TEST_DURATION \
        --time_based \
        --output-format=json \
        --output="$RESULTS_DIR/rand-write.json" \
        2>&1 | tee "$RESULTS_DIR/rand-write.log"
    
    # Test 2: Random read
    log_info "Test 2/3: Random read (4K blocks, ${TEST_DURATION}s)"
    fio --name=rand-read \
        --directory="$TEST_DIR" \
        --rw=randread \
        --bs=4k \
        --size=$MEDIUM_FILE_SIZE \
        --numjobs=4 \
        --direct=1 \
        --group_reporting \
        --runtime=$TEST_DURATION \
        --time_based \
        --output-format=json \
        --output="$RESULTS_DIR/rand-read.json" \
        2>&1 | tee "$RESULTS_DIR/rand-read.log"
    
    # Test 3: Mixed random I/O
    log_info "Test 3/3: Mixed random I/O (70% read, 30% write, ${TEST_DURATION}s)"
    fio --name=rand-mixed \
        --directory="$TEST_DIR" \
        --rw=randrw \
        --rwmixread=70 \
        --bs=4k \
        --size=$MEDIUM_FILE_SIZE \
        --numjobs=4 \
        --direct=1 \
        --group_reporting \
        --runtime=$TEST_DURATION \
        --time_based \
        --output-format=json \
        --output="$RESULTS_DIR/rand-mixed.json" \
        2>&1 | tee "$RESULTS_DIR/rand-mixed.log"
    
    log_success "Random I/O tests completed"
}

run_metadata_tests() {
    log_info "Running metadata operation tests..."
    
    # Test 1: File creation
    log_info "Test 1/3: File creation rate"
    local create_start=$(date +%s.%N)
    for i in $(seq 1 $NUM_FILES); do
        touch "$TEST_DIR/file_${i}.txt"
    done
    local create_end=$(date +%s.%N)
    local create_duration=$(echo "$create_end - $create_start" | bc)
    local create_rate=$(echo "$NUM_FILES / $create_duration" | bc)
    
    echo "{\"files\": $NUM_FILES, \"duration\": $create_duration, \"rate\": $create_rate}" > "$RESULTS_DIR/metadata-create.json"
    log_info "  Created $NUM_FILES files in ${create_duration}s (${create_rate} files/sec)"
    
    # Test 2: Directory listing
    log_info "Test 2/3: Directory listing performance"
    local list_start=$(date +%s.%N)
    ls -la "$TEST_DIR" > /dev/null
    local list_end=$(date +%s.%N)
    local list_duration=$(echo "$list_end - $list_start" | bc)
    
    echo "{\"files\": $NUM_FILES, \"duration\": $list_duration}" > "$RESULTS_DIR/metadata-list.json"
    log_info "  Listed $NUM_FILES files in ${list_duration}s"
    
    # Test 3: File deletion
    log_info "Test 3/3: File deletion rate"
    local delete_start=$(date +%s.%N)
    rm -f "$TEST_DIR"/file_*.txt
    local delete_end=$(date +%s.%N)
    local delete_duration=$(echo "$delete_end - $delete_start" | bc)
    local delete_rate=$(echo "$NUM_FILES / $delete_duration" | bc)
    
    echo "{\"files\": $NUM_FILES, \"duration\": $delete_duration, \"rate\": $delete_rate}" > "$RESULTS_DIR/metadata-delete.json"
    log_info "  Deleted $NUM_FILES files in ${delete_duration}s (${delete_rate} files/sec)"
    
    log_success "Metadata tests completed"
}

run_nfs_specific_tests() {
    log_info "Running NFS-specific tests..."
    
    # Test 1: File reopen pattern (simulates NFS churn)
    log_info "Test 1/1: File reopen pattern (1000 iterations)"
    local reopen_start=$(date +%s.%N)
    for i in $(seq 1 1000); do
        echo "Iteration $i" >> "$TEST_DIR/reopen.log"
    done
    local reopen_end=$(date +%s.%N)
    local reopen_duration=$(echo "$reopen_end - $reopen_start" | bc)
    
    echo "{\"iterations\": 1000, \"duration\": $reopen_duration}" > "$RESULTS_DIR/nfs-reopen.json"
    log_info "  Completed 1000 reopen cycles in ${reopen_duration}s"
    
    log_success "NFS-specific tests completed"
}

parse_results() {
    log_info "Parsing test results..."
    
    local report="$RESULTS_DIR/summary_report.txt"
    
    cat > "$report" << EOF
=== Mountpoint Stress Test Report ===
Generated: $(date)
Mount Point: $MOUNT_PATH
Test Mode: $([ "$QUICK_MODE" = "true" ] && echo "Quick" || echo "Full")

== System Information ==
Kernel: $(uname -r)
NFS Mount: $(mount | grep "$MOUNT_PATH" || echo "Not found")

== Performance Results ==

EOF
    
    # Parse FIO results
    for json in "$RESULTS_DIR"/*.json; do
        if [ -f "$json" ]; then
            local test_name=$(basename "$json" .json)
            echo "### $test_name" >> "$report"
            
            # Extract metrics
            local read_bw=$(jq -r '.jobs[0].read.bw_bytes / 1048576' "$json" 2>/dev/null || echo "0")
            local write_bw=$(jq -r '.jobs[0].write.bw_bytes / 1048576' "$json" 2>/dev/null || echo "0")
            local read_iops=$(jq -r '.jobs[0].read.iops' "$json" 2>/dev/null || echo "0")
            local write_iops=$(jq -r '.jobs[0].write.iops' "$json" 2>/dev/null || echo "0")
            
            if [ "$read_bw" != "0" ] && [ "$read_bw" != "null" ]; then
                printf "  Read Bandwidth: %.2f MB/s\n" "$read_bw" >> "$report"
            fi
            
            if [ "$write_bw" != "0" ] && [ "$write_bw" != "null" ]; then
                printf "  Write Bandwidth: %.2f MB/s\n" "$write_bw" >> "$report"
            fi
            
            if [ "$read_iops" != "0" ] && [ "$read_iops" != "null" ]; then
                printf "  Read IOPS: %.0f\n" "$read_iops" >> "$report"
            fi
            
            if [ "$write_iops" != "0" ] && [ "$write_iops" != "null" ]; then
                printf "  Write IOPS: %.0f\n" "$write_iops" >> "$report"
            fi
            
            echo "" >> "$report"
        fi
    done
    
    log_success "Results parsed"
    log_info "Full report: $report"
    
    # Display summary
    echo ""
    echo "=== Quick Summary ==="
    cat "$report"
}

cleanup() {
    log_info "Cleaning up test files..."
    rm -rf "$TEST_DIR"
    log_success "Cleanup completed"
}

# Main execution
main() {
    echo ""
    log_info "=== Mountpoint Stress Test ==="
    log_info "Mode: $([ "$QUICK_MODE" = "true" ] && echo "Quick" || echo "Full")"
    echo ""
    
    check_prerequisites
    setup_test_environment
    
    echo ""
    run_sequential_write_tests
    
    echo ""
    run_sequential_read_tests
    
    echo ""
    run_random_io_tests
    
    echo ""
    run_metadata_tests
    
    echo ""
    run_nfs_specific_tests
    
    echo ""
    parse_results
    
    echo ""
    cleanup
    
    echo ""
    log_success "All tests completed!"
    log_info "Results saved to: $RESULTS_DIR"
    echo ""
}

# Handle interrupts
trap 'log_error "Test interrupted"; cleanup; exit 1' INT TERM

# Run main
main "$@"

# Made with Bob
