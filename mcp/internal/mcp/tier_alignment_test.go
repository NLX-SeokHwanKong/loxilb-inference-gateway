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

// The tier alignment (D13): the pre-campaign global/instance-scope
// mutations are admin-confirm behind the change gate, and lb_create splits
// create (operator) from replace (admin-confirm) by a pre-read of the key.

// adminConfirmTools are the 14 raised tools; lb_create is covered separately.
var adminConfirmTools = []string{
	"secrate_set", "secrate_reset", "bgp_neigh_set", "bgp_global_set", "bgp_policy_apply",
	"config_params_set", "gpu_mode_set", "gpu_conversations_cleanup", "llamafw_enable_set",
	"llamafw_configure", "llamafw_scanners_set", "pii_enable_set", "pii_configure", "pii_url_patterns_set",
}

func TestTierAlignmentOperatorSessionsDoNotListGlobalMutations(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))
	operator, _ := listNames(t, b, guard.RoleOperator)
	admin, _ := listNames(t, b, guard.RoleAdmin)
	for _, name := range adminConfirmTools {
		if operator[name] {
			t.Errorf("operator lists %s", name)
		}
		if !admin[name] {
			t.Errorf("admin does not list %s", name)
		}
	}
	if !operator["lb_create"] {
		t.Error("operator must still list lb_create (the create path is operator tier)")
	}
}

type gateCase struct {
	tool     string
	args     map[string]any
	mutation string // request prefix the apply step must send, and the preview must not
	impact   string
	rollback string // rollback.tool expected, "" when the preview documents why there is none
	readBack string // a key the read-back must carry, "" when the contract has no read
	current  bool   // whether a current state is expected in the preview
}

var gateCases = []gateCase{
	{"secrate_set", map[string]any{"rate_per_sec": 777}, "POST /netlox/v1/config/securityrate", "global", "secrate_set", "ratePerSec", true},
	{"secrate_reset", map[string]any{}, "PUT /netlox/v1/config/securityrate/reset", "global", "", "synBlocked", true},
	{"bgp_neigh_set", map[string]any{"ip_address": "10.0.0.2", "remote_as": 65002}, "POST /netlox/v1/config/bgp/neigh", "global", "bgp_neigh_set", "ipAddress", true},
	{"bgp_global_set", map[string]any{"local_as": 65000}, "POST /netlox/v1/config/bgp/global", "global", "", "", false},
	{"bgp_policy_apply", map[string]any{"ip_address": "10.0.0.2", "policy_type": "import", "policies": []string{"p1"}}, "POST /netlox/v1/config/bgp/policy/apply", "global", "", "", false},
	{"config_params_set", map[string]any{"log_level": "debug"}, "POST /netlox/v1/config/params", "instance", "config_params_set", "logLevel", true},
	{"gpu_mode_set", map[string]any{"enabled": true}, "POST /netlox/v1/config/gpu/enable", "global", "gpu_mode_set", "enabled", true},
	{"gpu_conversations_cleanup", map[string]any{"max_age_hours": 2}, "POST /netlox/v1/config/gpu/conversations/cleanup", "global", "", "worker_count", true},
	{"llamafw_enable_set", map[string]any{"enabled": true}, "POST /netlox/v1/config/llamafirewall/enable", "global", "llamafw_enable_set", "enabled", true},
	{"llamafw_configure", map[string]any{"fail_closed": false}, "POST /netlox/v1/config/llamafirewall/configure", "global", "llamafw_configure", "fail_closed", true},
	{"llamafw_scanners_set", map[string]any{"code_shield": true}, "POST /netlox/v1/config/llamafirewall/scanners", "global", "llamafw_scanners_set", "code_shield", true},
	{"pii_enable_set", map[string]any{"enabled": true}, "POST /netlox/v1/config/pii/enable", "global", "pii_enable_set", "enabled", true},
	{"pii_configure", map[string]any{"mode": "mask"}, "POST /netlox/v1/config/pii/configure", "global", "pii_configure", "mode", true},
	{"pii_url_patterns_set", map[string]any{"mode": "add", "patterns": []map[string]any{{"pattern": "/v2/*"}}}, "POST /netlox/v1/config/pii/url-patterns", "global", "pii_url_patterns_set", "url_patterns", true},
}

