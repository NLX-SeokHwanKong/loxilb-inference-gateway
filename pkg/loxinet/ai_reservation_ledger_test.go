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
	"fmt"
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
	rl "github.com/loxilb-io/loxilb/pkg/ratelimit"
)

const (
	ledgerSvc      = "10.10.10.254:2071"
	ledgerOtherSvc = "10.10.10.254:2072"
)

// ledgerSixSvc is sixBucketSvc on a service of this file's own: the
// read-back's bucket list is kept per service for the life of the process.
func ledgerSixSvc(svcIdent string) *mockRateLimitService {
	svc := ladderSvc()
	svc.tenantTPM = 10000
	svc.modelTPM = map[string]int{"m": 10000}
	svc.userRows["t1|alice"] = cmn.UserRateLimitEntry{TenantID: "t1", UserID: "alice", TokensPerMin: 10000}
	svc.userModelTPM = map[string]int{"t1|alice|m": 10000}
	svc.keyByID = map[string]*cmn.ApiKeySummary{"key-1": {KeyID: "key-1", TokensPerMin: 10000}}
	svc.defaults["rule|"+svcIdent] = cmn.RateLimitDefaultsEntry{
		Scope: cmn.RateLimitScopeRule, RuleIdent: svcIdent, VipSharedTPM: 10000,
	}
	return svc
}

func heldOf(store *rl.RateLimiterStore, svcIdent string) *cmn.FcEffectiveArg {
	eff := &cmn.FcEffectiveArg{}
	fillReservationsHeld(eff, store, svcIdent)
	return eff
}

// A service nothing reserved on reads as holding nothing, with the list
// present and empty and the epoch the store is in.
func TestReservationsHeldOfAnIdleService(t *testing.T) {
	store := rl.New()
	eff := heldOf(store, "10.10.10.254:2079")
	if eff.ReservationHeldClaims != 0 || eff.ReservationHeldTruncated {
		t.Errorf("idle service: %+v", eff)
	}
	if eff.ReservationHeld == nil || len(eff.ReservationHeld) != 0 {
		t.Errorf("the list must be present and empty, got %v", eff.ReservationHeld)
	}
	if eff.ReservationEpoch != rl.QuotaEpoch() || eff.ReservationEpoch == 0 {
		t.Errorf("epoch = %d, store is in %d", eff.ReservationEpoch, rl.QuotaEpoch())
	}
	// No store, no service: the same empty answer, never a panic.
	eff = &cmn.FcEffectiveArg{}
	fillReservationsHeld(eff, nil, ledgerSvc)
	if eff.ReservationHeld == nil || eff.ReservationHeldClaims != 0 {
		t.Errorf("no store: %+v", eff)
	}
}

