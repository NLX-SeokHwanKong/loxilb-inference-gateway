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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	cmn "github.com/loxilb-io/loxilb/common"
)

const (
	nidSvcA = "20.20.20.1:8080"
	nidSvcB = "20.20.20.2:8080"
	nidSvcC = "20.20.20.3:8443"
)

// nidDP stands in for the data plane's bound set: it holds what it was last
// told, and answers as a request on a service would read it.
type nidDP struct {
	mu     sync.Mutex
	bound  netidBound
	writes int
}

func (d *nidDP) publish(nb netidBound) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bound = nb
	d.writes++
}

func (d *nidDP) has(svc string) (bound, closed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bound.svcs == nil && !d.bound.all {
		return false, false
	}
	return d.bound.has(svc)
}

// nidIfs is the node's interface table for a test.
type nidIfs map[string]int

func (m nidIfs) index(name string) (int, bool) {
	idx, ok := m[name]
	return idx, ok
}

func newNidHolder(ifs nidIfs) (*NetIdentityH, *nidDP) {
	dp := &nidDP{}
	return newNetIdentityH(dp.publish, ifs.index), dp
}

func nidCIDR(name, tenant string, cidrs []string, svcs ...string) *cmn.NetIdentityMod {
	return &cmn.NetIdentityMod{Name: name, Tenant: tenant, Match: cmn.NetIdentityMatch{CIDR: cidrs}, Services: svcs}
}

func nidIf(name, tenant, ifName string, svcs ...string) *cmn.NetIdentityMod {
	return &cmn.NetIdentityMod{Name: name, Tenant: tenant, Match: cmn.NetIdentityMatch{IngressIf: ifName}, Services: svcs}
}

func mustAdd(t *testing.T, h *NetIdentityH, pm *cmn.NetIdentityMod) {
	t.Helper()
	if _, err := h.BindingAdd(pm); err != nil {
		t.Fatalf("add %s: %v", pm.Name, err)
	}
}

// nidTenant is the tenant the resolver answers for a source, "" for none.
func nidTenant(t *testing.T, h *NetIdentityH, ip string, ifindex int, svc string) string {
	t.Helper()
	ans, rc := netidAnswerFor(h, ip, ifindex, svc, "")
	if rc != 0 {
		t.Fatalf("resolver could not answer for %s on %s", ip, svc)
	}
	if !ans.matched {
		return ""
	}
	return ans.tenant
}

func nidFaults(t *testing.T, svc string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "loxilb_ai_netid_resolver_faults_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "service" && lp.GetValue() == svc {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// The longest prefix that holds the peer decides, whatever order the
// bindings were stored in.
func TestNetIdentityLongestPrefixWins(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}} {
		h, _ := newNidHolder(nil)
		all := []*cmn.NetIdentityMod{
			nidCIDR("any", "t-any", []string{"0.0.0.0/0"}, nidSvcA),
			nidCIDR("site", "t-site", []string{"10.20.0.0/16"}, nidSvcA),
			nidCIDR("line", "t-line", []string{"10.20.4.0/24", "10.20.9.0/24"}, nidSvcA),
			nidCIDR("host", "t-host", []string{"10.20.4.7/32"}, nidSvcA),
		}
		for _, i := range order {
			mustAdd(t, h, all[i])
		}
		for ip, want := range map[string]string{
			"10.20.4.7":        "t-host",
			"10.20.4.8":        "t-line",
			"10.20.9.255":      "t-line",
			"10.20.5.1":        "t-site",
			"10.21.0.1":        "t-any",
			"255.255.255.255":  "t-any",
			"::ffff:10.20.4.8": "t-line", // an IPv4 peer seen through a dual-stack socket
		} {
			if got := nidTenant(t, h, ip, 0, nidSvcA); got != want {
				t.Errorf("order %v: %s resolved to %q, want %q", order, ip, got, want)
			}
		}
		// Another service has none of them.
		if got := nidTenant(t, h, "10.20.4.7", 0, nidSvcB); got != "" {
			t.Errorf("a service with no binding resolved %q", got)
		}
	}
}

// What cannot be matched matches nothing, and is an answer, not a fault.
func TestNetIdentityUnmatchableSource(t *testing.T) {
	h, _ := newNidHolder(nil)
	mustAdd(t, h, nidCIDR("site", "t-site", []string{"10.20.0.0/16"}))
	before := nidFaults(t, nidSvcA)
	for _, ip := range []string{"", "not-an-address", "2001:db8::1", "10.20", "10.20.0.1.1", "10.20.0.1/32", "010.020.000.001"} {
		if got := nidTenant(t, h, ip, 0, nidSvcA); got != "" {
			t.Errorf("peer %q resolved to %q", ip, got)
		}
	}
	if got := nidFaults(t, nidSvcA); got != before {
		t.Errorf("an unmatchable peer counted %v faults", got-before)
	}
}

