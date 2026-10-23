#!/bin/bash
# validation.sh — P/D decode selection under KV pressure, exit gate (GPU-free mock P/D bed).
#
# Subject: pd_select_decode_excluding's KV-pressure term (sockproxy_pd.c), fed by the
# scraper sink (pkg/loxinet/ai_vllm_scraper.go) through llb_ai_update_ep_kv_usage, and
# switched by LLB_PD_DECODE_KV_WEIGHT (getenv-once). Cases S1–S9 of the Phase A plan §5.1.
#
# ── why this file reconfigures the bed halfway through ────────────────────────────────
# The weight is process environment read once at start, so the W=0 identity case and the
# W=8 pressure cases are TWO gateway lifecycles; config.sh is re-invoked between them with
# KV_W. With BASELINE_IMAGE set, a lifecycle on the untouched baseline binary runs first so
# S1 compares the W=0 distribution across binaries (the plan's byte-identity check).
#
# ── how a decode pick is observed ─────────────────────────────────────────────────────
# The decode that SERVED an answer is read off the mock's own X-Decode-Ep response header
# (ep-idx 2/4/5 = D1/D2/D3). The gateway's receipt, X-Request-Id:
# ___prefill_addr_<ip>:<nixl>___decode_addr_<ip>:<nixl>_<uuid>, is recorded next to it; the
# two agree whenever the first decode connect succeeds, but the receipt is stamped at
# admission and pd_connect_decode's connect-time fallback does NOT re-render it (observed in
# S6, FLAKES.md F-5, owner item), so attribution never relies on the receipt.
# A drive of N sequential requests records the serving decode of each one (D1/D2/D3) in order,
# and the asserts read counts and the shape of that sequence. Requests are sequential on
# purpose: with no overlap every candidate has active_conns 0 and the same scraped queue
# depth (the mock serves num_requests_waiting 3.0 for all), so the min-load score ties and
# the only terms that can separate the decodes are the RR tie-break and the kv term under
# test. A pure 3-cycle (D?, D?, D?, D?, ... with period 3) is therefore the fingerprint of
# "the kv term did not separate anyone", and a zero share is the fingerprint of "it did".
#
# ── the traps this stage encodes ──────────────────────────────────────────────────────
# 1. THE MOCK SERVES NO /metrics BY DEFAULT. Every case sets the mode it needs on every
#    mock and waits for the scraper (10 s period) to deliver; the scrape counter is read
#    as the receipt that a sample actually arrived (a case that drove before the scrape
#    would score the previous case's values as its own).
# 2. STALENESS IS 30 s, NOT ONE PERIOD. An EP switched to "absent" keeps its last value
#    fresh until telemetry_stale_ms (30000 default) has elapsed since its last stamp; the
#    neutralisation cases (S3, S4) wait STALE_WAIT before driving.
# 3. THE RR OFFSET IS NOT REPRODUCIBLE ACROSS LIFECYCLES (config.sh's warm-up loop may
#    consume a variable number of selections), so S1's byte-identity compares per-decode
#    counts and the cycle shape, never the absolute alignment of the two sequences.
# 4. FLOAT TRUNCATION: the Go sink stores uint32(ratio * 100); 0.92 and 0.4 are read as 92
#    and 40 exactly, which is why the fixture uses those values (0.29 would read as 28).
#
# Run: sudo ./config.sh && sudo ./validation.sh ; ./rmconfig.sh
#   BASELINE_IMAGE=<tag or ref>  also run the W=0 lifecycle on that image for S1 (optional)
#   N_REQ (default 30)           requests per drive

cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1
source ../common.sh

CFGDIR="$(cd "$(dirname "$0")" && pwd)"
# Sequences that must survive a lifecycle change are kept as .seq-*: rmconfig.sh (run by
# lifecycle() before every config.sh) sweeps .dp-*, so a kept copy under that glob is lost
# before the compare (FLAKES.md F-4). .seq-* is this script's own and removed on exit.
trap 'rm -f "${CFGDIR}"/.seq-*' EXIT
VIP="10.10.10.254"
VPORT="8080"
METRICS="http://localhost:11111/netlox/v1/metrics"
N_REQ="${N_REQ:-30}"
SCRAPE_WAIT="${SCRAPE_WAIT:-14}"   # > one 10 s scraper period, with margin
STALE_WAIT="${STALE_WAIT:-45}"     # > telemetry_stale_ms (30 s) + one scraper period
KV_W_PRESSURE=8                    # 92 % -> +7, 40 % -> +3 virtual in-flight (integer math)