// Every raised tool: a call without confirm_token previews and sends no
// mutation; a stale or mismatched token is refused and sends nothing; the
// preview's own token applies once and the result carries the read-back.
func TestTierAlignmentChangeGatePerTool(t *testing.T) {
	for _, tc := range gateCases {
		t.Run(tc.tool, func(t *testing.T) {
			mock := newMockLoxilb(t)
			b := newTestBridge(t, testConfig(mock.srv.URL))
			cs := session(t, b, guard.RoleAdmin)
			ctx := context.Background()

			res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil || res.IsError {
				t.Fatalf("preview call failed: %v %v", err, res)
			}
			out := callOut(t, res)
			if out["action"] != "preview" {
				t.Fatalf("want preview, got %v", out["action"])
			}
			if mock.sawMutation(tc.mutation) {
				t.Fatalf("preview sent %s", tc.mutation)
			}
			if out["impact"] != tc.impact {
				t.Errorf("impact = %v, want %s", out["impact"], tc.impact)
			}
			if _, ok := out["proposed"]; !ok {
				t.Error("preview carries no proposed state")
			}
			if _, ok := out["current"]; ok != tc.current {
				t.Errorf("current present = %v, want %v (%v)", ok, tc.current, out["current"])
			}
			rb, _ := out["rollback"].(map[string]any)
			if rb == nil {
				t.Fatal("preview carries no rollback")
			}
			if tool, _ := rb["tool"].(string); tool != tc.rollback {
				t.Errorf("rollback.tool = %q, want %q", tool, tc.rollback)
			}
			if tc.rollback == "" && rb["note"] == "" {
				t.Error("a preview without a rollback call must say why")
			}
			token, _ := out["confirm_token"].(string)
			if token == "" {
				t.Fatal("no confirm_token")
			}

			// A token that was never issued.
			stale := withToken(tc.args, "0123456789abcdef0123456789abcdef")
			res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: tc.tool, Arguments: stale})
			if err == nil && !res.IsError {
				t.Fatal("stale token accepted")
			}
			if mock.sawMutation(tc.mutation) {
				t.Fatal("stale token sent the mutation")
			}
			// The issued token with a different target binding: mismatch.
			mismatched := withToken(tc.args, token)
			mismatched["target"] = "t1" // same target by name resolves identically; change an argument instead
			mismatched = withChangedArg(tc, mismatched)
			res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: tc.tool, Arguments: mismatched})
			if err == nil && !res.IsError {
				t.Fatal("mismatched arguments accepted with the token")
			}
			if mock.sawMutation(tc.mutation) {
				t.Fatal("mismatched token sent the mutation")
			}

			// The mismatch burned the token; preview again and apply.
			res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil || res.IsError {
				t.Fatalf("second preview failed: %v", err)
			}
			token, _ = callOut(t, res)["confirm_token"].(string)
			res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: tc.tool, Arguments: withToken(tc.args, token)})
			if err != nil || res.IsError {
				t.Fatalf("apply failed: %v %+v", err, res)
			}
			out = callOut(t, res)
			if out["action"] != "executed" {
				t.Fatalf("want executed, got %v", out["action"])
			}
			if !mock.sawMutation(tc.mutation) {
				t.Fatalf("apply sent no %s", tc.mutation)
			}
			if tc.readBack != "" {
				rbk, _ := out["read_back"].(map[string]any)
				if _, ok := rbk[tc.readBack]; !ok {
					t.Errorf("read_back lacks %s: %v", tc.readBack, out["read_back"])
				}
			}
			// Replay of the consumed token.
			res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: tc.tool, Arguments: withToken(tc.args, token)})
			if err == nil && !res.IsError {
				t.Fatal("replayed token accepted")
			}
		})
	}
}

func withToken(args map[string]any, token string) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	out["confirm_token"] = token
	return out
}

// withChangedArg alters one argument so the token binding no longer matches.
func withChangedArg(tc gateCase, args map[string]any) map[string]any {
	switch tc.tool {
	case "secrate_reset":
		args["target"] = "" // identical binding is unavoidable here: use a never-issued token instead
		args["confirm_token"] = "ffffffffffffffffffffffffffffffff"
	case "secrate_set":
		args["rate_per_sec"] = 778
	case "bgp_neigh_set":
		args["remote_as"] = 65003
	case "bgp_global_set":
		args["local_as"] = 65001
	case "bgp_policy_apply":
		args["route_action"] = "reject"
	case "config_params_set":
		args["log_level"] = "trace"
	case "gpu_mode_set", "llamafw_enable_set", "pii_enable_set":
		args["enabled"] = false
	case "gpu_conversations_cleanup":
		args["max_age_hours"] = 3
	case "llamafw_configure":
		args["fail_closed"] = true
	case "llamafw_scanners_set":
		args["code_shield"] = false
	case "pii_configure":
		args["mode"] = "redact"
	case "pii_url_patterns_set":
		args["mode"] = "replace"
	}
	return args
}