// Precedence, in the order the resource states it: an interface match over
// any CIDR; the longest prefix among CIDRs; a binding of the service over
// one of every service where the two match as specifically.
func TestNetIdentityPrecedence(t *testing.T) {
	h, _ := newNidHolder(nidIfs{"vlan100": 7, "vlan200": 9})
	mustAdd(t, h, nidCIDR("g16", "g16", []string{"10.20.0.0/16"}))
	mustAdd(t, h, nidCIDR("g24", "g24", []string{"10.20.4.0/24"}))
	mustAdd(t, h, nidCIDR("s16", "s16", []string{"10.20.0.0/16"}, nidSvcA))
	mustAdd(t, h, nidIf("g-if", "g-if", "vlan100"))
	mustAdd(t, h, nidIf("s-if", "s-if", "vlan200", nidSvcA))
	mustAdd(t, h, nidIf("s-if-100", "s-if-100", "vlan100", nidSvcB))

	cases := []struct {
		name    string
		ip      string
		ifindex int
		svc     string
		want    string
	}{
		{"equal prefix: the service's binding", "10.20.9.1", 0, nidSvcA, "s16"},
		{"a longer prefix of every service beats the service's shorter one", "10.20.4.1", 0, nidSvcA, "g24"},
		{"a service with no binding of its own reads the global ones", "10.20.9.1", 0, nidSvcB, "g16"},
		{"an interface beats the longest prefix", "10.20.4.1", 9, nidSvcA, "s-if"},
		{"a global interface beats the service's prefix", "10.20.9.1", 7, nidSvcA, "g-if"},
		{"same interface: the service's binding", "10.20.9.1", 7, nidSvcB, "s-if-100"},
		{"an interface with no binding falls to the address", "10.20.9.1", 4, nidSvcA, "s16"},
		{"an interface not read (0) never matches one", "192.0.2.1", 0, nidSvcA, ""},
		{"an interface matches whatever the address", "2001:db8::1", 9, nidSvcA, "s-if"},
		{"a service's interface binding is not another service's", "192.0.2.1", 9, nidSvcB, ""},
	}
	for _, c := range cases {
		if got := nidTenant(t, h, c.ip, c.ifindex, c.svc); got != c.want {
			t.Errorf("%s: %s if=%d on %s resolved to %q, want %q", c.name, c.ip, c.ifindex, c.svc, got, c.want)
		}
	}
}

// An interface that does not exist contributes nothing, says so, and starts
// matching once it exists and the bindings are compiled again.
func TestNetIdentityMissingInterface(t *testing.T) {
	ifs := nidIfs{}
	h, dp := newNidHolder(ifs)
	pm := nidIf("zone", "zone", "vlan300", nidSvcA)
	mustAdd(t, h, pm)
	if w := h.BindingWarning(pm); !strings.Contains(w, "vlan300") || !strings.Contains(w, "ingress_if") {
		t.Fatalf("warning for a missing interface: %q", w)
	}
	if bound, _ := dp.has(nidSvcA); !bound {
		t.Fatal("the service is not bound although a binding names it")
	}
	if got := nidTenant(t, h, "192.0.2.1", 12, nidSvcA); got != "" {
		t.Fatalf("a binding of a missing interface matched: %q", got)
	}
	ifs["vlan300"] = 12
	if w := h.BindingWarning(nidIf("zone", "zone", " vlan300 ", nidSvcA)); w != "" {
		t.Fatalf("warning for an interface that exists, as the request spelled it: %q", w)
	}
	mustAdd(t, h, pm)
	if got := nidTenant(t, h, "192.0.2.1", 12, nidSvcA); got != "zone" {
		t.Fatalf("after the interface appeared: %q", got)
	}
	if w := h.BindingWarning(nidCIDR("c", "c", []string{"10.0.0.0/8"})); w != "" {
		t.Fatalf("warning for a CIDR binding: %q", w)
	}
}

// The answer carries the binding as the gate needs it.
func TestNetIdentityAnswerFields(t *testing.T) {
	h, _ := newNidHolder(nidIfs{"ipsec0": 3})
	mustAdd(t, h, &cmn.NetIdentityMod{
		Name: "line-1", Tenant: "tenant-line-1",
		Match:         cmn.NetIdentityMatch{CIDR: []string{"10.20.0.0/24"}},
		Services:      []string{nidSvcA},
		AllowedModels: []string{"llama-3.1-8b", "qwen3"}, Enforce: "strict", ForwardIdentity: true,
	})
	mustAdd(t, h, &cmn.NetIdentityMod{
		Name: "tunnel", Tenant: "tenant-tunnel",
		Match: cmn.NetIdentityMatch{IngressIf: "ipsec0"}, Credential: "network",
	})

	got, rc := netidAnswerFor(h, "10.20.0.9", 0, nidSvcA, "qwen3")
	want := netidAnswer{matched: true, strict: true, modelOK: true, forward: true, tenant: "tenant-line-1", binding: "line-1"}
	if rc != 0 || got != want {
		t.Fatalf("strict binding answered %+v rc %d, want %+v", got, rc, want)
	}
	// The allowlist is exact: another model, a prefix of one, another case
	// and no model at all are outside it.
	for _, model := range []string{"gpt-4", "qwen", "QWEN3", "qwen3 ", ""} {
		if got, _ := netidAnswerFor(h, "10.20.0.9", 0, nidSvcA, model); !got.matched || got.modelOK {
			t.Errorf("model %q: %+v, want a match that refuses the model", model, got)
		}
	}
	got, rc = netidAnswerFor(h, "192.0.2.1", 3, nidSvcA, "anything")
	want = netidAnswer{matched: true, credNetwork: true, modelOK: true, tenant: "tenant-tunnel", binding: "tunnel"}
	if rc != 0 || got != want {
		t.Fatalf("credential binding answered %+v rc %d, want %+v", got, rc, want)
	}
	if got, rc := netidAnswerFor(h, "192.0.2.1", 0, nidSvcA, ""); rc != 0 || got != (netidAnswer{}) {
		t.Fatalf("no binding answered %+v rc %d", got, rc)
	}
}