SKELETON_STRICT="${SKELETON_STRICT:-1}"
code=0

assert() {
    local name="$1" ok="$2"
    if [[ "$ok" == 1 ]]; then
        echo "  ${name} [OK]"
    elif [[ "$SKELETON_STRICT" == 1 ]]; then
        echo "  ${name} [FAILED]"
        code=1
    else
        echo "  ${name} [PENDING] (SKELETON_STRICT=0 dev dry-run — would have FAILED)"
    fi
}

llb_curl() { $hexec llb1 curl -s --max-time 10 --retry 2 "$@"; }

# series_sum <family> <label=value>... — the sum of every series of the family that
# carries all the given labels, in any order.
series_sum() {
    local fam=$1; shift
    llb_curl "${METRICS}" 2>/dev/null | awk -v fam="$fam" -v want="$*" '
        BEGIN { n = split(want, w, " ") }
        index($0, fam "{") == 1 {
            ok = 1
            for (i = 1; i <= n; i++) {
                split(w[i], kv, "=")
                if (index($0, kv[1] "=\"" kv[2] "\"") == 0) { ok = 0; break }
            }
            if (ok) s += $NF
        }
        END { printf "%d", s + 0 }'
}
scrape_ok()   { series_sum loxilb_ai_worker_scrape_total result=ok; }
scrape_err()  { series_sum loxilb_ai_worker_scrape_total result=http_error; }

# ── the selection counters (PR-3) ─────────────────────────────────────────────────────
# loxilb_ai_pd_decode_selected_total{path} is incremented once per decode SELECTION — at
# admission for every request here (hint = the session-pinned decode kept, score = min-load
# pick, score_kv = the kv term changed the set of best candidates). The vLLM connect-time
# re-read of the admission choice (pd_connect_decode) records nothing, so a drive of N
# sequential requests moves the family by exactly N. loxilb_ai_pd_decode_kv_fill_total
# counts the scored selections (W>0) in which a candidate had no fresh reading and took the
# neutral fill. Both are read as sums over every model label.
dec_sel() { series_sum loxilb_ai_pd_decode_selected_total path="$1"; }   # hint|score|score_kv
kv_fill() { series_sum loxilb_ai_pd_decode_kv_fill_total; }
snap()    { echo "$(dec_sel hint) $(dec_sel score) $(dec_sel score_kv) $(kv_fill)"; }
# delta <snap-before> <snap-after> -> "hint=<d> score=<d> score_kv=<d> kv_fill=<d>"
delta() {
    awk -v a="$1" -v b="$2" 'BEGIN { split(a, x, " "); split(b, y, " ");
        printf "hint=%d score=%d score_kv=%d kv_fill=%d", y[1]-x[1], y[2]-x[2], y[3]-x[3], y[4]-x[4] }'
}
dget() { echo "$1" | tr ' ' '\n' | sed -n "s/^$2=//p"; }    # dget "<delta>" <name>
# counters_are "<delta>" <hint> <score> <score_kv> <kv_fill> -> 1 when every delta matches
counters_are() {
    [[ "$(dget "$1" hint)" == "$2" && "$(dget "$1" score)" == "$3" &&
       "$(dget "$1" score_kv)" == "$4" && "$(dget "$1" kv_fill)" == "$5" ]] && echo 1 || echo 0
}
# The one-shot [PD_DEC] transition lines in the data-plane log: one when a pool first has to
# neutralise a candidate's kv term, one when every candidate reports fresh again.
fill_edges()    { docker exec llb1 grep -c 'PD_DEC\] .*neutral fill' /var/log/loxilbdp.log 2>/dev/null || echo 0; }
recover_edges() { docker exec llb1 grep -c 'PD_DEC\] .*fresh on all' /var/log/loxilbdp.log 2>/dev/null || echo 0; }

# running_env <VAR> — the knob off the RUNNING container, never off the builder's intent.
running_env() {
    docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' llb1 2>/dev/null \
        | grep -E "^$1=" | head -1 | cut -d= -f2-
}
assert_fixture() {
    local want="$1" got
    got=$(running_env LLB_PD_DECODE_KV_WEIGHT)
    assert "fixture: running llb1 has LLB_PD_DECODE_KV_WEIGHT=${want} (got '${got}')" \
        "$([[ "$got" == "$want" ]] && echo 1 || echo 0)"
}

