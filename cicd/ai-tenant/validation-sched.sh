#!/bin/bash
# cicd/ai-tenant: scheduling, release accounting, h2c parity and the band
# knobs (validation-sched.sh). Every case is declared below with the change
# that implements it; see README.md for the pending rule.
source ../common.sh
source ./lib/classload.sh
echo SCENARIO-ai-tenant-sched

OUT=${OUT:-$(pwd)/artifacts-sched}
rm -rf "$OUT"; mkdir -p "$OUT"
source ./lib/topo-b.sh
source ./lib/cases-b.sh

# ---- the declared cases -------------------------------------------------------
# "<id> <implemented|pending:<what>> <one line>"
DECLARED=$(cat <<'EOF'
R1 implemented fc_effective reads back every band knob with its source, and floors in units
P10 implemented band knobs: replace keeps, reset clears, null refused; kept across a restart
S8 implemented band knob validation: 400 with the field named; valid combinations accepted
B7 implemented sockMapMode request without an AI gateway refuses a band knob; accepts the rule without one
B9 implemented a restored document written before the band knobs restores every knob at its default
X12 implemented an inherited band setting the API would refuse runs on the defaults: one warn line, *_effective and band_fallback_reason read it back
X13 implemented eviction (fc_shed_queue on): a critical arrival at a full queue evicts the youngest sheddable waiter, answered 429 admission_shed on its own connection
X14 implemented the mirror (fc_shed_queue off): nobody is evicted, the arrival is refused by the queue-full path
X15 implemented more evictions in one second than the evicted list holds (16): every evicted waiter answered 429 within two passes, shed count equal to the evictions
X16 implemented an evicted keep-alive client keeps its connection: 429 admission_shed with keep-alive, the queue-full 429's headers, and its next request on the same connection is answered 200
X11 implemented a waiter is not idle: 504 at its queue window, never a reset at the idle bound; a client that leaves frees its place
P1 implemented every class admitted below the ceiling, no added queueing
P2 implemented a late critical request is admitted before older standard waiters
P3 implemented sheddable at the ceiling: 429 admission_shed, no receipt; with fc_shed_queue it waits
P5 implemented a flooding critical tenant is still bounded by its tenant share
P6 implemented P2 and P3 over h2c: no queueing, the shed order holds
P9 implemented a P/D service: the class rides the one service acquire
P11 implemented loxilb_ai_fc_band_total: fifteen series per service, three bands, five outcomes; only sheddable is shed
S1 implemented strict: a critical flood admits no standard; standard waiters end at their window; sheddable refused
X2 implemented client leaves while queued: entry gone, nothing admitted, everything returned
X3 implemented client leaves while running: units returned
X4 implemented backend connect failure after dequeue: an immediate connect failure fails over afresh, one service unit; total failure 502
X5 implemented backend TLS failure after dequeue: the refused handshake fails over afresh, one service unit; total failure 502
X6 implemented shed 429 at the ceiling: nothing held
H3 implemented over h2c: a stream cancelled while running returns its unit; a stream whose backend connect fails is answered 503 backend_unreachable, no retry
B3a implemented stateless MCP: standard only, no critical or sheddable counter moves, arrival order (the mcp-stateless replay against main is the live set's runner step)
B3b implemented MCP with tenant classes, on a stateless MCP pool with the key check (a full-proxy pool never gates POST /mcp)
B4 elsewhere:vllm-kvcache-routing-cpu KV-exact: a woken critical request routes by prefix as unparked; run by that scenario's arm b4-band-arm.sh, its result recorded in EVIDENCE beside this one
B5 implemented CHWBL bounded load: 16 parks and resumes return every unit
B6 implemented P/D under the weighted policy: one service unit
B15 implemented a request parked on the legacy P/D queue holds its service unit and takes none on resume; the critical waiter gets the freed unit
P4 implemented fc_critical_reserve_pct 25 at ceiling 4: standard capped at 3 with critical idle; a reserved unit wakes nobody
P8 implemented the adaptive ceiling tightens: new sheddable refused first, nothing cut, the floor in units follows the ceiling
S3 implemented min share 0:20:0 under strict: no standard demand, critical holds the whole ceiling; with standard waiting it keeps its unit
S12 implemented three critical tenants fill the ceiling; with a floor, standard keeps it
S13 implemented strict, saturating critical demand, a 10 percent sheddable floor: sheddable never below its floor over the run; with the floor at 0 it is starved
X8 implemented the adaptive ceiling tightens with 10 waiters under a floor: nothing leaks
H2 implemented over h2c a floor never binds (no stream waits); the reserve decides refusals
S6 implemented per-band wait windows 0:5000:500 inside a 30 s queue window: sheddable first, standard at 5 s, critical keeps waiting
S7 implemented per-band queue share 25:50:25: each band stops at its share (the third sheddable waiter is refused queue_full), and a share another band does not use is open to the rest
X1 implemented a band's window ends while queued (all three bands): 504, everything returned
X1t implemented fc_llmd_headers on: x-llm-d-inference-ttl shortens a wait (429 admission_ttl_expired), never extends it; the refusals carry the dropped-reason; the backend is sent the resolved objective and fairness identity
X1o implemented fc_llmd_headers off: the TTL header is ignored (504 at the window), no llm-d header on any refusal, the client's headers reach the backend as sent
X1h implemented over h2c, both arms of fc_llmd_headers: on forwards the resolved objective and fairness identity and puts the dropped-reason on a stream's refusal; off forwards and answers as before
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
s8 "the reserve and the other two floors past 100" ', "fc_critical_reserve_pct": 60, "fc_band_min_share_pct": "0:50:0"' fc_critical_reserve_pct
s8 "the reserve and the other two floors past 100 (sheddable)" ', "fc_critical_reserve_pct": 50, "fc_band_min_share_pct": "0:30:30"' fc_critical_reserve_pct
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
chk S8 "is a reserve, not a floor" "25 0 rule" "$(tq_fce $PORT_KNOB .bands.critical_reserve_pct) $(tq_fce $PORT_KNOB .bands.critical.min_share_pct) $(tq_fce $PORT_KNOB .source.critical_reserve_pct)"
chk S8 "in units of the ceiling in force (25 % of 4)" 1 "$(tq_fce $PORT_KNOB .bands.critical_reserve_units)"
has S8 "a critical floor beside the reserve is accepted" "200" "$(knob ', "fc_critical_reserve_pct": 40, "fc_band_min_share_pct": "40:20:0"')"
chk S8 "both read back, apart" "40 40 rule rule" "$(tq_fce $PORT_KNOB .bands.critical_reserve_pct) $(tq_fce $PORT_KNOB .bands.critical.min_share_pct) $(tq_fce $PORT_KNOB .source.critical_reserve_pct) $(tq_fce $PORT_KNOB .source.band_min_share_pct)"
has S8 "floors beside weighted weights: independent of them" "200" "$(knob ', "fc_wake_policy": "weighted", "fc_band_weights": "8:3:1", "fc_critical_reserve_pct": 0, "fc_band_min_share_pct": "10:20:30"')"
has S8 "floors alone, with no weights and the policy at its default" "200" "$(knob ', "fc_wake_policy": "inherit", "fc_band_weights": "0:0:0", "fc_band_min_share_pct": "0:20:0"')"
has S8 "band knobs with the gate off are accepted" "200" "$(knob ', "fc_mode": "off", "fc_shed_queue": "on"')"
has S8 "reset" "200" "$(knob ', "fc_mode": "inherit", "fc_critical_reserve_pct": 0, "fc_band_min_share_pct": "0:0:0", "fc_shed_queue": "inherit"')"

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

# ---- step 3: strict order and the shed -------------------------------------------
# Every case reads the band counter's deltas over itself (bt_begin / bt_end)
# and asserts both identities (bt_check); the per-band request counts it
# passes are the requests of each band that ended in a band outcome.
fce() { tq_fce $PORT_SCHED "$1"; }
bq() { fce ".bands.$1.queued"; }
bi() { fce ".bands.$1.inflight"; }
S3=$OUT/step3; mkdir -p "$S3"
cl_release_all

echo "=== P1: below the ceiling every class is admitted at once"
sched_cfg 8 16 30000 "$SCHED_RESET" || fail P1 "pool shape"
bt_begin $PORT_SCHED
cl_fire $PORT_SCHED t-crit 2 p1c "$S3"; cl_fire $PORT_SCHED t-a 2 p1s "$S3"; cl_fire $PORT_SCHED t-shed 2 p1b "$S3"
wait_receipts p1c 2 >/dev/null; wait_receipts p1s 2 >/dev/null; wait_receipts p1b 2 >/dev/null
chk P1 "six reached a backend together" 6 "$(( $(cl_receipts p1c) + $(cl_receipts p1s) + $(cl_receipts p1b) ))"
chk P1 "nothing waited" 0 "$(fce .queued)"
chk P1 "in flight per band, critical standard sheddable" "2 2 2" "$(bi critical) $(bi standard) $(bi sheddable)"
cl_release p1c; cl_release p1s; cl_release p1b; wait_done "$S3" 'p1?' 6
chk P1 "all answered 200" "200 200 200 200 200 200" "$(cl_errs "$S3" 'p1?')"
bt_end
bt_check P1 2:2:2 0
chk P1 "admitted per band, nothing queued" "2 0 2 0 2 0" "$(bd critical.admitted) $(bd critical.queued) $(bd standard.admitted) $(bd standard.queued) $(bd sheddable.admitted) $(bd sheddable.queued)"

echo "=== P2: a late critical request is admitted before older standard waiters"
sched_cfg 2 8 30000 "$SCHED_RESET" || fail P2 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-a p2run1 "$S3"; cl_one $PORT_SCHED t-a p2run2 "$S3"
wait_receipts p2run1 1 >/dev/null; wait_receipts p2run2 1 >/dev/null
for i in 1 2 3; do cl_one $PORT_SCHED t-b p2s$i "$S3"; sleep 0.3; done
cl_one $PORT_SCHED t-crit p2c "$S3"; sleep 0.5
chk P2 "three standard and one critical wait" "3 1" "$(bq standard) $(bq critical)"
cl_release p2run1
chk P2 "the freed unit went to the critical request" 1 "$(wait_receipts p2c 1)"
chk P2 "no standard waiter was taken" "0 0 0 3" "$(cl_receipts p2s1) $(cl_receipts p2s2) $(cl_receipts p2s3) $(bq standard)"
cl_release p2run2
chk P2 "the next unit to the oldest standard waiter" "1 0 0" "$(wait_receipts p2s1 1) $(cl_receipts p2s2) $(cl_receipts p2s3)"
cl_release p2c; chk P2 "then the next oldest" "1 0" "$(wait_receipts p2s2 1) $(cl_receipts p2s3)"
cl_release p2s1; wait_receipts p2s3 1 >/dev/null; cl_release p2s2; cl_release p2s3
wait_done "$S3" 'p2*' 6
chk P2 "all answered 200" "200 200 200 200 200 200" "$(cl_errs "$S3" 'p2*')"
bt_end
bt_check P2 1:5:0 0
chk P2 "critical admitted after one park; standard three parks" "1 1 5 3" "$(bd critical.admitted) $(bd critical.queued) $(bd standard.admitted) $(bd standard.queued)"

echo "=== P3, X6: sheddable at the ceiling is shed; with fc_shed_queue it waits"
sched_cfg 2 4 10000 "$SCHED_RESET" || fail P3 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-a p3run1 "$S3"; cl_one $PORT_SCHED t-a p3run2 "$S3"
wait_receipts p3run1 1 >/dev/null; wait_receipts p3run2 1 >/dev/null
cl_one $PORT_SCHED t-shed p3b "$S3"; wait_done "$S3" 'p3b' 1 5
chk P3 "429 admission_shed" "429:admission_shed" "$(cl_errs "$S3" 'p3b')"
chk P3 "no backend receipt" 0 "$(cl_receipts p3b)"
has P3 "the admission headers on the refusal" "X-Loxilb-Admission-Limit: 2" "$(tr -d '\r' < "$S3"/p3b-1.hdr)"
has P3 "its class" "X-Loxilb-Admission-Class: sheddable" "$(tr -d '\r' < "$S3"/p3b-1.hdr)"
has P3 "its band's wait window in force (the queue's)" "X-Loxilb-Admission-Band-Wait-Ms: 10000" "$(tr -d '\r' < "$S3"/p3b-1.hdr)"
chk P3 "the same body shape as admission_capacity" "1 250" "$(jq -r '"\(.retry_after) \(.jitter_hint_ms)"' "$S3"/p3b-1.body)"
chk X6 "a shed request holds nothing: in flight, waiting" "2 0" "$(fce .inflight) $(fce .queued)"
chk X6 "and no reservation" 0 "$(fce .reservation_held_claims | sed 's/none/0/')"
sched_cfg 2 4 10000 ', "fc_shed_queue": "on"' || fail P3 "shed queue on"
cl_one $PORT_SCHED t-shed p3q "$S3"; sleep 0.5
chk P3 "with fc_shed_queue on it waits in its band" "1 0" "$(bq sheddable) $(cl_receipts p3q)"
cl_release p3run1
chk P3 "and is admitted when a unit frees" 1 "$(wait_receipts p3q 1)"
cl_release p3q; cl_release p3run2; wait_done "$S3" 'p3*' 4
chk P3 "answers: two standard 200, the waiter 200, the shed 429" "200 200 200 429:admission_shed" "$(cl_errs "$S3" 'p3*')"
sched_cfg 2 4 10000 "$SCHED_RESET" || fail P3 "reset"
bt_end
bt_check P3 0:2:2 0
chk P3 "sheddable: admitted 1, queued 1, shed 1" "1 1 1 0 0" "$(bt_row sheddable)"
chk X6 "the shed counted once, as shed" 1 "$(bd d.shed)"

echo "=== P5: a flooding critical tenant is held to its tenant share"
sched_cfg 4 8 10000 ', "fc_tenant_max_share_pct": 50, "fc_shed_queue": "inherit"' || fail P5 "pool shape"
bt_begin $PORT_SCHED
for i in 1 2 3 4; do cl_one $PORT_SCHED t-crit p5c$i "$S3"; sleep 0.2; done
sleep 0.5
chk P5 "two critical run (its share of 4), two wait" "2 2" "$(bi critical) $(bq critical)"
cl_one $PORT_SCHED t-a p5a1 "$S3"; cl_one $PORT_SCHED t-a p5a2 "$S3"
chk P5 "another tenant is admitted at once beside them" "1 1" "$(wait_receipts p5a1 1) $(wait_receipts p5a2 1)"
cl_release p5a1; cl_release p5a2; wait_done "$S3" 'p5a*' 2; sleep 0.5
chk P5 "units freed by others do not go past the share" "2 2" "$(bi critical) $(bq critical)"
cl_release p5c1; cl_release p5c2
wait_receipts p5c3 1 >/dev/null; wait_receipts p5c4 1 >/dev/null
cl_release p5c3; cl_release p5c4; wait_done "$S3" 'p5*' 6
chk P5 "all answered 200" "200 200 200 200 200 200" "$(cl_errs "$S3" 'p5*')"
sched_cfg 4 8 10000 "$SCHED_RESET" || fail P5 "reset"
bt_end
bt_check P5 4:2:0 0
chk P5 "critical: admitted 4, queued 2" "4 2" "$(bd critical.admitted) $(bd critical.queued)"

echo "=== S1: strict: a critical flood admits no standard; standard waiters end at their window"
sched_cfg 2 8 5000 "$SCHED_RESET" || fail S1 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-crit s1c0a "$S3"; cl_one $PORT_SCHED t-crit s1c0b "$S3"
wait_receipts s1c0a 1 >/dev/null; wait_receipts s1c0b 1 >/dev/null
cl_one $PORT_SCHED t-a s1s1 "$S3"; cl_one $PORT_SCHED t-b s1s2 "$S3"; sleep 0.3
cl_one $PORT_SCHED t-shed s1b "$S3"
T0=$(date +%s.%N)
for i in 1 2 3 4 5 6 7; do
  cl_one $PORT_SCHED t-crit s1c${i}a "$S3"; cl_one $PORT_SCHED t-crit s1c${i}b "$S3"; sleep 0.4
  cl_release s1c$((i - 1))a; cl_release s1c$((i - 1))b
  wait_receipts s1c${i}a 1 2 >/dev/null; wait_receipts s1c${i}b 1 2 >/dev/null; sleep 0.4
done
wait_done "$S3" 's1s*' 2 5
chk S1 "no standard request reached a backend during the flood" "0 0" "$(cl_receipts s1s1) $(cl_receipts s1s2)"
chk S1 "both standard waiters ended at their window" "504:admission_queue_timeout 504:admission_queue_timeout" "$(cl_errs "$S3" 's1s*')"
chk S1 "sheddable refused at once" "429:admission_shed" "$(cl_errs "$S3" 's1b')"
cl_release s1c7a; cl_release s1c7b; wait_done "$S3" 's1c*' 16
chk S1 "every critical request admitted" 16 "$(cl_errs "$S3" 's1c*' | tr ' ' '\n' | grep -c '^200$')"
bt_end
bt_check S1 16:2:1 0
chk S1 "standard: queued 2, expired 2, admitted 0" "0 2 0 2 0" "$(bt_row standard)"

echo "=== X2: a client that leaves while queued"
sched_cfg 1 4 10000 "$SCHED_RESET" || fail X2 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-a x2run "$S3"; wait_receipts x2run 1 >/dev/null
CL_TIMEOUT=2 cl_one $PORT_SCHED t-crit x2c "$S3"; sleep 1
chk X2 "it waits" 1 "$(bq critical)"
sleep 2
chk X2 "gone at once when its client leaves" "0 0" "$(bq critical) $(fce .queued)"
cl_release x2run; wait_done "$S3" 'x2run' 1; sleep 0.5
chk X2 "nothing admitted on its behalf; everything returned" "0 0 0" "$(cl_receipts x2c) $(fce .inflight) $(fce .queued)"
bt_end
bt_check X2 1:1:0 0
chk X2 "critical: queued 1, cancelled 1" "0 1 0 0 1" "$(bt_row critical)"

echo "=== X3: a client that leaves while running"
sched_cfg 2 4 10000 "$SCHED_RESET" || fail X3 "pool shape"
bt_begin $PORT_SCHED
CL_TIMEOUT=2 cl_one $PORT_SCHED t-a x3 "$S3"; wait_receipts x3 1 >/dev/null
chk X3 "running" 1 "$(fce .inflight)"
sleep 2.5
chk X3 "its unit returned on close, the backend still holding it" "0 1" "$(fce .inflight) $(cl_receipts x3)"
cl_release x3
bt_end
bt_check X3 0:1:0 0

echo "=== X13: eviction: a critical arrival at a full queue takes a sheddable waiter's place"
sched_cfg 1 2 10000 ', "fc_shed_queue": "on", "fc_wake_policy": "inherit", "fc_tenant_max_share_pct": 0' || fail X13 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-a x13run "$S3"; wait_receipts x13run 1 >/dev/null
cl_one $PORT_SCHED t-shed x13b1 "$S3"; sleep 0.3; cl_one $PORT_SCHED t-shed x13b2 "$S3"; sleep 0.5
chk X13 "two sheddable wait, the queue at its depth" "2 2" "$(bq sheddable) $(fce .queued)"
cl_one $PORT_SCHED t-crit x13c "$S3"
wait_done "$S3" 'x13b2' 1 2
chk X13 "the youngest sheddable is answered on its own connection" "429:admission_shed" "$(cl_errs "$S3" 'x13b2')"
has X13 "with how long it waited" '"queued_ms"' "$(cat "$S3"/x13b2-1.body)"
has X13 "and its class" "X-Loxilb-Admission-Class: sheddable" "$(tr -d '\r' < "$S3"/x13b2-1.hdr)"
chk X13 "within the eviction, never a reset" yes "$(awk '{print ($1 == 429 && $2 < 2.5) ? "yes" : "no"}' "$S3"/x13b2-1.code)"
chk X13 "the critical one is queued, the older sheddable keeps its place" "1 1 0" "$(bq critical) $(bq sheddable) $(cl_receipts x13b2)"
cl_release x13run
chk X13 "the critical one is admitted first" "1 0" "$(wait_receipts x13c 1) $(cl_receipts x13b1)"
cl_release x13c; chk X13 "then the sheddable waiter" 1 "$(wait_receipts x13b1 1)"
cl_release x13b1; wait_done "$S3" 'x13*' 4
chk X13 "answers" "200 200 200 429:admission_shed" "$(cl_errs "$S3" 'x13*')"
sched_cfg 1 2 10000 "$SCHED_RESET" || fail X13 "reset"
bt_end
bt_check X13 1:1:2 1 2
chk X13 "sheddable: admitted 1, queued 2, shed 1 (the eviction)" "1 2 1 0 0" "$(bt_row sheddable)"

echo "=== X14: the mirror: fc_shed_queue off, nobody is evicted"
sched_cfg 1 2 10000 "$SCHED_RESET" || fail X14 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-a x14run "$S3"; wait_receipts x14run 1 >/dev/null
cl_one $PORT_SCHED t-b x14s1 "$S3"; sleep 0.3; cl_one $PORT_SCHED t-c x14s2 "$S3"; sleep 0.5
chk X14 "two standard wait, the queue at its depth" "2 2" "$(bq standard) $(fce .queued)"
cl_one $PORT_SCHED t-crit x14c "$S3"; wait_done "$S3" 'x14c' 1 3
chk X14 "the critical arrival is refused by the queue-full path" "429:admission_capacity" "$(cl_errs "$S3" 'x14c')"
has X14 "with its class on the refusal" "X-Loxilb-Admission-Class: critical" "$(tr -d '\r' < "$S3"/x14c-1.hdr)"
chk X14 "nobody was evicted" "2 0 0" "$(bq standard) $(cl_receipts x14s1) $(cl_receipts x14c)"
cl_one $PORT_SCHED t-shed x14b "$S3"; wait_done "$S3" 'x14b' 1 3
chk X14 "a sheddable arrival never waits" "429:admission_shed" "$(cl_errs "$S3" 'x14b')"
cl_release x14run; wait_receipts x14s1 1 >/dev/null; cl_release x14s1; wait_receipts x14s2 1 >/dev/null; cl_release x14s2
wait_done "$S3" 'x14*' 5
bt_end
bt_check X14 0:3:1 0
chk X14 "the refusal is a decision, not a band outcome" "1 0 0" "$(bd d.queue_full) $(bd critical.shed) $(bd critical.queued)"

echo "=== X15: more evictions in a second than the evicted list holds: every one answered"
sched_cfg 1 20 10000 ', "fc_shed_queue": "on", "fc_wake_policy": "inherit", "fc_tenant_max_share_pct": 0' || fail X15 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-a x15run "$S3"; wait_receipts x15run 1 >/dev/null
cl_fire $PORT_SCHED t-shed 20 x15b "$S3"
for i in $(seq 1 40); do [ "$(bq sheddable)" == 20 ] && break; sleep 0.25; done
chk X15 "twenty sheddable wait, the queue at its depth" "20 20" "$(bq sheddable) $(fce .queued)"
t0=$(date +%s.%N); cl_fire $PORT_SCHED t-crit 20 x15c "$S3"; t1=$(date +%s.%N)
chk X15 "twenty critical arrivals inside one second (more than the 16 the list holds)" yes "$(awk -v a="$t0" -v b="$t1" 'BEGIN { print (b - a < 1) ? "yes" : "no" }')"
wait_done "$S3" 'x15b' 20 4; t2=$(date +%s.%N)
chk X15 "every evicted waiter answered on its own connection" "$(for i in $(seq 20); do echo 429:admission_shed; done | tr '\n' ' ' | sed 's/ $//')" "$(cl_errs "$S3" 'x15b')"
chk X15 "within two 1 Hz passes of the last arrival" yes "$(awk -v a="$t1" -v b="$t2" 'BEGIN { print (b - a <= 2) ? "yes" : "no" }')"
chk X15 "the critical ones hold the queue, no sheddable left or served" "20 0 0" "$(bq critical) $(bq sheddable) $(cl_receipts x15b)"
cl_release x15run; cl_release x15c
wait_done "$S3" 'x15c' 20 20
chk X15 "the critical ones are all admitted" "20 0" "$(cl_receipts x15c) $(fce .queued)"
sched_cfg 1 2 10000 "$SCHED_RESET" || fail X15 "reset"
bt_end
bt_check X15 20:1:20 20 20
chk X15 "sheddable: admitted 0, queued 20, shed 20 (the evictions)" "0 20 20 0 0" "$(bt_row sheddable)"
chk X15 "no arrival refused as a full queue: the list kept up" 0 "$(bd d.queue_full)"

echo "=== X16: an evicted keep-alive client keeps its connection for its next request"
# A 429 is a retry signal: the eviction answer leaves the connection open, as
# the queue-full 429 does (X14's x14c is that 429). lib/ka_two.py sends both
# requests on one socket and never reconnects, so a close reads "closed".
sched_cfg 1 2 10000 ', "fc_shed_queue": "on", "fc_wake_policy": "inherit", "fc_tenant_max_share_pct": 0' || fail X16 "pool shape"
bt_begin $PORT_SCHED
rm -f "$S3/x16.gate"
cl_one $PORT_SCHED t-a x16run "$S3"; wait_receipts x16run 1 >/dev/null
cl_one $PORT_SCHED t-shed x16b1 "$S3"; sleep 0.3
( source ./keys.env; $hexec tqh1 python3 "$(pwd)/lib/ka_two.py" $VIP $PORT_SCHED "$KEY_t_shed" "$MODEL" x16k1 x16k2 "$S3/x16.gate" "$S3/x16k" ) &
KABG=$!
sleep 0.5
chk X16 "two sheddable wait, the keep-alive client the youngest, the queue at its depth" "2 2" "$(bq sheddable) $(fce .queued)"
cl_one $PORT_SCHED t-crit x16c "$S3"
wait_done "$S3" 'x16k' 1 3
chk X16 "the keep-alive client is evicted: 429 admission_shed" "429:admission_shed" "$(cl_errs "$S3" 'x16k')"
has X16 "with how long it waited" '"queued_ms"' "$(cat "$S3"/x16k-1.body)"
has X16 "the connection kept" "Connection: keep-alive" "$(tr -d '\r' < "$S3"/x16k-1.hdr)"
chk X16 "the same headers as the queue-full 429" \
  "$(tr -d '\r' < "$S3"/x14c-1.hdr | sed -n '2,$p' | cut -d: -f1 | grep -v '^$' | tr '\n' ' ')" \
  "$(tr -d '\r' < "$S3"/x16k-1.hdr | sed -n '2,$p' | cut -d: -f1 | grep -v '^$' | tr '\n' ' ')"
cl_release x16run; chk X16 "the critical one is admitted first" 1 "$(wait_receipts x16c 1)"
cl_release x16c; chk X16 "then the older sheddable waiter" 1 "$(wait_receipts x16b1 1)"
cl_release x16b1; wait_done "$S3" 'x16[rcb]*' 3
chk X16 "the pool is empty before the next request" "0 0" "$(fce .inflight) $(fce .queued)"
touch "$S3/x16.gate"; wait "$KABG"
chk X16 "its next request on the same connection is answered" 200 "$(awk '{print $1}' "$S3"/x16k-2.code 2>/dev/null)"
chk X16 "and reached the backend once" 1 "$(cl_receipts x16k2)"
chk X16 "answers" "200 200 200" "$(cl_errs "$S3" 'x16[rcb]*')"
sched_cfg 1 2 10000 "$SCHED_RESET" || fail X16 "reset"
bt_end
bt_check X16 1:1:3 1 2
chk X16 "sheddable: admitted 2, queued 2, shed 1 (the eviction)" "2 2 1 0 0" "$(bt_row sheddable)"

echo "=== P6: over h2c nothing queues, and the shed order holds"
h2run() { local t=$1; shift; source ./keys.env; local k; eval k=\$KEY_${t//-/_}; $hexec tqh1 python3 "$(pwd)/lib/h2_band.py" $VIP $PORT_H2 --api-key "$k" "$@"; }
bt_begin $PORT_H2
h2run t-a --streams 2 --delay-ms 4000 --nonce-prefix p6run > "$S3/p6run.out" 2>&1 &
H2BG=$!
sleep 1
chk P6 "two streams run (the ceiling)" 2 "$(tq_fce $PORT_H2 .inflight)"
h2run t-shed --streams 1 --nonce-prefix p6b > "$S3/p6b.out" 2>&1
P6B=$(jq -r 'select(.summary) | .summary | keys[0]' "$S3/p6b.out")
chk P6 "the stream's refusal carries its class and band wait" "sheddable 0" "$(jq -r 'select(.stream) | "\(.headers["x-loxilb-admission-class"]) \(.headers["x-loxilb-admission-band-wait-ms"])"' "$S3/p6b.out")"
P6C=$(h2run t-crit --streams 1 --nonce-prefix p6c | jq -r 'select(.summary) | .summary | keys[0]')
P6S=$(h2run t-b --streams 1 --nonce-prefix p6s | jq -r 'select(.summary) | .summary | keys[0]')
chk P6 "sheddable shed, critical and standard refused for capacity" "429:admission_shed 429:admission_capacity 429:admission_capacity" "$P6B $P6C $P6S"
chk P6 "nothing waited" 0 "$(tq_fce $PORT_H2 .queued)"
wait $H2BG
chk P6 "the running streams answered" '{"200":2}' "$(jq -c 'select(.summary) | .summary' "$S3/p6run.out")"
for t in t-crit t-a t-shed; do
  chk P6 "below the ceiling $t is admitted" '{"200":1}' "$(h2run $t --streams 1 --nonce-prefix p6low-$t | jq -c 'select(.summary) | .summary')"
done
bt_end
bt_check P6 1:3:2 0
chk P6 "sheddable: admitted 1, shed 1, never queued" "1 0 1 0 0" "$(bt_row sheddable)"

echo "=== P11: the band counter: fifteen series per service, band and outcome"
for p in $PORT_SCHED $PORT_H2 $PORT_IDLE $PORT_KNOB; do
  chk P11 "fifteen series for :$p" 15 "$(bt_snap $p | tr ' ' '\n' | sed -n 's/^series=//p')"
done
chk P11 "exactly the three bands and five outcomes" "critical.admitted critical.cancelled critical.expired critical.queued critical.shed sheddable.admitted sheddable.cancelled sheddable.expired sheddable.queued sheddable.shed standard.admitted standard.cancelled standard.expired standard.queued standard.shed" \
  "$(bt_snap $PORT_SCHED | tr ' ' '\n' | grep -E '^(critical|standard|sheddable)\.' | cut -d= -f1 | sort | tr '\n' ' ' | sed 's/ $//')"
chk P11 "only sheddable is ever shed by band policy" "0 0" "$(bt_snap $PORT_SCHED | tr ' ' '\n' | sed -n 's/^critical\.shed=//p') $(bt_snap $PORT_SCHED | tr ' ' '\n' | sed -n 's/^standard\.shed=//p')"

# ---- step 4: floors and the reserve ----------------------------------------------
# A floor is a guarantee under contention, not a reservation: it binds only
# while its band has a waiter. The reserve is the one true reservation. The
# rule the gate runs: when a unit frees, the highest-ranked band that is below
# its floor and has a runnable waiter takes it; when no band is below its
# floor, the wake policy chooses.
S4=$OUT/step4; mkdir -p "$S4"
# fl_json <floors c:s:b> [<shed queue on|inherit>] [<tenant share>] [<reserve>] [<adaptive>]:
# a gate-only replace of the keyed pool's band knobs, every one named so that
# the case before it leaves nothing behind (an omitted knob keeps its value,
# and a key given twice would be one the parser silently resolves).
fl_json() { echo ", \"fc_shed_queue\": \"${2:-inherit}\", \"fc_wake_policy\": \"inherit\", \"fc_tenant_max_share_pct\": ${3:-0}, \"fc_band_min_share_pct\": \"$1\", \"fc_critical_reserve_pct\": ${4:-0}, \"fc_adaptive\": \"${5:-inherit}\""; }
# drain_all: let every held request answer, again and again, until the pool is
# empty (a request admitted off the queue holds the backend until released).
drain_all() { local i; for i in $(seq 1 60); do cl_release_all; sleep 0.5; [ "$(fce .inflight) $(fce .queued)" == "0 0" ] && return 0; done; return 1; }
# be_waiting <n>: the backends report n requests waiting (the adaptive
# ceiling's signal); one process serves both ports.
be_waiting() { $hexec tqep1 curl -s -m 3 "http://127.0.0.1:8080/__waiting/$1" >/dev/null; }
# wait_cap <port> <want> [<secs>]: poll the ceiling in force
wait_cap() { local i got; for i in $(seq 1 $((${3:-60} * 2))); do got=$(tq_fce $1 .effective_max_outstanding); [ "$got" == "$2" ] && { echo "$got"; return; }; sleep 0.5; done; echo "$got"; }
# wait_full <want>: poll until the pool holds `want` units
wait_inflight() { local i got; for i in $(seq 1 $((${2:-4} * 4))); do got=$(fce .inflight); [ "$got" == "$1" ] && { echo "$got"; return; }; sleep 0.25; done; echo "$got"; }

echo "=== P4: the reserve: fc_critical_reserve_pct 25 at ceiling 4, standard capped at 3 with critical idle"
sched_cfg 4 8 30000 "$(fl_json 0:0:0 inherit 0 25)" || fail P4 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-a p4s1 "$S4"; cl_one $PORT_SCHED t-b p4s2 "$S4"; cl_one $PORT_SCHED t-c p4s3 "$S4"
for n in p4s1 p4s2 p4s3; do wait_receipts $n 1 >/dev/null; done
cl_one $PORT_SCHED t-a p4s4 "$S4"; sleep 0.8
chk P4 "the reserve in force: 25 %, one unit of four" "25 1" "$(fce .bands.critical_reserve_pct) $(fce .bands.critical_reserve_units)"
chk P4 "three standard run, the fourth waits: the fourth unit is the reserve, critical idle" "3 1 0" "$(bi standard) $(bq standard) $(cl_receipts p4s4)"
cl_one $PORT_SCHED t-shed p4b "$S4"; wait_done "$S4" p4b 1 3
chk P4 "a sheddable request is refused at once while a unit is free" "429:admission_shed" "$(cl_errs "$S4" p4b)"
cl_one $PORT_SCHED t-crit p4c "$S4"
chk P4 "critical takes the reserve" 1 "$(wait_receipts p4c 1)"
chk P4 "all four units in flight, the standard waiter still waiting" "4 1" "$(fce .inflight) $(bq standard)"
cl_release p4c; sleep 1.5
chk P4 "the reserve is back: nobody is woken for it" "3 0 1" "$(fce .inflight) $(cl_receipts p4s4) $(bq standard)"
cl_release p4s1
chk P4 "a unit that is not the reserve wakes the waiter" 1 "$(wait_receipts p4s4 1)"
drain_all; wait_done "$S4" 'p4*' 6 10
chk P4 "answers" "200 200 200 200 200 429:admission_shed" "$(cl_errs "$S4" 'p4*')"
bt_end
bt_check P4 1:4:1 0 1
chk P4 "critical: admitted 1; sheddable: shed 1" "1 0 0 0 0 | 0 0 1 0 0" "$(bt_row critical) | $(bt_row sheddable)"

echo "=== S3: strict, a standard floor of 20 % of 5: no standard demand leaves critical the whole ceiling; with standard waiting it keeps its unit"
sched_cfg 5 12 30000 "$(fl_json 0:20:0)" || fail S3 "pool shape"
bt_begin $PORT_SCHED
for i in 1 2 3 4 5; do cl_one $PORT_SCHED t-crit s3c$i "$S4"; done
for i in 1 2 3 4 5; do wait_receipts s3c$i 1 >/dev/null; done
chk S3 "the floor in units (20 % of 5)" 1 "$(fce .bands.standard.floor_units)"
chk S3 "no standard demand: critical holds all five (a floor is not a reservation)" "5 0" "$(bi critical) $(bi standard)"
cl_one $PORT_SCHED t-a s3s1 "$S4"; sleep 0.2; cl_one $PORT_SCHED t-b s3s2 "$S4"; sleep 0.2
for i in 6 7 8; do cl_one $PORT_SCHED t-crit s3c$i "$S4"; sleep 0.2; done
sleep 0.5
chk S3 "five critical run; two standard then three critical wait" "5 2 3" "$(bi critical) $(bq standard) $(bq critical)"
cl_release s3c1
chk S3 "the freed unit goes to standard, below its floor, not to an older higher-ranked waiter" "1 0" "$(wait_receipts s3s1 1) $(cl_receipts s3c6)"
chk S3 "standard holds its floor" "1 5" "$(bi standard) $(fce .inflight)"
cl_release s3c2
chk S3 "at its floor, the policy decides: a critical waiter" "1 0" "$(wait_receipts s3c6 1) $(cl_receipts s3s2)"
chk S3 "standard holds exactly its floor; its second waiter waits" "1 1" "$(bi standard) $(bq standard)"
drain_all; wait_done "$S4" 's3*' 10 10
chk S3 "answers" "200 200 200 200 200 200 200 200 200 200" "$(cl_errs "$S4" 's3*')"
bt_end
bt_check S3 8:2:0 0 5

echo "=== S12: three critical tenants fill the ceiling; with a floor standard keeps it"
sched_cfg 6 12 30000 "$(fl_json 0:30:0 inherit 34)" || fail S12 "pool shape"
bt_begin $PORT_SCHED
for t in "t-crit a" "t-crit2 b" "t-crit3 c"; do
  set -- $t; cl_one $PORT_SCHED $1 s12${2}1 "$S4"; cl_one $PORT_SCHED $1 s12${2}2 "$S4"
done
for x in a b c; do wait_receipts s12${x}1 1 >/dev/null; wait_receipts s12${x}2 1 >/dev/null; done
chk S12 "the floor in units (30 % of 6, rounded up)" 2 "$(fce .bands.standard.floor_units)"
chk S12 "three critical tenants fill the ceiling" "6 0" "$(bi critical) $(bi standard)"
cl_one $PORT_SCHED t-a s12s1 "$S4"; sleep 0.2; cl_one $PORT_SCHED t-b s12s2 "$S4"; sleep 0.2
for x in "t-crit a" "t-crit2 b" "t-crit3 c"; do set -- $x; cl_one $PORT_SCHED $1 s12${2}3 "$S4"; sleep 0.2; done
sleep 0.5
chk S12 "two standard and three critical wait" "2 3" "$(bq standard) $(bq critical)"
cl_release s12a1
chk S12 "a freed unit to standard, below its floor of two" 1 "$(wait_receipts s12s1 1)"
cl_release s12b1
chk S12 "the next to the second standard waiter" 1 "$(wait_receipts s12s2 1)"
chk S12 "standard keeps its floor while critical waits" "2 3" "$(bi standard) $(bq critical)"
cl_release s12c1; sleep 1
chk S12 "at its floor, one critical waiter takes the next unit" "1 2" "$(( $(cl_receipts s12a3) + $(cl_receipts s12b3) + $(cl_receipts s12c3) )) $(bq critical)"
chk S12 "and standard still holds exactly its floor" 2 "$(bi standard)"
drain_all; wait_done "$S4" 's12*' 11 10
chk S12 "answers" "$(for i in $(seq 11); do echo -n '200 '; done | sed 's/ $//')" "$(cl_errs "$S4" 's12*')"
bt_end
bt_check S12 9:2:0 0 5

echo "=== S13: strict, saturating critical demand, a sheddable floor of 10 %: never below it; with the floor at 0 it is starved"
# Ten critical requests fill the ceiling of ten; twenty more critical and four
# sheddable wait. Each round frees one critical unit; every third round the
# sheddable request in flight finishes too. After each round the pool is full
# again and the sheddable inflight is read.
starve_arm() { # starve_arm <floors> <tag> -> STARVE_R="<min sheddable inflight> <sheddable served by round 10> <critical inflight at the end>"
  local floors=$1 tag=$2 i r min=99 got
  sched_cfg 10 40 90000 "$(fl_json "$floors" on)" || { fail S13 "pool shape ($tag)"; return; }
  for i in $(seq 1 10); do cl_one $PORT_SCHED t-crit ${tag}c$i "$S4"; done
  for i in $(seq 1 10); do wait_receipts ${tag}c$i 1 >/dev/null; done
  for i in $(seq 11 30); do cl_one $PORT_SCHED t-crit ${tag}c$i "$S4"; done
  # One nonce per sheddable request: a release is sticky per nonce, so a shared
  # nonce would let every later sheddable request answer at once. They arrive
  # in order, so b1 is the first admitted, b2 the next.
  for i in 1 2 3 4; do cl_one $PORT_SCHED t-shed ${tag}b$i "$S4"; sleep 0.2; done
  sleep 1.5
  for r in $(seq 1 10); do
    cl_release ${tag}c$r
    [ $((r % 3)) -eq 0 ] && cl_release ${tag}b$((r / 3))
    sleep 0.7
    wait_inflight 10 4 >/dev/null
    got=$(bi sheddable); [ "$got" -lt "$min" ] && min=$got
  done
  STARVE_R="$min $(( $(cl_receipts ${tag}b1) + $(cl_receipts ${tag}b2) + $(cl_receipts ${tag}b3) + $(cl_receipts ${tag}b4) )) $(bi critical)"
}
bt_begin $PORT_SCHED
starve_arm 0:0:10 st1
chk S13 "floor 10 %: sheddable inflight never below one unit, and four served by round ten" "1 4 9" "$STARVE_R"
drain_all; wait_done "$S4" 'st1*' 34 10
starve_arm 0:0:0 st0
chk S13 "the mirror, floor 0: sheddable starved, nothing served in ten rounds" "0 0" "$(echo "$STARVE_R" | cut -d' ' -f1-2)"
chk S13 "critical took every freed unit" "10" "$(echo "$STARVE_R" | cut -d' ' -f3)"
drain_all; wait_done "$S4" 'st0*' 34 10
chk S13 "both arms answered in full (after the drain the starved sheddable are served)" "$(for i in $(seq 68); do echo -n '200 '; done | sed 's/ $//')" "$(cl_errs "$S4" 'st*')"
bt_end
bt_check S13 60:0:8 0 48

echo "=== P8: the adaptive ceiling tightens under a floor: new sheddable refused first, nothing cut, the floor in units follows it"
sched_cfg 8 8 90000 "$(fl_json 0:0:25 inherit 0 0 on), \"fc_telemetry_stale_ms\": 15000" || fail P8 "pool shape"
be_waiting 0
chk P8 "open at the configured ceiling; the floor is 2 units (25 % of 8)" "8 2" "$(wait_cap $PORT_SCHED 8 40) $(fce .bands.sheddable.floor_units)"
bt_begin $PORT_SCHED
for i in 1 2 3 4 5 6; do cl_one $PORT_SCHED t-crit p8c$i "$S4"; done
for i in 1 2 3 4 5 6; do wait_receipts p8c$i 1 >/dev/null; done
be_waiting 5
chk P8 "fresh waiting requests tighten it to a quarter" 2 "$(wait_cap $PORT_SCHED 2 90)"
chk P8 "the floor in units follows the ceiling in force (25 % of 2, never the whole ceiling)" 1 "$(fce .bands.sheddable.floor_units)"
chk P8 "nothing cut: the six running requests hold their units under a ceiling of two" "6 0" "$(fce .inflight) $(cl_receipts p8c7)"
cl_one $PORT_SCHED t-shed p8b "$S4"; wait_done "$S4" p8b 1 3
chk P8 "a new sheddable request is refused first, at once" "429:admission_shed" "$(cl_errs "$S4" p8b)"
cl_one $PORT_SCHED t-a p8s "$S4"; cl_one $PORT_SCHED t-crit p8c7 "$S4"; sleep 0.8
chk P8 "while standard and critical newcomers wait" "1 1 6" "$(bq standard) $(bq critical) $(fce .inflight)"
be_waiting 0
drain_all; wait_done "$S4" 'p8*' 9 20
chk P8 "answers" "200 200 200 200 200 200 200 200 429:admission_shed" "$(cl_errs "$S4" 'p8*')"
chk P8 "the ceiling is given back" 8 "$(wait_cap $PORT_SCHED 8 60)"
bt_end
bt_check P8 7:1:1 0 2

echo "=== X8: the adaptive ceiling tightens with ten waiters under a floor: nothing leaks"
sched_cfg 8 16 90000 "$(fl_json 0:20:0 on 0 0 on), \"fc_telemetry_stale_ms\": 15000" || fail X8 "pool shape"
be_waiting 0
wait_cap $PORT_SCHED 8 40 >/dev/null
bt_begin $PORT_SCHED
for i in 1 2 3 4 5 6 7 8; do cl_one $PORT_SCHED t-crit x8c$i "$S4"; done
for i in 1 2 3 4 5 6 7 8; do wait_receipts x8c$i 1 >/dev/null; done
for i in 1 2 3 4; do cl_one $PORT_SCHED t-a x8s$i "$S4"; sleep 0.1; done
for i in 9 10 11 12; do cl_one $PORT_SCHED t-crit x8c$i "$S4"; sleep 0.1; done
for i in 1 2; do cl_one $PORT_SCHED t-shed x8b$i "$S4"; sleep 0.1; done
sleep 0.5
chk X8 "eight run; ten wait (four standard, four critical, two sheddable)" "8 10" "$(fce .inflight) $(fce .queued)"
be_waiting 5
chk X8 "the ceiling tightens to two" 2 "$(wait_cap $PORT_SCHED 2 90)"
chk X8 "no waiter was dropped, nothing running was cut" "8 10" "$(fce .inflight) $(fce .queued)"
be_waiting 0
drain_all; wait_done "$S4" 'x8*' 18 20
chk X8 "everything answered 200" "$(for i in $(seq 18); do echo -n '200 '; done | sed 's/ $//')" "$(cl_errs "$S4" 'x8*')"
chk X8 "nothing leaks: no unit, no waiter, in any band" "0 0 0 0 0 0 0 0" "$(for b in critical standard sheddable; do echo -n "$(bi $b) $(bq $b) "; done | xargs) $(fce .inflight) $(fce .queued)"
chk X8 "the ceiling is given back" 8 "$(wait_cap $PORT_SCHED 8 60)"
bt_end
bt_check X8 12:4:2 0 10

echo "=== H2: over h2c nothing waits, so a floor never binds; the reserve decides refusals"
H2F=", \"api_key_auth\": \"required\", \"backend_protocol\": \"http2\", \"fc_max_outstanding\": 2"
h2cfg() { tq_api POST /config/loadbalancer "$(tq_rule_json $PORT_H2 "$H2F, \"fc_band_min_share_pct\": \"$1\", \"fc_critical_reserve_pct\": $2" 9090 9091)" | cut -c1-3; }
has H2 "a sheddable floor of 50 % on the h2c pool" "200" "$(h2cfg 0:0:50 0)"
bt_begin $PORT_H2
h2run t-a --streams 2 --delay-ms 3000 --nonce-prefix h2a > "$S4/h2a.out" 2>&1 &
H2BG=$!
sleep 1
chk H2 "two standard streams run: no sheddable stream waits, so the floor holds nothing back" 2 "$(tq_fce $PORT_H2 .inflight)"
wait $H2BG
chk H2 "both answered" '{"200":2}' "$(jq -c 'select(.summary) | .summary' "$S4/h2a.out")"
has H2 "the floor replaced by a reserve of 50 % (one unit of two)" "200" "$(h2cfg 0:0:0 50)"
h2run t-a --streams 1 --delay-ms 4000 --nonce-prefix h2b > "$S4/h2b.out" 2>&1 &
H2BG=$!
sleep 1
chk H2 "one stream runs, one unit is free and it is the reserve" "1 1" "$(tq_fce $PORT_H2 .inflight) $(tq_fce $PORT_H2 .bands.critical_reserve_units)"
H2S=$(h2run t-b --streams 1 --nonce-prefix h2s | jq -r 'select(.summary) | .summary | keys[0]')
H2B=$(h2run t-shed --streams 1 --nonce-prefix h2x | jq -r 'select(.summary) | .summary | keys[0]')
H2C=$(h2run t-crit --streams 1 --delay-ms 500 --nonce-prefix h2c | jq -c 'select(.summary) | .summary')
chk H2 "standard refused for capacity, sheddable shed, critical admitted" "429:admission_capacity 429:admission_shed" "$H2S $H2B"
chk H2 "critical takes the reserve" '{"200":1}' "$H2C"
chk H2 "nothing waited" 0 "$(tq_fce $PORT_H2 .queued)"
wait $H2BG
has H2 "the h2c pool back to its shape" "200" "$(h2cfg 0:0:0 0)"
bt_end
bt_check H2 1:3:1 0

echo "=== step 4: the pool back to its shape"
sched_cfg 8 16 30000 "$(fl_json 0:0:0 inherit 0 0 inherit)" || fail S13 "reset"


# ---- step 5: wait windows, queue shares, the client's TTL ------------------------
S5=$OUT/step5; mkdir -p "$S5"
# fl5_json <wait c:s:b> <queue pct c:s:b> <llm-d headers> <shed queue>: every knob of
# the step named, so that the case before leaves nothing behind.
fl5_json() { echo ", \"fc_shed_queue\": \"$4\", \"fc_wake_policy\": \"inherit\", \"fc_tenant_max_share_pct\": 0, \"fc_band_min_share_pct\": \"0:0:0\", \"fc_critical_reserve_pct\": 0, \"fc_adaptive\": \"inherit\", \"fc_band_wait_ms\": \"$1\", \"fc_band_queue_pct\": \"$2\", \"fc_llmd_headers\": \"$3\""; }
# cl_secs <outdir> <nonce>: the time curl measured for that request
cl_secs() { awk '{print $2}' "$1/$2"-1.code 2>/dev/null; }
within() { awk -v t="$1" -v lo="$2" -v hi="$3" 'BEGIN {print (t >= lo && t <= hi) ? "yes" : "no"}'; }
# cl_hdr <outdir> <nonce> <name>: the value of a response header ("" when absent)
cl_hdr() { tr -d '\r' < "$1/$2-1.hdr" 2>/dev/null | awk -F': ' -v n="$(echo "$3" | tr A-Z a-z)" 'tolower($1) == n {print $2; exit}'; }
# cl_seen <nonce>: the llm-d headers the backend received, compact JSON
cl_seen() { $hexec tqep1 curl -s -m 3 "http://127.0.0.1:8080/__headers/$1" | jq -cS '.headers'; }

echo "=== S6: per-band wait windows 0:5000:500 under a 30 s queue window"
sched_cfg 2 8 30000 "$(fl5_json 0:5000:500 0:0:0 inherit on)" || fail S6 "pool shape"
chk S6 "the windows as read back" "0 5000 500" "$(fce .bands.critical.wait_ms) $(fce .bands.standard.wait_ms) $(fce .bands.sheddable.wait_ms)"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-crit s6r1 "$S5"; cl_one $PORT_SCHED t-crit s6r2 "$S5"
wait_receipts s6r1 1 >/dev/null; wait_receipts s6r2 1 >/dev/null
cl_one $PORT_SCHED t-shed s6b "$S5"; sleep 0.2; cl_one $PORT_SCHED t-a s6s "$S5"; sleep 0.2
cl_one $PORT_SCHED t-crit s6c "$S5"; sleep 0.4
chk S6 "one waiter per band" "1 1 1" "$(bq critical) $(bq standard) $(bq sheddable)"
wait_done "$S5" s6b 1 5
chk S6 "the sheddable waiter ended first: 504 at its window" "504:admission_queue_timeout" "$(cl_errs "$S5" s6b)"
chk S6 "at 500 ms (to the next 1 Hz pass)" yes "$(within "$(cl_secs "$S5" s6b)" 0.4 2.6)"
chk S6 "its window on the answer" "500" "$(cl_hdr "$S5" s6b X-Loxilb-Admission-Band-Wait-Ms)"
chk S6 "the standard and the critical waiter are still waiting" "1 1 0" "$(bq critical) $(bq standard) $(bq sheddable)"
wait_done "$S5" s6s 1 9
chk S6 "the standard waiter ended at its window: 504" "504:admission_queue_timeout" "$(cl_errs "$S5" s6s)"
chk S6 "at 5 s, not at the queue's 30 s" yes "$(within "$(cl_secs "$S5" s6s)" 4.5 7.5)"
chk S6 "the critical waiter is still waiting (its window is the queue's)" "1" "$(bq critical)"
cl_release s6r1
chk S6 "the freed unit went to it" 1 "$(wait_receipts s6c 1)"
drain_all; wait_done "$S5" 's6*' 5 10
chk S6 "answers" "200 200 200 504:admission_queue_timeout 504:admission_queue_timeout" "$(cl_errs "$S5" 's6*' | sed 's/ /\n/g' | sort | tr '\n' ' ' | sed 's/ $//')"
bt_end
bt_check S6 3:1:1 0 1

echo "=== S7: per-band queue share 25:50:25 of a depth of 8"
sched_cfg 2 8 30000 "$(fl5_json 0:0:0 25:50:25 inherit on)" || fail S7 "pool shape"
chk S7 "the shares as read back" "25 50 25" "$(fce .bands.critical.queue_pct) $(fce .bands.standard.queue_pct) $(fce .bands.sheddable.queue_pct)"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-crit s7r1 "$S5"; cl_one $PORT_SCHED t-crit s7r2 "$S5"
wait_receipts s7r1 1 >/dev/null; wait_receipts s7r2 1 >/dev/null
for i in 1 2 3 4 5; do cl_one $PORT_SCHED t-shed s7b$i "$S5"; sleep 0.2; done
sleep 0.5
chk S7 "the sheddable band stops at 2 of 8" 2 "$(bq sheddable)"
chk S7 "the others were refused as a full queue at once" "429:admission_capacity 429:admission_capacity 429:admission_capacity" "$(cl_errs "$S5" 's7b[345]')"
for i in 1 2 3 4 5; do cl_one $PORT_SCHED t-a s7s$i "$S5"; sleep 0.2; done
sleep 0.5
chk S7 "the standard band stops at 4 of 8" 4 "$(bq standard)"
chk S7 "the fifth standard request was refused" "429:admission_capacity" "$(cl_errs "$S5" 's7s5')"
for i in 1 2 3; do cl_one $PORT_SCHED t-crit s7c$i "$S5"; sleep 0.2; done
sleep 0.5
chk S7 "the critical band stops at 2 of 8" 2 "$(bq critical)"
chk S7 "the third critical request was refused" "429:admission_capacity" "$(cl_errs "$S5" 's7c3')"
chk S7 "the queue holds its depth, no more" 8 "$(fce .queued)"
drain_all; wait_done "$S5" 's7*' 15 20
chk S7 "everyone queued was served: the two running, two critical, four standard, two sheddable" 10 "$(cl_errs "$S5" 's7*' | tr ' ' '\n' | grep -c '^200$')"
bt_end
chk S7 "five queue_full refusals in all (three sheddable, one standard, one critical)" 5 "$(bd d.queue_full)"
bt_check S7 4:4:2 0 8
echo "  (S7, part two: a share another band does not use is open to the rest)"
sched_cfg 2 4 30000 "$(fl5_json 0:0:0 25:0:0 inherit on)" || fail S7 "pool shape (part two)"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-crit s7q1 "$S5"; cl_one $PORT_SCHED t-crit s7q2 "$S5"
wait_receipts s7q1 1 >/dev/null; wait_receipts s7q2 1 >/dev/null
for i in 1 2 3 4 5; do cl_one $PORT_SCHED t-a s7t$i "$S5"; sleep 0.2; done
sleep 0.5
chk S7 "standard holds all four places although critical's share is one of them" 4 "$(bq standard)"
chk S7 "the fifth was refused by the depth" "429:admission_capacity" "$(cl_errs "$S5" 's7t5')"
drain_all; wait_done "$S5" 's7[qt]*' 7 10
bt_end
bt_check S7 2:4:0 0 4

echo "=== X1: a band's window ends while queued, in all three bands"
sched_cfg 2 8 30000 "$(fl5_json 1000:2000:500 0:0:0 inherit on)" || fail X1 "pool shape"
bt_begin $PORT_SCHED
cl_one $PORT_SCHED t-crit x1r1 "$S5"; cl_one $PORT_SCHED t-crit x1r2 "$S5"
wait_receipts x1r1 1 >/dev/null; wait_receipts x1r2 1 >/dev/null
cl_one $PORT_SCHED t-crit x1c "$S5"; sleep 0.2; cl_one $PORT_SCHED t-a x1s "$S5"; sleep 0.2; cl_one $PORT_SCHED t-shed x1b "$S5"; sleep 0.3
chk X1 "one waiter per band" "1 1 1" "$(bq critical) $(bq standard) $(bq sheddable)"
wait_done "$S5" 'x1[csb]' 3 8
chk X1 "all three ended 504 admission_queue_timeout" "504:admission_queue_timeout 504:admission_queue_timeout 504:admission_queue_timeout" "$(cl_errs "$S5" 'x1[csb]')"
chk X1 "critical at 1 s" yes "$(within "$(cl_secs "$S5" x1c)" 0.8 3.2)"
chk X1 "standard at 2 s" yes "$(within "$(cl_secs "$S5" x1s)" 1.8 4.2)"
chk X1 "sheddable at 500 ms" yes "$(within "$(cl_secs "$S5" x1b)" 0.4 2.6)"
chk X1 "each answer carries its band's window" "1000 2000 500" "$(cl_hdr "$S5" x1c X-Loxilb-Admission-Band-Wait-Ms) $(cl_hdr "$S5" x1s X-Loxilb-Admission-Band-Wait-Ms) $(cl_hdr "$S5" x1b X-Loxilb-Admission-Band-Wait-Ms)"
chk X1 "no backend receipt for any of the three" "0 0 0" "$(cl_receipts x1c) $(cl_receipts x1s) $(cl_receipts x1b)"
chk X1 "the two running requests are untouched, nothing waits" "2 0" "$(fce .inflight) $(fce .queued)"
drain_all; wait_done "$S5" 'x1*' 5 10
chk X1 "everything returned" "0 0" "$(fce .inflight) $(fce .queued)"
bt_end
bt_check X1 3:1:1 0 0

# X1t and X1o: one case per arm of fc_llmd_headers. Same pool, same requests: a
# 1.5 s client TTL under a 5 s queue window, a TTL longer than the window, one
# that is not a duration, no TTL, a sheddable request that is shed; the first
# request through carries the client's own copies of the objective and the
# fairness identity.
llmd_arm() { # llmd_arm <case> <on|off>
  local id=$1 arm=$2 d=$S5/$1
  mkdir -p "$d"
  sched_cfg 1 6 5000 "$(fl5_json 0:0:0 0:0:0 $arm off)" || fail $id "pool shape"
  chk $id "the switch as read back" "$arm" "$(fce .llmd_headers)"
  bt_begin $PORT_SCHED
  cl_one $PORT_SCHED t-crit ${id}f "$d" -H 'x-llm-d-inference-objective: spoof' -H 'x-llm-d-inference-fairness-id: spoof' \
    -H 'x-gateway-inference-objective: spoof-old' -H 'x-gateway-inference-fairness-id: spoof-old' -H 'x-llm-d-inference-ttl: 1h'
  wait_receipts ${id}f 1 >/dev/null
  SEEN=$(cl_seen ${id}f)
  cl_release ${id}f; wait_done "$d" ${id}f 1 5
  cl_one $PORT_SCHED t-crit ${id}h "$d"; wait_receipts ${id}h 1 >/dev/null
  cl_one $PORT_SCHED t-a ${id}a "$d" -H 'x-llm-d-inference-ttl: 1500ms'; sleep 0.2
  cl_one $PORT_SCHED t-b ${id}b "$d" -H 'x-llm-d-inference-ttl: 1h'; sleep 0.2
  cl_one $PORT_SCHED t-c ${id}c "$d" -H 'x-llm-d-inference-ttl: soon'; sleep 0.2
  cl_one $PORT_SCHED t-a ${id}d "$d"; sleep 0.2
  cl_one $PORT_SCHED t-shed ${id}e "$d"; sleep 0.3
  chk $id "four standard requests wait, the sheddable one is refused" "4 429:admission_shed" "$(bq standard) $(cl_errs "$d" ${id}e)"
  wait_done "$d" ${id}e 1 3
  REASON_SHED=$(cl_hdr "$d" ${id}e x-llm-d-request-dropped-reason)
  wait_done "$d" ${id}a 1 9
  A_ERR=$(cl_errs "$d" ${id}a); A_SECS=$(cl_secs "$d" ${id}a); A_REASON=$(cl_hdr "$d" ${id}a x-llm-d-request-dropped-reason)
  wait_done "$d" "${id}[bcd]" 3 12
  B_ERR=$(cl_errs "$d" "${id}[bcd]"); B_SECS=$(cl_secs "$d" ${id}b)
  B_REASON="$(cl_hdr "$d" ${id}b x-llm-d-request-dropped-reason)/$(cl_hdr "$d" ${id}c x-llm-d-request-dropped-reason)/$(cl_hdr "$d" ${id}d x-llm-d-request-dropped-reason)"
  drain_all; wait_done "$d" "${id}*" 7 10
  bt_end
}
echo "=== X1t: fc_llmd_headers on"
llmd_arm X1t on
chk X1t "the backend was sent the resolved objective and fairness identity, one copy each, none of the client's" \
  '{"x-llm-d-inference-fairness-id":["t-crit"],"x-llm-d-inference-objective":["critical"],"x-llm-d-inference-ttl":["1h"]}' "$SEEN"
chk X1t "a TTL shorter than the window ended the wait: 429 admission_ttl_expired" "429:admission_ttl_expired" "$A_ERR"
chk X1t "at about 1.5 s, not at the window" yes "$(within "$A_SECS" 1.2 3.6)"
chk X1t "with the dropped-reason for an expired TTL" rejected-ttl-expired "$A_REASON"
chk X1t "a longer TTL, one that is no duration and none: the window stands, 504" "504:admission_queue_timeout 504:admission_queue_timeout 504:admission_queue_timeout" "$B_ERR"
chk X1t "at the window of 5 s" yes "$(within "$B_SECS" 4.5 7.5)"
chk X1t "the window's 504 carries the dropped-reason too" "rejected-ttl-expired/rejected-ttl-expired/rejected-ttl-expired" "$B_REASON"
chk X1t "the shed refusal carries the saturated reason" rejected-saturated "$REASON_SHED"
bt_check X1t 2:4:1 0 0

echo "=== X1o: fc_llmd_headers off"
llmd_arm X1o off
chk X1o "the backend was sent the client's headers as they came" \
  '{"x-gateway-inference-fairness-id":["spoof-old"],"x-gateway-inference-objective":["spoof-old"],"x-llm-d-inference-fairness-id":["spoof"],"x-llm-d-inference-objective":["spoof"],"x-llm-d-inference-ttl":["1h"]}' "$SEEN"
chk X1o "the TTL header is ignored: every waiter ends at the window, 504" "504:admission_queue_timeout" "$A_ERR"
chk X1o "at 5 s, not at 1.5 s" yes "$(within "$A_SECS" 4.5 7.5)"
chk X1o "no dropped-reason on the 504" "" "$A_REASON"
chk X1o "the others end the same way" "504:admission_queue_timeout 504:admission_queue_timeout 504:admission_queue_timeout" "$B_ERR"
chk X1o "no dropped-reason on any" "//" "$B_REASON"
chk X1o "none on the shed refusal" "" "$REASON_SHED"
bt_check X1o 2:4:1 0 0

echo "=== X1h: over h2c, both arms of fc_llmd_headers"
h2cfg() { tq_api POST /config/loadbalancer "$(tq_rule_json $PORT_H2 ", \"api_key_auth\": \"required\", \"backend_protocol\": \"http2\", \"fc_max_outstanding\": 2, \"fc_shed_queue\": \"inherit\", \"fc_llmd_headers\": \"$1\"" 9090 9091)"; }
# h2seen <nonce>: the llm-d headers whichever h2c backend received for that nonce
h2seen() { local p; for p in 9090 9091; do $hexec tqep1 curl --http2-prior-knowledge -s -m 3 "http://127.0.0.1:$p/__headers/$1" | jq -cS '.headers | select(length > 0)'; done | head -1; }
x1h_arm() { # x1h_arm <on|off> <seen-want>
  local arm=$1 want=$2 o=$S5/x1h-$1
  has X1h "the $arm arm applied" 200 "$(h2cfg $arm)"
  h2run t-crit --streams 1 --nonce-prefix x1h-$arm --hdr 'x-llm-d-inference-objective: spoof' --hdr 'x-llm-d-inference-fairness-id: spoof' \
    --hdr 'x-gateway-inference-objective: spoof-old' --hdr 'x-gateway-inference-fairness-id: spoof-old' --hdr 'x-llm-d-inference-ttl: 1h' > "$o.out" 2>&1
  chk X1h "$arm: the stream was answered" '{"200":1}' "$(jq -c 'select(.summary) | .summary' "$o.out")"
  chk X1h "$arm: what the h2c backend received" "$want" "$(h2seen x1h-$arm-0)"
  h2run t-a --streams 2 --delay-ms 4000 --nonce-prefix x1h-run-$arm > "$o-run.out" 2>&1 &
  local bg=$!
  sleep 1
  h2run t-shed --streams 1 --nonce-prefix x1h-b-$arm > "$o-b.out" 2>&1
  chk X1h "$arm: a stream over the ceiling is shed" "429:admission_shed" "$(jq -r 'select(.summary) | .summary | keys[0]' "$o-b.out")"
  X1H_REASON=$(jq -r 'select(.stream) | .headers["x-llm-d-request-dropped-reason"] // ""' "$o-b.out")
  wait $bg
}
x1h_arm on '{"x-llm-d-inference-fairness-id":["t-crit"],"x-llm-d-inference-objective":["critical"],"x-llm-d-inference-ttl":["1h"]}'
chk X1h "on: the shed stream carries the saturated reason" rejected-saturated "$X1H_REASON"
x1h_arm off '{"x-gateway-inference-fairness-id":["spoof-old"],"x-gateway-inference-objective":["spoof-old"],"x-llm-d-inference-fairness-id":["spoof"],"x-llm-d-inference-objective":["spoof"],"x-llm-d-inference-ttl":["1h"]}'
chk X1h "off: no dropped-reason on the shed stream" "" "$X1H_REASON"
has X1h "the pool back to the process default" 200 "$(h2cfg inherit)"

echo "=== step 5: the pool back to its shape"
sched_cfg 8 16 30000 "$(fl5_json 0:0:0 0:0:0 inherit inherit)" || fail X1 "reset"

# ---- step 3, group B: P/D, stateless MCP, CHWBL, backend failures ---------------
# B15 last: it persists the configuration and restarts the gateway twice.
for c in P9 B6 B3a B3b B5 X4 X5 H3 B15; do case_$c; done

# ---- summary --------------------------------------------------------------------
echo "=== summary"
nd=0; np=0; npend=0; nf=0; nmiss=0; nelse=0
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
    elsewhere:*) nelse=$((nelse + 1)); echo "  $id RUN BY ${state#elsewhere:}"
      [ -n "${RESULT[$id]}" ] && { nmiss=$((nmiss + 1)); echo "  $id ran here while declared elsewhere"; } ;;
    pending:*) npend=$((npend + 1)); echo "  $id PENDING(${state#pending:})"
      [ -n "${RESULT[$id]}" ] && { nmiss=$((nmiss + 1)); echo "  $id ran while declared pending"; } ;;
    *) nmiss=$((nmiss + 1)); echo "  $id BAD DECLARATION" ;;
  esac
done <<< "$DECLARED"
for id in "${!RESULT[@]}"; do grep -q "^$id " <<< "$DECLARED" || { nmiss=$((nmiss + 1)); echo "  $id ran but is not declared"; }; done
echo "declared=$nd pass=$np fail=$nf pending=$npend elsewhere=$nelse not_executed=$nmiss"
[ "${REQUIRE_ALL:-0}" == 1 ] && [ $npend -gt 0 ] && { echo "SCENARIO-ai-tenant-sched [FAILED] $npend pending at REQUIRE_ALL"; exit 1; }
[ $nf -eq 0 ] && [ $nmiss -eq 0 ] || { echo "SCENARIO-ai-tenant-sched [FAILED]"; exit 1; }
echo "SCENARIO-ai-tenant-sched [OK]"
