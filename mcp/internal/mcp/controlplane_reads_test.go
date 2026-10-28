/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// The PR-B1 control-plane reads: every tool is listed for a viewer session,
// performs only GETs, and projects the payload the contract describes.

var controlPlaneReadTools = []string{
	"lb_get", "lb_kvexact_status", "ai_objective_list", "ai_netidentity_list", "ai_jwtprofile_list",
	"ai_user_ratelimit_list", "ai_user_ratelimit_get", "ai_ratelimit_defaults_get",
	"ai_model_profile_list", "ai_model_profile_get", "status_capabilities", "maintenance_get",
	"halfclose_get", "audit_status", "audit_policy_get", "audit_sink_get", "config_snapshot_get",
}

func callViewer(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %v", tool, err, resultText(res))
	}
	return callOut(t, res)
}

func callRefused(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any, want string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error %v", tool, err)
	}
	if !res.IsError || !strings.Contains(resultText(res), want) {
		t.Fatalf("%s: want a refusal containing %q, got %v", tool, want, resultText(res))
	}
}

func TestControlPlaneReadsAreViewerToolsAndSendNoMutation(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	viewer, _ := listNames(t, b, guard.RoleViewer)
	for _, name := range controlPlaneReadTools {
		if !viewer[name] {
			t.Errorf("viewer does not list %s", name)
		}
	}
	cs := session(t, b, guard.RoleViewer)
	calls := map[string]map[string]any{
		"lb_get":                    {"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp", "include": []string{"status", "stats"}},
		"lb_kvexact_status":         {"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp"},
		"ai_objective_list":         {},
		"ai_netidentity_list":       {},
		"ai_jwtprofile_list":        {},
		"ai_user_ratelimit_list":    {"tenant_id": "lab"},
		"ai_user_ratelimit_get":     {"tenant_id": "lab", "user_id": "alice"},
		"ai_ratelimit_defaults_get": {"scope": "global"},
		"ai_model_profile_list":     {},
		"ai_model_profile_get":      {"profile_id": "qwen3-8b"},
		"status_capabilities":       {},
		"maintenance_get":           {},
		"halfclose_get":             {},
		"audit_status":              {},
		"audit_policy_get":          {},
		"audit_sink_get":            {},
		"config_snapshot_get":       {},
	}
	for _, name := range controlPlaneReadTools {
		out := callViewer(t, cs, name, calls[name])
		if out["target"] != "t1" {
			t.Errorf("%s: target = %v", name, out["target"])
		}
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, r := range mock.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("a read tool sent %s", r)
		}
	}
}

func TestLBGetByKeyAndByIDWithStatusAndStats(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)

	out := callViewer(t, cs, "lb_get", map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "TCP", "include": []string{"status", "stats"}})
	rule, _ := out["rule"].(map[string]any)
	if rule["id"] != "rule-7" || rule["kv_model_profile"] != "qwen3-8b" || rule["endpoint_count"] != float64(1) {
		t.Errorf("rule is not the lb_list projection: %v", rule)
	}
	st, _ := out["status"].(map[string]any)
	if st["operatingStatus"] != "ONLINE" || st["adminStateUp"] != true {
		t.Errorf("status not attached: %v", out["status"])
	}
	stats, _ := out["stats"].(map[string]any)
	if stats["totalConnections"] != float64(42) {
		t.Errorf("stats not attached: %v", out["stats"])
	}
	if !mock.sawMutation("GET /netlox/v1/config/loadbalancer/externalipaddress/1.2.3.4/port/80/protocol/tcp/status") {
		t.Error("status was not read from the composite key")
	}

	out = callViewer(t, cs, "lb_get", map[string]any{"id": "rule-7"})
	rule, _ = out["rule"].(map[string]any)
	if rule["id"] != "rule-7" || out["status"] != nil {
		t.Errorf("by id: %v", out)
	}
	if !mock.sawMutation("GET /netlox/v1/config/loadbalancer/id/rule-7") {
		t.Error("id read did not hit GET /config/loadbalancer/id/{id}")
	}

	callRefused(t, cs, "lb_get", map[string]any{"external_ip": "9.9.9.9", "port": 81, "protocol": "tcp"}, "no rule at 9.9.9.9:81/tcp")
	callRefused(t, cs, "lb_get", map[string]any{"id": "rule-7", "external_ip": "1.2.3.4"}, "not both")
	callRefused(t, cs, "lb_get", map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp", "include": []string{"events"}}, "not one of status, stats")
	callRefused(t, cs, "lb_get", map[string]any{"id": "../x"}, "id:")
}