# ── mock admin (loopback :9000 inside each EP container) ──────────────────────────────
D1=l3ep2; D2=l3ep4; D3=l3ep5
DECODES="${D1} ${D2} ${D3}"
ALL_MOCKS="l3ep1 ${D1} l3ep3 ${D2} ${D3}"
mock_admin() { $dexec "$1" curl -s -m 5 -X POST "http://localhost:9000/admin/$2" 2>/dev/null; }
# set_mode <mode> [eps...]: vllm | sglang | absent, on the given mocks (default all five)
set_mode() {
    local mode="$1"; shift
    local eps="${*:-${ALL_MOCKS}}" ep r
    for ep in ${eps}; do
        r=$(mock_admin "$ep" "metrics-mode-${mode}")
        echo "$r" | grep -q "\"metrics_mode\": \"${mode}\"" || echo "  WARNING: ${ep} did not switch to ${mode}: ${r}"
    done
}
set_usage() {   # set_usage <ep> <pct>
    local r; r=$(mock_admin "$1" "metrics-kv-usage/$2")
    echo "$r" | grep -q '"kv_usage_pct"' || echo "  WARNING: $1 usage not set to $2: ${r}"
}
omit_usage() {  # omit_usage <ep>
    local r; r=$(mock_admin "$1" "metrics-kv-usage-omit")
    echo "$r" | grep -q '"kv_usage_omit": true' || echo "  WARNING: $1 usage not omitted: ${r}"
}
reset_mocks() { local ep; for ep in ${ALL_MOCKS}; do mock_admin "$ep" reset >/dev/null; done; }

# pressure_default: D1 at 92 %, D2/D3 at 40 % (the plan's S2 shape); prefill mocks at the
# historical 25 % so the prefill side keeps its shipped values.
pressure_default() { set_usage "$D1" 92; set_usage "$D2" 40; set_usage "$D3" 40; }
pressure_reversed() { set_usage "$D1" 40; set_usage "$D2" 92; set_usage "$D3" 92; }
pressure_flat()     { set_usage "$D1" 40; set_usage "$D2" 40; set_usage "$D3" 40; }

# wait_scrape <min-delta-of-ok-scrapes>: the receipt that samples arrived after a change.
wait_scrape() {
    local before after
    before=$(scrape_ok)
    sleep "${SCRAPE_WAIT}"
    after=$(scrape_ok)
    # Diagnostic to stderr: this function's stdout IS the assert's verdict (captured by $(...)).
    echo "  scrape ok ${before} -> ${after} after ${SCRAPE_WAIT}s" >&2
    [[ $(( after - before )) -ge "$1" ]] && echo 1 || echo 0
}

