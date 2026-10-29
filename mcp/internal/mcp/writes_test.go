/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// PR-B2: the AI control-plane writes. Every mutation previews before it
// applies (current, proposed, diff, rollback, impact), applies only with
// the preview's token (or, at operator tier on an object/tenant change,
// with apply: true), and re-reads the object afterwards.

const mockLBKey = "/netlox/v1/config/loadbalancer/externalipaddress/1.2.3.4/port/80/protocol/tcp"

var lbUpdateKey = map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp"}

func withArgs(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestLBUpdateIsAdminConfirmOnly(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))
	operator, _ := listNames(t, b, guard.RoleOperator)
	admin, _ := listNames(t, b, guard.RoleAdmin)
	if operator["lb_update"] || !admin["lb_update"] {
		t.Fatalf("lb_update: operator %v admin %v, want admin only", operator["lb_update"], admin["lb_update"])
	}
}

func TestLBUpdateAdminStateDownPreviewsThenPatches(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	// The mock rule reads back adminStateUp false; the change puts it up.
	args := withArgs(lbUpdateKey, map[string]any{"admin_state_up": true})

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	if out["action"] != "preview" || out["impact"] != "service" {
		t.Fatalf("want a service-impact preview, got %v", out)
	}
	if mock.sawMutation("PATCH ") {
		t.Fatal("preview sent a PATCH")
	}
	if !mock.sawMutation("GET " + mockLBKey) {
		t.Fatal("preview skipped the pre-read")
	}
	cur, _ := out["current"].(map[string]any)
	if cur["adminStateUp"] != false {
		t.Errorf("current must show the rule as read: %v", cur)
	}
	eps, _ := cur["endpoints"].([]any)
	if len(eps) != 1 {
		t.Errorf("current must carry the endpoint list: %v", cur["endpoints"])
	}
	diff, _ := out["diff"].(map[string]any)
	if d, _ := diff["adminStateUp"].(map[string]any); d == nil || d["from"] != false || d["to"] != true {
		t.Errorf("diff must show adminStateUp false -> true: %v", diff)
	}
	if _, touched := diff["endpoints"]; touched {
		t.Errorf("an unchanged endpoint list must not appear in the diff: %v", diff)
	}
	rb, _ := out["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	if rb["tool"] != "lb_update" || rbArgs["admin_state_up"] != false || rbArgs["external_ip"] != "1.2.3.4" {
		t.Errorf("rollback must be lb_update with the current state: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	out = callOut(t, res)
	if out["action"] != "executed" || !mock.sawMutation("PATCH "+mockLBKey) {
		t.Fatal("apply did not PATCH the key")
	}
	body := mock.lastBody()
	svc, _ := body["serviceArguments"].(map[string]any)
	if svc["adminStateUp"] != true || svc["externalIP"] != "1.2.3.4" || svc["port"] != float64(80) || svc["protocol"] != "tcp" {
		t.Errorf("PATCH body must overlay adminStateUp on the identifying tuple: %v", body)
	}
	if _, has := body["endpoints"]; has {
		t.Errorf("a state change must not send the endpoint list: %v", body)
	}
	for _, k := range []string{"fc_mode", "name", "sel", "mode", "host"} {
		if _, has := svc[k]; has {
			t.Errorf("PATCH body carries %s, which was not asked for: %v", k, svc)
		}
	}
	readBack, _ := out["read_back"].(map[string]any)
	if _, ok := readBack["rule"]; !ok {
		t.Errorf("read_back must carry the rule: %v", readBack)
	}
	if readBack["admin_state_up"] != false {
		t.Errorf("read_back must carry the effective admin state (the mock rule reads back false): %v", readBack)
	}
	if st, _ := readBack["status"].(map[string]any); st["operatingStatus"] != "ONLINE" {
		t.Errorf("read_back must carry GET .../status: %v", readBack)
	}
	if !mock.sawMutation("GET " + mockLBKey + "/status") {
		t.Error("the read-back did not GET the status")
	}
}

// The gateway marshals adminStateUp with omitempty: a paused rule's GET
// has no adminStateUp key. The preview reports the flag as read (absent)
// and its rollback states no value; a read-back after sending true that
// still sees no flag reports absent, not false.
func TestLBUpdateReportsAnAbsentAdminStateAsRead(t *testing.T) {
	mock := newMockLoxilb(t)
	mock.ruleJSON = `{"serviceArguments":{"externalIP":"1.2.3.4","port":80,"protocol":"tcp","name":"web"},"endpoints":[{"endpointIP":"10.0.0.1","targetPort":8000,"weight":1}]}`
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	args := withArgs(lbUpdateKey, map[string]any{"admin_state_up": true})
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	cur, _ := out["current"].(map[string]any)
	if cur["adminStateUp"] != "absent" {
		t.Errorf("an absent adminStateUp is reported as absent: %v", cur)
	}
	rb, _ := out["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	note, _ := rb["note"].(string)
	if _, has := rbArgs["admin_state_up"]; has || !strings.Contains(note, "absent") {
		t.Errorf("the rollback must state no admin_state_up and say why: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	rbk, _ := callOut(t, res)["read_back"].(map[string]any)
	if rbk["admin_state_up"] != "absent" {
		t.Errorf("after sending true, an absent flag stays absent (only a sent false resolves it): %v", rbk)
	}
	if _, has := rbk["admin_state_up_note"]; has {
		t.Errorf("the absent-to-false note belongs to a sent false only: %v", rbk)
	}
}

func TestLBUpdateWeightSwapSendsTheWholeEndpointList(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	args := withArgs(lbUpdateKey, map[string]any{"endpoint_weights": []map[string]any{{"ip": "10.0.0.1", "port": 8000, "weight": 3}}})

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	diff, _ := out["diff"].(map[string]any)
	if diff["endpoints"] == nil {
		t.Errorf("diff must show the endpoint change: %v", diff)
	}
	rb, _ := out["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	rbW, _ := rbArgs["endpoint_weights"].([]any)
	if len(rbW) != 1 || mapValue(rbW[0])["weight"] != float64(1) {
		t.Errorf("rollback must restore the current weight 1: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	eps, _ := mock.lastBody()["endpoints"].([]any)
	if len(eps) != 1 {
		t.Fatalf("PATCH must send the whole endpoint list: %v", mock.lastBody())
	}
	ep := mapValue(eps[0])
	if ep["endpointIP"] != "10.0.0.1" || ep["targetPort"] != float64(8000) || ep["weight"] != float64(3) || ep["ep_role"] != float64(1) || ep["nixl_port"] != float64(9001) {
		t.Errorf("endpoint entry must keep role and side-channel port with the new weight: %v", ep)
	}
}

func TestLBUpdateRefusesBeforeAnyRequest(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown field is refused by the schema", withArgs(lbUpdateKey, map[string]any{"name": "web2"}), "name"},
		{"nothing to change", lbUpdateKey, "nothing to change"},
		{"weight for an endpoint the rule does not have", withArgs(lbUpdateKey, map[string]any{"endpoint_weights": []map[string]any{{"ip": "10.0.0.9", "port": 8000, "weight": 2}}}), "not an endpoint of the rule"},
		{"zero weight", withArgs(lbUpdateKey, map[string]any{"endpoint_weights": []map[string]any{{"ip": "10.0.0.1", "port": 8000, "weight": 0}}}), "at least 1"},
		{"fc_mode outside the contract", withArgs(lbUpdateKey, map[string]any{"fc_mode": "sometimes"}), "fc_mode"},
		{"missing rule names lb_create", map[string]any{"external_ip": "9.9.9.9", "port": 80, "protocol": "tcp", "admin_state_up": false}, "lb_create creates one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || !strings.Contains(resultText(res), tc.want) {
				t.Fatalf("want a refusal naming %q, got %v", tc.want, resultText(res))
			}
		})
	}
	if mock.sawMutation("PATCH ") {
		t.Fatal("a refusal sent a PATCH")
	}
}

func TestLBUpdatePassesTheGatewayRefusalWithAHint(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	args := withArgs(lbUpdateKey, map[string]any{"admin_state_up": true})
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	token, _ := callOut(t, res)["confirm_token"].(string)
	mock.setFailing("PATCH " + mockLBKey)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: withToken(args, token)})
	if err != nil {
		t.Fatal(err)
	}
	txt := resultText(res)
	if !res.IsError || !strings.Contains(txt, "injected failure") || !strings.Contains(txt, "lb_create on the same key") {
		t.Fatalf("the gateway's answer must be passed on with the replace hint: %s", txt)
	}
}

// mockL4RuleJSON is the mock's rule as a plain L4 (mode 0) rule: the PATCH
// reaches it, but no capacity gate runs on it.
const mockL4RuleJSON = `{"serviceArguments":{"externalIP":"1.2.3.4","port":80,"protocol":"tcp","name":"web","mode":0,"adminStateUp":true},"endpoints":[{"endpointIP":"10.0.0.1","targetPort":8000,"weight":1,"state":"active"}]}`

// D26: every rule with an admission gate is fullproxy, and the gateway does
// not PATCH one, so fc_* on a fullproxy rule is refused from the pre-read,
// before any PATCH, naming the replace.
func TestLBUpdateRefusesFcOnAFullproxyRule(t *testing.T) {
	mock := newMockLoxilb(t) // mockRuleJSON is mode 4
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	for _, extra := range []map[string]any{
		{"fc_mode": "observe"},
		{"fc_max_outstanding": 8},
		{"fc_max_queue_depth": 4, "fc_max_queue_wait_ms": 2000},
		{"admin_state_up": true, "fc_mode": "enforce"},
	} {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lb_update", Arguments: withArgs(lbUpdateKey, extra)})
		if err != nil {
			t.Fatal(err)
		}
		txt := resultText(res)
		if !res.IsError || !strings.Contains(txt, "fullproxy") || !strings.Contains(txt, "lb_create on the same key") ||
			!strings.Contains(txt, "nothing was sent") {
			t.Fatalf("%v: want the bridge's fullproxy refusal naming the replace, got %s", extra, txt)
		}
	}
	if mock.sawMutation("PATCH ") {
		t.Fatal("a refused fc_* change sent a PATCH")
	}
	if !mock.sawMutation("GET " + mockLBKey) {
		t.Fatal("the refusal must come from the pre-read")
	}
}

// On an L4 rule the PATCH carries fc_*, and the preview and the read-back
// say that no gate runs there.
func TestLBUpdateNotesFcOnAnL4Rule(t *testing.T) {
	mock := newMockLoxilb(t)
	mock.ruleJSON = mockL4RuleJSON
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	args := withArgs(lbUpdateKey, map[string]any{"fc_mode": "enforce", "fc_max_outstanding": 32})
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	rb, _ := out["rollback"].(map[string]any)
	note, _ := rb["note"].(string)
	if !strings.Contains(note, "did not declare fc_mode, fc_max_outstanding") {
		t.Errorf("fields the rule did not declare must be named in the rollback note: %v", rb)
	}
	if !strings.Contains(note, "capacity gate runs only on AI-gateway (fullproxy) rules") {
		t.Errorf("the preview must say no gate runs on a mode 0 rule: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	svc, _ := mock.lastBody()["serviceArguments"].(map[string]any)
	if svc["fc_mode"] != "enforce" || svc["fc_max_outstanding"] != float64(32) {
		t.Errorf("PATCH body must carry the flow-control fields: %v", svc)
	}
	readBack, _ := callOut(t, res)["read_back"].(map[string]any)
	if fn, _ := readBack["fc_note"].(string); !strings.Contains(fn, "capacity gate runs only") {
		t.Errorf("read_back must carry the fc note: %v", readBack)
	}
}

// ---- operator writes: preview by default, apply: true as the shortcut ----

func TestApikeyCreatePreviewsUnlessApplied(t *testing.T) {
	mock := newAIMock(t)
	b := aiTestBridge(t, mock)
	cs := session(t, b, guard.RoleOperator)
	ctx := context.Background()
	args := map[string]any{"tenant_id": "t1", "name": "ci", "priority_class": "sheddable", "client_op_id": "op-1",
		"enabled": false, "default_objective": "latency", "allowed_objectives": []string{"latency", "tier:fast"}}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_apikey_create", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	if out["action"] != "preview" || out["impact"] != "tenant" {
		t.Fatalf("want a tenant-impact preview, got %v", out)
	}
	if mock.sawMutation("POST ") {
		t.Fatal("preview sent the POST")
	}
	prop, _ := out["proposed"].(map[string]any)
	if prop["priority_class"] != "sheddable" || prop["client_op_id"] != "op-1" || prop["enabled"] != false || prop["default_objective"] != "latency" {
		t.Errorf("proposed must carry the typed fields: %v", prop)
	}
	rb, _ := out["rollback"].(map[string]any)
	if note, _ := rb["note"].(string); !strings.Contains(note, "ai_apikey_delete") {
		t.Errorf("rollback note must name ai_apikey_delete: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_apikey_create", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	out = callOut(t, res)
	result, _ := out["result"].(map[string]any)
	if out["action"] != "executed" || result["key_id"] != "key-123" || result["key_file"] == "" {
		t.Fatalf("executed result must carry key_id and key_file: %v", out)
	}
	if _, leaked := result["raw_key"]; leaked {
		t.Error("raw_key in the default create result")
	}
	body := mock.lastBody()
	objs, _ := body["allowed_objectives"].([]any)
	if body["priority_class"] != "sheddable" || body["client_op_id"] != "op-1" || body["enabled"] != false || len(objs) != 2 {
		t.Errorf("POST body: %v", body)
	}
	if _, has := body["api_key"]; has {
		t.Error("the bridge never imports a key")
	}
	if rbk, _ := out["read_back"].(map[string]any); rbk["key_id"] != "key-123" {
		t.Errorf("read_back must re-read the key: %v", out["read_back"])
	}
	// The generated enum refuses an unknown class before any request.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_apikey_create", Arguments: map[string]any{"tenant_id": "t1", "priority_class": "urgent", "apply": true}})
	if err != nil || !res.IsError || !strings.Contains(resultText(res), "priority_class") {
		t.Fatalf("unknown priority_class must be refused: %v %v", err, resultText(res))
	}
	if n := mock.countRequests("POST /netlox/v1/config/ai/apikey"); n != 1 {
		t.Errorf("want exactly one POST, got %d", n)
	}
}

func TestApikeyUpdateFullSurfaceWithPreviewAndApply(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleOperator)
	ctx := context.Background()
	args := map[string]any{"key_id": "key-1", "enabled": false, "tokens_per_min": 500, "priority_class": "",
		"allowed_objectives": []string{}, "allowed_models": []string{"llama3", "qwen3"}}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_apikey_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	if out["action"] != "preview" || out["impact"] != "object" || mock.sawMutation("PATCH ") {
		t.Fatalf("want an object-impact preview without a PATCH, got %v", out)
	}
	cur, _ := out["current"].(map[string]any)
	if cur["enabled"] != true || cur["rate_limit_rps"] != float64(5) {
		t.Errorf("current must be the key as read: %v", cur)
	}
	diff, _ := out["diff"].(map[string]any)
	if d, _ := diff["enabled"].(map[string]any); d == nil || d["to"] != false {
		t.Errorf("diff must show enabled -> false: %v", diff)
	}
	rb, _ := out["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	if rb["tool"] != "ai_apikey_update" || rbArgs["key_id"] != "key-1" || rbArgs["enabled"] != true || rbArgs["priority_class"] != "standard" || rbArgs["tokens_per_min"] != float64(0) {
		t.Errorf("rollback must restore the current values (absent quota = 0): %v", rb)
	}
	if objs, _ := rbArgs["allowed_objectives"].([]any); objs == nil || len(objs) != 0 {
		t.Errorf("an absent list rolls back to an empty list: %v", rbArgs["allowed_objectives"])
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_apikey_update", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	if !mock.sawMutation("PATCH /netlox/v1/config/ai/apikey/key-1") {
		t.Fatal("apply did not PATCH")
	}
	body := mock.lastBody()
	objs, has := body["allowed_objectives"].([]any)
	if body["enabled"] != false || body["tokens_per_min"] != float64(500) || body["priority_class"] != "" || !has || len(objs) != 0 {
		t.Errorf("PATCH body must carry the named fields, empty values included: %v", body)
	}
	for _, k := range []string{"rate_limit_rps", "burst_size", "default_objective"} {
		if _, sent := body[k]; sent {
			t.Errorf("PATCH body carries %s, which was not named: %v", k, body)
		}
	}
	if rbk, _ := callOut(t, res)["read_back"].(map[string]any); rbk["key_id"] != "key-1" {
		t.Errorf("read_back must re-read the key: %v", rbk)
	}
	// apply: true at operator tier skips the preview; an empty change is refused.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_apikey_update", Arguments: map[string]any{"key_id": "key-1", "rate_limit_rps": 7, "apply": true}})
	if err != nil || res.IsError || callOut(t, res)["action"] != "executed" {
		t.Fatalf("apply=true: %v %v", err, resultText(res))
	}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_apikey_update", Arguments: map[string]any{"key_id": "key-1", "apply": true}})
	if err != nil || !res.IsError || !strings.Contains(resultText(res), "no fields to update") {
		t.Fatalf("an empty patch must be refused: %v %v", err, resultText(res))
	}
	if n := mock.countRequests("PATCH "); n != 2 {
		t.Errorf("want 2 PATCHes, got %d", n)
	}
}

func TestRatelimitSetPreviewsTheRowAndRollsBackModelQuotas(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleOperator)
	ctx := context.Background()
	args := map[string]any{"tenant_id": "lab", "rps": 10, "tokens_per_min": 2000, "default_objective": "fast",
		"model_limits": []map[string]any{{"model": "qwen3", "tokens_per_min": 300}}}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_ratelimit_set", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	if out["action"] != "preview" || out["impact"] != "tenant" || mock.sawMutation("POST ") {
		t.Fatalf("want a tenant-impact preview without a POST, got %v", out)
	}
	cur, _ := out["current"].(map[string]any)
	if cur["rps"] != float64(20) {
		t.Errorf("current must be the row as read: %v", cur)
	}
	rb, _ := out["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	if rb["tool"] != "ai_ratelimit_set" || rbArgs["rps"] != float64(20) || rbArgs["tokens_per_min"] != float64(5000) || rbArgs["default_objective"] != "" {
		t.Errorf("rollback must restore the row (absent objective = cleared): %v", rb)
	}
	mls, _ := rbArgs["model_limits"].([]any)
	if len(mls) != 2 || mapValue(mls[0])["model"] != "llama3" || mapValue(mls[0])["tokens_per_min"] != float64(1000) || mapValue(mls[1])["model"] != "qwen3" || mapValue(mls[1])["tokens_per_min"] != float64(0) {
		t.Errorf("rollback model_limits must keep llama3 and zero the new qwen3 quota: %v", mls)
	}
	if _, named := rbArgs["priority_class"]; named {
		t.Errorf("a field the call does not change is not in the rollback: %v", rbArgs)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_ratelimit_set", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	body := mock.lastBody()
	bm, _ := body["model_limits"].([]any)
	if body["tenant_id"] != "lab" || body["rps"] != float64(10) || body["burst_pct"] != float64(0) || body["default_objective"] != "fast" || len(bm) != 1 {
		t.Errorf("POST body: %v", body)
	}
	if _, sent := body["priority_class"]; sent {
		t.Errorf("priority_class was not named and must not be sent: %v", body)
	}
	if rbk, _ := callOut(t, res)["read_back"].(map[string]any); rbk["tenant_id"] != "lab" {
		t.Errorf("read_back must re-read the row: %v", rbk)
	}

	// No row yet: the rollback is a zero-quota set with a note.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_ratelimit_set", Arguments: map[string]any{"tenant_id": "nope", "rps": 1, "tokens_per_min": 0}})
	if err != nil || res.IsError {
		t.Fatalf("preview on a missing row: %v %v", err, resultText(res))
	}
	out = callOut(t, res)
	if _, has := out["current"]; has {
		t.Errorf("no current state when no row exists: %v", out["current"])
	}
	rb, _ = out["rollback"].(map[string]any)
	rbArgs, _ = rb["args"].(map[string]any)
	if rb["tool"] != "ai_ratelimit_set" || rbArgs["rps"] != float64(0) || !strings.Contains(rb["note"].(string), "no delete") {
		t.Errorf("rollback on a missing row: %v", rb)
	}
}

func TestUserRatelimitSetAndDelete(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleOperator)
	ctx := context.Background()

	// All-zero is refused before any request.
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_user_ratelimit_set", Arguments: map[string]any{"tenant_id": "lab", "user_id": "bob", "burst_size": 4, "apply": true}})
	if err != nil || !res.IsError || !strings.Contains(resultText(res), "all zero") {
		t.Fatalf("all-zero entry must be refused: %v %v", err, resultText(res))
	}
	// A new user: rollback is the delete.
	args := map[string]any{"tenant_id": "lab", "user_id": "bob", "rps": 2, "model_limits": []map[string]any{{"model": "llama3", "tokens_per_min": 100}}}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_user_ratelimit_set", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	rb, _ := out["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	if out["action"] != "preview" || out["impact"] != "tenant" || rb["tool"] != "ai_user_ratelimit_delete" || rbArgs["user_id"] != "bob" {
		t.Fatalf("preview of a new row must roll back through the delete: %v", out)
	}
	if mock.sawMutation("POST ") {
		t.Fatal("preview sent the POST")
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_user_ratelimit_set", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	body := mock.lastBody()
	bm, _ := body["model_limits"].([]any)
	if body["tenant_id"] != "lab" || body["user_id"] != "bob" || body["rps"] != float64(2) || body["burst_size"] != float64(0) || body["tokens_per_min"] != float64(0) || len(bm) != 1 {
		t.Errorf("POST body must be the whole entry: %v", body)
	}
	// An existing user: rollback is a set with the row and its model limits.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_user_ratelimit_set", Arguments: map[string]any{"tenant_id": "lab", "user_id": "alice", "rps": 9}})
	if err != nil || res.IsError {
		t.Fatalf("preview on an existing row: %v %v", err, resultText(res))
	}
	out = callOut(t, res)
	rb, _ = out["rollback"].(map[string]any)
	rbArgs, _ = rb["args"].(map[string]any)
	mls, _ := rbArgs["model_limits"].([]any)
	if rb["tool"] != "ai_user_ratelimit_set" || rbArgs["rps"] != float64(5) || rbArgs["burst_size"] != float64(10) || len(mls) != 1 {
		t.Errorf("rollback must carry the row with its model limits: %v", rb)
	}
	prop, _ := out["proposed"].(map[string]any)
	if pm, _ := prop["model_limits"].([]any); len(pm) != 0 {
		t.Errorf("a set without model_limits proposes an empty set (they are replaced): %v", prop)
	}

	// Delete: operator lists it, the preview carries the row as the rollback, no apply shortcut.
	operator, _ := listNames(t, b, guard.RoleOperator)
	if !operator["ai_user_ratelimit_delete"] {
		t.Fatal("operator must list ai_user_ratelimit_delete (re-creatable)")
	}
	del := map[string]any{"tenant_id": "lab", "user_id": "alice"}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_user_ratelimit_delete", Arguments: withArgs(del, map[string]any{"apply": true})})
	if err != nil || !res.IsError {
		t.Fatalf("the delete has no apply shortcut; the schema must refuse it: %v %v", err, resultText(res))
	}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_user_ratelimit_delete", Arguments: del})
	if err != nil || res.IsError {
		t.Fatalf("delete preview: %v %v", err, resultText(res))
	}
	out = callOut(t, res)
	rb, _ = out["rollback"].(map[string]any)
	rbArgs, _ = rb["args"].(map[string]any)
	mls, _ = rbArgs["model_limits"].([]any)
	if out["action"] != "preview" || rb["tool"] != "ai_user_ratelimit_set" || rbArgs["tokens_per_min"] != float64(1000) || len(mls) != 1 {
		t.Fatalf("delete preview must carry the row as the set rollback: %v", out)
	}
	if mock.sawMutation("DELETE ") {
		t.Fatal("preview sent the DELETE")
	}
	token, _ = out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_user_ratelimit_delete", Arguments: withToken(del, token)})
	if err != nil || res.IsError {
		t.Fatalf("delete apply: %v %v", err, resultText(res))
	}
	if !mock.sawMutation("DELETE /netlox/v1/config/ai/user/ratelimit/lab/alice") {
		t.Fatal("apply did not DELETE")
	}
	if rbk, _ := callOut(t, res)["read_back"].(map[string]any); rbk["exists"] != true {
		t.Errorf("read_back re-reads the row (the mock still serves it): %v", rbk)
	}
	// A missing row is refused before any request.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "ai_user_ratelimit_delete", Arguments: map[string]any{"tenant_id": "lab", "user_id": "bob"}})
	if err != nil || !res.IsError || !strings.Contains(resultText(res), "nothing to delete") {
		t.Fatalf("missing row: %v %v", err, resultText(res))
	}
	if n := mock.countRequests("DELETE "); n != 1 {
		t.Errorf("want one DELETE, got %d", n)
	}
}

// ---- secrate_disable (D18) and config_persist (D22): admin-confirm, never autopilot ----

func TestSecrateDisablePreviewsTheRollbackThenDeletes(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "secrate_disable", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	if out["action"] != "preview" || out["impact"] != "global" || mock.sawMutation("DELETE ") {
		t.Fatalf("want a global-impact preview without a DELETE, got %v", out)
	}
	cur, _ := out["current"].(map[string]any)
	prop, _ := out["proposed"].(map[string]any)
	if cur["synEnabled"] != true || cur["connRateEnabled"] != false || prop["synEnabled"] != false {
		t.Errorf("current must be the zero-defaulted configuration, proposed all off: %v -> %v", cur, prop)
	}
	if wl, _ := prop["whitelistIps"].([]any); wl == nil {
		t.Errorf("proposed must carry an empty whitelist: %v", prop["whitelistIps"])
	}
	rb, _ := out["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	if rb["tool"] != "secrate_set" || rbArgs["syn_enabled"] != true || rbArgs["syn_threshold"] != float64(1000) || rbArgs["rate_per_sec"] != float64(500) {
		t.Errorf("rollback must be secrate_set with the current configuration: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "secrate_disable", Arguments: withToken(map[string]any{}, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	if !mock.sawMutation("DELETE /netlox/v1/config/securityrate") {
		t.Fatal("apply did not DELETE")
	}
	if rbk, _ := callOut(t, res)["read_back"].(map[string]any); rbk == nil || rbk["synThreshold"] == nil {
		t.Errorf("read_back must re-read the configuration keys: %v", rbk)
	}

	// Unprotected target: nothing to put back, the rollback is a note.
	mock2 := newMockLoxilb(t)
	mock2.securityRateJSON = `{"whitelistIps":null}`
	b2 := newTestBridge(t, testConfig(mock2.srv.URL))
	cs2 := session(t, b2, guard.RoleAdmin)
	res, err = cs2.CallTool(ctx, &sdk.CallToolParams{Name: "secrate_disable", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("preview on an unprotected target: %v %v", err, resultText(res))
	}
	rb, _ = callOut(t, res)["rollback"].(map[string]any)
	if tool, _ := rb["tool"].(string); tool != "" || !strings.Contains(rb["note"].(string), "no protection") {
		t.Errorf("rollback on an unprotected target must be a note: %v", rb)
	}
}

func TestConfigPersistPreviewsTheSnapshotIdentity(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "config_persist", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	if out["action"] != "preview" || out["impact"] != "instance" || mock.sawMutation("POST ") {
		t.Fatalf("want an instance-impact preview without a POST, got %v", out)
	}
	cur, _ := out["current"].(map[string]any)
	if cur["schema"] != "1.12" || cur["checksum"] != "sha256:snap" {
		t.Errorf("current must be the running snapshot's identity: %v", cur)
	}
	rb, _ := out["rollback"].(map[string]any)
	if tool, _ := rb["tool"].(string); tool != "" || !strings.Contains(rb["note"].(string), "not reversible") {
		t.Errorf("rollback must say the write is not reversible: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "config_persist", Arguments: withToken(map[string]any{}, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	if !mock.sawMutation("POST /netlox/v1/config/persist") {
		t.Fatal("apply did not POST /config/persist")
	}
	if rbk, _ := callOut(t, res)["read_back"].(map[string]any); rbk["checksum"] != "sha256:snap" {
		t.Errorf("read_back must re-read the identity: %v", rbk)
	}

	// The snapshot gate: the 409 is the retryable verdict, nothing is written.
	mock.snapshotBusy = true
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "config_persist", Arguments: map[string]any{}})
	if err != nil || !res.IsError {
		t.Fatalf("busy gate must be a tool error: %v %v", err, resultText(res))
	}
	if txt := resultText(res); !strings.Contains(txt, "snapshot_gate_busy") || !strings.Contains(txt, "retry_after_sec: 3") {
		t.Errorf("verdict text: %s", txt)
	}
	if n := mock.countRequests("POST /netlox/v1/config/persist"); n != 1 {
		t.Errorf("the busy preview must write nothing (POSTs: %d)", n)
	}
}

func TestSecrateDisableAndConfigPersistAreNeverAutopilot(t *testing.T) {
	for _, name := range []string{"secrate_disable", "config_persist"} {
		if _, never := guard.NeverAutopilot[name]; !never {
			t.Errorf("%s must be in NeverAutopilot", name)
		}
		pol := &guard.Policy{Autopilot: []string{name}}
		if pol.AutopilotAllowed(name) {
			t.Errorf("%s: configuration must not make it autopilot", name)
		}
	}
}

// ---- lb_create typed additions and the pii is_exclude wire key ----

func TestLBCreateTypedAdditionsReachTheWire(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleOperator)
	ctx := context.Background()
	args := map[string]any{"external_ip": "9.9.9.9", "port": 80, "protocol": "tcp", "mode": 4, "host": "m.example",
		"endpoints": []map[string]any{{"ip": "10.0.0.5", "port": 8000}},
		"fc_mode":   "enforce", "fc_max_outstanding": 64, "fc_max_queue_depth": 8, "fc_max_queue_wait_ms": 250,
		"fc_ep_max_inflight": 4, "fc_prefill_max_inflight": 2, "fc_decode_max_inflight": 2, "fc_tenant_max_share_pct": 50,
		"epp_endpoint": "10.0.0.9:9002", "epp_failure_mode": "FailOpen", "epp_timeout_ms": 300, "epp_plaintext": true,
		"ai_default_reservation": "on", "ai_default_max_tokens": 512, "ai_objective_strict": false,
		"security": 1, "admin_state_up": false, "id": "rule-x"}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("create: %v %v", err, resultText(res))
	}
	svc, _ := mock.lastBody()["serviceArguments"].(map[string]any)
	want := map[string]any{"fc_mode": "enforce", "fc_max_outstanding": float64(64), "fc_max_queue_depth": float64(8), "fc_max_queue_wait_ms": float64(250),
		"fc_ep_max_inflight": float64(4), "fc_prefill_max_inflight": float64(2), "fc_decode_max_inflight": float64(2), "fc_tenant_max_share_pct": float64(50),
		"eppEndpoint": "10.0.0.9:9002", "eppFailureMode": "FailOpen", "eppTimeoutMs": float64(300), "eppPlaintext": true,
		"ai_default_reservation": "on", "ai_default_max_tokens": float64(512), "ai_objective_strict": false,
		"security": float64(1), "adminStateUp": false, "id": "rule-x"}
	for k, v := range want {
		if svc[k] != v {
			t.Errorf("serviceArguments.%s = %v, want %v", k, svc[k], v)
		}
	}
	for _, tc := range []struct{ arg, val, want string }{
		{"fc_mode", "sometimes", "fc_mode"}, {"epp_failure_mode", "Explode", "epp_failure_mode"}, {"ai_default_reservation", "maybe", "ai_default_reservation"},
	} {
		bad := map[string]any{"external_ip": "9.9.9.9", "port": 80, "protocol": "tcp", "endpoints": []map[string]any{{"ip": "10.0.0.5", "port": 8000}}, tc.arg: tc.val}
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: bad})
		if err != nil || !res.IsError || !strings.Contains(resultText(res), tc.want) {
			t.Errorf("%s=%s must be refused by the contract's enum: %v %v", tc.arg, tc.val, err, resultText(res))
		}
	}
	bad := map[string]any{"external_ip": "9.9.9.9", "port": 80, "protocol": "tcp", "endpoints": []map[string]any{{"ip": "10.0.0.5", "port": 8000}}, "security": 7}
	if res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: bad}); err != nil || !res.IsError || !strings.Contains(resultText(res), "security") {
		t.Errorf("security=7 must be refused: %v %v", err, resultText(res))
	}
	if n := mock.countRequests("POST /netlox/v1/config/loadbalancer"); n != 1 {
		t.Errorf("want one POST, got %d", n)
	}
}

