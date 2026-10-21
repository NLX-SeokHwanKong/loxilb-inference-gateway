#!/bin/bash
# lib/cases-b.sh — the group-B cases of cicd/ai-tenant: the band queue beside
# P/D, stateless MCP, CHWBL bounded load, and backend connect failures. One
# function per case, called from validation-sched.sh after the group-A cases;
# each uses chk / has / pass / fail from there, the helpers of
# lib/classload.sh and the topology of lib/topo-b.sh. Every case reads the
# band counter's deltas over itself on its own service (bt_begin / bt_end)
# and asserts both identities (bt_check).
#
# Not here: B4, an arm of cicd/vllm-kvcache-routing-cpu (b4-band-arm.sh).
# B15 restarts the gateway twice (the legacy P/D queue is environment-only):
# run it last.

B3=$OUT/groupb; mkdir -p "$B3"

# ---- helpers -------------------------------------------------------------------
# A gauge read by polling: the collector republishes every 10 s.
bm_sum() { # bm_sum <family> <label-substr>...
  local fam=$1; shift
  metrics_raw | awk -v fam="$fam" -v a="${1:-}" -v b="${2:-}" '
    $0 ~ "^" fam "[{ ]" { if (a != "" && index($0, a) == 0) next
                          if (b != "" && index($0, b) == 0) next; s += $NF }
    END { printf "%.0f", s + 0 }'
}
bm_wait() { # bm_wait <want> <secs> <family> <label-substr>...
  local want=$1 secs=$2 got i; shift 2
  for i in $(seq 1 "$secs"); do got=$(bm_sum "$@"); [ "$got" == "$want" ] && break; sleep 1; done
  echo "$got"
}
svcl() { echo "service=\"$VIP:$1\""; }
# P/D mock receipts: <role-port> <nonce>
pd_receipts() { $hexec tqep1 curl -s -m 3 "http://127.0.0.1:$1/__receipts/$2" | jq -r '.count // 0'; }
pd_wait() { # pd_wait <role-port> <nonce> <n> [<secs>]
  local i got
  for i in $(seq 1 $((${4:-5} * 4))); do got=$(pd_receipts "$1" "$2"); [ "$got" -ge "$3" ] 2>/dev/null && break; sleep 0.25; done
  echo "$got"
}
pd_release() { $hexec tqep1 curl -s -m 3 "http://127.0.0.1:8181/__release/$1" >/dev/null; }
pd_release_all() { $hexec tqep1 curl -s -m 3 "http://127.0.0.1:8181/__release_all" >/dev/null; }
# The one endpoint port a single request of <nonce> ran on (mock_backend's
# per-port peak), or "none".
peak_port() { $hexec tqep1 curl -s -m 3 "http://127.0.0.1:8080/__peak/$1" | jq -r '.ports | keys | if length == 1 then .[0] else "none:" + (join(",")) end'; }
# b_req <port> <tenant> <nonce> <content> <outdir> [<curl args>]: one held
# request whose prompt is <content> (CHWBL hashes the prompt), named <nonce>.
b_req() {
  local port=$1 tenant=$2 nonce=$3 content=$4 out=$5 key; shift 5
  source ./keys.env; eval key=\$KEY_${tenant//-/_}
  mkdir -p "$out"
  $hexec tqh1 curl -s -m "${CL_TIMEOUT:-60}" -D "$out/$nonce-1.hdr" -o "$out/$nonce-1.body" \
    -w '%{http_code} %{time_total}\n' -H 'Content-Type: application/json' -H "X-Api-Key: $key" \
    -H "X-Test-Nonce: $nonce" -H 'X-Test-Hold: 1' "$@" \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$content\"}],\"max_tokens\":4}" \
    "http://$VIP:$port/v1/chat/completions" > "$out/$nonce-1.code" 2>&1 &
}
mcp_one() { # mcp_one <port> <id> <out> [<tenant>]: one held tools/call
  local port=$1 id=$2 out=$3 t=$4 key=""
  [ -n "$t" ] && { source ./keys-b.env; eval key=\$KEYB_${t//-/_}; }
  $hexec tqh1 python3 "$(pwd)/lib/mcp_call.py" "http://$VIP:$port/mcp" --id "$id" --progress \
    ${key:+--api-key "$key"} > "$out/$id.json" 2>&1 &
}
mcp_order() { jq -r 'select(.event == "received") | .request_id' "$MCP_JOURNAL" 2>/dev/null | grep "^$1" | tr '\n' ' ' | sed 's/ $//'; }

# ---- P9: a P/D service, the class rides the one service acquire -----------------
case_P9() {
  echo "=== P9: P/D: the class rides the one service acquire; one service unit per request"
  pd_release_all
  bt_begin $PORT_PD
  local adm0; adm0=$(bm_sum loxilb_ai_admission_decisions_total "$(svcl $PORT_PD)" 'reason="admitted"')
  cl_one $PORT_PD t-a p9run "$B3"
  chk P9 "both legs of the first request ran (prefill answered, decode held)" "1 1" "$(pd_wait 8180 p9run 1) $(pd_wait 8181 p9run 1)"
  chk P9 "one service unit for a request with two legs" 1 "$(tq_fce $PORT_PD .inflight)"
  chk P9 "service in flight 1, decode leg 1 (metrics)" "1 1" \
    "$(bm_wait 1 15 loxilb_ai_admission_inflight "$(svcl $PORT_PD)" 'role="service"') $(bm_wait 1 15 loxilb_ai_admission_inflight "$(svcl $PORT_PD)" 'role="decode"')"
  cl_one $PORT_PD t-b p9s "$B3"; sleep 0.3
  cl_one $PORT_PD t-crit p9c "$B3"; sleep 0.5
  chk P9 "a standard and a later critical request wait" "1 1" "$(tq_fce $PORT_PD .bands.standard.queued) $(tq_fce $PORT_PD .bands.critical.queued)"
  pd_release p9run
  chk P9 "the freed unit goes to the critical request, both legs" "1 1 0" "$(pd_wait 8180 p9c 1) $(pd_wait 8181 p9c 1) $(pd_receipts 8180 p9s)"
  chk P9 "still one service unit" 1 "$(tq_fce $PORT_PD .inflight)"
  pd_release p9c
  chk P9 "then the standard one" 1 "$(pd_wait 8181 p9s 1)"
  pd_release p9s; wait_done "$B3" 'p9*' 3
  chk P9 "all answered 200" "200 200 200" "$(cl_errs "$B3" 'p9*')"
  bt_end
  bt_check P9 1:2:0 0
  chk P9 "admitted once per request, not once per leg" "3 1 2" \
    "$(( $(bm_sum loxilb_ai_admission_decisions_total "$(svcl $PORT_PD)" 'reason="admitted"') - adm0 )) $(bd critical.admitted) $(bd standard.admitted)"
  chk P9 "nothing held after" "0 0" "$(tq_fce $PORT_PD .inflight) $(tq_fce $PORT_PD .queued)"
}

# ---- B6: P/D under the weighted policy: one service unit -------------------------
# Weighted orders nothing yet (step 6); what step 3 guarantees under it is
# the unit accounting of a P/D request and the band split of its counters.
case_B6() {
  echo "=== B6: P/D under fc_wake_policy weighted: one service unit per request"
  pd_release_all
  tqb_post ":$PORT_PD weighted" "$(tqb_pd_json ', "fc_wake_policy": "weighted", "fc_band_weights": "8:3:1"' | sed 's/"fc_max_outstanding": 1,/"fc_max_outstanding": 2,/')" || { fail B6 "weighted P/D rule refused"; return; }
  chk B6 "weighted is in force" "weighted rule" "$(tq_fce $PORT_PD .wake_policy_effective) $(tq_fce $PORT_PD .source.wake_policy)"
  bt_begin $PORT_PD
  cl_one $PORT_PD t-crit b6c "$B3"; cl_one $PORT_PD t-a b6s "$B3"
  chk B6 "both P/D requests reached their decode leg" "1 1" "$(pd_wait 8181 b6c 1) $(pd_wait 8181 b6s 1)"
  chk B6 "two requests, four legs, two service units" 2 "$(tq_fce $PORT_PD .inflight)"
  chk B6 "one unit in each band" "1 1" "$(tq_fce $PORT_PD .bands.critical.inflight) $(tq_fce $PORT_PD .bands.standard.inflight)"
  cl_one $PORT_PD t-b b6w "$B3"; sleep 0.5
  chk B6 "a third waits (ceiling 2)" "1 0" "$(tq_fce $PORT_PD .queued) $(pd_receipts 8180 b6w)"
  pd_release b6c; chk B6 "and runs on a freed unit" 1 "$(pd_wait 8181 b6w 1)"
  pd_release b6s; pd_release b6w; wait_done "$B3" 'b6*' 3
  chk B6 "all answered 200" "200 200 200" "$(cl_errs "$B3" 'b6*')"
  bt_end
  bt_check B6 1:2:0 0
  tqb_post ":$PORT_PD reset" "$(tqb_pd_json ', "fc_wake_policy": "inherit", "fc_band_weights": "0:0:0"')" || fail B6 "reset refused"
  chk B6 "nothing held after" "0 0" "$(tq_fce $PORT_PD .inflight) $(tq_fce $PORT_PD .queued)"
}

# ---- B3a: stateless MCP: standard, FIFO, no critical or sheddable counter -------
# The plan's B3a is the cicd/mcp-stateless FC scripts replayed on the branch
# against main (a runner step outside this scenario: it starts its own
# gateway). This is the band half seen from here: a keyless MCP pool moves
# only the standard row and wakes in arrival order.
case_B3a() {
  echo "=== B3a: stateless MCP: standard only, arrival order"
  bt_begin $PORT_MCP h1 noai
  local i
  for i in 1 2 3 4; do mcp_one $PORT_MCP b3a-$i "$B3"; sleep 0.3; done
  sleep 0.3
  chk B3a "one runs, three wait, all standard" "1 3 0 0" \
    "$(tq_fce $PORT_MCP .inflight) $(tq_fce $PORT_MCP .bands.standard.queued) $(tq_fce $PORT_MCP .bands.critical.queued) $(tq_fce $PORT_MCP .bands.sheddable.queued)"
  for i in $(seq 1 40); do [ "$(cat "$B3"/b3a-*.json 2>/dev/null | grep -c '"status"')" -ge 4 ] && break; sleep 0.5; done
  chk B3a "all four answered 200" "200 200 200 200" "$(jq -r .status "$B3"/b3a-*.json | sort | tr '\n' ' ' | sed 's/ $//')"
  chk B3a "the backend received them in arrival order" "b3a-1 b3a-2 b3a-3 b3a-4" "$(mcp_order b3a-)"
  bt_end
  bt_check B3a 0:4:0 0
  chk B3a "no critical or sheddable counter moved" "0 0 0 0 0 0 0 0 0 0" "$(bt_row critical) $(bt_row sheddable)"
  chk B3a "standard: admitted 4, queued 3" "4 3" "$(bd standard.admitted) $(bd standard.queued)"
}

# ---- B3b: MCP through the AI gateway with tenant classes ------------------------
# A plain MCP full proxy with ai_gw_mode never reaches a capacity unit for its
# tool calls (POST /mcp is not an inference path, fc_is_inference_request;
# only a stateless MCP pool's POSTs pass the gate). The faithful shape is a
# stateless MCP pool that also takes the key check (ai_gw_mode via
# api_key_auth). If the API refuses it, this case fails on that.
case_B3b() {
  echo "=== B3b: MCP with the AI gateway: the classes apply to tool calls"
  local r
  r=$(tq_rule_get $PORT_MCPK | jq -r '.port // empty')
  chk B3b "a stateless MCP pool with api_key_auth exists" "$PORT_MCPK" "$r"
  [ "$r" == "$PORT_MCPK" ] || return
  rm -f "$B3"/b3b-*.json
  bt_begin $PORT_MCPK
  # The fixture holds a call about 2 s (two progress events 1 s apart): the
  # waiters are read while the first call still runs, then the sheddable
  # call is sent.
  mcp_one $PORT_MCPK b3b-run "$B3" t-a; sleep 0.5
  mcp_one $PORT_MCPK b3b-s1 "$B3" t-a; sleep 0.3
  mcp_one $PORT_MCPK b3b-c1 "$B3" t-crit; sleep 0.3
  chk B3b "one standard and one critical wait" "1 1" "$(tq_fce $PORT_MCPK .bands.standard.queued) $(tq_fce $PORT_MCPK .bands.critical.queued)"
  mcp_one $PORT_MCPK b3b-b1 "$B3" t-shed; sleep 0.5
  chk B3b "the sheddable tool call is shed at the ceiling" "429" "$(jq -r .status "$B3"/b3b-b1.json 2>/dev/null)"
  local i
  for i in $(seq 1 40); do [ "$(cat "$B3"/b3b-*.json 2>/dev/null | grep -c '"status"')" -ge 4 ] && break; sleep 0.5; done
  chk B3b "the critical call ran before the older standard one" "b3b-run b3b-c1 b3b-s1" \
    "$(jq -r 'select(.event == "executed") | .request_id' "$MCP_JOURNAL" | grep '^b3b-' | tr '\n' ' ' | sed 's/ $//')"
  chk B3b "tool calls answered: three 200" 3 "$(jq -r .status "$B3"/b3b-*.json | grep -c '^200$')"
  bt_end
  bt_check B3b 1:2:1 0
  chk B3b "sheddable: shed 1" "0 0 1 0 0" "$(bt_row sheddable)"
}

# ---- B5: CHWBL bounded load: 16 parks and resumes return every unit -------------
# The endpoint ceiling (1 per endpoint) makes the gate's endpoint step refuse
# after the CHWBL pick, so each park hands its bounded-load unit back
# (sockproxy_ep.c, chwbl_dec_runtime on the refusal) and each resume selects
# afresh. A leaked unit stays on the home endpoint and the bounded load then
# spills an idle request of the same prompt to the other endpoint; a double
# return logs the underflow protection.
case_B5() {
  echo "=== B5: CHWBL bounded load: 16 parks and resumes, every unit returned"
  local P="tenantq-b5-the-same-prompt-for-every-request" i home n
  b_req $PORT_CHWBL t-a b5home "$P" "$B3"; wait_receipts b5home 1 >/dev/null
  home=$(peak_port b5home); cl_release b5home; wait_done "$B3" b5home 1
  chk B5 "the prompt has a home endpoint" yes "$(case $home in 8080|8081) echo yes;; *) echo "no ($home)";; esac)"
  local u0; u0=$(gw_log | grep -c 'counter is already 0 (underflow protection)')
  bt_settle   # the home probe's counts land before the snapshot
  bt_begin $PORT_CHWBL
  for i in $(seq 1 18); do b_req $PORT_CHWBL t-a b5-$i "$P" "$B3"; sleep 0.1; done
  sleep 1
  chk B5 "two run (one per endpoint), sixteen wait" "2 16" "$(tq_fce $PORT_CHWBL .inflight) $(tq_fce $PORT_CHWBL .queued)"
  for i in $(seq 1 18); do cl_release b5-$i; done
  wait_done "$B3" 'b5-*' 18 40
  chk B5 "all eighteen answered 200" 18 "$(cl_errs "$B3" 'b5-*' | tr ' ' '\n' | grep -c '^200$')"
  bt_end
  # Sixteen parked, all admitted; a woken request the endpoint ceiling sends
  # back to its place is the same park (the queue-wait count sees it twice).
  bt_check B5 0:18:0 0 16
  chk B5 "sixteen parks" 16 "$(bd standard.queued)"
  chk B5 "nothing held after" "0 0" "$(tq_fce $PORT_CHWBL .inflight) $(tq_fce $PORT_CHWBL .queued)"
  chk B5 "no unit returned twice (no underflow protection logged)" "$u0" "$(gw_log | grep -c 'counter is already 0 (underflow protection)')"
  n=0
  for i in 1 2 3 4; do
    b_req $PORT_CHWBL t-a b5after-$i "$P" "$B3"; wait_receipts b5after-$i 1 >/dev/null
    [ "$(peak_port b5after-$i)" == "$home" ] && n=$((n + 1))
    cl_release b5after-$i; wait_done "$B3" b5after-$i 1
  done
  chk B5 "idle again, the prompt keeps its home endpoint (no leaked load)" 4 "$n"
}

# ---- X4: backend connect failure after dequeue ----------------------------------
# The product (owner 2026-10-18: the asserts follow it): an HTTP/1.1 request
# whose backend connect fails IMMEDIATELY takes the mid-cycle failover to the
# next endpoint (sockproxy_ep.c); the first connect of a plain rule is
# asynchronous (connect_async_ok), and one refused from its writable event
# is answered 502 backend_unreachable with no retry
# (proxy_backend_connect_event). On llbigw-1 a closed port on the endpoint
# netns is refused that way (run 3: no failover line, the dialled requests
# 502). X5 holds the failover path: a TLS connect is synchronous.
case_X4() {
  echo "=== X4: a backend connect failure after the wake: 502 with no retry, one service unit"
  local i max=0 v n200=0 n502=0 lost=0 c
  bt_begin $PORT_X4
  b_req $PORT_X4 t-a x4run x4run "$B3"; wait_receipts x4run 1 >/dev/null
  for i in 1 2 3 4; do b_req $PORT_X4 t-a x4-$i x4-$i "$B3"; sleep 0.2; done
  sleep 0.5
  chk X4 "four wait behind the ceiling" 4 "$(tq_fce $PORT_X4 .queued)"
  cl_release x4run
  for i in 1 2 3 4; do
    wait_receipts x4-$i 1 5 >/dev/null
    v=$(tq_fce $PORT_X4 .inflight); [ "$v" -gt "$max" ] 2>/dev/null && max=$v
    cl_release x4-$i
  done
  wait_done "$B3" 'x4*' 5 20
  for i in 1 2 3 4; do
    c=$(awk '{print $1}' "$B3/x4-$i-1.code")
    case "$c" in
      200) n200=$((n200 + 1)); [ "$(peak_port x4-$i)" == 8080 ] || lost=$((lost + 1)) ;;
      502) n502=$((n502 + 1)); [ "$(jq -r .error "$B3/x4-$i-1.body" 2>/dev/null)" == backend_unreachable ] || lost=$((lost + 1))
           [ "$(peak_port x4-$i)" == "none:" ] || lost=$((lost + 1)) ;;
      *) lost=$((lost + 1)) ;;
    esac
  done
  chk X4 "every woken request answered: 200 from the live endpoint, or 502 backend_unreachable" 4 "$((n200 + n502))"
  chk X4 "the closed endpoint was dialled after a wake (the precondition)" yes "$([ $n502 -ge 1 ] && echo yes || echo no)"
  chk X4 "a 502 was not retried, a 200 reached the live endpoint once" 0 "$lost"
  chk X4 "the running request answered 200" 200 "$(awk '{print $1}' "$B3/x4run-1.code")"
  chk X4 "one service unit at all times" 1 "$max"
  bt_end
  bt_check X4 0:5:0 0
  chk X4 "admitted once each" 5 "$(bd standard.admitted)"
  echo "--- X4: total failure"
  bt_begin $PORT_X4DEAD
  CL_TIMEOUT=15 b_req $PORT_X4DEAD t-a x4dead x4dead "$B3"; wait_done "$B3" x4dead 1 20
  chk X4 "total failure: 502 backend_unreachable" "502 backend_unreachable" \
    "$(awk '{print $1}' "$B3"/x4dead-1.code) $(jq -r .error "$B3"/x4dead-1.body 2>/dev/null)"
  sleep 0.5
  chk X4 "and everything returned" "0 0" "$(tq_fce $PORT_X4DEAD .inflight) $(tq_fce $PORT_X4DEAD .queued)"
  bt_end
  bt_check X4 0:1:0 0
}

