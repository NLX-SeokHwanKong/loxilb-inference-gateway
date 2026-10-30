#!/bin/bash
# mcp - E2E validation of the loxilb-mcp bridge.
# Builds the bridge from this repo, points it at llb1 over streamable HTTP
# (loopback bind), and drives it with plain curl JSON-RPC — CI needs no LLM.
#
# Covered areas:
#   1 observe        health_overview / lb_list vs loxicmd / ct_list under
#                    traffic / metrics_snapshot
#   2 manage         lb_create -> real traffic -> lb_delete confirm-token
#                    round-trip + audit.jsonl entries
#   3 guardrails     viewer role sees no mutating tools; delete without
#                    confirm_token stays a preview
#   3b change gate   operator lists no admin-confirm tool; lb_create on an
#                    existing key is refused (operator) / previewed (admin);
#                    secrate_set preview -> confirm -> read-back -> rollback
#   4 ai ops         ai_apikey create(file)/list/update/delete confirm flow,
#                    ai_ratelimit_set/get, against the PostgreSQL store the
#                    topology runs (config.sh, --userservice)
#   4b writes        PR-B2: lb_update (admin_state_up down -> traffic stops ->
#                    rollback -> resumes; weight swap; fullproxy refusal passed
#                    on), operator previews with apply (API key, tenant and
#                    user rate limits), secrate_disable, config_persist, and
#                    the mutation budget (client ci-tight: 4/min, stop after
#                    3 failures lifted by a read)
#   5 rca            diagnose_l4_errors / capacity_report evidence bundles,
#                    ai_traffic_report accounting caveat, triage-alert mappings
#   5b capacity gate PR-D: an AI rule with ceiling 1 under four concurrent
#                    requests -> diagnose_ai_latency admission/P-D/EPP
#                    sections and ai_traffic_report fc_band; lb_update fc_*
#                    refused on the fullproxy rule (D26); fc_mode changed by
#                    an lb_create replace, read back in fc_effective, rolled
#                    back (D25)
# The AI 429-under-traffic drill and the 12h soak need an SSE backend and
# wall-clock time — they run separately on a dedicated testbed, not here.
source ../common.sh
echo SCENARIO-mcp

code=0
fail() { echo "  [FAILED] $1"; code=1; }
pass() { echo "  [OK] $1"; }

# ---------- bridge setup ----------

