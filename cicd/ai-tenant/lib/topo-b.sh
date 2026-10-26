#!/bin/bash
# lib/topo-b.sh — the extra backends and rules of the group-B cases of
# cicd/ai-tenant (validation-sched.sh, lib/cases-b.sh). Sourced after
# lib/classload.sh; config.sh calls tqb_backends_up and tqb_rules_add after
# its own rules, rmconfig.sh appends tqb_leftovers to its leftover check.
#
# | VIP port | Rule | Backends on tqep1 |
# | 8088 | keyed, P/D (pd_disagg_mode), ceiling 1, queue 4, wait 10 s: P9, B6 | pd_mock prefill :8180 (ep_role 1), decode :8181 (ep_role 2) |
# | 8089 | stateless MCP, keyless, ceiling 1, queue 4, wait 20 s: B3a | MCP fixture :8201 (SSE, progress 1 s) |
# | 8093 | stateless MCP with the AI gateway (api_key_auth), ceiling 1, queue 4: B3b | the same fixture :8201 |
# | 8094 | keyed, CHWBL (sel 8), ceiling 8, endpoint ceiling 1, queue 16: B5 | mock_backend :8080, :8081 |
# | 8095 | keyed, round robin, ceiling 1, queue 4: X4 (one endpoint has nothing listening) | :8080 and the closed :8099 |
# | 8096 | keyed, ceiling 1, queue 4, only the closed :8099: X4 total failure | none listening |
# | 8097 | keyed, h2c, ceiling 2: H3 (one live, one closed endpoint) | h2c :9090 and the closed :9099 |
# | 8098 | keyed, e2e HTTPS (security 2) verifying the backend against tq-x5-ca, ceiling 1, queue 4: X5 | tls_backend :8444, :8445 (certs from a CA the rule does not trust), :8443 (trusted) |
# | 8100 | the same policy, only :8444: X5 total failure | |
#
# X5 material (tqb_pki, before tqb_rules_add): ./cert/x5/ holds CA-A (trusted by
# the rule), CA-B (not), a server certificate for 10.71.31.1 from each, and the
# listener certificate the clients of :8098/:8100 see (cert/server.crt and
# cert/<VIP>/server.crt: ./cert is the gateway's /opt/loxilb/cert).
#
# Ports 8099 and 9099 on tqep1 must stay closed: nothing in this scenario
# listens there.

PORT_PD=8088
PORT_MCP=8089
PORT_MCPK=8093
PORT_CHWBL=8094
PORT_X4=8095
PORT_X4DEAD=8096
PORT_H2X=8097
PORT_X5=8098
PORT_X5DEAD=8100
X5DIR=$(pwd)/cert/x5
MCP_JOURNAL=/tmp/ai-tenant-mcp-fixture.jsonl
TQB_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

tqb_backends_up() {
  local p i
  $hexec tqep1 python3 "$TQB_DIR/pd_mock.py" --role prefill --port 8180 > /tmp/ai-tenant-pd-prefill.log 2>&1 &
  track_helper
  $hexec tqep1 python3 "$TQB_DIR/pd_mock.py" --role decode --port 8181 > /tmp/ai-tenant-pd-decode.log 2>&1 &
  track_helper
  # The fixture runs as root (hexec): its journal is root's.
  sudo -n rm -f "$MCP_JOURNAL"
  $hexec tqep1 python3 -B "$TQB_DIR/../../mcp-stateless/fixture.py" --bind 0.0.0.0 --port 8201 \
    --backend-id tq-mcp --journal "$MCP_JOURNAL" --response-mode sse --progress-delay 1.0 \
    > /tmp/ai-tenant-mcp-fixture.log 2>&1 &
  track_helper
  for p in 8180 8181; do
    for i in $(seq 1 20); do
      $hexec tqep1 curl -sf --max-time 1 "http://127.0.0.1:$p/health" >/dev/null 2>&1 && break
      sleep 1
    done
    $hexec tqep1 curl -sf --max-time 1 "http://127.0.0.1:$p/health" >/dev/null 2>&1 || {
      echo "FATAL: P/D mock :$p did not become ready"; return 1; }
  done
  for i in $(seq 1 20); do
    $hexec tqep1 bash -c 'exec 3<>/dev/tcp/127.0.0.1/8201' >/dev/null 2>&1 && break
    sleep 1
  done
  $hexec tqep1 bash -c 'exec 3<>/dev/tcp/127.0.0.1/8201' >/dev/null 2>&1 || {
    echo "FATAL: MCP fixture :8201 did not listen"; return 1; }
  # X5: one trusted and two untrusted HTTPS backends, admin on 95xx.
  for p in 8443:good 8444:bad 8445:bad; do
    $hexec tqep1 python3 "$TQB_DIR/tls_backend.py" --port ${p%%:*} --admin-port $((${p%%:*} + 1100)) \
      --cert "$X5DIR/${p##*:}.crt" --key "$X5DIR/${p##*:}.key" > /tmp/ai-tenant-tls-${p%%:*}.log 2>&1 &
    track_helper
  done
  for p in 9543 9544 9545; do
    for i in $(seq 1 20); do
      $hexec tqep1 curl -sf --max-time 1 "http://127.0.0.1:$p/__handshakes" >/dev/null 2>&1 && break
      sleep 1
    done
    $hexec tqep1 curl -sf --max-time 1 "http://127.0.0.1:$p/__handshakes" >/dev/null 2>&1 || {
      echo "FATAL: TLS backend admin :$p did not become ready"; return 1; }
  done
  for p in 8099 9099; do
    $hexec tqep1 bash -c "exec 3<>/dev/tcp/127.0.0.1/$p" >/dev/null 2>&1 && {
      echo "FATAL: :$p on tqep1 must be closed for X4/H3"; return 1; }
  done
  return 0
}

