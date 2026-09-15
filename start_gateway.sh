#!/bin/bash
pkill bluestone || true
while true; do
  echo "Starting gateway..."
  ./bin/bluestone --config local-config.yaml > gateway.log 2>&1 &
  pid=$!
  sleep 40
  if ps -p $pid > /dev/null; then
    echo "Gateway is successfully running!"
    break
  else
    echo "Gateway failed to start, retrying..."
  fi
done
