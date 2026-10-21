#!/usr/bin/env python3
"""
Tier-0 static correctness gate for the LoxiLB monitoring stack.

Hermetic (no loxilb, no traffic, no network) checks over deploy/monitoring/ and
the exporter source. Catches the class of silent drift that manual validation
cannot re-catch on every change: a dashboard/alert referencing a metric the
exporter no longer emits, a broken PromQL expr, an alert annotation pointing at a
panel that was renamed, a panel wired to the wrong datasource.

Tier 0 of the internal monitoring CI plan. Run locally:

    python3 deploy/monitoring/ci/lint-monitoring.py

Exit 0 = clean, 1 = at least one ERROR. WARNINGs never fail the build.
promtool checks are skipped (with a WARNING) when promtool is absent; CI installs
it so the full gate runs there.
"""

import argparse
import glob
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

# --- metric names that are legitimately NOT loxilb exporter families ----------
# Prometheus/Go runtime + self-scrape names used by a few panels and the soak
# checks. Prefix match.
INFRA_PREFIXES = (
    "up", "scrape_", "prometheus_", "process_", "go_", "promhttp_", "ALERTS",
)
# Metric families that are registered lazily / conditionally: absent on an idle
# system by design (§3.5). Panels over these SHOULD set noValue.
LAZY_PREFIXES = ("loxilb_ai_", "loxilb_l4_error_")

# Package-boundary deny-list: metric surfaces owned by components outside the
# default product profile (DPU/DOCA hardware profile, the standalone AI
# controller, the standalone KV agent's gateway-side liveness gauge). The
# default dashboards and rules must not reference them at all — not as hidden
# panels, fallbacks inside a larger expression, or "No data" placeholders. A
# separately versioned profile is the only way to consume them.
BOUNDARY_DENY_PREFIXES = ("doca_", "aictrl_")
BOUNDARY_DENY_EXACT = {"loxilb_kv_agent_up"}


def boundary_violation(ref):
    return ref.startswith(BOUNDARY_DENY_PREFIXES) or ref in BOUNDARY_DENY_EXACT

# Lookarounds keep recorded-rule names (level:metric:operation) from
# matching their embedded fragments as family references.
METRIC_TOKEN = re.compile(
    r"(?<![A-Za-z0-9_:])((?:loxilb|doca|aictrl)_[a-z0-9_]+)(?![A-Za-z0-9_:])")
HIST_SUFFIXES = ("_bucket", "_count", "_sum")

# Grafana datasource sentinels that are not the Prometheus datasource.
BUILTIN_DS = {"-- Grafana --", "-- Mixed --", "-- Dashboard --", "__expr__",
              "grafana", "datasource"}
EXPECTED_DS_UID = "loxilb-prom"


class Report:
    def __init__(self):
        self.errors = []
        self.warnings = []

    def err(self, where, msg):
        self.errors.append(f"{where}: {msg}")

    def warn(self, where, msg):
        self.warnings.append(f"{where}: {msg}")

    def dump(self):
        for w in self.warnings:
            print(f"  WARN  {w}")
        for e in self.errors:
            print(f"  ERROR {e}")
        print()
        print(f"monitoring-lint: {len(self.errors)} error(s), "
              f"{len(self.warnings)} warning(s)")
        return 0 if not self.errors else 1


# ---------------------------------------------------------------------------
# Exporter metric surface (the ground truth for name resolution)
# ---------------------------------------------------------------------------
def collect_exporter_metrics(repo_root):
    """The family names the binaries can actually emit.

    Primary source: the committed metric ownership manifest, which is derived
    from Go AST extraction (tools/metric-manifest) and therefore knows about
    Namespace/Subsystem-composed names that never appear as a single string
    literal. gen-metric-manifest.py --check keeps that file honest in the same
    CI run, so trusting it here does not weaken the gate.

    Fallback (manifest missing, e.g. an old checkout): a literal scan over
    non-test Go sources. Test files are excluded either way — a mock metric
    name in *_test.go is not part of the exporter surface and must not make a
    dashboard reference resolve."""
    manifest = os.path.join(repo_root, "deploy", "monitoring", "manifest",
                            "metric-manifest.json")
    try:
        data = json.load(open(manifest, encoding="utf-8"))
        return {f["name"]: f.get("labels", []) for f in data["families"]}
    except (OSError, KeyError, json.JSONDecodeError):
        pass
    names = set()
    skip = {".git", "loxilb-ebpf", "vendor", "node_modules", "3rdparty"}
    lit = re.compile(r'"((?:loxilb|doca|aictrl)_[a-z0-9_]+)"')
    for dirpath, dirnames, filenames in os.walk(repo_root):
        dirnames[:] = [d for d in dirnames if d not in skip]
        for fn in filenames:
            if not fn.endswith(".go") or fn.endswith("_test.go"):
                continue
            try:
                with open(os.path.join(dirpath, fn), encoding="utf-8",
                          errors="ignore") as fh:
                    for m in lit.finditer(fh.read()):
                        names.add(m.group(1))
            except OSError:
                pass
    # literal fallback knows names only; None labels disable legend checks
    return {n: None for n in names}


