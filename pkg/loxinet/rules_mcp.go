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
	"fmt"
	cmn "github.com/loxilb-io/loxilb/common"
)

// resolveMCPService preserves omission on replace, and validates the resulting shape.
// Changing transport semantics on an already-open connection is not guaranteed;
// force an operator-visible delete/recreate instead of silently retaining stale policy.
func resolveMCPService(serv *cmn.LbServiceArg, existing *ruleEnt) error {
	if existing != nil {
		if !serv.MCPProtocolVersionPresent && serv.MCPProtocolVersion == "" {
			serv.MCPProtocolVersion = cmn.MCPVersion(existing.mcpProtocolVersion)
		}
		if string(serv.MCPProtocolVersion) != existing.mcpProtocolVersion {
			return fmt.Errorf("mcp_protocol_version is immutable; delete and recreate the rule to change transport mode")
		}
	}
	return cmn.ValidateMCPService(serv)
}

// A listener validates before host/path selection, so every alias must declare
// the same transport. Check overlapping port ranges as well as single ports.
func (R *RuleH) lbListenerMCPConflict(self *ruleEnt, rt *ruleTuples, want string) *ruleEnt {
	var found *ruleEnt
	var foundKey string
	for key, r := range R.tables[RtLB].eMap {
		if r == self {
			continue
		}
		if !r.tuples.l3Dst.addr.IP.Equal(rt.l3Dst.addr.IP) || r.tuples.l4Prot.val != rt.l4Prot.val {
			continue
		}
		if r.tuples.l4Dst.valMax < rt.l4Dst.valMin || rt.l4Dst.valMax < r.tuples.l4Dst.valMin {
			continue
		}
		if r.mcpProtocolVersion != want && (found == nil || key < foundKey) {
			found, foundKey = r, key
		}
	}
	return found
}
