/*
 * Copyright (c) 2026 LoxiLB Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package loxinet

import (
	"strings"
	"sync"

	cmn "github.com/loxilb-io/loxilb/common"
)

// A rule that is not an AI-gateway service and whose request direction the
// kernel carries (sockMapMode request or both) hands the gateway only the
// first request of a connection: its requests do not reach the admission
// gate, and the later ones of a keep-alive connection do not pass the
// upstream-header strip. Two things a binding promises would then read as
// protection and do nothing:
//
//   - credential: network, which admits by the network in place of a key;
//   - forward_identity, which makes the service drop every X-Auth-* header a
//     client sends.
//
// The pair is refused from both sides: the binding that names such a
// service (netidSockMapErr), and the rule that would become such a service
// while a binding names it (netidRuleSockMapErr). An AI-gateway service
// keeps its request direction on the relay whatever its sockMapMode, and is
// not refused. A service that has no rule yet is not refused either: the
// binding is stored, as any binding of a service that does not exist is.
//
// Both run under the configuration mutex, and the binding is stored before
// it is released, so of a rule change and a binding change that would make
// the pair, the later one is refused. The binding holder's lock is taken
// inside the configuration mutex and never the other way round.

// netidSockMapErr refuses a binding on a service that could never honour
// it. Called with the configuration mutex held, before the binding is
// stored and so before any ingress interface is read for it.
func (R *RuleH) netidSockMapErr(pm *cmn.NetIdentityMod) error {
	if pm == nil {
		return nil
	}
	credNetwork := strings.ToLower(strings.TrimSpace(pm.Credential)) == cmn.NetIdentityCredentialNetwork
	if !credNetwork && !pm.ForwardIdentity {
		return nil
	}
	field, what := "credential", "credential: network"
	if !credNetwork {
		field, what = "forward_identity", "forward_identity"
	}
	want := make(map[string]bool, len(pm.Services))
	for _, s := range pm.Services {
		svc, err := netidService(s)
		if err != nil {
			return nil // the binding's own validation names the field
		}
		want[svc] = true
	}
	// A binding that names no service refuses nothing here. With
	// credential: network it is one of every AI service, and a service that
	// is not one never asks the resolver; with forward_identity it is
	// refused by the binding's own validation.
	var hit string
	var mode uint8
	for _, data := range R.tables[RtLB].eMap {
		if data.aiGwMode() {
			continue
		}
		if req, _ := sockMapDirs(data.sockMapMode); !req {
			continue
		}
		svc := aiServiceIdent(data.tuples.l3Dst.addr.IP, data.tuples.l4Dst.valMin)
		if svc == "" || !want[svc] {
			continue
		}
		// The first in service order, so the same request is refused with
		// the same text every time.
		if hit == "" || svc < hit {
			hit, mode = svc, data.sockMapMode
		}
	}
	if hit == "" {
		return nil
	}
	return cmn.NewValidationError(field,
		"%s is refused on service %s: its rule is not an AI-gateway service and runs sockMapMode %s, so the gateway does not see every request on it",
		what, hit, cmn.SockMapCodeToMode(mode))
}

// netidRuleSockMapErr refuses the rule that would carry the request
// direction of a service in the kernel while a binding that forwards
// identity, or admits by the network, names that service. aiGw and
// sockMapCode are the values the rule will end up with. Called with the
// configuration mutex held.
func netidRuleSockMapErr(h *NetIdentityH, svc string, aiGw bool, sockMapCode uint8) error {
	if h == nil || svc == "" || aiGw {
		return nil
	}
	if req, _ := sockMapDirs(sockMapCode); !req {
		return nil
	}
	name, what := h.sockMapBlocker(svc)
	if name == "" {
		return nil
	}
	return cmn.NewValidationError("sockMapMode",
		"sockMapMode %s is refused on service %s: network-identity binding %s has %s on it, and this rule is not an AI-gateway service, so the gateway would not see every request",
		cmn.SockMapCodeToMode(sockMapCode), svc, name, what)
}

// netidAddUnderConfig stores a binding with the configuration mutex held
// from the question about the rules to the store, so that a rule change
// cannot land between the two.
func netidAddUnderConfig(cfg *sync.RWMutex, R *RuleH, h *NetIdentityH, pm *cmn.NetIdentityMod) (int, error) {
	cfg.RLock()
	defer cfg.RUnlock()
	if R != nil {
		if err := R.netidSockMapErr(pm); err != nil {
			return NetIdentityArgErr, err
		}
	}
	return h.BindingAdd(pm)
}