// Two bindings that match the same source on the same service as
// specifically may not name different tenants.
func TestNetIdentityConflictIs409(t *testing.T) {
	h, dp := newNidHolder(nidIfs{"vlan100": 7})
	mustAdd(t, h, nidCIDR("a", "tenant-a", []string{"10.20.0.0/24", "10.30.0.0/16"}, nidSvcA, nidSvcB))
	mustAdd(t, h, nidCIDR("g", "tenant-g", []string{"10.40.0.0/16"}))
	mustAdd(t, h, nidIf("i", "tenant-i", "vlan100", nidSvcA))
	writes := dp.writes

	refused := []struct {
		name string
		pm   *cmn.NetIdentityMod
		want []string
	}{
		{"same prefix, same service", nidCIDR("x", "tenant-x", []string{"10.20.0.0/24"}, nidSvcA),
			[]string{"binding a", "10.20.0.0/24", nidSvcA, "tenant-a"}},
		{"one prefix and one service of several", nidCIDR("x", "tenant-x", []string{"192.0.2.0/24", "10.30.0.0/16"}, nidSvcC, nidSvcB),
			[]string{"binding a", "10.30.0.0/16", nidSvcB}},
		{"same prefix, every service", nidCIDR("x", "tenant-x", []string{"10.40.0.0/16"}),
			[]string{"binding g", "every service", "tenant-g"}},
		{"same interface, same service", nidIf("x", "tenant-x", "vlan100", nidSvcA),
			[]string{"binding i", "ingress_if vlan100", nidSvcA}},
	}
	for _, c := range refused {
		code, err := h.BindingAdd(c.pm)
		var conflict *cmn.ConflictError
		if !errors.As(err, &conflict) || code != NetIdentityConflictErr {
			t.Errorf("%s: code %d err %v, want a conflict", c.name, code, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q does not name %q", c.name, err, w)
			}
		}
	}
	if dp.writes != writes {
		t.Errorf("a refused binding reached the data plane (%d writes)", dp.writes-writes)
	}
	if got, _ := h.BindingGet(); len(got) != 3 {
		t.Errorf("a refused binding was stored: %d bindings", len(got))
	}

	accepted := []struct {
		name string
		pm   *cmn.NetIdentityMod
	}{
		{"same prefix and service, same tenant", nidCIDR("a2", "tenant-a", []string{"10.20.0.0/24"}, nidSvcA)},
		{"same prefix, another service", nidCIDR("b", "tenant-b", []string{"10.20.0.0/24"}, nidSvcC)},
		{"same prefix, every service: less specific than the service's", nidCIDR("b2", "tenant-b", []string{"10.20.0.0/24"})},
		{"a longer prefix inside it", nidCIDR("b3", "tenant-b", []string{"10.20.0.0/25"}, nidSvcA)},
		{"a shorter prefix around it", nidCIDR("b4", "tenant-b", []string{"10.20.0.0/16"}, nidSvcA)},
		{"the same interface on another service", nidIf("i2", "tenant-b", "vlan100", nidSvcB)},
		{"an interface and a prefix on one service", nidIf("i3", "tenant-b", "vlan200", nidSvcA)},
		{"a replace of itself with another tenant", nidCIDR("a", "tenant-z", []string{"10.30.0.0/16"}, nidSvcB)},
	}
	for _, c := range accepted {
		if _, err := h.BindingAdd(c.pm); err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
	}
	// The replace moved a off 10.20.0.0/24: what it no longer claims is free.
	mustAdd(t, h, nidCIDR("free", "tenant-free", []string{"10.30.0.0/16"}, nidSvcA))
	// ... and a2 still holds 10.20.0.0/24 on the service for tenant-a.
	if _, err := h.BindingAdd(nidCIDR("x", "tenant-x", []string{"10.20.0.0/24"}, nidSvcA)); err == nil {
		t.Error("a second tenant on 10.20.0.0/24 was accepted once the first binding was replaced")
	}
}

// Two bindings of one tenant may claim the same source; the one first by
// name answers, whatever order they were stored in.
func TestNetIdentityTieResolvesByName(t *testing.T) {
	for _, order := range [][2]string{{"b-second", "a-first"}, {"a-first", "b-second"}} {
		h, _ := newNidHolder(nil)
		for _, name := range order {
			pm := nidCIDR(name, "tenant-a", []string{"10.20.0.0/24"}, nidSvcA)
			if name == "a-first" {
				pm.Enforce = "strict"
			}
			mustAdd(t, h, pm)
		}
		got, _ := netidAnswerFor(h, "10.20.0.1", 0, nidSvcA, "")
		if got.binding != "a-first" || !got.strict {
			t.Errorf("stored as %v: answered by %q (strict %v), want a-first", order, got.binding, got.strict)
		}
	}
}

