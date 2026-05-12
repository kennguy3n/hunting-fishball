#!/usr/bin/env bash
# Round-13 Task 20: Postgres-backed migration dry-run.
#
# Spins up a disposable Postgres container, applies every
# migration in migrations/*.sql in order, rolls each one back
# (when migrations/rollback/<name>.down.sql exists), and reports
# the first SQL syntax / semantic failure. Catches errors the
# SQLite-based `make migrate-dry-run` cannot — most notably
# JSONB types, TIMESTAMPTZ, and `ALTER TABLE ... ADD COLUMN IF
# NOT EXISTS` differences.
set -euo pipefail

CONTAINER_NAME="hf-migrate-dry-run-pg"
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
PG_DB="${PG_DB:-hf_dryrun}"
PG_USER="${PG_USER:-hf}"
PG_PASSWORD="${PG_PASSWORD:-hf}"
HOST_PORT="${HOST_PORT:-15432}"

cleanup() {
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if ! command -v docker >/dev/null 2>&1; then
    echo "docker not available; skipping migrate-dry-run-pg" >&2
    exit 0
fi
if ! docker info >/dev/null 2>&1; then
    echo "docker daemon not reachable; skipping migrate-dry-run-pg" >&2
    exit 0
fi

cleanup

echo "==> launching disposable Postgres ($PG_IMAGE) on :$HOST_PORT"
docker run -d --rm \
    --name "$CONTAINER_NAME" \
    -e "POSTGRES_USER=$PG_USER" \
    -e "POSTGRES_PASSWORD=$PG_PASSWORD" \
    -e "POSTGRES_DB=$PG_DB" \
    -p "$HOST_PORT:5432" \
    "$PG_IMAGE" >/dev/null

# Wait up to 60s for Postgres to accept connections.
echo -n "==> waiting for Postgres "
ready=0
for _ in $(seq 1 60); do
    if docker exec "$CONTAINER_NAME" pg_isready -U "$PG_USER" -d "$PG_DB" >/dev/null 2>&1; then
        ready=1
        echo " ready"
        break
    fi
    sleep 1
    echo -n "."
done
if [ "$ready" -ne 1 ]; then
    echo
    echo "Postgres did not become ready in 60s" >&2
    docker logs "$CONTAINER_NAME" 2>&1 | tail -20 >&2
    exit 1
fi
# Postgres reports ready before the database is fully initialised
# when the container is still applying initdb scripts; give it a
# moment so the first migration doesn't race against the shutdown.
for _ in $(seq 1 10); do
    if docker exec -e PGPASSWORD="$PG_PASSWORD" "$CONTAINER_NAME" \
        psql -U "$PG_USER" -d "$PG_DB" -c 'SELECT 1' >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

PSQL_CMD="docker exec -i -e PGPASSWORD=$PG_PASSWORD $CONTAINER_NAME psql -v ON_ERROR_STOP=1 -U $PG_USER -d $PG_DB"

shopt -s nullglob
migrations=(migrations/*.sql)
shopt -u nullglob

if [ "${#migrations[@]}" -eq 0 ]; then
    echo "no migrations found"
    exit 0
fi

# Apply every up migration in lexical order.
for f in $(printf '%s\n' "${migrations[@]}" | sort); do
    echo "==> apply $f"
    if ! $PSQL_CMD < "$f"; then
        echo "FAILED applying $f" >&2
        exit 1
    fi
done

# Roll back in reverse order where a rollback file exists.
shopt -s nullglob
rollbacks=(migrations/rollback/*.down.sql)
shopt -u nullglob
for f in $(printf '%s\n' "${rollbacks[@]}" | sort -r); do
    echo "==> rollback $f"
    if ! $PSQL_CMD < "$f"; then
        echo "FAILED rolling back $f" >&2
        exit 1
    fi
done

echo "==> migrate-dry-run-pg OK (${#migrations[@]} migrations, ${#rollbacks[@]} rollbacks)"