// The read-back shows the store's own counter on each bucket of the ladder
// while a request holds its claim, and nothing once it has settled.
func TestReservationsHeldFollowsTheLedger(t *testing.T) {
	store := rl.New()
	svc := ledgerSixSvc(ledgerSvc)

	allowed, _, epoch := tokenQuotaReserveInternal(svc, store, "t1", "m", "alice", "key-1", ledgerSvc, 300)
	if !allowed || epoch == 0 {
		t.Fatalf("reserve: allowed=%v epoch=%d", allowed, epoch)
	}
	eff := heldOf(store, ledgerSvc)
	if len(eff.ReservationHeld) != 6 || eff.ReservationHeldClaims != 6 || eff.ReservationHeldTruncated {
		t.Fatalf("one request of 300 on six buckets: listed=%d claims=%d truncated=%v, want one claim per bucket",
			len(eff.ReservationHeld), eff.ReservationHeldClaims, eff.ReservationHeldTruncated)
	}
	if eff.ReservationEpoch != epoch {
		t.Errorf("read-back epoch %d, the claim was tagged %d", eff.ReservationEpoch, epoch)
	}
	kinds := map[string]string{}
	for _, h := range eff.ReservationHeld {
		kinds[h.Kind] = h.Bucket
		if h.Tokens != 300 {
			t.Errorf("bucket %s (%s) holds %d, want 300", h.Bucket, h.Kind, h.Tokens)
		}
		if got := store.ReservedTokens(h.Bucket); got != int64(h.Tokens) {
			t.Errorf("bucket %s: read-back %d, the store's counter %d", h.Bucket, h.Tokens, got)
		}
	}
	want := map[string]string{
		cmn.FcResvBucketTenant:      "t1",
		cmn.FcResvBucketTenantModel: modelQuotaKey("t1", "m"),
		cmn.FcResvBucketUser:        rl.UserQuotaKey("t1", "alice"),
		cmn.FcResvBucketUserModel:   rl.UserModelQuotaKey("t1", "alice", "m"),
		cmn.FcResvBucketKey:         rl.KeyQuotaKey("key-1"),
		cmn.FcResvBucketService:     rl.VipSharedQuotaKey(ledgerSvc),
	}
	for kind, bucket := range want {
		if kinds[kind] != bucket {
			t.Errorf("kind %s: bucket %q, want %q", kind, kinds[kind], bucket)
		}
	}

	// A second request, through the elastic path, adds to the same buckets.
	ok2, _, epoch2, granted, _ := tokenQuotaReserveElasticReason(svc, store, "t1", "m", "alice", "key-1", ledgerSvc, 100, 50)
	if !ok2 || granted != 150 {
		t.Fatalf("elastic reserve: ok=%v granted=%d", ok2, granted)
	}
	if eff = heldOf(store, ledgerSvc); eff.ReservationHeldClaims != 12 || len(eff.ReservationHeld) != 6 {
		t.Fatalf("two requests: claims=%d listed=%d, want 12 claims on six buckets", eff.ReservationHeldClaims, len(eff.ReservationHeld))
	}
	for _, h := range eff.ReservationHeld {
		if h.Tokens != 450 {
			t.Errorf("bucket %s holds %d, want 450", h.Bucket, h.Tokens)
		}
	}

	// Settle both: nothing is held, on any bucket.
	tokenQuotaConsumeInternal(svc, store, "t1", "m", "alice", "key-1", ledgerSvc, 10, 300, epoch)
	if eff = heldOf(store, ledgerSvc); eff.ReservationHeldClaims != 6 {
		t.Fatalf("after the first settle: claims=%d, want 6", eff.ReservationHeldClaims)
	}
	tokenQuotaConsumeInternal(svc, store, "t1", "m", "alice", "key-1", ledgerSvc, 10, granted, epoch2)
	eff = heldOf(store, ledgerSvc)
	if eff.ReservationHeldClaims != 0 || len(eff.ReservationHeld) != 0 {
		t.Fatalf("after both settled: %+v", eff)
	}
}

// A refused reservation leaves nothing behind for the read-back to show.
func TestReservationsHeldAfterARefusal(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.tenantTPM = 100
	const svcIdent = "10.10.10.254:2073"
	if allowed, _, _ := tokenQuotaReserveInternal(svc, store, "t-small", "", "", "", svcIdent, 5000); allowed {
		t.Fatal("a claim larger than the bucket was admitted")
	}
	if eff := heldOf(store, svcIdent); eff.ReservationHeldClaims != 0 || len(eff.ReservationHeld) != 0 {
		t.Errorf("after a refusal: %+v", eff)
	}
}

// A tenant's bucket is one bucket whichever service the request came
// through: the service that reserved on it shows it, a service that never
// did shows nothing.
func TestReservationsHeldIsPerService(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.tenantTPM = 10000
	const a, b, c = "10.10.10.254:2074", "10.10.10.254:2075", "10.10.10.254:2076"

	if ok, _, _ := tokenQuotaReserveInternal(svc, store, "t-shared", "", "", "", a, 200); !ok {
		t.Fatal("reserve on a")
	}
	if ok, _, _ := tokenQuotaReserveInternal(svc, store, "t-shared", "", "", "", b, 300); !ok {
		t.Fatal("reserve on b")
	}
	for _, s := range []string{a, b} {
		eff := heldOf(store, s)
		if len(eff.ReservationHeld) != 1 || eff.ReservationHeld[0].Tokens != 500 || eff.ReservationHeldClaims != 2 {
			t.Errorf("service %s: %+v claims=%d, want the tenant's one bucket holding 500 for two claims",
				s, eff.ReservationHeld, eff.ReservationHeldClaims)
		}
	}
	if eff := heldOf(store, c); len(eff.ReservationHeld) != 0 || eff.ReservationHeldClaims != 0 {
		t.Errorf("a service the tenant never used shows %+v, %d claims", eff.ReservationHeld, eff.ReservationHeldClaims)
	}
}

