/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 */

package loxinet

import (
	"errors"
	cmn "github.com/loxilb-io/loxilb/common"
	"net"
	"strings"
	"testing"
)

func TestMCPAdmissionBeforeStateAccess(t *testing.T) {
	for _, version := range []string{"future", cmn.MCPStatelessProtocolVersion} {
		serv := cmn.LbServiceArg{MCPProtocolVersion: cmn.MCPVersion(version), Mode: cmn.LBModeFullNAT, Proto: "tcp"}
		var rules *RuleH
		code, err := rules.AddLbRule(serv, nil, nil, nil, nil)
		var argumentError *cmn.RuleArgumentError
		if code != RuleArgsErr || !errors.As(err, &argumentError) {
			t.Fatalf("version=%q: want typed pre-mutation refusal, got %d %v", version, code, err)
		}
	}
}

func TestMCPUnsupportedCombinationsBeforeStateAccess(t *testing.T) {
	for _, backend := range []string{"http2", "both"} {
		serv := cmn.LbServiceArg{MCPProtocolVersion: cmn.MCPStatelessProtocolVersion, Mode: cmn.LBModeFullProxy, Proto: "tcp", BackendProtocol: backend}
		var rules *RuleH
		code, err := rules.AddLbRule(serv, nil, nil, nil, nil)
		var argumentError *cmn.RuleArgumentError
		if code != RuleArgsErr || !errors.As(err, &argumentError) {
			t.Fatalf("backend=%q: want typed pre-mutation refusal, got %d %v", backend, code, err)
		}
	}
	serv := cmn.LbServiceArg{MCPProtocolVersion: cmn.MCPStatelessProtocolVersion, Mode: cmn.LBModeFullProxy, Proto: "tcp", Egress: true}
	var rules *RuleH
	code, err := rules.AddLbRule(serv, nil, nil, nil, nil)
	var argumentError *cmn.RuleArgumentError
	if code != RuleArgsErr || !errors.As(err, &argumentError) {
		t.Fatalf("egress: want typed pre-mutation refusal, got %d %v", code, err)
	}
	// Replacement resolves an omitted declaration before validating the combination.
	existing := &ruleEnt{mcpProtocolVersion: cmn.MCPStatelessProtocolVersion}
	for _, backend := range []string{"http2", "both"} {
		omitted := cmn.LbServiceArg{Mode: cmn.LBModeFullProxy, Proto: "tcp", BackendProtocol: backend}
		if err := resolveMCPService(&omitted, existing); err == nil || !strings.Contains(err.Error(), "backend_protocol") {
			t.Fatalf("omission bypassed backend guard: %v", err)
		}
	}
	if existing.mcpProtocolVersion != cmn.MCPStatelessProtocolVersion {
		t.Fatal("refusal mutated existing rule")
	}
}
func TestMCPReplacePreservesAndRejectsUnsafeTransitions(t *testing.T) {
	valid := func() cmn.LbServiceArg {
		return cmn.LbServiceArg{Mode: cmn.LBModeFullProxy, Proto: "tcp", Sel: cmn.LbSelRr}
	}
	for _, old := range []string{"", cmn.MCPStatelessProtocolVersion} {
		existing := &ruleEnt{mcpProtocolVersion: old}
		omitted := valid()
		if err := resolveMCPService(&omitted, existing); err != nil || string(omitted.MCPProtocolVersion) != old {
			t.Fatalf("omission lost mode %q: %v %+v", old, err, omitted)
		}
		same := valid()
		same.MCPProtocolVersion = cmn.MCPVersion(old)
		same.MCPProtocolVersionPresent = true
		if err := resolveMCPService(&same, existing); err != nil {
			t.Fatalf("same explicit mode refused: %v", err)
		}
		changed := valid()
		changed.MCPProtocolVersionPresent = true
		if old == "" {
			changed.MCPProtocolVersion = cmn.MCPStatelessProtocolVersion
		}
		if err := resolveMCPService(&changed, existing); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("transition from %q accepted: %v", old, err)
		}
		if existing.mcpProtocolVersion != old {
			t.Fatal("refused replace mutated old rule")
		}
	}
	existing := &ruleEnt{mcpProtocolVersion: cmn.MCPStatelessProtocolVersion}
	incompatible := valid()
	incompatible.SessionHeaderName = "mcp-session-id"
	if err := resolveMCPService(&incompatible, existing); err == nil {
		t.Fatal("omitted protocol bypassed resolved-mode validation")
	}
}

