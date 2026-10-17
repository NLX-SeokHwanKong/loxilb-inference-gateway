#!/bin/bash
# cicd/ai-tenant: scheduling, release accounting, h2c parity and the band
# knobs (validation-sched.sh). Every case is declared below with the change
# that implements it; see README.md for the pending rule.
source ../common.sh
source ./lib/classload.sh
echo SCENARIO-ai-tenant-sched

OUT=${OUT:-$(pwd)/artifacts-sched}
rm -rf "$OUT"; mkdir -p "$OUT"

# ---- the declared cases -------------------------------------------------------
# "<id> <implemented|pending:<what>> <one line>"
DECLARED=$(cat <<'EOF'
R1 implemented fc_effective reads back every band knob with its source, and floors in units
P10 implemented band knobs: replace keeps, reset clears, null refused; kept across a restart
S8 implemented band knob validation: 400 with the field named; valid combinations accepted
B7 implemented sockMapMode request without an AI gateway refuses a band knob; accepts the rule without one
B9 implemented a restored document written before the band knobs restores every knob at its default
X12 implemented an inherited band setting the API would refuse runs on the defaults: one warn line, *_effective and band_fallback_reason read it back
X11 implemented a waiter is not idle: 504 at its queue window, never a reset at the idle bound; a client that leaves frees its place
P1 pending:shed every class admitted below the ceiling, no added queueing
P2 pending:shed a late critical request is admitted before older standard waiters
P3 pending:shed sheddable at the ceiling: 429 admission_shed, no receipt; with fc_shed_queue it waits
P5 pending:shed a flooding critical tenant is still bounded by its tenant share
P6 pending:shed P2 and P3 over h2c: no queueing, the shed order holds
P9 pending:shed a P/D service: the class rides the one service acquire
P11 pending:shed the per-band decision counter (step 3, its family name lands with it) moves per band and outcome
S1 pending:shed strict: a critical flood admits no standard; standard waiters end at their window; sheddable refused
X2 pending:shed client leaves while queued: entry gone, nothing admitted, everything returned
X3 pending:shed client leaves while running: units returned
X4 pending:shed backend connect failure after dequeue: fresh selection, one service unit
X5 pending:shed backend TLS failure after dequeue: as X4
X6 pending:shed shed 429 at the ceiling: nothing held
H3 pending:shed X3 and X4 over h2c
B3a pending:shed stateless MCP: no critical or sheddable counter moves; FIFO as main
B3b pending:shed MCP full proxy with tenant classes
B4 pending:shed KV-exact: a woken critical request routes by prefix as unparked
B5 pending:shed CHWBL bounded load: 16 parks and resumes return every unit
B6 pending:shed P/D under the weighted policy: one service unit
B15 pending:shed a class-blind park never overtakes a critical waiter
P4 pending:floors fc_critical_reserve_pct 25 at ceiling 4: standard capped at 3
P8 pending:floors the adaptive ceiling tightens: new sheddable refused first, nothing cut
S3 pending:floors min share 0:20:0 under strict: standard never below its floor
S12 pending:floors three critical tenants fill the ceiling; with a floor standard keeps it
X8 pending:floors the adaptive ceiling tightens with 10 waiters: nothing leaks
H2 pending:floors S3 and S12 over h2c: the floor decides refusals
S6 pending:ttl per-band wait windows 0:5000:500: sheddable first, standard at 5 s
S7 pending:ttl per-band queue share 25:50:25: the fifth sheddable waiter is refused queue_full
X1 pending:ttl a band's window ends while queued (all three bands): 504, everything returned
X1t pending:ttl x-llm-d-inference-ttl shortens a wait, never extends it
S2 pending:weighted weighted 8:3:1: admissions per band within 10 percent of 8:3:1
S5 pending:weighted in-band order fifo against rr
S9 pending:weighted policy changes with 12 waiters, then the ceiling tightens under a floor
S10 pending:weighted all knobs set below the ceiling: nothing queued, p99 within tolerance
S11 pending:weighted two VIPs with independent policies
X9 pending:weighted replace with waiters continues; delete with waiters 503 admission_drained
H4 pending:weighted S10 over h2c
EOF
)