# ── the drive ─────────────────────────────────────────────────────────────────────────
# one_req [extra curl args...] -> prints "<code> <decode-name>" (decode-name = D1|D2|D3|none)
one_req() {
    local out code rid dip name sep served
    out=$($hexec l3h1 curl -s -o /dev/null -D - -m 10 -w '__code=%{http_code}\n' \
        -X POST "http://${VIP}:${VPORT}/v1/chat/completions" \
        -H 'Content-Type: application/json' "$@" \
        -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"pressure"}],"max_tokens":8}' 2>/dev/null)
    code=$(echo "$out" | sed -n 's/^__code=//p' | tail -1)
    rid=$(echo "$out" | grep -i '^X-Request-Id:' | head -1 | tr -d '\r')
    dip=$(echo "$rid" | sed -n 's/.*___decode_addr_\([0-9.]*\):.*/\1/p')
    case "$dip" in
        32.32.32.1) name=D1 ;; 34.34.34.1) name=D2 ;; 35.35.35.1) name=D3 ;; *) name=none ;;
    esac
    sep=$(echo "$out" | grep -i '^X-Decode-Ep:' | head -1 | tr -d '\r' | awk '{print $2}')
    case "$sep" in
        2) served=D1 ;; 4) served=D2 ;; 5) served=D3 ;; *) served=none ;;
    esac
    # "<code> <served-by> <receipt-decode>": every helper below attributes by column 2.
    echo "${code:-000} ${served} ${name}"
}
# drive <tag> <n> [extra curl args...]: sequential requests; writes ${CFGDIR}/.dp-<tag>
# (one "<code> <decode>" line per request) and prints the per-decode counts.
drive() {
    local tag="$1" n="$2"; shift 2
    local f="${CFGDIR}/.dp-${tag}" i
    : > "$f"
    for i in $(seq 1 "$n"); do one_req "$@" >> "$f"; done
    echo "  ${tag}: $(summary "$f")"
}
summary() {   # summary <file> -> "200=<n> D1=<n> D2=<n> D3=<n> none=<n> receipt!=served=<n>"
    awk '{ if ($1 == "200") ok++; c[$2]++; if ($3 != $2) mm++ }
         END { printf "200=%d D1=%d D2=%d D3=%d none=%d receipt!=served=%d", ok, c["D1"], c["D2"], c["D3"], c["none"], mm }' "$1"
}
mismatches() { awk '$3 != $2 { n++ } END { print n + 0 }' "$1"; }   # receipt names another decode
cnt() { awk -v w="$2" '$2 == w { n++ } END { print n + 0 }' "$1"; }        # cnt <file> <D?>
oks() { awk '$1 == "200" { n++ } END { print n + 0 }' "$1"; }              # oks <file>
# pure_cycle <file> -> 1 when the decode sequence has period 3 over distinct names
pure_cycle() {
    awk '{ s[NR] = $2 }
         END {
           if (NR < 6) { print 0; exit }
           if (s[1] == s[2] || s[2] == s[3] || s[1] == s[3]) { print 0; exit }
           for (i = 4; i <= NR; i++) if (s[i] != s[i-3]) { print 0; exit }
           print 1
         }' "$1"
}
# same_counts <file-a> <file-b> -> 1 when the per-decode counts are equal
same_counts() {
    [[ "$(cnt "$1" D1)/$(cnt "$1" D2)/$(cnt "$1" D3)" == "$(cnt "$2" D1)/$(cnt "$2" D2)/$(cnt "$2" D3)" ]] && echo 1 || echo 0
}
third_lo=$(( N_REQ / 3 - 3 )); third_hi=$(( N_REQ / 3 + 3 ))   # a 1/3 share with slack
within_third() { [[ "$1" -ge "$third_lo" && "$1" -le "$third_hi" ]] && echo 1 || echo 0; }

LC=0
lifecycle() {  # lifecycle <KV_W> [image] — config output goes to /tmp/vllm-pd-decode-pressure-L<n>.log
    local w="$1" img="${2:-}" log rc
    LC=$(( LC + 1 )); log="/tmp/vllm-pd-decode-pressure-L${LC}.log"
    "${CFGDIR}/rmconfig.sh" >/dev/null 2>&1 || true
    if [[ -n "$img" ]]; then
        LOXILB_DOCKER_IMAGE="$img" KV_W="$w" "${CFGDIR}/config.sh" > "$log" 2>&1
    else
        KV_W="$w" "${CFGDIR}/config.sh" > "$log" 2>&1
    fi
    rc=$?
    echo "  lifecycle ${LC}: config.sh KV_W=${w} image=${img:-default} rc=${rc} (log ${log})"
    [[ $rc == 0 ]] || { echo "FATAL: config.sh (KV_W=${w} image=${img:-default}) failed rc=${rc}"; tail -20 "$log"; exit 1; }
    assert_fixture "$w"
}

#################################################################################
# Lifecycle 0 (optional): the untouched baseline binary at W=0 — S1's reference
#################################################################################
BASE_SEQ=""
if [[ -n "${BASELINE_IMAGE:-}" ]]; then
    echo "#########################################"
    echo "Lifecycle 0: BASELINE_IMAGE=${BASELINE_IMAGE}, LLB_PD_DECODE_KV_WEIGHT=0"
    echo "#########################################"
    lifecycle 0 "${BASELINE_IMAGE}"
    set_mode vllm; pressure_default
    assert "L0: scraper delivered samples after metrics-mode-vllm" "$(wait_scrape 3)"
    drive base "${N_REQ}"
    BASE_SEQ="${CFGDIR}/.seq-base"; cp "${CFGDIR}/.dp-base" "$BASE_SEQ"
    assert "L0: ${N_REQ}/${N_REQ} requests 200 on the baseline binary" \
        "$([[ "$(oks "$BASE_SEQ")" == "$N_REQ" ]] && echo 1 || echo 0)"
    assert "L0: baseline W=0 distribution is a pure 3-cycle (min-load ties -> RR)" "$(pure_cycle "$BASE_SEQ")"
