#!/bin/bash
# k3d-helm-inference-gwapi — assertions.
# Two layers. First `helm test` runs the chart's own test pods (the management
# REST is the inference-gateway flavor; InferencePool -> Service -> loxilb rule).
# Then ../k3d-incluster-inference-gwapi/validation.sh is sourced unchanged
# against the helm-installed stack: the same assertions passing means the chart
# is equivalent to the testbed manifests.
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
if [ $rc -eq 0 ]; then
  echo "SCENARIO-k3d-helm-inference-gwapi [OK]"
  exit 0
fi
echo "SCENARIO-k3d-helm-inference-gwapi [FAILED]"
exit 1