declare -A RESULT
pass() { echo "  [PASS] $1 $2"; [ "${RESULT[$1]}" == FAIL ] || RESULT[$1]=PASS; }
fail() { echo "  [FAIL] $1 $2"; RESULT[$1]=FAIL; }
chk()  { if [ "$3" == "$4" ]; then pass "$1" "$2 ($4)"; else fail "$1" "$2: want '$3', got '$4'"; fi; }
has()  { case "$4" in *"$3"*) pass "$1" "$2";; *) fail "$1" "$2: want '$3' in '$(echo "$4" | cut -c1-200)'";; esac; }
# gw_restart: kill the gateway and start it again with $(tq_env) (GW_EXTRA_ENV
# adds to it); the persisted configuration comes back.
gw_restart() {
  local i
  docker exec tqllb pkill -9 -f '/root/loxilb-io/loxilb/loxilb' >/dev/null 2>&1
  for i in $(seq 1 20); do docker exec tqllb pgrep -f '/root/loxilb-io/loxilb/loxilb' >/dev/null 2>&1 || break; sleep 1; done
  docker exec -d tqllb bash -c "$(tq_env) /root/loxilb-io/loxilb/loxilb -p --sockmapsupport $(tq_db_args) --loglevel debug > /tmp/loxilb.out 2> /tmp/loxilb.err"
  sleep 3; tq_wait_api >/dev/null && tq_wait_boot >/dev/null
}
# The gateway log (/var/log/loxilb<hostname>.log inside the container).
gw_log() { docker exec tqllb sh -c 'cat /var/log/loxilb*.log 2>/dev/null'; }

# ---- R1, P10, S8: the knob rule --------------------------------------------------
knob() { tq_api POST /config/loadbalancer "$(tq_rule_json $PORT_KNOB "$KNOB_JSON$1" 8080 8081)"; }
echo "=== R1: defaults and their sources"
chk R1 "wake policy" strict "$(tq_fce $PORT_KNOB .wake_policy)"
chk R1 "wake policy source" default "$(tq_fce $PORT_KNOB .source.wake_policy)"
chk R1 "no band knob configured" false "$(tq_fce $PORT_KNOB .bands_configured)"
chk R1 "shed queue off, fifo, llm-d headers off" "off fifo off" "$(tq_fce $PORT_KNOB .shed_queue) $(tq_fce $PORT_KNOB .tenant_wake) $(tq_fce $PORT_KNOB .llmd_headers)"
chk R1 "every band reads zero" "0 0 0 0 0 0" "$(for b in critical standard sheddable; do echo -n "$(tq_fce $PORT_KNOB .bands.$b.inflight) $(tq_fce $PORT_KNOB .bands.$b.queued) "; done | xargs)"

echo "=== P10: replace semantics"
has P10 "a rule with every band knob" "200" "$(knob ', "fc_wake_policy": "weighted", "fc_band_weights": "8:3:1", "fc_band_min_share_pct": "0:25:0", "fc_band_wait_ms": "0:5000:500", "fc_band_queue_pct": "25:50:25", "fc_shed_queue": "on", "fc_tenant_wake": "rr", "fc_llmd_headers": "on"')"
G=$(tq_rule_get $PORT_KNOB); echo "$G" > "$OUT/p10-declared.json"
chk P10 "read back as declared" "weighted 8:3:1 0:25:0 0:5000:500 25:50:25 on rr on" \
  "$(echo "$G" | jq -r '[.fc_wake_policy, .fc_band_weights, .fc_band_min_share_pct, .fc_band_wait_ms, .fc_band_queue_pct, .fc_shed_queue, .fc_tenant_wake, .fc_llmd_headers] | join(" ")')"
