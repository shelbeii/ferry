#!/usr/bin/env bash
# Real database tests through the same Ferry serve protocol used by the desktop.
set -euo pipefail
cd "$(dirname "$0")/../.."

prefix="ferry-migration-$$-$RANDOM"
password=ferry-migration-e2e
pg="$prefix-pg"
pg_target="$prefix-pg-target"
mysql="$prefix-mysql"
mysql_target="$prefix-mysql-target"
maria="$prefix-maria"
fallback="$prefix-fallback"
started=()

cleanup() {
  if ((${#started[@]} != 0)); then
    docker rm -f "${started[@]}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

start() {
  local name=$1
  shift
  docker run -d --name "$name" "$@" >/dev/null
  started+=("$name")
}

wait_db() {
  local name=$1
  shift
  for ((attempt=0; attempt<90; attempt++)); do
    if docker exec "$name" "$@" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  docker logs "$name" >&2
  echo "Database did not become ready: $name" >&2
  return 1
}

start "$pg" -e "POSTGRES_PASSWORD=$password" -p 127.0.0.1::5432 postgres:17
start "$pg_target" -e "POSTGRES_PASSWORD=$password" -p 127.0.0.1::5432 postgres:17
for name in "$mysql" "$mysql_target" "$fallback"; do
  local_infile=ON
  if [[ $name == "$fallback" ]]; then local_infile=OFF; fi
  start "$name" -e "MYSQL_ROOT_PASSWORD=$password" -e 'MYSQL_ROOT_HOST=%' -p 127.0.0.1::3306 mysql:8.4 "--local-infile=$local_infile"
done
start "$maria" -e "MARIADB_ROOT_PASSWORD=$password" -e 'MARIADB_ROOT_HOST=%' -p 127.0.0.1::3306 mariadb:11.4 --local-infile=ON

for name in "$pg" "$pg_target"; do
  wait_db "$name" pg_isready -h 127.0.0.1 -U postgres -d postgres
done
for name in "$mysql" "$mysql_target" "$fallback"; do
  wait_db "$name" mysql -h 127.0.0.1 -uroot "-p$password" -e 'SELECT 1'
done
wait_db "$maria" mariadb -h 127.0.0.1 -uroot "-p$password" -e 'SELECT 1'

export FERRY_MIGRATION_E2E="postgres=$(docker port "$pg" 5432/tcp),mysql=$(docker port "$mysql" 3306/tcp),mariadb=$(docker port "$maria" 3306/tcp)"
export FERRY_MIGRATION_TARGET_POSTGRES="$(docker port "$pg_target" 5432/tcp)"
export FERRY_MIGRATION_TARGET_MYSQL="$(docker port "$mysql_target" 3306/tcp)"
export FERRY_MIGRATION_MYSQL_FALLBACK="$(docker port "$fallback" 3306/tcp)"
export FERRY_MIGRATION_PASSWORD="$password"
go test -count=1 -v ./cmd/ferry -run '^TestDatabaseMigrationE2E$'
