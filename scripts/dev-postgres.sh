#!/bin/sh
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
set -eu
cd "$(dirname "$0")/.."
mkdir -p .local/logs
if ! command -v initdb >/dev/null 2>&1 || ! command -v pg_ctl >/dev/null 2>&1; then
  echo 'PostgreSQL binaries initdb and pg_ctl must be installed and on PATH.' >&2
  exit 1
fi
if [ ! -f .local/postgres/PG_VERSION ]; then
  initdb -D .local/postgres --auth-local=trust --auth-host=trust --encoding=UTF8 --no-locale > .local/logs/initdb.log
fi
if ! pg_ctl -D .local/postgres status >/dev/null 2>&1; then
  pg_ctl -D .local/postgres -l .local/logs/postgres.log -o '-h 127.0.0.1 -p 55432 -k /tmp' -w start
fi
srk_database_exists=$(psql -h 127.0.0.1 -p 55432 -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='srk'")
if [ "$srk_database_exists" != "1" ]; then
  createdb -h 127.0.0.1 -p 55432 srk
fi
echo 'Development PostgreSQL ready on 127.0.0.1:55432. Trust auth is limited to the loopback development cluster.'
