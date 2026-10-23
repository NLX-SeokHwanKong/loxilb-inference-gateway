#!/bin/bash
# rmconfig.sh — idempotent SCOPED teardown for the vllm-pd-decode-pressure-cpu testbed.
#
# disconnect_docker_hosts / delete_docker_host already tolerate already-gone hosts, so this
# is safe to re-run after a partial or failed config.sh / validation.sh. Teardown is SCOPED
# per-container — NEVER a host-wide network-namespace sweep or a process-name-wide kill
# (`ip netns exec` is a NETWORK namespace, not a pid namespace; the mocks are killed inside
# their own containers only).
#
# This script is called BETWEEN LIFECYCLES, not only at the end: validation.sh tears the bed
# down and re-runs config.sh with a different LLB_PD_DECODE_KV_WEIGHT, because the knob is
# read getenv-once at process start.

cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1
source ../common.sh

CFGDIR="$(cd "$(dirname "$0")" && pwd)"

for ep in l3ep1 l3ep2 l3ep3 l3ep4 l3ep5; do
    $dexec "$ep" pkill -f 'mock_vllm.py' >/dev/null 2>&1 || true
done

disconnect_docker_hosts l3h1  llb1
disconnect_docker_hosts l3ep1 llb1
disconnect_docker_hosts l3ep2 llb1
disconnect_docker_hosts l3ep3 llb1
disconnect_docker_hosts l3ep4 llb1
disconnect_docker_hosts l3ep5 llb1

delete_docker_host llb1
delete_docker_host l3h1
delete_docker_host l3ep1
delete_docker_host l3ep2
delete_docker_host l3ep3
delete_docker_host l3ep4
delete_docker_host l3ep5

# Runtime artifacts dropped by validation.sh (per-case receipt sequences). Scoped by name.
rm -f "${CFGDIR}"/.dp-* >/dev/null 2>&1 || true

echo "#########################################"
echo "Deleted vllm-pd-decode-pressure-cpu testbed (5 EPs + client + llb1)"
echo "#########################################"
