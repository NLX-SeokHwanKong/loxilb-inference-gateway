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
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tk "github.com/loxilb-io/loxilib"

	prom "github.com/loxilb-io/loxilb/api/prometheus"
	cmn "github.com/loxilb-io/loxilb/common"
	"github.com/loxilb-io/loxilb/pkg/aikey"
)

// Network-identity bindings: the tenant of the requests that reach an AI
// service from a network.
//
// Two things are kept for the admission gate, and they are published in a
// fixed order. The resolver snapshot is what a request is matched against:
// immutable, swapped whole, read without a lock. The bound set tells the
// data plane which services have a binding at all, so that a service with
// none never asks. A service enters the bound set only after a snapshot that
// knows its bindings is in place, and leaves it before that snapshot is
// replaced by one that does not. A request that finds its service bound
// therefore always reads a snapshot at least as new as the one that bound
// it, and one that finds no snapshot matches nothing.

// Limits of the resource. A binding's name and tenant go into fixed-size
// fields of the gate's result (64 and 128 bytes with the terminator).
const (
	netidNameMax     = 63
	netidTenantMax   = 127
	netidModelMax    = 127
	netidIfNameMax   = 15
	netidMaxBindings = 4096
	netidMaxCIDRs    = 256
	netidMaxServices = 256
	netidMaxModels   = 256
)

// netidBinding is a binding as the resolver answers with it. Immutable once
// a snapshot holds it.
type netidBinding struct {
	name        string
	tenant      string
	strict      bool
	credNetwork bool
	forward     bool
	models      map[string]struct{} // nil: any model
}

func (b *netidBinding) modelOK(model string) bool {
	if b.models == nil {
		return true
	}
	_, ok := b.models[model]
	return ok
}

// netidTable holds the bindings of one scope: one service, or every service.
type netidTable struct {
	byIf   map[int]*netidBinding
	byCIDR [33]map[uint32]*netidBinding // by prefix length, keyed by the network address
	lens   []int                        // the prefix lengths in use, longest first
}

func (t *netidTable) addCIDR(pfx netip.Prefix, b *netidBinding) {
	bits := pfx.Bits()
	if t.byCIDR[bits] == nil {
		t.byCIDR[bits] = make(map[uint32]*netidBinding)
		t.lens = append(t.lens, bits)
		sort.Sort(sort.Reverse(sort.IntSlice(t.lens)))
	}
	key := netidV4(pfx.Addr())
	if _, taken := t.byCIDR[bits][key]; !taken {
		t.byCIDR[bits][key] = b
	}
}

func (t *netidTable) addIf(ifindex int, b *netidBinding) {
	if t.byIf == nil {
		t.byIf = make(map[int]*netidBinding)
	}
	if _, taken := t.byIf[ifindex]; !taken {
		t.byIf[ifindex] = b
	}
}

// lpm returns the binding of the longest prefix that holds ip, and that
// prefix's length; nil and -1 when none does.
func (t *netidTable) lpm(ip uint32) (*netidBinding, int) {
	for _, bits := range t.lens {
		if b := t.byCIDR[bits][ip&netidMask(bits)]; b != nil {
			return b, bits
		}
	}
	return nil, -1
}

func netidMask(bits int) uint32 {
	if bits == 0 {
		return 0
	}
	return ^uint32(0) << (32 - bits)
}

