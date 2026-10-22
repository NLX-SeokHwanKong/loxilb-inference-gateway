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

func gatherPdOccupancy(t *testing.T) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(pdOccupancyCollector{})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		if len(mf.GetMetric()) != 1 || mf.GetMetric()[0].GetGauge() == nil {
			t.Fatalf("%s: want one gauge series", mf.GetName())
		}
		got[mf.GetName()] = mf.GetMetric()[0].GetGauge().GetValue()
	}
	return got
}

// The three are gauges of the load now: they follow each snapshot down to
// zero and up again, nothing is carried over.
func TestPdOccupancyFollowsTheSnapshot(t *testing.T) {
	pdOccupancyMutex.Lock()
	saved := pdOccupancyStore
	pdOccupancyMutex.Unlock()
	defer updatePdOccupancyStore(saved)

	for _, s := range []pdOccupancySnapshot{{6, 5, 3}, {0, 0, 0}, {2, 2, 0}} {
		updatePdOccupancyStore(s)
		got := gatherPdOccupancy(t)
		if len(got) != 3 || got["loxilb_pd_prefill_inflight"] != float64(s.prefill) ||
			got["loxilb_pd_decode_inflight"] != float64(s.decode) ||
			got["loxilb_pd_parked_connections"] != float64(s.parked) {
			t.Fatalf("after %+v collected %v", s, got)
		}
	}
}