func TestLBRollbackCarriesTheTypedAdditions(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	// The mock rule declares id, adminStateUp false, security 1, eppEndpoint.
	replace := map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp", "mode": 4,
		"host": "models.example", "path_prefix": "/v1", "model_name": "llama3",
		"endpoints": []map[string]any{{"ip": "10.0.0.5", "port": 8000}}}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lb_create", Arguments: replace})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	rb, _ := callOut(t, res)["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	if rbArgs["id"] != "rule-7" || rbArgs["security"] != float64(1) || rbArgs["epp_endpoint"] != "10.0.0.9:9002" || rbArgs["admin_state_up"] != false {
		t.Errorf("rollback must carry the typed additions, adminStateUp false included: %v", rbArgs)
	}
}

func TestPIIURLPatternsSendIsExclude(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	args := map[string]any{"mode": "add", "patterns": []map[string]any{{"pattern": "/health*", "is_exclude": true}}}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "pii_url_patterns_set", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	token, _ := callOut(t, res)["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "pii_url_patterns_set", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	pats, _ := mock.lastBody()["patterns"].([]any)
	if len(pats) != 1 || mapValue(pats[0])["is_exclude"] != true {
		t.Errorf("the wire key is is_exclude (swagger PIIURLPattern): %v", mock.lastBody())
	}
	if _, old := mapValue(pats[0])["exclude"]; old {
		t.Errorf("the misspelt key must be gone: %v", pats[0])
	}
	// The old argument name is refused by the schema.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "pii_url_patterns_set", Arguments: map[string]any{"mode": "add", "patterns": []map[string]any{{"pattern": "/x", "exclude": true}}}})
	if err != nil || !res.IsError {
		t.Fatalf("the old argument name must be refused: %v %v", err, resultText(res))
	}
}

// mapValue returns v as an object, or an empty one (test-side copy of the
// tools helper).
func mapValue(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
