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

// The age bounds are read back out of the C header so a drift on either side
// fails here rather than on a dashboard.
func TestStreamAbandonBoundsMatchC(t *testing.T) {
	src, err := os.ReadFile("../../loxilb-ebpf/common/sockproxy_abandon.h")
	if err != nil {
		t.Skip("C header not available: ", err)
	}
	re := regexp.MustCompile(`(?s)abandon_age_bounds_s\[[^\]]*\]\s*=\s*\{([^}]*)\}`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatal("abandon_age_bounds_s not found")
	}
	var got []float64
	for _, f := range strings.Split(string(m[1]), ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			t.Fatalf("%q: %v", f, err)
		}
		got = append(got, v)
	}
	if len(got) != kvBounds {
		t.Fatalf("C has %d bounds, Go %d", len(got), kvBounds)
	}
	for i, v := range got {
		if v != abandonAgeBounds[i] {
			t.Errorf("bound %d: C %g, Go %g", i, v, abandonAgeBounds[i])
		}
	}
	// and the reason labels follow the enum's order
	for i, name := range []string{"ABANDON_CLIENT_GONE", "ABANDON_BACKEND_DIED", "ABANDON_STALL_ABORT"} {
		if !strings.Contains(string(src), name) {
			t.Errorf("enum %s (label %q) not in the header", name, abandonReasonLabels[i])
		}
	}
}

func TestStreamAbandonMergeMonotonic(t *testing.T) {
	var prev, raw streamAbandonSnapshot
	prev.total[0] = 10
	prev.age[0].count = 10
	prev.age[0].buckets[3] = 4
	raw.total[0] = 9 // torn: must not go backwards
	raw.age[0].count = 11
	raw.age[0].buckets[2], raw.age[0].buckets[3] = 5, 3 // torn inside one snapshot
	raw.stallAbortSec = 30
	m := mergeStreamAbandon(prev, raw)
	if m.total[0] != 10 {
		t.Fatalf("total went backwards: %d", m.total[0])
	}
	if m.age[0].buckets[3] != 5 || m.age[0].count != 11 {
		t.Fatalf("torn buckets not clamped: %v count %d", m.age[0].buckets, m.age[0].count)
	}
	if m.stallAbortSec != 30 {
		t.Fatalf("knob gauge not taken: %d", m.stallAbortSec)
	}
}

func TestStreamAbandonCollectorSeries(t *testing.T) {
	streamAbandonStoreMutex.Lock()
	saved := streamAbandonStore
	var s streamAbandonSnapshot
	s.total[0] = 7
	s.total[2] = 2
	s.age[0].buckets = [kvBounds]uint64{0, 1, 3, 6, 7, 7, 7, 7, 7, 7, 7, 7}
	s.age[0].count = 7
	s.age[0].sum = 61
	s.stallAbortSec = 45
	streamAbandonStore = s
	streamAbandonStoreMutex.Unlock()
	defer func() {
		streamAbandonStoreMutex.Lock()
		streamAbandonStore = saved
		streamAbandonStoreMutex.Unlock()
	}()

	reg := prometheus.NewRegistry()
	reg.MustRegister(streamAbandonCollector{})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"loxilb_proxy_stream_abandoned_total":     abandonReasons,
		"loxilb_proxy_stream_abandon_age_seconds": abandonReasons,
		"loxilb_proxy_sse_stall_abort_seconds":    1,
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
	if v := find("loxilb_proxy_stream_abandoned_total", map[string]string{"reason": "client_gone"}).GetCounter().GetValue(); v != 7 {
		t.Errorf("client_gone = %g, want 7", v)
	}
	if v := find("loxilb_proxy_stream_abandoned_total", map[string]string{"reason": "stall_abort"}).GetCounter().GetValue(); v != 2 {
		t.Errorf("stall_abort = %g, want 2", v)
	}
	h := find("loxilb_proxy_stream_abandon_age_seconds", map[string]string{"reason": "client_gone"}).GetHistogram()
	if h.GetSampleCount() != 7 || h.GetSampleSum() != 61 {
		t.Errorf("age histogram count/sum %d/%g", h.GetSampleCount(), h.GetSampleSum())
	}
	for _, b := range h.GetBucket() {
		if b.GetUpperBound() == 10 && b.GetCumulativeCount() != 6 {
			t.Errorf("le=10 holds %d, want 6", b.GetCumulativeCount())
		}
	}
	if v := find("loxilb_proxy_sse_stall_abort_seconds", map[string]string{}).GetGauge().GetValue(); v != 45 {
		t.Errorf("stall knob gauge = %g, want 45", v)
	}
}
