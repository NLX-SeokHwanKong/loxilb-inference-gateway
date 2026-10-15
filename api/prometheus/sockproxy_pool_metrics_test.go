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

func TestPoolMergeFreedNeverGoesBack(t *testing.T) {
	prev := poolSnapshot{live: 5, freed: 10}
	m := mergePool(prev, poolSnapshot{live: 3, freed: 9}) // torn read
	if m.freed != 10 {
		t.Fatalf("freed went backwards: %d", m.freed)
	}
	if m.live != 3 {
		t.Fatalf("live is a gauge and is taken as read: %d", m.live)
	}
	m = mergePool(m, poolSnapshot{live: 4, freed: 12})
	if m.freed != 12 || m.live != 4 {
		t.Fatalf("next snapshot not taken: %+v", m)
	}
}

func TestPoolCollectorSeries(t *testing.T) {
	poolStoreMutex.Lock()
	saved := poolStore
	poolStore = poolSnapshot{live: 7, freed: 100}
	poolStoreMutex.Unlock()
	defer func() {
		poolStoreMutex.Lock()
		poolStore = saved
		poolStoreMutex.Unlock()
	}()

	reg := prometheus.NewRegistry()
	reg.MustRegister(poolCollector{})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("%s: %d series, want 1", mf.GetName(), len(mf.GetMetric()))
		}
		m := mf.GetMetric()[0]
		switch {
		case m.GetGauge() != nil:
			got[mf.GetName()] = m.GetGauge().GetValue()
		case m.GetCounter() != nil:
			got[mf.GetName()] = m.GetCounter().GetValue()
		}
	}
	if len(got) != 2 || got["loxilb_proxy_pools_live"] != 7 || got["loxilb_proxy_pools_freed_total"] != 100 {
		t.Fatalf("collected %v", got)
	}
}