func TestLBKvexactStatusProjectsTheReadModel(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	out := callViewer(t, cs, "lb_kvexact_status", map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp", "model_name": "llama3"})
	if out["count"] != float64(1) {
		t.Fatalf("count = %v", out["count"])
	}
	entries, _ := out["entries"].([]any)
	e, _ := entries[0].(map[string]any)
	if e["enforcedState"] != "enforced" || e["bindingGen"] != float64(7) {
		t.Errorf("entry not projected: %v", e)
	}
	if !mock.sawMutation("GET /netlox/v1/config/loadbalancer/externalipaddress/1.2.3.4/port/80/protocol/tcp/kvexactstatus") {
		t.Error("kvexactstatus path not read")
	}
	callRefused(t, cs, "lb_kvexact_status", map[string]any{"external_ip": "not-an-ip", "port": 80, "protocol": "tcp"}, "external_ip")
}

func TestAIControlPlaneListsProjectTheContract(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)

	out := callViewer(t, cs, "ai_objective_list", nil)
	if out["count"] != float64(1) || out["source"] != "store" || out["store_reachable"] != true {
		t.Errorf("objective list: %v", out)
	}
	out = callViewer(t, cs, "ai_netidentity_list", nil)
	if out["count"] != float64(1) {
		t.Errorf("netidentity list: %v", out)
	}
	out = callViewer(t, cs, "ai_jwtprofile_list", nil)
	profiles, _ := out["profiles"].([]any)
	p, _ := profiles[0].(map[string]any)
	if p["jwks_url"] != "https://issuer.example/jwks.json" {
		t.Errorf("jwt profile list must show the JWKS URL: %v", p)
	}
	if p["token"] != "[REDACTED]" {
		t.Errorf("a secret-shaped key must be redacted: %v", p)
	}
	out = callViewer(t, cs, "ai_user_ratelimit_list", map[string]any{"tenant_id": "lab"})
	if out["count"] != float64(1) || out["tenant_id"] != "lab" {
		t.Errorf("user ratelimit list: %v", out)
	}
	out = callViewer(t, cs, "ai_user_ratelimit_get", map[string]any{"tenant_id": "lab", "user_id": "alice"})
	u, _ := out["user_ratelimit"].(map[string]any)
	if u["rps"] != float64(5) || u["model_limits"] == nil {
		t.Errorf("user ratelimit get: %v", out)
	}
	callRefused(t, cs, "ai_user_ratelimit_get", map[string]any{"tenant_id": "lab", "user_id": "a/b"}, "user_id")

	out = callViewer(t, cs, "ai_ratelimit_defaults_get", map[string]any{"scope": "rule", "rule_ident": "1.2.3.4:80/tcp"})
	dflt, _ := out["defaults"].(map[string]any)
	if dflt["rule_ident"] != "1.2.3.4:80/tcp" {
		t.Errorf("rule_ident must travel as the query parameter: %v", out)
	}
	callRefused(t, cs, "ai_ratelimit_defaults_get", map[string]any{"scope": "tenant"}, "not global or rule")
	callRefused(t, cs, "ai_ratelimit_defaults_get", map[string]any{"scope": "rule"}, "rule_ident is required")
	if mock.sawMutation("GET /netlox/v1/config/ai/ratelimit/defaults/tenant") {
		t.Error("a refused scope reached the gateway")
	}

	out = callViewer(t, cs, "ai_model_profile_list", nil)
	if out["count"] != float64(1) || out["registry_generation"] != float64(4) {
		t.Errorf("model profile list: %v", out)
	}
	out = callViewer(t, cs, "ai_model_profile_get", map[string]any{"profile_id": "qwen3-8b"})
	prof, _ := out["profile"].(map[string]any)
	if prof["baseModel"] != "Qwen/Qwen3-8B" {
		t.Errorf("model profile get: %v", out)
	}
}

func TestStatusCapabilitiesMaintenanceHalfclose(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	out := callViewer(t, cs, "status_capabilities", map[string]any{"model_name": "llama3"})
	nr, _ := out["not_ready"].([]any)
	if out["count"] != float64(2) || len(nr) != 1 || nr[0] != "pii" {
		t.Errorf("capabilities: %v", out)
	}
	out = callViewer(t, cs, "maintenance_get", nil)
	if out["state"] != "idle" || out["refusing_new_inference"] != false {
		t.Errorf("maintenance: %v", out)
	}
	out = callViewer(t, cs, "halfclose_get", nil)
	if out["allow"] != true || out["capSeconds"] != float64(240) {
		t.Errorf("halfclose: %v", out)
	}
	_ = mock
}