func TestMCPListenerAliasesCannotMixTransportModes(t *testing.T) {
	_, prefix, _ := net.ParseCIDR("192.0.2.10/32")
	tuple := func(port uint16) ruleTuples {
		return ruleTuples{l3Dst: ruleIPTuple{*prefix}, l4Prot: rule8Tuple{6, 255}, l4Dst: rule16RTuple{port, port, true}}
	}
	peer := &ruleEnt{tuples: tuple(8080), act: ruleAct{action: &ruleLBActs{mode: cmn.LBModeFullProxy}}}
	peer.tuples.path = "alias.example"
	peer.tuples.pathPrefix = "/mcp"
	rules := newTestRuleH()
	rules.tables[RtLB].eMap["peer"] = peer
	rt := tuple(8080)
	if rules.lbListenerMCPConflict(nil, &rt, cmn.MCPStatelessProtocolVersion) != peer {
		t.Fatal("host/path alias bypassed listener transport consistency")
	}
	peer.act.action.(*ruleLBActs).mode = cmn.LBModeFullNAT
	if rules.lbListenerMCPConflict(nil, &rt, cmn.MCPStatelessProtocolVersion) != peer {
		t.Fatal("L4 alias bypassed MCP admission through shared address/port")
	}
	peer.act.action.(*ruleLBActs).mode = cmn.LBModeFullProxy
	if rules.lbListenerMCPConflict(nil, &rt, "") != nil {
		t.Fatal("legacy-only listener rejected")
	}
	if rules.lbListenerMCPConflict(peer, &rt, cmn.MCPStatelessProtocolVersion) != nil {
		t.Fatal("self should be excluded")
	}
	peer.mcpProtocolVersion = cmn.MCPStatelessProtocolVersion
	if rules.lbListenerMCPConflict(nil, &rt, "") != peer {
		t.Fatal("legacy alias added to modern listener")
	}
	rt = tuple(8081)
	if rules.lbListenerMCPConflict(nil, &rt, "") != nil {
		t.Fatal("separate port rejected")
	}
	rt = tuple(8079)
	rt.l4Dst.valMax = 8081
	if rules.lbListenerMCPConflict(nil, &rt, "") != peer {
		t.Fatal("overlapping port range bypassed consistency")
	}
}

func TestMCPLegacyConflictIndexLifecycle(t *testing.T) {
	rules := newTestRuleH()
	rules.mcpRules = make(map[string]*ruleEnt)
	_, prefix, _ := net.ParseCIDR("192.0.2.10/32")
	rt := ruleTuples{l3Dst: ruleIPTuple{*prefix}, l4Prot: rule8Tuple{6, 255}, l4Dst: rule16RTuple{8080, 8080, true}}
	peer := &ruleEnt{tuples: rt, mcpProtocolVersion: cmn.MCPStatelessProtocolVersion}
	rules.tables[RtLB].eMap["modern"] = peer
	rules.trackMCPLbRule("modern", peer)
	if rules.lbListenerMCPConflict(nil, &rt, "") != peer {
		t.Fatal("index missed modern conflict")
	}
	delete(rules.tables[RtLB].eMap, "modern")
	rules.trackMCPLbRule("modern", nil)
	if rules.lbListenerMCPConflict(nil, &rt, "") != nil || len(rules.mcpRules) != 0 {
		t.Fatal("deleted rule remains indexed")
	}
	peer.mcpProtocolVersion = ""
	rules.tables[RtLB].eMap["legacy"] = peer
	rules.trackMCPLbRule("legacy", peer)
	if len(rules.mcpRules) != 0 || rules.lbListenerMCPConflict(nil, &rt, "") != nil {
		t.Fatal("legacy rule indexed as modern")
	}
	// Modern additions must still scan the primary map to refuse an existing legacy rule.
	if rules.lbListenerMCPConflict(nil, &rt, cmn.MCPStatelessProtocolVersion) != peer {
		t.Fatal("modern addition bypassed legacy peer")
	}
}
