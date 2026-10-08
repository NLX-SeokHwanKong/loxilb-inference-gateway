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
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The stage bounds are the TTFB bounds and the block bounds are the C
// recorder's: both are read back out of the C source so a drift on either
// side fails here rather than on a dashboard.
func TestKvExactBoundsMatchC(t *testing.T) {
	src, err := os.ReadFile("../../loxilb-ebpf/common/sockproxy_metrics.c")
	if err != nil {
		t.Skip("C source not available: ", err)
	}
	read := func(name string) []float64 {
		re := regexp.MustCompile(`(?s)` + name + `\[[^\]]*\]\s*=\s*\{([^}]*)\}`)
		m := re.FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in sockproxy_metrics.c", name)
		}
		var out []float64
		for _, f := range strings.Split(string(m[1]), ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				t.Fatalf("%s: %q: %v", name, f, err)
			}
			out = append(out, v)
		}
		return out
	}
	us := read("latency_bucket_bounds_us")
	if len(us) != kvBounds {
		t.Fatalf("latency_bucket_bounds_us has %d bounds, want %d", len(us), kvBounds)
	}
	for i, v := range us {
		if v/1e6 != kvStageBounds[i] {
			t.Errorf("stage bound %d: C %g us, Go %g s", i, v, kvStageBounds[i])
		}
	}
	bl := read("kv_blocks_bucket_bounds")
	if len(bl) != kvBounds {
		t.Fatalf("kv_blocks_bucket_bounds has %d bounds, want %d", len(bl), kvBounds)
	}
	for i, v := range bl {
		if v != kvBlocksBounds[i] {
			t.Errorf("blocks bound %d: C %g, Go %g", i, v, kvBlocksBounds[i])
		}
	}
}

func TestKvExactMergeTornReadAndMonotonic(t *testing.T) {
	var prev, raw kvExactSnapshot
	prev.stage[2][1].buckets[0] = 3
	prev.stage[2][1].count = 5
	prev.blocks[1].sum = 40

	h := &raw.stage[2][1]
	h.buckets[0], h.buckets[1], h.buckets[2] = 4, 2, 6 // torn: 2 < 4
	h.count = 5                                        // below the highest bucket
	h.sum = 0.5
	raw.blocks[1].sum = 30 // cannot go backwards

	m := mergeKvExact(prev, raw)
	g := m.stage[2][1]
	if g.buckets[1] != 4 || g.buckets[2] != 6 || g.buckets[kvBounds-1] != 6 {
		t.Fatalf("torn buckets not clamped: %v", g.buckets)
	}
	if g.count != 6 {
		t.Fatalf("count %d, want at least the highest bucket (6)", g.count)
	}
	if m.blocks[1].sum != 40 {
		t.Fatalf("sum went backwards: %g", m.blocks[1].sum)
	}
	// a genuine reset (count below half the previous) replaces the state
	var reset kvExactSnapshot
	reset.stage[2][1].count = 1
	if r := mergeKvExact(m, reset); r.stage[2][1].count != 1 {
		t.Fatalf("reset not honoured: count %d", r.stage[2][1].count)
	}
}

func TestKvExactCollectorSeries(t *testing.T) {
	kvExactStoreMutex.Lock()
	saved := kvExactStore
	var s kvExactSnapshot
	s.stage[0][1].buckets = [kvBounds]uint64{1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 3, 3}
	s.stage[0][1].count = 4
	s.stage[0][1].sum = 0.0123
	s.blocks[0].buckets = [kvBounds]uint64{0, 0, 0, 0, 0, 0, 0, 0, 5, 5, 5, 5}
	s.blocks[0].count = 5
	s.blocks[0].sum = 1000
	s.blocks[1].buckets = [kvBounds]uint64{1, 1, 1, 1, 1, 1, 1, 1, 5, 5, 5, 5}
	s.blocks[1].count = 5
	s.blocks[1].sum = 600
	kvExactStore = s
	kvExactStoreMutex.Unlock()
	defer func() {
		kvExactStoreMutex.Lock()
		kvExactStore = saved
		kvExactStoreMutex.Unlock()
	}()

	reg := prometheus.NewRegistry()
	reg.MustRegister(kvExactCollector{})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"loxilb_kv_exact_stage_seconds":  kvStages * kvOutcomes,
		"loxilb_kv_exact_prompt_blocks":  1,
		"loxilb_kv_exact_overlap_blocks": 1,
	}
	byName := map[string]*dto.MetricFamily{}
	for _, mf := range mfs {
		byName[mf.GetName()] = mf
	}
	for name, n := range want {
		mf := byName[name]
		if mf == nil {
			t.Errorf("%s not collected", name)
			continue
		}
		if len(mf.GetMetric()) != n {
			t.Errorf("%s: %d series, want %d", name, len(mf.GetMetric()), n)
		}
	}
	if len(byName) != len(want) {
		t.Errorf("collected %d families, want %d", len(byName), len(want))
	}

	labels := func(m *dto.Metric) map[string]string {
		out := map[string]string{}
		for _, lp := range m.GetLabel() {
			out[lp.GetName()] = lp.GetValue()
		}
		return out
	}
	find := func(name string, lv map[string]string) *dto.Metric {
		for _, m := range byName[name].GetMetric() {
			if reflect.DeepEqual(labels(m), lv) {
				return m
			}
		}
		t.Fatalf("%s%v not found", name, lv)
		return nil
	}
	h := find("loxilb_kv_exact_stage_seconds",
		map[string]string{"stage": "tokenize", "outcome": "hit"}).GetHistogram()
	if h.GetSampleCount() != 4 || h.GetSampleSum() != 0.0123 {
		t.Errorf("stage histogram count/sum %d/%g", h.GetSampleCount(), h.GetSampleSum())
	}
	for _, b := range h.GetBucket() {
		if b.GetUpperBound() == 0.25 && b.GetCumulativeCount() != 3 {
			t.Errorf("le=0.25 holds %d, want 3", b.GetCumulativeCount())
		}
	}
	ov := find("loxilb_kv_exact_overlap_blocks", map[string]string{}).GetHistogram()
	pr := find("loxilb_kv_exact_prompt_blocks", map[string]string{}).GetHistogram()
	if ov.GetSampleSum()/pr.GetSampleSum() != 0.6 {
		t.Errorf("cached share = %g, want 0.6", ov.GetSampleSum()/pr.GetSampleSum())
	}
	for _, b := range ov.GetBucket() {
		if b.GetUpperBound() == 1 && b.GetCumulativeCount() != 1 {
			t.Errorf("overlap le=1 holds %d, want 1 (the no_worker miss)", b.GetCumulativeCount())
		}
	}
}