func netidV4(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// netidSnapshot is what a request is resolved against.
type netidSnapshot struct {
	global *netidTable
	bySvc  map[string]*netidTable
}

// resolve finds the binding of a source on a service. An interface match
// wins over any CIDR; among CIDRs the longest prefix wins; where a binding
// of the service and one of every service match as specifically, the
// service's wins.
func (s *netidSnapshot) resolve(ip uint32, ipOK bool, ifindex int, svc string) *netidBinding {
	st := s.bySvc[svc]
	if ifindex > 0 {
		if st != nil {
			if b := st.byIf[ifindex]; b != nil {
				return b
			}
		}
		if b := s.global.byIf[ifindex]; b != nil {
			return b
		}
	}
	if !ipOK {
		return nil
	}
	gb, gl := s.global.lpm(ip)
	if st != nil {
		if sb, sl := st.lpm(ip); sb != nil && sl >= gl {
			return sb
		}
	}
	return gb
}

// buildNetidSnapshot compiles the bindings. They are taken in name order and
// the first to claim a key keeps it, so two bindings that match as
// specifically on one service (which the configuration allows only for one
// tenant) resolve to the same one every time. An interface that does not
// exist now contributes nothing.
func buildNetidSnapshot(bindings map[string]cmn.NetIdentityMod, ifIndex func(string) (int, bool)) *netidSnapshot {
	s := &netidSnapshot{global: &netidTable{}, bySvc: make(map[string]*netidTable)}
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pm := bindings[name]
		b := &netidBinding{
			name:        pm.Name,
			tenant:      pm.Tenant,
			strict:      pm.Enforce == cmn.NetIdentityEnforceStrict,
			credNetwork: pm.Credential == cmn.NetIdentityCredentialNetwork,
			forward:     pm.ForwardIdentity,
		}
		if len(pm.AllowedModels) > 0 {
			b.models = make(map[string]struct{}, len(pm.AllowedModels))
			for _, m := range pm.AllowedModels {
				b.models[m] = struct{}{}
			}
		}
		tables := []*netidTable{s.global}
		if len(pm.Services) > 0 {
			tables = tables[:0]
			for _, svc := range pm.Services {
				t := s.bySvc[svc]
				if t == nil {
					t = &netidTable{}
					s.bySvc[svc] = t
				}
				tables = append(tables, t)
			}
		}
		if pm.Match.IngressIf != "" {
			if idx, ok := ifIndex(pm.Match.IngressIf); ok && idx > 0 {
				for _, t := range tables {
					t.addIf(idx, b)
				}
			}
			continue
		}
		for _, c := range pm.Match.CIDR {
			pfx, err := netip.ParsePrefix(c)
			if err != nil {
				continue // stored bindings are validated; nothing to match on otherwise
			}
			for _, t := range tables {
				t.addCIDR(pfx, b)
			}
		}
	}
	return s
}

// netidBound is the bound set: the services that have a binding, and which
// of them refuse a request the resolver could not answer for.
type netidBound struct {
	all       bool            // a binding names no service: every AI service is bound
	allClosed bool            // ... and every one fails closed
	svcs      map[string]bool // service -> fails closed
}

func netidBoundOf(bindings map[string]cmn.NetIdentityMod) netidBound {
	nb := netidBound{svcs: make(map[string]bool)}
	for _, pm := range bindings {
		closed := pm.FailMode == cmn.NetIdentityFailClosed
		if len(pm.Services) == 0 {
			nb.all = true
			nb.allClosed = nb.allClosed || closed
			continue
		}
		for _, svc := range pm.Services {
			nb.svcs[svc] = nb.svcs[svc] || closed
		}
	}
	return nb.canon()
}

// canon drops what says nothing: once every service is bound, a listed
// service only matters where it fails closed and the others do not. Two
// sets that answer has alike are then equal.
func (nb netidBound) canon() netidBound {
	if !nb.all {
		return nb
	}
	for svc, closed := range nb.svcs {
		if nb.allClosed || !closed {
			delete(nb.svcs, svc)
		}
	}
	return nb
}

// has reports whether a service is bound, and whether it fails closed.
func (nb netidBound) has(svc string) (bound, closed bool) {
	c, listed := nb.svcs[svc]
	return nb.all || listed, (nb.all && nb.allClosed) || c
}

func (nb netidBound) equal(o netidBound) bool {
	if nb.all != o.all || nb.allClosed != o.allClosed || len(nb.svcs) != len(o.svcs) {
		return false
	}
	for svc, c := range nb.svcs {
		if oc, ok := o.svcs[svc]; !ok || oc != c {
			return false
		}
	}
	return true
}

