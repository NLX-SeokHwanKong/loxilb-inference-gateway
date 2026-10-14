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

package loxinet

import (
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	prom "github.com/loxilb-io/loxilb/api/prometheus"
	cmn "github.com/loxilb-io/loxilb/common"
	rl "github.com/loxilb-io/loxilb/pkg/ratelimit"
)

// aiDefaultResv is a rule's default reservation as it was declared: the
// switch (cmn.AIDefaultReservationRule*) and the value in tokens, 0 on
// either meaning "not declared here".
type aiDefaultResv struct {
	mode      uint8
	maxTokens uint32
}

// aiDefaultResvResolve returns the default reservation a rule will store:
// on create the declared values, on a replace the stored ones for a field
// the request does not name, where inherit and 0 return a field to unset.
func aiDefaultResvResolve(eRule *ruleEnt, serv *cmn.LbServiceArg) (aiDefaultResv, error) {
	mode, err := cmn.AIDefaultReservationToRule(serv.AIDefaultReservation)
	if err != nil {
		return aiDefaultResv{}, err
	}
	if err := cmn.AIDefaultMaxTokensError(serv.AIDefaultMaxTokens); err != nil {
		return aiDefaultResv{}, err
	}
	next := aiDefaultResv{mode: mode, maxTokens: serv.AIDefaultMaxTokens}
	if eRule != nil {
		if !serv.AIDefaultReservationPresent && serv.AIDefaultReservation == "" {
			next.mode = eRule.aiDefaultResv.mode
		}
		if !serv.AIDefaultMaxTokensPresent && serv.AIDefaultMaxTokens == 0 {
			next.maxTokens = eRule.aiDefaultResv.maxTokens
		}
	}
	return next, nil
}

// inForce resolves the default reservation the data plane runs a rule on,
// and where each half came from: the rule's own value, else the process
// environment, else off and the built-in value. An environment value that
// is not one of the switch's words, or not a number within the bound, is
// read as unset. tokens is 0 while the switch is off, whatever the value.
func (c aiDefaultResv) inForce() (on bool, tokens uint32, modeSrc, tokensSrc string) {
	modeSrc = cmn.AIDefaultSourceDefault
	switch c.mode {
	case cmn.AIDefaultReservationRuleOn:
		on, modeSrc = true, cmn.AIDefaultSourceRule
	case cmn.AIDefaultReservationRuleOff:
		modeSrc = cmn.AIDefaultSourceRule
	default:
		switch os.Getenv(cmn.AIDefaultReservationEnv) {
		case cmn.AIDefaultReservationOn:
			on, modeSrc = true, cmn.AIDefaultSourceEnv
		case cmn.AIDefaultReservationOff:
			modeSrc = cmn.AIDefaultSourceEnv
		}
	}

	tokens, tokensSrc = cmn.AIDefaultMaxTokensBuiltin, cmn.AIDefaultSourceDefault
	if c.maxTokens != 0 {
		tokens, tokensSrc = c.maxTokens, cmn.AIDefaultSourceRule
	} else if v, err := strconv.ParseUint(os.Getenv(cmn.AIDefaultMaxTokensEnv), 10, 32); err == nil &&
		v >= 1 && v <= uint64(cmn.AIDefaultMaxTokensMax) {
		tokens, tokensSrc = uint32(v), cmn.AIDefaultSourceEnv
	}
	if !on {
		tokens = 0
	}
	return on, tokens, modeSrc, tokensSrc
}

// tokensInForce - what the data plane gets: the default in tokens, 0 = off.
func (c aiDefaultResv) tokensInForce() uint32 {
	_, tokens, _, _ := c.inForce()
	return tokens
}

// fillEffective writes the default reservation in force into a rule's
// read-back, with the clamped counts of the rule's service. The gate takes
// the value from the rule a request routes to, so what a rule resolves to
// is what is in force for its requests.
func (c aiDefaultResv) fillEffective(eff *cmn.FcEffectiveArg, vip net.IP, port uint16) {
	on, tokens, modeSrc, tokensSrc := c.inForce()
	eff.DefaultReservation = cmn.AIDefaultReservationOff
	if on {
		eff.DefaultReservation = cmn.AIDefaultReservationOn
	}
	eff.DefaultMaxTokens = tokens
	eff.Source.DefaultReservation = modeSrc
	eff.Source.DefaultMaxTokens = tokensSrc
	svc := aiServiceIdent(vip, port)
	eff.ReservationClampedBurst = reservationClampedCount(svc, resvClampBurst)
	eff.ReservationClampedBudget = reservationClampedCount(svc, resvClampBudget)
	fillReservationsHeld(eff, getGlobalRL(), svc)
}

// aiServiceIdent is the data plane's service identity of a listener
// ("VIP:port", ai_gw_svc_ident in sockproxy_ai_admit.c), the key the
// clamped counts are kept under. Empty for an address that is not IPv4,
// as there.
func aiServiceIdent(vip net.IP, port uint16) string {
	v4 := vip.To4()
	if v4 == nil {
		return ""
	}
	return v4.String() + ":" + strconv.FormatUint(uint64(port), 10)
}

