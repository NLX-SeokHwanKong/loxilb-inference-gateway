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
	"testing"
	"time"
)

// TestRebindOpaqueIDMovesTheIndex: a replace that differs only in the id moves
// the rule to the new id - the old id no longer resolves, the new one does, the
// rule carries it, and lastUpdated moves - without any other state changing.
// This is the path a controller takes when it starts sending deterministic
// ids for rules it installed earlier under minted ones, or re-sends after a
// restart restored the rule under the id the snapshot kept.
func TestRebindOpaqueIDMovesTheIndex(t *testing.T) {
	R := newTestRuleH()
	r := &ruleEnt{}
	r.tuples = ruleTuples{path: "vip-a"}
	r.id = "44444444-4444-4444-8444-444444444444"
	R.registerOpaqueID(r)
	before := time.Now().Add(-time.Minute)
	r.lastUpdated = before

	want := "55555555-5555-5555-8555-555555555555"
	rebound, err := R.rebindOpaqueID(r, want)
	if err != nil {
		t.Fatalf("rebindOpaqueID: %v", err)
	}
	if !rebound {
		t.Fatalf("a differing id did not rebind")
	}
	if r.id != want {
		t.Errorf("rule id = %q, want %q", r.id, want)
	}
	if got := R.GetLBRuleByOpaqueID(want); got != r {
		t.Errorf("new id does not resolve to the rule (got %v)", got)
	}
	if got := R.GetLBRuleByOpaqueID("44444444-4444-4444-8444-444444444444"); got != nil {
		t.Errorf("old id still resolves (got %v)", got)
	}
	if len(R.opaqueID) != 1 {
		t.Errorf("index holds %d entries, want 1", len(R.opaqueID))
	}
	if !r.lastUpdated.After(before) {
		t.Errorf("lastUpdated did not move (%v)", r.lastUpdated)
	}
}

// TestRebindOpaqueIDSameOrEmptyIsNoop: a replace that carries no id, or the id
// the rule already has, changes nothing - that is the steady state of every
// controller re-sync and must stay free of churn.
func TestRebindOpaqueIDSameOrEmptyIsNoop(t *testing.T) {
	R := newTestRuleH()
	r := &ruleEnt{}
	r.tuples = ruleTuples{path: "vip-a"}
	r.id = "66666666-6666-4666-8666-666666666666"
	R.registerOpaqueID(r)
	stamp := time.Now().Add(-time.Minute)
	r.lastUpdated = stamp

	for _, supplied := range []string{"", r.id} {
		rebound, err := R.rebindOpaqueID(r, supplied)
		if err != nil {
			t.Fatalf("rebindOpaqueID(%q): %v", supplied, err)
		}
		if rebound {
			t.Errorf("rebindOpaqueID(%q) reported a rebinding", supplied)
		}
		if r.id != "66666666-6666-4666-8666-666666666666" || !r.lastUpdated.Equal(stamp) {
			t.Errorf("rebindOpaqueID(%q) changed the rule: id=%q lastUpdated=%v", supplied, r.id, r.lastUpdated)
		}
		if got := R.GetLBRuleByOpaqueID(r.id); got != r || len(R.opaqueID) != 1 {
			t.Errorf("rebindOpaqueID(%q) disturbed the index", supplied)
		}
	}
}

// TestRebindOpaqueIDRefusesAnotherRulesID: an id already bound to a different
// rule is a conflict, and the rule asking for it keeps the id it had - the
// same answer a create gets (lbrule-id conflict).
func TestRebindOpaqueIDRefusesAnotherRulesID(t *testing.T) {
	R := newTestRuleH()
	owner := &ruleEnt{}
	owner.tuples = ruleTuples{path: "vip-a"}
	owner.id = "77777777-7777-4777-8777-777777777777"
	R.registerOpaqueID(owner)
	other := &ruleEnt{}
	other.tuples = ruleTuples{path: "vip-b"}
	other.id = "88888888-8888-4888-8888-888888888888"
	R.registerOpaqueID(other)

	rebound, err := R.rebindOpaqueID(other, owner.id)
	if err == nil {
		t.Fatalf("an id bound to another rule was taken over")
	}
	if rebound {
		t.Errorf("a refused rebinding reported success")
	}
	if other.id != "88888888-8888-4888-8888-888888888888" {
		t.Errorf("the refused rule changed its id to %q", other.id)
	}
	if got := R.GetLBRuleByOpaqueID(owner.id); got != owner {
		t.Errorf("the owner lost its id (got %v)", got)
	}
	if got := R.GetLBRuleByOpaqueID(other.id); got != other {
		t.Errorf("the refused rule lost its own id (got %v)", got)
	}
}
