/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"strings"
	"testing"
)

// loxilb://docs/metrics is the curated half (key families, caveats) followed
// by the table generated from tools/metric-manifest. Regenerating the table
// must not drop the curated text.
func TestMetricsReferenceKeepsTheCuratedCaveatsAndTheGeneratedTable(t *testing.T) {
	for _, curated := range []string{
		"## Known caveats",
		"--userservice, the metrics endpoint requires a bearer",
		"loxilb_ai_requests_total carries an outcome label",
		"loxilb_ai_token_quota_denied_total is the union of both token-quota",
		"loxilb_kv_inventory_fresh 0 with loxilb_kv_subscriber_connected 1",
	} {
		if !strings.Contains(metricsReferenceDoc, curated) {
			t.Errorf("curated text lost: %q", curated)
		}
	}
	if !strings.HasPrefix(metricFamilyTable, "## All gateway families (generated)") {
		t.Fatalf("generated table header missing: %.80q", metricFamilyTable)
	}
	if !strings.Contains(metricsReferenceDoc, metricFamilyTable) {
		t.Fatal("the served reference does not carry the generated table")
	}
	for _, family := range []string{
		"loxilb_ai_admission_queue_wait_seconds", "loxilb_ai_admission_inflight", "loxilb_ai_admission_effective_limit",
		"loxilb_ai_admission_mode", "loxilb_pd_admission_shed_total", "loxilb_pd_admission_overflow_shed_total",
		"loxilb_ai_epp_duration_seconds", "loxilb_ai_epp_candidates_rejected_total", "loxilb_ai_epp_pending",
		"loxilb_kv_inventory_fresh", "loxilb_kv_subscriber_connected", "loxilb_kv_subscriber_last_event_timestamp_seconds",
		"loxilb_ai_token_quota_denied_total", "loxilb_ai_tokens_consumed_total", "loxilb_ai_fc_band_total",
	} {
		if !strings.Contains(metricFamilyTable, "| "+family+" |") {
			t.Errorf("generated table has no row for %s", family)
		}
	}
	if strings.Contains(metricFamilyTable, "| aictrl_") || strings.Contains(metricFamilyTable, "| doca_") {
		t.Error("aictrl_* and doca_* families are outside the bridge's scope")
	}
}