llbIP=$(docker inspect --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' llb1 2>/dev/null)
if [[ -z "$llbIP" ]]; then
    llbIP=$(docker inspect --format='{{.NetworkSettings.IPAddress}}' llb1 2>/dev/null)
fi
if [[ -z "$llbIP" ]]; then
    echo "SCENARIO-mcp [FAILED] (no llb1 container IP)"
    exit 1
fi

echo "building loxilb-mcp"
if command -v go >/dev/null 2>&1; then
    if ! (cd ../../mcp && go build -o ../cicd/mcp/loxilb-mcp ./cmd/loxilb-mcp); then
        echo "SCENARIO-mcp [FAILED] (build)"
        exit 1
    fi
elif [[ -x ./loxilb-mcp ]]; then
    echo "  host Go toolchain unavailable; using prebuilt ./loxilb-mcp"
else
    echo "SCENARIO-mcp [FAILED] (no Go toolchain and no prebuilt ./loxilb-mcp)"
    exit 1
fi

# ---------- target credentials ----------
# llb1 runs --userservice (config.sh): every REST call carries a bearer, the
# bridge logs in with the same credentials (username/password_env), and
# loxicmd inside llb1 reads the token file config.sh installed.
ADMIN_USER=admin
ADMIN_PW='Admin123!'
REST="http://$llbIP:11111/netlox/v1"
JWT=$(curl -s --max-time 10 -X POST "$REST/auth/login" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PW\"}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("token",""))' 2>/dev/null)
if [[ -z "$JWT" ]]; then
    echo "SCENARIO-mcp [FAILED] (REST login on llb1 failed; config.sh runs the gateway with --userservice)"
    exit 1
fi
AUTH=(-H "Authorization: Bearer $JWT")
echo "$JWT" | $dexec llb1 loxicmd set login --provider manual >/dev/null 2>&1
export LLB1_ADMIN_PASSWORD="$ADMIN_PW"

BTOKEN="ci-burst-token-0123456789abcdef0"
TTOKEN="ci-tight-token-0123456789abcdef0"
VTOKEN="ci-viewer-token-0123456789abcdef"
OTOKEN="ci-operator-token-0123456789abcd"
ATOKEN="ci-admin-token-0123456789abcdef0"
rm -rf .mcp-audit
cat > loxilb-mcp-ci.yaml <<EOF
default_target: llb1
targets:
  llb1:
    url: http://$llbIP:11111
    username: $ADMIN_USER
    password_env: LLB1_ADMIN_PASSWORD
clients:
  - { name: ci-viewer,   role: viewer,   token: $VTOKEN }
  # The scenario is one client doing a session's worth of changes in a few
  # minutes: the two mutating clients get a mutation budget sized for it
  # (default 6/min); ci-tight keeps a small one so the brakes are exercised.
  - { name: ci-operator, role: operator, token: $OTOKEN, mutations_per_minute: 120 }
  - { name: ci-admin,    role: admin,    token: $ATOKEN, mutations_per_minute: 120 }
  - { name: ci-burst,    role: viewer,   token: $BTOKEN, rps: 1, burst: 2 }
  - { name: ci-tight,    role: operator, token: $TTOKEN, mutations_per_minute: 4 }
audit_dir: ./.mcp-audit
secrets_dir: ./.mcp-audit/secrets
EOF

./loxilb-mcp --config loxilb-mcp-ci.yaml --transport http --listen 127.0.0.1:8891 &
MCP_PID=$!
cleanup() {
    kill $MCP_PID 2>/dev/null
    sudo killall -9 node 2>/dev/null
}
trap cleanup EXIT
sleep 2

MCP=http://127.0.0.1:8891

# ---------- minimal MCP-over-HTTP client ----------

# The bridge meters every HTTP request per client with a token bucket of
# burst 20 refilled at 10/s (T8); a request past the budget is answered
# with a plain-text HTTP 429, not an SSE frame. Each request here waits
# MCP_PACE first so the scenario stays under the refill rate whatever the
# runner's speed; a burst of 23 viewer reads inside 0.3 s hit the bucket
# on the hosted runner once.
MCP_PACE=${MCP_PACE:-0.11}
pace() { sleep "$MCP_PACE"; }

# mcp_session <token>: initialize a session, cache its id in $SESSION.
mcp_session() {
    local hdrs; hdrs=$(mktemp)
    pace
    curl -s -o /dev/null -D "$hdrs" "$MCP" \
        -H "Authorization: Bearer $1" -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cicd","version":"0"}}}'
    SESSION=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$hdrs" | tr -d '\r')
    rm -f "$hdrs"
    [[ -z "$SESSION" ]] && return 1
    pace
    curl -s -o /dev/null "$MCP" \
        -H "Authorization: Bearer $1" -H "Mcp-Session-Id: $SESSION" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
}

RPCID=1
# mcp_rpc <token> <method> [params-json]: raw JSON-RPC, prints the result
# line; an answer that is not an SSE frame (HTTP 401/429 text) is printed
# as is, so a refusal reads as what it was.
mcp_rpc() {
    RPCID=$((RPCID+1))
    local params="$3" body line
    [[ -z "$params" ]] && params='{}'
    pace
    body=$(curl -s "$MCP" \
        -H "Authorization: Bearer $1" -H "Mcp-Session-Id: $SESSION" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -d "{\"jsonrpc\":\"2.0\",\"id\":$RPCID,\"method\":\"$2\",\"params\":$params}")
    line=$(sed -n 's/^data: //p' <<< "$body")
    if [[ -n "$line" ]]; then echo "$line"; else echo "$body"; fi
}

# jfield <result-line> <python-expr>: a field of the tool's structured result
# (bound to r); the text form sorts keys, so greps on object literals do not work.
jfield() {
    python3 -c 'import json,sys; r=json.loads(json.loads(sys.stdin.read())["result"]["content"][0]["text"]); print(eval(sys.argv[1]))' "$2" <<< "$1" 2>/dev/null
}

# mcp_call <token> <tool> [args-json]: tools/call, prints the result line.
mcp_call() {
    local args="$3"
    [[ -z "$args" ]] && args='{}'
    mcp_rpc "$1" tools/call "{\"name\":\"$2\",\"arguments\":$args}"
}

# ---------- 0. sessions ----------

mcp_session "$ATOKEN" && pass "admin session established" || { fail "no admin session"; echo "SCENARIO-mcp [FAILED]"; exit 1; }
ADMIN_SESSION=$SESSION

# ---------- 1. observe ----------

echo "### observe"
res=$(mcp_call "$ATOKEN" health_overview)
echo "$res" | grep -q '"reachable":true' && pass "health_overview reachable" || fail "health_overview: $res"
# GET /status/ready is folded into health_overview; the verdict is present
# whether the gateway answered 200 or 503.
echo "$res" | grep -q '"ready":\(true\|false\)' && pass "health_overview carries the /status/ready verdict" || fail "health_overview without ready: $res"
res=$(mcp_call "$ATOKEN" version_get)
if echo "$res" | grep -q '"product":"loxilb-inference-gateway"'; then
    pass "version_get reports product and features"
else
    # VersionGetEntry.product/features are absent on builds that predate
    # them (swagger says so); the bridge then reports plain loxilb.
    echo "  [SKIP] version_get product/features (this gateway build predates the field)"
fi

res=$(mcp_call "$ATOKEN" lb_list)
mcpRules=$(echo "$res" | grep -o '"total_count":[0-9]*' | head -1 | cut -d: -f2)
cliRules=$($dexec llb1 loxicmd get lb -o json | grep -c '"externalIP"')
[[ -n "$mcpRules" && "$mcpRules" == "$cliRules" ]] \
    && pass "lb_list matches loxicmd ($mcpRules rules)" \
    || fail "lb_list=$mcpRules loxicmd=$cliRules"

$hexec l3ep1 node ../common/tcp_server.js server1 &
sleep 4
for i in $(seq 1 5); do $hexec l3h1 curl --max-time 5 -s 20.20.20.1:2020 >/dev/null; done
res=$(mcp_call "$ATOKEN" ct_list '{"service":""}')
echo "$res" | grep -q '"total_count"' && pass "ct_list returns aggregates" || fail "ct_list: $res"

res=$(mcp_call "$ATOKEN" metrics_snapshot '{"families":["loxilb_lb_rules","loxilb_active_conntrack_entries"]}')
echo "$res" | grep -q '"family_count":[1-9]' && pass "metrics_snapshot non-empty" || fail "metrics_snapshot: $res"

# ---------- 2. manage round-trip ----------

echo "### manage round-trip"
res=$(mcp_call "$ATOKEN" lb_create '{"external_ip":"20.20.20.2","port":2020,"protocol":"tcp","endpoints":[{"ip":"31.31.31.1","port":8080}]}')
echo "$res" | grep -q '"action":"executed"' && pass "lb_create executed" || fail "lb_create: $res"
sleep 3
out=$($hexec l3h1 curl --max-time 10 -s 20.20.20.2:2020)
[[ "$out" == "server1" ]] && pass "traffic flows through MCP-created VIP" || fail "no traffic through new VIP ($out)"

res=$(mcp_call "$ATOKEN" lb_delete '{"external_ip":"20.20.20.2","port":2020,"protocol":"tcp"}')
echo "$res" | grep -q '"action":"preview"' && pass "lb_delete first call previews" || fail "lb_delete preview: $res"
tok=$(echo "$res" | sed -n 's/.*"confirm_token":"\([^"]*\)".*/\1/p')
[[ -n "$tok" ]] || fail "no confirm_token in preview"
mcp_call "$ATOKEN" lb_list | grep -q '20.20.20.2' || fail "rule vanished after preview (must not)"

res=$(mcp_call "$ATOKEN" lb_delete "{\"external_ip\":\"20.20.20.2\",\"port\":2020,\"protocol\":\"tcp\",\"confirm_token\":\"$tok\"}")
echo "$res" | grep -q '"action":"executed"' && pass "lb_delete executed with token" || fail "lb_delete execute: $res"
mcp_call "$ATOKEN" lb_list | grep -q '20.20.20.2' && fail "rule still present after delete" || pass "rule gone after delete"

grep -q '"tool":"lb_create"' .mcp-audit/audit.jsonl && grep -q '"tool":"lb_delete"' .mcp-audit/audit.jsonl \
    && pass "audit.jsonl records create+delete" || fail "audit entries missing"

echo "### typed AI create/observe contract"
res=$(mcp_call "$ATOKEN" lb_create '{"external_ip":"20.20.20.3","port":2020,"protocol":"tcp","mode":4,"host":"20.20.20.3","sse_mode":false,"api_key_auth":"disabled","endpoints":[{"ip":"31.31.31.1","port":8080,"ep_role":0}]}')
echo "$res" | grep -q '"action":"executed"' && pass "typed AI lb_create executed" || fail "typed AI lb_create: $res"
sleep 3
res=$(mcp_call "$ATOKEN" lb_list '{"filter":"20.20.20.3"}')
echo "$res" | grep -q '"api_key_auth":"disabled"' && echo "$res" | grep -q '"host":"20.20.20.3"' \
    && pass "lb_list observes typed AI host and credential policy" \
    || fail "lb_list missing typed AI contract: $res"

res=$(mcp_call "$ATOKEN" lb_delete '{"external_ip":"20.20.20.3","port":2020,"protocol":"tcp","host_url":"20.20.20.3"}')
echo "$res" | grep -q '"action":"preview"' && pass "typed AI lb_delete previews the full rule key" || fail "typed AI lb_delete preview: $res"
tok=$(echo "$res" | sed -n 's/.*"confirm_token":"\([^"]*\)".*/\1/p')
res=$(mcp_call "$ATOKEN" lb_delete "{\"external_ip\":\"20.20.20.3\",\"port\":2020,\"protocol\":\"tcp\",\"host_url\":\"20.20.20.3\",\"confirm_token\":\"$tok\"}")
echo "$res" | grep -q '"action":"executed"' && pass "typed AI lb_delete executed with its full rule key" || fail "typed AI lb_delete execute: $res"
mcp_call "$ATOKEN" lb_list | grep -q '20.20.20.3' && fail "typed AI rule still present after delete" || pass "typed AI rule gone after delete"

# ---------- 2b. jwt credential policy (gap W1) ----------
# The bridge gains the profile tool in PR-B3; until then the profile is
# created through REST. A profile needs no reachable JWKS to be stored (the
# keyset is fetched asynchronously), so an .invalid issuer is enough for the
# create/observe/delete contract. Reuses the 20.20.20.3 VIP bound in config.sh.
echo "### jwt credential policy"
prof=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$REST/config/ai/jwtauthprofile" \
    "${AUTH[@]}" -H 'Content-Type: application/json' \
    -d '{"name":"ci-jwt","issuer":"https://issuer.ci.invalid","jwks_url":"https://issuer.ci.invalid/jwks.json","audiences":["ci"]}')
if [[ "$prof" == "200" || "$prof" == "201" || "$prof" == "204" ]]; then
    res=$(mcp_call "$ATOKEN" lb_create '{"external_ip":"20.20.20.3","port":2020,"protocol":"tcp","mode":4,"host":"jwt.ci","api_key_auth":"jwt","endpoints":[{"ip":"31.31.31.1","port":8080}]}')
    echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'jwt_auth_profile' \
        && pass "lb_create refuses api_key_auth=jwt without jwt_auth_profile before any request" \
        || fail "jwt without profile not refused: $res"
    res=$(mcp_call "$ATOKEN" lb_create '{"external_ip":"20.20.20.3","port":2020,"protocol":"tcp","mode":4,"host":"jwt.ci","api_key_auth":"jwt","jwt_auth_profile":"ci-jwt","endpoints":[{"ip":"31.31.31.1","port":8080}]}')
    echo "$res" | grep -q '"action":"executed"' && pass "lb_create with api_key_auth=jwt executed" || fail "jwt lb_create: $res"
    sleep 2
    res=$(mcp_call "$ATOKEN" lb_list '{"filter":"20.20.20.3"}')
    echo "$res" | grep -q '"api_key_auth":"jwt"' && echo "$res" | grep -q '"jwt_auth_profile":"ci-jwt"' \
        && pass "lb_list observes the jwt policy and its profile" \
        || fail "lb_list missing jwt contract: $res"
    res=$(mcp_call "$ATOKEN" lb_delete '{"external_ip":"20.20.20.3","port":2020,"protocol":"tcp","host_url":"jwt.ci"}')
    tok=$(echo "$res" | sed -n 's/.*"confirm_token":"\([^"]*\)".*/\1/p')
    res=$(mcp_call "$ATOKEN" lb_delete "{\"external_ip\":\"20.20.20.3\",\"port\":2020,\"protocol\":\"tcp\",\"host_url\":\"jwt.ci\",\"confirm_token\":\"$tok\"}")
    echo "$res" | grep -q '"action":"executed"' && pass "jwt rule deleted with token" || fail "jwt lb_delete: $res"
    curl -s -o /dev/null --max-time 10 "${AUTH[@]}" -X DELETE "$REST/config/ai/jwtauthprofile/ci-jwt"
else
    echo "  [SKIP] jwt credential policy (profile create answered HTTP $prof; the build has no usable jwtauthprofile API)"
fi

# ---------- 3. guardrails ----------

echo "### guardrails"
mcp_session "$VTOKEN" && pass "viewer session established" || fail "no viewer session"
res=$(mcp_rpc "$VTOKEN" tools/list)
echo "$res" | grep -q '"name":"lb_create"' && fail "viewer sees lb_create" || pass "viewer sees no lb_create"
echo "$res" | grep -q '"name":"ai_apikey_delete"' && fail "viewer sees ai_apikey_delete" || pass "viewer sees no ai_apikey_delete"
echo "$res" | grep -q '"name":"diagnose_l4_errors"' && pass "viewer sees diagnose tools" || fail "viewer missing diagnose tools"

# ---------- 3b. tier alignment: the change gate (D13) ----------
# Global/instance-scope changes are admin-confirm: an operator session does
# not list them, an admin session previews (current, proposed, diff,
# rollback) and applies only with the token. lb_create splits create
# (operator, direct) from replace (admin-confirm) by a pre-read of the key.
echo "### change gate"
mcp_session "$OTOKEN" && pass "operator session established" || fail "no operator session"
OPERATOR_SESSION=$SESSION
res=$(mcp_rpc "$OTOKEN" tools/list)
echo "$res" | grep -q '"name":"secrate_set"' && fail "operator sees secrate_set" || pass "operator sees no secrate_set (admin-confirm)"
echo "$res" | grep -q '"name":"gpu_mode_set"' && fail "operator sees gpu_mode_set" || pass "operator sees no gpu_mode_set (admin-confirm)"
echo "$res" | grep -q '"name":"lb_create"' && pass "operator still sees lb_create" || fail "operator missing lb_create"

# 20.20.20.1:2020/tcp is the baseline rule config.sh created (endpoint
# 31.31.31.1:8080, weight 1); a POST on that key with another endpoint port
# would replace it.
REPLACE='{"external_ip":"20.20.20.1","port":2020,"protocol":"tcp","endpoints":[{"ip":"31.31.31.1","port":9090}]}'
LBKEY="$REST/config/loadbalancer/externalipaddress/20.20.20.1/port/2020/protocol/tcp"
baseline_intact() { curl -s --max-time 10 "${AUTH[@]}" "$LBKEY" | grep -q '"targetPort":8080' && ! curl -s --max-time 10 "${AUTH[@]}" "$LBKEY" | grep -q '"targetPort":9090'; }
res=$(mcp_call "$OTOKEN" lb_create "$REPLACE")
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'admin-confirm' \
    && pass "operator lb_create on an existing key is refused" || fail "operator replace not refused: $res"
baseline_intact && pass "baseline rule untouched by the operator refusal" || fail "baseline rule changed"

SESSION=$ADMIN_SESSION
res=$(mcp_call "$ATOKEN" lb_create "$REPLACE")
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["rollback"]["tool"]')" == "lb_create" && -n "$(jfield "$res" 'r["diff"]["endpoints"]["to"]')" ]] \
    && pass "admin lb_create on an existing key previews the replace with diff and rollback" || fail "admin replace preview: $res"
baseline_intact && pass "baseline rule untouched by the preview" || fail "preview replaced the rule"
res=$(mcp_call "$ATOKEN" lb_create "${REPLACE%\}},\"confirm_token\":\"00000000000000000000000000000000\"}")
echo "$res" | grep -q '"isError":true' && pass "admin replace with a never-issued token is refused" || fail "stale token accepted: $res"
baseline_intact && pass "baseline rule untouched by the stale token" || fail "stale token replaced the rule"

# secrate_set. The plain image has no security-rate config, and POST
# /config/securityrate refuses a body that keeps every protection off (at
# least one per-source protection or an aggregate budget is required), so the
# gate must (1) refuse a change that leaves every protection off before it
# issues a token, (2) preview a change that switches one on with a
# secrate_disable rollback instead of a secrate_set call into the refused
# state, and (3) on the then-configured target preview -> confirm ->
# read-back -> rollback through secrate_set. secrate_disable (D18, the
# DELETE behind the change gate) restores the unconfigured state at the end.
res=$(mcp_call "$ATOKEN" secrate_set '{"rate_per_sec":777}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'enables no per-source protection' \
    && pass "secrate_set refuses a change that leaves every protection off, before any token" || fail "all-off change not refused: $res"
mcp_call "$ATOKEN" secrate_get | grep -q '"ratePerSec":777' && fail "refused change reached the gateway" || pass "refused change changed nothing (secrate_get)"
res=$(mcp_call "$ATOKEN" secrate_set '{"conn_rate_enabled":true,"rate_per_sec":777}')
# current.ratePerSec is 0 on a freshly booted image and 50 after a DELETE
# (the gateway keeps its built-in thresholds, disabled), so the check is on
# the flag the GET omits, not on the threshold value.
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["impact"]')" == "global" \
   && "$(jfield "$res" 'isinstance(r["current"]["ratePerSec"],int)')" == "True" && "$(jfield "$res" 'r["current"]["connRateEnabled"]')" == "False" \
   && "$(jfield "$res" 'r["proposed"]["connRateEnabled"]')" == "True" ]] \
    && pass "secrate_set previews current (zero defaults for the omitted keys), proposed and impact" || fail "secrate_set preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
[[ -n "$tok" && "$(jfield "$res" 'r["rollback"].get("tool","")')" == "secrate_disable" && "$(jfield "$res" 'r["rollback"]["note"]')" == *"DELETE /config/securityrate"* ]] \
    && pass "preview on the unconfigured target carries a token and a secrate_disable rollback" || fail "no token/secrate_disable rollback in preview: $res"
mcp_call "$ATOKEN" secrate_get | grep -q '"ratePerSec":777' && fail "preview changed the security rate config" || pass "preview changed nothing (secrate_get)"
res=$(mcp_call "$ATOKEN" secrate_set "{\"conn_rate_enabled\":true,\"rate_per_sec\":777,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["ratePerSec"]')" == "777" && "$(jfield "$res" 'r["read_back"]["connRateEnabled"]')" == "True" ]] \
    && pass "secrate_set applied with the token; read_back shows connRateEnabled and ratePerSec 777" || fail "secrate_set apply: $res"
mcp_call "$ATOKEN" secrate_get | grep -q '"ratePerSec":777' && pass "secrate_get confirms the change" || fail "secrate_get does not show 777"
# Configured target: a threshold change previews a secrate_set rollback and is put back through it.
res=$(mcp_call "$ATOKEN" secrate_set '{"rate_per_sec":778}')
tok=$(jfield "$res" 'r["confirm_token"]')
prev=$(jfield "$res" 'r["rollback"]["args"]["rate_per_sec"]')
[[ -n "$tok" && "$prev" == "777" && "$(jfield "$res" 'r["rollback"]["tool"]')" == "secrate_set" ]] \
    && pass "preview on the configured target carries a token and a secrate_set rollback (rate_per_sec $prev)" || fail "no token/rollback call in preview: $res"
res=$(mcp_call "$ATOKEN" secrate_set "{\"rate_per_sec\":778,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["ratePerSec"]')" == "778" ]] \
    && pass "secrate_set applied; read_back shows ratePerSec 778" || fail "secrate_set second apply: $res"
res=$(mcp_call "$ATOKEN" secrate_set "{\"rate_per_sec\":$prev}")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$ATOKEN" secrate_set "{\"rate_per_sec\":$prev,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["ratePerSec"]')" == "$prev" ]] \
    && pass "rollback applied; read_back shows ratePerSec $prev" || fail "rollback: $res"
