/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// The diagnostics sections are fed by expositions captured from the live
// latest-u24 gateway (provenance in each file's header), not hand-written
// text. The expected values are read from the fixture by a line scan that
// shares no code with the bridge's parser, so a re-captured fixture moves
// the expectation with it.

const bedExposition = "testdata/exposition-latest-u24-bed.txt"

func loadExposition(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// fixtureSum adds the values of every sample line of family whose label
// block contains all of the given label="value" pairs.
func fixtureSum(t *testing.T, text, family string, labels ...string) float64 {
	t.Helper()
	var total float64
	found := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, ok := strings.Cut(line, "{")
		if !ok {
			name, rest, ok = strings.Cut(line, " ")
			if !ok {
				continue
			}
			rest = "} " + rest
		}
		if name != family {
			continue
		}
		block, value, ok := strings.Cut(rest, "} ")
		if !ok {
			continue
		}
		match := true
		for _, l := range labels {
			if !strings.Contains(block, l) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("%s: %q: %v", family, line, err)
		}
		total += v
		found = true
	}
	if !found {
		t.Fatalf("fixture has no %s sample with %v", family, labels)
	}
	return total
}

func fixtureBridgeOut(t *testing.T, path, tool string) map[string]any {
	t.Helper()
	mock := newAIMock(t)
	mock.metricsText = loadExposition(t, path)
	b := aiTestBridge(t, mock)
	cs := session(t, b, guard.RoleViewer)
	return callOut(t, callTool(t, cs, tool, nil))
}

func TestDiagnoseAILatencyAdmissionFromTheBedFixture(t *testing.T) {
	text := loadExposition(t, bedExposition)
	const svc, pool = `service="20.20.20.3:2099"`, `pool="20.20.20.3_2099"`
	out := fixtureBridgeOut(t, bedExposition, "diagnose_ai_latency")
	ev, _ := out["evidence"].(map[string]any)
	pools, _ := ev["admission"].([]any)
	var p map[string]any
	for _, raw := range pools {
		if m, _ := raw.(map[string]any); m["service"] == "20.20.20.3:2099" {
			p = m
		}
	}
	if p == nil {
		t.Fatalf("no admission row for the loaded pool: %v", ev["admission"])
	}
	if want := fixtureSum(t, text, "loxilb_ai_admission_mode", svc, pool); want != 2 || p["mode"] != "enforce" {
		t.Errorf("mode = %v, fixture %v (2 = enforce)", p["mode"], want)
	}
	if want := fixtureSum(t, text, "loxilb_ai_admission_effective_limit", svc, pool); p["effective_limit"] != want {
		t.Errorf("effective_limit = %v, fixture %v", p["effective_limit"], want)
	}
	if want := fixtureSum(t, text, "loxilb_ai_admission_inflight", svc, pool, `role="service"`); p["inflight"] != want {
		t.Errorf("inflight = %v, fixture role=service %v", p["inflight"], want)
	}
	qw, _ := p["queue_wait"].(map[string]any)
	if want := fixtureSum(t, text, "loxilb_ai_admission_queue_wait_seconds_count", svc, pool); want == 0 || qw["count"] != want {
		t.Errorf("queue_wait.count = %v, fixture %v (the capture parked requests)", qw["count"], want)
	}
	if want := fixtureSum(t, text, "loxilb_ai_admission_queue_wait_seconds_sum", svc, pool); qw["sum_seconds"] != want {
		t.Errorf("queue_wait.sum_seconds = %v, fixture %v", qw["sum_seconds"], want)
	}
	pd, _ := ev["pd_admission_sheds"].(map[string]any)
	for _, f := range []string{"loxilb_pd_admission_shed_total", "loxilb_pd_admission_overflow_shed_total"} {
		if want := fixtureSum(t, text, f); pd[f] != want {
			t.Errorf("pd_admission_sheds[%s] = %v, fixture %v", f, pd[f], want)
		}
	}
	epp, _ := ev["epp"].(map[string]any)
	if want := fixtureSum(t, text, "loxilb_ai_epp_pending"); epp["pending"] != want {
		t.Errorf("epp.pending = %v, fixture %v", epp["pending"], want)
	}
	if want := fixtureSum(t, text, "loxilb_ai_epp_duration_seconds_count"); want == 0 && epp["duration"] != nil {
		t.Errorf("an EPP histogram with no sample must not be summarised: %v", epp["duration"])
	}
	// The bed runs no KV publisher: no subscriber series, no section.
	if _, has := ev["kv_freshness"]; has {
		t.Errorf("kv_freshness from an exposition without subscriber series: %v", ev["kv_freshness"])
	}
}

