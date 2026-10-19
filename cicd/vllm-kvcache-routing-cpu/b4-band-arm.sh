#!/bin/bash
# b4-band-arm.sh — campaign case B4 (tenant QoS band queue, step 3) as an arm
# of this scenario: a critical request parked behind a full capacity ceiling,
# then woken, is routed by KV-exact prefix selection to the same prefill
# endpoint as the same prompt unparked; nothing is held afterwards.
#
# Run (on the bed, LOXILB_DOCKER_IMAGE = the image under test):
#   MON_AIKEY=1 ./config.sh && ./b4-band-arm.sh; ./rmconfig.sh
# MON_AIKEY=1 because a class needs a tenant from the key store; in that mode
# the config REST wants the admin JWT (config.sh made the admin user), so this
# arm logs in itself and validation.sh is not run (it is unauthenticated).
#
# Oracles: the prefill choice is not client-visible in the P/D flow (the client
# sees the decode echo), so the selection proof is the scenario's own,
# loxilb_pd_kv_tier15_hits_total{ep_idx} delta on the expected prefill index,
# with the decode banner (serverD*) proving the P/D flow ran. The ceiling is
# held by putting every decode endpoint behind pd-fault-swap.sh slowok (the
# only mode that holds a connection open without failing it). Band identities
# from loxilb_ai_fc_band_total. The plan's "kv_sr_load_held balanced" names
# the single-role KV unit; this rule is P/D, whose per-endpoint loads have no
# read-back: what is read is nothing held at the gate afterwards and the same
# prompt, idle again, selecting the same prefill endpoint.
source ../common.sh
echo SCENARIO-vllm-kvcache-b4-band-arm
CFGDIR="$(cd "$(dirname "$0")" && pwd)"
VIP="10.10.10.254"; VPORT=8080
API="http://localhost:11111/netlox/v1"
KV_ZMQ_PORT=5557; KV_HASH_ALGO="sha256_cbor"; KV_BLOCK_SIZE=16; KV_WARMUP_SEC=20   # config.sh's value: a different one is a non-gate change that re-creates the pool
KV_MODEL="${KV_MODEL:-Qwen/Qwen3-0.6B}"; KV_MODEL_SLUG="${KV_MODEL//\//__}"
TOKENIZER_SRC="${CFGDIR}/../common/kv_hash/fixtures/tokenizers/${KV_MODEL_SLUG}/tokenizer.json"
VECTORS_SRC="${CFGDIR}/../common/kv_hash/fixtures/kv_hash_vectors.json"
PUBLISHER="${CFGDIR}/kv_event_publisher.py"
CORPUS="${CFGDIR}/prompts/corpus.json"
PUB_TAG="kvpub80"
PY_USER_SITE="$(python3 -m site --user-site 2>/dev/null || echo '')"
EP_B_IP="33.33.33.1"; EP_B_IDX=2
HOLD_SEC=${B4_HOLD_SEC:-6}
OUT=${OUT:-$CFGDIR/artifacts-b4}; rm -rf "$OUT"; mkdir -p "$OUT"
code=0
chk() { if [ "$2" == "$3" ]; then echo "  [PASS] B4 $1 ($3)"; else echo "  [FAIL] B4 $1: want '$2', got '$3'"; code=1; fi; }

# ---- the admin JWT --------------------------------------------------------------
TOKEN=$($hexec llb1 curl -s -X POST "$API/auth/login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"Admin123!"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null)
[ -n "$TOKEN" ] || { echo "SCENARIO-vllm-kvcache-b4-band-arm [FAILED] no admin JWT (run config.sh with MON_AIKEY=1)"; exit 1; }
api() { local m=$1 p=$2; shift 2; $hexec llb1 curl -s -m 10 -X "$m" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@" "$API$p"; }
metrics() { api GET /metrics; }
msum() { metrics | awk -v f="$1" -v a="${2:-}" -v b="${3:-}" '$0 ~ "^" f "[{ ]" { if (a != "" && index($0, a) == 0) next; if (b != "" && index($0, b) == 0) next; s += $NF } END { printf "%.0f", s + 0 }'; }
tier15_hits() { msum loxilb_pd_kv_tier15_hits_total "ep_idx=\"$1\""; }
fce() { api GET /config/loadbalancer/all | jq -r ".lbAttr[] | select(.serviceArguments.port==$VPORT) | .serviceArguments.fc_effective$1 | if . == null then \"none\" else . end"; }

# ---- the scenario's publish, with the JWT on its inventory reads -----------------
netns_for_ep_ip() { case "$1" in 31.31.31.1) echo l3ep1;; 33.33.33.1) echo l3ep3;; 35.35.35.1) echo l3ep5;; esac; }
SERVICE_ID=""
for _sid in 0 1 2 3 4 5 6 7 8; do
  api GET "/config/ai/kv/inventory?service_id=${_sid}&ep_idx=0" | grep -q '"service_id"' && { SERVICE_ID=$_sid; break; }
