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

package tools

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/client"
)

// The evidence sections diagnose_ai_latency builds from the capacity gate,
// the Endpoint Picker, the KV event subscriber and the token quotas. Each
// reads one scrape; a section whose families the gateway does not emit is
// left out (an older gateway, or the feature is not in use).

// diagnoseAIFamilies are the globs the sections need beside the latency
// histograms.
var diagnoseAIFamilies = []string{
	"loxilb_ai_admission_mode", "loxilb_ai_admission_inflight",
	"loxilb_ai_admission_effective_limit", "loxilb_ai_admission_queue_wait_seconds",
	"loxilb_pd_admission_*shed*",
	"loxilb_ai_epp_duration_seconds", "loxilb_ai_epp_candidates_rejected_total", "loxilb_ai_epp_pending",
	"loxilb_kv_inventory_fresh", "loxilb_kv_subscriber_connected",
	"loxilb_kv_subscriber_last_event_timestamp_seconds",
	"loxilb_ai_token_quota_denied_total", "loxilb_ai_rate_limit_hits_total",
}

// Values of the reason label on loxilb_ai_rate_limit_hits_total that split
// loxilb_ai_token_quota_denied_total, which is their union.
const (
	reasonTokenQuotaExceeded    = "token_quota_exceeded"
	reasonTokenQuotaWouldExceed = "token_quota_would_exceed"
)

// admissionCaveat and tokenQuotaCaveat say how the two families with the
// easiest misreading count.
const admissionCaveat = "admission.queue_wait has one sample per park, taken when the park ends " +
	"(admitted, expired, shed or cancelled) and measured from the first park; a request drained " +
	"from the queue is not sampled. admission.inflight is the pool-wide count (role service); " +
	"effective_limit 0 is unlimited"

const tokenQuotaCaveat = "quota_denied (token-quota refusals per tenant) is the union of both token-quota refusal modes: " +
	"exhausted (the tenant is over budget until the bucket refills) and would_exceed (this " +
	"request's worst case did not fit the headroom; a smaller one is still admitted), split by " +
	"the reason label of loxilb_ai_rate_limit_hits_total"

// admissionModeNames decodes loxilb_ai_admission_mode.
var admissionModeNames = map[float64]string{0: "off", 1: "observe", 2: "enforce"}

type admissionPool struct {
	Service        string          `json:"service"`
	Pool           string          `json:"pool"`
	Mode           string          `json:"mode"`
	Inflight       float64         `json:"inflight"`
	EffectiveLimit float64         `json:"effective_limit"`
	AtCeiling      bool            `json:"at_ceiling,omitempty"`
	QueueWait      *latencySummary `json:"queue_wait,omitempty"`
}

type kvFreshnessRow struct {
	Service        string   `json:"service"`
	Endpoint       string   `json:"ep"`
	Connected      *float64 `json:"connected,omitempty"`
	InventoryFresh *float64 `json:"inventory_fresh,omitempty"`
	LastEventAgeS  *float64 `json:"last_event_age_seconds,omitempty"`
	// NoEventAccepted marks a connected subscriber with no last-event
	// series: it has accepted no event batch since it started.
	NoEventAccepted bool   `json:"no_event_accepted,omitempty"`
	Problem         string `json:"problem,omitempty"`
}

type tokenQuotaRow struct {
	Tenant string  `json:"tenant"`
	Denied float64 `json:"denied"`
	// Exhausted and WouldExceed split Denied by the hits counter's reason:
	// the tenant over budget, or one request that did not fit the headroom.
	Exhausted   float64 `json:"exhausted"`
	WouldExceed float64 `json:"would_exceed"`
}

// poolKey is a sample's (service, pool) pair.
func poolKey(s client.Sample) [2]string { return [2]string{s.Labels["service"], s.Labels["pool"]} }

