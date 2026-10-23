#!/bin/bash
# validate_cli.sh — CLI-driven validation for the JWT auth profile resource
# (cicd/ai-jwtauth), with loxicmd-inference-gateway as the subject-under-test
# and the REST API as the oracle: the profile is created through `loxicmd`
# inside the gateway container (llb1), read back through GET
# /config/ai/jwtauthprofile from l3h1, and cross-checked with
# `loxicmd get jwtauthprofile` (default and -o wide columns, -o json).
#
# This scenario runs the gateway WITHOUT --userservice: the CLI needs no token
# and the REST oracle sends no Authorization header.
#
# The gateway advertises jwt_auth_profile in /version, so the profile
# commands print no advisory line; --priority-class uses the objectives
# feature, which it does not advertise, so that one flag prints exactly one
# line on stderr — "warning: gateway does not advertise objectives; sending
# anyway, the gateway's answer decides" — and the exit code stays the
# gateway's answer.
#
# Checks:
#   J1 create: every field config.sh sets on its profiles (issuer, jwks_url,
#      audiences, leeway_sec, refresh_sec, forward_identity,
#      authorization_passthrough) plus --priority-class critical; the REST
#      read-back must equal the read-back of the same profile posted with curl.
#   J2 get: the default table (Name, Issuer, Tenant Claim, Priority), the -o
#      wide columns, and -o json against the REST entry.
#   J3 delete in use: the scenario's own rule on :2040 references kc, and a
#      rule created through loxicmd references the CLI profile: both deletes
#      are refused 409 (exit 4), the profiles stay listed.
#   J4 delete: the rule removed, the profile deleted, gone from the REST list;
#      a second delete is 404 (exit 4), the list unchanged.
#
# Runs AFTER the matrix in validation.sh; folded into its exit code. Gated by
# jwtauth_cli_preflight (CLI_TESTS=auto|required|skip). A failed check is
# recorded and the run continues, so one run reports every check; the exit
# code is 1 when any check failed.

source ../common.sh
if [ ! -f .state ]; then
  echo "  FATAL: .state missing — run ./config.sh first"; exit 1
fi
source .state
KC_JWKS="$KC_ISSUER/protocol/openid-connect/certs"

GW=llb1
VIP=10.10.10.254
API="http://$VIP:11111/netlox/v1"
CLI_OUT=${CLI_OUT:-$(pwd)/artifacts-cli}
rm -rf "$CLI_OUT"; mkdir -p "$CLI_OUT"
T=$CLI_OUT/tmp; mkdir -p "$T"
PROF=kc-cli
PORT_CLI=2099   # a VIP port no rule of config.sh uses

echo ""
echo "========================================="
echo " CLI Validation (JWT auth profiles)"
echo "========================================="

# ── Preflight: does the packaged loxicmd carry the profile resource? ─────────
if ! jwtauth_cli_preflight $GW; then
  exit 0
fi

code=0
ok()  { echo "  $1 [OK]"; }
bad() { echo "  $1 [FAIL] — $2"; code=1; }
check_eq() {
  local label="$1" want="$2" got="$3"
  if [[ "$got" == "$want" ]]; then ok "$label ($got)"; else bad "$label" "expected '$want', got '$got'"; fi
}
check_has() {
  local label="$1" want="$2" got="$3"
  if [[ "$got" == *"$want"* ]]; then ok "$label"; else bad "$label" "expected '$want' in: $(echo "$got" | tr '\n' ' ' | cut -c1-300)"; fi
}
check_same() {
  local label="$1" a="$2" b="$3"
  if [[ "$a" == "$b" ]]; then ok "$label"; else
    bad "$label" "the two read-backs differ:"
    diff <(echo "$a" | jq . 2>/dev/null || echo "$a") <(echo "$b" | jq . 2>/dev/null || echo "$b") | sed 's/^/      /'
  fi
}