// The previews show the state the gate read, and the diff names the change.
func TestTierAlignmentPreviewShowsCurrentAndDiff(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "secrate_set", Arguments: map[string]any{"rate_per_sec": 777, "conn_rate_enabled": true}})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, res)
	}
	out := callOut(t, res)
	cur, _ := out["current"].(map[string]any)
	if cur["ratePerSec"] != float64(500) || cur["synThreshold"] != float64(1000) {
		t.Errorf("current not read from the gateway: %v", cur)
	}
	if cur["connRateEnabled"] != false || cur["synEnabled"] != false {
		t.Errorf("fields the GET omits must read as their zero value: %v", cur)
	}
	prop, _ := out["proposed"].(map[string]any)
	if prop["ratePerSec"] != float64(777) || prop["synThreshold"] != float64(1000) || prop["connRateEnabled"] != true {
		t.Errorf("proposed is not current overlaid with the change: %v", prop)
	}
	diff, _ := out["diff"].(map[string]any)
	if len(diff) != 2 || diff["ratePerSec"] == nil || diff["connRateEnabled"] == nil {
		t.Errorf("diff must name exactly the two changed keys: %v", diff)
	}
	rb, _ := out["rollback"].(map[string]any)
	rbArgs, _ := rb["args"].(map[string]any)
	if rbArgs["rate_per_sec"] != float64(500) || rbArgs["conn_rate_enabled"] != false {
		t.Errorf("rollback args must restore the current values: %v", rbArgs)
	}
	// Apply: the body sent carries the full merged configuration (the REST
	// contract requires every threshold).
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "secrate_set", Arguments: map[string]any{"rate_per_sec": 777, "conn_rate_enabled": true, "confirm_token": token}})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, res)
	}
	body := mock.lastBody()
	for _, k := range []string{"synEnabled", "synThreshold", "cookieThreshold", "connRateEnabled", "ratePerSec", "udpEnabled", "udpPktThreshold", "udpBandwidthMB"} {
		if _, ok := body[k]; !ok {
			t.Errorf("POST body lacks required %s: %v", k, body)
		}
	}
	if body["ratePerSec"] != float64(777) {
		t.Errorf("ratePerSec not applied: %v", body)
	}
	if v, present := body["whitelistIps"]; present && v == nil {
		t.Errorf("an absent list must not be sent as null: %v", body)
	}

	// pii_url_patterns_set add: the proposed list is the current one plus the new pattern.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "pii_url_patterns_set", Arguments: map[string]any{"mode": "add", "patterns": []map[string]any{{"pattern": "/v2/*", "exclude": true}}}})
	if err != nil || res.IsError {
		t.Fatalf("pii preview: %v %v", err, res)
	}
	out = callOut(t, res)
	prop, _ = out["proposed"].(map[string]any)
	if l, _ := prop["url_patterns"].([]any); len(l) != 2 {
		t.Errorf("proposed url_patterns = %v, want current + 1", prop["url_patterns"])
	}
	rb, _ = out["rollback"].(map[string]any)
	rbArgs, _ = rb["args"].(map[string]any)
	if rbArgs["mode"] != "replace" {
		t.Errorf("rollback must replace with the current list: %v", rbArgs)
	}
}

// NeverAutopilot: listing a raised tool as autopilot changes nothing, the
// preview is still returned and nothing is sent.
func TestTierAlignmentNeverAutopilotStillRefuses(t *testing.T) {
	mock := newMockLoxilb(t)
	pol := &guard.Policy{Autopilot: adminConfirmTools}
	b, err := NewBridge(testConfig(mock.srv.URL), pol, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	for _, tc := range gateCases {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %v", tc.tool, err, res)
		}
		if out := callOut(t, res); out["action"] != "preview" {
			t.Errorf("%s executed without a token although it is never autopilot-eligible", tc.tool)
		}
		if mock.sawMutation(tc.mutation) {
			t.Errorf("%s sent %s under autopilot", tc.tool, tc.mutation)
		}
	}
}

