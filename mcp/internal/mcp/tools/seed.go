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

// Package tools implements the loxilb-mcp tool set. ships the five
// seed tools: version_get, health_overview, lb_list,
// ct_list, metrics_snapshot. Later phases add the full domain files.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/client"
	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// Deps is what tool handlers need from the bridge.
type Deps struct {
	// Resolve maps a target name ("" = default) to its REST client.
	Resolve func(name string) (*client.Client, error)
	// Targets lists configured target names (for tool descriptions).
	Targets []string
	// Audit receives tool_call events (may be nil).
	Audit *guard.Auditor
	// Prom is the optional external Prometheus backend (nil disables
	// promql_* tools).
	Prom *client.PromClient
	// Alertmanager is the optional Alertmanager backend (nil disables
	// alerts_active).
	Alertmanager *client.AlertmanagerClient
	// AlertRules is the parsed alert-rule catalog (empty disables
	// alerts_catalog and the docs/alerts resource).
	AlertRules []AlertRule
	// Confirm gates destructive tools behind the preview→confirm-token flow.
	// nil (--no-confirm) executes destructive tools directly (CI use).
	Confirm *guard.Confirmer
	// AllowImport enables config_import (off by default even for admins).
	AllowImport bool
	// SecretsDir receives files holding secret material that must not enter
	// model context (ai_apikey_create raw keys, threat T5). Empty disables
	// the file sink; ai_apikey_create then requires reveal=true.
	SecretsDir string
	// Autopilot reports whether a destructive tool may execute without the
	// confirm-token step (§3.7 closed-loop hooks). nil = no autopilot tools.
	Autopilot func(tool string) bool
	// ResolveAll returns every configured target client, sorted by name
	// (multi-target fan-out).
	ResolveAll func() []*client.Client
	// Budget meters executed mutations per caller (change-gate requirement
	// 4). The REST clients enforce it; the change gate consults it before it
	// redeems a token so a refused apply does not burn the preview. nil
	// meters nothing.
	Budget *guard.ChangeBudget
}

// toolMeta records the policy metadata of every tool a Register* function
// has seen, permitted or not, so the bridge can tell a read tool from a
// mutation by name (the budget's consecutive-failure stop is lifted by a
// successful read tool call, not by the reads a mutation performs itself).
var (
	toolMetaMu sync.RWMutex
	toolMeta   = map[string]guard.ToolMeta{}
)

func noteMeta(meta guard.ToolMeta) {
	toolMetaMu.Lock()
	toolMeta[meta.Name] = meta
	toolMetaMu.Unlock()
}

// permit notes a tool's metadata and asks the policy whether the role may
// use it; every registration goes through it (or a reg closure that calls
// noteMeta) so IsMutating knows every tool, listed for this role or not.
func permit(pol *guard.Policy, role guard.Role, meta guard.ToolMeta) bool {
	noteMeta(meta)
	return pol.Permits(role, meta)
}

// IsMutating reports whether the named tool changes the gateway. An
// unknown name counts as mutating (the safe reading for the budget).
func IsMutating(name string) bool {
	toolMetaMu.RLock()
	defer toolMetaMu.RUnlock()
	m, ok := toolMeta[name]
	return !ok || m.Mutating
}

const (
	domainMgmt       = "mgmt"
	domainAnalysis   = "analysis"
	domainMonitoring = "monitoring"
	domainAI         = "ai"
)

func boolPtr(b bool) *bool { return &b }

// boolPtrValue returns a pointer to v when it is a JSON boolean and nil
// otherwise, so an absent read-back field stays absent in the projection.
func boolPtrValue(v any) *bool {
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

// cleanStrings applies clean to every element; nil stays nil.
func cleanStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, clean(s))
	}
	return out
}

// roAnnotations marks a tool read-only and closed-world per MCP spec hints.
func roAnnotations(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(false),
	}
}

// mutAnnotations marks a mutating, non-destructive tool.
func mutAnnotations(title string, idempotent bool) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: boolPtr(false),
		IdempotentHint:  idempotent,
		OpenWorldHint:   boolPtr(false),
	}
}

