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
PORT_H2=8087          # keyed, h2c backends: ceiling 2 (streams never queue)
SCHED_MAX=8
SCHED_DEPTH=16
SCHED_WAIT_MS=30000
PG_NAME=tenantq-pg-ai-tenant
PG_OWNER=oamuser
PG_OWNER_PW=oampass
PG_DB=loxilb
DP_PW=dp-secret-1
MGMT_PW=mgmt-secret-1
TENANTS="t-crit:critical t-crit2:critical t-crit3:critical t-a:standard t-b:standard t-c:standard t-shed:sheddable"

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
# tq_rule_del_model <port>: delete a rule made by tq_rule_json. It names a host and a
# model, its path prefix and match mode, which are part of the rule key: the plain
# address path would answer 404.
# Prints the answer ("200 ..." when the rule was deleted).
tq_rule_del_model() {
  local enc
  enc=$(python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=''))" "$MODEL")
  tq_api DELETE "/config/loadbalancer/hosturl/$VIP/externalipaddress/$VIP/port/$1/protocol/tcp?model_name=$enc&path_prefix=%2F&path_match_mode=prefix"
}

SCHED_JSON=", \"api_key_auth\": \"required\", \"fc_max_outstanding\": $SCHED_MAX, \"fc_max_queue_depth\": $SCHED_DEPTH, \"fc_max_queue_wait_ms\": $SCHED_WAIT_MS"
IDLE_JSON=", \"fc_max_outstanding\": 1, \"fc_max_queue_depth\": 4, \"fc_max_queue_wait_ms\": 5000"
KNOB_JSON=", \"fc_max_outstanding\": 4, \"fc_max_queue_depth\": 8, \"fc_max_queue_wait_ms\": 5000"

H2_JSON=", \"api_key_auth\": \"required\", \"backend_protocol\": \"http2\", \"fc_max_outstanding\": 2"

tq_add_rules() {
  tq_rule $PORT_SCHED "$SCHED_JSON" 8080 8081 || return 1
  tq_rule $PORT_H2 "$H2_JSON" 9090 9091 || return 1
  # The idle bound is the case: the rule's own inactiveTimeOut, 2 s.
  tq_rule $PORT_IDLE "$IDLE_JSON, \"inactiveTimeOut\": 2" 8080 || return 1
  tq_rule $PORT_KNOB "$KNOB_JSON" 8080 8081 || return 1
}

# Tenants with their class, one key each; keys.env holds KEY_<tenant>.
# priority_class is a cap, not a class (docs/load-balancing/19): a request
# with no hint runs at its tenant's default objective, standard when it has
# none. A critical tenant therefore gets the objective tq-critical
# (priority 1) as its default; a sheddable cap lowers by itself.
tq_add_tenants() {
  local t name cls r key def
  : > keys.env
  r=$(tq_api POST /config/ai/objective '{"name":"tq-critical","priority":1}')
  case "$r" in 20*) ;; *) echo "FATAL: objective tq-critical: $r"; return 1 ;; esac
  for t in $TENANTS; do
    name=${t%%:*}; cls=${t##*:}
    def=""; [ "$cls" == critical ] && def=',"default_objective":"tq-critical"'
    r=$(tq_api POST /config/ai/tenant/ratelimit "{\"tenant_id\":\"$name\",\"priority_class\":\"$cls\"$def}")
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
# One mock_backend.py process serves both ports and keeps one receipt count
# per nonce across them: ask one port.
cl_receipts() { $hexec tqep1 curl -s -m 3 "http://127.0.0.1:8080/__receipts/$1" | jq -r '.count // 0'; }
# cl_codes <outdir> <nonce> -> the sorted status codes
cl_codes() { cat "$1/$2"-*.code 2>/dev/null | awk '{print $1}' | sort | tr '\n' ' '; }

# ---- the scheduling pool, re-shaped per case -----------------------------------
# sched_cfg <max> <depth> <wait-ms> [<extra-json>]: a gate-only replace of the
# keyed pool (applied in place). Band knobs not named keep their value, so a
# case that sets one resets it explicitly. An identical replace is answered
# 409 lbrule-exists: the pool already runs that shape.
sched_cfg() {
  local r
  r=$(tq_api POST /config/loadbalancer "$(tq_rule_json $PORT_SCHED ", \"api_key_auth\": \"required\", \"fc_max_outstanding\": $1, \"fc_max_queue_depth\": $2, \"fc_max_queue_wait_ms\": $3${4:-}" 8080 8081)")
  case "$r" in 200*) return 0 ;; 409*lbrule-exists*|409*exists*) return 0 ;; esac
  echo "  sched_cfg $*: $(echo "$r" | cut -c1-200)"; return 1
}
# pool_cfg <port> <max> <depth> <wait-ms> [<extra-json>]: sched_cfg for any keyed pool.
pool_cfg() {
  local port=$1 r; shift
  r=$(tq_api POST /config/loadbalancer "$(tq_rule_json $port ", \"api_key_auth\": \"required\", \"fc_max_outstanding\": $1, \"fc_max_queue_depth\": $2, \"fc_max_queue_wait_ms\": $3${4:-}" 8080 8081)")
  case "$r" in 200*) return 0 ;; 409*lbrule-exists*|409*exists*) return 0 ;; esac
  echo "  pool_cfg $port $*: $(echo "$r" | cut -c1-200)"; return 1
}
SCHED_RESET=', "fc_shed_queue": "inherit", "fc_wake_policy": "inherit", "fc_tenant_max_share_pct": 0'

