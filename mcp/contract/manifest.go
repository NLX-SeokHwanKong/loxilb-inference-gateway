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

package contract

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tier is the authorization a mutation needs through the bridge.
type Tier string

// Tiers in increasing strength. admin-confirm means the admin role AND a
// redeemed confirm token.
const (
	TierViewer       Tier = "viewer"
	TierOperator     Tier = "operator"
	TierAdminConfirm Tier = "admin-confirm"
)

// Rank orders tiers; a higher rank is a stronger gate.
func (t Tier) Rank() int {
	switch t {
	case TierViewer:
		return 0
	case TierOperator:
		return 1
	case TierAdminConfirm:
		return 2
	}
	return -1
}

// Impact is the blast radius of an operation.
type Impact string

// Impact values (CAMPAIGN-GUIDE §3.1).
const (
	ImpactNone     Impact = "none"     // read
	ImpactObject   Impact = "object"   // one rule / key / user entry
	ImpactTenant   Impact = "tenant"   // one tenant's limits or keys
	ImpactService  Impact = "service"  // a rule's traffic
	ImpactGlobal   Impact = "global"   // every tenant or service
	ImpactInstance Impact = "instance" // restore, delete-all, maintenance, metrics, audit
)

var impacts = map[Impact]bool{
	ImpactNone: true, ImpactObject: true, ImpactTenant: true,
	ImpactService: true, ImpactGlobal: true, ImpactInstance: true,
}

// Valid reports whether the impact is one of the defined values.
func (i Impact) Valid() bool { return impacts[i] }

// Mutating reports whether the impact describes a change.
func (i Impact) Mutating() bool { return i != ImpactNone }

// NeverAutopilot reports whether the derivation table forbids autopilot
// for this impact regardless of configuration.
func (i Impact) NeverAutopilot() bool { return i == ImpactGlobal || i == ImpactInstance }

// MinTier is the derivation rule of §3.1: the weakest tier a manifest entry
// may declare for an impact and reversibility.
func MinTier(impact Impact, reversible bool) Tier {
	switch impact {
	case ImpactNone:
		return TierViewer
	case ImpactObject, ImpactTenant:
		if reversible {
			return TierOperator
		}
		return TierAdminConfirm
	default:
		return TierAdminConfirm
	}
}

// Phases a pending entry may name.
var Phases = map[string]bool{"P0": true, "P1a": true, "P1b": true, "P2": true, "P3": true, "P4": true}

// Entry is one manifest row. Exactly one of Tools, Pending, Excluded is set.
type Entry struct {
	Op         string   `yaml:"op"`
	Tools      []string `yaml:"tools,omitempty"`
	Impact     Impact   `yaml:"impact,omitempty"`
	Reversible *bool    `yaml:"reversible,omitempty"`
	Tier       Tier     `yaml:"tier,omitempty"`
	TierGap    string   `yaml:"tier_gap,omitempty"`
	Fields     []string `yaml:"fields,omitempty"`
	Pending    string   `yaml:"pending,omitempty"`
	Excluded   string   `yaml:"excluded,omitempty"`
	Note       string   `yaml:"note,omitempty"`
}

// State classifies an entry.
func (e Entry) State() string {
	switch {
	case len(e.Tools) > 0:
		return "mapped"
	case e.Pending != "":
		return "pending"
	case e.Excluded != "":
		return "excluded"
	}
	return "invalid"
}

// Method and Path split the op key.
func (e Entry) Method() string { m, _, _ := strings.Cut(e.Op, " "); return m }
func (e Entry) Path() string   { _, p, _ := strings.Cut(e.Op, " "); return p }

// EffectiveTier is the declared tier, defaulting to viewer for reads.
func (e Entry) EffectiveTier() Tier {
	if e.Tier == "" && e.Impact == ImpactNone {
		return TierViewer
	}
	return e.Tier
}

// Budget holds the ratchets.
type Budget struct {
	PendingMax int `yaml:"pending_max"`
	TierGapMax int `yaml:"tier_gap_max"`
}