func TestNetIdentityValidation(t *testing.T) {
	base := func() cmn.NetIdentityMod {
		return cmn.NetIdentityMod{Name: "line-1", Tenant: "t1", Match: cmn.NetIdentityMatch{CIDR: []string{"10.20.0.0/24"}}}
	}
	long := strings.Repeat("x", 300)
	many := func(n int, f func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = f(i)
		}
		return out
	}
	bad := []struct {
		name  string
		field string
		mut   func(*cmn.NetIdentityMod)
	}{
		{"no name", "name", func(p *cmn.NetIdentityMod) { p.Name = "" }},
		{"name too long", "name", func(p *cmn.NetIdentityMod) { p.Name = long[:64] }},
		{"name with a space", "name", func(p *cmn.NetIdentityMod) { p.Name = "line 1" }},
		{"name with a slash", "name", func(p *cmn.NetIdentityMod) { p.Name = "a/b" }},
		{"name with a newline", "name", func(p *cmn.NetIdentityMod) { p.Name = "a\nb" }},
		{"name starting with a dot", "name", func(p *cmn.NetIdentityMod) { p.Name = ".a" }},
		{"no match", "match", func(p *cmn.NetIdentityMod) { p.Match = cmn.NetIdentityMatch{} }},
		{"both matches", "match", func(p *cmn.NetIdentityMod) { p.Match.IngressIf = "eth0" }},
		{"an IPv6 network", "match.cidr", func(p *cmn.NetIdentityMod) { p.Match.CIDR = []string{"2001:db8::/32"} }},
		{"an address with no length", "match.cidr", func(p *cmn.NetIdentityMod) { p.Match.CIDR = []string{"10.20.0.0"} }},
		{"host bits set", "match.cidr", func(p *cmn.NetIdentityMod) { p.Match.CIDR = []string{"10.20.0.5/24"} }},
		{"a length past 32", "match.cidr", func(p *cmn.NetIdentityMod) { p.Match.CIDR = []string{"10.20.0.0/33"} }},
		{"an empty network", "match.cidr", func(p *cmn.NetIdentityMod) { p.Match.CIDR = []string{"10.20.0.0/24", ""} }},
		{"too many networks", "match.cidr", func(p *cmn.NetIdentityMod) {
			p.Match.CIDR = many(netidMaxCIDRs+1, func(i int) string { return fmt.Sprintf("10.%d.%d.0/24", i/256, i%256) })
		}},
		{"interface name too long", "match.ingress_if", func(p *cmn.NetIdentityMod) {
			p.Match = cmn.NetIdentityMatch{IngressIf: "abcdefghijklmnop"}
		}},
		{"interface name with a slash", "match.ingress_if", func(p *cmn.NetIdentityMod) {
			p.Match = cmn.NetIdentityMatch{IngressIf: "eth0/1"}
		}},
		{"no tenant", "tenant", func(p *cmn.NetIdentityMod) { p.Tenant = "" }},
		{"tenant too long", "tenant", func(p *cmn.NetIdentityMod) { p.Tenant = long[:128] }},
		{"tenant with the bucket-key delimiter", "tenant", func(p *cmn.NetIdentityMod) { p.Tenant = "a|b" }},
		{"tenant with a control character", "tenant", func(p *cmn.NetIdentityMod) { p.Tenant = "a\x00b" }},
		{"unknown enforce", "enforce", func(p *cmn.NetIdentityMod) { p.Enforce = "always" }},
		{"unknown credential", "credential", func(p *cmn.NetIdentityMod) { p.Credential = "mtls" }},
		{"credential network on an address range", "credential", func(p *cmn.NetIdentityMod) { p.Credential = "network" }},
		{"unknown priority class", "priority_class", func(p *cmn.NetIdentityMod) { p.PriorityClass = "urgent" }},
		{"unknown fail mode", "fail_mode", func(p *cmn.NetIdentityMod) { p.FailMode = "open" }},
		{"a service with no port", "services", func(p *cmn.NetIdentityMod) { p.Services = []string{"20.20.20.1"} }},
		{"a service with port 0", "services", func(p *cmn.NetIdentityMod) { p.Services = []string{"20.20.20.1:0"} }},
		{"a service with a port past 65535", "services", func(p *cmn.NetIdentityMod) { p.Services = []string{"20.20.20.1:65536"} }},
		{"a service by name", "services", func(p *cmn.NetIdentityMod) { p.Services = []string{"api.example.com:443"} }},
		{"an IPv6 service", "services", func(p *cmn.NetIdentityMod) { p.Services = []string{"[2001:db8::1]:443"} }},
		{"too many services", "services", func(p *cmn.NetIdentityMod) {
			p.Services = many(netidMaxServices+1, func(i int) string { return fmt.Sprintf("20.20.%d.%d:80", i/250, i%250+1) })
		}},
		{"an empty model", "allowed_models", func(p *cmn.NetIdentityMod) { p.AllowedModels = []string{"m", ""} }},
		{"a model too long", "allowed_models", func(p *cmn.NetIdentityMod) { p.AllowedModels = []string{long[:128]} }},
		{"too many models", "allowed_models", func(p *cmn.NetIdentityMod) {
			p.AllowedModels = many(netidMaxModels+1, func(i int) string { return fmt.Sprintf("m%d", i) })
		}},
	}
	h, dp := newNidHolder(nil)
	for _, c := range bad {
		pm := base()
		c.mut(&pm)
		code, err := h.BindingAdd(&pm)
		var invalid *cmn.ValidationError
		if !errors.As(err, &invalid) || code != NetIdentityArgErr {
			t.Errorf("%s: code %d err %v, want a validation error", c.name, code, err)
			continue
		}
		if !strings.Contains(err.Error(), strings.TrimPrefix(c.field, "match.")) {
			t.Errorf("%s: %q does not name %s", c.name, err, c.field)
		}
	}
	if _, err := h.BindingAdd(nil); err == nil {
		t.Error("a nil binding was accepted")
	}
	if got, _ := h.BindingGet(); len(got) != 0 || dp.writes != 0 {
		t.Fatalf("a refused binding left %d bindings and %d data-plane writes", len(got), dp.writes)
	}

	// What is accepted is stored in one spelling.
	h2, _ := newNidHolder(nidIfs{"vlan100": 7})
	mustAdd(t, h2, &cmn.NetIdentityMod{
		Name: "Line_1.a-b", Tenant: "t1",
		Match:         cmn.NetIdentityMatch{CIDR: []string{" 10.20.0.0/24", "10.20.0.0/24", "0.0.0.0/0"}},
		Services:      []string{"20.20.20.2:8080", " 20.20.20.1:08080 ", "20.20.20.2:8080"},
		AllowedModels: []string{"m2", "m1", "m2"},
		Enforce:       " STRICT ", FailMode: "Closed",
	})
	mustAdd(t, h2, &cmn.NetIdentityMod{
		Name: "tunnel", Tenant: "t2", Match: cmn.NetIdentityMatch{IngressIf: " vlan100 "},
		Credential: "Network", PriorityClass: "SHEDDABLE",
	})
	got, _ := h2.BindingGet()
	want := []cmn.NetIdentityMod{
		{
			Name: "Line_1.a-b", Tenant: "t1",
			Match:         cmn.NetIdentityMatch{CIDR: []string{"10.20.0.0/24", "0.0.0.0/0"}},
			Services:      []string{"20.20.20.1:8080", "20.20.20.2:8080"},
			AllowedModels: []string{"m2", "m1"},
			Enforce:       "strict", Credential: "none", PriorityClass: "standard", FailMode: "closed",
		},
		{
			Name: "tunnel", Tenant: "t2", Match: cmn.NetIdentityMatch{IngressIf: "vlan100"},
			Enforce: "advisory", Credential: "network", PriorityClass: "sheddable", FailMode: "neutral",
		},
	}
	if len(got) != 2 {
		t.Fatalf("stored %d bindings, want 2", len(got))
	}
	for i := range want {
		if !reflect.DeepEqual(nidNoNil(got[i]), nidNoNil(want[i])) {
			t.Errorf("binding %d stored as %+v, want %+v", i, got[i], want[i])
		}
	}
	// What is stored is accepted again unchanged: a snapshot restores.
	for i := range got {
		again, err := netidNormalize(&got[i])
		if err != nil || !reflect.DeepEqual(nidNoNil(again), nidNoNil(got[i])) {
			t.Errorf("stored binding %s does not normalize to itself: %+v (%v)", got[i].Name, again, err)
		}
	}
	// A caller cannot reach the stored copy through what it was handed.
	got[0].Services[0] = "1.1.1.1:1"
	got[0].Match.CIDR[0] = "9.9.9.0/24"
	if fresh, _ := h2.BindingGet(); fresh[0].Services[0] != "20.20.20.1:8080" || fresh[0].Match.CIDR[0] != "10.20.0.0/24" {
		t.Errorf("a GET handed out the stored slices: %+v", fresh[0])
	}
}

