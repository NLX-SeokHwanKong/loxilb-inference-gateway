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

package common

import "testing"

// The two orders of a band triple: the REST form critical:standard:sheddable
// and the band index the data plane uses ([0] standard, [1] critical, [2]
// sheddable). Each position must land in its own band, and every triple must
// come back as it was written.
func TestFcBandTripleOrderRoundTrip(t *testing.T) {
	got, err := FcBandTripleParse("fc_band_wait_ms", "100:200:300", FcBandWaitMsMax)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[FcBandCritical] != 100 || got[FcBandStandard] != 200 || got[FcBandSheddable] != 300 {
		t.Fatalf("positions landed in the wrong bands: critical=%d standard=%d sheddable=%d",
			got[FcBandCritical], got[FcBandStandard], got[FcBandSheddable])
	}
	// Index order is the C order: FC_BAND_STANDARD 0, CRITICAL 1, SHEDDABLE 2.
	if FcBandStandard != 0 || FcBandCritical != 1 || FcBandSheddable != 2 {
		t.Fatalf("band index drifted from FC_BAND_*: standard=%d critical=%d sheddable=%d",
			FcBandStandard, FcBandCritical, FcBandSheddable)
	}
	if back := FcBandTripleFormat(got); back != "100:200:300" {
		t.Fatalf("format(parse(%q)) = %q", "100:200:300", back)
	}

	// Every distinct triple over a small range comes back as written, and
	// the band-indexed array formats back to the same triple.
	for c := uint32(0); c < 4; c++ {
		for s := uint32(0); s < 4; s++ {
			for b := uint32(0); b < 4; b++ {
				var idx [FcBands]uint32
				idx[FcBandCritical], idx[FcBandStandard], idx[FcBandSheddable] = c, s, b
				w := FcBandTripleFormat(idx)
				if c == 0 && s == 0 && b == 0 {
					if w != "" {
						t.Fatalf("all-zero triple formats to %q, want empty", w)
					}
					continue
				}
				back, err := FcBandTripleParse("fc_band_queue_pct", w, FcBandPctMax)
				if err != nil {
					t.Fatalf("parse(%q): %v", w, err)
				}
				if back != idx {
					t.Fatalf("parse(format(%v)) = %v", idx, back)
				}
			}
		}
	}
}

func TestFcBandName(t *testing.T) {
	for b, want := range map[int]string{
		FcBandCritical: AIClassCritical, FcBandStandard: AIClassStandard,
		FcBandSheddable: AIClassSheddable, 7: AIClassStandard,
	} {
		if got := FcBandName(b); got != want {
			t.Errorf("FcBandName(%d) = %q, want %q", b, got, want)
		}
	}
}
