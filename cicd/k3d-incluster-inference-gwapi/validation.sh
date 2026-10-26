#!/bin/bash
# k3d-incluster-inference-gwapi — assertions.
# Ported from kube-loxilb's test/e2e-inference-gwapi validation (same checks,
# same reasons) onto the in-cluster topology: loxilb is a Pod, the gateway
# address is the node IP, and the client is a container on the k3d network.
# Checks 5 and 6 follow kube-loxilb feat/auth-annotation (EPP integration): a
# pool that names an Endpoint Picker is accepted and the picker is the gateway's
# to call; the pre-EPP controller refused it (NotSupportedByParent).
# Sentinel: SCENARIO-k3d-incluster-inference-gwapi [OK] / [FAILED].
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
[ -f "$HERE/.env" ] || { echo "FATAL: $HERE/.env missing - run ./config.sh first"; exit 1; }
. "$HERE/.env"; export KUBECONFIG

# Every assertion below extracts JSON with `jq`. An absent jq prints nothing,
# so each read comes back empty - indistinguishable from the cluster returning
# an empty field, and the scenario would invent defects it never measured.
if ! command -v jq >/dev/null 2>&1; then
  echo "FATAL: jq is not installed on this host, and every assertion here needs"
  echo "       it to read a response field. Install it:"
  echo "       sudo apt-get update && sudo apt-get install -y jq"
  echo "SCENARIO-k3d-incluster-inference-gwapi [FAILED]"
  exit 1
fi
VIP="$NODE_IP"
CLIENT="igw-client-$CLUSTER"
GW_PORT="${GW_PORT:-8080}"
POOL_SVC="vllm-qwen3-inference"

fails=0
ok()   { echo "  [OK]   $1"; }
bad()  { echo "  [FAIL] $1${2:+ - $2}"; fails=$((fails+1)); }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "got '$2', want '$3'"; fi; }
poll() { local t=$1; shift; for ((i=0;i<t;i++)); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }

rules() { curl -s --max-time 5 "http://$NODE_IP:11111/netlox/v1/config/loadbalancer/all"; }
svc_json() { kubectl -n llm get svc "$1" -o json 2>/dev/null; }
pool_cond() { # <pool> <conditionType> -> status
  kubectl -n llm get inferencepool "$1" -o json 2>/dev/null |
    jq -r --arg t "$2" '.status.parents[]? | select(.controllerName=="loxilb.io/kube-loxilb")
                        | .conditions[]? | select(.type==$t) | .status' | head -1
}
pool_reason() {
  kubectl -n llm get inferencepool "$1" -o json 2>/dev/null |
    jq -r '.status.parents[]? | select(.controllerName=="loxilb.io/kube-loxilb")
           | .conditions[]? | select(.type=="Accepted") | .reason' | head -1
}
pool_cond_reason() { # <pool> <conditionType> -> reason
  kubectl -n llm get inferencepool "$1" -o json 2>/dev/null |
    jq -r --arg t "$2" '.status.parents[]? | select(.controllerName=="loxilb.io/kube-loxilb")
                        | .conditions[]? | select(.type==$t) | .reason' | head -1
}
epp_rules() { rules | jq '[.lbAttr[]? | select(.serviceArguments.name|startswith("llm_epp-required-inference"))] | length'; }
gateway_has_epp() { curl -s --max-time 5 "http://$NODE_IP:11111/netlox/v1/version" | jq -e '.features[]? | select(.=="epp")' >/dev/null 2>&1; }

echo "=== 1. the pool became a Service ==="
if ! poll 90 kubectl -n llm get svc "$POOL_SVC"; then
  bad "service $POOL_SVC created" "not found after 90s"
else
  ok "service $POOL_SVC created"
  SVC=$(svc_json "$POOL_SVC")
  check "annotation lbmode"        "$(jq -r '.metadata.annotations["loxilb.io/lbmode"]' <<<"$SVC")"        "fullproxy"
  check "annotation usepodnetwork" "$(jq -r '.metadata.annotations["loxilb.io/usepodnetwork"]' <<<"$SVC")" "yes"
  check "annotation epselect"      "$(jq -r '.metadata.annotations["loxilb.io/epselect"]' <<<"$SVC")"      "chwbl"
  check "annotation model-name"    "$(jq -r '.metadata.annotations["loxilb.io/model-name"]' <<<"$SVC")"    "qwen3-32b"
  check "selector from the pool"   "$(jq -r '.spec.selector.app' <<<"$SVC")"                               "vllm-qwen3"
  check "listener port"            "$(jq -r '.spec.ports[0].port' <<<"$SVC")"                              "$GW_PORT"
  check "pool targetPort"          "$(jq -r '.spec.ports[0].targetPort' <<<"$SVC")"                        "8000"