fi

#################################################################################
# Lifecycle 1: image under test, W=0 — S1 identity
#################################################################################
echo "#########################################"
echo "Lifecycle 1: image under test, LLB_PD_DECODE_KV_WEIGHT=0"
echo "#########################################"
lifecycle 0
set_mode vllm; pressure_default
assert "L1: scraper delivered samples after metrics-mode-vllm" "$(wait_scrape 3)"

echo "--- S1: W=0, mixed kv usage (D1 92 / D2 40 / D3 40): the shipped min-load pick"
c0=$(snap)
drive s1 "${N_REQ}"
c1=$(snap); d=$(delta "$c0" "$c1")
S1_SEQ="${CFGDIR}/.seq-s1"; cp "${CFGDIR}/.dp-s1" "$S1_SEQ"   # survives the W=8 lifecycle for S4/S7
assert "S1: counters: ${N_REQ} selections on path=score, none on hint/score_kv, no kv_fill (${d})" \
    "$(counters_are "$d" 0 "$N_REQ" 0 0)"
assert "S1: kv_fill family is seeded for the model and reads 0 under W=0 (MON-04)" \
    "$([[ "$(llb_curl "${METRICS}" | grep -c '^loxilb_ai_pd_decode_kv_fill_total{')" -ge 1 && "$(kv_fill)" == 0 ]] && echo 1 || echo 0)"
assert "S1: ${N_REQ}/${N_REQ} requests 200" "$([[ "$(oks "${CFGDIR}/.dp-s1")" == "$N_REQ" ]] && echo 1 || echo 0)"
assert "S1: D1 share is a third despite 92 % usage (W=0 ignores the kv term; got $(cnt "${CFGDIR}/.dp-s1" D1))" \
    "$(within_third "$(cnt "${CFGDIR}/.dp-s1" D1)")"
assert "S1: distribution is a pure 3-cycle (ties -> RR, as shipped)" "$(pure_cycle "${CFGDIR}/.dp-s1")"
if [[ -n "$BASE_SEQ" ]]; then
    assert "S1: byte-identity — per-decode counts equal the baseline binary's ($(summary "$BASE_SEQ") vs $(summary "${CFGDIR}/.dp-s1"))" \
        "$(same_counts "$BASE_SEQ" "${CFGDIR}/.dp-s1")"
else
    echo "  S1: byte-identity against the baseline binary skipped (BASELINE_IMAGE unset)"
fi

#################################################################################
# Lifecycle 2: image under test, W=8 — pressure cases
#################################################################################
echo "#########################################"
echo "Lifecycle 2: image under test, LLB_PD_DECODE_KV_WEIGHT=${KV_W_PRESSURE}"
echo "#########################################"
lifecycle "${KV_W_PRESSURE}"

echo "--- S2: W=8, D1 92 / D2 40 / D3 40, equal conns -> D1 share ~0"
set_mode vllm; pressure_default
assert "S2: scraper delivered samples" "$(wait_scrape 3)"
c0=$(snap)
drive s2 "${N_REQ}"
c1=$(snap); d=$(delta "$c0" "$c1")
assert "S2: ${N_REQ}/${N_REQ} requests 200" "$([[ "$(oks "${CFGDIR}/.dp-s2")" == "$N_REQ" ]] && echo 1 || echo 0)"
assert "S2: D1 (92 %) received 0 of ${N_REQ} (got $(cnt "${CFGDIR}/.dp-s2" D1))" \
    "$([[ "$(cnt "${CFGDIR}/.dp-s2" D1)" == 0 ]] && echo 1 || echo 0)"
d2=$(cnt "${CFGDIR}/.dp-s2" D2); d3=$(cnt "${CFGDIR}/.dp-s2" D3)
assert "S2: D2 and D3 (40 % each) split the rest by RR (|${d2}-${d3}| <= 2)" \
    "$([[ $(( d2 > d3 ? d2 - d3 : d3 - d2 )) -le 2 ]] && echo 1 || echo 0)"
assert "S2: counters: every selection is path=score_kv (the kv term dropped D1 from the best set), no kv_fill (${d})" \
    "$(counters_are "$d" 0 0 "$N_REQ" 0)"