# cli <args...>: loxicmd inside the gateway container; stdout and stderr kept
# apart, the exit code in CLI_RC (also returned); every command logged.
cli() {
  $dexec $GW loxicmd "$@" > "$T/stdout" 2> "$T/stderr"
  CLI_RC=$?
  CLI_STDOUT=$(cat "$T/stdout"); CLI_STDERR=$(cat "$T/stderr")
  echo "loxicmd $* -> exit $CLI_RC" >> "$CLI_OUT/commands.log"
  return $CLI_RC
}
# warn_check <label> <feature|none>: the advisory line on stderr only, once,
# for a feature the gateway does not advertise; none otherwise.
warn_check() {
  local label=$1 feat=$2 ne no
  ne=$(grep -c '^warning: gateway does not advertise' "$T/stderr")
  no=$(grep -c 'warning: gateway does not advertise' "$T/stdout")
  if [[ "$feat" == none ]]; then
    check_eq "$label: no advisory line (stderr/stdout)" "0/0" "$ne/$no"
  else
    check_eq "$label: one advisory line, on stderr only (stderr/stdout)" "1/0" "$ne/$no"
    check_has "$label: it names $feat and defers to the gateway" \
      "warning: gateway does not advertise $feat; sending anyway, the gateway's answer decides" "$CLI_STDERR"
  fi
}

# ── REST oracle ─────────────────────────────────────────────────────────────
rest() { $hexec l3h1 curl -s -m 8 "$API$1"; }
# prof_rest <name>: the profile's entry as REST lists it; empty when absent
prof_rest() { rest /config/ai/jwtauthprofile | jq -S -c "[.jwtAuthProfileAttr[] | select(.name==\"$1\")] | first // empty"; }
# norm: empty values dropped at every level, for a document the CLI
# re-marshals through its omitempty struct against the gateway's own
norm() { jq -S -c 'walk(if type == "object" then with_entries(select(.value != null and .value != "" and .value != [] and .value != false and .value != 0)) else . end)'; }
rule_rest() { rest /config/loadbalancer/all | jq -c ".lbAttr[] | select(.serviceArguments.port==$1) | .serviceArguments"; }

PROFILES0=$(rest /config/ai/jwtauthprofile | jq -r '[.jwtAuthProfileAttr[].name] | sort | join(" ")')

# ── J1: create through the CLI, equal to the same profile posted with curl ──
echo ""
echo "J1: loxicmd create jwtauthprofile $PROF with every field config.sh sets, plus --priority-class=critical"
# The curl twin: config.sh's add_profile document with the fields its
# variants add (forward_identity, authorization_passthrough) and the cap.
twin_code=$($hexec l3h1 curl -s -o "$T/twin.out" -w '%{http_code}' -m 8 -X POST "$API/config/ai/jwtauthprofile" \
  -H 'Content-Type: application/json' -d '{
    "name": "'"$PROF-curl"'", "issuer": "'"$KC_ISSUER"'", "jwks_url": "'"$KC_JWKS"'", "audiences": ["aigw-api"],
    "leeway_sec": 2, "refresh_sec": 300, "forward_identity": true, "authorization_passthrough": true,
    "priority_class": "critical"}')
check_eq "J1 the curl twin $PROF-curl is accepted" 200 "$twin_code"
cli create jwtauthprofile $PROF --issuer="$KC_ISSUER" --jwks-url="$KC_JWKS" --audiences=aigw-api \
  --leeway-sec=2 --refresh-sec=300 --forward-identity=true --authorization-passthrough=true --priority-class=critical