def is_known_metric(ref, exporter):
    if ref.startswith(INFRA_PREFIXES):
        return True
    if ref in exporter:
        return True
    for suf in HIST_SUFFIXES:
        if ref.endswith(suf) and ref[: -len(suf)] in exporter:
            return True
    return False


LEGEND_VAR = re.compile(r"\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}")
BY_CLAUSE = re.compile(r"\bby\s*\(([^)]*)\)")
# Labels any Prometheus target/rule can carry regardless of the family schema.
AMBIENT_LABELS = {"instance", "job", "cluster", "le", "quantile"}


def check_legend(panel_title, target, exporter, rep, fname):
    """A legendFormat variable must be a label the query can actually
    produce: a label of the (single) referenced family, a by-clause label,
    or an ambient scrape label. A variable that matches nothing renders an
    empty legend in every deployment — a silent dashboard defect."""
    legend = target.get("legendFormat") or ""
    variables = LEGEND_VAR.findall(legend)
    if not variables:
        return
    expr = target.get("expr") or ""
    fams = {m.group(1) for m in METRIC_TOKEN.finditer(expr)}
    fams = {f for f in fams if isinstance(exporter, dict) and f in exporter}
    if len(fams) != 1:
        return  # multi-family or recorded-rule exprs: label set not derivable
    labels = exporter[next(iter(fams))]
    if labels is None:
        return  # literal-fallback mode has no schema knowledge
    allowed = set(labels) | AMBIENT_LABELS
    for grp in BY_CLAUSE.findall(expr):
        allowed.update(x.strip() for x in grp.split(",") if x.strip())
    for v in variables:
        if v not in allowed:
            rep.err(fname, f"panel '{panel_title}' legend uses '{{{{{v}}}}}' "
                           f"but the queried family has labels "
                           f"{sorted(labels)} (legend would render empty)")


# ---------------------------------------------------------------------------
# Dashboards
# ---------------------------------------------------------------------------
def iter_panels(panels):
    """Yield every panel, recursing into collapsed-row child panels."""
    for p in panels or []:
        yield p
        if p.get("panels"):
            yield from iter_panels(p["panels"])


def ds_uid(obj):
    ds = obj.get("datasource")
    if isinstance(ds, dict):
        return ds.get("uid")
    if isinstance(ds, str):
        return ds
    return None


