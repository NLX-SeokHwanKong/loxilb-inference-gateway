#!/bin/bash
# CICD scenario: ai-tenant (tenant QoS, the band queue). See README.md.
source ../common.sh
source ./lib/classload.sh

echo SCENARIO-ai-tenant-config
require_host_tools jq curl python3 || exit 1

mkdir -p cert
echo "#########################################"
echo "PostgreSQL for the tenant and key store"
echo "#########################################"
docker rm -f -v "$PG_NAME" >/dev/null 2>&1
for i in $(seq 1 30); do docker inspect "$PG_NAME" >/dev/null 2>&1 || break; sleep 1; done
docker run --rm -d --name "$PG_NAME" -e POSTGRES_USER="$PG_OWNER" -e POSTGRES_PASSWORD="$PG_OWNER_PW" \
  -e POSTGRES_DB="$PG_DB" postgres:18.6 >/dev/null || { echo "FATAL: PostgreSQL did not start"; exit 1; }
for i in $(seq 1 60); do
  docker exec "$PG_NAME" pg_isready -h 127.0.0.1 -U "$PG_OWNER" -d "$PG_DB" >/dev/null 2>&1 && break
  sleep 1
done
# The store's anonymous data volume is this scenario's own: its name is kept
# for rmconfig.sh, which removes it with the container and checks it is gone
# (a `docker rm -f` without -v leaves it behind even on a --rm container).
docker inspect -f '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{end}}{{end}}' "$PG_NAME" > .pg-volume
docker cp ../../scripts/aigw-db-bootstrap.sql "$PG_NAME:/tmp/aigw-db-bootstrap.sql"
docker exec -e AIGW_DB_PASSWORD="$DP_PW" -e AIGW_MGMT_DB_PASSWORD="$MGMT_PW" \
  "$PG_NAME" psql -h 127.0.0.1 -U "$PG_OWNER" -d "$PG_DB" -q -f /tmp/aigw-db-bootstrap.sql || {
  echo "FATAL: the store bootstrap failed"; exit 1; }
echo "$MGMT_PW" > cert/mgmt_db_password
echo "$DP_PW" > cert/aikey_password

echo "#########################################"
echo "Topology"
echo "#########################################"
spawn_docker_host --dock-type loxilb --dock-name tqllb \
  --docker-args "$(tq_env | sed 's/\([A-Z_]*=[a-z0-9]*\)/-e \1/g')" \
  --extra-args "--sockmapsupport $(tq_db_args)"
spawn_docker_host --dock-type host --dock-name tqh1
spawn_docker_host --dock-type host --dock-name tqep1
connect_docker_hosts tqh1  tqllb
connect_docker_hosts tqep1 tqllb
sleep 5
config_docker_host --host1 tqh1  --host2 tqllb --ptype phy --addr 10.71.10.1/24  --gw $VIP
config_docker_host --host1 tqep1 --host2 tqllb --ptype phy --addr 10.71.31.1/24  --gw 10.71.31.254
config_docker_host --host1 tqllb --host2 tqh1  --ptype phy --addr $VIP/24
config_docker_host --host1 tqllb --host2 tqep1 --ptype phy --addr 10.71.31.254/24
add_route tqh1  10.71.31.0/24 $VIP
add_route tqep1 10.71.10.0/24 10.71.31.254

echo "#########################################"
echo "Backends"
echo "#########################################"
$hexec tqep1 python3 "$(pwd)/../ai-admission/mock_backend.py" 8080 8081 > /tmp/ai-tenant-mock.log 2>&1 &
track_helper
for p in 8080 8081; do
  for i in $(seq 1 20); do
    $hexec tqep1 curl -sf --max-time 1 "http://127.0.0.1:$p/health" >/dev/null 2>&1 && break
    sleep 1
  done
  $hexec tqep1 curl -sf --max-time 1 "http://127.0.0.1:$p/health" >/dev/null 2>&1 || {
    echo "FATAL: mock backend :$p did not become ready"; exit 1; }
done

# h2c backends for the HTTP/2 pool: an HTTP/1.1 server cannot answer the
# frames the gateway forwards upstream for a stream.
for p in 9090 9091; do
  $hexec tqep1 python3 "$(pwd)/../ai-admission/h2c_backend.py" tq-h2-$p $p > /tmp/ai-tenant-h2-$p.log 2>&1 &
  track_helper
done
for p in 9090 9091; do
  for i in $(seq 1 20); do
    $hexec tqep1 curl -sf --max-time 1 --http2-prior-knowledge "http://127.0.0.1:$p/__receipts/probe" >/dev/null 2>&1 && break
    sleep 1
  done
  $hexec tqep1 curl -sf --max-time 1 --http2-prior-knowledge "http://127.0.0.1:$p/__receipts/probe" >/dev/null 2>&1 || {
    echo "FATAL: h2c backend :$p did not become ready"; exit 1; }
done

echo "#########################################"
echo "Gateway, tenants, keys, rules"
echo "#########################################"
tq_wait_api || exit 1
tq_wait_boot || exit 1
tq_add_tenants || exit 1
tq_add_rules || exit 1
source ./lib/topo-b.sh
tqb_pki && tqb_backends_up || exit 1
tqb_rules_add || exit 1
echo "config.sh done"
