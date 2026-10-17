#!/bin/bash
# lib/classload.sh — the gateway half of cicd/ai-tenant and the load driver
# the scheduling cases share. Sourced after ../common.sh.
#
# The driver fires N requests as one tenant (its key) at a pool, each held at
# the mock backend by a nonce until released, and keeps every response's
# status and X-Loxilb-Admission-* headers; admissions per band are counted
# from those headers and from fc_effective, never from timing.

VIP=10.71.10.254
MODEL=band-model
API="http://$VIP:11111/netlox/v1"
EP=10.71.31.1
PORT_SCHED=8083       # keyed: ceiling 8, queue 16, wait 30 s
PORT_IDLE=8086        # keyless: ceiling 1, queue 4, wait 5 s, idle 2 s
PORT_KNOB=8091        # keyless: ceiling 4, queue 8, the knob cases
PORT_SOCKMAP=8092     # plain L7, sockMapMode request (B7 creates and deletes it)
SCHED_MAX=8
SCHED_DEPTH=16
SCHED_WAIT_MS=30000
PG_NAME=tenantq-pg-ai-tenant
PG_OWNER=oamuser
PG_OWNER_PW=oampass
PG_DB=loxilb
DP_PW=dp-secret-1
MGMT_PW=mgmt-secret-1
TENANTS="t-crit:critical t-a:standard t-b:standard t-c:standard t-shed:sheddable"

tq_env() { echo "LLB_FC_MODE=enforce${GW_EXTRA_ENV:+ $GW_EXTRA_ENV}"; }

tq_db_args() {
  local ip
  ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$PG_NAME" 2>/dev/null)
  [ -n "$ip" ] || return 0
  echo "--mgmt-db-host $ip --mgmt-db-port 5432 --mgmt-db-user aigw_mgmt_user --mgmt-db-name $PG_DB" \
       "--mgmt-db-password-file /opt/loxilb/cert/mgmt_db_password" \
       "--aikey-db-host $ip --aikey-db-port 5432 --aikey-db-user aigwuser --aikey-db-name $PG_DB" \
       "--aikey-db-password-file /opt/loxilb/cert/aikey_password"
}

# tq_api <method> <path> [<json>] -> "<code> <body, one line>"
tq_api() {
  local m=$1 p=$2 d=$3 out
  if [ -n "$d" ]; then
    out=$($hexec tqh1 curl -s -m 10 -o /tmp/tq-api.out -w '%{http_code}' -X "$m" \
          -H 'Content-Type: application/json' -d "$d" "$API$p")
  else
    out=$($hexec tqh1 curl -s -m 10 -o /tmp/tq-api.out -w '%{http_code}' -X "$m" "$API$p")
  fi
  echo "$out $(tr -d '\n' < /tmp/tq-api.out 2>/dev/null | cut -c1-400)"
}

tq_wait_api() {
  local i
  for i in $(seq 1 40); do
    $hexec tqh1 curl -sf --max-time 3 "$API/version" >/dev/null 2>&1 && { echo "  REST API ready"; return 0; }
    sleep 2
  done
  echo "FATAL: the REST API never answered"; return 1
}

tq_wait_boot() {
  local i
  for i in $(seq 1 40); do
    $hexec tqh1 curl -s -m 3 -X POST "$API/config/loadbalancer" -H 'Content-Type: application/json' -d '{}' \
      | grep -qE 'boot config replay settles|frozen while a snapshot restore' || { echo "  boot settled"; return 0; }
    sleep 2
  done
  echo "FATAL: boot config replay never settled"; return 1
}

# tq_rule <port> <extra-json> [<target-port> ...]: an AI-gateway rule (full
# proxy, sse_mode) on the VIP.
tq_rule_json() {
  local port=$1 extra=$2 eps="" tp; shift 2
  for tp in "$@"; do eps="$eps${eps:+, }{\"endpointIP\": \"$EP\", \"targetPort\": $tp, \"weight\": 1}"; done
  echo "{\"serviceArguments\": {\"externalIP\": \"$VIP\", \"port\": $port, \"protocol\": \"tcp\", \"sel\": 0, \"mode\": 4, \"host\": \"$VIP\", \"path_prefix\": \"/\", \"path_match_mode\": \"prefix\", \"model_name\": \"$MODEL\", \"sse_mode\": true, \"inactiveTimeOut\": 60$extra}, \"endpoints\": [ $eps ]}"
}
tq_rule() {
  local port=$1 r; r=$(tq_api POST /config/loadbalancer "$(tq_rule_json "$@")")
  echo "  rule :$port: $(echo "$r" | cut -c1-120)"
  case "$r" in 200*) return 0 ;; *) return 1 ;; esac
}
tq_rule_del() { tq_api DELETE "/config/loadbalancer/externalipaddress/$VIP/port/$1/protocol/tcp" >/dev/null; }