def lint_dashboards(mon_dir, exporter, rep, activation=None):
    files = sorted(glob.glob(os.path.join(mon_dir, "grafana/dashboards/*.json")))
    if not files:
        rep.err("dashboards", "no dashboard JSON files found")
        return {}, []

    uids = {}
    title_to_panels = {}          # dashboard title -> set(panel titles)
    all_exprs = []                # (file, expr) for PromQL parse

    for f in files:
        name = os.path.basename(f)
        try:
            d = json.load(open(f, encoding="utf-8"))
        except (json.JSONDecodeError, OSError) as e:
            rep.err(name, f"invalid JSON: {e}")
            continue

        uid = d.get("uid")
        title = d.get("title")
        if not uid:
            rep.err(name, "missing top-level uid")
        elif uid in uids:
            rep.err(name, f"duplicate uid '{uid}' (also in {uids[uid]})")
        else:
            uids[uid] = name
        if not title:
            rep.err(name, "missing top-level title")

        tvars = {v.get("name") for v in
                 d.get("templating", {}).get("list", [])}
        for v in d.get("templating", {}).get("list", []):
            if v.get("name") in ("instance", "model", "tenant") and \
                    v.get("includeAll") and v.get("allValue") != ".*":
                rep.err(name, f"variable ${v.get('name')} has All without "
                              f"allValue \".*\": All is then an alternation "
                              f"of discovered values, and an empty label "
                              f"(tenant=\"\", a refusal before the key "
                              f"resolved) matches none of them (MON-05)")
        # bootstrap dashboard is intentionally variable-free
        if "instance" not in tvars and uid != "loxilb-bootstrap":
            rep.warn(name, "no $instance template variable (§3.8)")

        panel_ids = {}
        panel_titles = set()
        panels = list(iter_panels(d.get("panels", [])))
        for p in panels:
            pid = p.get("id")
            if pid is not None:
                if pid in panel_ids:
                    rep.err(name, f"duplicate panel id {pid} "
                                  f"('{p.get('title')}' & '{panel_ids[pid]}')")
                else:
                    panel_ids[pid] = p.get("title")
            if p.get("title"):
                panel_titles.add(p["title"])

            if p.get("type") == "row":
                continue

            # datasource consistency (panel level + each target)
            for scope, obj in [("panel", p)] + \
                    [("target", t) for t in (p.get("targets") or [])]:
                u = ds_uid(obj)
                if u and u not in BUILTIN_DS and u != EXPECTED_DS_UID:
                    rep.err(name, f"panel '{p.get('title')}' {scope} datasource "
                                  f"uid '{u}' != '{EXPECTED_DS_UID}'")

            lazy = False
            for t in p.get("targets") or []:
                expr = t.get("expr")
                if not expr or not expr.strip():
                    continue
                all_exprs.append((name, expr))
                check_legend(p.get("title"), t, exporter, rep, name)
                for fam in PARTITIONED_FAMILIES:
                    for msg in (outcome_partition_violation(expr, fam),
                                aborted_cause_violation(expr, fam)):
                        if msg:
                            rep.err(name, f"panel '{p.get('title')}' {msg}")
                msg = lazy_increase_violation(expr, activation or {})
                if msg:
                    rep.err(name, f"panel '{p.get('title')}' {msg}")
                for m in METRIC_TOKEN.finditer(expr):
                    ref = m.group(1)
                    if boundary_violation(ref):
                        rep.err(name, f"panel '{p.get('title')}' references "
                                      f"'{ref}', which belongs to a component "
                                      f"outside the default package boundary")
                    elif not is_known_metric(ref, exporter):
                        rep.err(name, f"panel '{p.get('title')}' references "
                                      f"unknown metric '{ref}' "
                                      f"(not in exporter source)")
                    if ref.startswith(LAZY_PREFIXES):
                        lazy = True
                # $instance coverage on loxilb selectors (warn only)
                if "loxilb_" in expr and "$instance" not in expr \
                        and uid != "loxilb-bootstrap":
                    rep.warn(name, f"panel '{p.get('title')}' selector without "
                                   f"$instance filter")

            for msg in filter_violations(p, tvars, exporter):
                rep.err(name, f"panel '{p.get('title')}' {msg}")

            if lazy:
                fc = (p.get("fieldConfig") or {}).get("defaults") or {}
                if "noValue" not in fc:
                    rep.warn(name, f"panel '{p.get('title')}' over a lazy/"
                                   f"conditional family without noValue (§3.5)")

        title_to_panels[title] = panel_titles

    return title_to_panels, all_exprs


# ---------------------------------------------------------------------------
# increase() on a lazy family (MON-04)
# ---------------------------------------------------------------------------
# A lazy family's series does not exist until its first increment, and
# increase() over a window counts only what happens after a series' first
# sample: the first event a window sees reads as nothing, so a "> 0" alert
# misses the first anomaly and a count panel shows 0 for it. A family the
# manifest classes lazy (V lazy vector children, C sample-lazy collector,
# Q quota-lazy collector, D+V) is counted with the form that also counts a
# series' first value:
#   clamp_min(X - X offset W, 0) or (X unless X offset W)
LAZY_ACTIVATION = {"V", "C", "Q", "D+V"}
INCREASE_TOKEN = re.compile(r"increase\(\s*((?:loxilb|doca|aictrl)_[a-z0-9_]+)")


