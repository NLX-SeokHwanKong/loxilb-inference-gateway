/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// The gateway serialises adminStateUp with omitempty. Reading an absent
// flag as "paused" is safe only while an enabled rule always carries an
// explicit true; the fixture is the live gateway's answer for one rule,
// enabled and then paused, and these tests run the tools against it.

type adminStateFixture struct {
	Key     map[string]any `json:"key"`
	Enabled struct {
		Rule   json.RawMessage `json:"rule"`
		Status json.RawMessage `json:"status"`
	} `json:"enabled"`
	Paused struct {
		Rule   json.RawMessage `json:"rule"`
		Status json.RawMessage `json:"status"`
	} `json:"paused"`
}

const fixtureLBKey = "/netlox/v1/config/loadbalancer/externalipaddress/20.20.20.1/port/2099/protocol/tcp"

func loadAdminStateFixture(t *testing.T) adminStateFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/adminstateup-latest-u24.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx adminStateFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func fixtureSvc(t *testing.T, rule json.RawMessage) map[string]any {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal(rule, &r); err != nil {
		t.Fatal(err)
	}
	return mapValue(r["serviceArguments"])
}

// The premise itself: an enabled rule's GET carries adminStateUp true, a
// paused rule's GET carries no adminStateUp. If a re-captured fixture
// shows an enabled rule without the explicit true, the inference in
// lb_update's read-back is unsafe and must go.
func TestAdminStateFixtureEnabledCarriesExplicitTrue(t *testing.T) {
	fx := loadAdminStateFixture(t)
	if v, has := fixtureSvc(t, fx.Enabled.Rule)["adminStateUp"]; !has || v != true {
		t.Fatalf("enabled rule must carry an explicit adminStateUp true, got %v (present %v): absent is then not a pause marker", v, has)
	}
	if _, has := fixtureSvc(t, fx.Paused.Rule)["adminStateUp"]; has {
		t.Fatalf("paused rule carries adminStateUp; the omitempty premise no longer holds, re-check the tri-state wording")
	}
}

func TestLBGetAndListReportTheFixtureTriState(t *testing.T) {
	fx := loadAdminStateFixture(t)
	for _, tc := range []struct {
		name string
		rule json.RawMessage
		want any
	}{
		{"enabled", fx.Enabled.Rule, true},
		{"paused", fx.Paused.Rule, "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockLoxilb(t)
			mock.routes = map[string]string{
				fixtureLBKey:                         string(tc.rule),
				"/netlox/v1/config/loadbalancer/all": `{"lbAttr":[` + string(tc.rule) + `]}`,
			}
			b := newTestBridge(t, testConfig(mock.srv.URL))
			cs := session(t, b, guard.RoleViewer)
			ctx := context.Background()
			res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_get", Arguments: fx.Key})
			if err != nil || res.IsError {
				t.Fatalf("lb_get: %v %v", err, resultText(res))
			}
			if got := mapValue(callOut(t, res)["rule"])["admin_state_up"]; got != tc.want {
				t.Errorf("lb_get admin_state_up = %v, want %v", got, tc.want)
			}
			res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_list", Arguments: map[string]any{}})
			if err != nil || res.IsError {
				t.Fatalf("lb_list: %v %v", err, resultText(res))
			}
			raw, _ := json.Marshal(res.StructuredContent)
			want, _ := json.Marshal(tc.want)
			if !strings.Contains(string(raw), `"admin_state_up":`+string(want)) {
				t.Errorf("lb_list must report admin_state_up %s: %s", want, raw)
			}
		})
	}
}

// Pausing the fixture rule: the preview shows the explicit true, the
// read-back after sending false sees the paused GET (no flag) and is the
// one place that resolves absent to false, saying so.
func TestLBUpdatePauseReadBackResolvesAbsentFromTheFixture(t *testing.T) {
	fx := loadAdminStateFixture(t)
	mock := newMockLoxilb(t)
	mock.routes = map[string]string{fixtureLBKey: string(fx.Enabled.Rule), fixtureLBKey + "/status": string(fx.Enabled.Status)}
	mock.routesAfterPatch = map[string]string{fixtureLBKey: string(fx.Paused.Rule), fixtureLBKey + "/status": string(fx.Paused.Status)}
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	args := withArgs(fx.Key, map[string]any{"admin_state_up": false})

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	if cur := mapValue(out["current"]); cur["adminStateUp"] != true {
		t.Errorf("preview current must show the explicit true: %v", cur)
	}
	if rb := mapValue(mapValue(out["rollback"])["args"]); rb["admin_state_up"] != true {
		t.Errorf("rollback must restore true: %v", out["rollback"])
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	rbk := mapValue(callOut(t, res)["read_back"])
	if rbk["admin_state_up"] != false {
		t.Errorf("read_back after sending false resolves the absent flag to false: %v", rbk)
	}
	if note, _ := rbk["admin_state_up_note"].(string); !strings.Contains(note, "omitempty") {
		t.Errorf("read_back must say why absent reads as false: %v", rbk)
	}
	if st := mapValue(rbk["status"]); st["operatingStatus"] != "OFFLINE" {
		t.Errorf("read_back status must be the paused status: %v", st)
	}
}

// Resuming the paused fixture rule: the preview reports absent as read and
// its rollback names no admin_state_up value; the read-back after sending
// true sees the explicit true and carries no note.
func TestLBUpdateResumeKeepsAbsentInThePreview(t *testing.T) {
	fx := loadAdminStateFixture(t)
	mock := newMockLoxilb(t)
	mock.routes = map[string]string{fixtureLBKey: string(fx.Paused.Rule), fixtureLBKey + "/status": string(fx.Paused.Status)}
	mock.routesAfterPatch = map[string]string{fixtureLBKey: string(fx.Enabled.Rule), fixtureLBKey + "/status": string(fx.Enabled.Status)}
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()
	args := withArgs(fx.Key, map[string]any{"admin_state_up": true})

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, resultText(res))
	}
	out := callOut(t, res)
	if cur := mapValue(out["current"]); cur["adminStateUp"] != "absent" {
		t.Errorf("preview current must report the flag as read (absent): %v", cur)
	}
	rb := mapValue(out["rollback"])
	if _, has := mapValue(rb["args"])["admin_state_up"]; has {
		t.Errorf("rollback must not state a value for an absent flag: %v", rb)
	}
	if note, _ := rb["note"].(string); !strings.Contains(note, "absent") {
		t.Errorf("rollback note must name the absent flag: %v", rb)
	}
	token, _ := out["confirm_token"].(string)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_update", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("apply: %v %v", err, resultText(res))
	}
	rbk := mapValue(callOut(t, res)["read_back"])
	if rbk["admin_state_up"] != true {
		t.Errorf("read_back after resuming must be true: %v", rbk)
	}
	if _, has := rbk["admin_state_up_note"]; has {
		t.Errorf("no note when the GET carries the flag: %v", rbk)
	}
}
