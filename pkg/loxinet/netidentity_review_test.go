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
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
)

func nidClosed(pm *cmn.NetIdentityMod) *cmn.NetIdentityMod {
	pm.FailMode = cmn.NetIdentityFailClosed
	return pm
}

// The most specific match wins, and scope is the last thing compared: a
// binding of every service on a /32 answers before a service's own /24.
func TestNetIdentityGlobalHostBeatsAServicePrefix(t *testing.T) {
	h, _ := newNidHolder(nil)
	mustAdd(t, h, nidCIDR("svc-24", "t-service", []string{"10.20.0.0/24"}, nidSvcA))
	mustAdd(t, h, nidCIDR("global-32", "t-global", []string{"10.20.0.7/32"}))

	if got := nidTenant(t, h, "10.20.0.7", 0, nidSvcA); got != "t-global" {
		t.Errorf("10.20.0.7 on the service resolved to %q, want the global /32's tenant", got)
	}
	if got := nidTenant(t, h, "10.20.0.8", 0, nidSvcA); got != "t-service" {
		t.Errorf("10.20.0.8 on the service resolved to %q, want the service's /24", got)
	}
	// Equal length: the service's own binding answers before the global one.
	mustAdd(t, h, nidCIDR("global-24", "t-global", []string{"10.30.0.0/24"}))
	mustAdd(t, h, nidCIDR("svc-24b", "t-service", []string{"10.30.0.0/24"}, nidSvcA))
	if got := nidTenant(t, h, "10.30.0.1", 0, nidSvcA); got != "t-service" {
		t.Errorf("equal prefixes: resolved to %q, want the service's own", got)
	}
	if got := nidTenant(t, h, "10.30.0.1", 0, nidSvcB); got != "t-global" {
		t.Errorf("another service: resolved to %q, want the global binding", got)
	}
}

// Two bindings of one tenant on the same match and scope are both stored
// and both listed, so the duplicate can be seen; another tenant is refused.
func TestNetIdentityDuplicateOfATenantIsListed(t *testing.T) {
	h, _ := newNidHolder(nil)
	mustAdd(t, h, nidCIDR("line-b", "tenant-a", []string{"10.20.0.0/24"}, nidSvcA))
	mustAdd(t, h, nidCIDR("line-a", "tenant-a", []string{"10.20.0.0/24"}, nidSvcA))
	if code, err := h.BindingAdd(nidCIDR("line-c", "tenant-other", []string{"10.20.0.0/24"}, nidSvcA)); err == nil || code != NetIdentityConflictErr {
		t.Fatalf("another tenant on the same match and scope: code=%d err=%v, want the conflict", code, err)
	}

	got, err := h.BindingGet()
	if err != nil || len(got) != 2 {
		t.Fatalf("GET returned %d bindings (%v), want both duplicates", len(got), err)
	}
	if got[0].Name != "line-a" || got[1].Name != "line-b" {
		t.Errorf("GET order = %s, %s; want name order", got[0].Name, got[1].Name)
	}
	for _, b := range got {
		if b.Tenant != "tenant-a" || len(b.Match.CIDR) != 1 || b.Match.CIDR[0] != "10.20.0.0/24" ||
			len(b.Services) != 1 || b.Services[0] != nidSvcA {
			t.Errorf("listed binding %+v does not show the shared match and scope", b)
		}
	}
	if ans, _ := netidAnswerFor(h, "10.20.0.1", 0, nidSvcA, ""); ans.binding != "line-a" {
		t.Errorf("answered by %q, want the first by name", ans.binding)
	}
}