// The list is cut at FcResvHeldMax, largest first, and says so; the count
// of claims still covers every bucket.
func TestReservationsHeldListIsBounded(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.tenantTPM = 100000
	const svcIdent = "10.10.10.254:2077"
	n := cmn.FcResvHeldMax + 6
	for i := 0; i < n; i++ {
		want := 10 + i
		if ok, _, _ := tokenQuotaReserveInternal(svc, store, fmt.Sprintf("t-many-%03d", i), "", "", "", svcIdent, want); !ok {
			t.Fatalf("reserve %d", i)
		}
	}
	eff := heldOf(store, svcIdent)
	if eff.ReservationHeldClaims != uint64(n) {
		t.Errorf("claims=%d, want %d: one request on each of %d buckets", eff.ReservationHeldClaims, n, n)
	}
	if len(eff.ReservationHeld) != cmn.FcResvHeldMax || !eff.ReservationHeldTruncated {
		t.Errorf("list of %d, truncated=%v; want %d and true", len(eff.ReservationHeld), eff.ReservationHeldTruncated, cmn.FcResvHeldMax)
	}
	for i := 1; i < len(eff.ReservationHeld); i++ {
		if eff.ReservationHeld[i].Tokens > eff.ReservationHeld[i-1].Tokens {
			t.Fatalf("list is not largest first at %d", i)
		}
	}
	if top := eff.ReservationHeld[0]; top.Tokens != uint64(10+n-1) {
		t.Errorf("largest listed = %d, want %d", top.Tokens, 10+n-1)
	}
}

// A bucket whose store entry is gone is forgotten at the next read-back, so
// the service's list follows the buckets in use.
func TestReservationsHeldForgetsVanishedBuckets(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.tenantTPM = 10000
	const svcIdent = "10.10.10.254:2078"
	if ok, _, _ := tokenQuotaReserveInternal(svc, store, "t-kept", "", "", "", svcIdent, 100); !ok {
		t.Fatal("reserve")
	}
	// A bucket the service remembers and the store has never seen: what an
	// evicted entry looks like from here.
	noteReservedBucket(svcIdent, quotaBucket{key: "t-gone", kind: cmn.FcResvBucketTenant})
	v, _ := aiResvLedger.Load(svcIdent)
	s := v.(*aiResvService)
	if got := s.n.Load(); got != 2 {
		t.Fatalf("remembered %d buckets, want 2", got)
	}
	eff := heldOf(store, svcIdent)
	if len(eff.ReservationHeld) != 1 || eff.ReservationHeld[0].Bucket != "t-kept" || eff.ReservationHeldClaims != 1 {
		t.Errorf("read-back: %+v", eff.ReservationHeld)
	}
	if got := s.n.Load(); got != 1 {
		t.Errorf("after the read-back the service remembers %d buckets, want 1", got)
	}
	if _, still := s.buckets.Load("t-gone"); still {
		t.Error("the vanished bucket is still remembered")
	}
}

// Past the per-service bound a new bucket is not remembered, and the
// read-back says the list may be short.
func TestReservationsHeldSaysWhenItMissedABucket(t *testing.T) {
	store := rl.New()
	const svcIdent = "10.10.10.254:2070"
	for i := 0; i < aiResvBucketsMax+1; i++ {
		noteReservedBucket(svcIdent, quotaBucket{key: fmt.Sprintf("t-cap-%05d", i), kind: cmn.FcResvBucketTenant})
	}
	v, _ := aiResvLedger.Load(svcIdent)
	s := v.(*aiResvService)
	if got := s.n.Load(); got != aiResvBucketsMax {
		t.Fatalf("remembered %d, want the bound %d", got, aiResvBucketsMax)
	}
	if !heldOf(store, svcIdent).ReservationHeldTruncated {
		t.Error("a missed bucket is not reported")
	}
	// That read-back dropped the buckets the store never had: room again,
	// and the next one is clean.
	if got := s.n.Load(); got != 0 {
		t.Fatalf("after the read-back the service remembers %d buckets", got)
	}
	if heldOf(store, svcIdent).ReservationHeldTruncated {
		t.Error("still reported after room was made")
	}
	// The empty and unknown service names are never remembered.
	noteReservedBucket("", quotaBucket{key: "x", kind: cmn.FcResvBucketTenant})
	if _, ok := aiResvLedger.Load(""); ok {
		t.Error("a claim with no service made an entry")
	}
}