check_eq "J1 loxicmd create jwtauthprofile (exit)" 0 "$CLI_RC"; warn_check "J1 create with --priority-class" objectives
echo "$CLI_STDOUT" | sed 's/^/      /'
A=$(prof_rest $PROF-curl | jq -S -c 'del(.name)'); B=$(prof_rest $PROF | jq -S -c 'del(.name)')
echo "$A" > "$CLI_OUT/j1-curl.json"; echo "$B" > "$CLI_OUT/j1-cli.json"
check_eq "J1 REST lists the CLI profile" "$PROF" "$(prof_rest $PROF | jq -r .name)"
check_same "J1 the CLI profile reads back as the curl one, field by field (name apart)" "$A" "$B"
check_eq "J1 the fields as declared (issuer jwks audiences leeway refresh fwd passthrough class)" \
  "$KC_ISSUER $KC_JWKS aigw-api 2 300 true true critical" \
  "$(prof_rest $PROF | jq -r '[.issuer, .jwks_url, (.audiences|join(",")), .leeway_sec, .refresh_sec, .forward_identity, .authorization_passthrough, .priority_class] | map(tostring) | join(" ")')"
del_code=$($hexec l3h1 curl -s -o /dev/null -w '%{http_code}' -m 8 -X DELETE "$API/config/ai/jwtauthprofile/$PROF-curl")
check_eq "J1 the curl twin removed again" 200 "$del_code"

# ── J2: get — the default table, -o wide, -o json ───────────────────────────
echo ""
echo "J2: loxicmd get jwtauthprofile (default, -o wide, -o json) against the REST entry"
cli get jwtauthprofile; check_eq "J2 get jwtauthprofile (exit)" 0 "$CLI_RC"; warn_check "J2 get jwtauthprofile" none
echo "$CLI_STDOUT" > "$CLI_OUT/j2-table.txt"
HDR=$(echo "$CLI_STDOUT" | grep -i -m1 'NAME')
check_has "J2 the default columns: Name, Issuer, Tenant Claim, Priority" "TENANT CLAIM" "$(echo "$HDR" | tr '[:lower:]' '[:upper:]')"
ROW=$(echo "$CLI_STDOUT" | grep -E "[| ]$PROF[| ]" | head -1); echo "      $ROW"
check_has "J2 the row names the issuer" "$KC_ISSUER" "$ROW"
check_has "J2 the row shows the priority class" "critical" "$ROW"
check_has "J2 the row lists every profile config.sh made (kc)" "kc" "$(echo "$CLI_STDOUT" | grep -E '[| ]kc[| ]')"
cli get jwtauthprofile -o wide; check_eq "J2 get jwtauthprofile -o wide (exit)" 0 "$CLI_RC"
echo "$CLI_STDOUT" > "$CLI_OUT/j2-wide.txt"
HDR=$(echo "$CLI_STDOUT" | grep -i -m1 'NAME' | tr '[:lower:]' '[:upper:]')
for col in "JWKS URL" "AUDIENCES" "ALGS" "LEEWAY(S)" "REFRESH(S)" "USER CLAIM" "MODELS CLAIM" "ROLES CLAIM" "MODEL ROLE PREFIX" "USERNAME CLAIM" "MODEL AUTHZ" "DEFAULT TENANT" "FORWARD IDENTITY" "AUTHZ PASSTHROUGH"; do
  check_has "J2 -o wide column: $col" "$col" "$HDR"
