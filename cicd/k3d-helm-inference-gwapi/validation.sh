#!/bin/bash
# k3d-helm-inference-gwapi — assertions.
# Three layers. First `helm test` runs the chart's own test pods (the management
# REST is the inference-gateway flavor; InferencePool -> Service -> loxilb rule).
# Then ../k3d-incluster-inference-gwapi/validation.sh is sourced unchanged
# against the helm-installed stack: the same assertions passing means the chart
# is equivalent to the testbed manifests. Last, the release's lifecycle: an
# in-place upgrade, an uninstall that leaves nothing but the CRDs, and a
# reinstall into the same cluster.
# Sentinel: SCENARIO-k3d-helm-inference-gwapi [OK] / [FAILED].
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
[ -f "$HERE/.env" ] || { echo "FATAL: $HERE/.env missing - run ./config.sh first"; exit 1; }
. "$HERE/.env"; export KUBECONFIG
rc=0

echo "=== helm test $HELM_RELEASE ==="
if helm test "$HELM_RELEASE" -n "$HELM_NAMESPACE" --logs --timeout 10m; then
  echo "  [OK]   helm test"
else
  echo "  [FAIL] helm test"; rc=1
fi

echo
echo "=== k3d-incluster-inference-gwapi assertions against the helm release ==="
# Sourced in a subshell because it ends with exit. Its HERE comes from $0, i.e.
# this script, so it reads this directory's .env and fixtures/ (a link to the
# original fixtures: checks 5-7 apply a FailClose/FailOpen pool by hand).
( . "$HERE/../k3d-incluster-inference-gwapi/validation.sh" ) || rc=1

