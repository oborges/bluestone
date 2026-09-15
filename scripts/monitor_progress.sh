#!/bin/bash
# Monitor Bluestone Progress During Stress Tests

echo "=== Bluestone Progress Monitor ==="
echo "This script monitors download progress, throughput, and errors"
echo "Press Ctrl+C to stop"
echo ""

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to format log output
format_logs() {
    while IFS= read -r line; do
        # Extract timestamp and message
        if echo "$line" | grep -q "Download progress"; then
            # Extract progress info
            progress=$(echo "$line" | grep -oP 'progress":"[^"]*' | cut -d'"' -f3)
            throughput=$(echo "$line" | grep -oP 'throughput":"[^"]*' | cut -d'"' -f3)
            readMB=$(echo "$line" | grep -oP 'readMB":"[^"]*' | cut -d'"' -f3)
            echo -e "${GREEN}[PROGRESS]${NC} Downloaded: ${BLUE}${readMB}${NC} | Progress: ${YELLOW}${progress}${NC} | Speed: ${GREEN}${throughput}${NC}"
        elif echo "$line" | grep -q "Starting object download"; then
            sizeMB=$(echo "$line" | grep -oP 'sizeMB":"[^"]*' | cut -d'"' -f3)
            key=$(echo "$line" | grep -oP 'key":"[^"]*' | cut -d'"' -f3)
            echo -e "${BLUE}[START]${NC} Downloading: ${key} (${sizeMB})"
        elif echo "$line" | grep -q "Object download complete"; then
            sizeMB=$(echo "$line" | grep -oP 'sizeMB":"[^"]*' | cut -d'"' -f3)
            key=$(echo "$line" | grep -oP 'key":"[^"]*' | cut -d'"' -f3)
            echo -e "${GREEN}[COMPLETE]${NC} Downloaded: ${key} (${sizeMB})"
        elif echo "$line" | grep -q "retrying GetObject"; then
            attempt=$(echo "$line" | grep -oP 'attempt":[0-9]+' | cut -d':' -f2)
            backoff=$(echo "$line" | grep -oP 'backoff":"[^"]*' | cut -d'"' -f3)
            echo -e "${YELLOW}[RETRY]${NC} Attempt ${attempt} after ${backoff}"
        elif echo "$line" | grep -q "GetObject succeeded after retry"; then
            attempt=$(echo "$line" | grep -oP 'attempt":[0-9]+' | cut -d':' -f2)
            echo -e "${GREEN}[SUCCESS]${NC} Retry succeeded on attempt ${attempt}"
        elif echo "$line" | grep -q '"level":"error"'; then
            error=$(echo "$line" | grep -oP 'error":"[^"]*' | cut -d'"' -f3)
            echo -e "${RED}[ERROR]${NC} ${error}"
        elif echo "$line" | grep -q "cache hit"; then
            echo -e "${GREEN}[CACHE HIT]${NC} Data served from cache"
        elif echo "$line" | grep -q "cache miss"; then
            echo -e "${YELLOW}[CACHE MISS]${NC} Fetching from COS"
        fi
    done
}

# Check if log file path is provided
if [ -z "$1" ]; then
    echo "Usage: $0 <log-file-path>"
    echo "Example: $0 /var/log/bluestone/gateway.log"
    echo ""
    echo "Or pipe logs directly:"
    echo "  tail -f /var/log/bluestone/gateway.log | $0"
    echo ""
    
    # If no argument, try to read from stdin
    if [ -t 0 ]; then
        echo "No log file specified and no input from pipe. Exiting."
        exit 1
    else
        # Reading from pipe
        format_logs
    fi
else
    # Follow log file
    LOG_FILE="$1"
    
    if [ ! -f "$LOG_FILE" ]; then
        echo "Error: Log file not found: $LOG_FILE"
        exit 1
    fi
    
    echo "Monitoring: $LOG_FILE"
    echo "---"
    
    tail -f "$LOG_FILE" | format_logs
fi

# Made with Bob