done
ROW=$(echo "$CLI_STDOUT" | grep -E "[| ]$PROF[| ]" | head -1); echo "      $ROW"
check_has "J2 -o wide: the JWKS URL" "$KC_JWKS" "$ROW"
check_has "J2 -o wide: the audience" "aigw-api" "$ROW"
RW=$(echo "$ROW" | sed 's/|/ /g' | tr -s ' ')
check_has "J2 -o wide: leeway 2" " 2 " "$RW"; check_has "J2 -o wide: refresh 300" " 300 " "$RW"
check_eq "J2 -o wide: forward identity and passthrough true (the last two cells)" "true true" "$(echo "$RW" | awk '{print $(NF-1), $NF}')"
cli get jwtauthprofile $PROF -o json; check_eq "J2 get jwtauthprofile $PROF -o json (exit)" 0 "$CLI_RC"
check_same "J2 -o json equals the REST entry" "$(prof_rest $PROF | norm)" \
  "$(echo "$CLI_STDOUT" | jq -c "if .jwtAuthProfileAttr then (.jwtAuthProfileAttr[] | select(.name==\"$PROF\")) else . end" 2>/dev/null | norm)"

# ── J3: a profile a rule references is not deletable (409) ──────────────────
echo ""
echo "J3: delete in use — the scenario's rule on :2040 references kc; a CLI rule on :$PORT_CLI references $PROF"
check_eq "J3 the scenario's rule on :2040 references kc" "kc" "$(rule_rest 2040 | head -1 | jq -r .jwt_auth_profile)"
cli delete jwtauthprofile kc
echo "      stderr: $CLI_STDERR"
check_eq "J3 delete jwtauthprofile kc: 409 is exit 4 (PRECONDITION)" 4 "$CLI_RC"; warn_check "J3 delete kc" none
check_has "J3 ... and says 409" "409" "$CLI_STDERR"
check_eq "J3 kc is still listed" "kc" "$(prof_rest kc | jq -r .name)"
cli create lb $VIP --tcp=$PORT_CLI:8080 --mode=fullproxy --endpoints=31.31.31.1:1 --host=$VIP --path-prefix=/ --path-match-mode=prefix \
  --model-name=llama-70b --api-key-auth=jwt --jwt-auth-profile=$PROF --inatimeout=30
check_eq "J3 loxicmd create lb :$PORT_CLI --api-key-auth=jwt --jwt-auth-profile=$PROF (exit)" 0 "$CLI_RC"; warn_check "J3 create lb" none
check_eq "J3 REST reads the rule with the profile" "jwt $PROF" "$(rule_rest $PORT_CLI | jq -r '.api_key_auth + " " + .jwt_auth_profile')"
cli delete jwtauthprofile $PROF
echo "      stderr: $CLI_STDERR"
check_eq "J3 delete jwtauthprofile $PROF while referenced: exit 4" 4 "$CLI_RC"
check_has "J3 ... and says 409" "409" "$CLI_STDERR"
check_eq "J3 $PROF is still listed" "$PROF" "$(prof_rest $PROF | jq -r .name)"

# ── J4: the rule removed, the profile deleted, 404 on the second delete ─────
echo ""
echo "J4: delete, then a second delete"
cli delete lb $VIP --tcp=$PORT_CLI --host=$VIP --path-prefix=/ --path-match-mode=prefix --model-name=llama-70b
check_eq "J4 loxicmd delete lb :$PORT_CLI by the full L7 key (exit)" 0 "$CLI_RC"
check_eq "J4 the rule is gone" "" "$(rule_rest $PORT_CLI)"
cli delete jwtauthprofile $PROF; check_eq "J4 delete jwtauthprofile $PROF (exit)" 0 "$CLI_RC"; warn_check "J4 delete" none
echo "$CLI_STDOUT" | sed 's/^/      /'
check_eq "J4 gone from the REST list" "" "$(prof_rest $PROF)"
cli delete jwtauthprofile $PROF
echo "      stderr: $CLI_STDERR"
check_eq "J4 a second delete: 404 is exit 4 (PRECONDITION)" 4 "$CLI_RC"
check_has "J4 ... and says 404" "404" "$CLI_STDERR"
check_eq "J4 still absent" "" "$(prof_rest $PROF)"
check_eq "J4 the profiles config.sh made are as they were" "$PROFILES0" "$(rest /config/ai/jwtauthprofile | jq -r '[.jwtAuthProfileAttr[].name] | sort | join(" ")')"

# ── Summary ─────────────────────────────────────────────────────────────────
echo ""
if [[ $code == 0 ]]; then
  echo "=== CLI Validation (JWT auth profiles): all J1-J4 checks passed ==="
else
  echo "=== CLI Validation (JWT auth profiles): FAILED (see [FAIL] lines above; evidence in $CLI_OUT) ==="
fi
exit $code