// The default reservation takes its claims through its own loop: a service
// whose only reservations are of that kind is remembered just the same.
func TestReservationsHeldSeesTheDefaultReservation(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.tenantTPM = 10000
	const svcIdent = "10.10.10.254:2069"
	ok, _, epoch, granted, _ := tokenQuotaReserveElasticReason(svc, store, "t-elastic", "", "", "", svcIdent, 20, 500)
	if !ok || granted != 520 || epoch == 0 {
		t.Fatalf("elastic reserve: ok=%v granted=%d epoch=%d", ok, granted, epoch)
	}
	eff := heldOf(store, svcIdent)
	if len(eff.ReservationHeld) != 1 || eff.ReservationHeldClaims != 1 || eff.ReservationHeld[0].Tokens != 520 ||
		eff.ReservationHeld[0].Bucket != "t-elastic" || eff.ReservationHeld[0].Kind != cmn.FcResvBucketTenant {
		t.Fatalf("read-back of a default reservation: %+v", eff.ReservationHeld)
	}
	tokenQuotaConsumeInternal(svc, store, "t-elastic", "", "", "", svcIdent, 30, granted, epoch)
	if eff = heldOf(store, svcIdent); eff.ReservationHeldClaims != 0 || len(eff.ReservationHeld) != 0 {
		t.Errorf("after settlement: %+v, %d claims", eff.ReservationHeld, eff.ReservationHeldClaims)
	}
}

// A default reservation cut down to what the bucket has left is still one
// claim, and one cut down to nothing is none: the count follows the claims
// held, not the tokens asked for.
func TestReservationsHeldCountsAClampedClaimOnce(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.tenantTPM = 1000
	svc.modelTPM = map[string]int{"m": 400}
	const svcIdent = "10.10.10.254:2068"
	// The model bucket holds less than the tenant's: the tenant's claim is
	// trimmed to what the model bucket granted, and both stay held.
	ok, _, epoch, granted, why := tokenQuotaReserveElasticReason(svc, store, "t-trim", "m", "", "", svcIdent, 20, 5000)
	if !ok || epoch == 0 || granted <= 20 || why == resvClampNone {
		t.Fatalf("clamped reserve: ok=%v epoch=%d granted=%d why=%v", ok, epoch, granted, why)
	}
	eff := heldOf(store, svcIdent)
	if eff.ReservationHeldClaims != 2 || len(eff.ReservationHeld) != 2 {
		t.Fatalf("a trimmed claim on two buckets: claims=%d listed=%+v, want 2", eff.ReservationHeldClaims, eff.ReservationHeld)
	}
	for _, h := range eff.ReservationHeld {
		if h.Tokens != uint64(granted) {
			t.Errorf("bucket %s holds %d, want the granted %d", h.Bucket, h.Tokens, granted)
		}
	}
	tokenQuotaConsumeInternal(svc, store, "t-trim", "m", "", "", svcIdent, 30, granted, epoch)
	if eff = heldOf(store, svcIdent); eff.ReservationHeldClaims != 0 || len(eff.ReservationHeld) != 0 {
		t.Fatalf("after settlement: claims=%d listed=%+v", eff.ReservationHeldClaims, eff.ReservationHeld)
	}

	// A refusal on the second bucket gives the first bucket's claim back.
	svc.modelTPM = map[string]int{"m2": 10}
	if ok, _, _, _, _ := tokenQuotaReserveElasticReason(svc, store, "t-trim", "m2", "", "", svcIdent, 50, 100); ok {
		t.Fatal("a firm part larger than the model bucket was admitted")
	}
	if eff = heldOf(store, svcIdent); eff.ReservationHeldClaims != 0 || len(eff.ReservationHeld) != 0 {
		t.Fatalf("after a refusal on the second bucket: claims=%d listed=%+v", eff.ReservationHeldClaims, eff.ReservationHeld)
	}
}