echo "--- S3: W=8, D1 metrics absent -> D1 neutralised to the fleet mean, still receives traffic"
set_mode absent "$D1"
e0=$(scrape_err); sleep "${STALE_WAIT}"; e1=$(scrape_err)
assert "S3: D1 scrapes now fail (http_error ${e0} -> ${e1})" "$([[ $(( e1 - e0 )) -ge 2 ]] && echo 1 || echo 0)"
c0=$(snap); fe0=$(fill_edges)
drive s3 "${N_REQ}"
c1=$(snap); d=$(delta "$c0" "$c1"); fe1=$(fill_edges)
assert "S3: ${N_REQ}/${N_REQ} requests 200 (zero loss)" "$([[ "$(oks "${CFGDIR}/.dp-s3")" == "$N_REQ" ]] && echo 1 || echo 0)"
assert "S3: D1 (unknown) is on par with D2/D3: a third of the picks (got $(cnt "${CFGDIR}/.dp-s3" D1), want ${third_lo}..${third_hi})" \
    "$(within_third "$(cnt "${CFGDIR}/.dp-s3" D1)")"
assert "S3: distribution is a pure 3-cycle (fill == the others' 40 -> ties -> RR)" "$(pure_cycle "${CFGDIR}/.dp-s3")"
assert "S3: counters: every selection took a fill (kv_fill == selections) and stayed path=score (${d})" \
    "$(counters_are "$d" 0 "$N_REQ" 0 "$N_REQ")"
assert "S3: one [PD_DEC] 'neutral fill in effect' line for the whole drive (one-shot transition; ${fe0} -> ${fe1})" \
    "$([[ $(( fe1 - fe0 )) == 1 ]] && echo 1 || echo 0)"

echo "--- S4: W=8, all decodes absent -> same distribution as S1"
set_mode absent "$D2" "$D3"
sleep "${STALE_WAIT}"
c0=$(snap); fe0=$(fill_edges)
drive s4 "${N_REQ}"
c1=$(snap); d=$(delta "$c0" "$c1"); fe1=$(fill_edges)
assert "S4: ${N_REQ}/${N_REQ} requests 200" "$([[ "$(oks "${CFGDIR}/.dp-s4")" == "$N_REQ" ]] && echo 1 || echo 0)"
assert "S4: all-unknown == W=0: per-decode counts equal S1's ($(summary "$S1_SEQ") vs $(summary "${CFGDIR}/.dp-s4"))" \
    "$(same_counts "$S1_SEQ" "${CFGDIR}/.dp-s4")"
assert "S4: distribution is a pure 3-cycle" "$(pure_cycle "${CFGDIR}/.dp-s4")"
assert "S4: counters: kv_fill == selections, all path=score (${d})" "$(counters_are "$d" 0 "$N_REQ" 0 "$N_REQ")"
assert "S4: no further 'neutral fill' line while the fill stays in effect (${fe0} -> ${fe1})" \
    "$([[ "$fe1" == "$fe0" ]] && echo 1 || echo 0)"

echo "--- S5: W=8, session key: the pinned decode is kept regardless of pressure"
# Pin while the pressure is REVERSED (D1 is the cheapest), then flip it so the pinned EP is
# the most pressured one: the hint must still win, and unkeyed requests must still avoid D1.
set_mode vllm; pressure_reversed
assert "S5: scraper delivered the reversed pressure" "$(wait_scrape 3)"
re0=$(recover_edges)
c0=$(snap)
drive s5pin 1 -H 'X-Conversation-Id: pd-kv-s5'
c1=$(snap); d=$(delta "$c0" "$c1")
pinned=$(awk 'NR==1 { print $2 }' "${CFGDIR}/.dp-s5pin")
assert "S5: the keyed request was pinned to the cheapest decode D1 (got ${pinned})" "$([[ "$pinned" == D1 ]] && echo 1 || echo 0)"
assert "S5: counters: the first keyed request was a fresh pick, path=score_kv (${d})" "$(counters_are "$d" 0 0 1 0)"
pressure_default
assert "S5: scraper delivered the flipped pressure (D1 now 92 %)" "$(wait_scrape 3)"
c0=$(snap)
drive s5keyed 10 -H 'X-Conversation-Id: pd-kv-s5'
c1=$(snap); dk=$(delta "$c0" "$c1")
drive s5plain 10
c2=$(snap); dp=$(delta "$c1" "$c2"); re1=$(recover_edges)
assert "S5: 10/10 keyed requests stayed on the pinned D1 under pressure (got $(cnt "${CFGDIR}/.dp-s5keyed" D1))" \
    "$([[ "$(cnt "${CFGDIR}/.dp-s5keyed" D1)" == 10 ]] && echo 1 || echo 0)"
