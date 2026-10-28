/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package contract

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// ---- the derivation rule, on synthetic manifests (no swagger needed) ----

func TestMinTierTable(t *testing.T) {
	cases := []struct {
		impact Impact
		rev    bool
		want   Tier
	}{
		{ImpactNone, false, TierViewer},
		{ImpactObject, true, TierOperator},
		{ImpactTenant, true, TierOperator},
		{ImpactObject, false, TierAdminConfirm},
		{ImpactTenant, false, TierAdminConfirm},
		{ImpactService, true, TierAdminConfirm},
		{ImpactService, false, TierAdminConfirm},
		{ImpactGlobal, true, TierAdminConfirm},
		{ImpactInstance, true, TierAdminConfirm},
	}
	for _, tc := range cases {
		if got := MinTier(tc.impact, tc.rev); got != tc.want {
			t.Errorf("MinTier(%s, %v) = %s, want %s", tc.impact, tc.rev, got, tc.want)
		}
	}
}

// tierProblems is the check CI runs: a declared tier weaker than the rule
// fails unless tier_gap: records why; a tier_gap on an entry that is not
// weaker is stale and fails too; gaps are counted against the budget.
func tierProblems(m *Manifest) (problems []string, gaps int) {
	for _, e := range m.Operations {
		if e.State() != "mapped" {
			continue
		}
		weaker := e.WeakerThanRule()
		switch {
		case weaker && e.TierGap == "":
			problems = append(problems, fmt.Sprintf("%s: tier %s is weaker than the rule (impact %s, reversible %v -> at least %s)",
				e.Op, e.EffectiveTier(), e.Impact, e.Reversible != nil && *e.Reversible, e.Derived()))
		case weaker:
			gaps++
		case e.TierGap != "":
			problems = append(problems, fmt.Sprintf("%s: tier_gap is stale, tier %s already satisfies the rule", e.Op, e.EffectiveTier()))
		}
	}
	if gaps > m.Budget.TierGapMax {
		problems = append(problems, fmt.Sprintf("tier gaps %d > budget %d", gaps, m.Budget.TierGapMax))
	}
	return problems, gaps
}

func TestDerivationRefusesWeakerTier(t *testing.T) {
	const synthetic = `
version: 1
budget: {pending_max: 0, tier_gap_max: 0}
operations:
  - op: GET /config/ai/zzz-probe
    tools: [zzz_list]
    impact: none
  - op: POST /config/ai/zzz-global
    tools: [zzz_set]
    impact: global
    reversible: true
    tier: operator
`
	m, err := ParseManifest([]byte(synthetic))
	if err != nil {
		t.Fatal(err)
	}
	problems, _ := tierProblems(m)
	if len(problems) != 1 || !strings.Contains(problems[0], "zzz-global") || !strings.Contains(problems[0], "admin-confirm") {
		t.Fatalf("want one refusal naming the global op and admin-confirm, got %v", problems)
	}

	// Stronger than the rule is allowed.
	stronger := strings.Replace(synthetic, "tier: operator", "tier: admin-confirm", 1)
	m, err = ParseManifest([]byte(stronger))
	if err != nil {
		t.Fatal(err)
	}
	if problems, _ := tierProblems(m); len(problems) != 0 {
		t.Fatalf("admin-confirm on a global op must pass, got %v", problems)
	}

	// A declared gap is tolerated only within the budget.
	gapped := strings.Replace(synthetic, "tier: operator", "tier: operator\n    tier_gap: \"recorded\"", 1)
	m, err = ParseManifest([]byte(gapped))
	if err != nil {
		t.Fatal(err)
	}
	if problems, gaps := tierProblems(m); gaps != 1 || len(problems) != 1 || !strings.Contains(problems[0], "> budget 0") {
		t.Fatalf("gap over budget must fail: problems=%v gaps=%d", problems, gaps)
	}
	m.Budget.TierGapMax = 1
	if problems, _ := tierProblems(m); len(problems) != 0 {
		t.Fatalf("gap within budget must pass, got %v", problems)
	}

	// A stale gap marker fails.
	stale := strings.Replace(stronger, "tier: admin-confirm", "tier: admin-confirm\n    tier_gap: \"stale\"", 1)
	m, err = ParseManifest([]byte(stale))
	if err != nil {
		t.Fatal(err)
	}
	if problems, _ := tierProblems(m); len(problems) != 1 || !strings.Contains(problems[0], "stale") {
		t.Fatalf("stale tier_gap must fail, got %v", problems)
	}
}