echo
echo "=== lifecycle: helm upgrade, uninstall, reinstall ==="
# What a release has to survive beyond its first install: an in-place upgrade
# (revision 2, the pre-upgrade hook runs again, nothing restarts, the rule and
# the traffic stay), a clean uninstall (every release resource gone, the Service
# kube-loxilb derived from the pool gone with it, the CRDs kept as Helm's crds/
# rule says), and a second install into the same cluster (CRDs already there,
# the rule and the traffic come back).
lfails=0
lok()   { echo "  [OK]   $1"; }
lbad()  { echo "  [FAIL] $1${2:+ - $2}"; lfails=$((lfails+1)); }
lcheck() { if [[ "$2" == "$3" ]]; then lok "$1"; else lbad "$1" "got '$2', want '$3'"; fi; }
lpoll() { local t=$1; shift; local i; for ((i=0;i<t;i++)); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
rule_count() { curl -s --max-time 5 "http://$NODE_IP:11111/netlox/v1/config/loadbalancer/all" | jq '[.lbAttr[]? | select(.serviceArguments.name|startswith("llm_vllm-qwen3-inference"))] | length'; }
served_by() { # one request through the VIP; prints the pod that answered, or nothing
  docker exec "igw-client-$CLUSTER" curl -s -i --max-time 8 -X POST "http://$NODE_IP:${GW_PORT:-8080}/v1/chat/completions" \
    -H 'Content-Type: application/json' -d '{"model":"qwen3-32b","messages":[{"role":"user","content":"ping"}]}' 2>/dev/null |
    tr -d '\r' | grep -i '^x-served-by:' | awk '{print $2}' | head -1
}
POOL_PODS() { kubectl -n llm get pods -l app=vllm-qwen3 -o jsonpath='{.items[*].metadata.name}'; }
# shellcheck disable=SC2086  # HELM_SET_ARGS is a list of space-free words

echo "--- upgrade in place (a GatewayClass annotation; no pod restarts) ---"
if helm upgrade "$HELM_RELEASE" "$CHART" -n "$HELM_NAMESPACE" -f "$CHART/values-k3d-dev.yaml" $HELM_SET_ARGS \
     --set 'inference.gatewayClass.annotations.loxilb\.io/lifecycle=upgraded' --wait --timeout 5m >/dev/null; then
  lok "helm upgrade returns success (pre-upgrade hook included)"
  lcheck "revision" "$(helm list -n "$HELM_NAMESPACE" -o json | jq -r --arg r "$HELM_RELEASE" '.[] | select(.name==$r) | .revision')" "2"
  lcheck "status"   "$(helm list -n "$HELM_NAMESPACE" -o json | jq -r --arg r "$HELM_RELEASE" '.[] | select(.name==$r) | .status')" "deployed"
  lcheck "the upgrade reached the GatewayClass" "$(kubectl get gatewayclass loxilb-gc -o jsonpath='{.metadata.annotations.loxilb\.io/lifecycle}')" "upgraded"
  lcheck "loxilb pod not restarted" "$(kubectl -n "$HELM_NAMESPACE" get pods -l app.kubernetes.io/name=loxilb-igw -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}')" "0"
  lcheck "rule still on the gateway" "$(rule_count)" "1"
  served=$(served_by)
  if [[ -n "$served" ]] && grep -qw "$served" <<<"$(POOL_PODS)"; then lok "traffic still served (by $served)"; else lbad "traffic still served" "no X-Served-By from a pool pod"; fi
else
  lbad "helm upgrade" "returned an error"
fi

echo "--- uninstall ---"
if helm uninstall "$HELM_RELEASE" -n "$HELM_NAMESPACE" --timeout 5m >/dev/null; then
  lok "helm uninstall returns success"
else
  lbad "helm uninstall" "returned an error"
fi
if helm status "$HELM_RELEASE" -n "$HELM_NAMESPACE" >/dev/null 2>&1; then lbad "release gone" "helm status still finds it"; else lok "release gone"; fi
if lpoll 60 bash -c "[ \"\$(kubectl -n $HELM_NAMESPACE get ds,deploy,svc,sa,pod -l app.kubernetes.io/instance=$HELM_RELEASE --no-headers 2>/dev/null | wc -l)\" = 0 ]"; then
  lok "no release-labelled workload, Service, ServiceAccount or pod left in $HELM_NAMESPACE"
else
  lbad "release resources removed" "$(kubectl -n "$HELM_NAMESPACE" get ds,deploy,svc,sa,pod -l app.kubernetes.io/instance="$HELM_RELEASE" --no-headers 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
fi
if lpoll 60 bash -c "! kubectl get gatewayclass loxilb-gc >/dev/null 2>&1 && ! kubectl -n llm get gateway inference-gw >/dev/null 2>&1 && ! kubectl -n llm get httproute llm-route >/dev/null 2>&1 && ! kubectl -n llm get inferencepool vllm-qwen3 >/dev/null 2>&1"; then
  lok "GatewayClass, Gateway, HTTPRoute and InferencePool removed"
else
  lbad "gateway objects removed" "$(kubectl get gatewayclass loxilb-gc -o name 2>/dev/null) $(kubectl -n llm get gateway,httproute,inferencepool -o name 2>/dev/null | tr '\n' ' ')"
fi
# The Service kube-loxilb derived from the pool is not in the release; it goes
# when kube-loxilb reconciles the pool's deletion. Helm deletes the pool before
# the kube-loxilb Deployment, so kube-loxilb has to get there first.
if lpoll 90 bash -c "! kubectl -n llm get svc vllm-qwen3-inference >/dev/null 2>&1"; then
  lok "the Service kube-loxilb derived from the pool is gone"
else
  lbad "derived Service removed" "llm/vllm-qwen3-inference still exists 90s after uninstall"
fi
lcheck "Gateway API, Inference Extension and loxilb CRDs stay (Helm never deletes crds/)" \
  "$(kubectl get crd gateways.gateway.networking.k8s.io inferencepools.inference.networking.k8s.io jwtauthprofiles.jwtauthprofile.loxilb.io loxiurls.loxiurl.loxilb.io -o name 2>/dev/null | wc -l | tr -d ' ')" "4"
if lpoll 30 bash -c "[ \"\$(kubectl -n $HELM_NAMESPACE get pods -l app.kubernetes.io/component=test --no-headers 2>/dev/null | wc -l)\" = 0 ]"; then
  lok "no helm test pod left behind (the pre-delete drain removes them)"
else
  lbad "helm test pods cleaned up" "$(kubectl -n "$HELM_NAMESPACE" get pods -l app.kubernetes.io/component=test --no-headers 2>/dev/null | awk '{print $1" "$3}' | tr '\n' ' ')"
fi

echo "--- reinstall into the same cluster (CRDs already present) ---"
if helm install "$HELM_RELEASE" "$CHART" -n "$HELM_NAMESPACE" -f "$CHART/values-k3d-dev.yaml" $HELM_SET_ARGS --wait --timeout 10m >/dev/null; then
  lok "helm install returns success on a cluster that already has the CRDs"
  if lpoll 120 bash -c "[ \"\$(curl -s --max-time 5 http://$NODE_IP:11111/netlox/v1/config/loadbalancer/all | jq '[.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_vllm-qwen3-inference\"))] | length')\" = 1 ]"; then
    lok "the rule is back on the gateway"
  else
    lbad "the rule is back on the gateway" "not within 120s"
  fi
  served=""
  for attempt in 1 2 3 4 5 6; do served=$(served_by); grep -qw "${served:-__none__}" <<<"$(POOL_PODS)" && break; served=""; sleep 5; done
  if [[ -n "$served" ]]; then lok "traffic served again (by $served)"; else lbad "traffic served again" "no X-Served-By from a pool pod"; fi
else
  lbad "helm install (reinstall)" "returned an error"
fi
[ $lfails -eq 0 ] || rc=1

echo
if [ $rc -eq 0 ]; then
  echo "SCENARIO-k3d-helm-inference-gwapi [OK]"
  exit 0
fi
echo "SCENARIO-k3d-helm-inference-gwapi [FAILED]"
exit 1
