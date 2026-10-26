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
	"reflect"
	"strings"
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
)

func listenerAuthTestRule(t ruleTuples, mode cmn.LBMode, want listenerAuth) *ruleEnt {
	r := &ruleEnt{tuples: t}
	r.act.action = &ruleLBActs{mode: mode}
	r.apiKeyAuth = want.apiKeyAuth
	r.jwtAuthProfile = want.jwtAuthProfile
	return r
}

// TestListenerAuthDiffKeepsUnsetAndDisabledApart: the two non-enforcing
// shapes are different listener values (one strips X-Api-Key, the other keeps
// proxying byte-identical), every enforcing mode differs from every other, and
// two JWT rules disagree on the profile alone.
func TestListenerAuthDiffKeepsUnsetAndDisabledApart(t *testing.T) {
	cases := []struct {
		name string
		a, b listenerAuth
		want []string
	}{
		{"both unset", listenerAuth{}, listenerAuth{}, nil},
		{"same jwt profile", listenerAuth{"jwt", "kc"}, listenerAuth{"jwt", "kc"}, nil},
		{"unset vs disabled", listenerAuth{}, listenerAuth{apiKeyAuth: cmn.ApiKeyAuthDisabled}, []string{"api_key_auth"}},
		{"unset vs required", listenerAuth{}, listenerAuth{apiKeyAuth: cmn.ApiKeyAuthRequired}, []string{"api_key_auth"}},
		{"unset vs jwt", listenerAuth{}, listenerAuth{"jwt", "kc"}, []string{"api_key_auth", "jwt_auth_profile"}},
		{"required vs jwt", listenerAuth{apiKeyAuth: cmn.ApiKeyAuthRequired}, listenerAuth{"jwt", "kc"}, []string{"api_key_auth", "jwt_auth_profile"}},
		{"jwt vs apikey-or-jwt, same profile", listenerAuth{"jwt", "kc"}, listenerAuth{"apikey-or-jwt", "kc"}, []string{"api_key_auth"}},
		{"jwt, different profile", listenerAuth{"jwt", "kc"}, listenerAuth{"jwt", "kc-other"}, []string{"jwt_auth_profile"}},
	}
	for _, c := range cases {
		if got := listenerAuthDiff(c.a, c.b); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: diff = %v, want %v", c.name, got, c.want)
		}
		if got := listenerAuthDiff(c.b, c.a); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s (reversed): diff = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestLbListenerAuthConflict mirrors the TLS listener check: only full-proxy
// rules on the same address, protocol and port count, a rule never conflicts
// with itself, and the refusal names the rule and the arguments but never a
// policy value or a profile name.
func TestLbListenerAuthConflict(t *testing.T) {
	jwt := listenerAuth{apiKeyAuth: cmn.ApiKeyAuthJWT, jwtAuthProfile: "kc"}
	unset := listenerAuth{}

	onListener := listenerAuthTestRule(listenerTestTuples("20.20.20.1", 2041, "a.example", "", ""), cmn.LBModeFullProxy, jwt)
	otherPort := listenerAuthTestRule(listenerTestTuples("20.20.20.1", 2042, "a.example", "", ""), cmn.LBModeFullProxy, unset)
	otherAddr := listenerAuthTestRule(listenerTestTuples("20.20.20.2", 2041, "a.example", "", ""), cmn.LBModeFullProxy, unset)
	notProxy := listenerAuthTestRule(listenerTestTuples("20.20.20.1", 2041, "", "", ""), cmn.LBModeDefault, unset)
	R := listenerTestTable(onListener, otherPort, otherAddr, notProxy)

	incoming := listenerTestTuples("20.20.20.1", 2041, "", "", "qwen3-32b")

	// The same policy shares the listener, whatever sits on other listeners
	// and whatever a rule that is not a proxy carries.
	if other, diff := R.lbListenerAuthConflict(nil, &incoming, jwt); other != nil {
		t.Fatalf("identical policy refused against %s: %v", listenerRuleName(&other.tuples), diff)
	}

	// A rule without a policy next to the jwt rule is the fail-open case: it
	// would be admitted under the listener's policy, or the jwt rule under
	// its lack of one, depending on which came first. Refused either way.
	other, diff := R.lbListenerAuthConflict(nil, &incoming, unset)
	if other != onListener {
		t.Fatalf("an unauthenticated rule was admitted onto a jwt listener (other=%v)", other)
	}
	if !reflect.DeepEqual(diff, []string{"api_key_auth", "jwt_auth_profile"}) {
		t.Errorf("diff = %v", diff)
	}
	msg := listenerAuthConflictError(other, diff).Error()
	for _, want := range []string{`host "a.example"`, "api_key_auth", "jwt_auth_profile", "same api_key_auth and jwt_auth_profile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %s", msg, want)
		}
	}
	for _, leak := range []string{`"kc"`, `"jwt"`} {
		if strings.Contains(msg, leak) {
			t.Errorf("refusal %q carries the value %s", msg, leak)
		}
	}

	// The same mode with another profile is a conflict too: one listener, one
	// verifier.
	if other, diff := R.lbListenerAuthConflict(nil, &incoming, listenerAuth{cmn.ApiKeyAuthJWT, "kc-other"}); other != onListener || !reflect.DeepEqual(diff, []string{"jwt_auth_profile"}) {
		t.Errorf("another profile: other=%v diff=%v", other, diff)
	}

	// A rule is never in conflict with itself: the only rule of a listener
	// can change its own policy (a replace of the lone rule).
	if other, _ := R.lbListenerAuthConflict(onListener, &onListener.tuples, unset); other != nil {
		t.Fatalf("a rule conflicts with itself")
	}

	// With a second rule on the listener, neither can move alone.
	second := listenerAuthTestRule(incoming, cmn.LBModeFullProxy, jwt)
	R.tables[RtLB].eMap[incoming.ruleKey()] = second
	if other, _ := R.lbListenerAuthConflict(onListener, &onListener.tuples, unset); other != second {
		t.Fatalf("a rule changed the credential policy of a listener it shares")
	}
}

// TestLbListenerAuthConflictIsStable: among several disagreeing rules the one
// with the smallest key is named, whatever the map order.
func TestLbListenerAuthConflictIsStable(t *testing.T) {
	unset := listenerAuth{}
	var rules []*ruleEnt
	for _, host := range []string{"d.example", "b.example", "c.example", "a.example"} {
		rules = append(rules, listenerAuthTestRule(listenerTestTuples("20.20.20.1", 2041, host, "", ""), cmn.LBModeFullProxy, unset))
	}
	R := listenerTestTable(rules...)
	incoming := listenerTestTuples("20.20.20.1", 2041, "e.example", "", "")
	for i := 0; i < 32; i++ {
		other, _ := R.lbListenerAuthConflict(nil, &incoming, listenerAuth{apiKeyAuth: cmn.ApiKeyAuthRequired})
		if other == nil || other.tuples.path != "a.example" {
			t.Fatalf("run %d named %v, want the rule with the smallest key", i, other)
		}
	}
}