// admissionSection reads the capacity gate per pool. Pools are reported in
// service, pool order.
func admissionSection(byName map[string]client.Family) []admissionPool {
	modes, ok := byName["loxilb_ai_admission_mode"]
	if !ok {
		return nil
	}
	pools := map[[2]string]*admissionPool{}
	for _, s := range modes.Samples {
		k := poolKey(s)
		mode, known := admissionModeNames[s.Value]
		if !known {
			mode = strconv.FormatFloat(s.Value, 'f', -1, 64)
		}
		pools[k] = &admissionPool{Service: clean(k[0]), Pool: clean(k[1]), Mode: mode}
	}
	for _, s := range byName["loxilb_ai_admission_inflight"].Samples {
		// role="service" is the pool-wide count; the other roles are
		// per-endpoint sums of a different ceiling.
		if p := pools[poolKey(s)]; p != nil && s.Labels["role"] == "service" {
			p.Inflight = s.Value
		}
	}
	for _, s := range byName["loxilb_ai_admission_effective_limit"].Samples {
		if p := pools[poolKey(s)]; p != nil {
			p.EffectiveLimit = s.Value
		}
	}
	waits := map[[2]string][]client.Sample{}
	for _, s := range byName["loxilb_ai_admission_queue_wait_seconds"].Samples {
		waits[poolKey(s)] = append(waits[poolKey(s)], s)
	}
	out := make([]admissionPool, 0, len(pools))
	for k, p := range pools {
		p.QueueWait = summarizeHistogram(client.Family{Samples: waits[k]})
		// 0 is unlimited; at the ceiling only matters where the gate refuses.
		p.AtCeiling = p.Mode == "enforce" && p.EffectiveLimit > 0 && p.Inflight >= p.EffectiveLimit
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Pool < out[j].Pool
	})
	return out
}

