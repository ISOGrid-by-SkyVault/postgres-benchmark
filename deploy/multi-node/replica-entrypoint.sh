#!/bin/sh
# Entrypoint of the demo replica.
#
# On first start the data directory is empty: take a base backup of the
# primary. `-R` writes standby.signal and the connection settings, so the
# server then starts as a hot standby that streams changes from the primary.
set -eu

if [ ! -s "$PGDATA/PG_VERSION" ]; then
  echo "replica: cloning $PRIMARY_HOST"
  mkdir -p "$PGDATA"
  chmod 700 "$PGDATA"
  until pg_basebackup -h "$PRIMARY_HOST" -U "$PRIMARY_USER" -D "$PGDATA" -R -X stream; do
    echo "replica: primary not ready, retrying in 2s"
    rm -rf "${PGDATA:?}"/*
    sleep 2
  done
fi

exec postgres