func TestManifestShapeIsEnforced(t *testing.T) {
	bad := []string{
		"version: 1\noperations:\n  - op: GET /x\n",                                                                                     // no state
		"version: 1\noperations:\n  - op: GET /x\n    tools: [a]\n",                                                                     // no impact
		"version: 1\noperations:\n  - op: POST /x\n    tools: [a]\n    impact: object\n",                                                // no tier / reversible
		"version: 1\noperations:\n  - op: POST /x\n    pending: P9\n",                                                                   // bad phase
		"version: 1\noperations:\n  - op: POST /x\n    excluded: why\n    tier: operator\n",                                             // excluded with tier
		"version: 1\noperations:\n  - op: GET /x\n    tools: [a]\n    impact: none\n  - op: GET /x\n    tools: [a]\n    impact: none\n", // dup
		"version: 1\noperations:\n  - op: FETCH /x\n    excluded: why\n",                                                                // bad method
		"version: 1\noperations:\n  - op: GET /x\n    tools: [a]\n    impact: none\n    bogus: 1\n",                                     // unknown key
	}
	for i, y := range bad {
		if _, err := ParseManifest([]byte(y)); err == nil {
			t.Errorf("case %d accepted:\n%s", i, y)
		}
	}
}

// ---- the real manifest against swagger and the registered tools ----

func TestManifestCoversSwagger(t *testing.T) {
	spec := loadSpec(t)
	m := loadManifest(t)

	inManifest := map[string]Entry{}
	for _, e := range m.Operations {
		inManifest[e.Op] = e
	}
	for _, op := range spec.Ops() {
		if _, ok := inManifest[op.Key()]; !ok {
			t.Errorf("new endpoint %s - add a tool, or add an excluded:/pending: entry", op.Key())
		}
	}
	for _, e := range m.Operations {
		if !spec.Has(e.Op) {
			t.Errorf("%s - endpoint gone; %s", e.Op, deadPathWho(e))
		}
	}
}

func deadPathWho(e Entry) string {
	if len(e.Tools) > 0 {
		return "tool " + strings.Join(e.Tools, ", ") + " calls a dead path"
	}
	return "drop the " + e.State() + " entry"
}

func TestManifestToolsAreRegisteredAtTheirTier(t *testing.T) {
	m := loadManifest(t)
	tools := registeredTools(t)
	credited := map[string]bool{}
	for _, e := range m.Operations {
		for _, name := range e.Tools {
			credited[name] = true
			got, ok := tools[name]
			if !ok {
				t.Errorf("tool %s is in the manifest but not registered", name)
				continue
			}
			want := e.EffectiveTier()
			if e.ToolTier != "" {
				want = e.ToolTier // the tool gates the stronger path itself (see Entry.ToolTier)
			}
			if got != want {
				t.Errorf("%s: manifest tier %s but the code registers %s as %s", e.Op, want, name, got)
			}
		}
	}
	var uncredited []string
	for name := range tools {
		if !credited[name] {
			uncredited = append(uncredited, name)
		}
	}
	sort.Strings(uncredited)
	summary(t, "Registered tools credited by no REST operation (bridge-local or external backends): %s", strings.Join(uncredited, ", "))
}

func TestManifestTiersFollowTheRule(t *testing.T) {
	m := loadManifest(t)
	problems, gaps := tierProblems(m)
	for _, p := range problems {
		t.Error(p)
	}
	c := m.Count()
	var rows []string
	for _, k := range sortedKeys(c.MutationTiers) {
		rows = append(rows, fmt.Sprintf("| %s | %d |", k, c.MutationTiers[k]))
	}
	summary(t, "### Contract manifest\n\n| state | count |\n|---|---|\n| mapped | %d |\n| pending | %d (budget %d) |\n| excluded | %d |\n| tier gaps | %d (budget %d) |\n",
		c.Mapped, c.Pending, m.Budget.PendingMax, c.Excluded, gaps, m.Budget.TierGapMax)
	summary(t, "Mapped mutations by impact/tier:\n\n| impact/tier | ops |\n|---|---|\n%s\n", strings.Join(rows, "\n"))
	for _, e := range m.Operations {
		if e.TierGap != "" {
			summary(t, "- tier gap: `%s` (%s) tier %s, rule %s: %s", e.Op, strings.Join(e.Tools, ","), e.EffectiveTier(), e.Derived(), e.TierGap)
		}
	}
}