// destAnnotations marks a destructive tool (confirm-token gated).
func destAnnotations(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: boolPtr(true),
		OpenWorldHint:   boolPtr(false),
	}
}

// clean sanitizes remote-originated strings before they reach model context:
// control characters stripped, length capped.
func clean(s string) string {
	const maxLen = 256
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxLen {
			break
		}
	}
	return b.String()
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func targetDesc(targets []string) string {
	return "Target loxilb instance name (omit for default). Configured: " +
		strings.Join(targets, ", ")
}

// RegisterSeed adds every seed tool permitted by the policy for the role.
func RegisterSeed(s *sdk.Server, role guard.Role, pol *guard.Policy, deps *Deps) {
	tdesc := targetDesc(deps.Targets)

	if permit(pol, role, guard.ToolMeta{Name: "version_get", Domain: domainAnalysis}) {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "version_get",
			Description: "Get the loxilb software version and build info of a target instance.",
			Annotations: roAnnotations("Get loxilb version"),
		}, deps.versionGet(tdesc))
	}
	if permit(pol, role, guard.ToolMeta{Name: "lb_list", Domain: domainMgmt}) {
		sdk.AddTool(s, &sdk.Tool{
			Name: "lb_list",
			Description: "List load-balancer rules (service VIP:port/protocol, mode, endpoint count " +
				"and states). Supports substring filtering and limiting; reports total_count when truncated.",
			Annotations: roAnnotations("List load-balancer rules"),
		}, deps.lbList(tdesc))
	}
	if permit(pol, role, guard.ToolMeta{Name: "ct_list", Domain: domainAnalysis}) {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ct_list",
			Description: "Summarize live connection-tracking entries: aggregate counts by state and " +
				"protocol plus a bounded sample of matching entries. Never dumps the full table.",
			Annotations: roAnnotations("Summarize conntrack table"),
		}, deps.ctList(tdesc))
	}
	if permit(pol, role, guard.ToolMeta{Name: "metrics_snapshot", Domain: domainMonitoring}) {
		sdk.AddTool(s, &sdk.Tool{
			Name: "metrics_snapshot",
			Description: "Scrape the loxilb Prometheus endpoint and return parsed metric families. " +
				"Use 'families' globs (e.g. loxilb_ai_*, loxilb_l4_error_events_total) to narrow output.",
			Annotations: roAnnotations("Snapshot Prometheus metrics"),
		}, deps.metricsSnapshot(tdesc))
	}
	if permit(pol, role, guard.ToolMeta{Name: "health_overview", Domain: domainAnalysis}) {
		sdk.AddTool(s, &sdk.Tool{
			Name: "health_overview",
			Description: "Start-here health check of a loxilb instance: reachability, version, " +
				"LB rule count, conntrack totals by state, and metric family count. " +
				"Sections degrade independently with per-section errors.",
			Annotations: roAnnotations("loxilb health overview"),
		}, deps.healthOverview(tdesc))
	}
}

func (d *Deps) audit(tool, target string, ok bool, errStr string) {
	d.Audit.Log(guard.Event{
		Kind: guard.EventToolCall, Tool: tool, Target: target, OK: ok, Err: errStr,
	})
}

// auditMut records a mutating call including its (redacted) arguments.
func (d *Deps) auditMut(tool, target string, args any, ok bool, errStr string) {
	var argMap map[string]any
	if raw, err := json.Marshal(args); err == nil {
		_ = json.Unmarshal(raw, &argMap)
	}
	d.Audit.Log(guard.Event{
		Kind: guard.EventToolCall, Tool: tool, Target: target,
		Args: guard.Redact(argMap), OK: ok, Err: errStr,
	})
}

// ---- version_get ----

type versionIn struct {
	Target string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
}

