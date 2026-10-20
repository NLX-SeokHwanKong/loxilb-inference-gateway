#!/bin/bash
# k3d-helm-inference-gwapi — bring-up.
# The in-cluster topology of ../k3d-incluster-inference-gwapi, except that
# loxilb, kube-loxilb and the gateway objects come from one `helm install` of
# deploy/helm/loxilb-inference-gateway (values-k3d-dev.yaml) instead of
# `kubectl apply` of the testbed manifests and fixtures. The cluster, the
# images, the mock vLLM servers and the Gateway API / Inference Extension CRDs
# still come from k3d_common.sh, so the only variable is the chart.
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../common/k8s-inference/k3d_common.sh"
CHART="$(cd "$HERE/../../deploy/helm/loxilb-inference-gateway" && pwd)"
HELM_RELEASE="${HELM_RELEASE:-igw}"
HELM_NAMESPACE="${HELM_NAMESPACE:-kube-system}"

command -v helm >/dev/null 2>&1 || { echo "FATAL: helm is not installed"; exit 1; }

# <registry/repo>[:tag] -> repository / tag (a registry port is not a tag)
image_repo() { case "${1##*/}" in *:*) echo "${1%:*}" ;; *) echo "$1" ;; esac; }
image_tag()  { case "${1##*/}" in *:*) echo "${1##*:}" ;; *) echo latest ;; esac; }

say "helm lint $CHART (values-k3d-dev.yaml)"
lint=$(helm lint "$CHART" --with-subcharts -f "$CHART/values-k3d-dev.yaml" 2>&1) || { echo "$lint"; exit 1; }

igw_cluster_up igw-helm           || exit 1
igw_prepare_images                || exit 1
# A preset KLB_IMAGE that is not in the local runtime would only surface as a
# swallowed import error and an ImagePullBackOff ten minutes later.
docker image inspect "$KLB_IMAGE" >/dev/null 2>&1 || { echo "FATAL: KLB_IMAGE $KLB_IMAGE is not in the local docker runtime"; exit 1; }
igw_import_images                 || exit 1
say "mock vLLM servers (namespace llm)";  igw_deploy_mock       || exit 1
say "Gateway API $GWAPI_VERSION + Inference Extension $GIE_VERSION CRDs"
igw_install_gwapi_crds            || exit 1
say "client container";                   igw_client_up         || exit 1

say "helm install $HELM_RELEASE: loxilb-igw + kube-loxilb + gateway objects (VIP $NODE_IP)"
if ! helm upgrade --install "$HELM_RELEASE" "$CHART" -n "$HELM_NAMESPACE" \
    -f "$CHART/values-k3d-dev.yaml" \
    --set "loxilb-igw.image.repository=$(image_repo "$IGW_IMAGE")" \
    --set "loxilb-igw.image.tag=$(image_tag "$IGW_IMAGE")" \
    --set "kube-loxilb.image.repository=$(image_repo "$KLB_IMAGE")" \
    --set "kube-loxilb.image.tag=$(image_tag "$KLB_IMAGE")" \
    --set "kube-loxilb.cidrPools.defaultPool=$NODE_IP/32" \
    --wait --timeout 10m; then
  echo "FATAL: helm install failed"
  kubectl get pods -A 2>/dev/null
  kubectl -n "$HELM_NAMESPACE" logs "job/$HELM_RELEASE-loxilb-inference-gateway-crd-precheck" 2>/dev/null
  exit 1
fi
# The same gate the manifest path applies after its DaemonSet rollout: the REST
# answers on the node, and it is the inference-gateway flavor.
for i in $(seq 1 30); do
  curl -s --max-time 3 "http://$NODE_IP:11111/netlox/v1/version" | grep -q '"product":"loxilb-inference-gateway"' && break
  [ "$i" -lt 30 ] || { echo "FATAL: loxilb REST did not come up (or wrong flavor) on $NODE_IP:11111"; exit 1; }
  sleep 2
done
igw_env_save "$HERE"
cat >> "$HERE/.env" <<ENV
HELM_RELEASE=$HELM_RELEASE
HELM_NAMESPACE=$HELM_NAMESPACE
ENV
say "testbed up (gateway address $NODE_IP, release $HELM_RELEASE)"
