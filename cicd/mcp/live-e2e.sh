#!/bin/bash
# live-e2e.sh - On-demand E2E validation of the loxilb-mcp bridge against a
# LIVE loxilb-inference-gateway target (no docker testbed, no LLM required).
#
# Unlike validation.sh (which stands up a docker testbed), this drives the
# bridge over a single persistent stdio JSON-RPC session against an already-
# running loxilb REST endpoint, so you can point it at a real deployment on
# demand:
#
#     ./live-e2e.sh                                 # default target, read-only
#     TARGET=http://1.2.3.4:11111 ./live-e2e.sh     # any live target
#     ./live-e2e.sh --mutate                        # + control-plane round-trip
#     TARGET_USER=admin TARGET_PASSWORD=... ./live-e2e.sh   # --userservice target
#
# Read-only mode (default) mutates NOTHING on the target. --mutate additionally
# runs an isolated lb_create -> lb_list -> lb_update (admin_state_up down and
# back, through the preview's rollback) -> confirm-token lb_delete round-trip
# on a TEST-NET-2 VIP (RFC 5737 198.51.100.0/24) that will not collide with
# real services, and cleans it up (including on failure), plus a secrate_set
# preview -> confirm -> read-back -> rollback round-trip that leaves the
# security-rate config as it found it (secrate_disable when it found none).
# The two-step confirm-token flow is why the harness holds one session open:
# the token is in-memory, single-use, per process. The bridge runs from a
# config file written here, with a mutation budget sized for the round-trip
# (the default of 6 per minute is for an agent, not a harness).
#
# Exit code 0 = all checks passed.
set -u

TARGET="${TARGET:-http://127.0.0.1:11111}"   # override with TARGET=http://<host>:11111
MUTATE=0
export VIP="${VIP:-198.51.100.7}"   # RFC 5737 TEST-NET-2, safe throwaway VIP
export VPORT="${VPORT:-19999}"
export VEP="${VEP:-198.51.100.8}"
# JWT_PROFILE names an existing JWT auth profile on the target; set, the
# --mutate round-trip adds an api_key_auth=jwt rule. Since #362 a full-proxy
# rule needs a VIP the target holds, so JWT_VIP must be such an address.
export JWT_PROFILE="${JWT_PROFILE:-}"
export JWT_VIP="${JWT_VIP:-$VIP}"

for a in "$@"; do
    case "$a" in
        --mutate) MUTATE=1 ;;
        -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
        *) echo "unknown arg: $a" >&2; exit 2 ;;
    esac
done

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
WORK=$(mktemp -d)
export BIN="$WORK/loxilb-mcp"      # built into the temp dir; nothing left behind
export AUDIT="$WORK/audit"
export TARGET MUTATE
trap 'rm -rf "$WORK"' EXIT

echo "=== loxilb-mcp live E2E ==="
echo "target : $TARGET"
echo "mode   : $([[ $MUTATE == 1 ]] && echo 'read-only + mutate round-trip' || echo 'read-only')"

echo "building loxilb-mcp"
if ! (cd "$root/mcp" && go build -o "$BIN" ./cmd/loxilb-mcp); then
    echo "=== live E2E [FAILED] (build) ==="; exit 1
fi

# The bridge's config: one target (with the --userservice credentials when
# TARGET_USER is set; the password travels through the environment, never
# the file) and a mutation budget sized for the round-trip.
export CFG="$WORK/live-e2e.yaml"
{
    echo "default_target: live"
    echo "targets:"
    echo "  live:"
    echo "    url: $TARGET"
    if [[ -n "${TARGET_USER:-}" ]]; then
        echo "    username: $TARGET_USER"
        echo "    password_env: TARGET_PASSWORD"
    fi
    echo "audit_dir: $AUDIT"
    echo "change_budget: { mutations_per_minute: 60 }"
} > "$CFG"
chmod 0600 "$CFG"
export TARGET_USER="${TARGET_USER:-}" TARGET_PASSWORD="${TARGET_PASSWORD:-}"

exec python3 - <<'PY'
import itertools, json, os, subprocess, sys, urllib.request