// nidNoNil makes an empty list and a missing one compare equal.
func nidNoNil(b cmn.NetIdentityMod) cmn.NetIdentityMod {
	if len(b.Match.CIDR) == 0 {
		b.Match.CIDR = nil
	}
	if len(b.Services) == 0 {
		b.Services = nil
	}
	if len(b.AllowedModels) == 0 {
		b.AllowedModels = nil
	}
	return b
}

func TestNetIdentityDelete(t *testing.T) {
	h, dp := newNidHolder(nil)
	if code, err := h.BindingDel("nobody"); err == nil || code != NetIdentityNoExistErr || !strings.Contains(err.Error(), "no such") {
		t.Fatalf("deleting a binding that is not there: code %d err %v", code, err)
	}
	mustAdd(t, h, nidCIDR("a", "ta", []string{"10.20.0.0/24"}, nidSvcA))
	mustAdd(t, h, nidCIDR("b", "tb", []string{"10.30.0.0/24"}, nidSvcA))
	if _, err := h.BindingDel("a"); err != nil {
		t.Fatal(err)
	}
	if got := nidTenant(t, h, "10.20.0.1", 0, nidSvcA); got != "" {
		t.Fatalf("a deleted binding still matches: %q", got)
	}
	if got := nidTenant(t, h, "10.30.0.1", 0, nidSvcA); got != "tb" {
		t.Fatalf("the other binding of the service: %q", got)
	}
	if bound, _ := dp.has(nidSvcA); !bound {
		t.Fatal("the service left the bound set with a binding still on it")
	}
	if _, err := h.BindingDel("b"); err != nil {
		t.Fatal(err)
	}
	if bound, _ := dp.has(nidSvcA); bound {
		t.Fatal("the service is still bound with no binding left")
	}
}

