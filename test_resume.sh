#!/bin/bash

echo "Testing resume functionality with SIGTERM handling..."

# Clean up any existing state files
rm -f .marianne-state-*.json

# Test URL - using a direct download URL with known size
TEST_URL="https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf"

echo "Starting download that will be interrupted..."
# Start download in background
timeout --signal=TERM 3s ./marianne -verbose "$TEST_URL" &
PID=$!

# Wait for the timeout to send SIGTERM
wait $PID 2>/dev/null

echo ""
echo "Download interrupted. Checking for state file..."

# Check if state file was created
STATE_FILE=$(ls .marianne-state-*.json 2>/dev/null | head -n1)

if [ -z "$STATE_FILE" ]; then
    echo "ERROR: No state file found!"
    exit 1
fi

echo "State file found: $STATE_FILE"
echo "Contents:"
cat "$STATE_FILE" | python3 -m json.tool | head -20

echo ""
echo "Now resuming download..."
./marianne -resume "$TEST_URL"

echo ""
echo "Cleaning up state file..."
rm -f "$STATE_FILE"

echo "Test completed!"