assert "S5: 0/10 unkeyed requests reached D1 (got $(cnt "${CFGDIR}/.dp-s5plain" D1))" \
    "$([[ "$(cnt "${CFGDIR}/.dp-s5plain" D1)" == 0 ]] && echo 1 || echo 0)"
assert "S5: counters: the 10 keyed selections are path=hint and nothing else moved (${dk})" "$(counters_are "$dk" 10 0 0 0)"
assert "S5: counters: the 10 unkeyed selections are path=score_kv (${dp})" "$(counters_are "$dp" 0 0 10 0)"
assert "S5: one [PD_DEC] 'fresh on all candidates again' line once every decode reported (${re0} -> ${re1})" \
    "$([[ $(( re1 - re0 )) == 1 ]] && echo 1 || echo 0)"

echo "--- S6: W=8, D1 hard-down (connect refused) -> routed to another decode within 2 attempts"
pressure_flat
assert "S6: scraper delivered the flat pressure (40/40/40)" "$(wait_scrape 3)"
dc0=$(docker exec llb1 grep -c 'PD_DECODE_CONNECT' /var/log/loxilbdp.log 2>/dev/null); dc0=${dc0:-0}
$dexec "$D1" pkill -f 'mock_vllm.py' >/dev/null 2>&1 || true
sleep 1
c0=$(snap)
drive s6 12
c1=$(snap); d=$(delta "$c0" "$c1")
dc1=$(docker exec llb1 grep -c 'PD_DECODE_CONNECT' /var/log/loxilbdp.log 2>/dev/null); dc1=${dc1:-0}
# Observation only: the connect-time fallback is not a selection, so the family moves by the
# 12 admissions whatever the retries did (score while D1 is still admitted, then score once
# the probe has demoted it; the fallback's re-pick records nothing).
echo "  S6: counters over the 12 requests: ${d} (observation; the fallback re-pick is not a selection)"
assert "S6: 12/12 requests 200 with D1 refusing connections" "$([[ "$(oks "${CFGDIR}/.dp-s6")" == 12 ]] && echo 1 || echo 0)"
assert "S6: 0/12 answers came from D1 (got $(cnt "${CFGDIR}/.dp-s6" D1))" "$([[ "$(cnt "${CFGDIR}/.dp-s6" D1)" == 0 ]] && echo 1 || echo 0)"
# Observation, not an assert (product behaviour outside this campaign's file list, F-5):
# the receipt is stamped at admission and the decode connect fallback does not re-render it.
echo "  S6: receipt named a decode other than the one that served on $(mismatches "${CFGDIR}/.dp-s6")/12 answers (observation F-5, owner item; not asserted)"
assert "S6: the connect-time retry path ran ([PD_DECODE_CONNECT] lines ${dc0} -> ${dc1})" "$([[ "$dc1" -gt "$dc0" ]] && echo 1 || echo 0)"
# Bring D1 back for the remaining cases: restart the mock, re-arm its metrics mode, and wait
# for the health probe to readmit it (probe retries 2 at the default interval).
$dexec "$D1" bash -c "nohup python3 /tmp/mock_vllm.py --role decode --port 8000 --nixl-port 9002 --ep-idx 2 >> /tmp/vllm-server2.log 2>&1 &"
sleep 3
set_mode vllm "$D1"; set_usage "$D1" 40
back=0
for _ in $(seq 1 12); do           # up to ~2 min for the http probe to readmit D1
    drive s6back 6 >/dev/null
    if [[ "$(cnt "${CFGDIR}/.dp-s6back" D1)" -ge 1 ]]; then back=1; break; fi
    sleep 8
done
assert "S6: D1 readmitted after its mock came back (health probe)" "$back"

