/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/tools"
)

// The mutation budget (change-gate requirement 4, D12): at most N mutations
// sent per sliding minute per caller, a stop after M consecutive failures
// lifted by a successful read-only tool call, both enforced by the bridge
// before a request is built.

var fwArgs = map[string]any{"match": map[string]any{"source_ip": "10.0.0.0/24", "protocol": "tcp"}, "action": "drop"}

func budgetBridge(t *testing.T, mock *mockLoxilb, perMinute, maxFail int) *Bridge {
	t.Helper()
	cfg := testConfig(mock.srv.URL)
	cfg.ChangeBudget = ChangeBudgetConfig{MutationsPerMinute: perMinute, MaxConsecutiveFailures: maxFail}
	return newTestBridge(t, cfg)
}

func TestChangeBudgetConfigValidation(t *testing.T) {
	for _, bad := range []ChangeBudgetConfig{{MutationsPerMinute: -1}, {MaxConsecutiveFailures: -2}} {
		cfg := testConfig("http://127.0.0.1:11111")
		cfg.ChangeBudget = bad
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "change_budget") {
			t.Errorf("accepted %+v: %v", bad, err)
		}
	}
	cfg := testConfig("http://127.0.0.1:11111")
	cfg.Clients = []ClientToken{{Name: "a", Role: "operator", Token: testToken, MutationsPerMinute: -1}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "mutations_per_minute") {
		t.Errorf("negative per-client budget accepted: %v", err)
	}
	cfg.Clients[0].MutationsPerMinute = 1
	cfg.ChangeBudget = ChangeBudgetConfig{MutationsPerMinute: 1, MaxConsecutiveFailures: 1}
	if err := cfg.validate(); err != nil {
		t.Errorf("the floors are valid values: %v", err)
	}
	b := newTestBridge(t, cfg)
	if pm, mf := b.budget.Limits(); pm != 1 || mf != 1 {
		t.Errorf("bridge budget = %d/%d, want 1/1", pm, mf)
	}
	b.budget.Record("mcp:a", true)
	if err := b.budget.Check("mcp:a"); err == nil {
		t.Error("the client's own allowance (1/min) must apply under its originator key")
	}
	if err := b.budget.Check("mcp:other"); err != nil {
		t.Errorf("another client is not affected: %v", err)
	}
}

func TestChangeBudgetRefusesTheMutationPastTheWindow(t *testing.T) {
	mock := newMockLoxilb(t)
	b := budgetBridge(t, mock, 1, 3)
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fw_create", Arguments: fwArgs})
	if err != nil || res.IsError {
		t.Fatalf("first mutation: %v %v", err, res)
	}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "fw_create", Arguments: fwArgs})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "mutation budget: 1 of 1 mutations per minute") {
		t.Fatalf("second mutation inside the minute must be refused: %v", resultText(res))
	}
	if n := mock.countRequests("POST /netlox/v1/config/firewall"); n != 1 {
		t.Fatalf("the refused mutation was sent (%d POSTs)", n)
	}
	// Reads are never counted.
	for i := 0; i < 3; i++ {
		if res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "version_get", Arguments: map[string]any{}}); err != nil || res.IsError {
			t.Fatalf("read %d refused: %v %v", i, err, res)
		}
	}
	// Previews are never counted either: a preview after the window is
	// full still answers.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "config_params_set", Arguments: map[string]any{"log_level": "debug"}})
	if err != nil || res.IsError || callOut(t, res)["action"] != "preview" {
		t.Fatalf("a preview is not a mutation: %v %v", err, res)
	}
}

func TestChangeBudgetRefusedApplyKeepsTheToken(t *testing.T) {
	mock := newMockLoxilb(t)
	b := budgetBridge(t, mock, 1, 3)
	now := time.Now()
	b.budget.SetClock(func() time.Time { return now })
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()

	args := map[string]any{"log_level": "debug"}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "config_params_set", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview: %v %v", err, res)
	}
	token, _ := callOut(t, res)["confirm_token"].(string)
	if res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fw_create", Arguments: fwArgs}); err != nil || res.IsError {
		t.Fatalf("spending the budget: %v %v", err, res)
	}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "config_params_set", Arguments: withToken(args, token)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "mutation budget") {
		t.Fatalf("the apply must be refused by the budget: %v", resultText(res))
	}
	if mock.sawMutation("POST /netlox/v1/config/params") {
		t.Fatal("the refused apply reached the gateway")
	}
	now = now.Add(61 * time.Second)
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "config_params_set", Arguments: withToken(args, token)})
	if err != nil || res.IsError {
		t.Fatalf("the preview's token must survive the refused apply: %v %v", err, resultText(res))
	}
	if callOut(t, res)["action"] != "executed" || !mock.sawMutation("POST /netlox/v1/config/params") {
		t.Fatal("apply with the kept token did not execute")
	}
}

func TestChangeBudgetStopIsLiftedByAReadTool(t *testing.T) {
	mock := newMockLoxilb(t)
	b := budgetBridge(t, mock, 60, 2)
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()

	mock.setFailing("POST /netlox/v1/config/firewall")
	for i := 0; i < 2; i++ {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fw_create", Arguments: fwArgs})
		if err != nil || !res.IsError || !strings.Contains(resultText(res), "injected failure") {
			t.Fatalf("failure %d must surface the gateway's answer: %v %v", i+1, err, res)
		}
	}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fw_create", Arguments: fwArgs})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "stopped after 2 consecutive failed mutations") {
		t.Fatalf("third mutation must be refused as stopped: %v", resultText(res))
	}
	if n := mock.countRequests("POST /netlox/v1/config/firewall"); n != 2 {
		t.Fatalf("the stopped mutation was sent (%d POSTs)", n)
	}
	// A failed read lifts nothing; a mutation's own pre-read lifts nothing.
	mock.setFailing("GET /netlox/v1/version")
	if res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "version_get", Arguments: map[string]any{}}); err != nil || !res.IsError {
		t.Fatalf("the injected read failure did not surface: %v %v", err, res)
	}
	if res, _ := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fw_create", Arguments: fwArgs}); !res.IsError || !strings.Contains(resultText(res), "stopped") {
		t.Fatal("a failed read must not lift the stop")
	}
	mock.setFailing("")
	if res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "version_get", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("read: %v %v", err, res)
	}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "fw_create", Arguments: fwArgs})
	if err != nil || res.IsError {
		t.Fatalf("after a successful read the mutation must be sent again: %v %v", err, resultText(res))
	}
	if n := mock.countRequests("POST /netlox/v1/config/firewall"); n != 3 {
		t.Fatalf("want 3 POSTs after the lift, got %d", n)
	}
}

// Every registered tool is known to IsMutating by name, listed or not:
// a read registered without a reg closure (seed, monitoring, fleet) must
// still count as a read, or it could never lift the stop.
func TestChangeBudgetKnowsEveryRegisteredTool(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))
	cs := session(t, b, guard.RoleAdmin)
	res, err := cs.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		ro := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		if got := !tools.IsMutating(tool.Name); got != ro {
			t.Errorf("%s: IsMutating says read=%v, annotations say read=%v", tool.Name, got, ro)
		}
	}
}
