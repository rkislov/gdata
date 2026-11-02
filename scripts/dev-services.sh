#!/bin/sh
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
set -eu
cd "$(dirname "$0")/.."
if [ ! -f .local/manager.json ] || [ ! -x bin/srk ]; then
  echo 'Run make build and bin/srk dev init first.' >&2
  exit 1
fi
pids=''
cleanup() {
  for pid in $pids; do kill "$pid" 2>/dev/null || true; done
  for pid in $pids; do wait "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT
trap 'exit 0' INT TERM
mkdir -p .local/logs
for component in manager media agent proxy; do
  bin/srk --config ".local/$component.json" "$component" serve >".local/logs/$component.log" 2>&1 &
  pids="$pids $!"
done
echo 'GData SRK: https://localhost:8443'
echo 'Password: .local/admin.password; logs: .local/logs/'
echo 'Ctrl+C stops all four components; PostgreSQL remains running.'
while :; do
  for pid in $pids; do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "A service exited. Inspect .local/logs/." >&2
      exit 1
    fi
  done
  sleep 2
done