echo "--- S7: W=8, usage family omitted on every decode (TRT-LLM-like) -> all unknown -> identical to S1"
set_mode vllm; omit_usage "$D1"; omit_usage "$D2"; omit_usage "$D3"
assert "S7: scraper delivered samples without the usage family" "$(wait_scrape 3)"
sleep "${SCRAPE_WAIT}"   # a second period: every decode has been re-sampled as "absent family" at least once
c0=$(snap)
drive s7 "${N_REQ}"
c1=$(snap); d=$(delta "$c0" "$c1")
assert "S7: ${N_REQ}/${N_REQ} requests 200" "$([[ "$(oks "${CFGDIR}/.dp-s7")" == "$N_REQ" ]] && echo 1 || echo 0)"
assert "S7: omitted family reads as unknown, not 0 %: per-decode counts equal S1's ($(summary "${CFGDIR}/.dp-s7"))" \
    "$(same_counts "$S1_SEQ" "${CFGDIR}/.dp-s7")"
assert "S7: distribution is a pure 3-cycle" "$(pure_cycle "${CFGDIR}/.dp-s7")"
assert "S7: counters: kv_fill == selections, all path=score (${d})" "$(counters_are "$d" 0 "$N_REQ" 0 "$N_REQ")"

echo "--- S8: SGLang family (token_usage) on every decode, D1 92 / D2 40 / D3 40 -> same as S2"
set_mode sglang "$D1" "$D2" "$D3"; pressure_default
assert "S8: scraper delivered the SGLang samples" "$(wait_scrape 3)"
c0=$(snap)
drive s8 "${N_REQ}"
c1=$(snap); d=$(delta "$c0" "$c1")
assert "S8: ${N_REQ}/${N_REQ} requests 200" "$([[ "$(oks "${CFGDIR}/.dp-s8")" == "$N_REQ" ]] && echo 1 || echo 0)"
assert "S8: D1 (token_usage 0.92) received 0 of ${N_REQ} (got $(cnt "${CFGDIR}/.dp-s8" D1))" \
    "$([[ "$(cnt "${CFGDIR}/.dp-s8" D1)" == 0 ]] && echo 1 || echo 0)"
d2=$(cnt "${CFGDIR}/.dp-s8" D2); d3=$(cnt "${CFGDIR}/.dp-s8" D3)
assert "S8: D2 and D3 split the rest by RR (|${d2}-${d3}| <= 2)" "$([[ $(( d2 > d3 ? d2 - d3 : d3 - d2 )) -le 2 ]] && echo 1 || echo 0)"
assert "S8: counters: every selection is path=score_kv, no kv_fill (${d})" "$(counters_are "$d" 0 0 "$N_REQ" 0)"

echo "--- S9: getenv-once: an environment change without a restart has no effect"
# The only way to change the knob is to recreate the container (documented in doc 11 §3).
# An env change on a process started inside the running container is not seen by the
# gateway process; the distribution must stay the W=8 one.
docker exec -e LLB_PD_DECODE_KV_WEIGHT=0 llb1 sh -c 'echo "  (exec with LLB_PD_DECODE_KV_WEIGHT=$LLB_PD_DECODE_KV_WEIGHT in a side process)"'
assert "S9: the running gateway still has LLB_PD_DECODE_KV_WEIGHT=${KV_W_PRESSURE} (got '$(running_env LLB_PD_DECODE_KV_WEIGHT)')" \
    "$([[ "$(running_env LLB_PD_DECODE_KV_WEIGHT)" == "$KV_W_PRESSURE" ]] && echo 1 || echo 0)"
c0=$(snap)
drive s9 "${N_REQ}"
c1=$(snap); d=$(delta "$c0" "$c1")
assert "S9: distribution unchanged by the env attempt: D1 still 0 of ${N_REQ} (got $(cnt "${CFGDIR}/.dp-s9" D1))" \
    "$([[ "$(cnt "${CFGDIR}/.dp-s9" D1)" == 0 ]] && echo 1 || echo 0)"
assert "S9: counters: still path=score_kv for every selection (${d})" "$(counters_are "$d" 0 0 "$N_REQ" 0)"

reset_mocks

#################################################################################
# Teardown + sentinel
#################################################################################
echo ""
"${CFGDIR}/rmconfig.sh" >/dev/null 2>&1 || true
rm -f "${CFGDIR}"/.dp-* >/dev/null 2>&1 || true

if [[ "$code" == 0 ]]; then
    echo "=== SCENARIO-vllm-pd-decode-pressure-cpu [OK] ==="
else
    echo "=== SCENARIO-vllm-pd-decode-pressure-cpu [FAILED] ==="
fi
exit "$code"