func TestAuditReadsChooseTheirOperation(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	out := callViewer(t, cs, "audit_status", nil)
	if out["running"] != true || out["seq_high"] != float64(120) {
		t.Errorf("audit status: %v", out)
	}
	out = callViewer(t, cs, "audit_policy_get", nil)
	if out["max_segment_bytes"] != float64(1048576) {
		t.Errorf("audit policy: %v", out)
	}
	out = callViewer(t, cs, "audit_sink_get", nil)
	sink, _ := out["sink"].(map[string]any)
	if out["named"] != false || sink["address"] != "siem.example:6514" || sink["client_key_path"] != "/etc/loxilb/k.pem" {
		t.Errorf("primary sink (paths are not material and stay): %v", out)
	}
	out = callViewer(t, cs, "audit_sink_get", map[string]any{"name": "second"})
	sink, _ = out["sink"].(map[string]any)
	if out["named"] != true || sink["name"] != "second" {
		t.Errorf("named sink: %v", out)
	}
	if !mock.sawMutation("GET /netlox/v1/audit/sinks/second") || !mock.sawMutation("GET /netlox/v1/audit/sink") {
		t.Error("the two sink operations were not both read")
	}
	callRefused(t, cs, "audit_sink_get", map[string]any{"name": "a/../b"}, "name:")
}

func TestConfigSnapshotGetCapsAndRedacts(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)

	out := callViewer(t, cs, "config_snapshot_get", map[string]any{"components": "loadbalancers"})
	if out["truncated"] != false || out["size_bytes"] == nil || out["sha256"] == nil {
		t.Fatalf("snapshot: %v", out)
	}
	raw, _ := json.Marshal(out["snapshot"])
	text := string(raw)
	if strings.Contains(text, "hunter2") || strings.Contains(text, "sk-secret-123") {
		t.Errorf("secret material leaked: %s", text)
	}
	if !strings.Contains(text, `"components":"loadbalancers"`) {
		t.Errorf("components query not forwarded: %s", text)
	}
	if !strings.Contains(text, `"schema":"1.12"`) {
		t.Errorf("document not returned whole: %s", text)
	}

	out = callViewer(t, cs, "config_snapshot_get", map[string]any{"max_bytes": 32})
	if out["truncated"] != true || out["snapshot"] != nil {
		t.Fatalf("a document over max_bytes must be described, not returned: %v", out)
	}
	sections, _ := out["sections"].(map[string]any)
	if sections["loadbalancers"] == nil || sections["auth"] == nil {
		t.Errorf("sections must list the top-level keys with their sizes: %v", out)
	}
}

// The gateway refuses GET /config/snapshot with 409 while its own
// auto-persist (or a restore) holds the snapshot gate. The tool maps that
// one answer to a structured, retryable error and retries nothing itself
// (D21): the text and the structured content carry the same object.
func TestConfigSnapshotGetMapsTheSnapshotGateBusyAnswer(t *testing.T) {
	mock := newMockLoxilb(t)
	mock.snapshotBusy = true
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "config_snapshot_get", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a 409 must be a tool error, got %v", resultText(res))
	}
	var verdict map[string]any
	if err := json.Unmarshal([]byte(resultText(res)), &verdict); err != nil {
		t.Fatalf("the error text must be the structured verdict, got %q: %v", resultText(res), err)
	}
	if verdict["error"] != "snapshot_gate_busy" || verdict["retry_after_sec"] != float64(3) {
		t.Fatalf("verdict: %v", verdict)
	}
	if d, _ := verdict["detail"].(string); !strings.Contains(d, "another snapshot or restore operation is in progress") {
		t.Fatalf("detail must carry the gateway's text, got %v", verdict)
	}
	if out := callOut(t, res); out["error"] != "snapshot_gate_busy" || out["retry_after_sec"] != float64(3) {
		t.Fatalf("structured content must carry the same verdict: %v", out)
	}
	n := 0
	for _, r := range mock.requests {
		if r == "GET /netlox/v1/config/snapshot" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the tool must not retry on the caller's behalf: %d snapshot GETs", n)
	}
}
