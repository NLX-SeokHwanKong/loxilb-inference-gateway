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

package prometheus

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// familySnapshot returns the child count and value sum of one registered
// family, so a test can assert that a call recorded nothing without minting
// the very child it is checking for.
func familySnapshot(t *testing.T, name string) (children int, sum float64) {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			children++
			sum += m.GetCounter().GetValue()
		}
	}
	return children, sum
}

// RecordPDDecodeSelected's path argument arrives from the C selector, and the
// lookup is the only thing keeping the path label a closed enum. Each valid
// encoding must land on exactly its own child; kv_fill must move only when
// the flag is set; and any other path value must record nothing in either
// family, including the fill.
func TestRecordPDDecodeSelected(t *testing.T) {
	model := "decodepathmodel"
	encodings := map[int]string{0: "hint", 1: "score", 2: "score_kv"}

	for path, label := range encodings {
		before := getCounterValue(aiPDDecodeSelectedTotal, label, model)
		fillBefore := getCounterValue(aiPDDecodeKVFillTotal, model)
		RecordPDDecodeSelected(model, path, false)
		if v := getCounterValue(aiPDDecodeSelectedTotal, label, model); v != before+1 {
			t.Fatalf("path %d: child %q = %f, want %f", path, label, v, before+1)
		}
		if v := getCounterValue(aiPDDecodeKVFillTotal, model); v != fillBefore {
			t.Fatalf("path %d without fill: kv_fill = %f, want %f", path, v, fillBefore)
		}
	}

	// The fill flag moves the second family by one, alongside the path child.
	scoreBefore := getCounterValue(aiPDDecodeSelectedTotal, "score", model)
	fillBefore := getCounterValue(aiPDDecodeKVFillTotal, model)
	RecordPDDecodeSelected(model, 1, true)
	if v := getCounterValue(aiPDDecodeSelectedTotal, "score", model); v != scoreBefore+1 {
		t.Fatalf("score with fill: child = %f, want %f", v, scoreBefore+1)
	}
	if v := getCounterValue(aiPDDecodeKVFillTotal, model); v != fillBefore+1 {
		t.Fatalf("score with fill: kv_fill = %f, want %f", v, fillBefore+1)
	}

	selChildren, selSum := familySnapshot(t, "loxilb_ai_pd_decode_selected_total")
	fillChildren, fillSum := familySnapshot(t, "loxilb_ai_pd_decode_kv_fill_total")

	for _, path := range []int{-1, 3, 15, 255} {
		RecordPDDecodeSelected(model, path, true)
	}

	if c, s := familySnapshot(t, "loxilb_ai_pd_decode_selected_total"); c != selChildren || s != selSum {
		t.Fatalf("unknown path encodings changed the selected family: children %d -> %d, sum %f -> %f",
			selChildren, c, selSum, s)
	}
	if c, s := familySnapshot(t, "loxilb_ai_pd_decode_kv_fill_total"); c != fillChildren || s != fillSum {
		t.Fatalf("unknown path encodings changed the kv_fill family: children %d -> %d, sum %f -> %f",
			fillChildren, c, fillSum, s)
	}
}

// pdDecodeChildren returns the registered children of the two decode
// selection families for one model label, keyed "family{path}" ("" path for
// kv_fill), as the gatherer sees them — a child that does not exist is simply
// absent, which is what the seeding test needs to tell apart from zero.
func pdDecodeChildren(t *testing.T, model string) map[string]float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]float64{}
	for _, mf := range mfs {
		name := mf.GetName()
		if name != "loxilb_ai_pd_decode_selected_total" && name != "loxilb_ai_pd_decode_kv_fill_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["model"] != model {
				continue
			}
			seen[name+"{"+labels["path"]+"}"] = m.GetCounter().GetValue()
		}
	}
	return seen
}

// MON-04: a model label's first decode selection must not be invisible to
// increase(). Registering a model (any P/D record does it) seeds the three
// path children and the kv_fill child at zero before anything increments
// them, so right after the first record on one path all four exist: that
// path at 1, the other three at 0. The model registry is process-global, so
// under -count=N the label is already registered on the repeats; the
// expectations are therefore relative to a snapshot taken before the record
// (absent == 0), and the "fresh label" case is asserted when it applies.
func TestPDDecodeSelectedSeededPerModel(t *testing.T) {
	model := "decodeseedmodel"
	children := []string{
		"loxilb_ai_pd_decode_selected_total{hint}",
		"loxilb_ai_pd_decode_selected_total{score}",
		"loxilb_ai_pd_decode_selected_total{score_kv}",
		"loxilb_ai_pd_decode_kv_fill_total{}",
	}
	before := pdDecodeChildren(t, model)
	fresh := len(before) == 0

	RecordPDDecodeSelected(model, 0, false) // registers the label on a fresh run; seeds the family

	after := pdDecodeChildren(t, model)
	if len(after) != len(children) {
		t.Fatalf("model %q has %d children, want %d: %v", model, len(after), len(children), after)
	}
	for _, k := range children {
		got, ok := after[k]
		if !ok {
			t.Fatalf("%s not seeded for model %q (seen %v)", k, model, after)
		}
		want := before[k] // 0 when the child did not exist before
		if k == children[0] {
			want++
		}
		if got != want {
			t.Fatalf("%s = %f, want %f (fresh label: %v)", k, got, want, fresh)
		}
		if fresh && k != children[0] && got != 0 {
			t.Fatalf("%s = %f on a fresh label, want 0", k, got)
		}
	}
}
