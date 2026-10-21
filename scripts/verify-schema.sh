#!/usr/bin/env bash
#
# Verify the database schema by executing it, not by reading it.
#
#   1. Apply every up-migration to a clean PostgreSQL 16.
#   2. Exercise the invariants the schema is supposed to enforce.
#   3. Roll the last migration back and re-apply it, which is what a bad release
#      actually looks like — and a different path from a full teardown, where
#      dropping the tables hides anything a down-migration forgot to clean up.
#   4. Reverse every down-migration and assert the schema is empty.
#   5. Re-apply the up-migrations, proving the cycle is repeatable.
#
# Requires Docker. Leaves nothing behind.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS="${REPO_ROOT}/apps/server/migrations"
CONTAINER="edgepilot-schema-verify-$$"
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
DB=edgepilot

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

UPS=()
while IFS= read -r f; do UPS+=("$f"); done < <(find "$MIGRATIONS" -name '*.up.sql' | sort)
if [ ${#UPS[@]} -eq 0 ]; then
    red "no migrations found in $MIGRATIONS"; exit 1
fi

bold "Starting $PG_IMAGE"
docker run -d --name "$CONTAINER" \
    -e POSTGRES_PASSWORD=verify -e POSTGRES_DB="$DB" \
    "$PG_IMAGE" >/dev/null

for _ in $(seq 1 60); do
    docker exec "$CONTAINER" pg_isready -U postgres -d "$DB" >/dev/null 2>&1 && break
    sleep 1
done
docker exec "$CONTAINER" pg_isready -U postgres -d "$DB" >/dev/null 2>&1 || {
    red "postgres did not become ready"; exit 1
}

psql_f() { docker exec "$CONTAINER" psql -U postgres -d "$DB" -v ON_ERROR_STOP=1 -q -f "$1"; }

docker cp "$MIGRATIONS/." "$CONTAINER:/migrations" >/dev/null

bold $'\n1. Applying up-migrations'
for f in "${UPS[@]}"; do
    name="$(basename "$f")"
    if psql_f "/migrations/$name" >/dev/null 2>&1; then
        green "   applied  $name"
    else
        red "   FAILED   $name"
        docker exec "$CONTAINER" psql -U postgres -d "$DB" -v ON_ERROR_STOP=1 -f "/migrations/$name" 2>&1 | tail -20
        exit 1
    fi
done

bold $'\n2. Exercising schema invariants'
docker cp "$MIGRATIONS/testdata/invariants.sql" "$CONTAINER:/invariants.sql" >/dev/null
OUT="$(docker exec "$CONTAINER" psql -U postgres -d "$DB" -tA -f /invariants.sql 2>&1)"
FAILURES=0
while IFS= read -r line; do
    case "$line" in
        PASS*) green "   $line" ;;
        FAIL*) red   "   $line"; FAILURES=$((FAILURES + 1)) ;;
    esac
done <<< "$OUT"

if [ "$FAILURES" -gt 0 ]; then
    red $'\n'"$FAILURES invariant(s) failed"
    exit 1
fi

# Rolling one migration back and redeploying is the normal response to a bad
# release, so it has to work. It is a different path from a full teardown: there,
# dropping the tables also drops their policies, triggers, and indexes, which
# hides anything the down-migration forgot to clean up.
bold $'\n3. Partial rollback and re-apply (single-step)'
LAST_UP="$(basename "${UPS[${#UPS[@]}-1]}")"
LAST_DOWN="${LAST_UP%.up.sql}.down.sql"
if psql_f "/migrations/$LAST_DOWN" >/dev/null 2>&1; then
    green "   reversed $LAST_DOWN"
else
    red "   FAILED   $LAST_DOWN"
    docker exec "$CONTAINER" psql -U postgres -d "$DB" -v ON_ERROR_STOP=1 -f "/migrations/$LAST_DOWN" 2>&1 | tail -20
    exit 1
fi
if psql_f "/migrations/$LAST_UP" >/dev/null 2>&1; then
    green "   re-applied $LAST_UP"
else
    red "   FAILED   $LAST_UP on re-apply"
    red "   a down-migration left something behind that its up-migration cannot recreate"
    docker exec "$CONTAINER" psql -U postgres -d "$DB" -v ON_ERROR_STOP=1 -f "/migrations/$LAST_UP" 2>&1 | tail -20
    exit 1
fi

bold $'\n4. Reversing down-migrations'
for (( i=${#UPS[@]}-1 ; i>=0 ; i-- )); do
    name="$(basename "${UPS[$i]}" .up.sql).down.sql"
    [ -f "$MIGRATIONS/$name" ] || { red "   MISSING  $name"; exit 1; }
    if psql_f "/migrations/$name" >/dev/null 2>&1; then
        green "   reversed $name"
    else
        red "   FAILED   $name"
        docker exec "$CONTAINER" psql -U postgres -d "$DB" -v ON_ERROR_STOP=1 -f "/migrations/$name" 2>&1 | tail -20
        exit 1
    fi
done

REMAINING="$(docker exec "$CONTAINER" psql -U postgres -d "$DB" -tAc \
    "SELECT count(*) FROM pg_tables WHERE schemaname='public';")"
if [ "$REMAINING" -ne 0 ]; then
    red "   schema not empty after down-migrations: $REMAINING table(s) remain"
    docker exec "$CONTAINER" psql -U postgres -d "$DB" -tAc \
        "SELECT tablename FROM pg_tables WHERE schemaname='public';"
    exit 1
fi
green "   schema is empty"

bold $'\n5. Re-applying up-migrations (cycle safety)'
for f in "${UPS[@]}"; do
    psql_f "/migrations/$(basename "$f")" >/dev/null 2>&1 || {
        red "   FAILED on re-apply: $(basename "$f")"; exit 1
    }
done
green "   up -> down -> up cycle is clean"

SUMMARY="$(docker exec "$CONTAINER" psql -U postgres -d "$DB" -tAc "
SELECT (SELECT count(*) FROM pg_tables WHERE schemaname='public')
       || ' tables (incl. partitions), '
       || (SELECT count(*) FROM permissions)         || ' permissions, '
       || (SELECT count(*) FROM roles)               || ' roles, '
       || (SELECT count(*) FROM command_definitions) || ' command definitions';")"

green $'\nSchema verified: '"$SUMMARY"
