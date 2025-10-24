#!/bin/bash

# Test memory usage with different settings
echo "Testing memory usage improvements..."
echo "=================================="

# URL for a reasonably sized file (100MB test file)
TEST_URL="https://speed.hetzner.de/100MB.bin"

echo "Test 1: Download with 500MB memory limit"
echo "Command: ./marianne -memory 500M -verbose $TEST_URL"
echo ""
echo "This should complete successfully without being killed."
echo "Previous version would accumulate chunks unbounded in memory."
echo ""
echo "Key improvements:"
echo "1. Memory-aware chunk buffering with enforced limits"
echo "2. Backpressure mechanism prevents overwhelming memory"
echo "3. Adaptive chunk queuing based on memory pressure"
echo "4. Limited pending chunks accumulation"
echo ""
echo "To run the test, execute:"
echo "./marianne -memory 500M -verbose -output /tmp/test-download $TEST_URL"
echo ""
echo "Monitor memory usage with:"
echo "watch -n 1 'ps aux | grep marianne | grep -v grep'"