type versionOut struct {
	Target    string   `json:"target"`
	Version   string   `json:"version"`
	BuildInfo string   `json:"build_info,omitempty"`
	Product   string   `json:"product,omitempty" jsonschema:"product identifier; loxilb-inference-gateway on this gateway, absent on upstream loxilb and on gateway builds that predate the field"`
	Features  []string `json:"features,omitempty" jsonschema:"gateway-only capability tokens this build implements (epp, api_key_auth, jwt_auth_profile, ...); absent on builds that predate the field"`
}

func (d *Deps) versionGet(string) sdk.ToolHandlerFor[versionIn, versionOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in versionIn) (*sdk.CallToolResult, versionOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, versionOut{}, err
		}
		v, err := c.Version(ctx)
		if err != nil {
			return nil, versionOut{}, err
		}
		return nil, versionOut{
			Target:    c.Name(),
			Version:   clean(v.Version),
			BuildInfo: clean(v.BuildInfo),
			Product:   clean(v.Product),
			Features:  cleanStrings(v.Features),
		}, nil
	}
}

// ---- lb_list ----

type lbListIn struct {
	Target string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Filter string `json:"filter,omitempty" jsonschema:"case-insensitive substring matched against service name and external IP"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum rules to return (default 50, max 500)"`
}

type lbRule struct {
	Name             string `json:"name,omitempty"`
	ExternalIP       string `json:"external_ip"`
	Port             any    `json:"port" jsonschema:"service port (number, or string for a port range)"`
	Protocol         string `json:"protocol"`
	Mode             any    `json:"mode,omitempty" jsonschema:"load-balancer mode as returned by loxilb (number or string)"`
	Host             string `json:"host,omitempty"`
	PathPrefix       string `json:"path_prefix,omitempty"`
	ModelName        string `json:"model_name,omitempty"`
	SSEMode          bool   `json:"sse_mode,omitempty"`
	PDDisaggMode     bool   `json:"pd_disagg_mode,omitempty"`
	PDCacheAwareMode bool   `json:"pd_cache_aware_mode,omitempty"`
	APIKeyAuth       string `json:"api_key_auth,omitempty"`
	KVExactMode      int    `json:"kv_exact_mode,omitempty"`
	KVEngineType     string `json:"kv_engine_type,omitempty"`
	ID               string `json:"id,omitempty" jsonschema:"rule identifier assigned by the gateway"`
	AdminStateUp     *bool  `json:"admin_state_up,omitempty" jsonschema:"administrative state; false pauses the rule without deleting it"`
	Security         any    `json:"security,omitempty" jsonschema:"TLS security mode of the listener as returned by the gateway"`
	JWTAuthProfile   string `json:"jwt_auth_profile,omitempty" jsonschema:"JWT auth profile the Bearer arm of api_key_auth validates against"`
	EPPEndpoint      string `json:"epp_endpoint,omitempty" jsonschema:"Endpoint Picker (EPP) address consulted by the data plane"`
	KVExactAPIMode   string `json:"kv_exact_api_mode,omitempty" jsonschema:"request API surfaces a KV-exact rule serves, as declared"`
	KVModelProfile   string `json:"kv_model_profile,omitempty" jsonschema:"KV model profile bound to the rule"`
	FCEffective      any    `json:"fc_effective,omitempty" jsonschema:"flow-control values in force, resolved by the data plane (read-back only)"`
	BackendTLSEff    any    `json:"backend_tls_effective,omitempty" jsonschema:"backend TLS state in force, resolved by the data plane (read-back only)"`
	HalfCloseEff     any    `json:"half_close_effective,omitempty" jsonschema:"half-close handling in force, resolved by the data plane (read-back only)"`
	EndpointCount    int    `json:"endpoint_count"`
	Endpoints        []any  `json:"endpoints,omitempty"`
}

type lbListOut struct {
	Target     string   `json:"target"`
	TotalCount int      `json:"total_count"`
	Returned   int      `json:"returned"`
	Rules      []lbRule `json:"rules"`
}

