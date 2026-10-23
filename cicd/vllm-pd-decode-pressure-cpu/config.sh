#!/bin/bash
# config.sh — P/D decode selection under KV pressure (GPU-free, mock P/D bed).
#
# Stands up one plain P/D disaggregation service over 2 prefill + 3 decode mock_vllm.py
# backends and arms the decode KV-pressure weight, which is process environment read
# getenv-once at start (pd_decode_kv_weight, sockproxy_pd.c):
#
#   LLB_PD_DECODE_KV_WEIGHT  (KV_W)   0 = the shipped min-load decode scorer (default);
#                                     1..1000 = virtual in-flight requests a decode EP at
#                                     100 % KV usage carries (score += kv_pct * W / 100)
#
# Because the knob cannot be toggled on a live gateway, validation.sh runs TWO gateway
# lifecycles (KV_W=0 for the byte-identity case, KV_W=8 for the pressure cases) and
# re-invokes this script between them, exactly as vllm-pd-admission-cpu does for its
# admission knobs. An optional third lifecycle runs the UNTOUCHED baseline image
# (BASELINE_IMAGE) so S1 can compare the W=0 distribution across binaries.
#
# Topology (5 EPs: prefill at absolute indices 0 and 2, decode at 1, 3, 4, so the decode
# set is a non-contiguous bitmask):
#
#   llb1   — loxilb (control plane + eBPF dataplane), REST on localhost:11111 (auth-off, CICD mode)
#   l3h1   — client host
#   l3ep1  — mock_vllm.py --role prefill  31.31.31.1:8000  nixl 9001  ep-idx 1   P1  ep_role=1
#   l3ep2  — mock_vllm.py --role decode   32.32.32.1:8000  nixl 9002  ep-idx 2   D1  ep_role=2
#   l3ep3  — mock_vllm.py --role prefill  33.33.33.1:8000  nixl 9003  ep-idx 3   P2  ep_role=1
#   l3ep4  — mock_vllm.py --role decode   34.34.34.1:8000  nixl 9004  ep-idx 4   D2  ep_role=2
#   l3ep5  — mock_vllm.py --role decode   35.35.35.1:8000  nixl 9005  ep-idx 5   D3  ep_role=2
#
# The mock is ../vllm-pd-disagg/mock_vllm.py, docker-cp'd as the sglang/trtllm scenarios do
# (one copy of the mock in the tree). Its /metrics answers 404 by default ("absent" mode), so
# validation.sh switches every mock explicitly (POST /admin/metrics-mode-vllm|sglang on the
# mock's loopback admin port 9000) and gives each decode EP its own usage with
# /admin/metrics-kv-usage/<pct> or drops the family with /admin/metrics-kv-usage-omit.
#
# Decode attribution: every answer carries X-Request-Id:
#   ___prefill_addr_<ip>:<nixl>___decode_addr_<ip>:<nixl>_<uuid>
# so the decode EP each request landed on is read off the receipt, not off backend logs.
#
# Run on the REMOTE testbed (Docker + ip netns are NOT available on macOS):
#   sudo ./config.sh && sudo ./validation.sh ; ./rmconfig.sh
# macOS only validates `bash -n` + `shellcheck -S error`.

# No host-side port publish: REST is driven via $hexec (in-netns curl); resident
# --net=host loxilb hosts own 8091/11111.
export LLB_HOST_PORTS=""
cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1
source ../common.sh

CFGDIR="$(cd "$(dirname "$0")" && pwd)"
MOCK="${CFGDIR}/../vllm-pd-disagg/mock_vllm.py"
[[ -f "${MOCK}" ]] || { echo "FATAL: ${MOCK} missing"; exit 1; }

# Idempotency: a prior (interactive/aborted) run leaves the topology + llb1 counters
# behind, and spawn_docker_host silently no-ops on existing names — validation would then
# run against STALE state. Always self-clean first; rmconfig is scoped to this scenario.
"${CFGDIR}/rmconfig.sh" >/dev/null 2>&1 || true

KV_W="${KV_W:-0}"
export KV_W

echo "#########################################"
echo "P/D decode-pressure fixture: LLB_PD_DECODE_KV_WEIGHT=${KV_W} image=${lxdocker}"
echo "#########################################"

echo "#########################################"
echo "Spawning all hosts (5 EPs = 2 prefill + 3 decode)"
echo "#########################################"

# The weight is injected HERE because it is read getenv-once at process start. Recreating
# this container is the only way to change it, which is what validation.sh does between
# its lifecycles.
spawn_docker_host --dock-type loxilb --dock-name llb1 \
    --docker-args "-e LLB_PD_DECODE_KV_WEIGHT=${KV_W}"