def load_activation(repo_root):
    manifest = os.path.join(repo_root, "deploy", "monitoring", "manifest",
                            "metric-manifest.json")
    try:
        data = json.load(open(manifest, encoding="utf-8"))
        return {f["name"]: f.get("activation") for f in data["families"]}
    except (OSError, KeyError, json.JSONDecodeError):
        return {}


def lazy_increase_violation(expr, activation):
    """A message when expr takes increase() of a lazy family, else None."""
    for m in INCREASE_TOKEN.finditer(expr):
        if activation.get(m.group(1)) in LAZY_ACTIVATION:
            return (f"takes increase() of {m.group(1)}, a lazy family "
                    f"({activation[m.group(1)]}): its first event reads as "
                    f"nothing; use clamp_min(X - X offset W, 0) or "
                    f"(X unless X offset W)")
    return None


# ---------------------------------------------------------------------------
# Dashboard filters (MON-05)
# ---------------------------------------------------------------------------
# A dashboard with a $model or $tenant variable promises that every panel
# obeys it. A selector on a family that carries the label but not the
# variable ignores the filter silently: the panel shows every model while the
# picker says one. So, on such a dashboard, every selector of a family with a
# model (tenant) label carries $model ($tenant); and a panel none of whose
# families carries either label says "(global)" in its title, so a reader
# knows the filter does not reach it. With allValue ".*" on the variables,
# "All" also matches the empty label (a refusal before the key resolved),
# which an alternation of discovered values never did.
FILTER_LABELS = (("model", "$model"), ("tenant", "$tenant"))
# What a panel says when one of the two selections does not reach it.
SCOPE_MARK = {"model": "(all models)", "tenant": "(all tenants)"}
SELECTOR_TOKEN = re.compile(r"((?:loxilb|doca|aictrl)_[a-z0-9_]+)(\{[^}]*\})?")


def family_labels(ref, exporter):
    """The label set of the family `ref` names (histogram suffixes folded),
    or None when it is not known with its labels."""
    if not isinstance(exporter, dict):
        return None
    if ref in exporter:
        return exporter[ref]
    for suf in HIST_SUFFIXES:
        if ref.endswith(suf) and ref[: -len(suf)] in exporter:
            return exporter[ref[: -len(suf)]]
    return None


def filter_violations(panel, tvars, exporter):
    """The MON-05 messages for one panel of a dashboard with variables tvars."""
    wanted = [(lab, var) for lab, var in FILTER_LABELS if lab in tvars]
    if not wanted:
        return []
    msgs = []
    known = False
    reach = {lab: False for lab, _ in wanted}
    for t in panel.get("targets") or []:
        for m in SELECTOR_TOKEN.finditer(t.get("expr") or ""):
            labels = family_labels(m.group(1), exporter)
            if labels is None:
                continue
            known = True
            selector = m.group(2) or ""
            for lab, var in wanted:
                if lab in labels:
                    reach[lab] = True
                    if var not in selector:
                        msgs.append(f"selects {m.group(1)} without {var}, so "
                                    f"the {lab} filter does not reach it")
    filterable = any(reach.values())
    title = panel.get("title") or ""
    if known and not filterable and "(global)" not in title:
        msgs.append("queries no family the dashboard's filters reach; say "
                    "(global) in its title")
    if filterable and "(global)" in title:
        msgs.append("says (global) but the dashboard's filters reach it")
    # A panel one filter reaches and the other does not (a model-only
    # family: the stream gauge, the P/D lifecycle, timing, fallback, tier and
    # session families) shows every tenant's value whatever the tenant
    # picker says, and read beside a tenant selection it passes B's stream
    # off as A's (QA 2026-10-21). Its title says which selection does not
    # reach it, and a panel the selection reaches never says it.
    for lab, _ in wanted:
        mark = SCOPE_MARK[lab]
        if filterable and not reach[lab] and mark not in title:
            msgs.append(f"queries no family with a {lab} label, so the "
                        f"{lab} filter does not reach it; say {mark} in its "
                        f"title")
        if reach[lab] and mark in title:
            msgs.append(f"says {mark} but the {lab} filter reaches it")
    return sorted(set(msgs))


