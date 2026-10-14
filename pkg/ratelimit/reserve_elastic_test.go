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

package ratelimit

import (
	"sync/atomic"
	"testing"
)

func elasticStore() *RateLimiterStore {
	return &RateLimiterStore{entries: make(map[string]*limiterEntry)}
}

func reservedOf(t *testing.T, s *RateLimiterStore, key string) int64 {
	t.Helper()
	v, ok := s.quotaMap.Load(key)
	if !ok {
		t.Fatalf("no quota entry for %q", key)
	}
	return atomic.LoadInt64(&v.(*tokenWindowEntry).reserved)
}

// TestReserveElasticGrantsAllThatFits: with room for both parts the claim
// is firm + elastic, and that is what the bucket holds.
func TestReserveElasticGrantsAllThatFits(t *testing.T) {
	s := elasticStore()
	allowed, _, ep, granted := s.ReserveTokensElastic("t", 100, 512, 1000, 0)
	if !allowed || ep == 0 || granted != 612 {
		t.Fatalf("allowed=%v epoch=%d granted=%d, want admitted with 612", allowed, ep, granted)
	}
	if got := reservedOf(t, s, "t"); got != 612 {
		t.Fatalf("bucket holds %d, want 612", got)
	}
	if got := s.ReservedTokens("t"); got != 612 {
		t.Fatalf("ReservedTokens = %d, want 612", got)
	}
}

// TestReserveElasticShrinksToTheHeadroom: the elastic part is cut to what
// the bucket still holds, the request is admitted, and the bucket holds
// exactly what was granted - nothing of the part that was cut.
func TestReserveElasticShrinksToTheHeadroom(t *testing.T) {
	s := elasticStore()
	if allowed, _, _ := s.ReserveTokens("t", 700, 1000, 0); !allowed {
		t.Fatal("setup reservation refused")
	}
	allowed, _, ep, granted := s.ReserveTokensElastic("t", 100, 512, 1000, 0)
	if !allowed || ep == 0 {
		t.Fatalf("a default that does not fit must not refuse (allowed=%v epoch=%d)", allowed, ep)
	}
	if granted != 300 {
		t.Fatalf("granted %d, want 300 (100 firm + the 200 left)", granted)
	}
	if got := reservedOf(t, s, "t"); got != 1000 {
		t.Fatalf("bucket holds %d, want 1000", got)
	}
}

// TestReserveElasticShrinksToNothing: no headroom beyond the firm part is
// still an admission, on the firm part alone.
func TestReserveElasticShrinksToNothing(t *testing.T) {
	s := elasticStore()
	if allowed, _, _ := s.ReserveTokens("t", 900, 1000, 0); !allowed {
		t.Fatal("setup reservation refused")
	}
	allowed, _, ep, granted := s.ReserveTokensElastic("t", 100, 512, 1000, 0)
	if !allowed || ep == 0 || granted != 100 {
		t.Fatalf("allowed=%v epoch=%d granted=%d, want admitted with the firm 100", allowed, ep, granted)
	}
	if got := reservedOf(t, s, "t"); got != 1000 {
		t.Fatalf("bucket holds %d, want 1000", got)
	}
}

// TestReserveElasticFirmPartStillRefuses: the firm part is judged exactly
// as ReserveTokens judges it - same verdict, same retry advice - and a
// refusal leaves nothing behind, of either part.
func TestReserveElasticFirmPartStillRefuses(t *testing.T) {
	plain, elastic := elasticStore(), elasticStore()
	for _, s := range []*RateLimiterStore{plain, elastic} {
		if allowed, _, _ := s.ReserveTokens("t", 950, 1000, 0); !allowed {
			t.Fatal("setup reservation refused")
		}
	}
	pAllowed, pRetry, pEpoch := plain.ReserveTokens("t", 100, 1000, 0)
	eAllowed, eRetry, eEpoch, granted := elastic.ReserveTokensElastic("t", 100, 512, 1000, 0)
	if pAllowed || eAllowed {
		t.Fatalf("a firm part that does not fit must be refused (plain=%v elastic=%v)", pAllowed, eAllowed)
	}
	if eRetry != pRetry || eEpoch != pEpoch || granted != 0 {
		t.Fatalf("elastic refusal retry=%d epoch=%d granted=%d, want ReserveTokens' retry=%d epoch=%d and nothing granted",
			eRetry, eEpoch, granted, pRetry, pEpoch)
	}
	if got := reservedOf(t, elastic, "t"); got != 950 {
		t.Fatalf("refused claim left %d on the bucket, want the 950 held before", got)
	}
	if elastic.IsTokenQuotaExceeded("t") {
		t.Fatal("a reservation refusal must not latch the bucket")
	}
}