func (d *Deps) lbList(string) sdk.ToolHandlerFor[lbListIn, lbListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in lbListIn) (*sdk.CallToolResult, lbListOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, lbListOut{}, err
		}
		raw, err := c.LBRules(ctx)
		if err != nil {
			d.audit("lb_list", c.Name(), false, err.Error())
			return nil, lbListOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 500 {
			limit = 500
		}
		filter := strings.ToLower(in.Filter)

		var matched []lbRule
		for _, entry := range raw {
			svc, _ := entry["serviceArguments"].(map[string]any)
			if svc == nil {
				continue
			}
			name, _ := svc["name"].(string)
			extIP, _ := svc["externalIP"].(string)
			if filter != "" &&
				!strings.Contains(strings.ToLower(name), filter) &&
				!strings.Contains(strings.ToLower(extIP), filter) {
				continue
			}
			rule, ok := projectLBRule(entry)
			if !ok {
				continue
			}
			matched = append(matched, rule)
		}
		out := lbListOut{Target: c.Name(), TotalCount: len(matched)}
		if len(matched) > limit {
			matched = matched[:limit]
		}
		out.Rules = matched
		out.Returned = len(matched)
		d.audit("lb_list", c.Name(), true, "")
		return nil, out, nil
	}
}

// projectLBRule is the read-back projection of one LoadbalanceEntry, shared
// by lb_list and lb_get. ok is false when the entry has no serviceArguments.
func projectLBRule(entry map[string]any) (lbRule, bool) {
	svc, _ := entry["serviceArguments"].(map[string]any)
	if svc == nil {
		return lbRule{}, false
	}
	name, _ := svc["name"].(string)
	extIP, _ := svc["externalIP"].(string)
	proto, _ := svc["protocol"].(string)
	eps, _ := entry["endpoints"].([]any)
	rule := lbRule{
		Name:             clean(name),
		ExternalIP:       clean(extIP),
		Port:             svc["port"],
		Protocol:         clean(proto),
		Mode:             svc["mode"],
		Host:             clean(stringValue(svc["host"])),
		PathPrefix:       clean(stringValue(svc["path_prefix"])),
		ModelName:        clean(stringValue(svc["model_name"])),
		SSEMode:          boolValue(svc["sse_mode"]),
		PDDisaggMode:     boolValue(svc["pd_disagg_mode"]),
		PDCacheAwareMode: boolValue(svc["pd_cache_aware_mode"]),
		APIKeyAuth:       clean(stringValue(svc["api_key_auth"])),
		KVExactMode:      intValue(svc["kvExactMode"]),
		KVEngineType:     clean(stringValue(svc["kvEngineType"])),
		ID:               clean(stringValue(svc["id"])),
		AdminStateUp:     boolPtrValue(svc["adminStateUp"]),
		Security:         sanitizeAny(svc["security"], 0),
		JWTAuthProfile:   clean(stringValue(svc["jwt_auth_profile"])),
		EPPEndpoint:      clean(stringValue(svc["eppEndpoint"])),
		KVExactAPIMode:   clean(stringValue(svc["kvExactApiMode"])),
		KVModelProfile:   clean(stringValue(svc["kvModelProfile"])),
		FCEffective:      sanitizeAny(svc["fc_effective"], 0),
		BackendTLSEff:    sanitizeAny(svc["backend_tls_effective"], 0),
		HalfCloseEff:     sanitizeAny(svc["half_close_effective"], 0),
		EndpointCount:    len(eps),
	}
	for _, epRaw := range eps {
		ep, _ := epRaw.(map[string]any)
		if ep == nil {
			continue
		}
		ip, _ := ep["endpointIP"].(string)
		state, _ := ep["state"].(string)
		rule.Endpoints = append(rule.Endpoints, map[string]any{
			"ip":          clean(ip),
			"target_port": ep["targetPort"],
			"state":       clean(state),
			"weight":      ep["weight"],
			"ep_role":     ep["ep_role"],
			"nixl_port":   ep["nixl_port"],
		})
	}
	return rule, true
}

// ---- ct_list ----

