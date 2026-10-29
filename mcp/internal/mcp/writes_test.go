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
	if st, _ := readBack["status"].(map[string]any); st["operatingStatus"] != "ONLINE" {
		t.Errorf("read_back must carry GET .../status: %v", readBack)
	}
	if !mock.sawMutation("GET " + mockLBKey + "/status") {
		t.Error("the read-back did not GET the status")
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
	args := withArgs(lbUpdateKey, map[string]any{"fc_mode": "enforce", "fc_max_outstanding": 32})
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	rb, _ := out["rollback"].(map[string]any)
	if note, _ := rb["note"].(string); !strings.Contains(note, "fc_mode") || !strings.Contains(note, "fc_max_outstanding") {
		t.Errorf("fields the rule did not declare must be named in the rollback note: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	mock.setFailing("PATCH " + mockLBKey)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: withToken(args, token)})
	if err != nil {
		t.Fatal(err)
	}
	txt := resultText(res)
	if !res.IsError || !strings.Contains(txt, "injected failure") || !strings.Contains(txt, "lb_create on the same key") {
		t.Fatalf("the gateway's answer must be passed on with the replace hint: %s", txt)
	}
	svc, _ := mock.lastBody()["serviceArguments"].(map[string]any)
	if svc["fc_mode"] != "enforce" || svc["fc_max_outstanding"] != float64(32) {
		t.Errorf("PATCH body must carry the flow-control fields: %v", svc)
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
