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

# llb1_config is mounted as the gateway's /etc/loxilb and the gateway writes
# into it as root (managed certificates, persisted state). One left by an
# earlier run is booted from: its certificate is re-registered and the
# fixture upload below then collides with it. Refuse instead of reusing it.
if [[ -e llb1_config ]]; then
  echo "FATAL: llb1_config is left from an earlier run; remove it (sudo ./rmconfig.sh) first"; exit 1
fi

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

# L7/TLS read fixtures, written over REST (never through the bridge): one
# managed certificate (its SAN lands in the SNI store) and one L7 policy on a
# plain proxy rule of its own VIP, so the baseline rule stays the only rule
# at 20.20.20.1. The policy injects a credential-named header whose value the
# bridge must mask. The key never leaves the stage directory, removed below.
CERTSTAGE=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -keyout "$CERTSTAGE/c.key" -out "$CERTSTAGE/c.crt" \
  -subj "/CN=mcp-sni.test" -addext "subjectAltName=DNS:mcp-sni.test" 2>/dev/null
cbody=$(python3 -c 'import json,sys; print(json.dumps({"certId":"mcp-cert1","certPem":open(sys.argv[1]).read(),"keyPem":open(sys.argv[2]).read()}))' \
  "$CERTSTAGE/c.crt" "$CERTSTAGE/c.key" 2>/dev/null)
