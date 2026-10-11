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
	"fmt"
	"strings"
)

// MCPStatelessProtocolVersion is the opt-in transport contract implemented by the proxy.
const MCPStatelessProtocolVersion = "2026-07-28"

// ValidateMCPService refuses settings that would bypass validation or introduce affinity.
// All entry points, including snapshot replay, use this before changing rule state.
func ValidateMCPService(serv *LbServiceArg) error {
	if serv.MCPProtocolVersion == "" {
		return nil
	}
	if serv.MCPProtocolVersion != MCPStatelessProtocolVersion {
		return fmt.Errorf("mcp_protocol_version must be empty or %s", MCPStatelessProtocolVersion)
	}
	if serv.Mode != LBModeFullProxy || serv.Proto != "tcp" {
		return fmt.Errorf("mcp_protocol_version requires mode=fullproxy (4) and protocol=tcp")
	}
	if serv.BackendProtocol != "" && serv.BackendProtocol != "http1" {
		return fmt.Errorf("mcp_protocol_version requires backend_protocol=http1 (or omitted); frontend HTTP/2 uses the HTTP/1.1 backend bridge")
	}
	if serv.Egress {
		return fmt.Errorf("mcp_protocol_version does not support egress=true")
	}
	if serv.Sel != LbSelRr && serv.Sel != LbSelPrio {
		return fmt.Errorf("mcp_protocol_version requires round-robin (sel=0) or weighted round-robin (sel=2)")
	}
	if serv.SessionHeaderName != "" {
		return fmt.Errorf("mcp_protocol_version forbids session_header_name (stateless transport has no session affinity)")
	}
	if serv.PDDisaggMode || serv.PDCacheAwareMode || serv.KvExactMode != 0 || serv.ModelName != "" || serv.KvModelProfile != "" || serv.KvExactApiMode != "" {
		return fmt.Errorf("mcp_protocol_version is incompatible with model routing, P/D disaggregation and KV routing")
	}
	if serv.SockMapMode != "" && serv.SockMapMode != SockMapModeOff {
		return fmt.Errorf("mcp_protocol_version requires sockMapMode=off (every request must be validated)")
	}
	return nil
}

// ValidateMCPL7Policy prevents a route from changing the validated transport
// metadata or introducing session affinity after the request passes admission.
func ValidateMCPL7Policy(routes []L7RuleArg) error {
	if len(routes) > 256 {
		return fmt.Errorf("mcp_protocol_version permits at most 256 L7 routes")
	}
	for i, route := range routes {
		for _, set := range route.MatchSets {
			for _, condition := range set.Conditions {
				if len(condition.Key) >= 64 || len(condition.Value) >= 256 || strings.ContainsRune(condition.Key, 0) || strings.ContainsRune(condition.Value, 0) {
					return fmt.Errorf("mcp_protocol_version L7 rule[%d] condition cannot fit routing fields", i)
				}
			}
		}
		if route.SessionPersistence != "" {
			return fmt.Errorf("mcp_protocol_version forbids L7 rule[%d] sessionPersistence", i)
		}
		for _, filter := range route.InsertHeaders {
			name := strings.ToLower(strings.TrimSpace(filter.Name))
			if strings.HasPrefix(name, "mcp-") || name == "accept" || name == "content-type" || name == "content-length" || name == "transfer-encoding" {
				return fmt.Errorf("mcp_protocol_version forbids L7 rule[%d] modification of validated header %q", i, filter.Name)
			}
		}
	}
	return nil
}

// MCPVersion is a scalar transport declaration with strict JSON-null handling.
// A field-local decoder preserves the outer snapshot decoder's unknown-field checks.
type MCPVersion string

func (version *MCPVersion) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("mcp_protocol_version must not be null")
	}
	var decoded string
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*version = MCPVersion(decoded)
	return nil
}
