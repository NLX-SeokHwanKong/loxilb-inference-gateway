#!/bin/bash
# CICD scenario: mcp — topology for the loxilb-mcp bridge validation.
#
#   l3h1 (10.10.10.1) ---- llb1 (10.10.10.254 / 31.31.31.254) ---- l3ep1 (31.31.31.1)
#   pg-mcp (docker bridge, reachable from llb1): the PostgreSQL management
#   store behind --userservice (two schemas, two roles, provisioned by the
#   script the product ships), so the API-key, tenant/user rate-limit and
#   defaults endpoints answer for real instead of 503.
#
# Timing is printed at every readiness step (the owner's bar: the store
# ready in under 60 s on the hosted runner); every wait is explicit and
# capped, and a cap that is hit ends the scenario with a FATAL line.

source ../common.sh

PG_NAME=pg-mcp
PG_OWNER=oamuser
PG_OWNER_PW=oampass
PG_DB=loxilb
DP_PW=dp-secret-1
MGMT_PW=mgmt-secret-1
ADMIN_USER=admin
ADMIN_PW='Admin123!'

# Enable the prometheus collector thread so /metrics exposes the families the
# MCP monitoring/diagnose tools consume.
extra_opts="-p"

t_since() { echo $(( $(date +%s) - $1 )); }

echo "#########################################"
echo "Spawning the management store (PostgreSQL)"
echo "#########################################"

STORE_T0=$(date +%s)
docker rm -f "$PG_NAME" >/dev/null 2>&1
# The container runs with --rm: a previous run's removal may still be in
# flight when its name is reused, so wait, bounded, until the name is free.
for i in $(seq 1 30); do
  docker inspect "$PG_NAME" >/dev/null 2>&1 || break
  sleep 1
done
if docker inspect "$PG_NAME" >/dev/null 2>&1; then
  echo "FATAL: a previous $PG_NAME container is still being removed"; exit 1
fi
docker run --rm -d --name "$PG_NAME" \
  -e POSTGRES_USER="$PG_OWNER" \
  -e POSTGRES_PASSWORD="$PG_OWNER_PW" \
  -e POSTGRES_DB="$PG_DB" \
  postgres:18.6 >/dev/null

# Over TCP, not the unix socket: pg_isready answers on the socket before the
# server listens on the port the gateway reaches. Cap: 60 s.
for i in $(seq 1 60); do
  if docker exec "$PG_NAME" pg_isready -h 127.0.0.1 -U "$PG_OWNER" -d "$PG_DB" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$PG_NAME" pg_isready -h 127.0.0.1 -U "$PG_OWNER" -d "$PG_DB" >/dev/null 2>&1 || {
  echo "FATAL: the store did not answer pg_isready within 60 s"; exit 1; }
echo "  store accepting connections after $(t_since $STORE_T0) s"

docker cp ../../scripts/aigw-db-bootstrap.sql "$PG_NAME:/tmp/aigw-db-bootstrap.sql"
docker exec -e AIGW_DB_PASSWORD="$DP_PW" -e AIGW_MGMT_DB_PASSWORD="$MGMT_PW" \
  "$PG_NAME" psql -h 127.0.0.1 -U "$PG_OWNER" -d "$PG_DB" -q -f /tmp/aigw-db-bootstrap.sql || {
  echo "FATAL: the bootstrap script failed"; exit 1; }
PG_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$PG_NAME")
echo "  store provisioned (two schemas, two roles) after $(t_since $STORE_T0) s; $PG_IP:5432"

echo "#########################################"
echo "Spawning all hosts (mcp)"
echo "#########################################"

# pick_config=yes mounts $(pwd)/llb1_config as /etc/loxilb/ inside llb1; the
# two role passwords arrive as mounted files, the deployment shape.
pick_config=yes
mkdir -p llb1_config
echo "$MGMT_PW" > llb1_config/mgmt_db_password
echo "$DP_PW"   > llb1_config/aikey_password

GW_T0=$(date +%s)
spawn_docker_host --dock-type loxilb --dock-name llb1 \
  --extra-args "-p --userservice \
    --mgmt-db-host $PG_IP --mgmt-db-port 5432 --mgmt-db-user aigw_mgmt_user \
    --mgmt-db-name $PG_DB --mgmt-db-password-file /etc/loxilb/mgmt_db_password \
    --aikey-db-host $PG_IP --aikey-db-port 5432 --aikey-db-user aigwuser \
    --aikey-db-name $PG_DB --aikey-db-password-file /etc/loxilb/aikey_password"
spawn_docker_host --dock-type host --dock-name l3h1
spawn_docker_host --dock-type host --dock-name l3ep1

echo "#########################################"
echo "Connecting and configuring hosts"
echo "#########################################"

connect_docker_hosts l3h1 llb1
connect_docker_hosts l3ep1 llb1

sleep 5

# Reset pick_config so the helpers below configure llb1 instead of leaving
# it to a config file (the /etc/loxilb/ mount already happened at spawn).
pick_config=""

