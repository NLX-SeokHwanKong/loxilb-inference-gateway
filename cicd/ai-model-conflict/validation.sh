#!/bin/bash
# ai-model-conflict validation.
#
# Contract under test (enforcing VIPs): ONE effective-model resolution,
# body-first, consumed by BOTH authorization and routing; body/header
# disagreement is a 400 model_conflict, never a silent steer.
#
# Born red: against the pre-fix binary T2 shows a llama-only key landing on
# the mistral pool (auth checked the body model, routing obeyed X-Model),
# and T3 shows routing acting on a header model auth never examined. Those
# failures are the proof this suite can detect the defect; do not soften an
# assert to make a run pass.
#
# T6 pins the OTHER half of the contract: non-enforcing VIPs keep the
# legacy header-first routing unchanged (green before AND after the fix).
cd "$(dirname "$0")"
source ../common.sh
echo SCENARIO-ai-model-conflict

PASS=0
FAIL=0
VIP=10.10.10.254

if [ ! -f .state ]; then
  echo "  FATAL: .state missing — run ./config.sh first"
  echo "SCENARIO-ai-model-conflict [FAILED]"
  exit 1
fi
# shellcheck disable=SC1091
source .state

chk_has() { # chk_has <name> <needle> <haystack>
  if [ "${3#*$2}" != "$3" ]; then
    echo "  [PASS] $1"; PASS=$((PASS + 1))
  else
    echo "  [FAIL] $1 — did not find '$2' in: $(echo "$3" | head -c 300)"; FAIL=$((FAIL + 1))
  fi
}
chk_not_has() { # chk_not_has <name> <forbidden> <haystack>
  if [ "${3#*$2}" = "$3" ]; then
    echo "  [PASS] $1"; PASS=$((PASS + 1))
  else
    echo "  [FAIL] $1 — found forbidden '$2' in: $(echo "$3" | head -c 300)"; FAIL=$((FAIL + 1))
  fi
}

chk_eq() { # chk_eq <name> <want> <got>
  if [ "$2" = "$3" ]; then
    echo "  [PASS] $1 ($3)"; PASS=$((PASS + 1))
  else
    echo "  [FAIL] $1 — want '$2', got '$3'"; FAIL=$((FAIL + 1))
  fi
}

# A refusal is one request in loxilb_ai_requests_total{outcome="denied"} with
# the status the client got, and one response in the class counter. The class
# counter is pulled every 10 s, so it is polled.
msum() { # msum <family> <label-substr> [label-substr] -> sum of matching samples
  $hexec l3h1 curl -s --max-time 8 "http://$VIP:11111/netlox/v1/metrics" 2>/dev/null \
    | awk -v fam="$1" -v a="$2" -v b="${3:-}" '
        $0 ~ "^" fam "[{ ]" && index($0, a) && (b == "" || index($0, b)) { s += $NF }
        END { printf "%.0f", s + 0 }'
}
denied() { msum loxilb_ai_requests_total 'outcome="denied"' "status=\"$1\""; }
completed() { msum loxilb_ai_requests_total 'outcome="completed"' "status=\"$1\""; }
klass() { msum loxilb_proxy_http_responses_by_status_total "status_class=\"$1\""; }
wait_delta() { # wait_delta <want> <before> <cmd...> -> the delta, once it reaches want or after 25 s
  local want=$1 b=$2 got; shift 2
  for _ in $(seq 1 25); do
    got=$(( $("$@") - b )); [ "$got" -ge "$want" ] && break; sleep 1
  done
  echo "$got"
}

body_llama='{"model":"llama-70b","messages":[{"role":"user","content":"hi"}]}'

req() { # req <port> <extra curl args...> — POST body_llama with the key
  local port=$1; shift
  $hexec l3h1 curl -s -i --max-time 8 -X POST \
    -H "Content-Type: application/json" \
    -H "X-Api-Key: $RAW_KEY" \
    "$@" \
    -d "$body_llama" \
    "http://$VIP:$port/v1/chat/completions"
}

echo ""
echo "T1: control — body model only, authorized → llama pool serves"
r=$(req 2030)
chk_has     "T1 llama pool answers"      "server-llama" "$r"
chk_not_has "T1 no conflict on agreement" "model_conflict" "$r"

den400=$(denied 400); cmp400=$(completed 400); k4=$(klass 4xx)
echo ""
echo "T2: body llama-70b + X-Model mistral-7b (authorized model in body ONLY)"
echo "    → 400 model_conflict; the llama-only key must never reach mistral"
r=$(req 2030 -H "X-Model: mistral-7b")
chk_has     "T2 400 status"          "400" "$(echo "$r" | head -1)"
chk_has     "T2 model_conflict body" "model_conflict" "$r"
chk_not_has "T2 mistral pool NOT reached (authz bypass)" "server-mistral" "$r"

echo ""
echo "T3: body llama-70b + X-Model ghost-model (no such pool)"
echo "    → 400 model_conflict (routing must not act on a model auth never checked)"
r=$(req 2030 -H "X-Model: ghost-model")
chk_has     "T3 400 status"          "400" "$(echo "$r" | head -1)"
chk_has     "T3 model_conflict body" "model_conflict" "$r"
chk_not_has "T3 no model_unavailable steer" "model_unavailable" "$r"
chk_eq "T3 requests_total{denied,400} moved by the two conflicts" 2 "$(wait_delta 2 "$den400" denied 400)"
chk_eq "T3 the 4xx class moved by two"                          2 "$(wait_delta 2 "$k4" klass 4xx)"
chk_eq "T3 and neither conflict counted as completed"            0 "$(( $(completed 400) - cmp400 ))"

echo ""
echo "T4: body llama-70b + X-Model llama-70b (both present, equal) → no false conflict"
r=$(req 2030 -H "X-Model: llama-70b")
chk_has     "T4 llama pool answers"  "server-llama" "$r"
chk_not_has "T4 agreement is not a conflict" "model_conflict" "$r"

den401=$(denied 401); cmp401=$(completed 401); k4=$(klass 4xx)
echo ""
echo "T5: no key on the enforcing VIP → 401 (conflict check never masks auth)"
r=$($hexec l3h1 curl -s -i --max-time 8 -X POST \
  -H "Content-Type: application/json" \
  -H "X-Model: mistral-7b" \
  -d "$body_llama" \
  "http://$VIP:2030/v1/chat/completions")
chk_has "T5 401 status" "401" "$(echo "$r" | head -1)"
chk_eq "T5 requests_total{denied,401} moved by one" 1 "$(wait_delta 1 "$den401" denied 401)"
chk_eq "T5 the 4xx class moved by one"              1 "$(wait_delta 1 "$k4" klass 4xx)"
chk_eq "T5 and not counted as completed"            0 "$(( $(completed 401) - cmp401 ))"

echo ""
echo "T6: non-enforcing VIP keeps LEGACY behavior — header steers, no 400"
echo "    (green before and after; enforcing-only is the deliberate scope)"
r=$($hexec l3h1 curl -s -i --max-time 8 -X POST \
  -H "Content-Type: application/json" \
  -H "X-Model: mistral-7b" \
  -d "$body_llama" \
  "http://$VIP:2031/v1/chat/completions")
chk_has     "T6 mistral pool answers (legacy header-first)" "server-mistral" "$r"
chk_not_has "T6 no conflict on none-mode"                   "model_conflict" "$r"

echo ""
if [ $FAIL -ne 0 ]; then
  echo "SCENARIO-ai-model-conflict [FAILED] ($PASS pass, $FAIL fail)"
  exit 1
fi
echo "SCENARIO-ai-model-conflict [OK] ($PASS pass)"
exit 0
