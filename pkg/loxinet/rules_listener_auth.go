/*
 * Copyright (c) 2026 NetLOX Inc
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
	"fmt"
	"strings"

	cmn "github.com/loxilb-io/loxilb/common"
)

// listenerAuth is the credential policy of a full-proxy rule as the data plane
// applies it: once per listener, not once per rule. The admission gate runs
// before a request is matched to a rule by host, path or model, so every rule
// on one address, port and protocol is admitted under the policy of whichever
// rule created the listener. A rule that declared "jwt" next to a rule that
// declared nothing would be served without authentication - a fail-open that
// a warning in the reply does not prevent - so the disagreement is refused
// like the listener's TLS settings are (rules_listener_tls.go).
//
// apiKeyAuth is the declaration AS STORED: "" (unset) and an explicit
// "disabled" are kept apart on purpose. Neither enforces, but "disabled" makes
// the gateway own and strip X-Api-Key while unset keeps proxying
// byte-identical (apiKeyAuthWireValue), and a listener has one of the two.
type listenerAuth struct {
	apiKeyAuth     string
	jwtAuthProfile string
}

// listenerAuthOfRule reads the policy a rule already on the listener carries.
func listenerAuthOfRule(r *ruleEnt) listenerAuth {
	return listenerAuth{apiKeyAuth: r.apiKeyAuth, jwtAuthProfile: r.jwtAuthProfile}
}

// listenerAuthDiff names the arguments in which two rules of one listener
// disagree, in a fixed order. Empty: the two can share the listener.
func listenerAuthDiff(a, b listenerAuth) []string {
	var diff []string
	if a.apiKeyAuth != b.apiKeyAuth {
		diff = append(diff, "api_key_auth")
	}
	if a.jwtAuthProfile != b.jwtAuthProfile {
		diff = append(diff, "jwt_auth_profile")
	}
	return diff
}

// lbListenerAuthConflict looks for a full-proxy rule on the listener of rt,
// other than self, whose credential policy differs from want. It returns that
// rule and the arguments that differ, or nil. Among several it returns the one
// with the smallest key, so the answer does not depend on map order. The
// listener is the address, protocol and port of rt, as for lbListenerTLSConflict.
func (R *RuleH) lbListenerAuthConflict(self *ruleEnt, rt *ruleTuples, want listenerAuth) (*ruleEnt, []string) {
	var found *ruleEnt
	var foundKey string
	var foundDiff []string
	for key, r := range R.tables[RtLB].eMap {
		if r == self {
			continue
		}
		at, ok := r.act.action.(*ruleLBActs)
		if !ok || at.mode != cmn.LBModeFullProxy {
			continue
		}
		if !r.tuples.l3Dst.addr.IP.Equal(rt.l3Dst.addr.IP) ||
			r.tuples.l4Prot.val != rt.l4Prot.val ||
			r.tuples.l4Dst.valMin != rt.l4Dst.valMin {
			continue
		}
		diff := listenerAuthDiff(want, listenerAuthOfRule(r))
		if len(diff) == 0 {
			continue
		}
		if found == nil || key < foundKey {
			found, foundKey, foundDiff = r, key, diff
		}
	}
	return found, foundDiff
}

// listenerAuthConflictError is the refusal of a rule that cannot share its
// listener. It names the rule already there and the arguments, never a
// profile name or a policy value.
func listenerAuthConflictError(other *ruleEnt, diff []string) error {
	return fmt.Errorf("%s: %s is already on this address, port and protocol with a different value; "+
		"rules that share a listener share its credential policy, so they must declare the same api_key_auth and jwt_auth_profile",
		strings.Join(diff, ", "), listenerRuleName(&other.tuples))
}
