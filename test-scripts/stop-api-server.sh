#!/bin/bash

# Function that keeps stopping kube-apiserver repeatedly
stop_api_forever() {
    while true; do
        CONTAINER_ID=$(sudo crictl ps | grep kube-apiserver | awk '{print $1}')
        if [ -n "$CONTAINER_ID" ]; then
            echo "$(date '+%H:%M:%S') - Stopping kube-apiserver ($CONTAINER_ID)..."
            sudo crictl stop "$CONTAINER_ID" >/dev/null 2>&1
        else
            echo "$(date '+%H:%M:%S') - kube-apiserver not found."
        fi
        sleep 1
    done
}

# Trap Ctrl+C (SIGINT) to stop the loop gracefully
trap 'echo ""; echo "Stopping stopAPI loop..."; exit 0' SIGINT

echo "Continuous kube-apiserver stopper is running."
echo "Press Ctrl+C to stop the process."

stop_api_forever