# ---------------------------------------------------------------------------
# Outcome-partitioned families
# ---------------------------------------------------------------------------
# loxilb_ai_requests_total is partitioned by an `outcome` label: "completed" is
# a request answered whole, "aborted" one whose client did not get the whole
# answer (with a `cause`: client, engine or gateway), "denied" one the
# AI-gateway policy gate refused before dialling a backend. All three live in
# one family so it can serve as a denominator for offered load.
#
# That makes ONE mistake very easy and completely silent: filtering the
# numerator of a ratio to completed traffic while leaving its denominator
# unfiltered (or the reverse). The expression still parses, the panel still
# renders, and the number is wrong in the direction that looks reassuring --
# during a rate-limit storm a mixed error ratio drops, because denials inflate
# the denominator only. The critical LoxilbAIErrorRatio alert was exactly this
# shape before the label existed.
#
# The rule enforced here is deliberately not "always filter": a panel showing
# true offered load must NOT filter, and one describing backend behaviour must.
# What is never right is doing both in a single expression. So: within one rule
# expr or one dashboard target, either every selector on a partitioned family
# carries an outcome matcher, or none does.
PARTITIONED_FAMILIES = ("loxilb_ai_requests_total",)


# A second mistake the aborted outcome makes easy: selecting outcome="aborted"
# with no cause matcher counts a client's cancel (traffic) together with the
# engine's and the gateway's cuts (errors). A ratio built on it pages for
# users closing their browser tabs. So a selector that narrows the family to
# aborted alone must also name its cause.
def aborted_cause_violation(expr, family):
    """Return a message when a selector of `family` selects outcome="aborted"
    without a cause matcher, else None."""
    for m in re.finditer(re.escape(family) + r"(\{[^}]*\})?", expr):
        selector = m.group(1) or ""
        if re.search(r'outcome\s*=\s*"aborted"', selector) and "cause" not in selector:
            return (f"selects {family}{{outcome=\"aborted\"}} without a cause "
                    f"matcher -- that counts the client's cancels (traffic) "
                    f"with the engine's and the gateway's cuts (errors); name "
                    f"the cause")
    return None


def outcome_partition_violation(expr, family):
    """Return a message when one expression mixes outcome-filtered and
    unfiltered selectors of `family`, else None."""
    filtered = unfiltered = 0
    for m in re.finditer(re.escape(family) + r"(\{[^}]*\})?", expr):
        selector = m.group(1) or ""
        if "outcome" in selector:
            filtered += 1
        else:
            unfiltered += 1
    if filtered and unfiltered:
        return (f"mixes {filtered} outcome-filtered and {unfiltered} "
                f"unfiltered selector(s) of {family} in one expression -- a "
                f"ratio built this way silently changes meaning when denials "
                f"spike; filter every selector or none")
    return None


# ---------------------------------------------------------------------------
# Alert rules
# ---------------------------------------------------------------------------
def extract_rule_exprs_and_annotations(rules_path):
    """Line-based extraction (stdlib-only, no PyYAML): pull every expr block's
    text and every (dashboard, panel) annotation pair. Good enough for token
    resolution and annotation mapping; promtool validates the actual rule
    syntax separately."""
    expr_text = []
    pairs = []           # (dashboard, panel)
    pending_dash = None
    in_expr = False
    expr_indent = 0
    with open(rules_path, encoding="utf-8") as fh:
        lines = fh.readlines()
    for line in lines:
        stripped = line.rstrip("\n")
        m = re.match(r"^(\s*)expr:\s*(.*)$", stripped)
        if m:
            in_expr = True
            expr_indent = len(m.group(1))
            rest = m.group(2).strip()
            if rest and rest not in (">-", ">", "|", "|-", ">+"):
                expr_text.append(rest)
            continue
        if in_expr:
            if stripped.strip() == "":
                continue
            indent = len(stripped) - len(stripped.lstrip())
            if indent > expr_indent:
                expr_text.append(stripped.strip())
                continue
            in_expr = False  # fall through to normal handling
        dm = re.match(r'^\s*dashboard:\s*"?(.+?)"?\s*$', stripped)
        if dm:
            pending_dash = dm.group(1)
            continue
        pm = re.match(r'^\s*panel:\s*"?(.+?)"?\s*$', stripped)
        if pm and pending_dash is not None:
            pairs.append((pending_dash, pm.group(1)))
            pending_dash = None
    return "\n".join(expr_text), pairs