# The X5 certificates: two CAs, a server certificate for the endpoint
# address from each, and the listener certificate. openssl on the bed host.
tqb_pki() {
  local d=$X5DIR
  rm -rf "$d"; mkdir -p "$d" "cert/$VIP" || return 1
  printf 'subjectAltName=IP:%s\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$EP" > "$d/ep.ext"
  printf 'subjectAltName=IP:%s\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$VIP" > "$d/vip.ext"
  for ca in a b; do
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=tenantq-x5-ca-$ca" \
      -keyout "$d/ca-$ca.key" -out "$d/ca-$ca.crt" >/dev/null 2>&1 || return 1
  done
  tqb_leaf() { # <name> <ca> <ext>
    openssl req -newkey rsa:2048 -nodes -subj "/CN=tenantq-x5-$1" -keyout "$d/$1.key" -out "$d/$1.csr" >/dev/null 2>&1 &&
    openssl x509 -req -in "$d/$1.csr" -CA "$d/ca-$2.crt" -CAkey "$d/ca-$2.key" -CAcreateserial \
      -days 2 -extfile "$d/$3" -out "$d/$1.crt" >/dev/null 2>&1
  }
  tqb_leaf good a ep.ext && tqb_leaf bad b ep.ext && tqb_leaf listener a vip.ext || return 1
  openssl verify -CAfile "$d/ca-a.crt" "$d/good.crt" >/dev/null 2>&1 || { echo "FATAL: x5 good cert does not verify"; return 1; }
  openssl verify -CAfile "$d/ca-a.crt" "$d/bad.crt" >/dev/null 2>&1 && { echo "FATAL: x5 bad cert verifies against CA-A"; return 1; }
  cp "$d/listener.crt" cert/server.crt; cp "$d/listener.key" cert/server.key
  cp "$d/listener.crt" "cert/$VIP/server.crt"; cp "$d/listener.key" "cert/$VIP/server.key"
  return 0
}

# A rule whose JSON is given whole (tq_rule builds only the plain shape).
tqb_post() {
  local name=$1 json=$2 r
  r=$(tq_api POST /config/loadbalancer "$json")
  echo "  rule $name: $(echo "$r" | cut -c1-120)"
  case "$r" in 200*) return 0 ;; *) return 1 ;; esac
}

# tqb_pd_json [<extra-json>]: the keyed P/D rule on PORT_PD.
tqb_pd_json() {
  echo "{\"serviceArguments\": {\"externalIP\": \"$VIP\", \"port\": $PORT_PD, \"protocol\": \"tcp\", \"sel\": 0, \"mode\": 4, \"host\": \"$VIP\", \"path_prefix\": \"/\", \"path_match_mode\": \"prefix\", \"model_name\": \"$MODEL\", \"sse_mode\": true, \"pd_disagg_mode\": true, \"inactiveTimeOut\": 60, \"api_key_auth\": \"required\", \"fc_max_outstanding\": 1, \"fc_max_queue_depth\": 4, \"fc_max_queue_wait_ms\": 10000${1:-}}, \"endpoints\": [{\"endpointIP\": \"$EP\", \"targetPort\": 8180, \"weight\": 1, \"ep_role\": 1}, {\"endpointIP\": \"$EP\", \"targetPort\": 8181, \"weight\": 1, \"ep_role\": 2}]}"
}