spawn_docker_host --dock-type host --dock-name l3h1
spawn_docker_host --dock-type host --dock-name l3ep1
spawn_docker_host --dock-type host --dock-name l3ep2
spawn_docker_host --dock-type host --dock-name l3ep3
spawn_docker_host --dock-type host --dock-name l3ep4
spawn_docker_host --dock-type host --dock-name l3ep5

# NOTE: this MUST run before config_docker_host below: --gw replaces each EP's default
# route with llb1, after which the container has no internet egress and apt grinds against
# unreachable mirrors for ~30 min before failing (F-3, FLAKES.md). sglang-pd-disagg orders
# it the same way; vllm-pd-disagg gets away with the late order only because its EPs have no --gw.
echo "#########################################"
echo "Installing python3 in endpoint containers (skipped when the image ships it)"
echo "#########################################"

for ep in l3ep1 l3ep2 l3ep3 l3ep4 l3ep5; do
  # Same recipe as vllm-pd-disagg/config.sh: skip apt when python3 is present, prefer the
  # Korean mirror when it must run (recurring archive.ubuntu.com flakiness on this infra).
  if ! $dexec $ep python3 --version > /dev/null 2>&1; then
    $dexec $ep bash -c "sed -i 's|//archive.ubuntu.com|//kr.archive.ubuntu.com|g' /etc/apt/sources.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true; apt-get update > /dev/null 2>&1 && apt-get install -y python3 > /dev/null 2>&1"
  fi
  $dexec $ep python3 --version || { echo "FATAL: python3 install failed on $ep"; exit 1; }
done

echo "#########################################"
echo "Connecting and configuring hosts"
echo "#########################################"

connect_docker_hosts l3h1  llb1
connect_docker_hosts l3ep1 llb1
connect_docker_hosts l3ep2 llb1
connect_docker_hosts l3ep3 llb1
connect_docker_hosts l3ep4 llb1
connect_docker_hosts l3ep5 llb1

sleep 5

# L3 config — each backend on its own /24 so loxilb routes to it as a distinct member.
config_docker_host --host1 l3h1  --host2 llb1 --ptype phy --addr 10.10.10.1/24 --gw 10.10.10.254
config_docker_host --host1 l3ep1 --host2 llb1 --ptype phy --addr 31.31.31.1/24 --gw 31.31.31.254
config_docker_host --host1 l3ep2 --host2 llb1 --ptype phy --addr 32.32.32.1/24 --gw 32.32.32.254
config_docker_host --host1 l3ep3 --host2 llb1 --ptype phy --addr 33.33.33.1/24 --gw 33.33.33.254
config_docker_host --host1 l3ep4 --host2 llb1 --ptype phy --addr 34.34.34.1/24 --gw 34.34.34.254
config_docker_host --host1 l3ep5 --host2 llb1 --ptype phy --addr 35.35.35.1/24 --gw 35.35.35.254
config_docker_host --host1 llb1 --host2 l3h1  --ptype phy --addr 10.10.10.254/24
config_docker_host --host1 llb1 --host2 l3ep1 --ptype phy --addr 31.31.31.254/24
config_docker_host --host1 llb1 --host2 l3ep2 --ptype phy --addr 32.32.32.254/24
config_docker_host --host1 llb1 --host2 l3ep3 --ptype phy --addr 33.33.33.254/24
config_docker_host --host1 llb1 --host2 l3ep4 --ptype phy --addr 34.34.34.254/24
config_docker_host --host1 llb1 --host2 l3ep5 --ptype phy --addr 35.35.35.254/24

sleep 5

echo "#########################################"
echo "Starting the mocks (P1 P2 prefill, D1 D2 D3 decode)"
echo "#########################################"

# <container> <role> <nixl-port> <ep-idx>
start_mock() {
  local ep="$1" role="$2" nixl="$3" idx="$4"
  docker cp "${MOCK}" "${ep}:/tmp/mock_vllm.py"
  $dexec "$ep" bash -c "nohup python3 /tmp/mock_vllm.py --role ${role} --port 8000 --nixl-port ${nixl} --ep-idx ${idx} > /tmp/vllm-server${idx}.log 2>&1 &"
}
start_mock l3ep1 prefill 9001 1
start_mock l3ep2 decode  9002 2
start_mock l3ep3 prefill 9003 3
start_mock l3ep4 decode  9004 4
start_mock l3ep5 decode  9005 5