// meet is the services bound in both sets, with the flags of next: the set
// to publish while the snapshot goes from the one nb describes to the one
// next describes.
func (nb netidBound) meet(next netidBound) netidBound {
	m := netidBound{all: nb.all && next.all, svcs: make(map[string]bool)}
	m.allClosed = m.all && next.allClosed
	keep := func(svc string) {
		if _, done := m.svcs[svc]; done {
			return
		}
		was, _ := nb.has(svc)
		is, closed := next.has(svc)
		if was && is {
			m.svcs[svc] = closed
		}
	}
	for svc := range nb.svcs {
		keep(svc)
	}
	for svc := range next.svcs {
		keep(svc)
	}
	return m.canon()
}

// NetIdentityH holds the configured bindings, the resolver snapshot compiled
// from them and what the data plane was last told about bound services.
type NetIdentityH struct {
	mu       sync.Mutex
	bindings map[string]cmn.NetIdentityMod
	bound    netidBound
	snap     atomic.Pointer[netidSnapshot]

	// publish hands the bound set to the data plane.
	publish func(netidBound)
	// ifIndex resolves an interface name on this node.
	ifIndex func(name string) (int, bool)
	// step, when set, is told each stage of a configuration change as it
	// completes ("bound", "snapshot"). Tests only.
	step func(stage string)
}

// netidActive is the holder the gate's resolver reads. Set once at start-up.
var netidActive atomic.Pointer[NetIdentityH]

// NetIdentityInit builds the holder with an empty snapshot in place: the
// data plane starts with no service bound, which is what no binding means.
func NetIdentityInit() *NetIdentityH {
	h := newNetIdentityH(netidBoundPublish, netidIfIndex)
	netidActive.Store(h)
	return h
}

func newNetIdentityH(publish func(netidBound), ifIndex func(string) (int, bool)) *NetIdentityH {
	h := &NetIdentityH{
		bindings: make(map[string]cmn.NetIdentityMod),
		bound:    netidBound{svcs: make(map[string]bool)},
		publish:  publish,
		ifIndex:  ifIndex,
	}
	h.snap.Store(buildNetidSnapshot(nil, ifIndex))
	return h
}

func netidIfIndex(name string) (int, bool) {
	ifi, err := net.InterfaceByName(name)
	if err != nil || ifi == nil {
		return 0, false
	}
	return ifi.Index, true
}

// commit makes next the configuration. Called with mu held.
func (h *NetIdentityH) commit(next map[string]cmn.NetIdentityMod) {
	snap := buildNetidSnapshot(next, h.ifIndex)
	nb := netidBoundOf(next)

	// Leave first: a service that is losing its bindings stops asking
	// before the snapshot forgets them.
	if mid := h.bound.meet(nb); !mid.equal(h.bound) {
		h.publish(mid)
		h.bound = mid
		h.told("bound")
	}
	h.snap.Store(snap)
	h.told("snapshot")
	// Enter last: a service that is gaining bindings starts asking once the
	// snapshot knows them.
	if !nb.equal(h.bound) {
		h.publish(nb)
		h.bound = nb
		h.told("bound")
	}
	h.bindings = next
}

func (h *NetIdentityH) told(stage string) {
	if h.step != nil {
		h.step(stage)
	}
}