grep -q '"tool":"secrate_set"' .mcp-audit/audit.jsonl && pass "audit.jsonl records secrate_set" || fail "secrate_set audit entry missing"
# secrate_disable: the DELETE behind the change gate; its preview carries
# the configuration it removes as a secrate_set rollback.
res=$(mcp_call "$ATOKEN" secrate_disable '{}')
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["impact"]')" == "global" && "$(jfield "$res" 'r["current"]["connRateEnabled"]')" == "True" \
   && "$(jfield "$res" 'r["proposed"]["connRateEnabled"]')" == "False" && "$(jfield "$res" 'r["rollback"]["tool"]')" == "secrate_set" \
   && "$(jfield "$res" 'r["rollback"]["args"]["conn_rate_enabled"]')" == "True" && "$(jfield "$res" 'r["rollback"]["args"]["rate_per_sec"]')" == "$prev" ]] \
    && pass "secrate_disable previews current, the all-off proposal and a secrate_set rollback (rate_per_sec $prev)" || fail "secrate_disable preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
mcp_call "$ATOKEN" secrate_get | grep -q '"connRateEnabled":true' && pass "preview changed nothing (protection still on)" || fail "secrate_disable preview changed the config"
res=$(mcp_call "$ATOKEN" secrate_disable "{\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["connRateEnabled"]')" == "False" ]] \
    && pass "secrate_disable applied with the token; read_back shows every protection off" || fail "secrate_disable apply: $res"
mcp_call "$ATOKEN" secrate_get | grep -q '"connRateEnabled":true' && fail "protection still on after secrate_disable" || pass "secrate_get shows no protection enabled again"
grep -q '"tool":"secrate_disable"' .mcp-audit/audit.jsonl && pass "audit.jsonl records secrate_disable" || fail "secrate_disable audit entry missing"

# ---------- 3d. the request budget (D20) ----------
# ci-burst has a budget of 2 requests refilled at 1/s: the session's
# initialize and initialized notification spend it, so the next request
# inside the second is refused. The refusal is a 429 with Retry-After and a
# JSON body, recorded as a rate_limit audit event, and the budget recovers.
echo "### request budget"
MCP_PACE=0 mcp_session "$BTOKEN" && pass "ci-burst session established (budget 2 at 1/s)" || fail "no ci-burst session"
hdrs=$(mktemp)
body=$(curl -s -D "$hdrs" "$MCP" -H "Authorization: Bearer $BTOKEN" -H "Mcp-Session-Id: $SESSION" \
    -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
    -d '{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}')
status=$(awk 'NR==1{print $2}' "$hdrs"); retry=$(awk 'tolower($1)=="retry-after:"{print $2}' "$hdrs" | tr -d '\r'); rm -f "$hdrs"
[[ "$status" == "429" ]] && pass "third request inside the second is refused with 429" || fail "request past the budget answered HTTP $status: $body"
[[ "$retry" =~ ^[0-9]+$ && "$retry" -ge 1 ]] && pass "429 carries Retry-After ($retry s)" || fail "Retry-After header missing or not whole seconds: '$retry'"
echo "$body" | python3 -c 'import sys,json; b=json.load(sys.stdin); assert b["error"]=="rate_limited" and b["client"]=="ci-burst" and b["retry_after_sec"]>=1 and b["limit"]=={"rps":1,"burst":2}, b' 2>/dev/null \
    && pass "429 body names the client, its budget (rps 1, burst 2) and retry_after_sec" || fail "429 body: $body"
grep -q '"rate_limit"' .mcp-audit/audit.jsonl && grep '"rate_limit"' .mcp-audit/audit.jsonl | grep -q 'ci-burst' \
    && pass "audit.jsonl records the rate_limit event for ci-burst" || fail "no rate_limit audit event for ci-burst"
sleep "$((retry + 1))"
res=$(MCP_PACE=0 mcp_rpc "$BTOKEN" tools/list)
echo "$res" | grep -q '"tools"' && pass "the budget recovers after Retry-After" || fail "after Retry-After: $res"
SESSION=$ADMIN_SESSION

# ---------- 4. AI ops ----------

# ---------- 3c. control-plane reads (PR-B1) ----------
# Every read is a viewer tool. A store-backed read the plain image cannot
# answer (HTTP 503 / 501) is reported as a skip, as the ai-ops legs are.

echo "### control-plane reads"
mcp_session "$VTOKEN" && pass "viewer session for the control-plane reads" || fail "no viewer session"
res=$(mcp_rpc "$VTOKEN" tools/list)
missing=""
for t in lb_get lb_kvexact_status ai_objective_list ai_netidentity_list ai_jwtprofile_list ai_user_ratelimit_list ai_user_ratelimit_get ai_ratelimit_defaults_get ai_model_profile_list ai_model_profile_get status_capabilities maintenance_get halfclose_get audit_status audit_policy_get audit_sink_get config_snapshot_get; do
    echo "$res" | grep -q "\"name\":\"$t\"" || missing="$missing $t"
done
[[ -z "$missing" ]] && pass "viewer lists the 17 control-plane reads" || fail "viewer misses:$missing"