TARGET = os.environ["TARGET"]
BIN    = os.environ["BIN"]
AUDIT  = os.environ["AUDIT"]
CFG    = os.environ["CFG"]
MUTATE = os.environ.get("MUTATE") == "1"
VIP, VPORT, VEP = os.environ["VIP"], int(os.environ["VPORT"]), os.environ["VEP"]

code = 0
def ok(m):   print(f"  [OK] {m}")
def bad(m):
    global code; code = 1; print(f"  [FAILED] {m}")

class Session:
    """One persistent stdio JSON-RPC session against the bridge."""
    def __init__(self, role):
        self.p = subprocess.Popen(
            [BIN, "--config", CFG, "--role", role, "--audit-dir", AUDIT],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, bufsize=1)
        self.ids = itertools.count(1)
        self._rpc("initialize", params={
            "protocolVersion": "2025-06-18", "capabilities": {},
            "clientInfo": {"name": "live-e2e", "version": "0"}})
        self._note("notifications/initialized")

    def _send(self, obj):
        self.p.stdin.write(json.dumps(obj) + "\n"); self.p.stdin.flush()

    def _note(self, method, **kw):
        self._send({"jsonrpc": "2.0", "method": method, **kw})

    def _rpc(self, method, **kw):
        i = next(self.ids)
        self._send({"jsonrpc": "2.0", "id": i, "method": method, **kw})
        for line in self.p.stdout:
            line = line.strip()
            if not line:
                continue
            o = json.loads(line)
            if o.get("id") == i:
                return o
        return {}

    def list_tools(self):
        return self._rpc("tools/list").get("result", {}).get("tools", [])

    def call(self, tool, **args):
        r = self._rpc("tools/call", params={"name": tool, "arguments": args})
        if "error" in r:
            return True, r["error"], r
        res = r.get("result", {})
        c = res.get("content", [])
        t = c[0].get("text", "") if c else ""
        try:
            return res.get("isError") is True, json.loads(t), r
        except Exception:
            return res.get("isError") is True, t, r

    def close(self):
        try:
            self.p.stdin.close(); self.p.wait(timeout=5)
        except Exception:
            self.p.kill()

def rest_headers():
    """The bearer for a --userservice target (TARGET_USER set), else none."""
    user, pw = os.environ.get("TARGET_USER", ""), os.environ.get("TARGET_PASSWORD", "")
    if not user:
        return {}
    try:
        req = urllib.request.Request(TARGET + "/netlox/v1/auth/login", method="POST",
                                     data=json.dumps({"username": user, "password": pw}).encode(),
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=8) as r:
            return {"Authorization": "Bearer " + json.load(r).get("token", "")}
    except Exception:
        return {}

def rest_lb_count():
    try:
        req = urllib.request.Request(TARGET + "/netlox/v1/config/loadbalancer/all", headers=rest_headers())
        with urllib.request.urlopen(req, timeout=8) as r:
            return len(json.load(r).get("lbAttr", []))
    except Exception:
        return None

# ---------------- read-only checks (admin) ----------------
print("### read-only checks (admin role)")
s = Session("admin")
tools = s.list_tools()
(ok if len(tools) > 40 else bad)(f"tools/list -> {len(tools)} tools")

e, v, _ = s.call("version_get")
(ok if isinstance(v, dict) and v.get("version") else bad)(f"version_get -> {v.get('version') if isinstance(v, dict) else v}")

e, v, _ = s.call("health_overview")
reachable = isinstance(v, dict) and v.get("reachable") is True
(ok if reachable else bad)("health_overview reachable")
health_lb = v.get("lb_rule_count") if isinstance(v, dict) else None

e, v, _ = s.call("lb_list")
lblist = v.get("returned") if isinstance(v, dict) else None
rest = rest_lb_count()
if rest is not None:
    (ok if lblist == rest == health_lb else bad)(
        f"lb_list==REST==health ({rest} rules)" if lblist == rest == health_lb
        else f"lb count mismatch lb_list={lblist} health={health_lb} REST={rest}")
else:
    (ok if lblist == health_lb and lblist is not None else bad)(
        f"lb_list==health ({lblist} rules; REST cross-check unavailable)")

e, v, _ = s.call("metrics_snapshot", families=["loxilb_lb_rules"])
fam = v.get("family_count") if isinstance(v, dict) else 0
(ok if fam and fam >= 1 else bad)(f"metrics_snapshot family_count={fam}")

