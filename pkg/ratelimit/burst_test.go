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

package ratelimit

import "testing"

// BurstTokens is the bound the elastic reservation shrinks against on a
// full bucket: a claim of exactly that much is held whole, one token more
// is cut.
func TestBurstTokensIsTheBoundAFullBucketHolds(t *testing.T) {
	if got := BurstTokens(0, 0); got != 0 {
		t.Fatalf("a bucket with no quota bounds %d, want 0", got)
	}
	for _, c := range []struct{ tpm, pct int }{{600, 0}, {600, 100}, {600, 50}, {1000, 200}} {
		burst := int(BurstTokens(c.tpm, c.pct))
		if burst <= 0 {
			t.Fatalf("tpm=%d pct=%d: burst %d", c.tpm, c.pct, burst)
		}
		s := New()
		if ok, _, _, granted := s.ReserveTokensElastic("k", 0, burst, c.tpm, c.pct); !ok || granted != burst {
			t.Fatalf("tpm=%d pct=%d: a claim of the burst %d was granted %d", c.tpm, c.pct, burst, granted)
		}
		s = New()
		if ok, _, _, granted := s.ReserveTokensElastic("k", 0, burst+1, c.tpm, c.pct); !ok || granted != burst {
			t.Fatalf("tpm=%d pct=%d: a claim of burst+1 was granted %d, want %d", c.tpm, c.pct, granted, burst)
		}
	}
}