# read_leg <tool> <args> <python-expr on r> <label>: OK when the expression
# is true; SKIP when the gateway answered 503/501 (no store behind the
# endpoint) or 404 "path ... was not found" (the published image predates
# the endpoint; the contract gate holds the bridge to the swagger of the
# tree, not of the image); FAILED otherwise.
# A read that lands while the gateway's debounced auto-persist holds the
# snapshot gate (it fires a quiet period after the change-gate writes) gets
# the bridge's retryable verdict {error: snapshot_gate_busy, retry_after_sec}
# (D21: the tool retries nothing itself). The leg does what an agent does:
# waits retry_after_sec and calls again, a bounded number of times, and
# says so.
read_leg() {
    local res try wait
    for try in 1 2 3; do
        res=$(mcp_call "$VTOKEN" "$1" "$2")
        echo "$res" | grep -q 'snapshot_gate_busy' || break
        wait=$(echo "$res" | grep -oE 'retry_after_sec\\?":[0-9]+' | grep -oE '[0-9]+$' | head -1)
        sleep "${wait:-3}"
    done
    [[ $try -gt 1 ]] && echo "  [NOTE] $1 answered snapshot_gate_busy $((try-1))x; retried after retry_after_sec (auto-persist held the snapshot gate)"
    if echo "$res" | grep -q '"isError":true'; then
        if echo "$res" | grep -qE 'HTTP 50[13]'; then echo "  [SKIP] $4 (gateway: $(echo "$res" | grep -oE 'HTTP 50[13][^"]{0,60}' | head -1))"; return; fi
        if echo "$res" | grep -qE 'HTTP 404: .*path [^ ]* was not found'; then echo "  [SKIP] $4 (this gateway build predates the endpoint: $(echo "$res" | grep -oE 'path [^ ]* was not found' | head -1))"; return; fi
        fail "$4: $res"; return
    fi
    [[ "$(jfield "$res" "$3")" == "True" ]] && pass "$4" || fail "$4: $res"
}

read_leg lb_get '{"external_ip":"20.20.20.1","port":2020,"protocol":"tcp","include":["status","stats"]}' \
    'r["rule"]["external_ip"]=="20.20.20.1" and "operatingStatus" in r["status"] and "totalConnections" in r["stats"]' \
    "lb_get by composite key attaches status and stats"
rid=$(jfield "$(mcp_call "$VTOKEN" lb_list)" 'next((x.get("id","") for x in r["rules"] if x["external_ip"]=="20.20.20.1"),"")')
if [[ -n "$rid" ]]; then
    read_leg lb_get "{\"id\":\"$rid\"}" 'r["rule"]["external_ip"]=="20.20.20.1"' "lb_get by opaque id ($rid)"
else
    echo "  [SKIP] lb_get by opaque id (this build assigns no rule id)"
