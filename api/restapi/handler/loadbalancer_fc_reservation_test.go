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

package handler

import (
	"encoding/json"
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
)

// The reservation read-back reaches the wire field for field, and an idle
// service says so explicitly: zeros and an empty list, never absent keys.
func TestFcReservationHeldReadBack(t *testing.T) {
	lb := cmn.LbRuleMod{Serv: cmn.LbServiceArg{
		ServIP: "10.10.10.254", ServPort: 2071, Proto: "tcp",
		FcEffective: &cmn.FcEffectiveArg{
			Mode: "off", ReservationEpoch: 29384756, ReservationHeldClaims: 900, ReservationHeldBuckets: 2,
			ReservationHeldTruncated: true,
			ReservationHeld: []cmn.FcResvHeldArg{
				{Kind: cmn.FcResvBucketTenant, Bucket: "t1", Tokens: 600},
				{Kind: cmn.FcResvBucketKey, Bucket: "kq:key-1", Tokens: 300},
			},
		},
	}}
	eff := serializeLBRule(lb).ServiceArguments.FcEffective
	if eff == nil || eff.ReservationEpoch != 29384756 || eff.ReservationHeldClaims != 900 ||
		eff.ReservationHeldBuckets != 2 || !eff.ReservationHeldTruncated || len(eff.ReservationHeld) != 2 {
		t.Fatalf("read back as %+v", eff)
	}
	if h := eff.ReservationHeld[0]; h.Kind != "tenant" || h.Bucket != "t1" || h.Tokens != 600 {
		t.Errorf("first bucket = %+v", h)
	}
	if h := eff.ReservationHeld[1]; h.Kind != "key" || h.Bucket != "kq:key-1" || h.Tokens != 300 {
		t.Errorf("second bucket = %+v", h)
	}
	if err := eff.Validate(nil); err != nil {
		t.Errorf("the model refuses its own read-back: %v", err)
	}

	lb.Serv.FcEffective = &cmn.FcEffectiveArg{Mode: "off"}
	wire, err := json.Marshal(serializeLBRule(lb).ServiceArguments.FcEffective)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"reservation_epoch": "0", "reservation_held_claims": "0", "reservation_held_buckets": "0",
		"reservation_held_truncated": "false", "reservation_held": "[]",
	} {
		if got, ok := m[key]; !ok || string(got) != want {
			t.Errorf("idle fc_effective.%s = %s present=%t, want %s", key, got, ok, want)
		}
	}
}