def iter_rule_exprs(rules_path):
    """Yield (rule name, expr text) per rule. extract_rule_exprs_and_annotations
    concatenates every expr in a file into one blob, which is right for name
    resolution and wrong for any check that must not see two rules as one
    expression -- a numerator in rule A and a denominator in rule B would look
    like a mixed selector."""
    out = []
    name = None
    expr_lines = None
    expr_indent = 0
    with open(rules_path, encoding="utf-8") as fh:
        lines = fh.readlines()

    def flush():
        if expr_lines is not None:
            out.append((name or "?", " ".join(expr_lines)))

    for line in lines:
        stripped = line.rstrip("\n")
        nm = re.match(r"^\s*-?\s*(?:alert|record):\s*\"?([\w:.]+)\"?\s*$", stripped)
        if nm:
            flush()
            expr_lines = None
            name = nm.group(1)
            continue
        m = re.match(r"^(\s*)expr:\s*(.*)$", stripped)
        if m:
            flush()
            expr_indent = len(m.group(1))
            rest = m.group(2).strip()
            expr_lines = [] if rest in (">-", ">", "|", "|-", ">+") else [rest]
            continue
        if expr_lines is not None:
            if stripped.strip() == "" or stripped.strip().startswith("#"):
                continue
            indent = len(stripped) - len(stripped.lstrip())
            if indent > expr_indent:
                expr_lines.append(stripped.strip())
            else:
                flush()
                expr_lines = None
    flush()
    return out


def lint_rules(mon_dir, exporter, title_to_panels, rep, activation=None):
    rule_paths = sorted(glob.glob(os.path.join(mon_dir,
                                               "prometheus/rules/*.yml")))
    if not rule_paths:
        rep.err("rules", "no rule files found under prometheus/rules/")
        return
    for rules_path in rule_paths:
        fname = os.path.basename(rules_path)
        expr_blob, pairs = extract_rule_exprs_and_annotations(rules_path)

        for m in METRIC_TOKEN.finditer(expr_blob):
            ref = m.group(1)
            if boundary_violation(ref):
                rep.err(fname,
                        f"rule expr references '{ref}', which belongs to a "
                        f"component outside the default package boundary")
            elif not is_known_metric(ref, exporter):
                rep.err(fname,
                        f"rule expr references unknown metric '{ref}'")

        for rule_name, rule_expr in iter_rule_exprs(rules_path):
            for fam in PARTITIONED_FAMILIES:
                for msg in (outcome_partition_violation(rule_expr, fam),
                            aborted_cause_violation(rule_expr, fam)):
                    if msg:
                        rep.err(fname, f"rule '{rule_name}' {msg}")
            msg = lazy_increase_violation(rule_expr, activation or {})
            if msg:
                rep.err(fname, f"rule '{rule_name}' {msg}")

        for dash, panel in pairs:
            if dash not in title_to_panels:
                rep.err(fname,
                        f"annotation dashboard '{dash}' matches no dashboard "
                        f"title")
            elif panel not in title_to_panels[dash]:
                rep.err(fname,
                        f"annotation panel '{panel}' not found on '{dash}' "
                        f"(§3.7)")


# ---------------------------------------------------------------------------
# promtool (optional): rule validity + dashboard PromQL syntax
# ---------------------------------------------------------------------------
def find_promtool(explicit):
    if explicit:
        return explicit if os.path.isfile(explicit) else None
    return shutil.which("promtool")


def promtool_check_rules(promtool, mon_dir, rep):
    for rules_path in sorted(glob.glob(os.path.join(mon_dir,
                                                    "prometheus/rules/*.yml"))):
        r = subprocess.run([promtool, "check", "rules", rules_path],
                           capture_output=True, text=True)
        if r.returncode != 0:
            rep.err("promtool check rules",
                    (r.stdout + r.stderr).strip() or "failed")
    tests = sorted(glob.glob(os.path.join(mon_dir,
                                          "prometheus/rules/tests/*.yml")))
    for t in tests:
        r = subprocess.run([promtool, "test", "rules", t],
                           capture_output=True, text=True)
        if r.returncode != 0:
            rep.err("promtool test rules",
                    (r.stdout + r.stderr).strip()[:800] or "failed")


def _sub_macros(expr):
    expr = re.sub(r"\$__\w+", "5m", expr)      # __rate_interval/__interval/__range
    expr = re.sub(r"\$\{?\w+\}?", "x", expr)    # $instance/$service/$model/$tenant
    return expr


