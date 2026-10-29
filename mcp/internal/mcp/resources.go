/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/tools"
)

//go:generate go run ../../contract/gen-metricsdoc -root ../../.. -out zz_metrics_reference_gen.go

// metricsReferenceDoc is the metric-family reference served as
// loxilb://docs/metrics: the curated key families and caveats below, then
// the generated table of every gateway family (metricFamilyTable). Keep the
// caveats section in sync with docs/MCP-OPERATIONS.md
const metricsReferenceDoc = metricsCuratedDoc + "\n" + metricFamilyTable

const metricsCuratedDoc = `# loxilb metric family reference

Scrape endpoint: GET /netlox/v1/metrics (Prometheus exposition text).
Use the metrics_snapshot tool with a families glob, or promql_query when a
Prometheus server is configured.

## Key families

- loxilb_l4_error_events_total (counter, labels: proto, reason) — always-on,
  unsampled L4 connection error events. Basis of the LoxilbL4ErrorBurst alert.
  Independent of the sampled L4 tracer (l4trace_status_get).
- loxilb_ai_* — AI-gateway families: request counts, token throughput,
  TTFB/TTFT latency histograms, rate-limit drops, per-model/tenant labels.
- loxilb_healthy_endpoints / loxilb_unhealthy_endpoints — endpoint probe
  results feeding the NoHealthyEndpoints/UnhealthyEndpoints alerts.
- loxilb_active_conntrack_entries / loxilb_conntrack_max_entries and the
  flow/traffic families — dataplane load and conntrack headroom.
- loxilb_ai_admission_* (labels service, pool) — the capacity gate per model
  pool: loxilb_ai_admission_mode (0 off, 1 observe, 2 enforce),
  loxilb_ai_admission_inflight (role="service" is the pool-wide count),
  loxilb_ai_admission_effective_limit (the ceiling in force, 0 unlimited),
  loxilb_ai_admission_queue_wait_seconds. Basis of the LoxilbAIAdmission*
  alerts; diagnose_ai_latency reads them per pool.
- loxilb_pd_admission_shed_total / loxilb_pd_admission_overflow_shed_total —
  P/D per-endpoint admission sheds (the overflow valve is the only shed that
  can fire with per-endpoint queueing on).
- loxilb_ai_epp_duration_seconds, loxilb_ai_epp_candidates_rejected_total
  (reason), loxilb_ai_epp_pending — the Endpoint Picker's decision latency,
  the candidates the data plane skipped and the decisions it holds.
- loxilb_kv_subscriber_connected, loxilb_kv_inventory_fresh,
  loxilb_kv_subscriber_last_event_timestamp_seconds (labels service, ep) —
  KV event subscriber freshness behind KV-aware routing.
- loxilb_ai_tokens_consumed_total (model, tenant, kind),
  loxilb_ai_token_quota_denied_total (tenant), loxilb_ai_fc_band_total
  (service, band, outcome) — tokens charged, token-quota refusals and the
  capacity gate's outcomes per priority band (ai_traffic_report).

## Known caveats

- When loxilb runs --userservice, the metrics endpoint requires a bearer
  token; unauthenticated Prometheus scrapes receive 401. The bridge
  authenticates automatically, but external scrapers must be configured.
- loxilb_ai_requests_total carries an outcome label: outcome="completed" is
  traffic answered whole, outcome="aborted" is requests whose client did not
  get the whole answer (cause "client" = a cancel or a stall, traffic;
  "engine" or "gateway" = an error; status "none" when no status was sent),
  outcome="denied" is requests the policy gate refused before any backend was
  dialled, and the unfiltered total is offered load. The error ratio is (completed 5xx +
  aborted engine/gateway) / (completed + aborted); any ratio must name the
  outcome on every selector, or a rate-limit storm reads as a backend fault.
- loxilb_ai_rate_limit_hits_total is not a duplicate of the denied series: it
  carries the reason. One request trips one gate but several reason counters
  exist, so expect hits <= requests_total{outcome="denied",status="429"}.
- The bridge reaches gateways of any age. No outcome label means an older
  build whose total excludes denials, and one older still counts only
  SSE-terminated streams — cross-check with the L7 response counters.
- loxilb_ai_token_quota_denied_total is the union of both token-quota
  refusal modes: the tenant over budget (reason token_quota_exceeded on
  loxilb_ai_rate_limit_hits_total) and one request that did not fit the
  headroom (token_quota_would_exceed). Only the hits counter tells them apart.
- loxilb_ai_tokens_consumed_total is fed from the token-quota charge path, so
  it counts only requests charged to a credential. A rule without a
  credential (no api_key_auth) never charges a quota: its completions leave
  the series at 0. Count that traffic with loxilb_ai_requests_total.
- loxilb_ai_admission_queue_wait_seconds has one sample per park, taken when
  the park ends; a request drained from the queue is not sampled.
  loxilb_ai_fc_band_total outcome="queued" counts parks, not requests.
- loxilb_kv_inventory_fresh 0 with loxilb_kv_subscriber_connected 1 means the
  socket is up but the inventory holds nothing from the current publisher:
  KV-aware routing runs cold for that endpoint. An idle engine publishes no
  events, so the last-event age alone is not a fault.
`

