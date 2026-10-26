#!/bin/bash
# record_identity <outdir> <gateway container>: the candidate tuple of a
# scenario run, written to <outdir>/identity.json and printed on one line:
# the superproject commit, the loxilb-ebpf gitlink and its libbpf gitlink,
# the gateway image the container runs (tag, id, repo digest when pulled) and
# the SHA256 of the loxilb binary inside it. A result without this tuple
# cannot be matched to a candidate, so a run records it before its first case.
record_identity() {
  local out=$1 c=$2 root img iid dig bin super ebpf libbpf
  root=$(git rev-parse --show-toplevel 2>/dev/null)
  super=$(git -C "$root" rev-parse HEAD 2>/dev/null || echo unknown)
  ebpf=$(git -C "$root" ls-tree HEAD loxilb-ebpf 2>/dev/null | awk '{print $3}'); ebpf=${ebpf:-unknown}
  libbpf=$(git -C "$root/loxilb-ebpf" ls-tree HEAD libbpf 2>/dev/null | awk '{print $3}'); libbpf=${libbpf:-unknown}
  img=$(docker inspect -f '{{.Config.Image}}' "$c" 2>/dev/null || echo unknown)
  iid=$(docker inspect -f '{{.Image}}' "$c" 2>/dev/null || echo unknown)
  dig=$(docker image inspect -f '{{join .RepoDigests ","}}' "$iid" 2>/dev/null); dig=${dig:-none}
  bin=$(docker exec "$c" sha256sum /root/loxilb-io/loxilb/loxilb 2>/dev/null | awk '{print $1}'); bin=${bin:-unknown}
  mkdir -p "$out"
  python3 - "$out/identity.json" "$super" "$ebpf" "$libbpf" "$img" "$iid" "$dig" "$bin" "$c" <<'PY'
import json, sys, time
f, super_, ebpf, libbpf, img, iid, dig, bin_, c = sys.argv[1:]
json.dump({"recorded": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "gateway_commit": super_, "ebpf_commit": ebpf,
           "libbpf_commit": libbpf, "image": img, "image_id": iid, "image_digest": dig,
           "binary_sha256": bin_, "container": c}, open(f, "w"), indent=2)
PY
  echo "IDENTITY gateway=$super ebpf=$ebpf libbpf=$libbpf image=$img id=$iid digest=$dig binary=$bin"
}