chk R1 "configured and in force, from the rule" "weighted weighted none rule rule" "$(tq_fce $PORT_KNOB .wake_policy) $(tq_fce $PORT_KNOB .wake_policy_effective) $(tq_fce $PORT_KNOB .band_fallback_reason) $(tq_fce $PORT_KNOB .source.wake_policy) $(tq_fce $PORT_KNOB .source.band_weights)"
chk R1 "the standard floor in units (25 % of 4, rounded up)" 1 "$(tq_fce $PORT_KNOB .bands.standard.floor_units)"
chk R1 "a band knob is configured" true "$(tq_fce $PORT_KNOB .bands_configured)"
# An identical POST is answered 409 lbrule-exists: the replace changes one
# field outside the bands and omits every band knob.
has P10 "a replace that omits every band knob" "200" "$(knob ', "fc_ep_max_inflight": 4')"
chk P10 "keeps them all" "weighted 8:3:1 0:25:0 on rr" \
  "$(tq_rule_get $PORT_KNOB | jq -r '[.fc_wake_policy, .fc_band_weights, .fc_band_min_share_pct, .fc_shed_queue, .fc_tenant_wake] | join(" ")')"
has P10 "a replace with the reset values" "200" "$(knob ', "fc_wake_policy": "inherit", "fc_band_weights": "0:0:0", "fc_band_min_share_pct": "0:0:0", "fc_band_wait_ms": "0:0:0", "fc_band_queue_pct": "0:0:0", "fc_shed_queue": "inherit", "fc_tenant_wake": "inherit", "fc_llmd_headers": "inherit"')"
chk P10 "back to the defaults" "strict false off fifo none" "$(tq_fce $PORT_KNOB .wake_policy_effective) $(tq_fce $PORT_KNOB .bands_configured) $(tq_fce $PORT_KNOB .shed_queue_effective) $(tq_fce $PORT_KNOB .tenant_wake_effective) $(tq_fce $PORT_KNOB .band_fallback_reason)"
# A triple at its all-zero default (and a zero reserve, an inherited switch)
# reads back absent, like every numeric zero of the rule.
chk P10 "the reset values read back absent" "false false false false false false false false" \
  "$(tq_rule_get $PORT_KNOB | jq -r '[has("fc_wake_policy"), has("fc_band_weights"), has("fc_band_min_share_pct"), has("fc_critical_reserve_pct"), has("fc_band_wait_ms"), has("fc_band_queue_pct"), has("fc_shed_queue"), has("fc_tenant_wake")] | map(tostring) | join(" ")')"
for f in fc_wake_policy fc_band_weights fc_band_min_share_pct fc_critical_reserve_pct fc_band_wait_ms fc_band_queue_pct fc_shed_queue fc_tenant_wake fc_llmd_headers; do
  has P10 "JSON null on $f is refused" "400" "$(knob ", \"$f\": null" | cut -c1-3)"
done