func TestNetIdentityBindingLimit(t *testing.T) {
	h, _ := newNidHolder(nil)
	for i := 0; i < netidMaxBindings; i++ {
		name := fmt.Sprintf("b%d", i)
		h.bindings[name] = cmn.NetIdentityMod{Name: name, Tenant: "t", Match: cmn.NetIdentityMatch{CIDR: []string{
			fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)}}}
	}
	var invalid *cmn.ValidationError
	if _, err := h.BindingAdd(nidCIDR("one-more", "t", []string{"192.0.2.0/24"})); !errors.As(err, &invalid) {
		t.Fatalf("a binding past the limit: %v", err)
	}
	// A replace of one that exists is not one more.
	if _, err := h.BindingAdd(nidCIDR("b7", "t", []string{"192.0.2.0/24"})); err != nil {
		t.Fatalf("a replace at the limit: %v", err)
	}
}

// The bound set a configuration needs.
func TestNetIdentityBoundSet(t *testing.T) {
	closed := func(pm *cmn.NetIdentityMod) *cmn.NetIdentityMod {
		pm.FailMode = cmn.NetIdentityFailClosed
		return pm
	}
	h, dp := newNidHolder(nil)
	type want struct{ bound, closed bool }
	check := func(step string, w map[string]want) {
		t.Helper()
		for svc, x := range w {
			if b, c := dp.has(svc); b != x.bound || c != x.closed {
				t.Errorf("%s: %s reads bound=%v closed=%v, want %v %v", step, svc, b, c, x.bound, x.closed)
			}
		}
	}
	check("nothing configured", map[string]want{nidSvcA: {}, nidSvcB: {}})

	mustAdd(t, h, nidCIDR("a", "ta", []string{"10.1.0.0/16"}, nidSvcA))
	check("one service", map[string]want{nidSvcA: {true, false}, nidSvcB: {}})

	mustAdd(t, h, closed(nidCIDR("a2", "ta2", []string{"10.2.0.0/16"}, nidSvcA, nidSvcB)))
	check("a closed binding closes each service it names", map[string]want{nidSvcA: {true, true}, nidSvcB: {true, true}, nidSvcC: {}})

	mustAdd(t, h, nidCIDR("g", "tg", []string{"10.3.0.0/16"}))
	check("a binding of every service binds all, and closes none", map[string]want{
		nidSvcA: {true, true}, nidSvcB: {true, true}, nidSvcC: {true, false}})

	mustAdd(t, h, closed(nidCIDR("g", "tg", []string{"10.3.0.0/16"})))
	check("closed on every service", map[string]want{nidSvcA: {true, true}, nidSvcC: {true, true}})

	if _, err := h.BindingDel("g"); err != nil {
		t.Fatal(err)
	}
	check("back to the named services", map[string]want{nidSvcA: {true, true}, nidSvcB: {true, true}, nidSvcC: {}})

	mustAdd(t, h, nidCIDR("a2", "ta2", []string{"10.2.0.0/16"}, nidSvcB))
	check("a replace that drops a service and the flag", map[string]want{nidSvcA: {true, false}, nidSvcB: {true, false}})

	// A change that leaves the set as it is tells the data plane nothing.
	writes := dp.writes
	mustAdd(t, h, nidCIDR("a3", "ta3", []string{"10.9.0.0/16"}, nidSvcA))
	mustAdd(t, h, nidCIDR("a", "ta", []string{"10.1.0.0/16", "10.8.0.0/16"}, nidSvcA))
	if dp.writes != writes {
		t.Errorf("%d data-plane writes for changes that leave the bound set alone", dp.writes-writes)
	}

	// Two sets that answer alike are equal, however they were built.
	listed := netidBoundOf(map[string]cmn.NetIdentityMod{
		"g": *nidCIDR("g", "t", []string{"10.0.0.0/8"}),
		"a": *nidCIDR("a", "t", []string{"10.0.0.0/9"}, nidSvcA),
	})
	bare := netidBoundOf(map[string]cmn.NetIdentityMod{"g": *nidCIDR("g", "t", []string{"10.0.0.0/8"})})
	if !listed.equal(bare) || !bare.equal(listed) {
		t.Errorf("a named service beside a binding of every service changed the set: %+v vs %+v", listed, bare)
	}
}

