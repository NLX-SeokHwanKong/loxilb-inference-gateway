/*
 * Copyright (c) 2026 LoxiLB Authors
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
	"github.com/loxilb-io/loxilb/api/prometheus"
	"github.com/loxilb-io/loxilb/pkg/aikey"
)

// aiKeyLookupSnapshot is what the loxilb_ai_key_store_* families report: the
// key service's data-path lookup statistics, every result series present
// from the start so that a first event shows in increase().
func aiKeyLookupSnapshot() prometheus.AIKeyLookupSnapshot {
	return aiKeyLookupSnapshotOf(mh.AIKeyService.LookupStats())
}

func aiKeyLookupSnapshotOf(st aikey.LookupStats) prometheus.AIKeyLookupSnapshot {
	snap := prometheus.AIKeyLookupSnapshot{CircuitOpen: st.CircuitOpen, InFlight: st.InFlight}
	for r, name := range aikey.LookupResultNames {
		buckets := make(map[float64]uint64, len(aikey.LookupBucketBounds))
		for i, bound := range aikey.LookupBucketBounds {
			buckets[bound] = st.Buckets[r][i]
		}
		snap.Results = append(snap.Results, prometheus.AIKeyLookupResult{
			Result: name, Count: st.Count[r], Sum: st.Sum[r], Buckets: buckets,
		})
	}
	return snap
}

func init() {
	prometheus.SetAIKeyLookupSource(aiKeyLookupSnapshot)
}