done
SERVICE_ID=${SERVICE_ID:-1}
inv_total() { api GET "/config/ai/kv/inventory?service_id=${SERVICE_ID}&ep_idx=$1" | grep -Eo '"total":[0-9]+' | grep -Eo '[0-9]+' | head -1; }
publish_prompt_to_ep() { # <prompt-id> <ep-ip>: this EP's inventory = that prompt's blocks
  local pid=$1 ep_ip=$2 one="$OUT/.kvpub-$1-$2.json" ns before cur seen=0 w
  python3 -c "import json,sys
d=json.load(open('${CORPUS}'))
for p in d['prompts']:
    if p['id']==sys.argv[1]: json.dump([{'prompt':p['prompt']}], open(sys.argv[2],'w')); break" "$pid" "$one"
  ns=$(netns_for_ep_ip "$ep_ip"); before=$(inv_total $EP_B_IDX)
  for _pp in $(pgrep -f "${PUB_TAG}" 2>/dev/null); do kill "$_pp" >/dev/null 2>&1 || true; done
  sleep 1
  setsid $hexec "$ns" bash -c "export PYTHONPATH='${PY_USER_SITE}' PYTHONHASHSEED=0; exec -a ${PUB_TAG} python3 '${PUBLISHER}' \
    --corpus '${one}' --tokenizer '${TOKENIZER_SRC}' --vectors '${VECTORS_SRC}' \
    --bind '${ep_ip}' --port ${KV_ZMQ_PORT} --algo ${KV_HASH_ALGO} \
    --block-size ${KV_BLOCK_SIZE} --repeat 3 --repeat-interval 6 --no-vocabulary" > "$OUT/kvpub-$pid.log" 2>&1 &
  for w in $(seq 1 24); do
    cur=$(inv_total $EP_B_IDX); [ "$cur" != "$before" ] && seen=1
    [ "$seen" == 1 ] && [ "${cur:-0}" -gt 0 ] && return 0
    sleep 1
  done
  echo "  WARN: inventory change on $ep_ip not observed"
}
prompt_body() { python3 -c "import json,sys
d=json.load(open('${CORPUS}'))
for p in d['prompts']:
    if p['id']==sys.argv[1]: print(json.dumps({'model':sys.argv[2],'prompt':p['prompt'],'max_tokens':8})); break" "$1" "$KV_MODEL"; }
req() { # req <tag> <key> <prompt-id|raw:text>: one request in the background -> $OUT/<tag>.{code,body}
  local body
  case "$3" in raw:*) body=$(python3 -c "import json,sys; print(json.dumps({'model':sys.argv[1],'prompt':sys.argv[2],'max_tokens':8}))" "$KV_MODEL" "${3#raw:}");;
               *) body=$(prompt_body "$3");; esac
  $hexec l3h1 curl -s -m 60 -o "$OUT/$1.body" -w '%{http_code} %{time_total}\n' -X POST "http://$VIP:$VPORT/v1/completions" \
    -H 'Content-Type: application/json' -H "X-Api-Key: $2" --data-binary "$body" > "$OUT/$1.code" 2>&1 &
}
# The decode leg's answer: its server's banner, or, while pd-fault-swap.sh
# slowok holds the decode endpoints, the fault stub's control completion
# (the stub stands only on decode endpoints).
banner() { grep -q '"chatcmpl-pd-fault-control"' "$OUT/$1.body" && { echo serverD-stub; return; }; grep -Eo 'server[PD][0-9]' "$OUT/$1.body" | head -1; }
band() { msum loxilb_ai_fc_band_total "service=\"$VIP:$VPORT\"" "band=\"$1\",outcome=\"$2\"" ; }
qwait() { msum loxilb_ai_admission_queue_wait_seconds_count "service=\"$VIP:$VPORT\""; }
nseries() { metrics | grep '^loxilb_ai_fc_band_total{' | grep -c "service=\"$VIP:$VPORT\""; }
# The two identities over the parked section (lib/classload.sh bt_check in
# cicd/ai-tenant): per band admitted + shed + expired + cancelled = its
# requests, and queued = resumed + expired + cancelled (no eviction here).
band_snap() { local b o; for b in critical standard sheddable; do for o in admitted queued shed expired cancelled; do echo -n "$(band $b $o) "; done; done; qwait; }

