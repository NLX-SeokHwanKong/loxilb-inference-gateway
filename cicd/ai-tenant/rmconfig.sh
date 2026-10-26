#!/bin/bash
source ../common.sh
source ./lib/classload.sh
source ./lib/topo-b.sh
echo SCENARIO-ai-tenant-cleanup
stop_helpers
sleep 1
disconnect_docker_hosts tqllb tqh1
disconnect_docker_hosts tqllb tqep1
delete_docker_host tqep1
delete_docker_host tqh1
delete_docker_host tqllb
docker rm -f -v "$PG_NAME" >/dev/null 2>&1
pg_volume=$(cat .pg-volume 2>/dev/null)
rm -rf cert __pycache__ .loxi-helpers.pid keys.env .pg-volume
# Nothing this scenario started may be left behind.
left=""
for c in tqllb tqh1 tqep1 "$PG_NAME"; do docker inspect "$c" >/dev/null 2>&1 && left="$left container:$c"; done
for n in tqllb tqh1 tqep1; do ip netns list 2>/dev/null | grep -qw "$n" && left="$left netns:$n"; done
pgrep -f '/ai-tenant/\.\./ai-admission/mock_backend\.py 8080 8081$' >/dev/null 2>&1 && left="$left process:mock_backend"
pgrep -f '/ai-tenant/\.\./ai-admission/h2c_backend\.py tq-h2-' >/dev/null 2>&1 && left="$left process:h2c_backend"
if [ -n "$pg_volume" ] && docker volume inspect "$pg_volume" >/dev/null 2>&1; then left="$left volume:$pg_volume"; fi
left="$left$(tqb_leftovers)"
if [ -n "$left" ]; then
  echo "SCENARIO-ai-tenant-cleanup [FAILED] leftovers:$left"; exit 1
fi
echo SCENARIO-ai-tenant-cleanup [OK]
