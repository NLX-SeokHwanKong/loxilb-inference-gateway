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
	"testing"

	"github.com/loxilb-io/loxilb/pkg/aikey"
)

// The scrape adapter seeds every result series from the start, so a first
// event shows in increase(), and carries the gauges through unchanged.
func TestAIKeyLookupSnapshotSeedsEveryResult(t *testing.T) {
	var st aikey.LookupStats
	st.CircuitOpen = true
	st.InFlight = 2
	st.Count[2] = 7
	st.Buckets[2][len(aikey.LookupBucketBounds)-1] = 7
	snap := aiKeyLookupSnapshotOf(st)
	if len(snap.Results) != len(aikey.LookupResultNames) {
		t.Fatalf("results = %d, want one per label value (%d)", len(snap.Results), len(aikey.LookupResultNames))
	}
	if !snap.CircuitOpen || snap.InFlight != 2 {
		t.Fatalf("gauges not carried: %+v", snap)
	}
	seen := map[string]bool{}
	for _, r := range snap.Results {
		seen[r.Result] = true
		if len(r.Buckets) != len(aikey.LookupBucketBounds) {
			t.Fatalf("%s: %d buckets, want %d", r.Result, len(r.Buckets), len(aikey.LookupBucketBounds))
		}
		if r.Result == "timeout" && (r.Count != 7 || r.Buckets[1] != 7) {
			t.Fatalf("timeout series not carried: %+v", r)
		}
	}
	for _, name := range aikey.LookupResultNames {
		if !seen[name] {
			t.Fatalf("result %q missing from the seeded snapshot", name)
		}
	}
}