# ---- tenants and keys -----------------------------------------------------------
# priority_class is a cap: a critical tenant runs critical through its
# default objective (docs/load-balancing/19).
api POST /config/ai/objective -d '{"name":"tq-b4-critical","priority":1}' >/dev/null
key_for() { # key_for <tenant> <class>
  local def=""; [ "$2" == critical ] && def=',"default_objective":"tq-b4-critical"'
  api POST /config/ai/tenant/ratelimit -d "{\"tenant_id\":\"$1\",\"priority_class\":\"$2\"$def}" >/dev/null
  api POST /config/ai/apikey -d "{\"tenant_id\":\"$1\",\"name\":\"$1-b4\",\"allowed_models\":[\"$KV_MODEL\"],\"rate_limit_rps\":10000,\"burst_size\":10000,\"enabled\":true}" | jq -r '.raw_key // empty'
}
KEY_C=$(key_for tenantq-b4-crit critical); KEY_S=$(key_for tenantq-b4-std standard)
chk "keys for a critical and a standard tenant" yes "$([ -n "$KEY_C" ] && [ -n "$KEY_S" ] && echo yes || echo no)"

# ---- the rule with a ceiling of one and a queue -----------------------------------
rule() { # rule <extra-json>: config.sh's KV-exact rule, api_key_auth on
  cat <<JSON
{"serviceArguments": {"externalIP": "${VIP}", "port": ${VPORT}, "protocol": "tcp", "sel": 0, "mode": 4,
  "host": "${VIP}", "model_name": "${KV_MODEL}", "api_key_auth": "required", "pd_disagg_mode": true,
  "probeRetries": 1, "kvExactMode": 1, "kvZmqPort": ${KV_ZMQ_PORT}, "kvHashAlgo": "${KV_HASH_ALGO}",
  "kvWarmupSec": ${KV_WARMUP_SEC}, "kvBlockSize": ${KV_BLOCK_SIZE}$1},
 "endpoints": [
  {"endpointIP": "31.31.31.1", "targetPort": 80, "weight": 1, "ep_role": 1},
  {"endpointIP": "32.32.32.1", "targetPort": 80, "weight": 1, "ep_role": 2},
  {"endpointIP": "33.33.33.1", "targetPort": 80, "weight": 1, "ep_role": 1},
  {"endpointIP": "34.34.34.1", "targetPort": 80, "weight": 1, "ep_role": 2},
  {"endpointIP": "35.35.35.1", "targetPort": 80, "weight": 1, "ep_role": 1},
  {"endpointIP": "36.36.36.1", "targetPort": 80, "weight": 1, "ep_role": 2}]}
JSON
}
# This scenario's gateway runs with the process default fc_mode (off: the
# gate counts nothing and holds nothing), so the rule says enforce.
R=$(api POST /config/loadbalancer -o /dev/null -w '%{http_code}' -d "$(rule ', "fc_mode": "enforce", "fc_max_outstanding": 1, "fc_max_queue_depth": 4, "fc_max_queue_wait_ms": 30000')")
chk "the KV-exact rule takes a ceiling of 1 and a queue" 200 "$R"
chk "and runs it, enforcing" "1 4 enforce" "$(fce .max_outstanding) $(fce .queue_depth) $(fce .mode)"