// Manifest is coverage.yml.
type Manifest struct {
	Version    int     `yaml:"version"`
	Budget     Budget  `yaml:"budget"`
	Operations []Entry `yaml:"operations"`
}

var opKeyRe = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) /[A-Za-z0-9_{}./-]+$`)

// LoadManifest parses coverage.yml and checks each entry's shape. Semantic
// checks against swagger and the registered tools are the tests' job.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseManifest(raw)
}

// ParseManifest is LoadManifest on bytes.
func ParseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("version %d: want 1", m.Version)
	}
	seen := map[string]bool{}
	for i, e := range m.Operations {
		if !opKeyRe.MatchString(e.Op) {
			return nil, fmt.Errorf("operations[%d]: op %q is not \"METHOD /path\"", i, e.Op)
		}
		if seen[e.Op] {
			return nil, fmt.Errorf("%s: listed twice", e.Op)
		}
		seen[e.Op] = true
		if err := e.check(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Op, err)
		}
	}
	return &m, nil
}

func (e Entry) check() error {
	states := 0
	if len(e.Tools) > 0 {
		states++
	}
	if e.Pending != "" {
		states++
	}
	if e.Excluded != "" {
		states++
	}
	if states != 1 {
		return fmt.Errorf("exactly one of tools:, pending:, excluded: is required")
	}
	if e.Impact != "" && !e.Impact.Valid() {
		return fmt.Errorf("impact %q is not one of none|object|tenant|service|global|instance", e.Impact)
	}
	if e.Tier != "" && e.Tier.Rank() < 0 {
		return fmt.Errorf("tier %q is not one of viewer|operator|admin-confirm", e.Tier)
	}
	switch e.State() {
	case "mapped":
		if e.Impact == "" {
			return fmt.Errorf("a mapped operation needs impact:")
		}
		if e.Impact.Mutating() {
			if e.Tier == "" {
				return fmt.Errorf("a mutation needs tier:")
			}
			if e.Reversible == nil && e.Impact != ImpactNone {
				return fmt.Errorf("a mutation needs reversible: true|false")
			}
		} else if e.Reversible != nil {
			return fmt.Errorf("reversible: is meaningless on a read")
		}
	case "pending":
		if !Phases[e.Pending] {
			return fmt.Errorf("pending %q is not one of P0|P1a|P1b|P2|P3|P4", e.Pending)
		}
		if e.Tier != "" || e.TierGap != "" || len(e.Fields) > 0 {
			return fmt.Errorf("a pending operation carries no tier:, tier_gap: or fields:")
		}
	case "excluded":
		if e.Impact != "" || e.Tier != "" || e.TierGap != "" || len(e.Fields) > 0 || e.Reversible != nil {
			return fmt.Errorf("an excluded operation carries only excluded: and note:")
		}
	}
	return nil
}

// Derived returns the minimum tier the rule requires for a mapped mutation.
func (e Entry) Derived() Tier {
	rev := e.Reversible != nil && *e.Reversible
	return MinTier(e.Impact, rev)
}

// WeakerThanRule reports whether the declared tier is below the derived one.
func (e Entry) WeakerThanRule() bool {
	if e.State() != "mapped" {
		return false
	}
	return e.EffectiveTier().Rank() < e.Derived().Rank()
}

// Counts summarises a manifest for the CI step summary.
type Counts struct {
	Mapped, Pending, Excluded, TierGaps int
	PendingByPhase                      map[string]int
	// MutationTiers counts mapped mutations by "impact/tier".
	MutationTiers map[string]int
}

// Count tallies the manifest.
func (m *Manifest) Count() Counts {
	c := Counts{PendingByPhase: map[string]int{}, MutationTiers: map[string]int{}}
	for _, e := range m.Operations {
		switch e.State() {
		case "mapped":
			c.Mapped++
			if e.Impact.Mutating() {
				c.MutationTiers[string(e.Impact)+"/"+string(e.EffectiveTier())]++
			}
			if e.TierGap != "" {
				c.TierGaps++
			}
		case "pending":
			c.Pending++
			c.PendingByPhase[e.Pending]++
		case "excluded":
			c.Excluded++
		}
	}
	return c
}