func TestDiagnoseAILatencyTokenQuotaFromTheBedFixture(t *testing.T) {
	text := loadExposition(t, bedExposition)
	const tenant = `tenant="fixture-t"`
	out := fixtureBridgeOut(t, bedExposition, "diagnose_ai_latency")
	ev, _ := out["evidence"].(map[string]any)
	rows, _ := ev["quota_denied"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want one tenant with token-quota refusals, got %v", ev["quota_denied"])
	}
	r, _ := rows[0].(map[string]any)
	denied := fixtureSum(t, text, "loxilb_ai_token_quota_denied_total", tenant)
	exhausted := fixtureSum(t, text, "loxilb_ai_rate_limit_hits_total", tenant, `reason="token_quota_exceeded"`)
	would := fixtureSum(t, text, "loxilb_ai_rate_limit_hits_total", tenant, `reason="token_quota_would_exceed"`)
	if r["tenant"] != "fixture-t" || r["denied"] != denied || r["exhausted"] != exhausted || r["would_exceed"] != would {
		t.Errorf("row = %v, fixture denied %v exhausted %v would_exceed %v", r, denied, exhausted, would)
	}
	// The premise the split rests on: the denied series is the union of the
	// two reasons (api/prometheus/ai_metrics.go, aiTokenQuotaDeniedTotal).
	if denied != exhausted+would {
		t.Errorf("fixture: denied %v != exhausted %v + would_exceed %v", denied, exhausted, would)
	}
	found := false
	for _, raw := range out["suggested_actions"].([]any) {
		a, _ := raw.(map[string]any)
		args, _ := a["args"].(map[string]any)
		if a["tool"] == "ai_ratelimit_get" && args["tenant_id"] == "fixture-t" && a["risk"] == "none" {
			found = true
		}
	}
	if !found {
		t.Errorf("token-quota refusals must suggest ai_ratelimit_get for the tenant: %v", out["suggested_actions"])
	}
}

func TestAITrafficReportTokensAndBandsFromTheBedFixture(t *testing.T) {
	text := loadExposition(t, bedExposition)
	const tenant = `tenant="fixture-t"`
	out := fixtureBridgeOut(t, bedExposition, "ai_traffic_report")
	if want := fixtureSum(t, text, "loxilb_ai_tokens_consumed_total", `kind="prompt"`); want == 0 || out["tokens_consumed_prompt"] != want {
		t.Errorf("tokens_consumed_prompt = %v, fixture %v", out["tokens_consumed_prompt"], want)
	}
	if want := fixtureSum(t, text, "loxilb_ai_tokens_consumed_total", `kind="completion"`); want == 0 || out["tokens_consumed_completion"] != want {
		t.Errorf("tokens_consumed_completion = %v, fixture %v", out["tokens_consumed_completion"], want)
	}
	if want := fixtureSum(t, text, "loxilb_ai_token_quota_denied_total", tenant); out["quota_denied_total"] != want {
		t.Errorf("quota_denied_total = %v, fixture %v", out["quota_denied_total"], want)
	}
	bands, _ := out["fc_band"].([]any)
	if len(bands) == 0 {
		t.Fatal("no fc_band rows from a fixture with admitted traffic")
	}
	for _, raw := range bands {
		r, _ := raw.(map[string]any)
		want := fixtureSum(t, text, "loxilb_ai_fc_band_total",
			`service="`+r["service"].(string)+`"`, `band="`+r["band"].(string)+`"`, `outcome="`+r["outcome"].(string)+`"`)
		if r["count"] != want || want == 0 {
			t.Errorf("fc_band row %v, fixture %v (zero rows are left out)", r, want)
		}
	}
	if want := fixtureSum(t, text, "loxilb_ai_fc_band_total", `band="standard"`, `outcome="admitted"`); want == 0 {
		t.Error("fixture: the capture admitted standard-band requests")
	}
}