e, v, _ = s.call("targets_list")
(ok if isinstance(v, dict) and v.get("count", 0) >= 1 else bad)("targets_list non-empty")

e, v, _ = s.call("fleet_overview")
(ok if isinstance(v, dict) and v.get("reachable", 0) >= 1 else bad)("fleet_overview reachable")

e, v, _ = s.call("diagnose_l4_errors")
(ok if isinstance(v, dict) and "evidence" in v else bad)("diagnose_l4_errors evidence bundle")

e, v, _ = s.call("ai_traffic_report")
# The caveat is identified by its text; the internal finding label it once
# carried left the tool in 2026-08.
has_caveat = isinstance(v, dict) and any("accounting caveat" in c for c in v.get("caveats", []))
(ok if has_caveat else bad)("ai_traffic_report surfaces the accounting caveat")

# ---------------- control-plane reads (PR-B1) ----------------
e, v, _ = s.call("lb_list")
first = (v.get("rules") or [{}])[0] if isinstance(v, dict) else {}
if first.get("external_ip"):
    e, v, _ = s.call("lb_get", external_ip=first["external_ip"], port=first["port"], protocol=first["protocol"], include=["status", "stats"])
    (ok if isinstance(v, dict) and v.get("rule", {}).get("external_ip") == first["external_ip"] and "operatingStatus" in v.get("status", {}) and "totalConnections" in v.get("stats", {}) else bad)(
        f"lb_get {first['external_ip']}:{first['port']}/{first['protocol']} with status and stats")
else:
    print("  [SKIP] lb_get (the target holds no rule)")
e, v, _ = s.call("status_capabilities")
(ok if isinstance(v, dict) and v.get("count", 0) >= 1 else bad)(f"status_capabilities -> {v.get('count') if isinstance(v, dict) else v} capabilities, not ready: {v.get('not_ready') if isinstance(v, dict) else '?'}")
e, v, _ = s.call("maintenance_get")
(ok if isinstance(v, dict) and v.get("state") else bad)(f"maintenance_get -> {v.get('state') if isinstance(v, dict) else v}")
e, v, _ = s.call("halfclose_get")
(ok if isinstance(v, dict) and "allow" in v else bad)(f"halfclose_get -> allow={v.get('allow') if isinstance(v, dict) else v} capSeconds={v.get('capSeconds') if isinstance(v, dict) else '?'}")
e, v, _ = s.call("config_snapshot_get")
(ok if isinstance(v, dict) and v.get("truncated") is False and v.get("size_bytes", 0) > 0 else bad)(f"config_snapshot_get -> {v.get('size_bytes') if isinstance(v, dict) else v} bytes, sha256 {str(v.get('sha256', ''))[:12] if isinstance(v, dict) else ''}")

# Stage-2 arm (read-only): KV_RULE=<vip>:<port>[/<proto>] names a KV-exact
# rule on the target (the GPU fleet); KV_MODEL narrows it to one model.
kv_rule = os.environ.get("KV_RULE", "")
if kv_rule:
    hp, _, proto = kv_rule.partition("/")
    ip, _, port = hp.rpartition(":")
    args = {"external_ip": ip, "port": int(port), "protocol": proto or "tcp"}
    if os.environ.get("KV_MODEL"):
        args["model_name"] = os.environ["KV_MODEL"]
    e, v, _ = s.call("lb_kvexact_status", **args)
    ent = (v.get("entries") or [{}])[0] if isinstance(v, dict) else {}
    (ok if isinstance(v, dict) and v.get("count", 0) >= 1 and ent.get("enforcedState") else bad)(
        f"lb_kvexact_status {kv_rule} -> {v.get('count') if isinstance(v, dict) else v} entries; desired={ent.get('desiredState')} enforced={ent.get('enforcedState')} reasons={ent.get('reasonCodes')}")
    e, v, _ = s.call("lb_get", external_ip=ip, port=int(port), protocol=proto or "tcp", include=["status"])
    st = v.get("status", {}) if isinstance(v, dict) else {}
    (ok if st.get("operatingStatus") else bad)(f"lb_get {kv_rule} include=status -> adminStateUp={st.get('adminStateUp')} operatingStatus={st.get('operatingStatus')}")
else:
    print("  [SKIP] KV-exact status arm (set KV_RULE=<vip>:<port> to a KV-exact rule on the target)")