// BindingAdd creates or replaces a binding.
func (h *NetIdentityH) BindingAdd(pm *cmn.NetIdentityMod) (int, error) {
	nm, err := netidNormalize(pm)
	if err != nil {
		return NetIdentityArgErr, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, replace := h.bindings[nm.Name]; !replace && len(h.bindings) >= netidMaxBindings {
		return NetIdentityArgErr, cmn.NewValidationError("", "at most %d network-identity bindings", netidMaxBindings)
	}
	if err := netidConflict(h.bindings, &nm); err != nil {
		return NetIdentityConflictErr, err
	}
	next := make(map[string]cmn.NetIdentityMod, len(h.bindings)+1)
	for name, b := range h.bindings {
		next[name] = b
	}
	next[nm.Name] = nm
	h.commit(next)
	tk.LogIt(tk.LogInfo, "[NetIdentity] binding %s stored (tenant %s, %s)\n", nm.Name, nm.Tenant, netidMatchWord(&nm))
	return 0, nil
}

// BindingDel removes a binding.
func (h *NetIdentityH) BindingDel(name string) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.bindings[name]; !ok {
		// "no such" is the phrase the API classifier reads as 404.
		return NetIdentityNoExistErr, fmt.Errorf("no such network-identity binding %s", name)
	}
	next := make(map[string]cmn.NetIdentityMod, len(h.bindings))
	for n, b := range h.bindings {
		if n != name {
			next[n] = b
		}
	}
	h.commit(next)
	tk.LogIt(tk.LogInfo, "[NetIdentity] binding %s deleted\n", name)
	return 0, nil
}