def promtool_check_exprs(promtool, exprs, rep):
    """Wrap each dashboard target expr as a recording rule and let promtool
    parse it. Batch first; on failure, re-run individually to pinpoint."""
    def run(items):
        rules = "groups:\n- name: lint\n  rules:\n"
        for i, (_, e) in enumerate(items):
            rules += f"  - record: lint_expr_{i}\n    expr: |\n"
            rules += "".join(f"        {ln}\n"
                             for ln in _sub_macros(e).splitlines() or [""])
        with tempfile.NamedTemporaryFile("w", suffix=".yml", delete=False) as tf:
            tf.write(rules)
            path = tf.name
        try:
            return subprocess.run([promtool, "check", "rules", path],
                                  capture_output=True, text=True).returncode
        finally:
            os.unlink(path)

    if not exprs or run(exprs) == 0:
        return
    for item in exprs:
        if run([item]) != 0:
            fname, e = item
            rep.err(f"{fname} PromQL",
                    f"expr does not parse: {e[:120]}"
                    + ("..." if len(e) > 120 else ""))


# ---------------------------------------------------------------------------
# MON-06: external_labels are attached only to what leaves Prometheus
# (alerts, federation, remote write), never to its own series. The recording
# rules group by (cluster, instance) and the KV alerts by cluster, so the
# loxilb scrape job must put the same cluster on the series it scrapes, or
# every cluster:/instance: series is produced without it. The promtool tests
# inject cluster into their inputs, so they cannot see this; this check can.
EXT_CLUSTER = re.compile(r"external_labels:\s*\n(?:[ \t]*#.*\n)*[ \t]+cluster:[ \t]*(\S+)")
JOB_CLUSTER = re.compile(r"^[ \t]+cluster:[ \t]*(\S+)", re.M)


def lint_scrape_cluster(mon_dir, rep):
    path = os.path.join(mon_dir, "prometheus", "prometheus.yml")
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        return
    ext = EXT_CLUSTER.search(text)
    m = re.search(r"^\s*- job_name:\s*loxilb\s*$", text, re.M)
    if not ext or not m:
        rep.err("prometheus.yml", "external_labels.cluster or the loxilb "
                                  "scrape job not found")
        return
    nxt = re.search(r"^\s*- job_name:", text[m.end():], re.M)
    job = text[m.end(): m.end() + nxt.start()] if nxt else text[m.end():]
    job = "\n".join(l for l in job.splitlines() if not l.lstrip().startswith("#"))
    got = JOB_CLUSTER.findall(job)
    if not got:
        rep.err("prometheus.yml", "the loxilb scrape job sets no cluster "
                                  "label: the recording rules' by (cluster, "
                                  "instance) produce series without it")
    elif any(g != ext.group(1) for g in got):
        rep.err("prometheus.yml", f"the loxilb scrape job's cluster {got} "
                                  f"differs from external_labels.cluster "
                                  f"'{ext.group(1)}'")


# MON-04: the first event of a series is an increase. A counter series exists
# from its first increment, so the first scrape of it already reads 1 and
# rate() and increase() see no 0 -> 1 step: the first error of a new (model,
# tenant) pair, or of a fresh process, was invisible to every rate panel and
# alert while the raw counter read 1 (QA 2026-10-21). Seeding cannot reach
# it: the series a first error creates is born with it. The exporter stamps
# every counter with its creation time and the protobuf exposition carries
# the stamp (the text formats do not); under
# --enable-feature=created-timestamp-zero-ingestion Prometheus writes a 0 at
# that instant before the series' first sample. So the loxilb scrape job
# prefers PrometheusProto, and the compose file starts Prometheus with the
# flag. Either one missing returns the first event to invisibility, silently.
CT_FLAG = "--enable-feature=created-timestamp-zero-ingestion"


def loxilb_job_text(text):
    """The loxilb scrape job's block of prometheus.yml with its comment lines
    removed, or None when the job is not there."""
    m = re.search(r"^\s*- job_name:\s*loxilb\s*$", text, re.M)
    if not m:
        return None
    nxt = re.search(r"^\s*- job_name:", text[m.end():], re.M)
    job = text[m.end(): m.end() + nxt.start()] if nxt else text[m.end():]
    return "\n".join(l for l in job.splitlines() if not l.lstrip().startswith("#"))