type ctListIn struct {
	Target   string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Protocol string `json:"protocol,omitempty" jsonschema:"filter by protocol (tcp|udp|sctp|icmp)"`
	State    string `json:"state,omitempty" jsonschema:"filter by conntrack state substring (e.g. est, closed, sync)"`
	Service  string `json:"service,omitempty" jsonschema:"filter by service name substring"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum sample entries to return (default 50, max 500)"`
}

type ctEntry struct {
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Proto   string `json:"proto"`
	State   string `json:"state"`
	Service string `json:"service,omitempty"`
	Packets int64  `json:"packets"`
	Bytes   int64  `json:"bytes"`
	AgeMs   uint64 `json:"age_ms,omitempty"`
}

type ctListOut struct {
	Target     string         `json:"target"`
	TotalCount int            `json:"total_count"`
	Matched    int            `json:"matched"`
	ByState    map[string]int `json:"by_state"`
	ByProto    map[string]int `json:"by_proto"`
	Returned   int            `json:"returned"`
	Entries    []ctEntry      `json:"entries"`
}

func (d *Deps) ctList(string) sdk.ToolHandlerFor[ctListIn, ctListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ctListIn) (*sdk.CallToolResult, ctListOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, ctListOut{}, err
		}
		cts, err := c.Conntracks(ctx)
		if err != nil {
			d.audit("ct_list", c.Name(), false, err.Error())
			return nil, ctListOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 500 {
			limit = 500
		}
		out := ctListOut{
			Target:     c.Name(),
			TotalCount: len(cts),
			ByState:    map[string]int{},
			ByProto:    map[string]int{},
		}
		proto := strings.ToLower(in.Protocol)
		state := strings.ToLower(in.State)
		service := strings.ToLower(in.Service)
		for _, ct := range cts {
			if proto != "" && !strings.EqualFold(ct.Protocol, proto) {
				continue
			}
			if state != "" && !strings.Contains(strings.ToLower(ct.State), state) {
				continue
			}
			if service != "" && !strings.Contains(strings.ToLower(ct.ServName), service) {
				continue
			}
			out.Matched++
			out.ByState[clean(strings.ToLower(ct.State))]++
			out.ByProto[clean(strings.ToLower(ct.Protocol))]++
			if len(out.Entries) < limit {
				out.Entries = append(out.Entries, ctEntry{
					Src:     clean(fmt.Sprintf("%s:%d", ct.SourceIP, ct.SourcePort)),
					Dst:     clean(fmt.Sprintf("%s:%d", ct.DestinationIP, ct.DestinationPort)),
					Proto:   clean(ct.Protocol),
					State:   clean(ct.State),
					Service: clean(ct.ServName),
					Packets: ct.Packets,
					Bytes:   ct.Bytes,
					AgeMs:   ct.AgeMs,
				})
			}
		}
		out.Returned = len(out.Entries)
		d.audit("ct_list", c.Name(), true, "")
		return nil, out, nil
	}
}

// ---- metrics_snapshot ----