fi
res=$(mcp_call "$VTOKEN" lb_get '{"external_ip":"20.20.20.9","port":2020,"protocol":"tcp"}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'no rule at 20.20.20.9:2020/tcp' \
    && pass "lb_get on a missing key says so" || fail "lb_get missing key: $res"
res=$(mcp_call "$VTOKEN" lb_kvexact_status '{"external_ip":"20.20.20.1","port":2020,"protocol":"tcp"}')
if echo "$res" | grep -q '"isError":true'; then
    echo "$res" | grep -qE 'HTTP 4(04|22)' && pass "lb_kvexact_status passes the gateway's verdict on a non-KV-exact key ($(echo "$res" | grep -oE 'HTTP 4[0-9]{2}' | head -1))" || fail "lb_kvexact_status: $res"
else
    [[ "$(jfield "$res" 'r["count"]')" == "0" ]] && pass "lb_kvexact_status reports no KV-exact rule on the baseline key" || fail "lb_kvexact_status on a plain rule: $res"
fi
read_leg status_capabilities '{}' 'r["count"]>=1 and "not_ready" in r' "status_capabilities lists the capability verdicts"
read_leg maintenance_get '{}' 'len(r.get("state",""))>0' "maintenance_get reports the state"
read_leg halfclose_get '{}' '"allow" in r and r["capSeconds"]>=1' "halfclose_get reports allow and capSeconds"
read_leg audit_status '{}' '"available" in r' "audit_status reports the trail state"
read_leg audit_policy_get '{}' '"max_segment_bytes" in r' "audit_policy_get reports the segment limits"
read_leg audit_sink_get '{}' '"sink" in r and r["named"]==False and (r["sink"]=={} or "address" in r["sink"])' "audit_sink_get reports the primary sink (an empty object when none is configured)"
read_leg config_snapshot_get '{}' 'r["truncated"]==False and r["size_bytes"]>0 and "snapshot" in r' "config_snapshot_get returns the whole snapshot with size and sha256"
read_leg config_snapshot_get '{"max_bytes":64}' 'r["truncated"]==True and "sections" in r and "snapshot" not in r' "config_snapshot_get over max_bytes describes the sections instead"
read_leg ai_objective_list '{}' '"objectives" in r and "source" in r' "ai_objective_list reports objectives and their source"
read_leg ai_netidentity_list '{}' '"bindings" in r' "ai_netidentity_list reports bindings"
read_leg ai_jwtprofile_list '{}' 'isinstance(r["count"],int)' "ai_jwtprofile_list reports profiles"
read_leg ai_user_ratelimit_list '{"tenant_id":"cicd"}' '"users" in r' "ai_user_ratelimit_list reports a tenant's rows"
# A fresh store holds no defaults row until PR-B2's ai_ratelimit_defaults_set
# writes one; the gateway's verdict for that state is a 404 "rate-limit
# defaults row not found", passed through as is.
res=$(mcp_call "$VTOKEN" ai_ratelimit_defaults_get '{"scope":"global"}')
if echo "$res" | grep -q '"isError":true'; then
    echo "$res" | grep -q 'HTTP 404: .*defaults row not found' \
        && pass "ai_ratelimit_defaults_get passes the gateway's verdict on an unset global row (HTTP 404)" \
        || fail "ai_ratelimit_defaults_get global: $res"
else
    [[ "$(jfield "$res" '"defaults" in r')" == "True" ]] && pass "ai_ratelimit_defaults_get reports the global row" || fail "ai_ratelimit_defaults_get global: $res"
fi
read_leg ai_model_profile_list '{}' '"profiles" in r' "ai_model_profile_list reports the published registry"
res=$(mcp_call "$VTOKEN" ai_ratelimit_defaults_get '{"scope":"tenant"}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'not global or rule' \
    && pass "ai_ratelimit_defaults_get refuses an unknown scope before any request" || fail "scope refusal: $res"

# ---------- PR-C1: L7, TLS and tracing reads (viewer) ----------
# Fixtures from config.sh, written over REST: certificate mcp-cert1 (SAN
# mcp-sni.test) and L7 policy mcp-l7pol1 on the plain rule 20.20.20.4:8082,
# whose Authorization insert header carries "mcp-fixture-secret".
echo "### l7/tls/tracing reads"
res=$(mcp_rpc "$VTOKEN" tools/list)
missing=""
for t in cert_get sni_cert_list l7policy_list l7policy_get trace_otlp_get trace_parsers_list trace_catalog_parser_get opa_watcher_get dpu_debug_get dpu_hwcounters_get; do
    echo "$res" | grep -q "\"name\":\"$t\"" || missing="$missing $t"
done
[[ -z "$missing" ]] && pass "viewer lists the 10 L7/TLS/tracing reads" || fail "viewer misses:$missing"

# The oracle for the fingerprint is the gateway's own certificate, hashed
# here from a raw REST read; the bridge result must carry it and no PEM.
FP=$(curl -s --max-time 10 -H "Authorization: Bearer $JWT" "$REST/config/cert/mcp-cert1" | python3 -c '
import sys,json,ssl,hashlib
print(hashlib.sha256(ssl.PEM_cert_to_DER_cert(json.load(sys.stdin)["certPem"])).hexdigest())' 2>/dev/null)
read_leg cert_get '{"cert_id":"mcp-cert1"}' \
    "r['usage']=='server' and 'mcp-sni.test' in r['hostnames'] and len(r['certificates'])==1 and r['certificates'][0]['sha256_fingerprint']=='$FP' and r['certificates'][0]['dns_names']==['mcp-sni.test'] and r['certificates'][0]['expired']==False and 'BEGIN' not in json.dumps(r) and 'Pem' not in json.dumps(r)" \
    "cert_get reports the metadata of mcp-cert1 (fingerprint = the gateway's certificate, no PEM)"
res=$(mcp_call "$VTOKEN" cert_get '{"cert_id":"mcp-nope"}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'HTTP 404' \
    && pass "cert_get passes the gateway's 404 on an unknown certId" || fail "cert_get unknown: $res"
read_leg sni_cert_list '{}' "any(c.get('hostname')=='mcp-sni.test' for c in r['certificates'])" \
    "sni_cert_list shows the SAN of mcp-cert1 in the global SNI store"
read_leg l7policy_list '{}' "any(p.get('id')=='mcp-l7pol1' for p in r['policies']) and 'mcp-fixture-secret' not in json.dumps(r) and '\"blue\"' in json.dumps(r)" \
    "l7policy_list shows mcp-l7pol1 with the Authorization insert header masked"
read_leg l7policy_get '{"id":"mcp-l7pol1"}' "r['policy']['rules'][0]['action']['kind']=='REJECT' and 'mcp-fixture-secret' not in json.dumps(r) and '[REDACTED]' in json.dumps(r)" \
    "l7policy_get reads mcp-l7pol1 by id, credential value masked"
res=$(mcp_call "$VTOKEN" l7policy_get '{"id":"mcp-nope"}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'HTTP 404' \
    && pass "l7policy_get passes the gateway's 404 on an unknown id" || fail "l7policy_get unknown: $res"
read_leg trace_otlp_get '{}' "'endpoint' in r and 'protocol' in r and isinstance(r['headers'],dict) and all(v=='[REDACTED]' or v.startswith('missing') for v in r['headers'].values())" \
    "trace_otlp_get reports the export target, header values never returned"
# The topology runs with tracing disabled; the gateway's verdict for the
# parser list is then a 500 "tracing not enabled", passed through as is.
res=$(mcp_call "$VTOKEN" trace_parsers_list)
if echo "$res" | grep -q '"isError":true'; then
    echo "$res" | grep -q 'HTTP 500: .*tracing not enabled' \
        && pass "trace_parsers_list passes the gateway's verdict while tracing is disabled (HTTP 500 tracing not enabled)" || fail "trace_parsers_list: $res"
else
    [[ "$(jfield "$res" "isinstance(r['count'],int) and r['count']==len(r['parsers'])")" == "True" ]] \
        && pass "trace_parsers_list reports $(jfield "$res" 'r["count"]') parsers" || fail "trace_parsers_list: $res"
fi
# No REST read lists the numeric catalog ids; id 1 either carries a mapping
# or the gateway's own 404 verdict, passed through.
res=$(mcp_call "$VTOKEN" trace_catalog_parser_get '{"catalog_id":1}')
if echo "$res" | grep -q '"isError":true'; then
    echo "$res" | grep -q 'HTTP 404' && pass "trace_catalog_parser_get passes the gateway's 404 for catalog 1 ($(echo "$res" | grep -oE 'No parser assigned to catalog [0-9]+' | head -1))" || fail "trace_catalog_parser_get: $res"
else
    [[ "$(jfield "$res" '"parser_name" in r["mapping"]')" == "True" ]] && pass "trace_catalog_parser_get reads the mapping of catalog 1" || fail "trace_catalog_parser_get: $res"
fi
res=$(mcp_call "$VTOKEN" trace_catalog_parser_get '{"catalog_id":256}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'out of range 1-255' \
    && pass "trace_catalog_parser_get refuses an id outside 1-255 before any request" || fail "catalog id bound: $res"
read_leg opa_watcher_get '{}' "r['status'] in ('not_configured','running','stopped') and 'note' in r" \
    "opa_watcher_get reports the watcher status ($(jfield "$(mcp_call "$VTOKEN" opa_watcher_get)" 'r.get("status","?")'))"
# The docker topology has no DPU: the gateway answers the disabled shape
# with 200, which is a verdict, not a skip.
res=$(mcp_call "$VTOKEN" dpu_debug_get)
if [[ "$(jfield "$res" 'r.get("enabled")')" == "False" ]]; then
    [[ "$(jfield "$res" 'r["filtered"]==False and "no DPU provider" in r["note"]')" == "True" ]] \
        && pass "dpu_debug_get reports enabled false on a host without a DPU" || fail "dpu_debug_get: $res"
    echo "  [NOTE] dpu_debug_get: no DPU provider on this host; counters are empty by construction"
else
    read_leg dpu_debug_get '{}' "'enabled' in r and 'plugins' in r" "dpu_debug_get reports the offload state"
fi
res=$(mcp_call "$VTOKEN" dpu_debug_get '{"limit":5}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'filtered mode only' \
    && pass "dpu_debug_get refuses a limit without a filter before any request" || fail "dpu_debug_get limit: $res"
res=$(mcp_call "$VTOKEN" dpu_debug_get '{"pipe":"no-such-pipe"}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'HTTP 400' \
    && pass "dpu_debug_get passes the gateway's 400 on an unknown pipe" || fail "dpu_debug_get pipe: $res"
read_leg dpu_hwcounters_get '{}' "r['total_flows']==len([f for f in r['flows'] if isinstance(f,dict)])" \
    "dpu_hwcounters_get reports $(jfield "$(mcp_call "$VTOKEN" dpu_hwcounters_get)" 'r.get("total_flows","?")') hardware rows"
SESSION=$ADMIN_SESSION

echo "### ai ops"
SESSION=$ADMIN_SESSION
# config.sh seeded tenant cicd-seed with one key; the store answers for real.
mcp_call "$ATOKEN" ai_apikey_list '{"tenant_id":"cicd-seed"}' | grep -q '"name":"seed-key"' \
    && pass "ai_apikey_list shows the seeded key of tenant cicd-seed" || fail "seeded key not listed"
# apply: true is the operator shortcut past the preview (the preview flows
# are in "### writes").
res=$(mcp_call "$ATOKEN" ai_apikey_create '{"tenant_id":"cicd","name":"mcp-ci","rate_limit_rps":5,"apply":true}')
if echo "$res" | grep -q '"key_file"'; then
    pass "ai_apikey_create (apply) wrote key to file"
    echo "$res" | grep -q '"raw_key"' && fail "raw key leaked in default create response" || pass "no key material in response"
    kf=$(jfield "$res" 'r["result"]["key_file"]')
    [[ -f "$kf" ]] && pass "key file exists on bridge host" || fail "key file $kf missing"
    kid=$(jfield "$res" 'r["result"]["key_id"]')
    [[ "$(jfield "$res" 'r["read_back"]["key_id"]')" == "$kid" ]] && pass "executed create re-reads the key ($kid)" || fail "create read_back: $res"

    mcp_call "$ATOKEN" ai_apikey_list '{"tenant_id":"cicd"}' | grep -q "$kid" \
        && pass "ai_apikey_list shows the key" || fail "created key not listed"

    res=$(mcp_call "$ATOKEN" ai_apikey_update "{\"key_id\":\"$kid\",\"enabled\":false,\"apply\":true}")
    [[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["enabled"]')" == "False" ]] && pass "ai_apikey_update (apply) disabled the key; read_back shows enabled false" || fail "ai_apikey_update: $res"

    res=$(mcp_call "$ATOKEN" ai_apikey_delete "{\"key_id\":\"$kid\"}")
    tok=$(echo "$res" | sed -n 's/.*"confirm_token":"\([^"]*\)".*/\1/p')
    [[ -n "$tok" ]] && pass "ai_apikey_delete previews with token" || fail "ai_apikey_delete preview: $res"
    res=$(mcp_call "$ATOKEN" ai_apikey_delete "{\"key_id\":\"$kid\",\"confirm_token\":\"$tok\"}")
    echo "$res" | grep -q '"action":"executed"' && pass "ai_apikey_delete executed" || fail "ai_apikey_delete execute: $res"
else
    fail "ai_apikey_create against the topology's store: $(echo "$res" | head -c 200)"
fi

res=$(mcp_call "$ATOKEN" ai_ratelimit_set '{"tenant_id":"cicd","rps":5,"tokens_per_min":1000,"apply":true}')
if echo "$res" | grep -q '"action":"executed"'; then
    pass "ai_ratelimit_set (apply) executed"
    mcp_call "$ATOKEN" ai_ratelimit_get '{"tenant_id":"cicd"}' | grep -q '"rps":5' \
        && pass "ai_ratelimit_get reads back rps=5" || fail "ai_ratelimit_get readback"
else
    fail "ai_ratelimit_set against the topology's store: $(echo "$res" | head -c 200)"
fi

# ---------- 4b. PR-B2 writes ----------
# Every write previews (current, proposed, diff, rollback, impact) and
# applies only with the preview's token; an operator write on one object
# or tenant may also be applied directly with apply: true. Executed
# results carry read_back.

echo "### writes"
SESSION=$OPERATOR_SESSION
res=$(mcp_rpc "$OTOKEN" tools/list)
for t in lb_update secrate_disable config_persist; do
    echo "$res" | grep -q "\"name\":\"$t\"" && fail "operator sees $t" || pass "operator sees no $t (admin-confirm)"
done
for t in ai_user_ratelimit_set ai_user_ratelimit_delete; do
    echo "$res" | grep -q "\"name\":\"$t\"" && pass "operator sees $t" || fail "operator misses $t"
done

# lb_update on the baseline rule 20.20.20.1:2020/tcp (l3ep1 serves
# server1): admin_state_up false -> traffic stops -> the preview's rollback
# -> traffic resumes; then a weight swap round-trip.
SESSION=$ADMIN_SESSION
KEY='"external_ip":"20.20.20.1","port":2020,"protocol":"tcp"'
serves() { [[ "$($hexec l3h1 curl --max-time 3 -s 20.20.20.1:2020)" == "server1" ]]; }
serves && pass "baseline VIP serves before lb_update" || fail "baseline VIP not serving before lb_update"
# D24: the gateway serialises adminStateUp with omitempty; an enabled rule
# carries an explicit true, a paused one no flag. lb_get/lb_list report the
# flag as read (true | false | "absent").
res=$(mcp_call "$ATOKEN" lb_get "{$KEY}")
[[ "$(jfield "$res" 'r["rule"]["admin_state_up"]')" == "True" ]] \
    && pass "lb_get reports the enabled rule's explicit admin_state_up true" || fail "lb_get enabled admin_state_up: $res"
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"admin_state_up\":false}")
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["impact"]')" == "service" \
   && "$(jfield "$res" 'r["diff"]["adminStateUp"]["to"]')" == "False" && "$(jfield "$res" 'r["rollback"]["tool"]')" == "lb_update" \
   && "$(jfield "$res" 'r["rollback"]["args"]["admin_state_up"]')" == "True" ]] \
    && pass "lb_update admin_state_up=false previews current, diff and an lb_update rollback (admin_state_up true)" || fail "lb_update preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
serves && pass "preview changed nothing (VIP still serves)" || fail "the preview took the VIP down"
# The gateway omits a false adminStateUp from the rule it returns
# (omitempty); the read-back after sending false is the one place the
# bridge resolves the absent flag to false, and it says so in a note.
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"admin_state_up\":false,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["admin_state_up"]')" == "False" \
   && "$(jfield "$res" '"adminStateUp" in r["read_back"]["rule"]["serviceArguments"]')" == "False" \
   && "$(jfield "$res" '"omitempty" in r["read_back"]["admin_state_up_note"]')" == "True" \
   && "$(jfield "$res" 'r["read_back"]["status"]["operatingStatus"]')" == "OFFLINE" ]] \
    && pass "lb_update applied; the rule GET has no adminStateUp, read_back resolves it to false with the note, status OFFLINE" || fail "lb_update apply: $res"
sleep 2
serves && fail "VIP still serves with adminStateUp false" || pass "traffic stops with adminStateUp false"
res=$(mcp_call "$ATOKEN" lb_get "{$KEY,\"include\":[\"status\"]}")
[[ "$(jfield "$res" 'r["rule"]["admin_state_up"]')" == "absent" && "$(jfield "$res" '"adminStateUp" in r["status"]')" == "False" \
   && "$(jfield "$res" 'r["status"]["operatingStatus"]')" == "OFFLINE" ]] \
    && pass "lb_get reports the paused rule's admin_state_up as absent (status: no flag, OFFLINE)" || fail "lb_get paused admin_state_up: $res"
res=$(mcp_call "$ATOKEN" lb_list '{"filter":"20.20.20.1"}')
[[ "$(jfield "$res" '[x["admin_state_up"] for x in r["rules"] if x["port"]==2020 and x["protocol"]=="tcp"][0]')" == "absent" ]] \
    && pass "lb_list reports the paused rule's admin_state_up as absent" || fail "lb_list paused admin_state_up: $res"
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"admin_state_up\":true}")
[[ "$(jfield "$res" 'r["current"]["adminStateUp"]')" == "absent" && "$(jfield "$res" '"admin_state_up" in r["rollback"]["args"]')" == "False" \
   && "$(jfield "$res" '"absent" in r["rollback"]["note"]')" == "True" ]] \
    && pass "resume preview reports adminStateUp absent and its rollback states no value, with the note" || fail "resume preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"admin_state_up\":true,\"confirm_token\":\"$tok\"}")
# The baseline rule has no health monitor: an enabled rule reads NO_MONITOR
# (deriveOperatingStatus), ONLINE needs probing.
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["admin_state_up"]')" == "True" \
   && "$(jfield "$res" '"admin_state_up_note" in r["read_back"]')" == "False" \
   && "$(jfield "$res" 'r["read_back"]["status"]["operatingStatus"]')" == "NO_MONITOR" ]] \
    && pass "rollback applied; read_back shows admin_state_up true and status NO_MONITOR (no health monitor)" || fail "lb_update rollback: $res"
sleep 2
serves && pass "traffic resumes after the rollback" || fail "VIP not serving after the rollback"
# weight swap: the bridge sends the full endpoint list with the weight changed.
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"endpoint_weights\":[{\"ip\":\"31.31.31.1\",\"port\":8080,\"weight\":2}]}")
[[ "$(jfield "$res" 'r["action"]')" == "preview" && -n "$(jfield "$res" 'r["diff"]["endpoints"]')" \
   && "$(jfield "$res" 'r["rollback"]["args"]["endpoint_weights"][0]["weight"]')" == "1" ]] \
    && pass "lb_update weight 1->2 previews the endpoint diff and a rollback to weight 1" || fail "weight preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"endpoint_weights\":[{\"ip\":\"31.31.31.1\",\"port\":8080,\"weight\":2}],\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["rule"]["endpoints"][0]["weight"]')" == "2" ]] \
    && pass "weight applied; read_back shows weight 2" || fail "weight apply: $res"
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"endpoint_weights\":[{\"ip\":\"31.31.31.1\",\"port\":8080,\"weight\":1}]}")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"endpoint_weights\":[{\"ip\":\"31.31.31.1\",\"port\":8080,\"weight\":1}],\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["rule"]["endpoints"][0]["weight"]')" == "1" ]] \
    && pass "weight rolled back to 1" || fail "weight rollback: $res"
