#!/bin/sh
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin .local/rescue
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/srk-rescue-linux-amd64 ./cmd/srk-rescue
docker build --platform linux/amd64 -f packaging/rescue/Dockerfile -t srk-rescue-builder:local .
container_id=$(docker create --platform linux/amd64 srk-rescue-builder:local)
trap 'docker rm "$container_id" >/dev/null' EXIT
docker cp "$container_id:/out/." .local/rescue/
# Store only artifact basename so sha256sum -c works from this directory.
sed 's, /out/, ,' .local/rescue/srk-rescue-amd64.iso.sha256 > .local/rescue/checksums.sha256
printf 'ISO: %s/.local/rescue/srk-rescue-amd64.iso\n' "$PWD"