fi

echo "=== 2. the Service was given the gateway address ==="
if poll 90 bash -c "kubectl -n llm get svc $POOL_SVC -o json | jq -e '.status.loadBalancer.ingress[0]' >/dev/null"; then
  check "external IP" "$(svc_json "$POOL_SVC" | jq -r '.status.loadBalancer.ingress[0].hostname // .status.loadBalancer.ingress[0].ip')" "llb-$VIP"
else
  bad "external IP assigned" "still pending after 90s"
fi

echo "=== 3. loxilb programmed an inference rule ==="
RULE_NAME="llm_$POOL_SVC"
if ! poll 60 bash -c "curl -s --max-time 5 http://$NODE_IP:11111/netlox/v1/config/loadbalancer/all | jq -e --arg n '$RULE_NAME' '.lbAttr[]? | select(.serviceArguments.name|startswith(\$n))' >/dev/null"; then
  bad "rule $RULE_NAME present" "not found after 60s"
  rules | jq -c '.lbAttr[]?.serviceArguments | {name,externalIP,port}' 2>/dev/null | head -5
else
  ok "rule $RULE_NAME present"
  RULE=$(rules | jq -c --arg n "$RULE_NAME" '.lbAttr[] | select(.serviceArguments.name|startswith($n))' | head -1)
  check "rule address is the gateway's" "$(jq -r '.serviceArguments.externalIP' <<<"$RULE")" "$VIP"
  # 8 = CHWBL, 4 = fullproxy: gateway-only values, so their presence proves the
  # flavor was detected and the AI fields were not stripped.
  check "sel is chwbl(8)"      "$(jq -r '.serviceArguments.sel' <<<"$RULE")"  "8"
  check "mode is fullproxy(4)" "$(jq -r '.serviceArguments.mode' <<<"$RULE")" "4"
  check "model_name on the rule" "$(jq -r '.serviceArguments.model_name' <<<"$RULE")" "qwen3-32b"
  EPS=$(jq -r '[.endpoints[]?.endpointIP] | sort | join(",")' <<<"$RULE")
  PODS=$(kubectl -n llm get pods -l app=vllm-qwen3 -o json | jq -r '[.items[].status.podIP] | sort | join(",")')
  check "endpoints are the pool's pods" "$EPS" "$PODS"
  # Without the lookup key the rule reads back fine and answers every request
  # with model_unavailable.
  check "routing key host"       "$(jq -r '.serviceArguments.host' <<<"$RULE")"            "$VIP"
  check "routing key path"       "$(jq -r '.serviceArguments.path_prefix' <<<"$RULE")"     "/"
  check "routing key match mode" "$(jq -r '.serviceArguments.path_match_mode' <<<"$RULE")" "prefix"
fi

echo "=== 3b. the pool owns the listener alone ==="
COUNT=$(rules | jq --arg ip "$VIP" '[.lbAttr[]? | select(.serviceArguments.externalIP==$ip and .serviceArguments.port==('"$GW_PORT"'))] | length')
check "exactly one rule on $VIP:$GW_PORT" "$COUNT" "1"
if kubectl -n llm get svc inference-gw-ingress-service >/dev/null 2>&1; then
  bad "no ingress service on the claimed listener" "inference-gw-ingress-service still exists"
else
  ok "no ingress service on the claimed listener"
fi

echo "=== 3c. fullproxy is listening on the VIP ==="
vip_hex=$(printf '%02X%02X%02X%02X:%04X' $(echo "$VIP" | awk -F. '{print $4,$3,$2,$1}') "$GW_PORT")
if docker exec "$NODE" cat /proc/net/tcp | awk 'NR>1 && $4=="0A"{print $2}' | grep -qi "^$vip_hex$"; then
  ok "socket bound on $VIP:$GW_PORT"