sleep 3
for ep in l3ep1 l3ep2 l3ep3 l3ep4 l3ep5; do
  ok=0
  for _ in $(seq 1 10); do
    if $dexec $ep curl -sf http://localhost:8000/health > /dev/null 2>&1; then ok=1; break; fi
    sleep 1
  done
  [[ "$ok" == 1 ]] || { echo "FATAL: mock on $ep did not answer /health"; exit 1; }
  echo "  $ep mock healthy"
done

# ── REST readiness poll: REST on :11111 comes up only AFTER the eBPF load (10-20s). ──
LBBASE="http://localhost:11111/netlox/v1/config/loadbalancer"
echo "Waiting for loxilb REST API (localhost:11111) to be ready..."
api_ready=0
for _ in $(seq 1 40); do
    rc=$($hexec llb1 curl -s -m 3 -o /dev/null -w "%{http_code}" "${LBBASE}/all" 2>/dev/null)
    if [[ "$rc" == "200" ]]; then
        api_ready=1; echo "  loxilb REST API ready (HTTP ${rc})"; break
    fi
    sleep 1
done
[[ "$api_ready" == 1 ]] || { echo "  FATAL: loxilb REST API not ready after 40s"; exit 1; }

# Metrics are what the scenario reads for its fixture receipts (scrape outcomes).
$hexec llb1 curl -s -X POST http://localhost:11111/netlox/v1/config/metrics >/dev/null 2>&1 || true

# ── Seed the P/D service ───────────────────────────────────────────────────────────────
# Plain pd_disagg over HTTP (no TLS, no kvExactMode, no cache-aware mode): the decode
# selector runs on every request after the prefill pick, so nothing else is needed, and
# every unrelated failure surface stays out of a scenario whose subject is one scorer.
# The http probe on /health is what lets S6 (a decode EP hard-down) see the EP demoted
# after the connect-time retry has already routed around it.
VIP="10.10.10.254"
VPORT=8080
read -r -d '' SEED_PDRULE <<JSON
{
  "serviceArguments": {
    "externalIP": "${VIP}",
    "port": ${VPORT},
    "protocol": "tcp",
    "sel": 0,
    "mode": 4,
    "pd_disagg_mode": true,
    "sse_mode": true,
    "monitor": true,
    "probetype": "http",
    "probeport": 8000,
    "probereq": "/health",
    "probeTimeout": 5,
    "probeRetries": 2
  },
  "endpoints": [
    { "endpointIP": "31.31.31.1", "targetPort": 8000, "weight": 1, "ep_role": 1, "nixl_port": 9001 },
    { "endpointIP": "32.32.32.1", "targetPort": 8000, "weight": 1, "ep_role": 2, "nixl_port": 9002 },
    { "endpointIP": "33.33.33.1", "targetPort": 8000, "weight": 1, "ep_role": 1, "nixl_port": 9003 },
    { "endpointIP": "34.34.34.1", "targetPort": 8000, "weight": 1, "ep_role": 2, "nixl_port": 9004 },
    { "endpointIP": "35.35.35.1", "targetPort": 8000, "weight": 1, "ep_role": 2, "nixl_port": 9005 }
  ]
}
JSON

echo "Seeding P/D service ${VIP}:${VPORT} (2 prefill + 3 decode)..."
seed_rc=$($hexec llb1 curl -s -o /dev/null -w "%{http_code}" \
    -X POST "${LBBASE}" -H 'Content-Type: application/json' -d "${SEED_PDRULE}")
echo "  POST /config/loadbalancer (P/D rule) -> HTTP ${seed_rc}"
[[ "$seed_rc" == "200" ]] || { echo "  FATAL: P/D rule seed answered '${seed_rc}' (expected 200)"; exit 1; }

sleep 3

echo "Waiting for the VIP to serve..."
vip_ok=0
for _ in $(seq 1 30); do
    vrc=$($hexec l3h1 curl -s -m 5 -o /dev/null -w "%{http_code}" \
        -X POST "http://${VIP}:${VPORT}/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"warmup"}],"max_tokens":4}' 2>/dev/null)
    if [[ "$vrc" == "200" ]]; then vip_ok=1; echo "  VIP serving (HTTP 200)"; break; fi
    sleep 2
done
[[ "$vip_ok" == 1 ]] || { echo "  FATAL: VIP ${VIP}:${VPORT} never served a 200"; exit 1; }

echo "#########################################"
echo "vllm-pd-decode-pressure-cpu config done (LLB_PD_DECODE_KV_WEIGHT=${KV_W})"
echo "#########################################"
