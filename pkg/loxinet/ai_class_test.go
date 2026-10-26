/*
 * Copyright (c) 2025 LoxiLB Authors
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
	"testing"

	"github.com/loxilb-io/loxilb/pkg/aikey"

	cmn "github.com/loxilb-io/loxilb/common"
)

const classSvc = "10.0.0.1:8080"

func classSnap() *aikey.ClassSnapshot {
	return &aikey.ClassSnapshot{
		Objectives: map[string]cmn.AIObjectiveEntry{
			"realtime":  {Name: "realtime", Priority: 10},
			"default":   {Name: "default", Priority: 0},
			"batch":     {Name: "batch", Priority: -5},
			"urgent":    {Name: "urgent", Priority: 100},
			"scoped":    {Name: "scoped", Priority: 7, Services: []string{"10.0.0.9:80", classSvc}},
			"elsewhere": {Name: "elsewhere", Priority: 7, Services: []string{"10.0.0.9:80"}},
		},
		Tenants: map[string]cmn.AIClassPolicy{
			"premium":   {PriorityClass: cmn.AIClassCritical, AllowedObjectives: []string{"batch", "elsewhere", "ghost", "realtime", "scoped"}},
			"capped":    {PriorityClass: cmn.AIClassStandard, AllowedObjectives: []string{"batch", "realtime"}, DefaultObjective: "realtime"},
			"free":      {PriorityClass: cmn.AIClassSheddable, DefaultObjective: "batch"},
			"vip":       {PriorityClass: cmn.AIClassCritical, DefaultObjective: "realtime", AllowedObjectives: []string{"batch", "realtime", "tier:flex", "urgent"}},
			"plain":     {DefaultObjective: "elsewhere"},
			"tiered":    {PriorityClass: cmn.AIClassCritical, AllowedObjectives: []string{"tier:flex", "tier:priority"}},
			"unclassed": {AllowedObjectives: []string{"realtime", "tier:fast", "tier:priority"}, DefaultObjective: "realtime"},
		},
		Keys: map[string]cmn.AIClassPolicy{
			"k-shed":    {PriorityClass: cmn.AIClassSheddable},
			"k-narrow":  {AllowedObjectives: []string{"batch", "urgent-not-the-tenants"}, DefaultObjective: "batch"},
			"k-default": {DefaultObjective: "urgent-not-the-tenants"},
			"k-raise":   {PriorityClass: cmn.AIClassCritical},
			"k-flex":    {AllowedObjectives: []string{"batch", "tier:flex"}, DefaultObjective: "batch"},
		},
	}
}

func TestClassResolve(t *testing.T) {
	snap := classSnap()
	cases := []struct {
		name string
		in   classInput
		want classAnswer
	}{
		// ---- nothing asked
		{"a tenant the store says nothing about is standard",
			classInput{tenant: "nobody"}, classAnswer{}},
		{"the default objective decides when nothing is asked",
			classInput{tenant: "vip"}, classAnswer{priority: 10, class: aiClassNumCritical, objective: "realtime"}},
		{"a default objective that does not apply to this service is no default",
			classInput{tenant: "plain"}, classAnswer{}},

		// ---- the header hint
		{"an allowed objective is applied",
			classInput{tenant: "premium", hint: "realtime"}, classAnswer{priority: 10, class: aiClassNumCritical, objective: "realtime"}},
		{"a hint can lower",
			classInput{tenant: "vip", hint: "batch"}, classAnswer{priority: -5, class: aiClassNumSheddable, objective: "batch"}},
		{"an objective scoped to this service is applied",
			classInput{tenant: "premium", hint: "scoped"}, classAnswer{priority: 7, class: aiClassNumCritical, objective: "scoped"}},
		{"an objective scoped to another service is denied",
			classInput{tenant: "premium", hint: "elsewhere"}, classAnswer{denied: true}},
		{"an allowed name with no objective row is denied",
			classInput{tenant: "premium", hint: "ghost"}, classAnswer{denied: true}},
		{"an unknown name is denied",
			classInput{tenant: "premium", hint: "no-such"}, classAnswer{denied: true}},
		{"a raise the tenant may not ask for is denied, and the default applies",
			classInput{tenant: "capped", hint: "urgent"}, classAnswer{priority: 0, class: aiClassNumStandard, denied: true, objective: "realtime"}},
		{"a tenant with no list may raise to nothing",
			classInput{tenant: "free", hint: "realtime"}, classAnswer{priority: -5, class: aiClassNumSheddable, denied: true, objective: "batch"}},
		{"a tenant with no row may raise to nothing",
			classInput{tenant: "nobody", hint: "realtime"}, classAnswer{denied: true}},
		{"names are compared exactly",
			classInput{tenant: "premium", hint: "Realtime"}, classAnswer{denied: true}},

		// ---- the cap: min(asked, cap)
		{"a standard cap holds a critical objective at standard",
			classInput{tenant: "capped", hint: "realtime"}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "realtime"}},
		{"a standard cap holds the default objective too",
			classInput{tenant: "capped"}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "realtime"}},
		{"a cap does not raise what is below it",
			classInput{tenant: "capped", hint: "batch"}, classAnswer{priority: -5, class: aiClassNumSheddable, objective: "batch"}},
		{"a sheddable cap makes every request sheddable",
			classInput{tenant: "free", tier: aiTierPriority}, classAnswer{priority: -5, class: aiClassNumSheddable, denied: true, objective: "batch"}},
		{"a critical cap is no cap",
			classInput{tenant: "vip", hint: "urgent"}, classAnswer{priority: 100, class: aiClassNumCritical, objective: "urgent"}},
		{"the key's cap lowers the tenant's",
			classInput{tenant: "vip", keyID: "k-shed", hint: "urgent"}, classAnswer{priority: -1, class: aiClassNumSheddable, objective: "urgent"}},
		{"a key cannot raise its tenant's cap",
			classInput{tenant: "capped", keyID: "k-raise", hint: "realtime"}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "realtime"}},
		{"the binding's class is a cap",
			classInput{tenant: "vip", bindingClass: cmn.AIClassStandard}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "realtime"}},
		{"the profile's class is a cap",
			classInput{tenant: "vip", profileClass: cmn.AIClassSheddable}, classAnswer{priority: -1, class: aiClassNumSheddable, objective: "realtime"}},
		{"the lowest of the four decides",
			classInput{tenant: "vip", keyID: "k-raise", bindingClass: cmn.AIClassStandard, profileClass: cmn.AIClassSheddable, hint: "urgent"},
			classAnswer{priority: -1, class: aiClassNumSheddable, objective: "urgent"}},
		{"an identity that sets no class is held at standard: its allowed objective",
			classInput{tenant: "unclassed", hint: "realtime"}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "realtime"}},
		{"an identity that sets no class is held at standard: its default objective",
			classInput{tenant: "unclassed"}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "realtime"}},
		{"an identity that sets no class is held at standard: an allowed tier",
			classInput{tenant: "unclassed", tier: aiTierPriority}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "tier:priority"}},
		{"a key's critical does not raise a tenant that sets no class",
			classInput{tenant: "unclassed", keyID: "k-raise", hint: "realtime"}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "realtime"}},
		{"a binding's critical does not raise a tenant that sets no class",
			classInput{tenant: "unclassed", bindingClass: cmn.AIClassCritical, tier: aiTierFast}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "tier:fast"}},
		{"a profile's critical does not raise a tenant that sets no class",
			classInput{tenant: "unclassed", profileClass: cmn.AIClassCritical, hint: "realtime"}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "realtime"}},
		{"a tenant with no row and a critical binding is standard",
			classInput{tenant: "nobody", bindingClass: cmn.AIClassCritical, profileClass: cmn.AIClassCritical}, classAnswer{}},
		{"a key lowers a tenant that sets no class",
			classInput{tenant: "unclassed", keyID: "k-shed", hint: "realtime"}, classAnswer{priority: -1, class: aiClassNumSheddable, objective: "realtime"}},
		{"a key of another tenant's snapshot row is not consulted without a key id",
			classInput{tenant: "vip", hint: "urgent"}, classAnswer{priority: 100, class: aiClassNumCritical, objective: "urgent"}},

		// ---- a key narrows its tenant
		{"a key's list narrows the tenant's",
			classInput{tenant: "vip", keyID: "k-narrow", hint: "realtime"}, classAnswer{priority: -5, class: aiClassNumSheddable, denied: true, objective: "batch"}},
		{"a key's list cannot add a name the tenant's lacks",
			classInput{tenant: "vip", keyID: "k-narrow", hint: "urgent-not-the-tenants"}, classAnswer{priority: -5, class: aiClassNumSheddable, denied: true, objective: "batch"}},
		{"a key's default replaces the tenant's when the tenant allows it",
			classInput{tenant: "vip", keyID: "k-narrow"}, classAnswer{priority: -5, class: aiClassNumSheddable, objective: "batch"}},
		{"a key's default the tenant does not allow is not used",
			classInput{tenant: "vip", keyID: "k-default"}, classAnswer{priority: 10, class: aiClassNumCritical, objective: "realtime"}},

		// ---- the body's tier: a hint under the same list as the header's
		{"a tier the identity may not use is denied, as a header would be",
			classInput{tenant: "nobody", tier: aiTierPriority}, classAnswer{denied: true}},
		{"a tier the identity may not use falls to its default",
			classInput{tenant: "capped", tier: aiTierFast}, classAnswer{priority: 0, class: aiClassNumStandard, denied: true, objective: "realtime"}},
		{"flex, allowed, is sheddable",
			classInput{tenant: "tiered", tier: aiTierFlex}, classAnswer{priority: -1, class: aiClassNumSheddable, objective: "tier:flex"}},
		{"priority, allowed, is critical",
			classInput{tenant: "tiered", tier: aiTierPriority}, classAnswer{priority: 1, class: aiClassNumCritical, objective: "tier:priority"}},
		{"fast is its own name: tier:priority does not allow it",
			classInput{tenant: "tiered", tier: aiTierFast}, classAnswer{denied: true}},
		{"a tier wins over the default objective",
			classInput{tenant: "vip", tier: aiTierFlex}, classAnswer{priority: -1, class: aiClassNumSheddable, objective: "tier:flex"}},
		{"a key's list narrows the tiers too: flex is above the key's batch",
			classInput{tenant: "vip", keyID: "k-narrow", tier: aiTierFlex}, classAnswer{priority: -5, class: aiClassNumSheddable, denied: true, objective: "batch"}},
		{"the same key granted tier:flex: flex, no denied mark",
			classInput{tenant: "vip", keyID: "k-flex", tier: aiTierFlex}, classAnswer{priority: -1, class: aiClassNumSheddable, objective: "tier:flex"}},
		{"the header wins over the tier",
			classInput{tenant: "vip", hint: "realtime", tier: aiTierFlex}, classAnswer{priority: 10, class: aiClassNumCritical, objective: "realtime"}},
		{"a denied header is not rescued by the tier: the default applies",
			classInput{tenant: "capped", hint: "urgent", tier: aiTierFlex}, classAnswer{priority: 0, class: aiClassNumStandard, denied: true, objective: "realtime"}},
		{"an unknown tier value is no hint",
			classInput{tenant: "vip", tier: 99}, classAnswer{priority: 10, class: aiClassNumCritical, objective: "realtime"}},

		// ---- a hint that asks for no more than the default needs no grant
		{"flex not granted is the request's: it asks for less",
			classInput{tenant: "capped", tier: aiTierFlex}, classAnswer{priority: -1, class: aiClassNumSheddable, objective: "tier:flex"}},
		{"flex from a tenant with no row is the request's",
			classInput{tenant: "nobody", tier: aiTierFlex}, classAnswer{priority: -1, class: aiClassNumSheddable, objective: "tier:flex"}},
		{"a header below the default, not granted, is the request's",
			classInput{tenant: "vip", hint: "scoped"}, classAnswer{priority: 7, class: aiClassNumCritical, objective: "scoped"}},
		{"a header equal to the default as held by the cap, not granted, is the request's",
			classInput{tenant: "capped", hint: "default"}, classAnswer{priority: 0, class: aiClassNumStandard, objective: "default"}},
		{"a header equal to the default, not granted, is the request's",
			classInput{tenant: "free", hint: "batch"}, classAnswer{priority: -5, class: aiClassNumSheddable, objective: "batch"}},
		{"the default is compared as the cap holds it: a tier above that is a raise",
			classInput{tenant: "capped", tier: aiTierPriority}, classAnswer{priority: 0, class: aiClassNumStandard, denied: true, objective: "realtime"}},
		{"a lowering header is still an objective: an unknown name is denied",
			classInput{tenant: "capped", hint: "no-such"}, classAnswer{priority: 0, class: aiClassNumStandard, denied: true, objective: "realtime"}},
		{"a lowering header is still scoped: another service's is denied",
			classInput{tenant: "vip", hint: "elsewhere"}, classAnswer{priority: 10, class: aiClassNumCritical, denied: true, objective: "realtime"}},
	}
	for _, c := range cases {
		c.in.svc = classSvc
		if got := classResolve(snap, c.in); got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// With nothing in the store the binding's and the profile's caps still
// apply, and nothing else does.
func TestClassResolveWithoutSnapshot(t *testing.T) {
	if got := classResolve(nil, classInput{tenant: "t", svc: classSvc}); got != (classAnswer{}) {
		t.Errorf("no snapshot, nothing asked = %+v, want the zero answer", got)
	}
	got := classResolve(nil, classInput{tenant: "t", svc: classSvc, tier: aiTierFast, bindingClass: cmn.AIClassCritical})
	if want := (classAnswer{denied: true}); got != want {
		t.Errorf("no snapshot, a tier under a critical binding = %+v, want %+v: no list allows it", got, want)
	}
	if got := classResolve(nil, classInput{tenant: "t", svc: classSvc, bindingClass: cmn.AIClassSheddable}); got.class != aiClassNumSheddable || got.priority != -1 {
		t.Errorf("no snapshot, a sheddable binding with nothing asked = %+v: a sheddable cap makes every request sheddable", got)
	}
	if got := classResolve(nil, classInput{tenant: "t", svc: classSvc, hint: "realtime"}); !got.denied || got.priority != 0 {
		t.Errorf("no snapshot, a header hint = %+v, want denied at standard", got)
	}
	got = classResolve(nil, classInput{tenant: "t", svc: classSvc, tier: aiTierFlex})
	if want := (classAnswer{priority: -1, class: aiClassNumSheddable, objective: "tier:flex"}); got != want {
		t.Errorf("no snapshot, flex = %+v, want %+v: asking for less needs no list", got, want)
	}
}

// A hint at or below what the request gets without one is never denied and
// is applied, by the header and by the body, whatever the lists say: the
// lists govern raises only.
func TestClassLoweringHintIsNeverDenied(t *testing.T) {
	snap := classSnap()
	snap.Tenants["nobody"] = cmn.AIClassPolicy{}
	hints := []string{"", "realtime", "default", "batch", "urgent", "scoped"}
	n, unlisted := 0, 0
	for tenant := range snap.Tenants {
		for _, keyID := range []string{"", "k-shed", "k-narrow", "k-default", "k-raise", "k-flex"} {
			for _, bind := range []string{"", cmn.AIClassStandard, cmn.AIClassCritical, cmn.AIClassSheddable} {
				base := classResolve(snap, classInput{tenant: tenant, keyID: keyID, svc: classSvc, bindingClass: bind})
				for _, hint := range hints {
					for tier := aiTierNone; tier <= aiTierFast; tier++ {
						asks := int32(0)
						switch {
						case hint != "":
							asks = snap.Objectives[hint].Priority
						case tier != aiTierNone:
							asks = aiTierPriorityOf(tier)
						default:
							continue
						}
						if asks > base.priority {
							continue
						}
						n++
						name := hint
						if name == "" {
							name = cmn.AITierObjectivePrefix + tierName(tier)
						}
						tp, kp := snap.Tenants[tenant], snap.Keys[keyID]
						if !inList(tp.AllowedObjectives, name) || (len(kp.AllowedObjectives) > 0 && !inList(kp.AllowedObjectives, name)) {
							unlisted++
						}
						got := classResolve(snap, classInput{tenant: tenant, keyID: keyID, svc: classSvc, hint: hint, tier: tier, bindingClass: bind})
						if got.denied || got.priority != asks {
							t.Errorf("tenant %s key %q binding %q hint %q tier %d asks %d at or below %d: got %+v, want applied, not denied",
								tenant, keyID, bind, hint, tier, asks, base.priority, got)
						}
					}
				}
			}
		}
	}
	if n == 0 || unlisted == 0 {
		t.Fatalf("lowering hints tried %d, of them not in the lists %d: both must be above 0", n, unlisted)
	}
}

// A hint never raises a request above its identity's cap, whatever is asked
// and by whichever channel, and an identity that sets no class is never
// above standard.
func TestClassHintNeverRaises(t *testing.T) {
	snap := classSnap()
	snap.Tenants["nobody"] = cmn.AIClassPolicy{}
	hints := []string{"", "realtime", "batch", "urgent", "scoped", "elsewhere", "ghost", "no-such"}
	for tenant := range snap.Tenants {
		for _, keyID := range []string{"", "k-shed", "k-narrow", "k-default", "k-raise", "k-flex"} {
			for _, bind := range []string{"", cmn.AIClassStandard, cmn.AIClassCritical, cmn.AIClassSheddable} {
				// The tenant grants, the others only lower: a tenant
				// that sets no class grants standard.
				limit := int32(0)
				if snap.Tenants[tenant].PriorityClass == cmn.AIClassCritical {
					limit = int32(1 << 30)
				}
				for _, c := range []string{snap.Tenants[tenant].PriorityClass, snap.Keys[keyID].PriorityClass, bind} {
					switch c {
					case cmn.AIClassStandard:
						limit = min(limit, 0)
					case cmn.AIClassSheddable:
						limit = min(limit, -1)
					}
				}
				for _, hint := range hints {
					for tier := aiTierNone; tier <= aiTierFast; tier++ {
						got := classResolve(snap, classInput{tenant: tenant, keyID: keyID, svc: classSvc, hint: hint, tier: tier, bindingClass: bind})
						if got.priority > limit {
							t.Errorf("tenant %s key %q binding %q hint %q tier %d: priority %d above the cap %d",
								tenant, keyID, bind, hint, tier, got.priority, limit)
						}
						if got.class != classNumOf(got.priority) {
							t.Errorf("class %d is not the sign of priority %d", got.class, got.priority)
						}
					}
				}
			}
		}
	}
}

func TestClassNumbersMatchNames(t *testing.T) {
	for prio, want := range map[int32]string{5: cmn.AIClassCritical, 0: cmn.AIClassStandard, -3: cmn.AIClassSheddable} {
		if got := cmn.AIClassOfPriority(prio); got != want {
			t.Errorf("class of %d = %s, want %s", prio, got, want)
		}
		if got := aiClassName(classNumOf(prio)); got != want {
			t.Errorf("name of class number of %d = %s, want %s", prio, got, want)
		}
	}
}