# cl_one <port> <tenant> <nonce> <outdir> [<curl args>...]: one held request,
# its own nonce, so a case can release requests one at a time.
cl_one() { local port=$1 tenant=$2 nonce=$3 out=$4; shift 4; cl_fire "$port" "$tenant" 1 "$nonce" "$out" "$@"; }
# cl_fast <port> <tenant> <nonce> <outdir> <delay-ms> [<curl args>...]: one request
# the backend answers after <delay-ms> (not held). The time it ended is written
# beside the answer (<nonce>-1.end, epoch seconds), so the order in which a pool
# served its waiters, one at a time, can be read back (cl_order).
cl_fast() {
  local port=$1 tenant=$2 nonce=$3 out=$4 delay=$5 key=""; shift 5
  [ "$tenant" != - ] && { source ./keys.env; eval key=\$KEY_${tenant//-/_}; }
  mkdir -p "$out"
  ( $hexec tqh1 curl -s -m "${CL_TIMEOUT:-60}" -D "$out/$nonce-1.hdr" -o "$out/$nonce-1.body" \
      -w '%{http_code} %{time_total}\n' -H 'Content-Type: application/json' \
      ${key:+-H "X-Api-Key: $key"} -H "X-Test-Nonce: $nonce" -H "X-Test-Delay-Ms: $delay" "$@" \
      -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$nonce\"}],\"max_tokens\":4}" \
      "http://$VIP:$port/v1/chat/completions" > "$out/$nonce-1.code" 2>&1
    date +%s.%N > "$out/$nonce-1.end" ) &
}
# cl_order <outdir> <nonce-glob>: the nonces, in the order their answers ended
cl_order() {
  local f
  for f in "$1"/$2-1.end; do [ -f "$f" ] && echo "$(cat "$f") $(basename "$f" -1.end)"; done | sort -n | awk '{print $2}'
}
# cl_errs <outdir> <nonce-glob> -> "<status>:<error>" per response, sorted
cl_errs() {
  local f c e
  for f in "$1"/$2-*.code; do
    [ -f "$f" ] || continue
    c=$(awk '{print $1}' "$f"); e=$(jq -r '.error // empty' "${f%.code}.body" 2>/dev/null)
    echo "$c${e:+:$e}"
  done | sort | tr '\n' ' ' | sed 's/ $//'
}
# wait_receipts <nonce> <n> [<secs>]: poll the backends' receipt count
wait_receipts() {
  local i got
  for i in $(seq 1 $((${3:-5} * 4))); do
    got=$(cl_receipts "$1"); [ "$got" -ge "$2" ] 2>/dev/null && { echo "$got"; return; }
    sleep 0.25
  done
  echo "$got"
}
# wait_done <outdir> <nonce-glob> <n> [<secs>]: wait for n responses
wait_done() {
  local i n
  for i in $(seq 1 $((${4:-10} * 4))); do
    n=$(cat "$1"/$2-*.code 2>/dev/null | grep -c '^[0-9]')
    [ "$n" -ge "$3" ] && return 0
    sleep 0.25
  done
  return 1
}

# ---- the band counter -----------------------------------------------------------
metrics_raw() { $hexec tqh1 curl -s --max-time 8 "$API/metrics" 2>/dev/null; }
# bt_snap <port>: one line of "key=value" for the service: series=<n of
# loxilb_ai_fc_band_total series>, <band>.<outcome>=<v> for each, qw=<queue-
# wait histogram count: one sample per park that ended, however it ended>,
# and d.<reason>=<v> for the gate decisions summed over its pools;
# and rs=<HTTP/1.1 responses recorded as received by a client>, the
# gateway-wide responses-by-status counter (it has no service label); rc, rd,
# ra = loxilb_ai_requests_total completed, denied, aborted, and rac its
# aborted with cause client (gateway-wide too).
bt_snap() {
  metrics_raw | awk -v svc="service=\"$VIP:$1\"" '
    /^loxilb_proxy_http_responses_by_status_total\{/ { rs += $NF; next }
    /^loxilb_ai_requests_total\{.*outcome="completed"/ { rc += $NF; next }
    /^loxilb_ai_requests_total\{.*outcome="denied"/ { rd += $NF; next }
    /^loxilb_ai_requests_total\{.*outcome="aborted"/ {
      ra += $NF; if ($0 ~ /cause="client"/) rac += $NF; next }
    index($0, svc) == 0 { next }
    /^loxilb_ai_fc_band_total\{/ {
      match($0, /band="[a-z]+"/); b = substr($0, RSTART + 6, RLENGTH - 7)
      match($0, /outcome="[a-z]+"/); o = substr($0, RSTART + 9, RLENGTH - 10)
      v[b "." o] += $NF; n++; next }
    /^loxilb_ai_admission_queue_wait_seconds_count\{/ { qw += $NF; next }
    /^loxilb_ai_admission_decisions_total\{/ {
      match($0, /reason="[a-z_]+"/); d["d." substr($0, RSTART + 8, RLENGTH - 9)] += $NF; next }
    END { printf "series=%d qw=%d rs=%d rc=%d rd=%d ra=%d rac=%d", n, qw, rs, rc, rd, ra, rac
          for (k in v) printf " %s=%d", k, v[k]
          for (k in d) printf " %s=%d", k, d[k]
          print "" }'
}
declare -A BT0 BT1 BD
bt_parse() { # bt_parse <array-name> <snapshot>
  local -n a=$1; local kv
  a=()
  for kv in $2; do a[${kv%%=*}]=${kv#*=}; done
}
# The collector republishes every 10 s: a reading taken 11 s after the last
# request of a case has seen all of it. The start of a case settles too: the
# responses counter is gateway-wide, and a request the previous case sent
# after its own reading (a check that the pool is idle again, a stream left
# to time out) is published only by the next republish.
bt_settle() { sleep 11; }
# bt_begin <port> [h1|h2] [noai]: h2 when the service's clients speak HTTP/2;
# noai when the service is not in AI-gateway mode, whose requests the data
# plane never records in loxilb_ai_requests_total (proxy_owes_completion)
bt_begin() { bt_settle; bt_parse BT0 "$(bt_snap "$1")"; BT_PORT=$1; BT_PROTO=${2:-h1}; BT_AI=${3:-ai}; }
bt_end() {
  local k
  bt_settle
  bt_parse BT1 "$(bt_snap "$BT_PORT")"
  BD=()
  for k in "${!BT1[@]}"; do BD[$k]=$(( ${BT1[$k]:-0} - ${BT0[$k]:-0} )); done
}
bd() { echo "${BD[$1]:-0}"; }
# bt_check <case> <c:s:b requests that ended in a band outcome> <evicted>
# [<parked requests that ended admitted>]: the identities, read from the
# counter's deltas over the case.
#   per band: admitted + shed + expired + cancelled = the band's requests
#   queued (all bands) = queue-wait samples: the histogram takes one sample
#                        when a park ends (admitted, expired, evicted or
#                        cancelled), the queued count is taken at the park:
#                        two writers, every park ended once
#   queued (all bands) = the parks that ended: admitted after a park +
#                        expired + evicted + cancelled, when the case knows
#                        how many of its parked requests were admitted
#   admitted + shed + expired (all bands) + refused - left = the responses
#                        recorded as received: every terminal outcome but
#                        cancelled is an answer its client received (the
#                        backend's reply, or the gateway's own 429 or 504),
#                        recorded once; a cancelled request's client left
#                        before any answer. refused = the gate's refusals that
#                        are no band outcome (queue full, tenant share,
#                        capacity, no healthy capacity, a drain), each
#                        answered; left = admitted requests whose client left
#                        before the backend answered (BT_LEFT, from the case).
#                        HTTP/1.1 only: HTTP/2 has no class counter.
#   completed + denied + aborted = admitted + shed + expired + refused +
#                        cancelled: every request that reached the gate
#                        leaves one request record. A cancelled request (its
#                        client left while it waited) is aborted, cause
#                        client, at its teardown; so is an admitted one whose
#                        client left before the answer: cancelled <= aborted
#                        with cause client <= cancelled + left. The band
#                        outcome is the per-band split of the gate's
#                        decisions beside it, not a second record.
BT_REFUSED="capacity_shed queue_full tenant_share no_healthy_capacity draining drained"
bt_check() {
  local id=$1 want=$2 ev=$3 b r i=0 tq=0 te=0 tc=0 ta=0 got rs=${4:-}
  local -a w=(${want//:/ })
  chk "$id" "fifteen band series for the service" 15 "${BT1[series]:-0}"
  for b in critical standard sheddable; do
    got=$(( $(bd $b.admitted) + $(bd $b.shed) + $(bd $b.expired) + $(bd $b.cancelled) ))
    chk "$id" "$b: admitted + shed + expired + cancelled = its requests" "${w[$i]}" "$got"
    tq=$((tq + $(bd $b.queued))); te=$((te + $(bd $b.expired))); tc=$((tc + $(bd $b.cancelled)))
    ta=$((ta + $(bd $b.admitted) + $(bd $b.shed) + $(bd $b.expired)))
    i=$((i + 1))
  done
  local refused=0 adm=0 lo
  for r in $BT_REFUSED; do refused=$((refused + $(bd d.$r))); done
  for b in critical standard sheddable; do adm=$((adm + $(bd $b.admitted))); done
  ta=$((ta + refused))
  if [ "$BT_PROTO" == h1 ]; then
    chk "$id" "admitted + shed + expired + refused - left = responses recorded as received" "$((ta - ${BT_LEFT:-0}))" "$(bd rs)"
    # A refusal is counted once, as denied, never also as completed: every
    # band refusal is one outcome="denied" request, and the completed count
    # is the admitted requests (less those whose client left, when the
    # case says how many: whether such a request completes is not asserted).
    if [ "$BT_AI" == noai ]; then
      # Outside the AI gateway neither outcome is written: the family does
      # not move at all over the case (nothing else runs in the window).
      chk "$id" "outside the AI gateway the AI request family does not move (completed, denied, aborted)" "0 0 0" "$(bd rc) $(bd rd) $(bd ra)"
    else
      chk "$id" "a refusal is counted once: denied = shed + expired + refused" "$((ta - adm))" "$(bd rd)"
      lo=$((adm - ${BT_LEFT:-0}))
      chk "$id" "and never also as completed: admitted - left <= completed <= admitted" yes \
        "$([ "$(bd rc)" -ge "$lo" ] && [ "$(bd rc)" -le "$adm" ] && echo yes || echo "no (completed $(bd rc), admitted $adm, left ${BT_LEFT:-0})")"
      chk "$id" "one request record per request: completed + denied + aborted = the requests" "$((ta + tc))" "$(( $(bd rc) + $(bd rd) + $(bd ra) ))"
      chk "$id" "a cancelled request is aborted by its client: cancelled <= aborted (client) <= cancelled + left" yes \
        "$([ "$(bd rac)" -ge "$tc" ] && [ "$(bd rac)" -le "$((tc + ${BT_LEFT:-0}))" ] && echo yes || echo "no (aborted client $(bd rac), cancelled $tc, left ${BT_LEFT:-0})")"
    fi
  fi
  chk "$id" "queued = queue-wait samples (one per park, at its end)" "$tq" "$(bd qw)"
  [ -n "$rs" ] && chk "$id" "queued = admitted after a park + expired + evicted + cancelled" "$tq" "$(( rs + te + ev + tc ))"
  return 0
}
# bt_row <band>: "admitted queued shed expired cancelled" deltas
bt_row() { echo "$(bd $1.admitted) $(bd $1.queued) $(bd $1.shed) $(bd $1.expired) $(bd $1.cancelled)"; }