else
  bad "socket bound on $VIP:$GW_PORT"
fi

echo "=== 4. the pool reports its status ==="
if poll 60 bash -c "[[ \"\$(kubectl -n llm get inferencepool vllm-qwen3 -o json | jq -r '.status.parents | length')\" != '0' ]]"; then
  check "Accepted"     "$(pool_cond vllm-qwen3 Accepted)"     "True"
  check "ResolvedRefs" "$(pool_cond vllm-qwen3 ResolvedRefs)" "True"
  check "parent is the gateway" \
    "$(kubectl -n llm get inferencepool vllm-qwen3 -o json | jq -r '.status.parents[0].parentRef.name')" "inference-gw"
else
  bad "status written" "no parents after 60s"
fi

echo "=== 5. an Endpoint Picker with FailClose is accepted; calling it is the gateway's job ==="
# kube-loxilb (feat/auth-annotation, with the EPP integration) no longer refuses
# a pool that names an Endpoint Picker: it accepts the pool, writes the picker's
# address and failure mode on the derived Service, and leaves the call to the
# gateway. The picker Service "some-epp" does not exist here, so the reference
# does not resolve: ResolvedRefs says so, and FailClose points the rule at an
# address that always refuses, so every request is answered 503 until the picker
# appears - never served by a policy nobody asked for. Whether a rule reaches
# loxilb depends on the gateway: one that lists "epp" in its /version features
# installs it (eppFailureMode FailClose); one that does not gets no rule for the
# pool, because FailClose cannot be served without the picker.
kubectl apply -f "$HERE/fixtures/inferencepool-failclose.yaml" >/dev/null
kubectl apply -f "$HERE/fixtures/httproute-failclose.yaml" >/dev/null
if poll 60 bash -c "[[ \"\$(kubectl -n llm get inferencepool epp-required -o json | jq -r '.status.parents | length')\" != '0' ]]"; then
  check "Accepted is True"      "$(pool_cond epp-required Accepted)"                  "True"
  check "ResolvedRefs is False" "$(pool_cond epp-required ResolvedRefs)"              "False"
  check "ResolvedRefs reason"   "$(pool_cond_reason epp-required ResolvedRefs)"       "InvalidExtensionRef"
else
  bad "accepted pool reports status" "no parents after 60s"
fi
if poll 60 kubectl -n llm get svc epp-required-inference; then
  ok "service for the accepted pool"
  EPP_SVC=$(svc_json epp-required-inference)
  check "annotation epp-failure-mode" "$(jq -r '.metadata.annotations["loxilb.io/epp-failure-mode"]' <<<"$EPP_SVC")" "FailClose"
  if [[ -n "$(jq -r '.metadata.annotations["loxilb.io/epp-endpoint"] // ""' <<<"$EPP_SVC")" ]]; then
    ok "annotation epp-endpoint set (an unresolved FailClose picker is pointed at a refusing address)"
  else
    bad "annotation epp-endpoint" "absent on a FailClose pool whose picker does not resolve"
  fi
else
  bad "service for the accepted pool" "epp-required-inference not found after 60s"
fi
# The gateway side. The fixture shares the listener with vllm-qwen3, so on one
# VIP kube-loxilb's IPAM also keeps this Service without an address; the rule
# table is the observation that holds either way.
if gateway_has_epp; then
  if poll 60 bash -c "[ \"\$(curl -s --max-time 5 http://$NODE_IP:11111/netlox/v1/config/loadbalancer/all | jq '[.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))] | length')\" != 0 ]"; then
    check "rule carries eppFailureMode FailClose (gateway with epp)" \
      "$(rules | jq -r '[.lbAttr[]? | select(.serviceArguments.name|startswith("llm_epp-required-inference"))][0].serviceArguments.eppFailureMode')" "FailClose"
  else
    bad "rule for the FailClose pool (gateway with epp)" "not found after 60s"
  fi
else
  sleep 10
  check "no rule for the FailClose pool on a gateway without epp" "$(epp_rules)" "0"
fi