// tokenQuotaReserveElasticInternal is tokenQuotaReserveInternal for a request
// that declares no completion ceiling on a rule with a default reservation:
// firm (the prompt estimate) is claimed and refused as before, elastic (the
// default) is claimed beside it on every bucket and shrunk to what the
// tightest of them still holds. The default alone never refuses a request.
//
// The elastic part can only shrink from one bucket to the next. A bucket
// that granted more than the last one did gives the difference back once the
// ladder is done, so every bucket ends up holding the same granted total —
// which is what settlement releases from each of them
// (tokenQuotaConsumeInternal walks quotaReleaseKeysFor with one amount).
//
// Returns tokenQuotaReserveInternal's three values, granted (firm plus what
// was left of elastic; the amount to echo at settlement) and clamped (the
// request holds less than the default it asked for). resEpoch is 0 when
// nothing is held on any bucket.
func tokenQuotaReserveElasticInternal(svc rateLimitService, store *rl.RateLimiterStore, tenantID, modelName, userID, keyID, svcIdent string, firm, elastic int) (allowed bool, retrySecs int, resEpoch int64, granted int, clamped bool) {
	allowed, retrySecs, resEpoch, granted, why := tokenQuotaReserveElasticReason(svc, store, tenantID, modelName, userID, keyID, svcIdent, firm, elastic)
	return allowed, retrySecs, resEpoch, granted, why != resvClampNone
}

// resvClamp says why a default reservation was admitted smaller than the
// default.
type resvClamp uint8

const (
	resvClampNone resvClamp = iota
	// resvClampBudget: a bucket's budget was already taken by other
	// claims or by spend. Passes as the window refills.
	resvClampBudget
	// resvClampBurst: the default beside the prompt is more than a bucket
	// can ever hold, so the same request is clamped on an idle bucket too.
	// A configuration to fix: the default is too large for that quota.
	resvClampBurst
)

// The reason label values of loxilb_ai_reservation_clamped_total.
func (c resvClamp) String() string {
	switch c {
	case resvClampBudget:
		return prom.ReservationClampBudget
	case resvClampBurst:
		return prom.ReservationClampBurst
	}
	return ""
}

// tokenQuotaReserveElasticReason is tokenQuotaReserveElasticInternal with the
// reason for a clamp in place of the flag. Burst wins where both apply: a
// request that an idle bucket would clamp is the persistent case, whatever
// the budgets held at that moment.
func tokenQuotaReserveElasticReason(svc rateLimitService, store *rl.RateLimiterStore, tenantID, modelName, userID, keyID, svcIdent string, firm, elastic int) (allowed bool, retrySecs int, resEpoch int64, granted int, why resvClamp) {
	if firm < 0 {
		firm = 0
	}
	if elastic <= 0 {
		allowed, retrySecs, resEpoch = tokenQuotaReserveInternal(svc, store, tenantID, modelName, userID, keyID, svcIdent, firm)
		if allowed && resEpoch != 0 {
			granted = firm
		}
		return allowed, retrySecs, resEpoch, granted, resvClampNone
	}
	if tenantID == "" && svcIdent == "" {
		return true, 0, 0, 0, resvClampNone
	}
	buckets := quotaBucketsFor(svc, tenantID, modelName, userID, keyID, svcIdent)
	if len(buckets) == 0 {
		return true, 0, 0, 0, resvClampNone
	}

	left := elastic
	held := make([]int, len(buckets))
	overBurst := false
	for i, b := range buckets {
		if burst := rl.BurstTokens(b.tpm, b.burstPct); burst > 0 && int64(firm)+int64(elastic) > burst {
			overBurst = true
		}
		bAllowed, bRetry, bEpoch, bGranted := store.ReserveTokensElastic(b.key, firm, left, b.tpm, b.burstPct)
		if !bAllowed {
			// The firm part did not fit this bucket: today's denial, and
			// every claim taken so far goes back.
			for j := range i {
				store.ReleaseReservation(buckets[j].key, held[j], resEpoch)
			}
			return false, bRetry, 0, 0, resvClampNone
		}
		held[i] = bGranted
		left = min(left, max(bGranted-firm, 0))
		if resEpoch == 0 {
			resEpoch = bEpoch
		}
		if bEpoch != 0 {
			noteReservedBucket(svcIdent, b)
		}
	}
	granted = firm + left
	for i, b := range buckets {
		if held[i] > granted {
			if granted == 0 {
				// Nothing of the claim is kept: it ends here.
				store.ReleaseReservation(b.key, held[i], resEpoch)
			} else {
				store.TrimReservation(b.key, held[i]-granted, resEpoch)
			}
		}
	}
	if left < elastic {
		why = resvClampBudget
		if overBurst {
			why = resvClampBurst
		}
	}
	if granted == 0 {
		return true, 0, 0, 0, why
	}
	return true, 0, resEpoch, granted, why
}

// aiReservationClamped counts, per service ("VIP:port") and reason, the
// requests whose default reservation was admitted with less than the
// default: the read side of the rule's reservation_clamped_* read-back. The
// Prometheus counter is incremented beside it.
var aiReservationClamped sync.Map // service identity -> *[resvClampBurst + 1]atomic.Uint64

func noteReservationClamped(svcIdent string, why resvClamp) {
	if why != resvClampBudget && why != resvClampBurst {
		return
	}
	v, ok := aiReservationClamped.Load(svcIdent)
	if !ok {
		v, _ = aiReservationClamped.LoadOrStore(svcIdent, new([resvClampBurst + 1]atomic.Uint64))
	}
	v.(*[resvClampBurst + 1]atomic.Uint64)[why].Add(1)
	prom.RecordReservationClamped(svcIdent, why.String())
}

// reservationClampedCount - the clamped default reservations counted on a
// service for one reason since the process started.
func reservationClampedCount(svcIdent string, why resvClamp) uint64 {
	if why != resvClampBudget && why != resvClampBurst {
		return 0
	}
	if v, ok := aiReservationClamped.Load(svcIdent); ok {
		return v.(*[resvClampBurst + 1]atomic.Uint64)[why].Load()
	}
	return 0
}