// TestReserveElasticFirmLargerThanTheBucket: the oversize refusal is the
// firm part's too.
func TestReserveElasticFirmLargerThanTheBucket(t *testing.T) {
	s := elasticStore()
	allowed, retry, ep, granted := s.ReserveTokensElastic("t", 1001, 512, 1000, 0)
	if allowed || ep != 0 || granted != 0 || retry <= 0 {
		t.Fatalf("allowed=%v retry=%d epoch=%d granted=%d, want a refusal with a retry", allowed, retry, ep, granted)
	}
	if got := reservedOf(t, s, "t"); got != 0 {
		t.Fatalf("oversize refusal left %d on the bucket", got)
	}
}

// TestReserveElasticBoundedByTheBurst: a default larger than what the
// bucket can ever hold beside the firm part is cut to that, not refused.
func TestReserveElasticBoundedByTheBurst(t *testing.T) {
	s := elasticStore()
	allowed, _, ep, granted := s.ReserveTokensElastic("t", 400, 5000, 1000, 0)
	if !allowed || ep == 0 || granted != 1000 {
		t.Fatalf("allowed=%v epoch=%d granted=%d, want admitted with the whole bucket (1000)", allowed, ep, granted)
	}
	// A tenant burst of half the limit halves it.
	s = elasticStore()
	_, _, _, granted = s.ReserveTokensElastic("t", 100, 5000, 1000, 50)
	if granted != 500 {
		t.Fatalf("granted %d under a 50%% burst, want 500", granted)
	}
}

// TestReserveElasticNothingToHold: no firm part and no headroom is an
// admission that holds nothing, so there is no epoch to settle.
func TestReserveElasticNothingToHold(t *testing.T) {
	s := elasticStore()
	if allowed, _, _ := s.ReserveTokens("t", 1000, 1000, 0); !allowed {
		t.Fatal("setup reservation refused")
	}
	allowed, _, ep, granted := s.ReserveTokensElastic("t", 0, 512, 1000, 0)
	if !allowed || ep != 0 || granted != 0 {
		t.Fatalf("allowed=%v epoch=%d granted=%d, want admitted holding nothing", allowed, ep, granted)
	}
	if got := reservedOf(t, s, "t"); got != 1000 {
		t.Fatalf("bucket holds %d, want the 1000 held before", got)
	}
}

// TestReserveElasticWithoutElasticIsReserveTokens: an elastic part of 0 is
// the plain reservation, and no quota is no reservation.
func TestReserveElasticWithoutElasticIsReserveTokens(t *testing.T) {
	s := elasticStore()
	if got := s.ReservedTokens("never-seen"); got != 0 {
		t.Fatalf("a key with no entry holds %d", got)
	}
	// A negative firm part is no firm part.
	if allowed, _, ep, granted := s.ReserveTokensElastic("neg", -5, 200, 1000, 0); !allowed || ep == 0 || granted != 200 {
		t.Fatalf("negative firm: allowed=%v epoch=%d granted=%d, want the 200 default alone", allowed, ep, granted)
	}
	allowed, _, ep, granted := s.ReserveTokensElastic("t", 300, 0, 1000, 0)
	if !allowed || ep == 0 || granted != 300 {
		t.Fatalf("allowed=%v epoch=%d granted=%d, want the plain 300", allowed, ep, granted)
	}
	allowed, _, ep, granted = s.ReserveTokensElastic("u", 300, 512, 0, 0)
	if !allowed || ep != 0 || granted != 0 {
		t.Fatalf("no quota: allowed=%v epoch=%d granted=%d, want admitted with nothing recorded", allowed, ep, granted)
	}
}

// TestReserveElasticSettleReleasesTheGrant: releasing what was granted
// empties the bucket's ledger; releasing what was ASKED would take another
// request's claim with it.
func TestReserveElasticSettleReleasesTheGrant(t *testing.T) {
	s := elasticStore()
	if allowed, _, _ := s.ReserveTokens("t", 700, 1000, 0); !allowed {
		t.Fatal("setup reservation refused")
	}
	_, _, ep, granted := s.ReserveTokensElastic("t", 100, 512, 1000, 0)
	s.ReleaseReservation("t", granted, ep)
	if got := reservedOf(t, s, "t"); got != 700 {
		t.Fatalf("after releasing the grant the bucket holds %d, want the other request's 700", got)
	}
}
