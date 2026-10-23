#!/bin/bash
# full-suite-manifest.sh <suite artifacts> <B4 artifacts> [<manifest.json>]
#
# The scheduling suite (validation-sched.sh) declares B4 elsewhere: it is run
# by cicd/vllm-kvcache-routing-cpu/b4-band-arm.sh, where the KV infrastructure
# is. A run of the suite alone is therefore a scheduling-only result, and its
# summary says so (elsewhere=1). A FULL-SUITE claim needs both results on the
# SAME candidate: this script reads each run's identity.json and summary.txt
# (both written by the runs themselves) and writes one manifest that says
# which claim the pair supports and why. It exits 0 only for a full-suite
# claim; a missing, failed or stale B4 result (another image id or another
# gateway/eBPF commit than the suite's) never becomes a PASS, it blocks the
# claim and the manifest names the reason. REQUIRE_ALL=1 is the suite's own
# rule for pending cases; it says nothing about B4, this manifest does.
S=${1:?suite artifacts dir}; B=${2:?B4 artifacts dir}; M=${3:-$S/full-suite-manifest.json}
python3 - "$S" "$B" "$M" <<'PY'
import json, os, re, sys
S, B, M = sys.argv[1:4]
def load(d, name):
    try:
        return json.load(open(os.path.join(d, 'identity.json'))), open(os.path.join(d, 'summary.txt')).read()
    except FileNotFoundError as e:
        return None, 'missing: %s' % e.filename
sid, ssum = load(S, 'suite'); bid, bsum = load(B, 'B4')
reasons = []
inv = dict(re.findall(r'(\w+)=(\d+)', ssum))
if sid is None: reasons.append('suite identity or summary ' + ssum)
elif '[OK]' not in ssum: reasons.append('the suite did not pass: ' + ssum.strip().splitlines()[-1])
else:
    for k in ('fail', 'pending', 'not_executed'):
        if inv.get(k, '1') != '0': reasons.append('suite %s=%s' % (k, inv.get(k)))
    if inv.get('elsewhere') != '1': reasons.append('suite declares %s cases elsewhere, expected 1 (B4)' % inv.get('elsewhere'))
if bid is None: reasons.append('B4 ' + bsum)
elif '[OK]' not in bsum: reasons.append('B4 did not pass: ' + bsum.strip())
if sid and bid:
    for k in ('image_id', 'gateway_commit', 'ebpf_commit'):
        if sid.get(k) != bid.get(k): reasons.append('stale: B4 %s %s is not the suite\'s %s' % (k, bid.get(k), sid.get(k)))
claim = 'full-suite' if not reasons else 'scheduling-only'
out = {'claim': claim, 'reasons': reasons, 'candidate': {k: (sid or {}).get(k) for k in ('gateway_commit', 'ebpf_commit', 'libbpf_commit', 'image', 'image_id', 'image_digest', 'binary_sha256')},
       'suite': {'identity': sid, 'summary': ssum.strip()}, 'b4': {'identity': bid, 'summary': bsum.strip()}}
json.dump(out, open(M, 'w'), indent=2)
print('FULL-SUITE MANIFEST %s: %s%s' % (M, claim, ('' if not reasons else ' (' + '; '.join(reasons) + ')')))
sys.exit(0 if claim == 'full-suite' else 1)
PY