# L3 config: l3h1 is the client, l3ep1 the backend
config_docker_host --host1 l3h1 --host2 llb1 --ptype phy --addr 10.10.10.1/24 --gw 10.10.10.254
config_docker_host --host1 l3ep1 --host2 llb1 --ptype phy --addr 31.31.31.1/24 --gw 31.31.31.254
config_docker_host --host1 llb1 --host2 l3h1 --ptype phy --addr 10.10.10.254/24
config_docker_host --host1 llb1 --host2 l3ep1 --ptype phy --addr 31.31.31.254/24

echo "#########################################"
echo "Waiting for the gateway REST API"
echo "#########################################"

# Cap: 60 s (30 x 2 s).
for i in $(seq 1 30); do
  if $hexec llb1 curl -sf http://localhost:11111/netlox/v1/version >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
$hexec llb1 curl -sf http://localhost:11111/netlox/v1/version >/dev/null 2>&1 || {
  echo "FATAL: the gateway REST API did not answer within 60 s"; exit 1; }
echo "  gateway REST ready after $(t_since $GW_T0) s ($(t_since $STORE_T0) s since the store started)"

# The REST listener answers before the boot config replay settles; until it
# does the freeze middleware 503s every mutation. Probe with a write that can
# never apply (an empty body fails validation), as cicd/ai-apikey does.
for i in $(seq 1 40); do
  if ! $hexec llb1 curl -s -m 3 -X POST http://localhost:11111/netlox/v1/config/loadbalancer -H 'Content-Type: application/json' -d '{}' | grep -qE 'boot config replay settles|frozen while a snapshot restore is in progress'; then
    echo "  boot config settled after $(t_since $GW_T0) s"; break
  fi
  if [ "$i" -eq 40 ]; then
    echo "FATAL: boot config replay never settled; last probe answer:"
    $hexec llb1 curl -s -m 3 -X POST http://localhost:11111/netlox/v1/config/loadbalancer -H 'Content-Type: application/json' -d '{}'
    exit 1
  fi
  sleep 2
done

echo "#########################################"
echo "Admin user, login, seeded tenant and key"
echo "#########################################"

# POST /auth/users needs no auth on a fresh store; every later call carries
# the bearer. validation.sh logs in again with the same credentials.
$hexec llb1 curl -s -o /dev/null -X POST http://localhost:11111/netlox/v1/auth/users \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PW\",\"role\":\"admin\"}"
TOKEN=$($hexec llb1 curl -s -X POST http://localhost:11111/netlox/v1/auth/login \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PW\"}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null)
if [[ -z "$TOKEN" ]]; then
  echo "FATAL: REST login failed (no token)"; exit 1
fi
echo "  admin logged in (token ${TOKEN:0:12}...)"
# loxicmd inside llb1 reads /tmp/loxilbtoken; the scenario's loxicmd calls
# (create_lb_rule below, the lb_list cross-check) need it under --userservice.
echo "$TOKEN" | $dexec llb1 loxicmd set login --provider manual >/dev/null 2>&1

# The typed AI rule (validation.sh, mode=4) listens on 20.20.20.3; since #362 a
# full-proxy rule whose VIP the gateway does not hold is refused at create, so
# the VIP is bound on llb1 first, as the httpsproxy scenarios do.
$dexec llb1 ip addr add 20.20.20.3/32 dev lo

# Baseline TCP LB the observe checks look at; the manage round-trip creates
# and deletes its own second VIP (20.20.20.2) through MCP.
create_lb_rule llb1 20.20.20.1 --tcp=2020:8080 --endpoints=31.31.31.1:1

# One seeded tenant with one key, so the list reads have a row to show and
# the ai-ops legs run against a store that already holds data. The key
# material is not printed.
seed=$($hexec llb1 curl -s -X POST http://localhost:11111/netlox/v1/config/ai/apikey \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"tenant_id":"cicd-seed","name":"seed-key","rate_limit_rps":5,"burst_size":10,"tokens_per_min":1000,"enabled":true}')
seed_id=$(printf '%s' "$seed" | python3 -c "import sys,json; print(json.load(sys.stdin).get('key_id',''))" 2>/dev/null)
if [[ -z "$seed_id" ]]; then
  echo "FATAL: the seeded key was not created: $(printf '%s' "$seed" | head -c 200)"; exit 1
fi
echo "  seeded tenant cicd-seed, key seed-key ($seed_id)"

# Auto-persist is a debounced write-through fired by the successful writes
# above; /status/ready true after them is the verdict that it has settled
# (cicd/ai-apikey, F-P0-3). Cap: 80 s.
for i in $(seq 1 40); do
  rs=$($hexec llb1 curl -s -m 5 -H "Authorization: Bearer $TOKEN" \
    http://localhost:11111/netlox/v1/status/ready 2>/dev/null)
  if printf '%s' "$rs" | python3 -c \
      "import sys,json; sys.exit(0 if json.load(sys.stdin).get('ready') is True else 1)" 2>/dev/null; then
    echo "  configuration ready after $(t_since $GW_T0) s"
    break
  fi
  if [ "$i" -eq 40 ]; then
    echo "FATAL: /status/ready never reported ready; last answer:"
    printf '%s\n' "$rs"
    exit 1
  fi
  sleep 2
done

echo "#########################################"
echo "mcp testbed ready: store + gateway in $(t_since $STORE_T0) s"
echo "#########################################"