echo "=== S8: validation"
s8() { local r; r=$(knob "$2"); has S8 "$1: 400" "400" "${r:0:3}"; has S8 "$1: names $3" "$3" "$r"; }
s8 "weighted with a zero weight" ', "fc_wake_policy": "weighted", "fc_band_weights": "8:0:1"' fc_band_weights
s8 "weighted without weights" ', "fc_wake_policy": "weighted"' fc_band_weights
s8 "weighted with weights declared 0:0:0" ', "fc_wake_policy": "weighted", "fc_band_weights": "0:0:0"' fc_band_weights
s8 "weights summing past 64" ', "fc_wake_policy": "weighted", "fc_band_weights": "40:20:5"' fc_band_weights
s8 "a weight past 62" ', "fc_band_weights": "63:1:0"' fc_band_weights
s8 "a zero weight under strict" ', "fc_wake_policy": "strict", "fc_band_weights": "8:0:1"' fc_band_weights
s8 "floors summing past 100" ', "fc_band_min_share_pct": "50:40:20"' fc_band_min_share_pct
s8 "reserve with a critical floor" ', "fc_critical_reserve_pct": 10, "fc_band_min_share_pct": "10:0:0"' fc_critical_reserve_pct
s8 "reserve and floors past 100" ', "fc_critical_reserve_pct": 60, "fc_band_min_share_pct": "0:50:0"' fc_band_min_share_pct
s8 "queue shares past 100" ', "fc_band_queue_pct": "50:40:20"' fc_band_queue_pct
s8 "a malformed triple" ', "fc_band_wait_ms": "100:200"' fc_band_wait_ms
s8 "a wait past an hour" ', "fc_band_wait_ms": "0:3600001:0"' fc_band_wait_ms
# A switch word outside its enum is a binding error (422, "... should be one
# of"), as fc_expose_headers is (ai-admission XH13); the cross-field rules
# above are the rule layer's 400s.
R=$(knob ', "fc_tenant_wake": "lifo"'); has S8 "a bad switch: 422" "422" "${R:0:3}"; has S8 "a bad switch: names fc_tenant_wake" fc_tenant_wake "$R"
has S8 "shed queue with no queue: 400" "400" "$(tq_api POST /config/loadbalancer "$(tq_rule_json $PORT_KNOB ', "fc_max_outstanding": 4, "fc_max_queue_depth": 0, "fc_shed_queue": "on"' 8080 8081)" | cut -c1-3)"
has S8 "weights under strict are accepted" "200" "$(knob ', "fc_wake_policy": "strict", "fc_band_weights": "8:3:1"')"
chk S8 "and reported unused" "strict strict false" "$(tq_fce $PORT_KNOB .wake_policy) $(tq_fce $PORT_KNOB .wake_policy_effective) $(tq_fce $PORT_KNOB .weights_in_use)"
has S8 "two queue shares with the third zero may exceed 100" "200" "$(knob ', "fc_band_queue_pct": "60:60:0"')"
has S8 "the reserve alone" "200" "$(knob ', "fc_band_weights": "0:0:0", "fc_band_queue_pct": "0:0:0", "fc_critical_reserve_pct": 25')"
chk S8 "is the critical floor" "25 rule" "$(tq_fce $PORT_KNOB .bands.critical.min_share_pct) $(tq_fce $PORT_KNOB .source.critical_reserve_pct)"
has S8 "band knobs with the gate off are accepted" "200" "$(knob ', "fc_mode": "off", "fc_shed_queue": "on"')"
has S8 "reset" "200" "$(knob ', "fc_mode": "inherit", "fc_critical_reserve_pct": 0, "fc_shed_queue": "inherit"')"

echo "=== B7: sockmap"
SM() { echo "{\"serviceArguments\": {\"externalIP\": \"$VIP\", \"port\": $PORT_SOCKMAP, \"protocol\": \"tcp\", \"sel\": 0, \"mode\": 4, \"sockMapMode\": \"request\"$1}, \"endpoints\": [{\"endpointIP\": \"$EP\", \"targetPort\": 8080, \"weight\": 1}]}"; }
R=$(tq_api POST /config/loadbalancer "$(SM ', "fc_shed_queue": "on"')")
has B7 "a band knob is refused: 400" "400" "${R:0:3}"; has B7 "naming it" fc_shed_queue "$R"
R=$(tq_api POST /config/loadbalancer "$(SM ', "fc_wake_policy": "weighted", "fc_band_weights": "2:1:1"')")
has B7 "the wake policy too: 400" "400" "${R:0:3}"
has B7 "the same rule with no band knob is accepted" "200" "$(tq_api POST /config/loadbalancer "$(SM '')" | cut -c1-3)"
tq_rule_del $PORT_SOCKMAP