def lint_first_event_contract(mon_dir, rep):
    path = os.path.join(mon_dir, "prometheus", "prometheus.yml")
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        return
    job = loxilb_job_text(text)
    if job is None:
        rep.err("prometheus.yml", "the loxilb scrape job not found")
    else:
        m = re.search(r"^\s*scrape_protocols:\s*\[\s*([A-Za-z0-9.]+)", job, re.M)
        if not m or m.group(1) != "PrometheusProto":
            rep.err("prometheus.yml", "the loxilb scrape job must list "
                                      "PrometheusProto first in "
                                      "scrape_protocols: the created "
                                      "timestamps that make a series' first "
                                      "event an increase travel in the "
                                      "protobuf exposition only (MON-04)")
    path = os.path.join(mon_dir, "docker-compose.yml")
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        return
    live = "\n".join(l for l in text.splitlines()
                     if not l.lstrip().startswith("#"))
    if CT_FLAG not in live:
        rep.err("docker-compose.yml", f"the prometheus service must start "
                                      f"with {CT_FLAG}: without it the "
                                      f"first event of a series is invisible "
                                      f"to rate() and increase() (MON-04)")


# The MCP module ships its own copy of the alert rules (it is released on its
# own), which drifted from the default package's: an alert removed from one
# lived on in the other, and a description fixed in one stayed wrong in the
# other. The copy is a copy: byte for byte.
MCP_ALERT_RULES_COPY = os.path.join("mcp", "deploy", "monitoring", "prometheus",
                                    "rules", "loxilb-alerts.yml")


def lint_mcp_rules_copy(repo_root, mon_dir, rep):
    src = os.path.join(mon_dir, "prometheus", "rules", "loxilb-alerts.yml")
    dst = os.path.join(repo_root, MCP_ALERT_RULES_COPY)
    if not os.path.isfile(dst):
        return
    with open(src, "rb") as a, open(dst, "rb") as b:
        if a.read() != b.read():
            rep.err(MCP_ALERT_RULES_COPY,
                    "differs from deploy/monitoring/prometheus/rules/"
                    "loxilb-alerts.yml; copy it over (the MCP copy is a copy)")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--repo-root", default=None)
    ap.add_argument("--promtool", default=None,
                    help="path to promtool (default: $PATH)")
    ap.add_argument("--require-promtool", action="store_true",
                    help="fail (instead of warn) when promtool is absent, so "
                         "CI cannot silently skip rule and PromQL validation")
    args = ap.parse_args()

    here = os.path.dirname(os.path.abspath(__file__))
    repo_root = args.repo_root or os.path.abspath(os.path.join(here, "..", "..", ".."))
    mon_dir = os.path.join(repo_root, "deploy", "monitoring")
    if not os.path.isdir(mon_dir):
        print(f"deploy/monitoring not found under {repo_root}", file=sys.stderr)
        return 2

    print(f"monitoring-lint: repo={repo_root}")
    rep = Report()

    exporter = collect_exporter_metrics(repo_root)
    print(f"  exporter metric names discovered: {len(exporter)}")
    if len(exporter) < 20:
        rep.err("exporter", "suspiciously few metric names found — is the "
                            "api/prometheus source present and readable?")

    activation = load_activation(repo_root)
    title_to_panels, exprs = lint_dashboards(mon_dir, exporter, rep, activation)
    lint_rules(mon_dir, exporter, title_to_panels, rep, activation)
    lint_mcp_rules_copy(repo_root, mon_dir, rep)
    lint_scrape_cluster(mon_dir, rep)
    lint_first_event_contract(mon_dir, rep)

    promtool = find_promtool(args.promtool)
    if promtool:
        print(f"  promtool: {promtool}")
        promtool_check_rules(promtool, mon_dir, rep)
        promtool_check_exprs(promtool, exprs, rep)
    elif args.require_promtool:
        rep.err("promtool", "not found but required — rule-validity and "
                            "PromQL syntax checks did not run")
    else:
        rep.warn("promtool", "not found — skipped rule-validity and PromQL "
                             "syntax checks (CI installs promtool)")

    print()
    return rep.dump()


if __name__ == "__main__":
    sys.exit(main())
