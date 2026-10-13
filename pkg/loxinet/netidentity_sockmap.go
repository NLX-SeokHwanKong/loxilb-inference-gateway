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

	cmn "github.com/loxilb-io/loxilb/common"
)

// netidSockMapErr refuses a binding on a service that could never honour
// it. A rule that is not an AI-gateway service and whose request direction
// the kernel carries (sockMapMode request or both) hands the gateway only
// the first request of a connection: its requests do not reach the admission
// gate, and the later ones of a keep-alive connection do not pass the
// upstream-header strip. Two things a binding promises would then read as
// protection and do nothing:
//
//   - credential: network, which admits by the network in place of a key;
//   - forward_identity, which makes the service drop every X-Auth-* header a
//     client sends.
//
// Such a binding is refused when it names that service, and a
// forward_identity binding of every service is refused while any such rule
// exists. An AI-gateway service keeps its request direction on the relay
// whatever its sockMapMode, and is not refused. A service that has no rule
// yet is not refused either: the binding is stored, as any binding of a
// service that does not exist is.
//
// Called with the configuration lock held, before the binding is stored and
// so before any ingress interface is read for it.
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
	if len(want) == 0 && !pm.ForwardIdentity {
		// credential: network of every AI service: a service that is not
		// one never asks the resolver, so there is nothing to refuse.
		return nil
	}
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
		if svc == "" || (len(want) != 0 && !want[svc]) {
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
	if len(want) == 0 {
		field, what = "forward_identity", "forward_identity on every service"
	}
	return cmn.NewValidationError(field,
		"%s is refused on service %s: its rule is not an AI-gateway service and runs sockMapMode %s, so the gateway does not see every request on it",
		what, hit, cmn.SockMapCodeToMode(mode))
}