// The order of publication. A request reads the bound set first and the
// snapshot second, so at every instant of a change "would this source be
// admitted by its network" must be answered by the old configuration or by
// the new one, and a service must never read as bound while the snapshot
// does not know its bindings yet or any more.
func TestNetIdentityPublicationOrder(t *testing.T) {
	h, dp := newNidHolder(nil)
	const src = "10.20.0.9"
	var trace []string
	// wouldAdmit is the gate: it asks the resolver only for a bound service.
	wouldAdmit := func(svc string) bool {
		if bound, _ := dp.has(svc); !bound {
			return false
		}
		ans, rc := netidAnswerFor(h, src, 0, svc, "")
		return rc == 0 && ans.matched
	}
	snapKnows := func(svc string) bool {
		ans, rc := netidAnswerFor(h, src, 0, svc, "")
		return rc == 0 && ans.matched
	}
	h.step = func(stage string) {
		boundA, _ := dp.has(nidSvcA)
		boundB, _ := dp.has(nidSvcB)
		trace = append(trace, fmt.Sprintf("%s A[bound=%v snap=%v] B[bound=%v snap=%v]",
			stage, boundA, snapKnows(nidSvcA), boundB, snapKnows(nidSvcB)))
		for _, svc := range []string{nidSvcA, nidSvcB} {
			if bound, _ := dp.has(svc); bound && !snapKnows(svc) {
				t.Errorf("after %q: %s is bound and the snapshot has no binding for it", stage, svc)
			}
		}
	}
	expect := func(what string, want ...string) {
		t.Helper()
		if !reflect.DeepEqual(trace, want) {
			t.Errorf("%s:\n got  %q\n want %q", what, trace, want)
		}
		trace = nil
	}

	// Add: the snapshot first, the bound set after it.
	mustAdd(t, h, nidCIDR("line", "t-line", []string{"10.20.0.0/24"}, nidSvcA))
	expect("add",
		"snapshot A[bound=false snap=true] B[bound=false snap=false]",
		"bound A[bound=true snap=true] B[bound=false snap=false]")
	if !wouldAdmit(nidSvcA) || wouldAdmit(nidSvcB) {
		t.Fatal("after the add the source is not admitted on its service, or is on another")
	}

	// Replace that moves the binding to another service: A leaves the bound
	// set while the snapshot still knows it, then the snapshot changes, then
	// B enters.
	mustAdd(t, h, nidCIDR("line", "t-line", []string{"10.20.0.0/24"}, nidSvcB))
	expect("move",
		"bound A[bound=false snap=true] B[bound=false snap=false]",
		"snapshot A[bound=false snap=false] B[bound=false snap=true]",
		"bound A[bound=false snap=false] B[bound=true snap=true]")

	// Delete: the bound set first, the snapshot after it.
	if _, err := h.BindingDel("line"); err != nil {
		t.Fatal(err)
	}
	expect("delete",
		"bound A[bound=false snap=false] B[bound=false snap=true]",
		"snapshot A[bound=false snap=false] B[bound=false snap=false]")
	if wouldAdmit(nidSvcA) || wouldAdmit(nidSvcB) {
		t.Fatal("after the delete the source is still admitted by its network")
	}

	// A binding of every service, added and removed, follows the same order.
	mustAdd(t, h, nidCIDR("all", "t-all", []string{"10.20.0.0/24"}))
	expect("add for every service",
		"snapshot A[bound=false snap=true] B[bound=false snap=true]",
		"bound A[bound=true snap=true] B[bound=true snap=true]")
	if _, err := h.BindingDel("all"); err != nil {
		t.Fatal(err)
	}
	expect("delete for every service",
		"bound A[bound=false snap=true] B[bound=false snap=true]",
		"snapshot A[bound=false snap=false] B[bound=false snap=false]")
}

// A service that reads as bound while there is nothing to resolve against
// gets no match: never an admission. It is counted as a fault.
func TestNetIdentityBoundWithoutSnapshotNeverAdmits(t *testing.T) {
	h, dp := newNidHolder(nil)
	mustAdd(t, h, nidCIDR("line", "t-line", []string{"10.20.0.0/24"}, nidSvcA))
	if bound, _ := dp.has(nidSvcA); !bound {
		t.Fatal("setup: the service is not bound")
	}

	const svc = "198.51.100.7:9001" // a label of this test's own
	before := nidFaults(t, svc)
	h.snap.Store(nil)
	if ans, rc := netidAnswerFor(h, "10.20.0.9", 0, svc, ""); rc != -1 || ans != (netidAnswer{}) {
		t.Fatalf("no snapshot answered %+v rc %d, want no answer", ans, rc)
	}
	// No holder at all (the gate called before start-up finished).
	if ans, rc := netidAnswerFor(nil, "10.20.0.9", 0, svc, ""); rc != -1 || ans != (netidAnswer{}) {
		t.Fatalf("no holder answered %+v rc %d, want no answer", ans, rc)
	}
	// A resolver that fails inside: recovered, and no answer either.
	h.snap.Store(&netidSnapshot{})
	if ans, rc := netidAnswerFor(h, "10.20.0.9", 0, svc, ""); rc != -1 || ans != (netidAnswer{}) {
		t.Fatalf("a failing resolver answered %+v rc %d, want no answer", ans, rc)
	}
	if got := nidFaults(t, svc) - before; got != 3 {
		t.Fatalf("3 faults counted %v on the service", got)
	}
	if got := nidFaults(t, nidSvcA); got != 0 {
		t.Fatalf("another service counted %v faults", got)
	}

	// The next configuration change puts a snapshot back.
	mustAdd(t, h, nidCIDR("line2", "t-line2", []string{"10.30.0.0/24"}, nidSvcA))
	if got := nidTenant(t, h, "10.20.0.9", 0, nidSvcA); got != "t-line" {
		t.Fatalf("after the next change: %q", got)
	}
}