type metricsIn struct {
	Target     string   `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Families   []string `json:"families,omitempty" jsonschema:"glob patterns of metric family names to include (e.g. loxilb_ai_*); empty means all"`
	MaxSamples int      `json:"max_samples,omitempty" jsonschema:"cap on samples per family (default 200)"`
}

type metricsOut struct {
	Target      string          `json:"target"`
	FamilyCount int             `json:"family_count"`
	Families    []client.Family `json:"families"`
}

func (d *Deps) metricsSnapshot(string) sdk.ToolHandlerFor[metricsIn, metricsOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in metricsIn) (*sdk.CallToolResult, metricsOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, metricsOut{}, err
		}
		text, err := c.MetricsText(ctx)
		if err != nil {
			d.audit("metrics_snapshot", c.Name(), false, err.Error())
			return nil, metricsOut{}, err
		}
		fams, err := client.ParseFamilies(text, in.Families, in.MaxSamples)
		if err != nil {
			return nil, metricsOut{}, err
		}
		d.audit("metrics_snapshot", c.Name(), true, "")
		return nil, metricsOut{Target: c.Name(), FamilyCount: len(fams), Families: fams}, nil
	}
}

// ---- health_overview ----

type healthIn struct {
	Target string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
}

type healthOut struct {
	Target         string            `json:"target"`
	Reachable      bool              `json:"reachable"`
	Version        string            `json:"version,omitempty"`
	Product        string            `json:"product,omitempty" jsonschema:"product identifier from GET /version; absent on upstream loxilb and on builds that predate the field"`
	Features       []string          `json:"features,omitempty" jsonschema:"gateway-only capability tokens from GET /version; absent on builds that predate the field"`
	Ready          *bool             `json:"ready,omitempty" jsonschema:"configuration-readiness verdict of GET /status/ready; absent when the endpoint is unavailable (see errors.ready)"`
	ReadyReasons   []string          `json:"ready_reasons,omitempty" jsonschema:"why the gateway is not ready; empty when ready"`
	LBRuleCount    int               `json:"lb_rule_count"`
	ConntrackTotal int               `json:"conntrack_total"`
	CtByState      map[string]int    `json:"ct_by_state,omitempty"`
	MetricFamilies int               `json:"metric_families"`
	Errors         map[string]string `json:"errors,omitempty"`
}

func (d *Deps) healthOverview(string) sdk.ToolHandlerFor[healthIn, healthOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in healthIn) (*sdk.CallToolResult, healthOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, healthOut{}, err
		}
		out := d.healthProbe(ctx, c)
		// If every section failed, surface a real error so the model sees
		// IsError instead of an all-zero overview.
		if !out.Reachable && out.Errors != nil && len(out.Errors) >= 4 {
			keys := make([]string, 0, len(out.Errors))
			for k := range out.Errors {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			d.audit("health_overview", c.Name(), false, out.Errors[keys[0]])
			return nil, healthOut{}, fmt.Errorf("target %s unreachable: %s", c.Name(), out.Errors[keys[0]])
		}
		d.audit("health_overview", c.Name(), true, "")
		return nil, out, nil
	}
}

// healthProbe gathers the health sections of one target concurrently; failing
// sections degrade into Errors entries.
func (d *Deps) healthProbe(ctx context.Context, c *client.Client) healthOut {
	out := healthOut{Target: c.Name(), Errors: map[string]string{}}
	{
		var mu sync.Mutex
		var wg sync.WaitGroup
		fail := func(section string, err error) {
			mu.Lock()
			out.Errors[section] = clean(err.Error())
			mu.Unlock()
		}

		wg.Add(5)
		go func() {
			defer wg.Done()
			if v, err := c.Version(ctx); err != nil {
				fail("version", err)
			} else {
				mu.Lock()
				out.Version = clean(v.Version)
				out.Product = clean(v.Product)
				out.Features = cleanStrings(v.Features)
				out.Reachable = true
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			// GET /status/ready: 200 and 503 both carry the verdict; an
			// older build without the endpoint degrades into errors.ready.
			if rs, err := c.Ready(ctx); err != nil {
				fail("ready", err)
			} else {
				ready := rs.Ready
				mu.Lock()
				out.Ready = &ready
				out.ReadyReasons = cleanStrings(rs.Reasons)
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			if rules, err := c.LBRules(ctx); err != nil {
				fail("loadbalancer", err)
			} else {
				mu.Lock()
				out.LBRuleCount = len(rules)
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			if cts, err := c.Conntracks(ctx); err != nil {
				fail("conntrack", err)
			} else {
				byState := map[string]int{}
				for _, ct := range cts {
					byState[clean(strings.ToLower(ct.State))]++
				}
				mu.Lock()
				out.ConntrackTotal = len(cts)
				out.CtByState = byState
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			if text, err := c.MetricsText(ctx); err != nil {
				fail("metrics", err)
			} else if fams, err := client.ParseFamilies(text, nil, 1); err != nil {
				fail("metrics", err)
			} else {
				mu.Lock()
				out.MetricFamilies = len(fams)
				mu.Unlock()
			}
		}()
		wg.Wait()
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return out
}
