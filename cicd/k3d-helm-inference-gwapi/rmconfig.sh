#!/bin/bash
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../common/k8s-inference/k3d_common.sh"
# Uninstall before the cluster goes, so a release that does not come out
# cleanly (hooks, CRD finalizers) is seen here rather than hidden by the delete.
if [ -f "$HERE/.env" ]; then
  . "$HERE/.env"; export KUBECONFIG
  helm uninstall "${HELM_RELEASE:-igw}" -n "${HELM_NAMESPACE:-kube-system}" --timeout 5m 2>&1 | sed 's/^/  /'
fi
igw_teardown igw-helm "$HERE"
echo "testbed removed"