// lb_create: the create/replace split.
func TestTierAlignmentLBCreateSplit(t *testing.T) {
	ctx := context.Background()
	// The mock holds 1.2.3.4:80/tcp with host models.example, path /v1, model llama3.
	replace := map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp", "mode": 4,
		"host": "models.example", "path_prefix": "/v1", "model_name": "llama3",
		"endpoints": []map[string]any{{"ip": "10.0.0.5", "port": 8000}}}
	create := map[string]any{"external_ip": "9.9.9.9", "port": 80, "protocol": "tcp",
		"endpoints": []map[string]any{{"ip": "10.0.0.5", "port": 8000}}}
	sibling := map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp", "mode": 4,
		"host": "other.example", "endpoints": []map[string]any{{"ip": "10.0.0.5", "port": 8000}}}

	t.Run("admin replace previews then applies with the token", func(t *testing.T) {
		mock := newMockLoxilb(t)
		b := newTestBridge(t, testConfig(mock.srv.URL))
		cs := session(t, b, guard.RoleAdmin)
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: replace})
		if err != nil || res.IsError {
			t.Fatalf("preview: %v %v", err, res)
		}
		out := callOut(t, res)
		if out["action"] != "preview" || out["impact"] != "service" {
			t.Fatalf("want a service-impact preview, got %v", out)
		}
		if mock.sawMutation("POST ") {
			t.Fatal("preview sent a POST")
		}
		diff, _ := out["diff"].(map[string]any)
		if diff["endpoints"] == nil {
			t.Errorf("diff must show the endpoint change: %v", diff)
		}
		rb, _ := out["rollback"].(map[string]any)
		rbArgs, _ := rb["args"].(map[string]any)
		if rb["tool"] != "lb_create" || rbArgs["host"] != "models.example" {
			t.Errorf("rollback must re-create the current rule: %v", rb)
		}
		eps, _ := rbArgs["endpoints"].([]any)
		if len(eps) != 1 {
			t.Errorf("rollback endpoints: %v", rbArgs["endpoints"])
		}
		token, _ := out["confirm_token"].(string)
		res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: withToken(replace, token)})
		if err != nil || res.IsError {
			t.Fatalf("apply: %v %v", err, res)
		}
		if callOut(t, res)["action"] != "executed" || !mock.sawMutation("POST /netlox/v1/config/loadbalancer") {
			t.Fatal("apply did not POST")
		}
	})
	t.Run("admin replace with a stale token is refused", func(t *testing.T) {
		mock := newMockLoxilb(t)
		b := newTestBridge(t, testConfig(mock.srv.URL))
		cs := session(t, b, guard.RoleAdmin)
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: withToken(replace, "0123456789abcdef0123456789abcdef")})
		if err == nil && !res.IsError {
			t.Fatal("stale token accepted")
		}
		if mock.sawMutation("POST ") {
			t.Fatal("stale token sent a POST")
		}
	})
	t.Run("operator replace is refused before any request", func(t *testing.T) {
		mock := newMockLoxilb(t)
		b := newTestBridge(t, testConfig(mock.srv.URL))
		cs := session(t, b, guard.RoleOperator)
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: replace})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatalf("operator replaced a rule: %v", callOut(t, res))
		}
		if txt := resultText(res); !strings.Contains(txt, "admin-confirm") {
			t.Errorf("refusal must name the tier: %s", txt)
		}
		if mock.sawMutation("POST ") {
			t.Fatal("operator refusal sent a POST")
		}
	})
	t.Run("create path is unchanged for operator and admin", func(t *testing.T) {
		for _, role := range []guard.Role{guard.RoleOperator, guard.RoleAdmin} {
			mock := newMockLoxilb(t)
			b := newTestBridge(t, testConfig(mock.srv.URL))
			cs := session(t, b, role)
			res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: create})
			if err != nil || res.IsError {
				t.Fatalf("%s create: %v %v", role, err, res)
			}
			if callOut(t, res)["action"] != "executed" || !mock.sawMutation("POST /netlox/v1/config/loadbalancer") {
				t.Fatalf("%s create did not execute directly", role)
			}
			if !mock.sawMutation("GET /netlox/v1/config/loadbalancer/externalipaddress/9.9.9.9/port/80/protocol/tcp") {
				t.Errorf("%s create skipped the pre-read", role)
			}
		}
	})
	t.Run("a sibling host on a shared VIP is a create", func(t *testing.T) {
		mock := newMockLoxilb(t)
		b := newTestBridge(t, testConfig(mock.srv.URL))
		cs := session(t, b, guard.RoleOperator)
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: sibling})
		if err != nil || res.IsError {
			t.Fatalf("sibling create: %v %v", err, res)
		}
		if callOut(t, res)["action"] != "executed" {
			t.Fatal("sibling host was treated as a replace")
		}
	})
}

func resultText(res *sdk.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
