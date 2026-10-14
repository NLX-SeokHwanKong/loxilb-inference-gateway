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
	"math"

	"github.com/loxilb-io/loxilb/pkg/aikey"

	cmn "github.com/loxilb-io/loxilb/common"
)

// The class of a request.
//
// A request's priority starts from what it asks for and is then held under
// what its identity is allowed:
//
//	asked    the objective named by the request's header, when the identity
//	         may use it; else the body's tier hint, when the identity may
//	         use it (its name is "tier:<value>" in allowed_objectives);
//	         else the identity's default objective; else 0. A hint that
//	         asks for no more than the request gets without one is always
//	         the request's: allowed_objectives governs raises only.
//	cap      the tenant's priority_class, standard when it sets none, then
//	         lowered by the key's, the JWT profile's and the binding's when
//	         they set one: those three can only lower what the tenant grants.
//	result   min(asked, cap). A hint can lower a request, never raise it
//	         above what its identity allows, by the header or by the body.
//
// The class is the sign of the result (cmn.AIClassOfPriority).

// Tier hints, as the data plane hands them over (the body's service_tier).
const (
	aiTierNone     = 0
	aiTierFlex     = 1 // "flex"
	aiTierPriority = 2 // "priority"
	aiTierFast     = 3 // "fast"
)

// Class numbers of the data plane. Zero is standard, so that zeroed memory
// is a request no class was resolved for.
const (
	aiClassNumStandard  = 0
	aiClassNumCritical  = 1
	aiClassNumSheddable = 2
)

// What a tier hint stands for, as a priority. A tier needs no objective row.
const (
	aiTierPriorityCritical  = 1
	aiTierPrioritySheddable = -1
)

// classInput is one request, as far as its class depends on it.
type classInput struct {
	tenant string
	keyID  string
	svc    string // the service, "VIP:port"
	hint   string // objective name from the request header, "" = none
	tier   int    // aiTier*
	// bindingClass and profileClass are the priority_class of the binding
	// that named the tenant and of the JWT profile that verified the token
	// ("" = none, or not that kind of request).
	bindingClass string
	profileClass string
}

// classAnswer is what the data plane is told.
type classAnswer struct {
	priority  int32
	class     int
	denied    bool   // the header named an objective the request may not use
	objective string // the hint that was applied: a name, "tier:<value>", or ""
}

// capOfClass turns a priority_class into the highest priority it admits.
// Unset admits anything here; identityCap decides what a tenant's unset is.
func capOfClass(class string) int32 {
	switch class {
	case cmn.AIClassStandard:
		return 0
	case cmn.AIClassSheddable:
		return -1
	}
	return math.MaxInt32 // critical, or unset
}

// identityCap is the cap of a request: the tenant's class, standard when the
// tenant sets none, lowered by every other class that is set. Only a tenant
// the operator granted critical can have critical requests; a key, a JWT
// profile or a binding narrows its tenant and never raises it.
func identityCap(tenantClass string, lowering ...string) int32 {
	limit := int32(0)
	if tenantClass != "" {
		limit = capOfClass(tenantClass)
	}
	for _, c := range lowering {
		if v := capOfClass(c); v < limit {
			limit = v
		}
	}
	return limit
}

func classNumOf(priority int32) int {
	switch {
	case priority > 0:
		return aiClassNumCritical
	case priority < 0:
		return aiClassNumSheddable
	}
	return aiClassNumStandard
}

// aiClassName is the class's name on the records and the API.
func aiClassName(class int) string {
	switch class {
	case aiClassNumCritical:
		return cmn.AIClassCritical
	case aiClassNumSheddable:
		return cmn.AIClassSheddable
	}
	return cmn.AIClassStandard
}

// aiTierPriorityOf is the priority a tier hint stands for.
func aiTierPriorityOf(tier int) int32 {
	if tier == aiTierFlex {
		return aiTierPrioritySheddable
	}
	return aiTierPriorityCritical
}

func tierName(tier int) string {
	switch tier {
	case aiTierFlex:
		return "flex"
	case aiTierPriority:
		return "priority"
	case aiTierFast:
		return "fast"
	}
	return ""
}

// objectiveFor returns the objective of that name when it exists and
// applies to the service.
func objectiveFor(snap *aikey.ClassSnapshot, name, svc string) (cmn.AIObjectiveEntry, bool) {
	if snap == nil || name == "" {
		return cmn.AIObjectiveEntry{}, false
	}
	o, ok := snap.Objectives[name]
	if !ok {
		return o, false
	}
	if len(o.Services) == 0 {
		return o, true
	}
	for _, s := range o.Services {
		if s == svc {
			return o, true
		}
	}
	return o, false
}

func inList(l []string, name string) bool {
	for _, n := range l {
		if n == name {
			return true
		}
	}
	return false
}

// classResolve decides one request. snap may be nil (nothing configured in
// the store): the caps of the binding and the profile still apply.
func classResolve(snap *aikey.ClassSnapshot, in classInput) classAnswer {
	var tenantPol, keyPol cmn.AIClassPolicy
	if snap != nil {
		tenantPol = snap.Tenants[in.tenant]
		if in.keyID != "" {
			keyPol = snap.Keys[in.keyID]
		}
	}

	// The cap: the lowest of the four that are set.
	limit := identityCap(tenantPol.PriorityClass, keyPol.PriorityClass, in.bindingClass, in.profileClass)

	// What the identity may ask for, and what it gets when it asks for
	// nothing. A key narrows its tenant: its list is used only for the
	// names the tenant's list also holds, and its default only when that
	// default is one of them.
	allowed := func(name string) bool {
		if !inList(tenantPol.AllowedObjectives, name) {
			return false
		}
		return len(keyPol.AllowedObjectives) == 0 || inList(keyPol.AllowedObjectives, name)
	}
	deflt := tenantPol.DefaultObjective
	if keyPol.DefaultObjective != "" && allowed(keyPol.DefaultObjective) {
		deflt = keyPol.DefaultObjective
	}

	// What the request gets when it asks for nothing. A hint at or below it
	// asks for less, which is no forgery: it needs no grant.
	base, baseName := int32(0), ""
	if o, ok := objectiveFor(snap, deflt, in.svc); ok {
		base, baseName = o.Priority, o.Name
	}
	baseHeld := min(base, limit)
	may := func(name string, priority int32) bool {
		return priority <= baseHeld || allowed(name)
	}

	var ans classAnswer
	asked := int32(0)
	decided := false
	if in.hint != "" {
		if o, ok := objectiveFor(snap, in.hint, in.svc); ok && may(in.hint, o.Priority) {
			asked, ans.objective, decided = o.Priority, o.Name, true
		} else {
			// Unknown, out of scope for this service, or a raise this
			// identity may not ask for: the request falls to its default,
			// and the record says it asked.
			ans.denied = true
		}
	}
	if !decided && !ans.denied && tierName(in.tier) != "" {
		// The body's tier is a hint like the header's, under the same rule:
		// a client does not get by the body what it may not ask for by name.
		name := cmn.AITierObjectivePrefix + tierName(in.tier)
		if may(name, aiTierPriorityOf(in.tier)) {
			asked, ans.objective, decided = aiTierPriorityOf(in.tier), name, true
		} else {
			ans.denied = true
		}
	}
	if !decided {
		asked, ans.objective = base, baseName
	}

	ans.priority = asked
	if ans.priority > limit {
		ans.priority = limit
	}
	ans.class = classNumOf(ans.priority)
	return ans
}
