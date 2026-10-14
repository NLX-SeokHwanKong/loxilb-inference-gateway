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
//	         may use it; else the body's tier hint; else the identity's
//	         default objective; else 0.
//	cap      the lowest class among the tenant's, the key's, the binding's
//	         and the JWT profile's priority_class. Unset is no cap.
//	result   min(asked, cap). A hint can lower a request, never raise it
//	         above what its identity allows.
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
func capOfClass(class string) int32 {
	switch class {
	case cmn.AIClassStandard:
		return 0
	case cmn.AIClassSheddable:
		return -1
	}
	return math.MaxInt32 // critical, or unset: no cap
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

	// The cap: the lowest of the four.
	limit := capOfClass(tenantPol.PriorityClass)
	for _, c := range []string{keyPol.PriorityClass, in.bindingClass, in.profileClass} {
		if v := capOfClass(c); v < limit {
			limit = v
		}
	}

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

	var ans classAnswer
	asked := int32(0)
	decided := false
	if in.hint != "" {
		if o, ok := objectiveFor(snap, in.hint, in.svc); ok && allowed(in.hint) {
			asked, ans.objective, decided = o.Priority, o.Name, true
		} else {
			// Unknown, out of scope for this service, or not this
			// identity's to use: the request falls to its default, and
			// the record says it asked.
			ans.denied = true
		}
	}
	if !decided && !ans.denied {
		switch in.tier {
		case aiTierFlex:
			asked, ans.objective, decided = aiTierPrioritySheddable, "tier:"+tierName(in.tier), true
		case aiTierPriority, aiTierFast:
			asked, ans.objective, decided = aiTierPriorityCritical, "tier:"+tierName(in.tier), true
		}
	}
	if !decided {
		if o, ok := objectiveFor(snap, deflt, in.svc); ok {
			asked, ans.objective = o.Priority, o.Name
		}
	}

	ans.priority = asked
	if ans.priority > limit {
		ans.priority = limit
	}
	ans.class = classNumOf(ans.priority)
	return ans
}