# ---- H3: X3 and X4 over h2c -----------------------------------------------------
case_H3() {
  echo "=== H3: over h2c: a stream cancelled while running returns its unit; a dead endpoint fails over"
  h2runp() { local p=$1 t=$2; shift 2; source ./keys.env; local k; eval k=\$KEY_${t//-/_}; $hexec tqh1 python3 "$(pwd)/lib/h2_band.py" $VIP $p --api-key "$k" "$@"; }
  bt_begin $PORT_H2 h2
  h2runp $PORT_H2 t-a --streams 1 --delay-ms 6000 --rst 1 --rst-after-s 1.5 --nonce-prefix h3rst > "$B3/h3rst.out" 2>&1 &
  local pid=$!
  sleep 1
  chk H3 "the stream runs" 1 "$(tq_fce $PORT_H2 .inflight)"
  sleep 1.5
  chk H3 "cancelled while running: its unit is back at once" 0 "$(tq_fce $PORT_H2 .inflight)"
  wait $pid
  chk H3 "the stream was cancelled by the client" '{"RST_SENT":1}' "$(jq -c 'select(.summary) | .summary' "$B3/h3rst.out")"
  bt_end
  bt_check H3 0:1:0 0
  # (b) The product: a stream whose backend connect fails is answered 503
  # backend_unreachable on that stream with no retry (sockproxy_h2.c, the
  # stream connect-failure branch); the others run on the live endpoint.
  bt_begin $PORT_H2X h2
  local o="$B3/h3fo.out" n200 nfail nonce lost=0 got=""
  h2runp $PORT_H2X t-a --streams 2 --nonce-prefix h3fo > "$o"
  h2runp $PORT_H2X t-a --streams 2 --nonce-prefix h3fo2 >> "$o"
  n200=$(jq -r 'select(.stream) | .status' "$o" | grep -c '^200$')
  nfail=$(jq -r 'select(.stream) | "\(.status):\(.error)"' "$o" | grep -c '^503:backend_unreachable$')
  chk H3 "every stream answered: 200, or 503 backend_unreachable" 4 "$((n200 + nfail))"
  chk H3 "the closed endpoint was dialled (the precondition)" yes "$([ "$nfail" -ge 1 ] && echo yes || echo no)"
  for nonce in $(jq -r 'select(.stream and .status == "503") | .nonce' "$o"); do
    lost=$(( lost + $($hexec tqep1 curl -s -m 3 --http2-prior-knowledge "http://127.0.0.1:9090/__receipts/$nonce") ))
  done
  chk H3 "a failed stream was not retried on the live endpoint" 0 "$lost"
  for nonce in $(jq -r 'select(.stream and .status == "200") | .nonce' "$o"); do
    got="$got $($hexec tqep1 curl -s -m 3 --http2-prior-knowledge "http://127.0.0.1:9090/__receipts/$nonce")"
  done
  chk H3 "each 200 stream reached the live endpoint once" "$(printf ' 1%.0s' $(seq 1 "$n200"))" "$got"
  sleep 0.5
  chk H3 "everything returned" 0 "$(tq_fce $PORT_H2X .inflight)"
  bt_end
  bt_check H3 0:4:0 0
}

# ---- X5: backend TLS failure after dequeue ---------------------------------------
# The rule verifies its endpoints against tq-x5-ca; :8444 and :8445 present
# certificates from another CA, :8443 a trusted one. The backend handshake is
# driven synchronously in the dispatch (proxy_ep_connect_finish_leg,
# sockproxy_conn.c: "ssl-connect <ep>(failed)" and -1), so a refused
# certificate takes the same mid-cycle failover as a refused connect
# (sockproxy_ep.c, "Generic mid-cycle failover"): the remaining endpoints in
# turn, an HTTP 502 when none accepts.
tls_admin() { $hexec tqep1 curl -s -m 3 "http://127.0.0.1:$1/$2"; }
tls_receipts() { tls_admin $(( $1 + 1100 )) "__receipts/$2" | jq -r '.count // 0'; }
tls_failed_hs() { echo $(( $(tls_admin 9544 __handshakes | jq -r '.failed // 0') + $(tls_admin 9545 __handshakes | jq -r '.failed // 0') )); }
tls_release() { local p; for p in 9543 9544 9545; do tls_admin $p "__release/$1" >/dev/null; done; }
x5_req() { # x5_req <port> <nonce>: one held request over TLS (the listener's certificate is not checked)
  local key; source ./keys.env; key=$KEY_t_a
  $hexec tqh1 curl -sk -m "${CL_TIMEOUT:-60}" -o "$B3/$2-1.body" -w '%{http_code} %{time_total}\n' \
    -H 'Content-Type: application/json' -H "X-Api-Key: $key" -H "X-Test-Nonce: $2" -H 'X-Test-Hold: 1' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$2\"}],\"max_tokens\":4}" \
    "https://$VIP:$1/v1/chat/completions" > "$B3/$2-1.code" 2>&1 &
}
x5_wait() { local i; for i in $(seq 1 20); do [ "$(tls_receipts 8443 "$1")" -ge 1 ] 2>/dev/null && return 0; sleep 0.25; done; return 1; }

case_X5() {
  echo "=== X5: a backend TLS failure after the wake: fresh selection, one service unit"
  local i max=0 v hs0 hs1 log0
  bt_begin $PORT_X5
  x5_req $PORT_X5 x5run; x5_wait x5run
  chk X5 "the first request reached the trusted backend" 1 "$(tls_receipts 8443 x5run)"
  for i in 1 2 3 4; do x5_req $PORT_X5 x5-$i; sleep 0.2; done
  sleep 0.5
  chk X5 "four wait behind the ceiling" 4 "$(tq_fce $PORT_X5 .queued)"
  hs0=$(tls_failed_hs); log0=$(gw_log | grep -c "ssl-connect $EP:844[45](failed)")
  tls_release x5run
  for i in 1 2 3 4; do
    x5_wait x5-$i
    v=$(tq_fce $PORT_X5 .inflight); [ "$v" -gt "$max" ] 2>/dev/null && max=$v
    tls_release x5-$i
  done
  wait_done "$B3" 'x5*' 5 20
  hs1=$(tls_failed_hs)
  chk X5 "woken requests dialled an untrusted backend (refused handshakes after the wake)" yes \
    "$([ "$hs1" -gt "$hs0" ] && [ "$(gw_log | grep -c "ssl-connect $EP:844[45](failed)")" -gt "$log0" ] && echo yes || echo "no ($hs0->$hs1)")"
  chk X5 "each woken request reached the trusted backend once (failover selected afresh)" "1 1 1 1" \
    "$(for i in 1 2 3 4; do echo -n "$(tls_receipts 8443 x5-$i) "; done | xargs)"
  v=0; for n in x5run x5-1 x5-2 x5-3 x5-4; do v=$(( v + $(tls_receipts 8444 $n) + $(tls_receipts 8445 $n) )); done
  chk X5 "no request reached an untrusted backend" 0 "$v"
  chk X5 "all answered 200" "200 200 200 200 200" "$(cl_errs "$B3" 'x5*')"
  chk X5 "one service unit at all times" 1 "$max"
  bt_end
  bt_check X5 0:5:0 0
  chk X5 "admitted once each" 5 "$(bd standard.admitted)"
  echo "--- X5: total failure (only an untrusted backend)"
  bt_begin $PORT_X5DEAD
  CL_TIMEOUT=15 x5_req $PORT_X5DEAD x5dead; wait_done "$B3" x5dead 1 20
  # The failover's exhaustion answers 502 (proxy_send_backend_unreachable,
  # sockproxy_ep.c), as cicd/e2ehttpsproxy-betls expects of an HTTP/1.1 rule.
  chk X5 "total failure: 502 backend_unreachable" "502 backend_unreachable" \
    "$(awk '{print $1}' "$B3"/x5dead-1.code) $(jq -r .error "$B3"/x5dead-1.body 2>/dev/null)"
  sleep 0.5
  chk X5 "and everything returned" "0 0 0" "$(tq_fce $PORT_X5DEAD .inflight) $(tq_fce $PORT_X5DEAD .queued) $(tls_receipts 8444 x5dead)"
  bt_end
  bt_check X5 0:1:0 0
}

# ---- B15: a class-blind park never overtakes a critical waiter ---------------------
# Two halves, each beside one critical waiter queued at the capacity gate.
#
# (a) The legacy P/D per-endpoint queue (LLB_PD_MAX_INFLIGHT_PER_EP,
# LLB_PD_QUEUE_DEPTH_PER_EP; environment only, read once). A P/D request takes
# its service unit at the gate (sp_fc_h1_gate_service, sockproxy_ep.c) BEFORE
# pd_select_prefill parks it on a full prefill endpoint (PD_PREFILL_PARKED,
# sockproxy_pd.c), so it waits HOLDING that unit; its resume re-drives the
# dispatch and the gate returns at once for a permit that is already
# EXECUTING on the pool. The slot is freed in pd_cleanup when the request
# holding it ends (sockproxy_http.c, "DEQUEUE-ON-SLOT-FREE"). Pass: the
# service unit freed with that request goes to the critical waiter, the
# P/D-parked request's resume takes none (in flight never above the ceiling,
# admitted once per request).
#
# (b) The byte shaper's half is not a band case (owner, 2026-10-18): an AI
# request meets the gate on its routing prefix (the first 64 KB,
# SP_JSON_ROUTE_PREFIX_MAX), inside the shaper's first burst, so no shaper
# park can come before the gate. That an 8 Mbit/s shaper did not slow a 4 MB
# upload is a finding of its own (the campaign's cleanup list), not a case.
b15_metric() { bm_sum "$1" "$2" "${3:-}"; }
# The service's queue-wait histogram: sum in ms, count.
b15_qw() {
  metrics_raw | awk -v svc="$(svcl $PORT_PD)" '
    index($0, svc) == 0 { next }
    /^loxilb_ai_admission_queue_wait_seconds_sum\{/ { s += $NF }
    /^loxilb_ai_admission_queue_wait_seconds_count\{/ { c += $NF }
    END { printf "%d %d", s * 1000, c }'
}
case_B15() {
  echo "=== B15 (a): the legacy P/D per-endpoint queue beside a critical waiter"
  local i max=0 v q0 adm0
  pd_release_all
  tq_api POST /config/persist '{}' >/dev/null
  GW_EXTRA_ENV="LLB_PD_MAX_INFLIGHT_PER_EP=1 LLB_PD_QUEUE_DEPTH_PER_EP=4${GW_EXTRA_ENV:+ $GW_EXTRA_ENV}" gw_restart
  chk B15 "the gateway runs with the legacy P/D queue" yes \
    "$(docker exec tqllb sh -c 'tr "\0" "\n" < /proc/$(pgrep -x loxilb | head -1)/environ' | grep -q '^LLB_PD_QUEUE_DEPTH_PER_EP=4$' && echo yes || echo no)"
  tqb_post ":$PORT_PD ceiling 2" "$(tqb_pd_json | sed 's/"fc_max_outstanding": 1,/"fc_max_outstanding": 2,/')" || fail B15 "P/D rule ceiling 2"
  bt_begin $PORT_PD
  q0=$(b15_metric loxilb_pd_admission_queued_total "")
  adm0=$(bm_sum loxilb_ai_admission_decisions_total "$(svcl $PORT_PD)" 'reason="admitted"')
  read -r qws0 qwc0 <<< "$(b15_qw)"
  cl_one $PORT_PD t-a b15a "$B3"
  chk B15 "a P/D request holds the one prefill slot (decode held)" 1 "$(pd_wait 8181 b15a 1)"
  cl_one $PORT_PD t-b b15b "$B3"
  chk B15 "a second one parks on the legacy queue (the precondition)" "$((q0 + 1))" "$(bm_wait $((q0 + 1)) 15 loxilb_pd_admission_queued_total "")"
  chk B15 "it waits there holding its service unit" "2 0 0" "$(tq_fce $PORT_PD .inflight) $(tq_fce $PORT_PD .queued) $(pd_receipts 8180 b15b)"
  cl_one $PORT_PD t-crit b15c "$B3"; sleep 0.5
  chk B15 "a critical request waits at the gate" 1 "$(tq_fce $PORT_PD .bands.critical.queued)"
  # Both waiters wait 2 s more: the legacy-parked one is then >= 2.5 s into
  # its first park, the critical one ~2 s into its gate wait.
  sleep 2
  pd_release b15a
  for i in $(seq 1 15); do
    v=$(tq_fce $PORT_PD .inflight); [ "$v" -gt "$max" ] 2>/dev/null && max=$v
    [ "$(tq_fce $PORT_PD .bands.critical.inflight)" == 1 ] && break
    sleep 0.2
  done
  chk B15 "the unit freed with the first request went to the critical waiter" "1 0" \
    "$(tq_fce $PORT_PD .bands.critical.inflight) $(tq_fce $PORT_PD .bands.critical.queued)"
  # Which of the two takes the freed prefill slot first is the legacy queue's
  # own (class-blind) order: a request arriving at pd_select_prefill does not
  # wait behind one parked there (try 5: the critical one took it). Not
  # asserted; that the parked request runs in the end is.
  # The peak is 2 when the parked request resumed before the critical one
  # took the prefill unit; when it met that unit held it handed its own back
  # and moved to the gate holding nothing, so the polls may only see 1. Its
  # resume taking a unit would read 3.
  v=$(tq_fce $PORT_PD .inflight); [ "$v" -gt "$max" ] 2>/dev/null && max=$v
  chk B15 "its resume took no service unit (never above the ceiling)" yes \
    "$([ "$max" -ge 1 ] && [ "$max" -le 2 ] && echo yes || echo "no (peak $max)")"
  pd_release b15b; pd_wait 8181 b15c 1 10 >/dev/null; pd_release b15c
  wait_done "$B3" 'b15?' 3 20
  chk B15 "all answered 200" "200 200 200" "$(cl_errs "$B3" 'b15?')"
  chk B15 "the legacy-parked request ran both legs in the end" "1 1" "$(pd_receipts 8180 b15b) $(pd_receipts 8181 b15b)"
  bt_end
  bt_check B15 1:2:0 0
  chk B15 "admitted once per request" 3 "$(( $(bm_sum loxilb_ai_admission_decisions_total "$(svcl $PORT_PD)" 'reason="admitted"') - adm0 ))"
  # Whether the legacy-parked request, resumed, met the prefill ceiling the
  # critical one had just taken is the race the fix ends: it then waits at
  # the gate (standard queued 1, a second queue-wait sample) and its sample
  # counts from its first park, so the two samples sum to >= 4.5 s; counted
  # from the move it would be ~2 s. Not forced here (B15b forces the wait by
  # configuration); read and, when it happened, asserted.
  read -r qws1 qwc1 <<< "$(b15_qw)"
  if [ "$(bd standard.queued)" = 1 ]; then
    echo "  B15 reading: the legacy-parked request moved to the gate (queue-wait samples $((qwc1 - qwc0)), sum $((qws1 - qws0)) ms)"
    chk B15 "moved to the gate: two queue-wait samples" 2 "$((qwc1 - qwc0))"
    chk B15 "moved to the gate: its wait counts from its first park (sum >= 4500 ms)" yes \
      "$([ $((qws1 - qws0)) -ge 4500 ] && echo yes || echo "no ($((qws1 - qws0)) ms)")"
  else
    echo "  B15 reading: the legacy-parked request ran before the critical one took the prefill unit (no move; queue-wait samples $((qwc1 - qwc0)))"
  fi
  tqb_post ":$PORT_PD reset" "$(tqb_pd_json)" || fail B15 "P/D rule reset"
  tq_api POST /config/persist '{}' >/dev/null
  gw_restart

}

# B15b: the two counts disagree by configuration, no race. The legacy cap
# per prefill endpoint is 2 and the rule's prefill ceiling 1: a second
# request passes the legacy count and meets the ceiling the first holds.
# Before the fix it was refused 429 admission_capacity at once; now it waits
# at the gate and runs both legs after the first.
case_B15b() {
  echo "=== B15b: the legacy cap above the prefill ceiling: the second request waits at the gate"
  local q0 adm0 shed0 qws0 qwc0 qws1 qwc1
  pd_release_all
  tq_api POST /config/persist '{}' >/dev/null
  GW_EXTRA_ENV="LLB_PD_MAX_INFLIGHT_PER_EP=2 LLB_PD_QUEUE_DEPTH_PER_EP=4${GW_EXTRA_ENV:+ $GW_EXTRA_ENV}" gw_restart
  tqb_post ":$PORT_PD ceiling 2, prefill 1" "$(tqb_pd_json ', "fc_prefill_max_inflight": 1' | sed 's/"fc_max_outstanding": 1,/"fc_max_outstanding": 2,/')" || fail B15b "P/D rule ceiling 2, prefill 1"
  chk B15b "the prefill ceiling is the rule's 1, the legacy cap 2" "1 yes" \
    "$(tq_fce $PORT_PD .prefill_max_inflight) $(docker exec tqllb sh -c 'tr "\0" "\n" < /proc/$(pgrep -x loxilb | head -1)/environ' | grep -q '^LLB_PD_MAX_INFLIGHT_PER_EP=2$' && echo yes || echo no)"
  bt_begin $PORT_PD
  q0=$(b15_metric loxilb_pd_admission_queued_total "")
  adm0=$(bm_sum loxilb_ai_admission_decisions_total "$(svcl $PORT_PD)" 'reason="admitted"')
  shed0=$(bm_sum loxilb_ai_admission_decisions_total "$(svcl $PORT_PD)" 'reason="capacity_shed"')
  read -r qws0 qwc0 <<< "$(b15_qw)"
  cl_one $PORT_PD t-a b15d "$B3"
  chk B15b "the first holds the prefill unit (decode held)" 1 "$(pd_wait 8181 b15d 1)"
  cl_one $PORT_PD t-b b15e "$B3"; sleep 1
  chk B15b "the second waits at the gate, holding nothing, not on the legacy queue" "1 1 0 0" \
    "$(tq_fce $PORT_PD .inflight) $(tq_fce $PORT_PD .queued) $(pd_receipts 8180 b15e) $(( $(b15_metric loxilb_pd_admission_queued_total "") - q0 ))"
  chk B15b "and is not answered" 0 "$(cat "$B3"/b15e-*.code 2>/dev/null | grep -c '^[0-9]')"
  pd_release b15d
  chk B15b "the first's release lets the second run" 1 "$(pd_wait 8181 b15e 1 10)"
  pd_release b15e
  wait_done "$B3" 'b15[de]' 2 20
  chk B15b "both answered 200" "200 200" "$(cl_errs "$B3" 'b15[de]')"
  chk B15b "the second ran both legs" "1 1" "$(pd_receipts 8180 b15e) $(pd_receipts 8181 b15e)"
  bt_end
  bt_check B15b 0:2:0 0 1
  read -r qws1 qwc1 <<< "$(b15_qw)"
  chk B15b "one queue-wait sample (the second's)" 1 "$((qwc1 - qwc0))"
  chk B15b "admitted once per request, none refused" "2 0" \
    "$(( $(bm_sum loxilb_ai_admission_decisions_total "$(svcl $PORT_PD)" 'reason="admitted"') - adm0 )) $(( $(bm_sum loxilb_ai_admission_decisions_total "$(svcl $PORT_PD)" 'reason="capacity_shed"') - shed0 ))"
  tqb_post ":$PORT_PD reset" "$(tqb_pd_json)" || fail B15b "P/D rule reset"
  tq_api POST /config/persist '{}' >/dev/null
  gw_restart
}