// The fault line is bounded; the count is not.
func TestNetIdentityFaultLogIsBounded(t *testing.T) {
	const svc = "198.51.100.7:9002"
	netidFaultLogged.Store(0)
	netidFaultSkipped.Store(0)
	before := nidFaults(t, svc)
	for i := 0; i < 50; i++ {
		netidFault(svc, "test")
	}
	if got := nidFaults(t, svc) - before; got != 50 {
		t.Fatalf("50 faults counted %v", got)
	}
	if got := netidFaultSkipped.Load(); got != 49 {
		t.Fatalf("50 faults in one window wrote %d lines, want 1", 50-got)
	}
}

// Requests resolve while the configuration churns. A source bound in every
// configuration always resolves to its tenant, one bound in none never
// resolves, and one whose binding comes and goes resolves to its tenant or
// to nothing. Run under the race detector.
func TestNetIdentityResolveUnderChurn(t *testing.T) {
	h, dp := newNidHolder(nil)
	mustAdd(t, h, nidCIDR("stable", "t-stable", []string{"10.1.0.0/16"}, nidSvcA))
	var stop atomic.Bool
	var wg sync.WaitGroup
	var resolves atomic.Int64
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if ans, rc := netidAnswerFor(h, "10.1.2.3", 0, nidSvcA, ""); rc != 0 || ans.tenant != "t-stable" {
					t.Errorf("stable source answered %+v rc %d", ans, rc)
					return
				}
				if ans, rc := netidAnswerFor(h, "192.0.2.1", 0, nidSvcA, ""); rc != 0 || ans.matched {
					t.Errorf("unbound source answered %+v rc %d", ans, rc)
					return
				}
				if ans, rc := netidAnswerFor(h, "10.2.0.1", 0, nidSvcB, ""); rc != 0 || (ans.matched && ans.tenant != "t-churn") {
					t.Errorf("churning source answered %+v rc %d", ans, rc)
					return
				}
				if bound, _ := dp.has(nidSvcA); !bound {
					t.Errorf("a service bound in every configuration read as unbound")
					return
				}
				resolves.Add(1)
			}
		}()
	}
	for i := 0; i < 400; i++ {
		mustAdd(t, h, nidCIDR("churn", "t-churn", []string{"10.2.0.0/16"}, nidSvcB, nidSvcC))
		mustAdd(t, h, nidCIDR(fmt.Sprintf("extra-%d", i%8), "t-extra", []string{fmt.Sprintf("10.%d.0.0/16", 100+i%8)}))
		if _, err := h.BindingDel("churn"); err != nil {
			t.Fatal(err)
		}
		if i%8 == 7 {
			for j := 0; j < 8; j++ {
				if _, err := h.BindingDel(fmt.Sprintf("extra-%d", j)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	if resolves.Load() == 0 {
		t.Fatal("no resolve ran beside the churn")
	}
}

func FuzzNetIdentityNormalize(f *testing.F) {
	f.Add("line-1", "10.20.0.0/24", "", "20.20.20.1:8080", "t1", "m", "strict", "none", "standard", "closed")
	f.Add("tunnel", "", "vlan100", "", "t2", "", "", "network", "", "")
	f.Add("", "::/0", "eth0", "[::1]:80", "a|b", "\x00", "x", "y", "z", "w")
	f.Add("a", "10.0.0.1/8", "", "1.2.3.4:99999", "t", "m", "ADVISORY", "NETWORK", "Critical", "NEUTRAL")
	f.Fuzz(func(t *testing.T, name, cidr, ifName, svc, tenant, model, enforce, credential, class, fail string) {
		pm := &cmn.NetIdentityMod{Name: name, Tenant: tenant, Enforce: enforce, Credential: credential,
			PriorityClass: class, FailMode: fail, Match: cmn.NetIdentityMatch{IngressIf: ifName}}
		if cidr != "" {
			pm.Match.CIDR = strings.Split(cidr, ",")
		}
		if svc != "" {
			pm.Services = strings.Split(svc, ",")
		}
		if model != "" {
			pm.AllowedModels = strings.Split(model, ",")
		}
		out, err := netidNormalize(pm)
		if err != nil {
			var invalid *cmn.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("a refusal that is not a validation error: %v", err)
			}
			return
		}
		// What is accepted fits the gate's fields, is in canonical form (it
		// normalizes to itself) and compiles into a snapshot that resolves.
		if len(out.Name) > netidNameMax || len(out.Tenant) > netidTenantMax || out.Name == "" || out.Tenant == "" {
			t.Fatalf("accepted a name or tenant the gate cannot carry: %+v", out)
		}
		if (len(out.Match.CIDR) == 0) == (out.Match.IngressIf == "") {
			t.Fatalf("accepted a binding without exactly one match: %+v", out)
		}
		if out.Credential == cmn.NetIdentityCredentialNetwork && out.Match.IngressIf == "" {
			t.Fatalf("accepted credential network on an address range: %+v", out)
		}
		again, err := netidNormalize(&out)
		if err != nil || !reflect.DeepEqual(nidNoNil(again), nidNoNil(out)) {
			t.Fatalf("the stored form does not normalize to itself: %+v -> %+v (%v)", out, again, err)
		}
		s := buildNetidSnapshot(map[string]cmn.NetIdentityMod{out.Name: out}, nidIfs{out.Match.IngressIf: 5}.index)
		for _, svc := range append([]string{"203.0.113.1:1"}, out.Services...) {
			s.resolve(0x0a140001, true, 5, svc)
			s.resolve(0, false, 0, svc)
		}
	})
}
