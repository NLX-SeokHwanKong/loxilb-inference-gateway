#!/bin/bash
source ../common.sh
source ./lib/classload.sh
echo SCENARIO-ai-tenant-cleanup
stop_helpers
sleep 1
disconnect_docker_hosts tqllb tqh1
disconnect_docker_hosts tqllb tqep1
delete_docker_host tqep1
delete_docker_host tqh1
delete_docker_host tqllb
docker rm -f "$PG_NAME" >/dev/null 2>&1
rm -rf cert __pycache__ .loxi-helpers.pid keys.env
# Nothing this scenario started may be left behind.
left=""
for c in tqllb tqh1 tqep1 "$PG_NAME"; do docker inspect "$c" >/dev/null 2>&1 && left="$left container:$c"; done
for n in tqllb tqh1 tqep1; do ip netns list 2>/dev/null | grep -qw "$n" && left="$left netns:$n"; done
pgrep -f '/ai-tenant/\.\./ai-admission/mock_backend\.py 8080 8081$' >/dev/null 2>&1 && left="$left process:mock_backend"
if [ -n "$left" ]; then
  echo "SCENARIO-ai-tenant-cleanup [FAILED] leftovers:$left"; exit 1
fi
echo SCENARIO-ai-tenant-cleanup [OK]
