#!/bin/bash

# Test downloading a small file to verify counters don't go negative
echo "Testing download with fixed counters..."

# Use a small test file from a reliable source
TEST_URL="https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf"

# Run the download with verbose mode to see chunk progress
./marianne -verbose -output test_output "$TEST_URL"

# Check exit code
if [ $? -eq 0 ]; then
    echo "Download completed successfully!"
    rm -rf test_output
    exit 0
else
    echo "Download failed!"
    rm -rf test_output
    exit 1
fi