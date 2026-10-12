/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 */

package common

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func mcpTestService() LbServiceArg {
	return LbServiceArg{MCPProtocolVersion: MCPStatelessProtocolVersion, Mode: LBModeFullProxy, Proto: "tcp", Sel: LbSelRr}
}
func TestMCPServiceRefusesIncompatibleSettings(t *testing.T) {
	cases := []struct {
		name, word string
		set        func(*LbServiceArg)
	}{
		{"version", "mcp_protocol_version", func(s *LbServiceArg) { s.MCPProtocolVersion = "2025-11-25" }},
		{"nul-version", "mcp_protocol_version", func(s *LbServiceArg) { s.MCPProtocolVersion = MCPStatelessProtocolVersion + "\x00" }},
		{"nat", "fullproxy", func(s *LbServiceArg) { s.Mode = LBModeFullNAT }},
		{"udp", "tcp", func(s *LbServiceArg) { s.Proto = "udp" }},
		{"backend-h2", "backend_protocol", func(s *LbServiceArg) { s.BackendProtocol = "http2" }},
		{"backend-both", "backend_protocol", func(s *LbServiceArg) { s.BackendProtocol = "both" }},
		{"backend-unknown", "backend_protocol", func(s *LbServiceArg) { s.BackendProtocol = "future" }},
		{"egress", "egress", func(s *LbServiceArg) { s.Egress = true }},
		{"persist", "sel=0", func(s *LbServiceArg) { s.Sel = LbSelRrPersist }},
		{"hash", "sel=0", func(s *LbServiceArg) { s.Sel = LbSelHash }},
		{"session", "session_header_name", func(s *LbServiceArg) { s.SessionHeaderName = "mcp-session-id" }},
		{"model", "model", func(s *LbServiceArg) { s.ModelName = "model-a" }},
		{"pd", "P/D", func(s *LbServiceArg) { s.PDDisaggMode = true }},
		{"pd-cache", "P/D", func(s *LbServiceArg) { s.PDCacheAwareMode = true }},
		{"kv", "KV", func(s *LbServiceArg) { s.KvExactMode = 3 }},
		{"kv-profile", "KV", func(s *LbServiceArg) { s.KvModelProfile = "profile-a" }},
		{"kv-api", "KV", func(s *LbServiceArg) { s.KvExactApiMode = "both" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mcpTestService()
			c.set(&s)
			if err := ValidateMCPService(&s); err == nil || !strings.Contains(err.Error(), c.word) {
				t.Fatalf("want %q rejection, got %v", c.word, err)
			}
		})
	}
	for _, mode := range []string{SockMapModeBoth, SockMapModeRequest, SockMapModeResponse} {
		s := mcpTestService()
		s.SockMapMode = mode
		if err := ValidateMCPService(&s); err == nil {
			t.Fatalf("sockmap %s bypass accepted", mode)
		}
	}
}
func TestMCPServiceAllowsLegacyAndStatelessSelectors(t *testing.T) {
	legacy := LbServiceArg{Mode: LBModeFullNAT, Proto: "udp", Sel: LbSelRrPersist, SessionHeaderName: "legacy-session"}
	legacy.BackendProtocol, legacy.Egress = "both", true
	if err := ValidateMCPService(&legacy); err != nil {
		t.Fatal(err)
	}
	for _, sel := range []EpSelect{LbSelRr, LbSelPrio} {
		s := mcpTestService()
		s.Sel = sel
		s.SockMapMode = SockMapModeOff
		s.BackendProtocol = "http1"
		if err := ValidateMCPService(&s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMCPL7PolicyProtectsValidatedHeadersAndStatelessSelection(t *testing.T) {
	for _, name := range []string{"MCP-Protocol-Version", "mcp-method", "mcp-name", "Mcp-Session-Id", "Accept", "Content-Type", "Content-Length", "Transfer-Encoding"} {
		for _, op := range []string{L7HdrOpSet, L7HdrOpAdd, L7HdrOpRemove} {
			routes := []L7RuleArg{{InsertHeaders: []L7HeaderFilterArg{{Name: name, Op: op}}}}
			if err := ValidateMCPL7Policy(routes); err == nil {
				t.Fatalf("validated header %s/%s may be rewritten", name, op)
			}
		}
	}
	for _, mode := range []string{"HTTP_COOKIE", "APP_COOKIE", "SOURCE_IP"} {
		if err := ValidateMCPL7Policy([]L7RuleArg{{SessionPersistence: mode}}); err == nil {
			t.Fatalf("affinity %s accepted", mode)
		}
	}
	if err := ValidateMCPL7Policy([]L7RuleArg{{InsertHeaders: []L7HeaderFilterArg{{Name: "X-Request-Id", Op: L7HdrOpSet, Value: "trace"}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestMCPDomainJSONPreservesMissingEmptyAndRejectsNull(t *testing.T) {
	for _, c := range []struct {
		raw, version    string
		present, reject bool
	}{
		{`{}`, "", false, false},
		{`{"mcp_protocol_version":""}`, "", false, false},
		{`{"mcp_protocol_version":"2026-07-28"}`, MCPStatelessProtocolVersion, false, false},
		{`{"mcp_protocol_version":null}`, "", false, true},
	} {
		var serv LbServiceArg
		err := json.Unmarshal([]byte(c.raw), &serv)
		if (err != nil) != c.reject {
			t.Fatalf("%s: rejected=%v want %v", c.raw, err != nil, c.reject)
		}
		if !c.reject && (string(serv.MCPProtocolVersion) != c.version || serv.MCPProtocolVersionPresent != c.present) {
			t.Fatalf("%s: lost value/presence: %+v", c.raw, serv)
		}
	}
	kept := mcpTestService()
	if err := json.Unmarshal([]byte(`{"mcp_protocol_version":null}`), &kept); err == nil || kept.MCPProtocolVersion != MCPStatelessProtocolVersion {
		t.Fatal("null downgraded an existing domain declaration")
	}
}

func TestMCPFieldDecoderPreservesStrictServiceUnknownFieldChecks(t *testing.T) {
	decoder := json.NewDecoder(bytes.NewBufferString(`{"mcp_protocol_version":"2026-07-28","unexpected_mcp_option":true}`))
	decoder.DisallowUnknownFields()
	var serv LbServiceArg
	if err := decoder.Decode(&serv); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("MCP decoder bypassed strict service field validation: %v", err)
	}
}

func TestMCPL7PolicyRefusesConditionTruncation(t *testing.T) {
	for _, c := range []L7ConditionArg{{Key: strings.Repeat("k", 64)}, {Value: strings.Repeat("v", 256)}, {Key: "x\x00y"}, {Value: "a\x00b"}} {
		if ValidateMCPL7Policy([]L7RuleArg{{MatchSets: []L7MatchSetArg{{Conditions: []L7ConditionArg{c}}}}}) == nil {
			t.Fatalf("truncating condition accepted: %#v", c)
		}
	}
	if ValidateMCPL7Policy(make([]L7RuleArg, 257)) == nil {
		t.Fatal("route cursor bound exceeded")
	}
	if err := ValidateMCPL7Policy([]L7RuleArg{{MatchSets: []L7MatchSetArg{{Conditions: []L7ConditionArg{{Key: strings.Repeat("k", 63), Value: strings.Repeat("v", 255)}}}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestMCPProtocolVersionDatapathLockstep(t *testing.T) {
	raw, err := os.ReadFile("../loxilb-ebpf/common/sockproxy_mcp.h")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`(?m)^#define SP_MCP_VERSION "([^"]+)"$`).FindSubmatch(raw)
	if len(matches) != 2 || string(matches[1]) != MCPStatelessProtocolVersion {
		t.Fatal("Go and C MCP version declarations differ")
	}
}
