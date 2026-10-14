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

package common

import "time"

// Priority classes of an AI request. A class is the sign of a priority: an
// objective with a positive priority is critical, a negative one sheddable,
// zero (and no objective at all) standard. The mapping is lossy on purpose:
// the order between classes is kept, the order between two positive
// integers is not.
const (
	AIClassStandard  = "standard"
	AIClassCritical  = "critical"
	AIClassSheddable = "sheddable"
)

// Limits on an objective, enforced at the API.
const (
	// AIObjectiveNameMax is the longest objective name: it travels in a
	// request header, a response header and a 64-byte field of the data
	// plane's decision.
	AIObjectiveNameMax = 63
	// AIObjectiveServicesMax bounds the services one objective names.
	AIObjectiveServicesMax = 256
	// AIObjectiveListMax bounds a tenant's or key's allowed_objectives.
	AIObjectiveListMax = 64
)

// AIObjectiveEntry is one row of the objective table: a named priority,
// for every service or for the ones it lists ("VIP:port").
type AIObjectiveEntry struct {
	Name      string    `json:"name"`
	Services  []string  `json:"services,omitempty"`
	Priority  int32     `json:"priority"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AIClassPolicy is what a tenant row, an API key, a JWT profile or a
// network-identity binding says about the class of its requests.
//
// PriorityClass is a cap: the highest class a request of this identity may
// have. Empty means no cap. DefaultObjective is the objective a request
// gets when it names none (or one it may not use); AllowedObjectives are
// the objective names a request may ask for by header. An empty list
// allows none.
type AIClassPolicy struct {
	PriorityClass     string   `json:"priority_class,omitempty"`
	DefaultObjective  string   `json:"default_objective,omitempty"`
	AllowedObjectives []string `json:"allowed_objectives,omitempty"`
}

// IsZero reports whether the policy says nothing.
func (p AIClassPolicy) IsZero() bool {
	return p.PriorityClass == "" && p.DefaultObjective == "" && len(p.AllowedObjectives) == 0
}

// AIClassPatch is a change to a class policy: a nil field is left as it is,
// a field that is present replaces the stored one (an empty value clears
// it).
type AIClassPatch struct {
	PriorityClass     *string
	DefaultObjective  *string
	AllowedObjectives *[]string
}

// IsZero reports whether the patch changes nothing.
func (c AIClassPatch) IsZero() bool {
	return c.PriorityClass == nil && c.DefaultObjective == nil && c.AllowedObjectives == nil
}

// Apply returns base with the patch's fields laid over it.
func (c AIClassPatch) Apply(base AIClassPolicy) AIClassPolicy {
	if c.PriorityClass != nil {
		base.PriorityClass = *c.PriorityClass
	}
	if c.DefaultObjective != nil {
		base.DefaultObjective = *c.DefaultObjective
	}
	if c.AllowedObjectives != nil {
		base.AllowedObjectives = append([]string(nil), (*c.AllowedObjectives)...)
	}
	return base
}

// AIClassOfPriority maps a priority to its class.
func AIClassOfPriority(priority int32) string {
	switch {
	case priority > 0:
		return AIClassCritical
	case priority < 0:
		return AIClassSheddable
	}
	return AIClassStandard
}

// Where the class configuration in force was read from.
const (
	// AIClassSourceStore: the key store answered the last read.
	AIClassSourceStore = "store"
	// AIClassSourceLKG: the store has not answered since the gateway
	// started; the last-known-good file is being served.
	AIClassSourceLKG = "lkg"
	// AIClassSourceNone: nothing was ever read (no store, or a store that
	// has not answered and no file).
	AIClassSourceNone = "none"
)
