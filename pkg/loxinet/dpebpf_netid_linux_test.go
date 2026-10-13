//go:build linux

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
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
)

// The bound set as the data plane holds it after the control plane wrote
// it: the bridge, the two bits and the gate's own reader, with no stand-in.
func TestNetIdentityBoundSetReachesTheDataPlane(t *testing.T) {
	const both = netidBoundBit | netidFailClosedBit
	defer netidBoundPublish(netidBound{})

	netidBoundPublish(netidBound{svcs: map[string]bool{nidSvcA: false, nidSvcB: true}})
	for svc, want := range map[string]int{nidSvcA: netidBoundBit, nidSvcB: both, nidSvcC: 0, "": 0} {
		if got := netidBoundRead(svc); got != want {
			t.Errorf("listed: %q reads %#x, want %#x", svc, got, want)
		}
	}
	netidBoundPublish(netidBound{all: true, svcs: map[string]bool{nidSvcB: true}})
	for svc, want := range map[string]int{nidSvcA: netidBoundBit, nidSvcB: both, nidSvcC: netidBoundBit, "": 0} {
		if got := netidBoundRead(svc); got != want {
			t.Errorf("all: %q reads %#x, want %#x", svc, got, want)
		}
	}
	netidBoundPublish(netidBound{all: true, allClosed: true})
	if got := netidBoundRead(nidSvcC); got != both {
		t.Errorf("all closed: reads %#x, want %#x", got, both)
	}
	netidBoundPublish(netidBound{})
	for _, svc := range []string{nidSvcA, nidSvcB, nidSvcC} {
		if got := netidBoundRead(svc); got != 0 {
			t.Errorf("empty: %q reads %#x", svc, got)
		}
	}
}

// A holder wired to the real data plane: a binding binds its services there
// and its removal unbinds them.
func TestNetIdentityHolderDrivesTheDataPlane(t *testing.T) {
	defer netidBoundPublish(netidBound{})
	h := newNetIdentityH(netidBoundPublish, nidIfs{}.index)

	pm := nidCIDR("line", "t-line", []string{"10.20.0.0/24"}, nidSvcA, nidSvcB)
	pm.FailMode = cmn.NetIdentityFailClosed
	mustAdd(t, h, pm)
	if a, c := netidBoundRead(nidSvcA), netidBoundRead(nidSvcC); a != netidBoundBit|netidFailClosedBit || c != 0 {
		t.Fatalf("after the add: bound service reads %#x, another %#x", a, c)
	}
	mustAdd(t, h, nidCIDR("line", "t-line", []string{"10.20.0.0/24"}, nidSvcB))
	if a, b := netidBoundRead(nidSvcA), netidBoundRead(nidSvcB); a != 0 || b != netidBoundBit {
		t.Fatalf("after the replace: dropped service reads %#x, kept one %#x", a, b)
	}
	// More services than the data plane lists by name: every one reads as
	// bound, none is missed.
	for i := 0; i < 5; i++ {
		svcs := make([]string, 0, netidMaxServices)
		for j := 0; j < netidMaxServices; j++ {
			svcs = append(svcs, fmt.Sprintf("30.%d.%d.1:80", i, j))
		}
		mustAdd(t, h, nidCIDR(fmt.Sprintf("wide-%d", i), "t-wide", []string{fmt.Sprintf("10.%d.0.0/16", 50+i)}, svcs...))
	}
	for _, svc := range []string{"30.0.0.1:80", "30.4.255.1:80", nidSvcB} {
		if got := netidBoundRead(svc); got&netidBoundBit == 0 {
			t.Errorf("%s is not bound among %d services", svc, 5*netidMaxServices+1)
		}
	}
	for _, name := range []string{"line", "wide-0", "wide-1", "wide-2", "wide-3", "wide-4"} {
		if _, err := h.BindingDel(name); err != nil {
			t.Fatal(err)
		}
	}
	if got := netidBoundRead(nidSvcB); got != 0 {
		t.Fatalf("after the deletes: reads %#x", got)
	}
}