sleep 2
serves && pass "VIP serves after the weight round-trip" || fail "VIP not serving after the weight round-trip"
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"name\":\"renamed\"}")
echo "$res" | grep -q '"isError":true' && pass "lb_update refuses a field outside the phase-1 allowlist (schema)" || fail "unknown field accepted: $res"
res=$(mcp_call "$ATOKEN" lb_update "{$KEY,\"endpoint_weights\":[{\"ip\":\"31.31.31.9\",\"port\":8080,\"weight\":2}]}")
echo "$res" | grep -q 'not an endpoint of the rule' && pass "lb_update refuses a weight for an endpoint the rule does not have, before any request" || fail "foreign endpoint accepted: $res"
# D8: a fullproxy (mode 4) rule cannot be patched; the gateway's answer is
# passed on with the hint naming the replace path.
res=$(mcp_call "$ATOKEN" lb_create '{"external_ip":"20.20.20.3","port":2020,"protocol":"tcp","mode":4,"host":"patch.ci","endpoints":[{"ip":"31.31.31.1","port":8080}]}')
echo "$res" | grep -q '"action":"executed"' && pass "fullproxy rule created for the PATCH refusal leg" || fail "fullproxy lb_create: $res"
res=$(mcp_call "$ATOKEN" lb_update '{"external_ip":"20.20.20.3","port":2020,"protocol":"tcp","admin_state_up":false}')
tok=$(jfield "$res" 'r["confirm_token"]')
[[ -n "$tok" ]] && pass "lb_update on a fullproxy rule previews (the gateway, not the bridge, refuses it)" || fail "fullproxy preview: $res"
res=$(mcp_call "$ATOKEN" lb_update "{\"external_ip\":\"20.20.20.3\",\"port\":2020,\"protocol\":\"tcp\",\"admin_state_up\":false,\"confirm_token\":\"$tok\"}")
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'fullproxy' && echo "$res" | grep -q 'lb_create on the same key' \
    && pass "the gateway's fullproxy refusal is passed on verbatim with the replace hint" || fail "fullproxy apply: $res"
res=$(mcp_call "$ATOKEN" lb_delete '{"external_ip":"20.20.20.3","port":2020,"protocol":"tcp","host_url":"patch.ci"}')
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$ATOKEN" lb_delete "{\"external_ip\":\"20.20.20.3\",\"port\":2020,\"protocol\":\"tcp\",\"host_url\":\"patch.ci\",\"confirm_token\":\"$tok\"}")
echo "$res" | grep -q '"action":"executed"' && pass "fullproxy rule removed" || fail "fullproxy lb_delete: $res"

# Operator writes with preview: API key, tenant rate limit, user rate limit.
SESSION=$OPERATOR_SESSION
res=$(mcp_call "$OTOKEN" ai_apikey_create '{"tenant_id":"cicd","name":"mcp-b2","priority_class":"sheddable"}')
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["impact"]')" == "tenant" && "$(jfield "$res" 'r["proposed"]["priority_class"]')" == "sheddable" \
   && "$(jfield "$res" 'r["rollback"]["note"]')" == *"ai_apikey_delete"* ]] \
    && pass "ai_apikey_create previews the key with its priority class and a rollback note naming ai_apikey_delete" || fail "apikey create preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_apikey_create "{\"tenant_id\":\"cicd\",\"name\":\"mcp-b2\",\"priority_class\":\"sheddable\",\"confirm_token\":\"$tok\"}")
kid2=$(jfield "$res" 'r["result"]["key_id"]')
[[ "$(jfield "$res" 'r["action"]')" == "executed" && -n "$kid2" && -f "$(jfield "$res" 'r["result"]["key_file"]')" && "$(jfield "$res" 'r["read_back"]["priority_class"]')" == "sheddable" ]] \
    && pass "ai_apikey_create applied with the token; key file written, read_back shows the priority class" || fail "apikey create apply: $res"
res=$(mcp_call "$OTOKEN" ai_apikey_create '{"tenant_id":"cicd","priority_class":"urgent"}')
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'priority_class' && pass "ai_apikey_create refuses a priority class outside the contract" || fail "unknown priority_class: $res"
res=$(mcp_call "$OTOKEN" ai_apikey_update "{\"key_id\":\"$kid2\",\"enabled\":false,\"tokens_per_min\":1234}")
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["impact"]')" == "object" && "$(jfield "$res" 'r["current"]["enabled"]')" == "True" \
   && "$(jfield "$res" 'r["diff"]["enabled"]["to"]')" == "False" && "$(jfield "$res" 'r["rollback"]["tool"]')" == "ai_apikey_update" && "$(jfield "$res" 'r["rollback"]["args"]["enabled"]')" == "True" ]] \
    && pass "ai_apikey_update previews current, diff and an ai_apikey_update rollback" || fail "apikey update preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_apikey_update "{\"key_id\":\"$kid2\",\"enabled\":false,\"tokens_per_min\":1234,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["enabled"]')" == "False" ]] \
    && pass "ai_apikey_update applied; read_back shows enabled false" || fail "apikey update apply: $res"
res=$(mcp_call "$OTOKEN" ai_apikey_update "{\"key_id\":\"$kid2\",\"enabled\":true}")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_apikey_update "{\"key_id\":\"$kid2\",\"enabled\":true,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["enabled"]')" == "True" ]] \
    && pass "rollback applied; read_back shows enabled true" || fail "apikey update rollback: $res"
SESSION=$ADMIN_SESSION
res=$(mcp_call "$ATOKEN" ai_apikey_delete "{\"key_id\":\"$kid2\"}")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$ATOKEN" ai_apikey_delete "{\"key_id\":\"$kid2\",\"confirm_token\":\"$tok\"}")
echo "$res" | grep -q '"action":"executed"' && pass "the preview-created key is deleted" || fail "apikey delete: $res"

SESSION=$OPERATOR_SESSION
# tenant rate limit: the ai-ops section left cicd at rps 5 / 1000 tpm.
res=$(mcp_call "$OTOKEN" ai_ratelimit_set '{"tenant_id":"cicd","rps":7,"tokens_per_min":3000,"model_limits":[{"model":"llama3","tokens_per_min":500}]}')
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["impact"]')" == "tenant" && "$(jfield "$res" 'r["current"]["rps"]')" == "5" \
   && "$(jfield "$res" 'r["rollback"]["args"]["rps"]')" == "5" && "$(jfield "$res" 'r["rollback"]["args"]["model_limits"][0]["tokens_per_min"]')" == "0" ]] \
    && pass "ai_ratelimit_set previews the current row and a rollback that zeroes the new model quota" || fail "ratelimit preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_ratelimit_set "{\"tenant_id\":\"cicd\",\"rps\":7,\"tokens_per_min\":3000,\"model_limits\":[{\"model\":\"llama3\",\"tokens_per_min\":500}],\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["rps"]')" == "7" && "$(jfield "$res" 'r["read_back"]["model_limits"][0]["tokens_per_min"]')" == "500" ]] \
    && pass "ai_ratelimit_set applied; read_back shows rps 7 and the llama3 quota" || fail "ratelimit apply: $res"
