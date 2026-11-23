#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
export MSYS_NO_PATHCONV=1
prefix="ferry-db-native-$$-$RANDOM"
network="$prefix-net"
password='ferry-native-e2e'
tmp=$(mktemp -d)
absolute=$tmp
if command -v cygpath >/dev/null; then absolute=$(cygpath -m "$tmp"); fi
started=()
cleanup() {
  if ((${#started[@]})); then docker rm -f "${started[@]}" >/dev/null 2>&1 || true; fi
  docker network rm "$network" >/dev/null 2>&1 || true
  docker network rm "$network-relay" >/dev/null 2>&1 || true
  rm -f "$tmp/identity" "$tmp/identity.pub" "$tmp/ferry-agent-linux-amd64" "$tmp/ferry.yaml"
  rmdir "$tmp" || true
}
trap cleanup EXIT

docker build -q -t ferry-e2e:db-native -f test/e2e/Dockerfile.db-native test/e2e >/dev/null
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$absolute/ferry-agent-linux-amd64" ./cmd/ferry-agent
ssh-keygen -q -t ed25519 -N '' -f "$tmp/identity"
docker network create "$network" >/dev/null
docker network create "$network-relay" >/dev/null

start() {
  local name="$prefix-$1" net=$network
  if [[ "$1" == relay ]]; then net="$network-relay"; fi
  shift
  docker run -d --name "$name" --network "$net" "$@" >/dev/null
  started+=("$name")
}

ready() {
  local name="$prefix-$1"
  shift
  for ((n=0; n<90; n++)); do
    if docker exec "$name" "$@" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  docker logs "$name" >&2
  echo "Database did not start: $name" >&2
  return 1
}

start worker -p 127.0.0.1::22 -p 127.0.0.1::3306 ferry-e2e:db-native
start maria -p 127.0.0.1::22 -p 127.0.0.1::3306 ferry-e2e:db-native
start relay -p 127.0.0.1::22 ferry-e2e:db-native
for name in pg pg-target; do
  start "$name" -p 127.0.0.1::5432 -e "POSTGRES_PASSWORD=$password" postgres:17
done
for name in mysql mysql-target; do
  start "$name" -p 127.0.0.1::3306 -e "MYSQL_ROOT_PASSWORD=$password" -e 'MYSQL_ROOT_HOST=%' mysql:8.4 --local-infile=ON
done

for name in worker maria relay; do
  docker exec -i "$prefix-$name" sh -c 'cat > /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys' < "$tmp/identity.pub"
done
for name in worker maria; do
  docker exec "$prefix-$name" sh -c 'install -d -o mysql -g mysql /native/source; mariadb-install-db --user=mysql --datadir=/native/source --auth-root-authentication-method=normal --skip-test-db >/dev/null'
  docker exec -d "$prefix-$name" mariadbd --user=mysql --datadir=/native/source --socket=/tmp/ferry-maria.sock --pid-file=/tmp/ferry-maria.pid --port=3306 --bind-address=0.0.0.0 --log-error=/tmp/ferry-maria.log
  ready "$name" mariadb --socket=/tmp/ferry-maria.sock -uroot -e 'SELECT 1'
  docker exec "$prefix-$name" mariadb --socket=/tmp/ferry-maria.sock -uroot -e "CREATE USER 'loader'@'%' IDENTIFIED BY '$password'; GRANT ALL PRIVILEGES ON *.* TO 'loader'@'%' WITH GRANT OPTION"
done

docker cp "$absolute/identity" "$prefix-worker:/root/.ssh/id_ed25519"
docker exec "$prefix-worker" chmod 600 /root/.ssh/id_ed25519
docker exec "$prefix-maria" cat /etc/ssh/ssh_host_ed25519_key.pub | docker exec -i "$prefix-worker" sh -c "sed 's/^/$prefix-maria /' > /root/.ssh/known_hosts"

for name in pg pg-target; do ready "$name" pg_isready -h 127.0.0.1 -U postgres -d postgres; done
docker exec "$prefix-pg" sh -c "printf '\\nhost replication postgres all scram-sha-256\\n' >> \"\$PGDATA/pg_hba.conf\"; psql -U postgres -c 'SELECT pg_reload_conf()'" >/dev/null
for name in mysql mysql-target; do
  ready "$name" mysql -h 127.0.0.1 -uroot "-p$password" -e 'SELECT 1'
  docker exec -e "MYSQL_PWD=$password" "$prefix-$name" mysql -uroot -e "INSTALL PLUGIN clone SONAME 'mysql_clone.so'"
  docker exec -e "MYSQL_PWD=$password" "$prefix-$name" mysql -uroot -e "GRANT BACKUP_ADMIN, CLONE_ADMIN ON *.* TO 'root'@'%'"
done
docker exec -e "MYSQL_PWD=$password" "$prefix-mysql-target" mysql -uroot -e "SET GLOBAL clone_valid_donor_list='$prefix-mysql:3306'"
docker exec "$prefix-mysql-target" install -d -o mysql -g mysql /native

cat > "$tmp/ferry.yaml" <<'EOF'
version: 1
project: db-transfer-e2e
targets:
EOF
for name in worker maria relay; do
  addr=$(docker port "$prefix-$name" 22/tcp)
  fingerprint=$(docker exec "$prefix-$name" ssh-keygen -l -f /etc/ssh/ssh_host_ed25519_key.pub | awk '{print $2}')
  cat >> "$tmp/ferry.yaml" <<EOF
  $name:
    host: 127.0.0.1
    port: ${addr##*:}
    user: root
    auth: key
    identityFile: '$absolute/identity'
    hostKey: '$fingerprint'
    root: /native/ferry
    runAs: postgres
EOF
done
cat >> "$tmp/ferry.yaml" <<'EOF'
apps:
  - name: unused
    type: binary
    artifact: {path: unused}
EOF
export FERRY_NATIVE_CONFIG="$absolute/ferry.yaml" FERRY_NATIVE_PREFIX="$prefix" FERRY_NATIVE_PASSWORD="$password" FERRY_AGENT_DIR="$absolute"
go test -count=1 -v ./cmd/ferry -run '^TestNativeDatabaseMigrationE2E$'