# The stateless MCP rules: no model, no host or path match (the
# mcp-stateless scenario's own shape); 8093 adds the AI gateway's key check.
tqb_mcp_json() {
  local port=$1 extra=$2
  echo "{\"serviceArguments\": {\"externalIP\": \"$VIP\", \"port\": $port, \"protocol\": \"tcp\", \"mode\": 4, \"sel\": 0, \"security\": 0, \"mcp_protocol_version\": \"2026-07-28\", \"inactiveTimeOut\": 60, \"fc_max_outstanding\": 1, \"fc_max_queue_depth\": 4, \"fc_max_queue_wait_ms\": 20000$extra}, \"endpoints\": [{\"endpointIP\": \"$EP\", \"targetPort\": 8201, \"weight\": 1}]}"
}

tqb_rules_add() {
  local r
  tqb_post ":$PORT_PD (P/D)" "$(tqb_pd_json)" || return 1
  tqb_post ":$PORT_MCP (stateless MCP)" "$(tqb_mcp_json $PORT_MCP '')" || return 1
  # Not fatal: whether a stateless MCP pool takes the key check is what B3b
  # finds out; the case reads the refusal and fails on it.
  tqb_post ":$PORT_MCPK (stateless MCP + api_key_auth)" "$(tqb_mcp_json $PORT_MCPK ', "api_key_auth": "required", "sse_mode": true')" || \
    echo "  rule :$PORT_MCPK refused (B3b will fail on it)"
  tq_rule $PORT_CHWBL ", \"api_key_auth\": \"required\", \"sel\": 8, \"fc_max_outstanding\": 8, \"fc_ep_max_inflight\": 1, \"fc_max_queue_depth\": 16, \"fc_max_queue_wait_ms\": 30000" 8080 8081 || return 1
  tq_rule $PORT_X4 ", \"api_key_auth\": \"required\", \"fc_max_outstanding\": 1, \"fc_max_queue_depth\": 4, \"fc_max_queue_wait_ms\": 20000" 8080 8099 || return 1
  tq_rule $PORT_X4DEAD ", \"api_key_auth\": \"required\", \"fc_max_outstanding\": 1, \"fc_max_queue_depth\": 4, \"fc_max_queue_wait_ms\": 20000" 8099 || return 1
  tq_rule $PORT_H2X ", \"api_key_auth\": \"required\", \"backend_protocol\": \"http2\", \"fc_max_outstanding\": 2" 9090 9099 || return 1
  # X5: the CA as a registry entry, then the verifying rules.
  r=$(tq_api POST /config/cert "$(jq -nc --arg c "$(cat "$X5DIR/ca-a.crt")" '{certId:"tq-x5-ca",usage:"ca",certPem:$c,keyPem:""}')")
  case "$r" in 20*) ;; *) echo "FATAL: x5 CA upload: $(echo "$r" | cut -c1-160)"; return 1 ;; esac
  local X5ARGS=", \"security\": 2, \"mtls_backend\": {\"verify_server_cert\": true}, \"backend_ca_cert_id\": \"tq-x5-ca\", \"api_key_auth\": \"required\", \"fc_max_outstanding\": 1, \"fc_max_queue_depth\": 4, \"fc_max_queue_wait_ms\": 20000"
  tq_rule $PORT_X5 "$X5ARGS" 8444 8445 8443 || return 1
  tq_rule $PORT_X5DEAD "$X5ARGS" 8444 || return 1
  # Keys without allowed_models for the MCP pool (an MCP body names no
  # model); keys-b.env holds KEYB_<tenant>.
  local t name key
  : > keys-b.env
  for t in t-crit t-a t-shed; do
    r=$(tq_api POST /config/ai/apikey "{\"tenant_id\":\"$t\",\"name\":\"$t-mcp\",\"rate_limit_rps\":10000,\"burst_size\":10000,\"enabled\":true}")
    key=$(echo "$r" | sed 's/^[0-9]* //' | jq -r '.raw_key // empty' 2>/dev/null)
    [ -n "$key" ] || { echo "FATAL: MCP key for $t: $r"; return 1; }
    echo "KEYB_${t//-/_}=$key" >> keys-b.env
  done
  return 0
}

# Appended to rmconfig.sh's leftover list.
tqb_leftovers() {
  pgrep -f 'ai-tenant/lib/pd_mock\.py' >/dev/null 2>&1 && echo -n " process:pd_mock"
  pgrep -f 'mcp-stateless/fixture\.py .*--backend-id tq-mcp' >/dev/null 2>&1 && echo -n " process:mcp_fixture"
  pgrep -f 'ai-tenant/lib/tls_backend\.py' >/dev/null 2>&1 && echo -n " process:tls_backend"
  rm -rf "$X5DIR" cert/server.crt cert/server.key "cert/$VIP"
  rm -f keys-b.env; sudo -n rm -f "$MCP_JOURNAL"
}