// BindingGet returns the bindings in name order.
func (h *NetIdentityH) BindingGet() ([]cmn.NetIdentityMod, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]cmn.NetIdentityMod, 0, len(h.bindings))
	for _, b := range h.bindings {
		out = append(out, netidCopy(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// BindingWarning is what is worth saying about a binding that was just
// stored. It refuses nothing. Empty when there is nothing to say.
func (h *NetIdentityH) BindingWarning(pm *cmn.NetIdentityMod) string {
	if pm == nil {
		return ""
	}
	var msgs []string
	if ifName := strings.TrimSpace(pm.Match.IngressIf); ifName != "" {
		if _, ok := h.ifIndex(ifName); !ok {
			msgs = append(msgs, fmt.Sprintf("match.ingress_if: no interface %q on this node; the binding matches nothing until it exists", ifName))
		}
	}
	if nm, err := netidNormalize(pm); err == nil {
		h.mu.Lock()
		msg := netidFailModeWarning(h.bindings, &nm)
		h.mu.Unlock()
		if msg != "" {
			msgs = append(msgs, msg)
		}
	}
	// Each warning after its own marker, as a rule reply carries several.
	return strings.Join(msgs, "; warning: ")
}

// netidFailModeWarning is the text for a binding whose fail_mode is not the
// one of every other binding that applies to the same service. The flag is
// per service: one binding that says closed, naming the service or naming
// none, makes the service fail closed for every source. Empty when the
// bindings agree.
func netidFailModeWarning(bindings map[string]cmn.NetIdentityMod, nm *cmn.NetIdentityMod) string {
	applies := func(b *cmn.NetIdentityMod, svc string) bool {
		if len(b.Services) == 0 {
			return true
		}
		for _, s := range b.Services {
			if s == svc {
				return true
			}
		}
		return false
	}
	// The services to look at: the binding's own, or, for a binding of
	// every service, each service any binding names, and "" for the
	// bindings of every service among themselves.
	svcs := append([]string(nil), nm.Services...)
	if len(svcs) == 0 {
		seen := map[string]bool{"": true}
		svcs = append(svcs, "")
		for _, b := range bindings {
			for _, s := range b.Services {
				if !seen[s] {
					seen[s] = true
					svcs = append(svcs, s)
				}
			}
		}
	}
	sort.Strings(svcs)

	var first string
	var closed, neutral []string
	split := 0
	for _, svc := range svcs {
		var c, n []string
		for name, b := range bindings {
			b := b
			if svc == "" && len(b.Services) != 0 {
				continue
			}
			if svc != "" && !applies(&b, svc) {
				continue
			}
			if b.FailMode == cmn.NetIdentityFailClosed {
				c = append(c, name)
			} else {
				n = append(n, name)
			}
		}
		if len(c) == 0 || len(n) == 0 {
			continue
		}
		if split == 0 {
			first, closed, neutral = svc, c, n
		}
		split++
	}
	if split == 0 {
		return ""
	}
	sort.Strings(closed)
	sort.Strings(neutral)
	where := first
	if where == "" {
		where = "every AI service"
	}
	more := ""
	if split > 1 {
		more = fmt.Sprintf(" (and on %d more)", split-1)
	}
	return fmt.Sprintf("fail_mode: the bindings that apply to %s disagree (closed: %s; neutral: %s)%s; "+
		"fail_mode is per service, and a service fails closed when any binding that applies to it says closed",
		where, strings.Join(closed, ", "), strings.Join(neutral, ", "), more)
}

// resolve answers the gate. ok is false when there is no snapshot to read,
// which the caller reports as a resolver that could not answer.
func (h *NetIdentityH) resolve(clientIP string, ifindex int, svc string) (b *netidBinding, ok bool) {
	s := h.snap.Load()
	if s == nil {
		return nil, false
	}
	var ip uint32
	ipOK := false
	if a, err := netip.ParseAddr(clientIP); err == nil {
		if a = a.Unmap(); a.Is4() {
			ip, ipOK = netidV4(a), true
		}
	}
	return s.resolve(ip, ipOK, ifindex, svc), true
}

// netidAnswer is a resolved binding as the gate reads it.
type netidAnswer struct {
	matched     bool
	strict      bool
	credNetwork bool
	modelOK     bool
	forward     bool
	tenant      string
	binding     string
}

// netidAnswerFor is the resolver as the gate calls it. rc is 0 when it
// answered and -1 when it could not: no holder, no snapshot, or a failure
// inside, which is recovered here because the caller is a data-plane
// thread. A fault is counted and logged, and never answers with a match.
func netidAnswerFor(h *NetIdentityH, clientIP string, ifindex int, svc, model string) (ans netidAnswer, rc int) {
	defer func() {
		if r := recover(); r != nil {
			ans, rc = netidAnswer{}, -1
			netidFault(svc, fmt.Sprintf("recovered panic: %v", r))
		}
	}()
	if h == nil {
		netidFault(svc, "resolver not started")
		return ans, -1
	}
	b, ok := h.resolve(clientIP, ifindex, svc)
	if !ok {
		netidFault(svc, "no snapshot")
		return ans, -1
	}
	if b == nil {
		return ans, 0
	}
	return netidAnswer{
		matched:     true,
		strict:      b.strict,
		credNetwork: b.credNetwork,
		modelOK:     b.modelOK(model),
		forward:     b.forward,
		tenant:      b.tenant,
		binding:     b.name,
	}, 0
}

// The bits of the bound set, as sockproxy_ai_admit.h defines them
// (AI_GW_NETID_BOUND, AI_GW_NETID_FAIL_CLOSED).
const (
	netidBoundBit      = 0x1
	netidFailClosedBit = 0x2
)

// netidFaultLogEvery bounds the fault line: a fault is per request, and a
// resolver that fails does so for every request of its services.
const netidFaultLogEvery = 10 * time.Second

var (
	netidFaultLogged  atomic.Int64 // when the last line was written, Unix nanoseconds
	netidFaultSkipped atomic.Uint64
)

// netidFault records a request the resolver could not answer for: always
// counted, logged at most once per netidFaultLogEvery.
func netidFault(svc, why string) {
	prom.RecordNetIDResolverFault(svc)
	now := time.Now().UnixNano()
	last := netidFaultLogged.Load()
	if (last != 0 && now-last < int64(netidFaultLogEvery)) || !netidFaultLogged.CompareAndSwap(last, now) {
		netidFaultSkipped.Add(1)
		return
	}
	tk.LogIt(tk.LogWarning, "[NetIdentity] resolver could not answer for service %s (%s): the request is decided "+
		"without its binding, and a strict binding is not enforced, unless the service fails closed; "+
		"%d more since the last such line\n", svc, why, netidFaultSkipped.Swap(0))
}

func netidCopy(b cmn.NetIdentityMod) cmn.NetIdentityMod {
	b.Match.CIDR = append([]string(nil), b.Match.CIDR...)
	b.Services = append([]string(nil), b.Services...)
	b.AllowedModels = append([]string(nil), b.AllowedModels...)
	return b
}

func netidMatchWord(pm *cmn.NetIdentityMod) string {
	if pm.Match.IngressIf != "" {
		return "ingress_if " + pm.Match.IngressIf
	}
	return "cidr " + strings.Join(pm.Match.CIDR, ",")
}

func netidHasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func netidNameOK(s string) bool {
	if s == "" || len(s) > netidNameMax {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum && (i == 0 || (c != '-' && c != '_' && c != '.')) {
			return false
		}
	}
	return true
}

// netidEnum lowers v and holds it to the allowed values; an empty v is the
// first of them.
func netidEnum(field, v string, allowed ...string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return allowed[0], nil
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", cmn.NewValidationError(field, "%s must be one of %s", field, strings.Join(allowed, ", "))
}

// netidService holds a service to the form the gate's service identity has:
// dotted IPv4, a colon, the port.
func netidService(s string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return "", cmn.NewValidationError("services", "services: %q is not VIP:port", s)
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !a.Is4() {
		return "", cmn.NewValidationError("services", "services: %q does not name an IPv4 VIP", s)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return "", cmn.NewValidationError("services", "services: %q does not name a port between 1 and 65535", s)
	}
	return a.String() + ":" + strconv.FormatUint(p, 10), nil
}

// netidNormalize validates a binding and returns it in the form it is
// stored: enumerated fields spelled out, lists free of repeats, CIDRs and
// services in their canonical spelling.
func netidNormalize(pm *cmn.NetIdentityMod) (cmn.NetIdentityMod, error) {
	var out cmn.NetIdentityMod
	if pm == nil {
		return out, cmn.NewValidationError("", "a binding is required")
	}
	if !netidNameOK(pm.Name) {
		return out, cmn.NewValidationError("name",
			"name must be 1 to %d characters of letters, digits, '-', '_' and '.', starting with a letter or digit", netidNameMax)
	}
	out.Name = pm.Name

	ifName := strings.TrimSpace(pm.Match.IngressIf)
	switch {
	case len(pm.Match.CIDR) == 0 && ifName == "":
		return out, cmn.NewValidationError("match", "match needs one of cidr or ingress_if")
	case len(pm.Match.CIDR) != 0 && ifName != "":
		return out, cmn.NewValidationError("match", "match takes one of cidr or ingress_if, not both")
	case ifName != "":
		if len(ifName) > netidIfNameMax || strings.ContainsAny(ifName, "/ \t") || netidHasControl(ifName) {
			return out, cmn.NewValidationError("match.ingress_if", "match.ingress_if: %q is not an interface name", pm.Match.IngressIf)
		}
		out.Match.IngressIf = ifName
	default:
		if len(pm.Match.CIDR) > netidMaxCIDRs {
			return out, cmn.NewValidationError("match.cidr", "match.cidr: at most %d networks", netidMaxCIDRs)
		}
		seen := make(map[netip.Prefix]bool, len(pm.Match.CIDR))
		for _, c := range pm.Match.CIDR {
			pfx, err := netip.ParsePrefix(strings.TrimSpace(c))
			if err != nil || !pfx.Addr().Is4() {
				return out, cmn.NewValidationError("match.cidr", "match.cidr: %q is not an IPv4 network (a.b.c.d/len)", c)
			}
			if pfx != pfx.Masked() {
				return out, cmn.NewValidationError("match.cidr", "match.cidr: %q has host bits set; the network is %s", c, pfx.Masked())
			}
			if !seen[pfx] {
				seen[pfx] = true
				out.Match.CIDR = append(out.Match.CIDR, pfx.String())
			}
		}
	}

	if pm.Tenant == "" || len(pm.Tenant) > netidTenantMax || netidHasControl(pm.Tenant) {
		return out, cmn.NewValidationError("tenant", "tenant must be 1 to %d bytes with no control character", netidTenantMax)
	}
	if err := aikey.ValidateQoSIdentity("tenant", pm.Tenant); err != nil {
		return out, err
	}
	out.Tenant = pm.Tenant

	var err error
	if out.Enforce, err = netidEnum("enforce", pm.Enforce,
		cmn.NetIdentityEnforceAdvisory, cmn.NetIdentityEnforceStrict); err != nil {
		return out, err
	}
	if out.Credential, err = netidEnum("credential", pm.Credential,
		cmn.NetIdentityCredentialNone, cmn.NetIdentityCredentialNetwork); err != nil {
		return out, err
	}
	if out.Credential == cmn.NetIdentityCredentialNetwork && out.Match.IngressIf == "" {
		return out, cmn.NewValidationError("credential",
			"credential: network needs an ingress_if match; an address range does not stand in for a credential")
	}
	if out.PriorityClass, err = netidEnum("priority_class", pm.PriorityClass,
		cmn.NetIdentityClassStandard, cmn.NetIdentityClassCritical, cmn.NetIdentityClassSheddable); err != nil {
		return out, err
	}
	if out.FailMode, err = netidEnum("fail_mode", pm.FailMode,
		cmn.NetIdentityFailNeutral, cmn.NetIdentityFailClosed); err != nil {
		return out, err
	}
	out.ForwardIdentity = pm.ForwardIdentity

	if len(pm.Services) > netidMaxServices {
		return out, cmn.NewValidationError("services", "services: at most %d", netidMaxServices)
	}
	svcSeen := make(map[string]bool, len(pm.Services))
	for _, s := range pm.Services {
		svc, err := netidService(s)
		if err != nil {
			return out, err
		}
		if !svcSeen[svc] {
			svcSeen[svc] = true
			out.Services = append(out.Services, svc)
		}
	}
	sort.Strings(out.Services)

	if len(pm.AllowedModels) > netidMaxModels {
		return out, cmn.NewValidationError("allowed_models", "allowed_models: at most %d", netidMaxModels)
	}
	modelSeen := make(map[string]bool, len(pm.AllowedModels))
	for _, m := range pm.AllowedModels {
		if m == "" || len(m) > netidModelMax || netidHasControl(m) {
			return out, cmn.NewValidationError("allowed_models",
				"allowed_models: a model is 1 to %d bytes with no control character", netidModelMax)
		}
		if !modelSeen[m] {
			modelSeen[m] = true
			out.AllowedModels = append(out.AllowedModels, m)
		}
	}
	return out, nil
}

// netidKeys lists what a binding claims: one key for each scope and match it
// has. Two bindings that share a key match the same source on the same
// service as specifically as each other.
func netidKeys(pm *cmn.NetIdentityMod) []string {
	scopes := pm.Services
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	matches := pm.Match.CIDR
	kind := "cidr "
	if pm.Match.IngressIf != "" {
		matches, kind = []string{pm.Match.IngressIf}, "ingress_if "
	}
	keys := make([]string, 0, len(scopes)*len(matches))
	for _, sc := range scopes {
		for _, m := range matches {
			keys = append(keys, sc+"|"+kind+m)
		}
	}
	return keys
}

// netidConflict refuses a binding that would leave a source with two tenants:
// another binding claims one of its keys for a different tenant.
func netidConflict(have map[string]cmn.NetIdentityMod, pm *cmn.NetIdentityMod) error {
	mine := make(map[string]bool)
	for _, k := range netidKeys(pm) {
		mine[k] = true
	}
	names := make([]string, 0, len(have))
	for name := range have {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		other := have[name]
		if name == pm.Name || other.Tenant == pm.Tenant {
			continue
		}
		for _, k := range netidKeys(&other) {
			if !mine[k] {
				continue
			}
			scope, match, _ := strings.Cut(k, "|")
			if scope == "" {
				scope = "every service"
			} else {
				scope = "service " + scope
			}
			return cmn.NewConflictError("binding %s already maps %s on %s to tenant %s",
				other.Name, match, scope, other.Tenant)
		}
	}
	return nil
}

// error codes
const (
	NetIdentityErrBase = iota - 119000
	NetIdentityArgErr
	NetIdentityNoExistErr
	NetIdentityConflictErr
)