SCHED_JSON=", \"api_key_auth\": \"required\", \"fc_max_outstanding\": $SCHED_MAX, \"fc_max_queue_depth\": $SCHED_DEPTH, \"fc_max_queue_wait_ms\": $SCHED_WAIT_MS"
IDLE_JSON=", \"fc_max_outstanding\": 1, \"fc_max_queue_depth\": 4, \"fc_max_queue_wait_ms\": 5000"
KNOB_JSON=", \"fc_max_outstanding\": 4, \"fc_max_queue_depth\": 8, \"fc_max_queue_wait_ms\": 5000"

tq_add_rules() {
  tq_rule $PORT_SCHED "$SCHED_JSON" 8080 8081 || return 1
  # The idle bound is the case: the rule's own inactiveTimeOut, 2 s.
  tq_rule $PORT_IDLE "$IDLE_JSON, \"inactiveTimeOut\": 2" 8080 || return 1
  tq_rule $PORT_KNOB "$KNOB_JSON" 8080 8081 || return 1
}

# Tenants with their class, one key each; keys.env holds KEY_<tenant>.
tq_add_tenants() {
  local t name cls r key
  : > keys.env
  for t in $TENANTS; do
    name=${t%%:*}; cls=${t##*:}
    r=$(tq_api POST /config/ai/tenant/ratelimit "{\"tenant_id\":\"$name\",\"priority_class\":\"$cls\"}")
    case "$r" in 20*) ;; *) echo "FATAL: tenant $name: $r"; return 1 ;; esac
    r=$(tq_api POST /config/ai/apikey "{\"tenant_id\":\"$name\",\"name\":\"$name-band\",\"allowed_models\":[\"$MODEL\"],\"rate_limit_rps\":10000,\"burst_size\":10000,\"enabled\":true}")
    key=$(echo "$r" | sed 's/^[0-9]* //' | jq -r '.raw_key // empty' 2>/dev/null)
    [ -n "$key" ] || { echo "FATAL: key for $name: $r"; return 1; }
    echo "KEY_${name//-/_}=$key" >> keys.env
    echo "  tenant $name ($cls) and its key"
  done
}

# tq_rule_get <port> -> the rule's serviceArguments (JSON, one line)
tq_rule_get() {
  $hexec tqh1 curl -s -m 8 "$API/config/loadbalancer/all" \
    | jq -c ".lbAttr[] | select(.serviceArguments.port==$1) | .serviceArguments"
}
# tq_fce <port> <jq-path under fc_effective>
# Absent reads "none"; a boolean false stays "false" (jq's // treats false
# as absent).
tq_fce() { tq_rule_get "$1" | jq -r ".fc_effective$2 | if . == null then \"none\" else . end"; }

# cl_fire <port> <tenant|-> <n> <nonce> <outdir> [<extra curl args>...]: n
# requests in the background, held at the backend by nonce. Each writes
# <outdir>/<nonce>-<i>.{code,hdr,body}.
cl_fire() {
  local port=$1 tenant=$2 n=$3 nonce=$4 out=$5 i key=""; shift 5
  [ "$tenant" != - ] && { source ./keys.env; eval key=\$KEY_${tenant//-/_}; }
  mkdir -p "$out"
  for i in $(seq 1 "$n"); do
    $hexec tqh1 curl -s -m "${CL_TIMEOUT:-60}" -D "$out/$nonce-$i.hdr" -o "$out/$nonce-$i.body" \
      -w '%{http_code} %{time_total}\n' -H 'Content-Type: application/json' \
      ${key:+-H "X-Api-Key: $key"} -H "X-Test-Nonce: $nonce" -H 'X-Test-Hold: 1' "$@" \
      -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$nonce\"}],\"max_tokens\":4}" \
      "http://$VIP:$port/v1/chat/completions" > "$out/$nonce-$i.code" 2>&1 &
  done
}
cl_release() { local p; for p in 8080 8081; do $hexec tqep1 curl -s -m 3 "http://127.0.0.1:$p/__release/$1" >/dev/null; done; }
cl_release_all() { local p; for p in 8080 8081; do $hexec tqep1 curl -s -m 3 "http://127.0.0.1:$p/__release_all" >/dev/null; done; }
cl_receipts() { local p n=0 c; for p in 8080 8081; do c=$($hexec tqep1 curl -s -m 3 "http://127.0.0.1:$p/__receipts/$1" | jq -r '.count // 0'); n=$((n + c)); done; echo $n; }
# cl_codes <outdir> <nonce> -> the sorted status codes
cl_codes() { cat "$1/$2"-*.code 2>/dev/null | awk '{print $1}' | sort | tr '\n' ' '; }