echo "=== 6. switching that pool to FailOpen keeps it accepted; loxilb selects itself ==="
# An unresolved FailOpen picker carries nothing: the picker annotations leave
# the Service and the rule, where the gateway installs one, falls back to
# loxilb's own endpoint selection - the pool's declared fallback.
kubectl apply -f "$HERE/fixtures/inferencepool-failopen.yaml" >/dev/null
if poll 60 bash -c "[[ \"\$(kubectl -n llm get svc epp-required-inference -o json | jq -r '.metadata.annotations[\"loxilb.io/epp-endpoint\"] // \"\"')\" == '' ]]"; then
  ok "epp-endpoint leaves the Service under FailOpen"
else
  bad "epp-endpoint leaves the Service under FailOpen" "still $(svc_json epp-required-inference | jq -r '.metadata.annotations["loxilb.io/epp-endpoint"]') after 60s"
fi
check "epp-failure-mode leaves the Service under FailOpen" "$(svc_json epp-required-inference | jq -r '.metadata.annotations["loxilb.io/epp-failure-mode"] // ""')" ""
check "Accepted stays True" "$(pool_cond epp-required Accepted)" "True"

echo "=== 7. removing the route removes the Service (and its rule) ==="
kubectl -n llm delete httproute epp-route >/dev/null 2>&1
if poll 90 bash -c "! kubectl -n llm get svc epp-required-inference >/dev/null 2>&1"; then
  ok "service removed with its route"
else
  bad "service removed with its route" "still present after 90s"
fi
# The L7-keyed delete must reach loxilb too - a leaked rule keeps the socket.
if poll 60 bash -c "[ \"\$(curl -s http://$NODE_IP:11111/netlox/v1/config/loadbalancer/all | jq '[.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))] | length')\" = 0 ]"; then
  ok "no leaked rule for the removed pool service"
else
  bad "no leaked rule for the removed pool service" "$(rules | jq -c '[.lbAttr[]?.serviceArguments.name]')"
fi

echo "=== 8. traffic reaches a model server through the VIP ==="
# The mock stamps X-Served-By with its pod name: the proof that says which
# endpoint served, which a 200 alone cannot.
BODY='{"model":"qwen3-32b","messages":[{"role":"user","content":"ping"}]}'
PODS=$(kubectl -n llm get pods -l app=vllm-qwen3 -o jsonpath='{.items[*].metadata.name}')
served=""
for attempt in 1 2 3 4 5; do
  served=$(docker exec "$CLIENT" curl -s -i --max-time 8 -X POST "http://$VIP:$GW_PORT/v1/chat/completions" \
             -H 'Content-Type: application/json' -d "$BODY" 2>/dev/null |
           tr -d '\r' | grep -i '^x-served-by:' | awk '{print $2}' | head -1)
  grep -qw "${served:-__none__}" <<<"$PODS" && break
  served=""; sleep 5
done
if [[ -n "$served" ]]; then
  ok "served by pod $served"
else
  bad "traffic reaches a model server" "no X-Served-By from any pool pod"
fi

OTHER=$(docker exec "$CLIENT" curl -s --max-time 8 -w '\n%{http_code}' -X POST "http://$VIP:$GW_PORT/v1/chat/completions" \
          -H 'Content-Type: application/json' -d '{"model":"not-this-one","messages":[]}' 2>/dev/null)
check "an unknown model is refused (503)" "$(tail -1 <<<"$OTHER")" "503"

SSE=$(docker exec "$CLIENT" curl -s -N --max-time 15 -X POST "http://$VIP:$GW_PORT/v1/chat/completions" \
        -H 'Content-Type: application/json' -H 'Accept: text/event-stream' \
        -d '{"model":"qwen3-32b","messages":[{"role":"user","content":"count"}],"stream":true}' 2>/dev/null)
if grep -q '^data:' <<<"$SSE" && grep -q 'data: \[DONE\]' <<<"$SSE"; then
  ok "SSE stream delivers data: events and [DONE]"
else
  bad "SSE stream through the gateway" "$(head -c 120 <<<"$SSE")"
fi

echo
if [[ $fails -eq 0 ]]; then
  echo "SCENARIO-k3d-incluster-inference-gwapi [OK]"
  exit 0
fi
echo "SCENARIO-k3d-incluster-inference-gwapi [FAILED] ($fails check(s))"
exit 1