// pdAdmissionSection reads the per-endpoint P/D admission sheds.
func pdAdmissionSection(byName map[string]client.Family) map[string]float64 {
	out := map[string]float64{}
	for _, name := range []string{"loxilb_pd_admission_shed_total", "loxilb_pd_admission_overflow_shed_total"} {
		if f, ok := byName[name]; ok {
			out[name] = sumFamily(f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// eppSection reads the Endpoint Picker: its request-phase latency, the
// candidates the data plane skipped by reason, and the decisions it holds.
func eppSection(byName map[string]client.Family) map[string]any {
	out := map[string]any{}
	if s := summarizeHistogram(byName["loxilb_ai_epp_duration_seconds"]); s != nil {
		out["duration"] = s
	}
	if f, ok := byName["loxilb_ai_epp_candidates_rejected_total"]; ok {
		rej := map[string]float64{}
		for _, s := range f.Samples {
			if s.Value > 0 {
				rej[clean(s.Labels["reason"])] += s.Value
			}
		}
		out["candidates_rejected"] = rej
	}
	if f, ok := byName["loxilb_ai_epp_pending"]; ok {
		out["pending"] = sumFamily(f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// kvFreshnessSection reads the KV event subscriber per (service, endpoint).
// now is the bridge's clock: the age is only as true as the two clocks agree.
func kvFreshnessSection(byName map[string]client.Family, now time.Time) []kvFreshnessRow {
	rows := map[[2]string]*kvFreshnessRow{}
	row := func(s client.Sample) *kvFreshnessRow {
		k := [2]string{s.Labels["service"], s.Labels["ep"]}
		if rows[k] == nil {
			rows[k] = &kvFreshnessRow{Service: clean(k[0]), Endpoint: clean(k[1])}
		}
		return rows[k]
	}
	for _, s := range byName["loxilb_kv_subscriber_connected"].Samples {
		v := s.Value
		row(s).Connected = &v
	}
	for _, s := range byName["loxilb_kv_inventory_fresh"].Samples {
		v := s.Value
		row(s).InventoryFresh = &v
	}
	for _, s := range byName["loxilb_kv_subscriber_last_event_timestamp_seconds"].Samples {
		if s.Value > 0 {
			age := now.Sub(time.Unix(0, int64(s.Value*1e9))).Seconds()
			if age < 0 {
				age = 0
			}
			row(s).LastEventAgeS = &age
		}
	}
	out := make([]kvFreshnessRow, 0, len(rows))
	for _, r := range rows {
		r.NoEventAccepted = r.Connected != nil && *r.Connected == 1 && r.LastEventAgeS == nil
		switch {
		case r.Connected != nil && *r.Connected == 0:
			r.Problem = "disconnected"
		case r.InventoryFresh != nil && *r.InventoryFresh == 0:
			// The socket looks fine, but the inventory holds nothing from
			// the current publisher: KV-aware routing runs cold here.
			r.Problem = "inventory_not_fresh"
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Endpoint < out[j].Endpoint
	})
	return out
}

// tokenQuotaSection reads the token-quota refusals per tenant, split by the
// two refusal modes the hits counter names.
func tokenQuotaSection(byName map[string]client.Family) []tokenQuotaRow {
	denied, ok := byName["loxilb_ai_token_quota_denied_total"]
	if !ok {
		return nil
	}
	rows := map[string]*tokenQuotaRow{}
	for _, s := range denied.Samples {
		if s.Value == 0 {
			continue
		}
		t := s.Labels["tenant"]
		if rows[t] == nil {
			rows[t] = &tokenQuotaRow{Tenant: clean(t)}
		}
		rows[t].Denied += s.Value
	}
	for _, s := range byName["loxilb_ai_rate_limit_hits_total"].Samples {
		r := rows[s.Labels["tenant"]]
		if r == nil {
			continue
		}
		switch s.Labels["reason"] {
		case reasonTokenQuotaExceeded:
			r.Exhausted += s.Value
		case reasonTokenQuotaWouldExceed:
			r.WouldExceed += s.Value
		}
	}
	out := make([]tokenQuotaRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out
}

// serviceKey splits a service label written VIP:port into lb_get's
// composite key. AI-gateway services are tcp. ok is false for any other
// shape.
func serviceKey(service string) (ip string, port int, ok bool) {
	host, p, err := net.SplitHostPort(service)
	if err != nil || net.ParseIP(host) == nil {
		return "", 0, false
	}
	port, err = strconv.Atoi(p)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, false
	}
	return host, port, true
}

// aiSectionActions proposes the read-only follow-ups the sections justify.
func aiSectionActions(target string, pools []admissionPool, kv []kvFreshnessRow, quota []tokenQuotaRow) []suggestedAction {
	var acts []suggestedAction
	for _, p := range pools {
		if !p.AtCeiling {
			continue
		}
		ip, port, ok := serviceKey(p.Service)
		if !ok {
			continue
		}
		acts = append(acts, suggestedAction{
			Tool: "lb_get",
			Args: map[string]any{"external_ip": ip, "port": port, "protocol": "tcp", "target": target},
			Rationale: fmt.Sprintf("pool %s holds %g of its ceiling %g (enforce): requests over it queue or are refused - "+
				"fc_effective shows the ceiling in force; a change is an lb_create replace with fc_* (admin-confirm preview)",
				p.Pool, p.Inflight, p.EffectiveLimit),
			Risk: "none",
		})
	}
	seen := map[string]bool{}
	for _, r := range kv {
		if r.Problem == "" || seen[r.Service] {
			continue
		}
		seen[r.Service] = true
		rationale := fmt.Sprintf("KV subscriber for endpoint %s of service %s is %s - KV-aware routing for it runs without a current inventory",
			r.Endpoint, r.Service, r.Problem)
		// The label is the gateway's numeric service id on current builds
		// (no REST read maps it to a rule); a VIP:port label names the rule.
		if ip, port, ok := serviceKey(r.Service); ok {
			acts = append(acts, suggestedAction{
				Tool:      "lb_kvexact_status",
				Args:      map[string]any{"external_ip": ip, "port": port, "protocol": "tcp", "target": target},
				Rationale: rationale, Risk: "none",
			})
		} else if id, err := strconv.ParseInt(r.Service, 10, 64); err == nil {
			ep, err := strconv.Atoi(r.Endpoint)
			if err != nil {
				continue
			}
			acts = append(acts, suggestedAction{
				Tool:      "ai_kv_inventory_get",
				Args:      map[string]any{"service_id": id, "ep_idx": ep, "target": target},
				Rationale: rationale + "; read that endpoint's block inventory (the series' service label is the numeric service id)",
				Risk:      "none",
			})
		}
	}
	for _, q := range quota {
		acts = append(acts, suggestedAction{
			Tool: "ai_ratelimit_get",
			Args: map[string]any{"tenant_id": q.Tenant, "target": target},
			Rationale: fmt.Sprintf("tenant %s had %g requests refused by a token quota (%g over budget, %g did not fit the headroom) - "+
				"read its limits before reading the latency as a backend fault", q.Tenant, q.Denied, q.Exhausted, q.WouldExceed),
			Risk: "none",
		})
	}
	return acts
}