rm -rf "$CERTSTAGE"
rc=$($hexec llb1 curl -s -m 10 -o /tmp/mcp-cert.json -w "%{http_code}" -X POST http://localhost:11111/netlox/v1/config/cert \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" -d "$cbody")
if [[ "$rc" != "201" ]]; then
  echo "FATAL: fixture certificate refused (HTTP $rc): $($hexec llb1 cat /tmp/mcp-cert.json 2>/dev/null | head -c 200)"
  # The refusal body carries only a reference; the cause is in the gateway's
  # log (named after the container id), printed before the bed is torn down.
  docker exec llb1 sh -c 'grep -hiE "cert|sni" /var/log/loxilb*.log 2>/dev/null | tail -n 15' | cut -c1-300
  exit 1
fi
echo "  fixture: managed certificate mcp-cert1 (SAN mcp-sni.test)"

$dexec llb1 ip addr add 20.20.20.4/32 dev lo
rc=$($hexec llb1 curl -s -m 10 -o /dev/null -w "%{http_code}" -X POST http://localhost:11111/netlox/v1/config/loadbalancer \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"serviceArguments":{"externalIP":"20.20.20.4","port":8082,"protocol":"tcp","sel":0,"mode":4,"name":"mcp-l7-fixture","host":"20.20.20.4"},"endpoints":[{"endpointIP":"31.31.31.1","targetPort":8080,"weight":1}]}')
[[ "$rc" == "200" ]] || { echo "FATAL: L7 fixture rule refused (HTTP $rc)"; exit 1; }
L7_LBID=$($hexec llb1 curl -s -m 10 -H "Authorization: Bearer $TOKEN" http://localhost:11111/netlox/v1/config/loadbalancer/all \
  | python3 -c 'import sys,json; print(next((r["serviceArguments"].get("id","") for r in json.load(sys.stdin).get("lbAttr",[]) if r["serviceArguments"].get("name")=="mcp-l7-fixture"),""))' 2>/dev/null)
[[ -n "$L7_LBID" ]] || { echo "FATAL: no id for the L7 fixture rule"; exit 1; }
rc=$($hexec llb1 curl -s -m 10 -o /tmp/mcp-l7.json -w "%{http_code}" -X POST http://localhost:11111/netlox/v1/config/l7policy \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d "{\"id\":\"mcp-l7pol1\",\"name\":\"mcp-block\",\"lbId\":\"$L7_LBID\",\"rules\":[{\"position\":1,\"matchSets\":[{\"conditions\":[{\"field\":\"PATH\",\"op\":\"STARTS_WITH\",\"value\":\"/blocked\"}]}],\"action\":{\"kind\":\"REJECT\",\"reject\":{\"statusCode\":451}},\"insertHeaders\":[{\"op\":\"SET\",\"name\":\"Authorization\",\"value\":\"Bearer mcp-fixture-secret\"},{\"op\":\"SET\",\"name\":\"X-Route\",\"value\":\"blue\"}]}]}")
if [[ "$rc" != "200" && "$rc" != "204" ]]; then
  echo "FATAL: fixture L7 policy refused (HTTP $rc): $($hexec llb1 cat /tmp/mcp-l7.json 2>/dev/null | head -c 200)"; exit 1
fi
echo "  fixture: L7 policy mcp-l7pol1 on rule $L7_LBID (20.20.20.4:8082)"

# IPsec read fixtures, written over REST: a PSK tunnel (responder, auto=add,
# so nothing is initiated; peer reachability is irrelevant) whose generated
# peer configuration carries the key the bridge must mask, one CA
# certificate and one certificate with its key. Not fatal: an image whose
# IPsec handler refuses them makes the IPsec legs skip with the answer as
# the reason. The keys never leave the stage directory, removed below.
ipsec_post() {
  $hexec llb1 curl -s -m 10 -o /tmp/mcp-ipsec.json -w "%{http_code}" -X POST "http://localhost:11111/netlox/v1/config/ipsec/$1" \
    -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" -d "$2"
}
rc=$(ipsec_post tunnels '{"name":"mcp-tun1","localIp":"31.31.31.254","remoteIp":"31.31.31.253","authMode":"psk","psk":"mcp-ipsec-fixture-psk","localId":"mcp-a","remoteId":"mcp-b","ikeVersion":"ikev2","tunnelMode":"tunnel","auto":"add"}')
echo "  fixture: IPsec PSK tunnel mcp-tun1 (HTTP $rc$([[ "$rc" == 20? ]] || echo ": $($hexec llb1 cat /tmp/mcp-ipsec.json 2>/dev/null | head -c 160)"))"
CERTSTAGE=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -keyout "$CERTSTAGE/ca.key" -out "$CERTSTAGE/ca.crt" \
  -subj "/CN=mcp-ipsec-ca" -addext "basicConstraints=critical,CA:TRUE" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$CERTSTAGE/gw.key" -out "$CERTSTAGE/gw.csr" -subj "/CN=mcp-ipsec-gw" 2>/dev/null
printf 'subjectAltName=DNS:mcp-ipsec-gw.test\n' > "$CERTSTAGE/ext"
openssl x509 -req -in "$CERTSTAGE/gw.csr" -CA "$CERTSTAGE/ca.crt" -CAkey "$CERTSTAGE/ca.key" -CAcreateserial -days 2 \
  -extfile "$CERTSTAGE/ext" -out "$CERTSTAGE/gw.crt" 2>/dev/null
cabody=$(python3 -c 'import json,sys; print(json.dumps({"name":"mcp-ca1","certificate":open(sys.argv[1]).read()}))' "$CERTSTAGE/ca.crt" 2>/dev/null)
gwbody=$(python3 -c 'import json,sys; print(json.dumps({"name":"mcp-ipcert1","certificate":open(sys.argv[1]).read(),"privateKey":open(sys.argv[2]).read()}))' \
  "$CERTSTAGE/gw.crt" "$CERTSTAGE/gw.key" 2>/dev/null)
rm -rf "$CERTSTAGE"
rc=$(ipsec_post ca-certificates "$cabody")
echo "  fixture: IPsec CA certificate mcp-ca1 (HTTP $rc$([[ "$rc" == 20? ]] || echo ": $($hexec llb1 cat /tmp/mcp-ipsec.json 2>/dev/null | head -c 160)"))"
rc=$(ipsec_post certificates "$gwbody")
echo "  fixture: IPsec certificate mcp-ipcert1 (HTTP $rc$([[ "$rc" == 20? ]] || echo ": $($hexec llb1 cat /tmp/mcp-ipsec.json 2>/dev/null | head -c 160)"))"

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
