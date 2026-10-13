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
	"sort"
	"sync"
	"sync/atomic"

	cmn "github.com/loxilb-io/loxilb/common"
	rl "github.com/loxilb-io/loxilb/pkg/ratelimit"
)

// The reservation read-back: which quota buckets a service's requests
// reserve on, so a rule's read-back can show what those buckets hold.
//
// The quota store keys a bucket by identity (a tenant, a user, a key), not
// by service, and cannot say which service a claim came through. So each
// service remembers the buckets its requests took a claim on, and the
// read-back asks the store what each of them holds now. The store's own
// counter is what is read: this keeps no second count that could disagree
// with it.

// aiResvBucketsMax bounds the buckets one service remembers. Past it a new
// bucket is not remembered until a read-back has dropped the ones whose
// store entry is gone; the read-back says so (truncated).
const aiResvBucketsMax = 4096

type aiResvService struct {
	buckets sync.Map // bucket key -> kind (string)
	n       atomic.Int32
	// missed is set when a bucket could not be remembered: the list is
	// then not the whole truth until the next read-back makes room.
	missed atomic.Bool
}

var aiResvLedger sync.Map // service identity -> *aiResvService

// noteReservedBucket remembers that a request of the service took a claim
// on a bucket. Called on the admission path once per bucket of the ladder:
// two lock-free map reads when the bucket is already known, which is every
// request after a bucket's first.
func noteReservedBucket(svcIdent string, b quotaBucket) {
	if svcIdent == "" || b.key == "" {
		return
	}
	v, ok := aiResvLedger.Load(svcIdent)
	if !ok {
		v, _ = aiResvLedger.LoadOrStore(svcIdent, &aiResvService{})
	}
	s := v.(*aiResvService)
	if _, known := s.buckets.Load(b.key); known {
		return
	}
	if s.n.Load() >= aiResvBucketsMax {
		s.missed.Store(true)
		return
	}
	if _, loaded := s.buckets.LoadOrStore(b.key, b.kind); !loaded {
		s.n.Add(1)
	}
}

// fillReservationsHeld writes what the service's buckets hold now into a
// rule's read-back. A bucket whose store entry is gone (idle and evicted)
// is forgotten here, which is what keeps the service's list bounded by the
// buckets in use.
func fillReservationsHeld(eff *cmn.FcEffectiveArg, store *rl.RateLimiterStore, svcIdent string) {
	eff.ReservationEpoch = rl.QuotaEpoch()
	eff.ReservationHeldClaims = 0
	eff.ReservationHeldTruncated = false
	eff.ReservationHeld = []cmn.FcResvHeldArg{}
	if store == nil || svcIdent == "" {
		return
	}
	v, ok := aiResvLedger.Load(svcIdent)
	if !ok {
		return
	}
	s := v.(*aiResvService)

	var held []cmn.FcResvHeldArg
	var nclaims uint64
	s.buckets.Range(func(k, kind any) bool {
		key := k.(string)
		tokens, claims, exists := store.ReservedNow(key)
		if !exists {
			if _, was := s.buckets.LoadAndDelete(key); was {
				s.n.Add(-1)
			}
			return true
		}
		if claims > 0 {
			nclaims += uint64(claims)
		}
		if tokens > 0 {
			held = append(held, cmn.FcResvHeldArg{Kind: kind.(string), Bucket: key, Tokens: uint64(tokens)})
		}
		return true
	})
	// Room was made (or there always was room): a bucket missed before is
	// remembered again on its next reservation.
	missed := s.missed.Load()
	if missed && s.n.Load() < aiResvBucketsMax {
		s.missed.Store(false)
	}

	sort.Slice(held, func(i, j int) bool {
		if held[i].Tokens != held[j].Tokens {
			return held[i].Tokens > held[j].Tokens
		}
		return held[i].Bucket < held[j].Bucket
	})
	eff.ReservationHeldClaims = nclaims
	eff.ReservationHeldTruncated = missed
	if len(held) > cmn.FcResvHeldMax {
		held = held[:cmn.FcResvHeldMax]
		eff.ReservationHeldTruncated = true
	}
	if held != nil {
		eff.ReservationHeld = held
	}
}