# ---- every decode endpoint holds a request HOLD_SEC seconds -----------------------
for ns in l3ep2 l3ep4 l3ep6; do sudo STUB_DELAY="$HOLD_SEC" "$CFGDIR/pd-fault-swap.sh" "$ns" slowok | sed 's/^/  /'; done

# ---- the prompt's home: EP-B holds every block of shared-prefix-base ---------------
publish_prompt_to_ep shared-prefix-base "$EP_B_IP"
echo "--- unparked"
h0=$(tier15_hits $EP_B_IDX)
req unparked "$KEY_C" shared-prefix-base; wait
chk "unparked: the P/D flow answered (decode banner)" "200 D" "$(awk '{print $1}' "$OUT/unparked.code") $(banner unparked | cut -c7)"
chk "unparked: KV-exact selected EP-B" 1 "$(( $(tier15_hits $EP_B_IDX) - h0 ))"

echo "--- parked, then woken"
# The baselines are read after the unparked request's own counts have reached
# the scrape, like B0; read earlier, they can miss its admission.
sleep 11; B0=($(band_snap))
c0=$(band critical queued); a0=$(band critical admitted); s0=$(band standard admitted)
req filler "$KEY_S" "raw:tenantq b4 filler that shares no block with the corpus"; sleep 1
chk "the filler holds the one unit" "1 0" "$(fce .inflight) $(fce .queued)"
h0=$(tier15_hits $EP_B_IDX)
req woken "$KEY_C" shared-prefix-base; sleep 1
chk "the critical request waits at the gate" "1 0" "$(fce .bands.critical.queued) $(( $(tier15_hits $EP_B_IDX) - h0 ))"
wait
chk "both answered 200 through the P/D flow" "200 D 200 D" \
  "$(awk '{print $1}' "$OUT/filler.code") $(banner filler | cut -c7) $(awk '{print $1}' "$OUT/woken.code") $(banner woken | cut -c7)"
chk "woken: KV-exact selected EP-B, as unparked" 1 "$(( $(tier15_hits $EP_B_IDX) - h0 ))"
sleep 11
chk "the band counter: critical queued 1, admitted 1; standard admitted 1" "1 1 1" \
  "$(( $(band critical queued) - c0 )) $(( $(band critical admitted) - a0 )) $(( $(band standard admitted) - s0 ))"
B1=($(band_snap)); D=(); for i in "${!B1[@]}"; do D[$i]=$(( B1[i] - B0[i] )); done
chk "fifteen band series for the service" 15 "$(nseries)"
# D: per band admitted queued shed expired cancelled (5 each), then queue-wait count
chk "identity 1: per band admitted + shed + expired + cancelled = its requests (c:s:b)" "1 1 0" \
  "$(( D[0] + D[2] + D[3] + D[4] )) $(( D[5] + D[7] + D[8] + D[9] )) $(( D[10] + D[12] + D[13] + D[14] ))"
chk "identity 2: queued = resumed + expired + cancelled" "$(( D[1] + D[6] + D[11] ))" \
  "$(( D[15] + D[3] + D[8] + D[13] + D[4] + D[9] + D[14] ))"

echo "--- afterwards"
chk "nothing held at the gate" "0 0" "$(fce .inflight) $(fce .queued)"
for ns in l3ep2 l3ep4 l3ep6; do sudo "$CFGDIR/pd-fault-swap.sh" "$ns" off | sed 's/^/  /'; done
h0=$(tier15_hits $EP_B_IDX)
req again "$KEY_C" shared-prefix-base; wait
chk "idle again, the prompt selects EP-B" 1 "$(( $(tier15_hits $EP_B_IDX) - h0 ))"
api POST /config/loadbalancer -o /dev/null -d "$(rule ', "fc_mode": "inherit", "fc_max_outstanding": 0, "fc_max_queue_depth": 0, "fc_max_queue_wait_ms": 0')"

[ $code -eq 0 ] && echo "SCENARIO-vllm-kvcache-b4-band-arm [OK]" || echo "SCENARIO-vllm-kvcache-b4-band-arm [FAILED]"
exit $code
