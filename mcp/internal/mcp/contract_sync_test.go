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
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// The bridge follows the REST contract for the fields the gap analysis found
// it misreading (W1-W7) and projects the rule read-back the contract added.

func lbCreateArgs(extra map[string]any) map[string]any {
	args := map[string]any{
		"external_ip": "20.20.20.3", "port": 2020, "protocol": "tcp", "mode": 4,
		"endpoints": []any{map[string]any{"ip": "31.31.31.1", "port": 8080}},
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// W1: api_key_auth accepts every value of the swagger enum; the JWT modes
// require jwt_auth_profile, which is forwarded under its contract name.
func TestLBCreateFollowsCredentialPolicyEnum(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleOperator)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: lbCreateArgs(map[string]any{
		"api_key_auth": "jwt", "jwt_auth_profile": "corp-oidc", "bgp": true,
	})})
	if err != nil || res.IsError {
		t.Fatalf("jwt policy refused: %v %+v", err, res)
	}
	body := mock.lastBody()
	svc, _ := body["serviceArguments"].(map[string]any)
	if svc["api_key_auth"] != "jwt" || svc["jwt_auth_profile"] != "corp-oidc" {
		t.Fatalf("jwt policy not forwarded: %#v", svc)
	}
	if _, upper := svc["BGP"]; upper || svc["bgp"] != true {
		t.Fatalf("bgp must be sent under the swagger key, got %#v", svc)
	}

	for name, args := range map[string]map[string]any{
		"jwt without profile":       {"api_key_auth": "jwt"},
		"apikey-or-jwt w/o profile": {"api_key_auth": "apikey-or-jwt"},
		"value outside the enum":    {"api_key_auth": "optional"},
		"sel outside the enum":      {"sel": 11},
		"mode outside the enum":     {"mode": 6},
	} {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: lbCreateArgs(args)})
		if err == nil && (res == nil || !res.IsError) {
			t.Errorf("%s: accepted, want refusal", name)
		}
	}
	if mock.sawMutation("POST /netlox/v1/config/loadbalancer") && len(mock.bodies) != 1 {
		t.Errorf("a refused call reached the gateway: %d bodies", len(mock.bodies))
	}
}

// W3/W4: the lb_create schema carries the generated swagger descriptions and
// the api_key_auth enum, so the model reads the contract's own words.
func TestLBCreateSchemaCarriesGeneratedContract(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))
	cs := session(t, b, guard.RoleOperator)
	lst, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	for _, tl := range lst.Tools {
		if tl.Name == "lb_create" {
			raw, _ := json.Marshal(tl.InputSchema)
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
		}
	}
	if schema == nil {
		t.Fatal("lb_create not listed")
	}
	props, _ := schema["properties"].(map[string]any)
	prop := func(name string) map[string]any {
		p, _ := props[name].(map[string]any)
		if p == nil {
			t.Fatalf("schema has no property %q", name)
		}
		return p
	}
	if d, _ := prop("sel")["description"].(string); !strings.Contains(d, "CHWBL") || !strings.Contains(d, "GPU-aware") {
		t.Errorf("sel description is not the generated one: %q", d)
	}
	if d, _ := prop("mode")["description"].(string); !strings.Contains(d, "5") || !strings.Contains(d, "host one-arm") {
		t.Errorf("mode description is not the generated one: %q", d)
	}
	enum, _ := prop("api_key_auth")["enum"].([]any)
	found := map[any]bool{}
	for _, v := range enum {
		found[v] = true
	}
	for _, want := range []string{"disabled", "required", "jwt", "apikey-or-jwt"} {
		if !found[want] {
			t.Errorf("api_key_auth enum lacks %q: %v", want, enum)
		}
	}
	if _, ok := props["jwt_auth_profile"]; !ok {
		t.Error("jwt_auth_profile is not a typed argument")
	}
}