func TestPendingBudget(t *testing.T) {
	m := loadManifest(t)
	c := m.Count()
	if c.Pending > m.Budget.PendingMax {
		t.Errorf("pending %d > budget %d", c.Pending, m.Budget.PendingMax)
	}
	var phases []string
	for _, p := range sortedKeys(c.PendingByPhase) {
		phases = append(phases, fmt.Sprintf("| %s | %d |", p, c.PendingByPhase[p]))
	}
	summary(t, "Pending by phase:\n\n| phase | ops |\n|---|---|\n%s\n", strings.Join(phases, "\n"))
	var pending []string
	for _, e := range m.Operations {
		if e.Pending != "" {
			pending = append(pending, fmt.Sprintf("- %s `%s` %s", e.Pending, e.Op, e.Note))
		}
	}
	summary(t, "<details><summary>Pending operations (%d)</summary>\n\n%s\n\n</details>", len(pending), strings.Join(pending, "\n"))

	// A raised budget is not an error, but it must be visible to the reviewer.
	cmd := exec.Command("git", "show", "origin/main:mcp/contract/coverage.yml")
	cmd.Dir = ".."
	raw, err := cmd.Output()
	if err != nil {
		summary(t, "Budget ratchet: origin/main has no mcp/contract/coverage.yml to compare against")
		return
	}
	base, err := ParseManifest(raw)
	if err != nil {
		summary(t, "Budget ratchet: origin/main manifest does not parse (%v)", err)
		return
	}
	if m.Budget.PendingMax > base.Budget.PendingMax {
		summary(t, "> **WARNING** budget.pending_max raised %d -> %d against origin/main; the PR must say why", base.Budget.PendingMax, m.Budget.PendingMax)
	}
	if m.Budget.TierGapMax > base.Budget.TierGapMax {
		summary(t, "> **WARNING** budget.tier_gap_max raised %d -> %d against origin/main; the PR must say why", base.Budget.TierGapMax, m.Budget.TierGapMax)
	}
}

func TestFieldsAllowlistsNameSwaggerProperties(t *testing.T) {
	spec := loadSpec(t)
	m := loadManifest(t)
	for _, e := range m.Operations {
		for _, f := range e.Fields {
			model, prop := "serviceArguments", f
			if strings.HasPrefix(f, "endpoints[].") {
				model, prop = "endpoints", strings.TrimPrefix(f, "endpoints[].")
			}
			found := false
			for _, p := range spec.PropNames(model) {
				if p == prop {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s (%s).fields: %s is not a %s property", e.Op, strings.Join(e.Tools, ","), f, model)
			}
		}
	}
}

// Every REST path literal in the bridge source must be an operation of
// swagger, and when the enclosing handler names its tool the manifest must
// credit that tool with the operation.
func TestPathLiteralsAreCredited(t *testing.T) {
	spec := loadSpec(t)
	m := loadManifest(t)
	byOp := map[string]Entry{}
	for _, e := range m.Operations {
		byOp[e.Op] = e
	}
	credits := func(e Entry, tool string) bool {
		for _, n := range e.Tools {
			if n == tool {
				return true
			}
		}
		return false
	}

	var refs []PathRef
	for _, dir := range []string{toolsDir, clientDir} {
		r, err := ScanPathRefs(dir)
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		refs = append(refs, r...)
	}
	if len(refs) < 50 {
		t.Fatalf("only %d path literals found; the scanner lost its targets", len(refs))
	}
	unattributed := 0
	for _, r := range refs {
		if r.Literal == spec.BasePath || strings.HasPrefix(r.Literal, "/api/") {
			continue // the base path constant; Prometheus / Alertmanager clients
		}
		ops := spec.Match(r.Method, r.Literal)
		if len(ops) == 0 {
			t.Errorf("%s:%d: %s %s matches no swagger operation (dead path; tool %q)", r.File, r.Line, r.Method, r.Literal, r.Tool)
			continue
		}
		if r.Tool == "" {
			unattributed++
			ok := false
			for _, op := range ops {
				if _, present := byOp[op.Key()]; present {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s:%d: %s is not in the manifest", r.File, r.Line, r.Literal)
			}
			continue
		}
		ok := false
		for _, op := range ops {
			if e, present := byOp[op.Key()]; present && credits(e, r.Tool) {
				ok = true
			}
		}
		if !ok {
			keys := make([]string, 0, len(ops))
			for _, op := range ops {
				keys = append(keys, op.Key())
			}
			t.Errorf("%s:%d: %s calls %s but the manifest does not credit it (%s)", r.File, r.Line, r.Tool, r.Literal, strings.Join(keys, " | "))
		}
	}
	summary(t, "Path literals scanned: %d (%d not attributable to a tool, checked for presence only)", len(refs), unattributed)
}

// Change-gate requirement 6: every global- or instance-impact mutation is in
// guard.NeverAutopilot, so configuration cannot make it skip the confirm step.
func TestGlobalAndInstanceMutationsAreNeverAutopilot(t *testing.T) {
	m := loadManifest(t)
	for _, e := range m.Operations {
		if e.State() != "mapped" || !e.Impact.NeverAutopilot() {
			continue
		}
		for _, name := range e.Tools {
			if _, ok := guard.NeverAutopilot[name]; !ok {
				t.Errorf("%s: %s has impact %s but is missing from guard.NeverAutopilot", e.Op, name, e.Impact)
			}
		}
	}
}