RB='{"tenant_id":"cicd","rps":5,"tokens_per_min":1000,"burst_pct":0,"model_limits":[{"model":"llama3","tokens_per_min":0}]'
res=$(mcp_call "$OTOKEN" ai_ratelimit_set "$RB}")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_ratelimit_set "$RB,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["rps"]')" == "5" && "$(jfield "$res" 'len(r["read_back"].get("model_limits") or [])')" == "0" ]] \
    && pass "rollback applied; read_back shows rps 5 and no model quota" || fail "ratelimit rollback: $res"

# user rate limit: set (no row -> rollback is the delete), get, delete
# (rollback is the set with the row), get 404, set back, delete again.
UKEY='"tenant_id":"cicd","user_id":"alice"'
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_set "{$UKEY,\"burst_size\":4}")
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'all zero' && pass "ai_user_ratelimit_set refuses an all-zero entry before any request" || fail "all-zero entry: $res"
USET="{$UKEY,\"rps\":3,\"model_limits\":[{\"model\":\"llama3\",\"tokens_per_min\":200}]"
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_set "$USET}")
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["rollback"]["tool"]')" == "ai_user_ratelimit_delete" ]] \
    && pass "ai_user_ratelimit_set on a new user previews with an ai_user_ratelimit_delete rollback" || fail "user set preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_set "$USET,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["rps"]')" == "3" && "$(jfield "$res" 'r["read_back"]["model_limits"][0]["tokens_per_min"]')" == "200" ]] \
    && pass "ai_user_ratelimit_set applied; read_back shows rps 3 and the model quota" || fail "user set apply: $res"
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_get "{$UKEY}")
[[ "$(jfield "$res" 'r["user_ratelimit"]["rps"]')" == "3" ]] && pass "ai_user_ratelimit_get reads the row back" || fail "user get: $res"
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_delete "{$UKEY}")
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["rollback"]["tool"]')" == "ai_user_ratelimit_set" && "$(jfield "$res" 'r["rollback"]["args"]["rps"]')" == "3" \
   && "$(jfield "$res" 'r["rollback"]["args"]["model_limits"][0]["tokens_per_min"]')" == "200" ]] \
    && pass "ai_user_ratelimit_delete previews the row as an ai_user_ratelimit_set rollback" || fail "user delete preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_delete "{$UKEY,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["exists"]')" == "False" ]] \
    && pass "ai_user_ratelimit_delete applied; read_back shows the row gone" || fail "user delete apply: $res"
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_get "{$UKEY}")
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'HTTP 404' && pass "ai_user_ratelimit_get passes the 404 on the removed row" || fail "user get after delete: $res"
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_set "$USET}")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_set "$USET,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["read_back"]["rps"]')" == "3" ]] && pass "the delete's rollback re-creates the row" || fail "user set rollback: $res"
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_delete "{$UKEY}")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$OTOKEN" ai_user_ratelimit_delete "{$UKEY,\"confirm_token\":\"$tok\"}")
echo "$res" | grep -q '"action":"executed"' && pass "user row removed again (cleanup)" || fail "user delete cleanup: $res"

# The mutation budget (change-gate requirement 4, D12): ci-tight has 4
# mutations per minute and the default stop after 3 consecutive failures.
# Three PATCHes on a missing key are sent and refused by the gateway (404);
# the fourth mutation is refused by the bridge before any request; a
# successful read lifts the stop; the next mutation is sent (the fourth of
# the minute); the one after is refused as exceeded.
echo "### mutation budget"
mcp_session "$TTOKEN" && pass "ci-tight session established (4 mutations/min)" || fail "no ci-tight session"
MISSING='{"key_id":"no-such-key","enabled":false,"apply":true}'
for i in 1 2 3; do
    res=$(mcp_call "$TTOKEN" ai_apikey_update "$MISSING")
    echo "$res" | grep -q 'HTTP 404' && pass "failed mutation $i reached the gateway (404)" || fail "mutation $i: $res"
done
res=$(mcp_call "$TTOKEN" ai_apikey_update "$MISSING")
echo "$res" | grep -q 'stopped after 3 consecutive failed mutations' && pass "fourth mutation refused by the bridge: stopped after 3 failures" || fail "no stop after 3 failures: $res"
res=$(mcp_call "$TTOKEN" ai_apikey_list '{"tenant_id":"cicd"}')
echo "$res" | grep -q '"keys"' && pass "a read-only call succeeds while stopped" || fail "read while stopped: $res"
res=$(mcp_call "$TTOKEN" ai_ratelimit_set '{"tenant_id":"cicd","rps":5,"tokens_per_min":1000,"apply":true}')
echo "$res" | grep -q '"action":"executed"' && pass "the read lifted the stop; the next mutation is sent" || fail "mutation after the read: $res"
res=$(mcp_call "$TTOKEN" ai_apikey_update "$MISSING")
echo "$res" | grep -q '4 of 4 mutations per minute' && pass "fifth mutation of the minute refused: budget exceeded" || fail "no window refusal: $res"
grep -q 'mutation budget' .mcp-audit/audit.jsonl && pass "audit.jsonl records the budget refusals" || fail "no budget refusal in audit.jsonl"
SESSION=$ADMIN_SESSION

# config_persist, last: the running configuration to disk. The gateway's
# auto-persist holds the snapshot gate about 3 s after a config write; the
# tool answers snapshot_gate_busy and retries nothing, so the leg waits
# and calls again, as an agent would.
echo "### config persist"
persist_call() {
    local res try
    for try in 1 2 3 4; do
        res=$(mcp_call "$ATOKEN" config_persist "$1")
        echo "$res" | grep -q 'snapshot_gate_busy' || break
        sleep 3
    done
    [[ $try -gt 1 ]] && echo "  [NOTE] config_persist answered snapshot_gate_busy $((try-1))x; retried after retry_after_sec" >&2
    echo "$res"
}
res=$(persist_call '{}')
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["impact"]')" == "instance" && -n "$(jfield "$res" 'r["current"]["checksum"]')" \
   && "$(jfield "$res" 'r["rollback"]["note"]')" == *"not reversible"* ]] \
    && pass "config_persist previews the running snapshot's identity, instance impact, not reversible" || fail "persist preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(persist_call "{\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" && "$(jfield "$res" 'r["result"]["path"]')" == *snapshot.json && -n "$(jfield "$res" 'r["result"]["checksum"]')" && -n "$(jfield "$res" 'r["read_back"]["checksum"]')" ]] \
    && pass "config_persist applied; result names the file and its checksum, read_back re-reads the identity" || fail "persist apply: $res"
grep -q '"tool":"config_persist"' .mcp-audit/audit.jsonl && pass "audit.jsonl records config_persist" || fail "config_persist audit entry missing"

# ---------- 5. RCA / diagnostics ----------

echo "### rca"
res=$(mcp_call "$ATOKEN" diagnose_l4_errors)
# GET /diagnostics is folded into every diagnose tool as evidence.diagnostics.
echo "$res" | grep -q '"diagnostics":{' && pass "diagnose_l4_errors carries the /diagnostics projection" || fail "no diagnostics evidence: $res"
res=$(mcp_call "$ATOKEN" diagnose_l4_errors)
echo "$res" | grep -q '"evidence"' && pass "diagnose_l4_errors returns evidence" || fail "diagnose_l4_errors: $res"
res=$(mcp_call "$ATOKEN" capacity_report)
echo "$res" | grep -q '"evidence"' && pass "capacity_report returns evidence" || fail "capacity_report: $res"
res=$(mcp_call "$ATOKEN" ai_traffic_report)
echo "$res" | grep -q 'SSE-terminated' && pass "ai_traffic_report surfaces the accounting caveat" || fail "accounting caveat missing: $res"
res=$(mcp_rpc "$ATOKEN" prompts/list)
echo "$res" | grep -q 'triage-alert' && pass "prompts registered" || fail "prompts/list: $res"
res=$(mcp_rpc "$ATOKEN" prompts/get '{"name":"triage-alert","arguments":{"alert":"LoxilbAIAdmissionShedding"}}')
echo "$res" | grep -q 'LoxilbAIAdmission\*' && echo "$res" | grep -q 'lb_create replace' && echo "$res" | grep -q 'lb_kvexact_status' \
    && pass "triage-alert maps LoxilbAIAdmission* to an lb_create replace preview and KvExact* to lb_kvexact_status" || fail "triage-alert mappings: $res"

# ---------- 5b. PR-D: the capacity gate, read and changed ----------