// The lb_list projection carries the rule identity, the pause state, the
// credential and EPP references and the three data-plane read-back values.
func TestLBListProjectsReadBackContract(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lb_list", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("lb_list failed: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	text := string(raw)
	for _, want := range []string{
		`"id":"rule-7"`, `"admin_state_up":false`, `"security":1`, `"jwt_auth_profile":"corp-oidc"`,
		`"epp_endpoint":"10.0.0.9:9002"`, `"kv_exact_api_mode":"chat"`, `"kv_model_profile":"qwen3-8b"`,
		`"fc_effective":{"max_outstanding":64,"mode":"enforce"}`, `"backend_tls_effective":{"enabled":false}`,
		`"half_close_effective":"relay"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("lb_list omitted %s: %s", want, text)
		}
	}
}

// W5 + ready folding: version_get and health_overview report product,
// features and the /status/ready verdict, a 503 not-ready included.
func TestVersionAndHealthCarryProductFeaturesReady(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	ctx := context.Background()

	out := callOut(t, callTool(t, cs, "version_get", nil))
	if out["product"] != "loxilb-inference-gateway" {
		t.Errorf("version_get product = %v", out["product"])
	}
	if feats, _ := out["features"].([]any); len(feats) != 3 || feats[2] != "jwt_auth_profile" {
		t.Errorf("version_get features = %v", out["features"])
	}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "health_overview"})
	if err != nil || res.IsError {
		t.Fatalf("health_overview failed: %v %+v", err, res)
	}
	h := callOut(t, res)
	if h["reachable"] != true || h["product"] != "loxilb-inference-gateway" {
		t.Errorf("health_overview reachable/product = %v / %v", h["reachable"], h["product"])
	}
	if h["ready"] != false {
		t.Errorf("health_overview ready = %v, want false from the 503 body", h["ready"])
	}
	if reasons, _ := h["ready_reasons"].([]any); len(reasons) != 1 || reasons[0] != "boot replay pending" {
		t.Errorf("health_overview ready_reasons = %v", h["ready_reasons"])
	}
	if errs, _ := h["errors"].(map[string]any); errs["ready"] != nil {
		t.Errorf("a 503 verdict must not be an error: %v", errs)
	}

	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "fleet_overview"})
	if err != nil || res.IsError {
		t.Fatalf("fleet_overview failed: %v %+v", err, res)
	}
	f := callOut(t, res)
	targets, _ := f["targets"].([]any)
	first, _ := targets[0].(map[string]any)
	if first["ready"] != false || first["product"] != "loxilb-inference-gateway" {
		t.Errorf("fleet_overview target lacks ready/product: %v", first)
	}
}

// GET /diagnostics is folded into every diagnose tool as a projection; keys
// outside the projection (which may carry propagated text) are dropped.
func TestDiagnoseToolsCarryDiagnosticsEvidence(t *testing.T) {
	mock := newAIMock(t)
	b := aiTestBridge(t, mock)
	cs := session(t, b, guard.RoleViewer)
	for _, tool := range []string{"diagnose_l4_errors", "diagnose_ai_latency", "capacity_report"} {
		out := callOut(t, callTool(t, cs, tool, nil))
		ev, _ := out["evidence"].(map[string]any)
		diag, _ := ev["diagnostics"].(map[string]any)
		if diag == nil {
			t.Fatalf("%s: no diagnostics evidence: %v", tool, out)
		}
		if diag["ready"] != false || diag["maintenance_state"] != "active" || diag["product"] != "loxilb-inference-gateway" {
			t.Errorf("%s: diagnostics projection = %v", tool, diag)
		}
		if _, leaked := diag["boot"]; leaked {
			t.Errorf("%s: boot is outside the projection", tool)
		}
	}
	out := callOut(t, callTool(t, cs, "diagnose_endpoint", map[string]any{"host": "10.0.0.1"}))
	ev, _ := out["evidence"].(map[string]any)
	if _, ok := ev["diagnostics"].(map[string]any); !ok {
		t.Errorf("diagnose_endpoint: no diagnostics evidence: %v", out)
	}
}

// W7: ai_kv_inventory_get surfaces the admission verdict.
func TestKvInventoryCarriesAdmission(t *testing.T) {
	mock := newAIMock(t)
	b := aiTestBridge(t, mock)
	cs := session(t, b, guard.RoleViewer)
	out := callOut(t, callTool(t, cs, "ai_kv_inventory_get", map[string]any{"service_id": 1, "ep_idx": 0}))
	if out["admission"] != "admitted" || out["hash_algo"] != "sha256_cbor" || out["total"] != float64(1) {
		t.Errorf("kv inventory = %v", out)
	}
}

// W2: the burst_size argument states the bucket semantics of the contract.
func TestApikeyCreateBurstSizeSemantics(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))
	cs := session(t, b, guard.RoleOperator)
	lst, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range lst.Tools {
		if tl.Name != "ai_apikey_create" {
			continue
		}
		raw, _ := json.Marshal(tl.InputSchema)
		text := string(raw)
		if !strings.Contains(text, "total request-bucket capacity") || strings.Contains(text, "above the steady-state") {
			t.Errorf("burst_size description still describes additional capacity: %s", text)
		}
		return
	}
	t.Fatal("ai_apikey_create not listed")
}
