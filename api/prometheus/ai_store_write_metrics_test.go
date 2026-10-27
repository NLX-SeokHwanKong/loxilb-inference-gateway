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

package prometheus

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The collector emits one counter series per plane the source reports, and
// nothing with no source set.
func TestAIStoreWriteOutcomeCollectorEmitsOneSeriesPerPlane(t *testing.T) {
	gather := func() map[string]float64 {
		reg := prometheus.NewRegistry()
		reg.MustRegister(aiStoreWriteOutcomeCollector{})
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != "loxilb_ai_store_write_outcome_unknown_total" {
				continue
			}
			if mf.GetType() != dto.MetricType_COUNTER {
				t.Fatalf("type %v, want counter", mf.GetType())
			}
			for _, m := range mf.Metric {
				out[m.Label[0].GetValue()] = m.Counter.GetValue()
			}
		}
		return out
	}

	aiStoreWriteOutcomeSource.Store(nil)
	if got := gather(); len(got) != 0 {
		t.Fatalf("with no source: %v", got)
	}

	SetAIStoreWriteOutcomeSource(func() AIStoreWriteOutcomeSnapshot {
		return AIStoreWriteOutcomeSnapshot{UnknownByPlane: map[string]uint64{"aikey": 2, "mgmt": 0}}
	})
	t.Cleanup(func() { aiStoreWriteOutcomeSource.Store(nil) })
	got := gather()
	if len(got) != 2 || got["aikey"] != 2 || got["mgmt"] != 0 {
		t.Fatalf("series: %v", got)
	}
}
