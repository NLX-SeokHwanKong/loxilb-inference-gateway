/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 */

package handler

import (
	cmn "github.com/loxilb-io/loxilb/common"
	"net/http"
	"testing"
)

func TestMCPReadbackAndPresence(t *testing.T) {
	m := serializeLBRule(cmn.LbRuleMod{Serv: cmn.LbServiceArg{MCPProtocolVersion: cmn.MCPStatelessProtocolVersion}})
	if m.ServiceArguments.McpProtocolVersion == nil || *m.ServiceArguments.McpProtocolVersion != cmn.MCPStatelessProtocolVersion {
		t.Fatal("MCP protocol declaration missing from GET")
	}
	legacy := serializeLBRule(cmn.LbRuleMod{})
	if legacy.ServiceArguments.McpProtocolVersion != nil {
		t.Fatal("legacy GET unexpectedly enables MCP")
	}
	p, err := parseLoadbalancerRequestPresence([]byte(`{"serviceArguments":{"mcp_protocol_version":null}}`))
	if err != nil || !p.svcIsNull("mcp_protocol_version") {
		t.Fatalf("JSON null was not recognized: %v", err)
	}
}

func TestMCPPostNullRefusedBeforeRuleMutation(t *testing.T) {
	params := newCreateParams("")
	params.HTTPRequest = params.HTTPRequest.WithContext(WithRawLoadbalancerBody(params.HTTPRequest.Context(), []byte(`{"serviceArguments":{"mcp_protocol_version":null}}`)))
	old := ApiHooks
	stub := &stubLbAddHook{}
	ApiHooks = stub
	defer func() { ApiHooks = old }()
	result := ConfigPostLoadbalancer(params, nil)
	errResult, ok := result.(*ErrorResponse)
	if !ok || errResult.Payload == nil || errResult.Payload.Code != http.StatusBadRequest {
		t.Fatalf("null must answer 400: %T %#v", result, result)
	}
	if stub.captured != nil {
		t.Fatal("JSON null reached rule hook")
	}
}
func TestMCPPostCopiesExplicitPresence(t *testing.T) {
	for _, c := range []struct {
		version *string
		present bool
	}{{nil, false}, {new(string), true}, {func() *string { v := cmn.MCPStatelessProtocolVersion; return &v }(), true}} {
		params := newCreateParams("")
		params.Attr.ServiceArguments.McpProtocolVersion = c.version
		old := ApiHooks
		stub := &stubLbAddHook{}
		ApiHooks = stub
		ConfigPostLoadbalancer(params, nil)
		ApiHooks = old
		if stub.captured == nil {
			t.Fatal("POST did not reach rule hook")
		}
		got := stub.captured.Serv
		if got.MCPProtocolVersionPresent != c.present {
			t.Fatalf("presence=%v want %v", got.MCPProtocolVersionPresent, c.present)
		}
		if c.version != nil && string(got.MCPProtocolVersion) != *c.version {
			t.Fatal("POST lost protocol version")
		}
	}
}