# An AI-gateway rule (fullproxy, sse_mode) with a ceiling of one in-flight
# request and a queue of four, in front of cicd/ai-admission/mock_backend.py
# on l3ep1:8090. Four concurrent 600 ms requests: one runs, three park.
# diagnose_ai_latency and ai_traffic_report read the gate from the scrape.
# The gateway does not PATCH a fullproxy rule, so lb_update's fc_* is
# refused by the bridge (D26) and the change is an lb_create replace,
# read back in fc_effective and rolled back the same way (D25).
echo "### capacity gate"
AIV=20.20.20.3; AIP=2097
AIKEY="\"external_ip\":\"$AIV\",\"port\":$AIP,\"protocol\":\"tcp\""
AIRULE="$AIKEY,\"mode\":4,\"sse_mode\":true,\"api_key_auth\":\"disabled\",\"fc_max_outstanding\":1,\"fc_max_queue_depth\":4,\"fc_max_queue_wait_ms\":3000,\"endpoints\":[{\"ip\":\"31.31.31.1\",\"port\":8090}]"
fc_mode_in_force() { jfield "$(mcp_call "$ATOKEN" lb_get "{$AIKEY}")" 'r["rule"]["fc_effective"]["mode"]'; }
# The admission and band families are published on the sockproxy metrics
# cycle (PrometheusDefaultPeriod, 10 s) and /metrics serves that cache, so a
# read at a fixed delay sees no row or a mid-burst snapshot. admission_settle
# polls the raw scrape until the service has an admission row and its
# decisions, queue_wait count and band values are the same on two scrapes
# taken at least 10 s apart; RAW then holds the settled scrape. Past 30 s the
# scenario stops.
rawsum() { grep "^$1{" <<< "$RAW" | grep "service=\"$AIV:$AIP\"" | grep -- "${2:-}" | awk '{s+=$NF} END{printf "%g", s+0}'; }
admission_settle() {
    local start=$SECONDS prev="" prev_t=0 cur
    while true; do
        RAW=$(curl -s --max-time 5 -H "Authorization: Bearer $JWT" "$REST/metrics")
        cur=$(grep -E '^loxilb_ai_(admission_decisions_total|admission_queue_wait_seconds_count|fc_band_total)\{' <<< "$RAW" \
            | grep "service=\"$AIV:$AIP\"" | sort)
        if grep -q '^loxilb_ai_admission_' <<< "$cur"; then
            if [[ -n "$prev" && $((SECONDS - prev_t)) -ge 10 && "$cur" == "$prev" ]]; then
                echo "  [NOTE] admission counters for $AIV:$AIP settled after $((SECONDS - start)) s ($1)"
                return 0
            fi
            prev=$cur; prev_t=$SECONDS
            (( SECONDS - start + 10 > 30 )) && break
            sleep 10
        else
            (( SECONDS - start >= 30 )) && break
            sleep 1
        fi
    done
    fail "admission counters for $AIV:$AIP did not settle within 30 s ($1): last scrape ${cur:-has no row}"
    echo "SCENARIO-mcp [FAILED]"; exit 1
}
$hexec l3ep1 bash -c "nohup python3 ../ai-admission/mock_backend.py 8090 > /tmp/mcp-ai-mock.log 2>&1 &"
for i in $(seq 1 20); do $hexec l3ep1 curl -s -m 1 localhost:8090/health >/dev/null && break; sleep 0.5; done
res=$(mcp_call "$ATOKEN" lb_create "{$AIRULE,\"fc_mode\":\"enforce\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" ]] && pass "AI rule $AIV:$AIP created with fc_mode enforce, ceiling 1, queue 4" || fail "AI rule create: $res"
sleep 3
[[ "$(fc_mode_in_force)" == "enforce" ]] && pass "lb_get reads fc_effective.mode enforce" || fail "fc_effective after create: $(mcp_call "$ATOKEN" lb_get "{$AIKEY}")"
aichat() { $hexec l3h1 curl -s -o /dev/null -w '%{http_code}' -m 10 "http://$AIV:$AIP/v1/chat/completions" \
    -H "Content-Type: application/json" "$@" -d '{"model":"m1","messages":[{"role":"user","content":"hi"}]}'; }
# The listener may take a moment after the create: one request must answer
# 200 before the burst (it is admitted too, so the burst's oracle is >= 4).
for i in $(seq 1 15); do [[ "$(aichat)" == "200" ]] && break; sleep 1; done
# Settled counts before the burst, so the burst's own counts are a delta.
admission_settle "before the burst"
B_ADM=$(rawsum loxilb_ai_admission_decisions_total 'reason="admitted"'); B_QUE=$(rawsum loxilb_ai_admission_decisions_total 'reason="queued"')
B_QW=$(rawsum loxilb_ai_admission_queue_wait_seconds_count)
# wait on the four curls only: a bare wait also waits for tcp_server.js.
CPIDS=(); CODES=$(mktemp)
for i in 1 2 3 4; do
    { aichat -H "X-Test-Delay-Ms: 600"; echo; } >> "$CODES" &
    CPIDS+=($!)
done
wait "${CPIDS[@]}"
[[ "$(grep -c '^200$' "$CODES")" == 4 ]] && pass "four concurrent requests answered 200 through the gate" || fail "burst status codes: $(tr '\n' ' ' < "$CODES")"
rm -f "$CODES"
# The oracle is the bridge's fidelity: what the tools report equals the
# settled raw scrape. The gateway's counts of the burst are printed as notes.
admission_settle "after the burst"
res=$(mcp_call "$ATOKEN" diagnose_ai_latency)
AP="[p for p in r['evidence'].get('admission',[]) if p['service']=='$AIV:$AIP']"
QW=$(jfield "$res" "'%g' % (($AP or [{}])[0].get('queue_wait') or {}).get('count', 0)")
[[ "$(jfield "$res" "($AP or [{}])[0].get('mode')")" == "enforce" \
   && "$(jfield "$res" "($AP or [{}])[0].get('effective_limit') == 1")" == "True" \
   && "$QW" == "$(rawsum loxilb_ai_admission_queue_wait_seconds_count)" ]] \
    && pass "diagnose_ai_latency admission section: enforce, ceiling 1, queue_wait count $QW as the scrape has it" || fail "admission section: $(jfield "$res" "$AP") vs scrape queue_wait count $(rawsum loxilb_ai_admission_queue_wait_seconds_count)"
echo "  [NOTE] gateway counts for the burst of four (settled delta): admitted decisions $(( $(rawsum loxilb_ai_admission_decisions_total 'reason="admitted"') - B_ADM )), queued $(( $(rawsum loxilb_ai_admission_decisions_total 'reason="queued"') - B_QUE )), queue_wait samples $(( QW - B_QW ))"
[[ "$(jfield "$res" "sorted(r['evidence']['pd_admission_sheds'])")" == *"loxilb_pd_admission_overflow_shed_total"* \
   && "$(jfield "$res" "'pending' in r['evidence']['epp']")" == "True" ]] \
    && pass "diagnose_ai_latency carries the P/D admission sheds and the EPP section" || fail "pd/epp sections: $res"
res=$(mcp_call "$ATOKEN" ai_traffic_report)
BA=$(jfield "$res" "'%g' % sum(b['count'] for b in r.get('fc_band') or [] if b['service']=='$AIV:$AIP' and b['outcome']=='admitted')")
BQ=$(jfield "$res" "'%g' % sum(b['count'] for b in r.get('fc_band') or [] if b['service']=='$AIV:$AIP' and b['outcome']=='queued')")
[[ -n "$BA" && "$BA" == "$(rawsum loxilb_ai_fc_band_total 'outcome="admitted"')" && "$BQ" == "$(rawsum loxilb_ai_fc_band_total 'outcome="queued"')" ]] \
    && pass "ai_traffic_report fc_band for the rule: admitted $BA, queued $BQ, as the scrape has them" || fail "fc_band rows: $(jfield "$res" 'r.get("fc_band")') vs scrape admitted $(rawsum loxilb_ai_fc_band_total 'outcome="admitted"') queued $(rawsum loxilb_ai_fc_band_total 'outcome="queued"')"
res=$(mcp_call "$ATOKEN" lb_update "{$AIKEY,\"fc_mode\":\"observe\"}")
echo "$res" | grep -q '"isError":true' && echo "$res" | grep -q 'nothing was sent' && echo "$res" | grep -q 'lb_create on the same key' \
    && pass "lb_update fc_mode on the fullproxy rule is refused by the bridge, naming the replace (D26)" || fail "lb_update fc on fullproxy: $res"
res=$(mcp_call "$ATOKEN" lb_create "{$AIRULE,\"fc_mode\":\"observe\"}")
[[ "$(jfield "$res" 'r["action"]')" == "preview" && "$(jfield "$res" 'r["rollback"]["tool"]')" == "lb_create" \
   && "$(jfield "$res" 'r["rollback"]["args"]["fc_mode"]')" == "enforce" ]] \
    && pass "lb_create on the key previews the replace; the rollback re-creates fc_mode enforce" || fail "replace preview: $res"
tok=$(jfield "$res" 'r["confirm_token"]'); RB=$(jfield "$res" 'json.dumps(r["rollback"]["args"])')
res=$(mcp_call "$ATOKEN" lb_create "{$AIRULE,\"fc_mode\":\"observe\",\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" ]] && pass "replace applied with the token" || fail "replace apply: $res"
sleep 3
admission_settle "after the replace"
[[ "$(fc_mode_in_force)" == "observe" ]] && pass "fc_effective.mode reads observe after the replace (D25 live proof)" || fail "fc_effective after replace: $(mcp_call "$ATOKEN" lb_get "{$AIKEY}")"
res=$(mcp_call "$ATOKEN" lb_create "$RB")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$ATOKEN" lb_create "$(python3 -c 'import json,sys; a=json.loads(sys.argv[1]); a["confirm_token"]=sys.argv[2]; print(json.dumps(a))' "$RB" "$tok")")
[[ "$(jfield "$res" 'r["action"]')" == "executed" ]] && pass "rollback args from the preview applied" || fail "rollback apply: $res"
sleep 3
admission_settle "after the rollback"
[[ "$(fc_mode_in_force)" == "enforce" ]] && pass "fc_effective.mode reads enforce again after the rollback" || fail "fc_effective after rollback: $(mcp_call "$ATOKEN" lb_get "{$AIKEY}")"
res=$(mcp_call "$ATOKEN" lb_delete "{$AIKEY}")
tok=$(jfield "$res" 'r["confirm_token"]')
res=$(mcp_call "$ATOKEN" lb_delete "{$AIKEY,\"confirm_token\":\"$tok\"}")
[[ "$(jfield "$res" 'r["action"]')" == "executed" ]] && pass "AI rule removed" || fail "AI rule delete: $res"
$hexec l3ep1 pkill -f 'mock_backend.py 8090'; true

if [[ $code == 0 ]]; then
    echo SCENARIO-mcp [OK]
else
    echo SCENARIO-mcp [FAILED]
fi
exit $code