// registerResources adds the MCP resources.
func (b *Bridge) registerResources(s *sdk.Server) {
	if len(b.alertRules) > 0 {
		s.AddResource(&sdk.Resource{
			URI:         "loxilb://docs/alerts",
			Name:        "loxilb-alert-rules",
			Description: "Catalog of loxilb Prometheus alert rules with expressions and severities.",
			MIMEType:    "text/markdown",
		}, staticResource("loxilb://docs/alerts", "text/markdown",
			renderAlertRulesDoc(b.alertRules)))
	}

	s.AddResource(&sdk.Resource{
		URI:         "loxilb://docs/metrics",
		Name:        "loxilb-metrics-reference",
		Description: "Curated reference of loxilb Prometheus metric families and known caveats.",
		MIMEType:    "text/markdown",
	}, staticResource("loxilb://docs/metrics", "text/markdown", metricsReferenceDoc))

	if b.cfg.OpenapiSpecPath != "" {
		s.AddResource(&sdk.Resource{
			URI:         "loxilb://spec/openapi",
			Name:        "loxilb-openapi-spec",
			Description: "The loxilb REST API swagger specification (source of truth for endpoint details).",
			MIMEType:    "application/yaml",
		}, b.fileResource("loxilb://spec/openapi", "application/yaml", b.cfg.OpenapiSpecPath))
	}
}

func staticResource(uri, mime, content string) sdk.ResourceHandler {
	return func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{
			{URI: uri, MIMEType: mime, Text: content},
		}}, nil
	}
}

// fileResource serves a fixed, operator-configured file path (never a
// client-supplied one).
func (b *Bridge) fileResource(uri, mime, path string) sdk.ResourceHandler {
	return func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", uri, err)
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{
			{URI: uri, MIMEType: mime, Text: string(raw)},
		}}, nil
	}
}

func renderAlertRulesDoc(rules []tools.AlertRule) string {
	var sb strings.Builder
	sb.WriteString("# loxilb alert rules catalog\n\n")
	group := ""
	for _, r := range rules {
		if r.Group != group {
			group = r.Group
			fmt.Fprintf(&sb, "## group: %s\n\n", group)
		}
		fmt.Fprintf(&sb, "### %s\n", r.Alert)
		if r.Severity != "" {
			fmt.Fprintf(&sb, "- severity: %s\n", r.Severity)
		}
		if r.For != "" {
			fmt.Fprintf(&sb, "- for: %s\n", r.For)
		}
		fmt.Fprintf(&sb, "- expr: `%s`\n", strings.TrimSpace(r.Expr))
		if r.Summary != "" {
			fmt.Fprintf(&sb, "- summary: %s\n", r.Summary)
		}
		if r.Description != "" {
			fmt.Fprintf(&sb, "- description: %s\n", r.Description)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
