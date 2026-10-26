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
	"errors"
	"fmt"
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
	rl "github.com/loxilb-io/loxilb/pkg/ratelimit"
)

var storeAway = fmt.Errorf("test: key store away: %w", cmn.ErrDBUnavailable)

// A key the store cannot describe during an outage, with nothing cached or
// last-known, is refused: admitting it on zeroes would switch its limits off.
func TestRateLimitCheckFailsClosedWhenTheKeyIsUnknowable(t *testing.T) {
	store := rl.New()
	svc := &mockRateLimitService{keyByIDErr: storeAway}
	decision, retry, code := rateLimitCheckInternal(svc, store, "key-1", "tenant-1", "", "", "")
	if decision != 4 || retry != 5 || code != "policy_store_unavailable" {
		t.Fatalf("unknowable key admitted: decision=%d retry=%d code=%q", decision, retry, code)
	}
}

// The store's own verdict that the row is gone is not an outage: no key
// limits apply (the key's next validation refuses it), the request proceeds
// through the remaining stages as before.
func TestRateLimitCheckKeyNotFoundIsAVerdictNotAnOutage(t *testing.T) {
	store := rl.New()
	svc := &mockRateLimitService{keyByIDErr: errors.New("API key not found")}
	decision, _, code := rateLimitCheckInternal(svc, store, "key-gone", "tenant-1", "", "", "")
	if decision != 0 || code != "" {
		t.Fatalf("a not-found verdict was treated as an outage: decision=%d code=%q", decision, code)
	}
}

// The charge-side bucket set omits a key whose quota cannot be known, and
// keeps every other bucket; with the key described, its bucket is present.
func TestQuotaBucketsOmitAnUnknowableKeyOnly(t *testing.T) {
	away := &mockRateLimitService{tenantTPM: 600, keyByIDErr: storeAway}
	got := quotaBucketsFor(away, "tenant-1", "", "", "key-1", "")
	for _, b := range got {
		if b.kind == cmn.FcResvBucketKey {
			t.Fatalf("a key bucket with an unknown limit was charged: %+v", b)
		}
	}
	if len(got) != 1 || got[0].kind != cmn.FcResvBucketTenant {
		t.Fatalf("tenant bucket lost with the key's: %+v", got)
	}

	known := &mockRateLimitService{tenantTPM: 600, keyByID: map[string]*cmn.ApiKeySummary{
		"key-1": {KeyID: "key-1", TokensPerMin: 120},
	}}
	got = quotaBucketsFor(known, "tenant-1", "", "", "key-1", "")
	var keyBuckets int
	for _, b := range got {
		if b.kind == cmn.FcResvBucketKey && b.tpm == 120 {
			keyBuckets++
		}
	}
	if keyBuckets != 1 {
		t.Fatalf("key bucket missing with the key described: %+v", got)
	}
}