// fail_mode is per service in effect: a service fails closed when any
// binding that applies to it says closed, and the reply to a binding says
// so when the bindings on a service disagree.
func TestNetIdentityFailModeDisagreementWarns(t *testing.T) {
	// Two bindings of one service.
	h, dp := newNidHolder(nil)
	neutral := nidCIDR("line-n", "t1", []string{"10.20.0.0/24"}, nidSvcA)
	closed := nidClosed(nidCIDR("line-c", "t2", []string{"10.21.0.0/24"}, nidSvcA))
	mustAdd(t, h, neutral)
	if w := h.BindingWarning(neutral); w != "" {
		t.Fatalf("one binding cannot disagree: %q", w)
	}
	mustAdd(t, h, closed)
	w := h.BindingWarning(closed)
	for _, want := range []string{"fail_mode", nidSvcA, "closed: line-c", "neutral: line-n", "per service"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q does not say %q", w, want)
		}
	}
	if _, isClosed := dp.has(nidSvcA); !isClosed {
		t.Error("the service does not fail closed although one of its bindings says closed")
	}
	// The other binding's own reply says the same: the state disagrees,
	// whichever binding was written last.
	if w := h.BindingWarning(neutral); !strings.Contains(w, "closed: line-c") {
		t.Errorf("the neutral binding's reply: %q", w)
	}
	// Made to agree: nothing to say.
	mustAdd(t, h, nidClosed(nidCIDR("line-n", "t1", []string{"10.20.0.0/24"}, nidSvcA)))
	if w := h.BindingWarning(closed); w != "" {
		t.Errorf("agreeing bindings still warn: %q", w)
	}

	// A binding of every service against a service's own.
	h, dp = newNidHolder(nil)
	scoped := nidCIDR("line-a", "t1", []string{"10.20.0.0/24"}, nidSvcA)
	global := nidClosed(nidCIDR("zone", "t9", []string{"10.99.0.0/16"}))
	mustAdd(t, h, scoped)
	mustAdd(t, h, global)
	w = h.BindingWarning(global)
	if !strings.Contains(w, nidSvcA) || !strings.Contains(w, "closed: zone") || !strings.Contains(w, "neutral: line-a") {
		t.Errorf("global closed against a neutral service binding: %q", w)
	}
	if w := h.BindingWarning(scoped); !strings.Contains(w, "closed: zone") {
		t.Errorf("the service binding's reply: %q", w)
	}
	if _, isClosed := dp.has(nidSvcA); !isClosed {
		t.Error("a closed binding of every service does not close a service with a neutral binding")
	}
	if _, isClosed := dp.has(nidSvcB); !isClosed {
		t.Error("a closed binding of every service does not close a service with no binding of its own")
	}

	// Two bindings of every service that disagree: named as such.
	h, _ = newNidHolder(nil)
	g1 := nidCIDR("zone-1", "t1", []string{"10.1.0.0/16"})
	g2 := nidClosed(nidCIDR("zone-2", "t2", []string{"10.2.0.0/16"}))
	mustAdd(t, h, g1)
	mustAdd(t, h, g2)
	if w := h.BindingWarning(g2); !strings.Contains(w, "every AI service") || !strings.Contains(w, "closed: zone-2") {
		t.Errorf("two global bindings: %q", w)
	}

	// Bindings of different services never disagree with each other, and
	// several services in disagreement are counted.
	h, _ = newNidHolder(nil)
	mustAdd(t, h, nidCIDR("a-n", "t1", []string{"10.20.0.0/24"}, nidSvcA))
	onB := nidClosed(nidCIDR("b-c", "t2", []string{"10.21.0.0/24"}, nidSvcB))
	mustAdd(t, h, onB)
	if w := h.BindingWarning(onB); w != "" {
		t.Errorf("bindings of two services: %q", w)
	}
	both := nidClosed(nidCIDR("ab-c", "t3", []string{"10.22.0.0/24"}, nidSvcA, nidSvcB))
	mustAdd(t, h, both)
	if w := h.BindingWarning(both); !strings.Contains(w, nidSvcA) || strings.Contains(w, "more)") {
		t.Errorf("only one of its two services disagrees: %q", w)
	}
	mustAdd(t, h, nidCIDR("b-n", "t4", []string{"10.23.0.0/24"}, nidSvcB))
	if w := h.BindingWarning(both); !strings.Contains(w, "(and on 1 more)") {
		t.Errorf("both of its services disagree: %q", w)
	}
}

// The two warnings a binding can earn ride one reply, each after its own
// marker.
func TestNetIdentityWarningsAreJoined(t *testing.T) {
	h, _ := newNidHolder(nidIfs{})
	mustAdd(t, h, nidCIDR("line-n", "t1", []string{"10.20.0.0/24"}, nidSvcA))
	pm := nidClosed(nidIf("zone", "zone", "vlan300", nidSvcA))
	mustAdd(t, h, pm)
	w := h.BindingWarning(pm)
	parts := strings.Split(w, "; warning: ")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "match.ingress_if") || !strings.HasPrefix(parts[1], "fail_mode") {
		t.Errorf("warning = %q, want the interface text and the fail_mode text, each after its marker", w)
	}
	if h.BindingWarning(nil) != "" {
		t.Error("a nil binding earned a warning")
	}
}