# ---------------- security guardrails ----------------
print("### security guardrails")
e, v, _ = s.call("lb_list", target=TARGET)      # URL as target name
(ok if e else bad)("URL-as-target rejected (anti-SSRF)")
e, v, _ = s.call("lb_list", target="no-such-target")
(ok if e else bad)("unknown target rejected")
e, v, r = s.call("does_not_exist")
(ok if "error" in r else bad)("unknown tool -> JSON-RPC error")
s.close()

# viewer role must not even see mutating tools
sv = Session("viewer")
vtools = [t["name"] for t in sv.list_tools()]
sv.close()
(ok if vtools and "lb_create" not in vtools else bad)(
    f"viewer role: no lb_create ({len(vtools)} read tools)")

# ---------------- optional mutation round-trip ----------------
if MUTATE:
    print(f"### mutation round-trip (VIP {VIP}:{VPORT} -> {VEP})")
    m = Session("admin")
    created = False
    try:
        e, v, _ = m.call("lb_create", external_ip=VIP, port=VPORT, protocol="tcp",
                         endpoints=[{"ip": VEP, "port": 80}])
        created = isinstance(v, dict) and v.get("action") == "executed"
        (ok if created else bad)("lb_create executed" if created else f"lb_create: {v}")

        e, v, _ = m.call("lb_list")
        present = isinstance(v, dict) and any(r.get("external_ip") == VIP for r in v.get("rules", []))
        (ok if present else bad)("new VIP present in lb_list")

        # lb_update round-trip (PR-B2): admin_state_up false previews with
        # an lb_update rollback, applies with the token and reads the rule
        # and its status back; the rollback puts it up again.
        if created:
            key = {"external_ip": VIP, "port": VPORT, "protocol": "tcp"}
            e, v, _ = m.call("lb_update", admin_state_up=False, **key)
            tok = v.get("confirm_token") if isinstance(v, dict) else None
            rb = v.get("rollback", {}) if isinstance(v, dict) else {}
            (ok if tok and v.get("impact") == "service" and rb.get("tool") == "lb_update" and rb.get("args", {}).get("admin_state_up") is True else bad)(
                "lb_update admin_state_up=false previews with an lb_update rollback" if tok else f"lb_update preview: {v}")
            e, v, _ = m.call("lb_update", admin_state_up=False, confirm_token=tok, **key)
            rbk = v.get("read_back", {}) if isinstance(v, dict) else {}
            down = isinstance(v, dict) and v.get("action") == "executed" and \
                rbk.get("admin_state_up") is False and "operatingStatus" in rbk.get("status", {})
            (ok if down else bad)(f"lb_update applied; read_back shows admin_state_up false, status {rbk.get('status', {}).get('operatingStatus')}" if down else f"lb_update apply: {v}")
            # D24: on the paused rule, lb_get reports the flag as read; the
            # gateway's GET carries no adminStateUp (omitempty).
            e, v, _ = m.call("lb_get", **key)
            got = (v.get("rule", {}) if isinstance(v, dict) else {}).get("admin_state_up")
            (ok if got == "absent" else bad)(f"lb_get on the paused rule reports admin_state_up {got!r}")
            e, v, _ = m.call("lb_update", **rb.get("args", dict(admin_state_up=True, **key)))
            tok = v.get("confirm_token") if isinstance(v, dict) else None
            e, v, _ = m.call("lb_update", confirm_token=tok, **rb.get("args", dict(admin_state_up=True, **key)))
            up = isinstance(v, dict) and v.get("action") == "executed" and v.get("read_back", {}).get("admin_state_up") is True
            (ok if up else bad)("rollback applied; read_back shows admin_state_up true" if up else f"lb_update rollback: {v}")

        e, v, _ = m.call("lb_delete", external_ip=VIP, port=VPORT, protocol="tcp")
        tok = v.get("confirm_token") if isinstance(v, dict) else None
        (ok if isinstance(v, dict) and v.get("action") == "preview" and tok else bad)(
            "lb_delete preview returns confirm_token")

        e, v, _ = m.call("lb_list")
        still = isinstance(v, dict) and any(r.get("external_ip") == VIP for r in v.get("rules", []))
        (ok if still else bad)("VIP still present after preview (no mutation)")

        e, v, _ = m.call("lb_delete", external_ip=VIP, port=VPORT, protocol="tcp",
                         confirm_token=tok)
        done = isinstance(v, dict) and v.get("action") == "executed"
        (ok if done else bad)("lb_delete executed with token")
        created = created and not done

        e, v, _ = m.call("lb_list")
        gone = isinstance(v, dict) and not any(r.get("external_ip") == VIP for r in v.get("rules", []))
        (ok if gone else bad)("VIP gone after delete")

        # secrate_set change gate: preview -> confirm -> read-back -> rollback.
        # SYN protection is switched on with the threshold the preview read
        # raised by one (above the cookie threshold), so the change is real
        # but harmless. A target that had no protection enabled cannot be put
        # back through secrate_set (POST /config/securityrate refuses an
        # all-off body): the preview then carries a secrate_disable rollback
        # and the restore goes through that tool. Either way the config is
        # left as it was found.
        e, v, _ = m.call("secrate_set", syn_enabled=True, syn_threshold=1)
        cur = v.get("current", {}) if isinstance(v, dict) else {}
        base = cur.get("synThreshold")
        was_on = any(cur.get(k) is True for k in ("synEnabled", "connRateEnabled", "udpEnabled")) or \
            any((cur.get(k) or 0) > 0 for k in ("aggregateSynThreshold", "aggregateConnRatePerSec",
                                                "aggregateUdpPktThreshold", "aggregateUdpBandwidthMB"))
        (ok if isinstance(v, dict) and v.get("action") == "preview" and isinstance(base, int) else bad)(
            f"secrate_set previews the current config (synThreshold {base}, protection {'on' if was_on else 'off'})")
        if isinstance(base, int):
            want = max(base, cur.get("cookieThreshold") or 0) + 1
            change = {"syn_enabled": True, "syn_threshold": want}
            e, v, _ = m.call("secrate_set", **change)
            tok = v.get("confirm_token") if isinstance(v, dict) else None
            rb = v.get("rollback", {}) if isinstance(v, dict) else {}
            if was_on:
                (ok if tok and rb.get("tool") == "secrate_set" and rb.get("args", {}).get("syn_threshold") == base else bad)(
                    "secrate_set preview carries a token and a rollback to the current value")
            else:
                (ok if tok and rb.get("tool") == "secrate_disable" and "DELETE /config/securityrate" in rb.get("note", "") else bad)(
                    "secrate_set preview carries a token and a secrate_disable rollback (no protection was on)")
            e, v, _ = m.call("secrate_get")
            attr = (v.get("securityrateAttr") or [{}])[0] if isinstance(v, dict) else {}
            (ok if (attr.get("synThreshold") or 0) == base else bad)("preview changed nothing (secrate_get)")
            e, v, _ = m.call("secrate_set", confirm_token=tok, **change)
            applied = isinstance(v, dict) and v.get("action") == "executed" and v.get("read_back", {}).get("synThreshold") == want
            (ok if applied else bad)("secrate_set applied with the token; read_back shows the new threshold" if applied else f"secrate_set apply: {v}")
            if was_on:
                e, v, _ = m.call("secrate_set", **rb.get("args", {"syn_threshold": base}))
                tok = v.get("confirm_token") if isinstance(v, dict) else None
                e, v, _ = m.call("secrate_set", confirm_token=tok, **rb.get("args", {"syn_threshold": base}))
                restored = isinstance(v, dict) and v.get("action") == "executed" and v.get("read_back", {}).get("synThreshold") == base
                (ok if restored else bad)("rollback applied; read_back shows the original threshold" if restored else f"rollback: {v}")
            else:
                e, v, _ = m.call("secrate_disable")
                tok = v.get("confirm_token") if isinstance(v, dict) else None
                drb = v.get("rollback", {}) if isinstance(v, dict) else {}
                (ok if tok and v.get("impact") == "global" and drb.get("tool") == "secrate_set" and drb.get("args", {}).get("syn_threshold") == want else bad)(
                    "secrate_disable previews the all-off state with a secrate_set rollback carrying the configuration" if tok else f"secrate_disable preview: {v}")
                e, v, _ = m.call("secrate_disable", confirm_token=tok)
                e2, g, _ = m.call("secrate_get")
                attr = (g.get("securityrateAttr") or [{}])[0] if isinstance(g, dict) else {}
                restored = isinstance(v, dict) and v.get("action") == "executed" and v.get("read_back", {}).get("synEnabled") is False and attr.get("synEnabled") is not True
                (ok if restored else bad)("secrate_disable applied with the token; the unconfigured state is back" if restored else f"secrate_disable apply: {v}, {attr}")

        # api_key_auth=jwt arm: needs a JWT auth profile that already exists
        # on the target (the bridge gains the profile tool in PR-B3). Name it
        # in JWT_PROFILE; unset, the arm is reported as skipped, not passed.
        prof = os.environ.get("JWT_PROFILE", "")
        JVIP = os.environ.get("JWT_VIP", VIP)
        if prof:
            e, v, _ = m.call("lb_create", external_ip=JVIP, port=VPORT, protocol="tcp", mode=4,
                             host="jwt.live-e2e", api_key_auth="jwt",
                             endpoints=[{"ip": VEP, "port": 80}])
            (ok if e else bad)("lb_create api_key_auth=jwt without jwt_auth_profile is refused")
            e, v, _ = m.call("lb_create", external_ip=JVIP, port=VPORT, protocol="tcp", mode=4,
                             host="jwt.live-e2e", api_key_auth="jwt", jwt_auth_profile=prof,
                             endpoints=[{"ip": VEP, "port": 80}])
            jcreated = isinstance(v, dict) and v.get("action") == "executed"
            created = created or jcreated
            (ok if jcreated else bad)("lb_create api_key_auth=jwt executed" if jcreated else f"jwt lb_create: {v}")
            if jcreated:
                e, v, _ = m.call("lb_list", filter=JVIP)
                rules = v.get("rules", []) if isinstance(v, dict) else []
                seen = any(r.get("api_key_auth") == "jwt" and r.get("jwt_auth_profile") == prof for r in rules)
                (ok if seen else bad)("lb_list observes api_key_auth=jwt and jwt_auth_profile")
                e, v, _ = m.call("lb_delete", external_ip=JVIP, port=VPORT, protocol="tcp", host_url="jwt.live-e2e")
                tok = v.get("confirm_token") if isinstance(v, dict) else None
                e, v, _ = m.call("lb_delete", external_ip=JVIP, port=VPORT, protocol="tcp",
                                 host_url="jwt.live-e2e", confirm_token=tok)
                jdone = isinstance(v, dict) and v.get("action") == "executed"
                (ok if jdone else bad)("jwt rule deleted with token")
                created = created and not jdone
        else:
            print("  [SKIP] api_key_auth=jwt arm (set JWT_PROFILE=<existing profile name> to run it)")

        try:
            with open(os.path.join(AUDIT, "audit.jsonl")) as f:
                a = f.read()
            (ok if '"tool":"lb_create"' in a and '"tool":"lb_delete"' in a else bad)(
                "audit.jsonl records create+delete")
        except FileNotFoundError:
            bad("no audit.jsonl written")
    finally:
        if created:
            print("  [cleanup] removing leftover VIP via --no-confirm")
            subprocess.run([BIN, "--config", CFG, "--role", "admin",
                            "--no-confirm", "--audit-dir", AUDIT],
                           input=('{"jsonrpc":"2.0","id":1,"method":"initialize","params":'
                                  '{"protocolVersion":"2025-06-18","capabilities":{},'
                                  '"clientInfo":{"name":"cleanup","version":"0"}}}\n'
                                  '{"jsonrpc":"2.0","method":"notifications/initialized"}\n'
                                  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":'
                                  '{"name":"lb_delete","arguments":{"external_ip":"%s",'
                                  '"port":%d,"protocol":"tcp"}}}\n'
                                  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":'
                                  '{"name":"lb_delete","arguments":{"external_ip":"%s",'
                                  '"port":%d,"protocol":"tcp","host_url":"jwt.live-e2e"}}}\n'
                                  % (VIP, VPORT, os.environ.get("JWT_VIP", VIP), VPORT)),
                           text=True, capture_output=True, timeout=15)
        m.close()

print()
print("=== live E2E [OK] ===" if code == 0 else "=== live E2E [FAILED] ===")
sys.exit(code)
PY