echo "=== B9: a document from before the band knobs"
# The band fields are omitempty: a snapshot taken while no rule sets a band
# knob holds no band key at all, the shape of a document written before
# they existed, and carries a valid checksum (an edited document would not
# restore: restore verifies it).
knob ', "fc_wake_policy": "inherit", "fc_band_weights": "0:0:0", "fc_band_min_share_pct": "0:0:0", "fc_critical_reserve_pct": 0, "fc_band_wait_ms": "0:0:0", "fc_band_queue_pct": "0:0:0", "fc_shed_queue": "inherit", "fc_tenant_wake": "inherit", "fc_llmd_headers": "inherit"' >/dev/null
$hexec tqh1 curl -s -m 10 "$API/config/snapshot" > "$OUT/b9-old.json"
chk B9 "a document with no band key" 0 "$(grep -c 'fc_band_\|fc_wake_policy\|fc_shed_queue\|fc_tenant_wake\|fc_llmd_headers\|fc_critical_reserve_pct' "$OUT/b9-old.json")"
has B9 "band knobs on the knob rule" "200" "$(knob ', "fc_wake_policy": "weighted", "fc_band_weights": "8:3:1", "fc_shed_queue": "on", "fc_band_wait_ms": "0:5000:500"')"
$hexec tqh1 curl -s -m 10 "$API/config/snapshot" > "$OUT/b9-snap.json"
chk B9 "a snapshot now carries them" "weighted" "$(jq -r "[.. | objects | select(.port? == $PORT_KNOB) | .fc_wake_policy // empty] | first // \"none\"" "$OUT/b9-snap.json")"
R=$($hexec tqh1 curl -s -m 30 -o "$OUT/b9-restore.json" -w '%{http_code}' -X POST "$API/config/restore?mode=commit" \
     -H 'Content-Type: application/json' --data-binary @"$OUT/b9-old.json")
chk B9 "it restores" 200 "$R"
tq_wait_boot >/dev/null
chk B9 "every band knob at its default" "strict false off fifo none 0 0" \
  "$(tq_fce $PORT_KNOB .wake_policy_effective) $(tq_fce $PORT_KNOB .bands_configured) $(tq_fce $PORT_KNOB .shed_queue_effective) $(tq_fce $PORT_KNOB .tenant_wake_effective) $(tq_fce $PORT_KNOB .band_fallback_reason) $(tq_fce $PORT_KNOB .bands.standard.wait_ms) $(tq_fce $PORT_KNOB .bands.critical.min_share_pct)"
chk B9 "and read back as defaults" "strict 0:0:0" "$(tq_rule_get $PORT_KNOB | jq -r '"\(.fc_wake_policy // "strict") \(.fc_band_weights // "0:0:0")"')"
chk B9 "the other rules are back too" 2 "$(for p in $PORT_SCHED $PORT_IDLE; do tq_rule_get $p | jq -r .port; done | grep -c .)"

echo "=== P10: kept across a restart"
has P10 "band knobs for the restart" "200" "$(knob ', "fc_wake_policy": "weighted", "fc_band_weights": "8:3:1", "fc_tenant_wake": "rr"')"
has P10 "persisted" "200" "$(tq_api POST /config/persist '{}' | cut -c1-3)"
gw_restart
chk P10 "the knobs came back" "weighted 8:3:1 rr" "$(tq_rule_get $PORT_KNOB | jq -r '"\(.fc_wake_policy) \(.fc_band_weights) \(.fc_tenant_wake)"')"
chk P10 "and are in force" "weighted rr" "$(tq_fce $PORT_KNOB .wake_policy_effective) $(tq_fce $PORT_KNOB .tenant_wake_effective)"
knob ', "fc_wake_policy": "inherit", "fc_band_weights": "0:0:0", "fc_band_wait_ms": "0:0:0", "fc_shed_queue": "inherit", "fc_tenant_wake": "inherit"' >/dev/null
has P10 "the reset persisted" "200" "$(tq_api POST /config/persist '{}' | cut -c1-3)"

echo "=== X12: weighted inherited from the environment"
# What a rule is refused to declare, it does not run by inheriting: the
# gate falls back, logs it once at apply time and reads it back.
GW_EXTRA_ENV="LLB_FC_WAKE_POLICY=weighted${GW_EXTRA_ENV:+ $GW_EXTRA_ENV}" gw_restart
chk X12 "the knob rule inherits weighted with no weights: strict" "weighted env strict zero_weight false" \
  "$(tq_fce $PORT_KNOB .wake_policy) $(tq_fce $PORT_KNOB .source.wake_policy) $(tq_fce $PORT_KNOB .wake_policy_effective) $(tq_fce $PORT_KNOB .band_fallback_reason) $(tq_fce $PORT_KNOB .weights_in_use)"
has X12 "weights on the knob rule" "200" "$(knob ', "fc_band_weights": "8:3:1"')"
chk X12 "then it runs weighted" "weighted none true" \
  "$(tq_fce $PORT_KNOB .wake_policy_effective) $(tq_fce $PORT_KNOB .band_fallback_reason) $(tq_fce $PORT_KNOB .weights_in_use)"
has X12 "a sockmap rule that declares no band knob is accepted" "200" "$(tq_api POST /config/loadbalancer "$(SM ', "fc_mode": "enforce", "fc_max_outstanding": 4')" | cut -c1-3)"
sleep 1
L1=$(gw_log | grep "admission bands run on fallback (sockmap_request)" | grep -c ":$PORT_SOCKMAP (")
gw_log | grep "admission bands run on fallback" > "$OUT/x12-warn.log"
chk X12 "one warn line at apply time" 1 "$L1"
has X12 "naming the inherited value and the one that runs" "wake=weighted(env) runs strict" "$(gw_log | grep "admission bands run on fallback (sockmap_request)" | grep ":$PORT_SOCKMAP (" | tail -1)"
chk X12 "read back: configured weighted from env, runs strict, why" "weighted env strict sockmap_request false false" \
  "$(tq_fce $PORT_SOCKMAP .wake_policy) $(tq_fce $PORT_SOCKMAP .source.wake_policy) $(tq_fce $PORT_SOCKMAP .wake_policy_effective) $(tq_fce $PORT_SOCKMAP .band_fallback_reason) $(tq_fce $PORT_SOCKMAP .weights_in_use) $(tq_fce $PORT_SOCKMAP .bands_configured)"
# One line per apply: a replace outside the gate (inactiveTimeOut) re-creates
# the proxy entry (DpRemove + add), a new pool, applied once: exactly one
# more line; nothing more while the rule stands.
has X12 "a replace that re-creates the pool" "200" "$(tq_api POST /config/loadbalancer "$(SM ', "fc_mode": "enforce", "fc_max_outstanding": 4, "inactiveTimeOut": 30')" | cut -c1-3)"
sleep 1
chk X12 "one more line for that apply" 2 "$(gw_log | grep "admission bands run on fallback (sockmap_request)" | grep -c ":$PORT_SOCKMAP (")"
sleep 5
chk X12 "and none while the rule stands" 2 "$(gw_log | grep "admission bands run on fallback (sockmap_request)" | grep -c ":$PORT_SOCKMAP (")"
chk X12 "the re-created pool reads back the fallback" "strict sockmap_request" "$(tq_fce $PORT_SOCKMAP .wake_policy_effective) $(tq_fce $PORT_SOCKMAP .band_fallback_reason)"
tq_rule_del $PORT_SOCKMAP
knob ', "fc_band_weights": "0:0:0"' >/dev/null
gw_restart
chk X12 "the environment back to its default: strict from default" "strict default strict none" \
  "$(tq_fce $PORT_KNOB .wake_policy) $(tq_fce $PORT_KNOB .source.wake_policy) $(tq_fce $PORT_KNOB .wake_policy_effective) $(tq_fce $PORT_KNOB .band_fallback_reason)"

echo "=== X11: a waiter is not idle"
body() { echo "{\"model\":\"$MODEL\",\"stream\":$1,\"messages\":[{\"role\":\"user\",\"content\":\"$2\"}],\"max_tokens\":4}"; }
xcurl() { $hexec tqh1 curl -s "$@"; }
xcurl -N -m 12 -o "$OUT/x11-a.body" -w '%{http_code} %{time_total}\n' -H 'Content-Type: application/json' -H 'X-Test-Forever: 1' \
  -d "$(body true x11a)" "http://$VIP:$PORT_IDLE/v1/chat/completions" > "$OUT/x11-a.code" 2>&1 &
sleep 0.5
xcurl -m 15 -o "$OUT/x11-b.body" -w '%{http_code} %{time_total}\n' -H 'Content-Type: application/json' \
  -d "$(body false x11b)" "http://$VIP:$PORT_IDLE/v1/chat/completions" > "$OUT/x11-b.code" 2>&1 &
BPID=$!
sleep 0.2
xcurl -m 1 -o /dev/null -w '%{http_code} %{time_total}\n' -H 'Content-Type: application/json' \
  -d "$(body false x11c)" "http://$VIP:$PORT_IDLE/v1/chat/completions" > "$OUT/x11-c.code" 2>&1 &
sleep 0.4; Q1=$(tq_fce $PORT_IDLE .queued)
sleep 1.6; Q2=$(tq_fce $PORT_IDLE .queued)
wait $BPID
read BC BT < "$OUT/x11-b.code"
wait
chk X11 "both waited" 2 "$Q1"
chk X11 "the one that left freed its place by 2.7 s" 1 "$Q2"
chk X11 "the one that stayed: 504, not reset at the 2 s idle bound" 504 "$BC"
chk X11 "with the queue's code" admission_queue_timeout "$(jq -r .error "$OUT/x11-b.body" 2>/dev/null)"
chk X11 "at its 5 s window" yes "$(awk -v t="$BT" 'BEGIN {print (t >= 4.5 && t < 7) ? "yes" : "no"}')"
sleep 1
chk X11 "nothing held after" "0 0" "$(tq_fce $PORT_IDLE .inflight) $(tq_fce $PORT_IDLE .queued)"

# ---- summary --------------------------------------------------------------------
echo "=== summary"
nd=0; np=0; npend=0; nf=0; nmiss=0
while read -r id state _; do
  [ -n "$id" ] || continue
  nd=$((nd + 1))
  case "$state" in
    implemented)
      case "${RESULT[$id]}" in
        PASS) np=$((np + 1)); echo "  $id PASS" ;;
        FAIL) nf=$((nf + 1)); echo "  $id FAIL" ;;
        *)    nmiss=$((nmiss + 1)); echo "  $id NOT EXECUTED" ;;
      esac ;;
    pending:*) npend=$((npend + 1)); echo "  $id PENDING(${state#pending:})"
      [ -n "${RESULT[$id]}" ] && { nmiss=$((nmiss + 1)); echo "  $id ran while declared pending"; } ;;
    *) nmiss=$((nmiss + 1)); echo "  $id BAD DECLARATION" ;;
  esac
done <<< "$DECLARED"
for id in "${!RESULT[@]}"; do grep -q "^$id " <<< "$DECLARED" || { nmiss=$((nmiss + 1)); echo "  $id ran but is not declared"; }; done
echo "declared=$nd pass=$np fail=$nf pending=$npend not_executed=$nmiss"
[ "${REQUIRE_ALL:-0}" == 1 ] && [ $npend -gt 0 ] && { echo "SCENARIO-ai-tenant-sched [FAILED] $npend pending at REQUIRE_ALL"; exit 1; }
[ $nf -eq 0 ] && [ $nmiss -eq 0 ] || { echo "SCENARIO-ai-tenant-sched [FAILED]"; exit 1; }
echo "SCENARIO-ai-tenant-sched [OK]"
