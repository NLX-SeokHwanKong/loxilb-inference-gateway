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
	"net"
	"slices"
	"strings"
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
	rl "github.com/loxilb-io/loxilb/pkg/ratelimit"
)

const resvSvc = "10.10.10.254:2050"

// sixBucketSvc gives one request all six ladder buckets: tenant, tenant|model,
// user, user|model, key and the per-VIP shared one.
func sixBucketSvc(tenant, model, user, userModel, key, vip int) *mockRateLimitService {
	svc := ladderSvc()
	svc.tenantTPM = tenant
	svc.modelTPM = map[string]int{"m": model}
	svc.userRows["t1|alice"] = cmn.UserRateLimitEntry{TenantID: "t1", UserID: "alice", TokensPerMin: user}
	svc.userModelTPM = map[string]int{"t1|alice|m": userModel}
	svc.keyByID = map[string]*cmn.ApiKeySummary{"key-1": {KeyID: "key-1", TokensPerMin: key}}
	svc.defaults["rule|"+resvSvc] = cmn.RateLimitDefaultsEntry{
		Scope: cmn.RateLimitScopeRule, RuleIdent: resvSvc, VipSharedTPM: vip,
	}
	return svc
}

func sixKeys() []string {
	return quotaReleaseKeysFor("t1", "m", "alice", "key-1", resvSvc)
}

func reserveSix(svc rateLimitService, store *rl.RateLimiterStore, firm, elastic int) (bool, int, int64, int, bool) {
	return tokenQuotaReserveElasticInternal(svc, store, "t1", "m", "alice", "key-1", resvSvc, firm, elastic)
}

// TestDefaultReservationLandsOnAllSixBuckets: with room everywhere the
// default is held in full on every bucket of the ladder, and settlement
// with the granted amount empties every one of them.
func TestDefaultReservationLandsOnAllSixBuckets(t *testing.T) {
	store := rl.New()
	svc := sixBucketSvc(10000, 10000, 10000, 10000, 10000, 10000)
	keys := sixKeys()
	if len(keys) != 6 {
		t.Fatalf("the fixture resolves %d release keys, want all six", len(keys))
	}

	allowed, _, epoch, granted, clamped := reserveSix(svc, store, 100, 512)
	if !allowed || epoch == 0 || granted != 612 || clamped {
		t.Fatalf("allowed=%v epoch=%d granted=%d clamped=%v, want 612 held and not clamped", allowed, epoch, granted, clamped)
	}
	for _, k := range keys {
		if got := store.ReservedTokens(k); got != 612 {
			t.Errorf("bucket %q holds %d, want 612", k, got)
		}
	}

	tokenQuotaConsumeInternal(svc, store, "t1", "m", "alice", "key-1", resvSvc, 40, granted, epoch)
	for _, k := range keys {
		if got := store.ReservedTokens(k); got != 0 {
			t.Errorf("bucket %q still holds %d after settlement", k, got)
		}
	}
}

// TestDefaultReservationClampsAcrossTheLadder is the six-bucket clamp. Each
// bucket in turn is the tight one; whichever it is, the request is admitted,
// every bucket ends up holding the SAME total (the one settlement will
// release), and the buckets claimed before the tight one have given their
// surplus back.
func TestDefaultReservationClampsAcrossTheLadder(t *testing.T) {
	const firm, elastic, tight = 100, 512, 300 // the tight bucket leaves 200 for the default
	for pos, name := range []string{"tenant", "tenant|model", "user", "user|model", "key", "vip"} {
		t.Run(name, func(t *testing.T) {
			limits := [6]int{10000, 10000, 10000, 10000, 10000, 10000}
			limits[pos] = tight
			store := rl.New()
			svc := sixBucketSvc(limits[0], limits[1], limits[2], limits[3], limits[4], limits[5])

			allowed, _, epoch, granted, clamped := reserveSix(svc, store, firm, elastic)
			if !allowed {
				t.Fatalf("a default that does not fit the %s bucket refused the request", name)
			}
			if granted != tight || !clamped || epoch == 0 {
				t.Fatalf("granted=%d clamped=%v epoch=%d, want %d clamped", granted, clamped, epoch, tight)
			}
			for _, k := range sixKeys() {
				if got := store.ReservedTokens(k); got != int64(granted) {
					t.Errorf("bucket %q holds %d, want the granted %d", k, got, granted)
				}
			}

			tokenQuotaConsumeInternal(svc, store, "t1", "m", "alice", "key-1", resvSvc, 0, granted, epoch)
			for _, k := range sixKeys() {
				if got := store.ReservedTokens(k); got != 0 {
					t.Errorf("bucket %q still holds %d after settlement", k, got)
				}
			}
		})
	}
}

// TestDefaultReservationNeverRefuses: a second concurrent request finds the
// budget taken by the first one's claim. Its default is cut to nothing and
// it is admitted on its prompt estimate, which fits.
func TestDefaultReservationNeverRefuses(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.tenantTPM = 700

	_, _, _, first, firstClamped := tokenQuotaReserveElasticInternal(svc, store, "t1", "", "", "", "", 100, 512)
	if first != 612 || firstClamped {
		t.Fatalf("first request granted %d clamped=%v, want 612 in full", first, firstClamped)
	}
	allowed, _, epoch, second, clamped := tokenQuotaReserveElasticInternal(svc, store, "t1", "", "", "", "", 80, 512)
	if !allowed || epoch == 0 {
		t.Fatalf("second request refused: only its 80-token prompt estimate is firm, and 88 are left")
	}
	if second != 88 || !clamped {
		t.Fatalf("second request granted %d clamped=%v, want 88 (80 firm + the 8 left) clamped", second, clamped)
	}
	if got := store.ReservedTokens("t1"); got != 700 {
		t.Fatalf("tenant bucket holds %d, want 700", got)
	}

	// The same second request with a DECLARED ceiling of 512 is refused:
	// only a declared ceiling refuses.
	store2 := rl.New()
	tokenQuotaReserveElasticInternal(svc, store2, "t1", "", "", "", "", 100, 512)
	if allowed, _, _ := tokenQuotaReserveInternal(svc, store2, "t1", "", "", "", "", 80+512); allowed {
		t.Fatal("a declared ceiling that does not fit must still be refused")
	}
}

// TestDefaultReservationFirmRefusalRollsBack: the prompt estimate does not
// fit a later bucket. The request is refused as it is today, and the
// earlier buckets give back everything - their firm part and the default
// they had granted beside it.
func TestDefaultReservationFirmRefusalRollsBack(t *testing.T) {
	store := rl.New()
	svc := sixBucketSvc(10000, 10000, 10000, 10000, 50, 10000) // the key bucket is too small for the prompt

	allowed, retry, epoch, granted, clamped := reserveSix(svc, store, 100, 512)
	if allowed || epoch != 0 || granted != 0 || clamped {
		t.Fatalf("allowed=%v epoch=%d granted=%d clamped=%v, want a plain refusal", allowed, epoch, granted, clamped)
	}
	if retry <= 0 {
		t.Fatalf("refusal carries retry=%d, want a hint", retry)
	}
	for _, k := range sixKeys() {
		if got := store.ReservedTokens(k); got != 0 {
			t.Errorf("bucket %q holds %d after the refusal, want 0", k, got)
		}
	}

	// The same verdict the knob-off path gives.
	if allowed, _, _ := tokenQuotaReserveInternal(svc, rl.New(), "t1", "m", "alice", "key-1", resvSvc, 100); allowed {
		t.Fatal("fixture: the prompt estimate alone was expected not to fit")
	}
}

// TestDefaultReservationOffIsThePlainLadder: an elastic part of 0 runs the
// existing ladder, with the plain claim as the granted amount.
func TestDefaultReservationOffIsThePlainLadder(t *testing.T) {
	svc := sixBucketSvc(1000, 1000, 1000, 1000, 1000, 1000)
	plain, elastic := rl.New(), rl.New()

	pAllowed, pRetry, pEpoch := tokenQuotaReserveInternal(svc, plain, "t1", "m", "alice", "key-1", resvSvc, 400)
	eAllowed, eRetry, eEpoch, granted, clamped := reserveSix(svc, elastic, 400, 0)
	if pAllowed != eAllowed || pRetry != eRetry || (pEpoch == 0) != (eEpoch == 0) {
		t.Fatalf("plain (%v,%d,%d) and elastic-off (%v,%d,%d) disagree", pAllowed, pRetry, pEpoch, eAllowed, eRetry, eEpoch)
	}
	if granted != 400 || clamped {
		t.Fatalf("granted=%d clamped=%v, want the plain 400", granted, clamped)
	}
	for _, k := range sixKeys() {
		if p, e := plain.ReservedTokens(k), elastic.ReservedTokens(k); p != e {
			t.Errorf("bucket %q: plain holds %d, elastic-off holds %d", k, p, e)
		}
	}

	// No quota anywhere: nothing reserved, nothing clamped.
	allowed, _, epoch, granted, clamped := tokenQuotaReserveElasticInternal(ladderSvc(), rl.New(), "t1", "", "", "", "", 100, 512)
	if !allowed || epoch != 0 || granted != 0 || clamped {
		t.Fatalf("no quota: allowed=%v epoch=%d granted=%d clamped=%v", allowed, epoch, granted, clamped)
	}
	// No identity at all: nothing to reserve against.
	allowed, _, epoch, granted, _ = tokenQuotaReserveElasticInternal(svc, rl.New(), "", "", "", "", "", 100, 512)
	if !allowed || epoch != 0 || granted != 0 {
		t.Fatalf("no identity: allowed=%v epoch=%d granted=%d", allowed, epoch, granted)
	}
}

// TestDefaultReservationHoldingNothing: no prompt estimate and a bucket with
// nothing left. The request is admitted, nothing is held anywhere - the
// bucket that had granted the default gives it back - and there is no epoch
// to settle. It still counts as clamped: the default was asked and not held.
func TestDefaultReservationHoldingNothing(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.tenantTPM = 10000
	svc.modelTPM = map[string]int{"m": 300}
	if allowed, _, _ := store.ReserveTokens(modelQuotaKey("t1", "m"), 300, 300, 0); !allowed {
		t.Fatal("setup reservation refused")
	}

	allowed, _, epoch, granted, clamped := tokenQuotaReserveElasticInternal(svc, store, "t1", "m", "", "", "", -1, 512)
	if !allowed || epoch != 0 || granted != 0 || !clamped {
		t.Fatalf("allowed=%v epoch=%d granted=%d clamped=%v, want admitted holding nothing", allowed, epoch, granted, clamped)
	}
	if got := store.ReservedTokens("t1"); got != 0 {
		t.Fatalf("the tenant bucket kept %d of a default the model bucket could not hold", got)
	}
	if got := store.ReservedTokens(modelQuotaKey("t1", "m")); got != 300 {
		t.Fatalf("the model bucket holds %d, want the other request's 300", got)
	}
}

// TestDefaultReservationKeylessVipBucket: a keyless request has one bucket,
// the per-VIP shared one, and its default is clamped there like anywhere.
func TestDefaultReservationKeylessVipBucket(t *testing.T) {
	store := rl.New()
	svc := ladderSvc()
	svc.defaults["rule|"+resvSvc] = cmn.RateLimitDefaultsEntry{
		Scope: cmn.RateLimitScopeRule, RuleIdent: resvSvc, VipSharedTPM: 400,
	}
	allowed, _, epoch, granted, clamped := tokenQuotaReserveElasticInternal(svc, store, "", "", "", "", resvSvc, 100, 512)
	if !allowed || epoch == 0 || granted != 400 || !clamped {
		t.Fatalf("allowed=%v epoch=%d granted=%d clamped=%v, want 400 clamped", allowed, epoch, granted, clamped)
	}
}

// TestReservationClampedCountIsPerServiceAndReason: the read-back counter
// moves for the service and the reason that clamped and for no other.
func TestReservationClampedCountIsPerServiceAndReason(t *testing.T) {
	const a, b = "192.0.2.10:2050", "192.0.2.10:2051"
	burst, budget := reservationClampedCount(a, resvClampBurst), reservationClampedCount(a, resvClampBudget)
	noteReservationClamped(a, resvClampBurst)
	noteReservationClamped(a, resvClampBurst)
	noteReservationClamped(a, resvClampBudget)
	noteReservationClamped(a, resvClampNone) // not a clamp: counted nowhere
	if got := reservationClampedCount(a, resvClampBurst) - burst; got != 2 {
		t.Fatalf("service a counted %d for burst, want 2", got)
	}
	if got := reservationClampedCount(a, resvClampBudget) - budget; got != 1 {
		t.Fatalf("service a counted %d for budget, want 1", got)
	}
	if got := reservationClampedCount(b, resvClampBurst) + reservationClampedCount(b, resvClampBudget); got != 0 {
		t.Fatalf("service b counted %d, want 0", got)
	}

	var eff cmn.FcEffectiveArg
	aiDefaultResv{}.fillEffective(&eff, net.ParseIP("192.0.2.10"), 2050)
	if eff.ReservationClampedBurst != reservationClampedCount(a, resvClampBurst) ||
		eff.ReservationClampedBudget != reservationClampedCount(a, resvClampBudget) {
		t.Fatalf("read-back %d burst, %d budget does not match the counts",
			eff.ReservationClampedBurst, eff.ReservationClampedBudget)
	}
}

// TestClampReason: a default cut by what other claims left is "budget"; a
// default that an idle bucket cannot hold beside the prompt is "burst", and
// stays "burst" when a budget cut it further.
func TestClampReason(t *testing.T) {
	svc := ladderSvc()
	svc.tenantTPM = 600
	burst := int(rl.BurstTokens(600, 0))

	// Fits an idle bucket whole: no clamp.
	store := rl.New()
	_, _, _, granted, why := tokenQuotaReserveElasticReason(svc, store, "t1", "", "", "", "", 10, burst-10)
	if granted != burst || why != resvClampNone {
		t.Fatalf("idle bucket, claim of exactly the burst: granted=%d why=%v", granted, why)
	}
	// The same bucket, now taken: the next default is cut by budget.
	_, _, _, granted, why = tokenQuotaReserveElasticReason(svc, store, "t1", "", "", "", "", 0, 50)
	if granted != 0 || why != resvClampBudget {
		t.Fatalf("taken bucket: granted=%d why=%v, want nothing held, budget", granted, why)
	}

	// One token more than an idle bucket can hold beside the prompt: burst.
	store = rl.New()
	_, _, _, granted, why = tokenQuotaReserveElasticReason(svc, store, "t1", "", "", "", "", 10, burst-9)
	if granted != burst || why != resvClampBurst {
		t.Fatalf("default over the burst on an idle bucket: granted=%d why=%v, want %d, burst", granted, why, burst)
	}
	// Over the burst and the budget taken as well: still burst.
	_, _, _, granted, why = tokenQuotaReserveElasticReason(svc, store, "t1", "", "", "", "", 0, burst+1)
	if granted != 0 || why != resvClampBurst {
		t.Fatalf("over the burst on a taken bucket: granted=%d why=%v, want nothing held, burst", granted, why)
	}

	// A refusal of the firm part is not a clamp.
	if allowed, _, _, _, why := tokenQuotaReserveElasticReason(svc, rl.New(), "t1", "", "", "", "", burst+1, 50); allowed || why != resvClampNone {
		t.Fatalf("firm part over the burst: allowed=%v why=%v", allowed, why)
	}
	if resvClampBurst.String() != "burst" || resvClampBudget.String() != "budget" || resvClampNone.String() != "" {
		t.Fatal("the reason words are the metric's label values")
	}
}

// TestFillEffectiveShowsTheListenersValue: the read-back is what the gate
// runs the VIP and port on. A rule that resolves to another value sees the
// listener's, sourced "listener"; one that agrees keeps its own sources.
func TestFillEffectiveShowsTheListenersValue(t *testing.T) {
	t.Setenv(cmn.AIDefaultReservationEnv, "")
	t.Setenv(cmn.AIDefaultMaxTokensEnv, "")
	vip := net.ParseIP("192.0.2.78")
	u := func(v uint32) *uint32 { return &v }
	cases := []struct {
		name               string
		rule               aiDefaultResv
		listener           *uint32
		word               string
		tokens             uint32
		modeSrc, tokensSrc string
	}{
		{"not read: the rule's own", aiDefaultResv{mode: cmn.AIDefaultReservationRuleOn, maxTokens: 256}, nil, "on", 256, "rule", "rule"},
		{"agrees", aiDefaultResv{mode: cmn.AIDefaultReservationRuleOn, maxTokens: 256}, u(256), "on", 256, "rule", "rule"},
		{"listener holds another value", aiDefaultResv{mode: cmn.AIDefaultReservationRuleOn, maxTokens: 256}, u(512), "on", 512, "listener", "listener"},
		{"listener is off, rule declares on", aiDefaultResv{mode: cmn.AIDefaultReservationRuleOn, maxTokens: 256}, u(0), "off", 0, "listener", "listener"},
		{"listener is on, rule declares nothing", aiDefaultResv{}, u(512), "on", 512, "listener", "listener"},
		{"both off", aiDefaultResv{}, u(0), "off", 0, "default", "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eff := cmn.FcEffectiveArg{ListenerDefaultTokens: c.listener}
			c.rule.fillEffective(&eff, vip, 2050)
			if eff.DefaultReservation != c.word || eff.DefaultMaxTokens != c.tokens ||
				eff.Source.DefaultReservation != c.modeSrc || eff.Source.DefaultMaxTokens != c.tokensSrc {
				t.Fatalf("read-back %s/%d sources %s/%s, want %s/%d sources %s/%s",
					eff.DefaultReservation, eff.DefaultMaxTokens, eff.Source.DefaultReservation, eff.Source.DefaultMaxTokens,
					c.word, c.tokens, c.modeSrc, c.tokensSrc)
			}
		})
	}
}

// TestAIDefaultResvListenerWarning: silent while the rules of a VIP and port
// agree; otherwise it lists what they resolve to and names the value in
// effect when the data plane's could be read.
func TestAIDefaultResvListenerWarning(t *testing.T) {
	const svc = "192.0.2.79:2050"
	for _, agree := range [][]uint32{nil, {512}, {512, 512, 512}, {0, 0}} {
		if msg := aiDefaultResvListenerWarning(svc, agree, 512, true); msg != "" {
			t.Fatalf("rules %v agree, yet: %s", agree, msg)
		}
	}
	msg := aiDefaultResvListenerWarning(svc, []uint32{512, 256, 512, 0}, 512, true)
	for _, want := range []string{"ai_default_reservation", svc, "(off, 256 tokens, 512 tokens)", "per listener",
		"512 tokens is in effect for all of them"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("warning lacks %q: %s", want, msg)
		}
	}
	if msg := aiDefaultResvListenerWarning(svc, []uint32{256, 0}, 0, true); !strings.Contains(msg, "off is in effect for all of them") {
		t.Fatalf("a listener that is off is not named: %s", msg)
	}
	msg = aiDefaultResvListenerWarning(svc, []uint32{256, 512}, 0, false)
	if !strings.Contains(msg, "the first-installed rule's value is in effect") || strings.Contains(msg, "off is in effect") {
		t.Fatalf("with the listener unread the warning must not name a value: %s", msg)
	}
}

func TestAIServiceIdentMatchesTheDataPlane(t *testing.T) {
	if got := aiServiceIdent(net.ParseIP("10.10.10.254"), 2040); got != "10.10.10.254:2040" {
		t.Fatalf("aiServiceIdent = %q, want 10.10.10.254:2040", got)
	}
	if got := aiServiceIdent(net.ParseIP("2001:db8::1"), 2040); got != "" {
		t.Fatalf("aiServiceIdent of an IPv6 VIP = %q, want empty as the data plane leaves it", got)
	}
}

// TestAIDefaultResvResolve pins create and replace semantics: an omitted
// field keeps the stored value, inherit and an explicit 0 return it to
// unset, and bad values are refused with the field named.
func TestAIDefaultResvResolve(t *testing.T) {
	got, err := aiDefaultResvResolve(nil, &cmn.LbServiceArg{AIDefaultReservation: "on", AIDefaultMaxTokens: 2000})
	if err != nil || got != (aiDefaultResv{mode: cmn.AIDefaultReservationRuleOn, maxTokens: 2000}) {
		t.Fatalf("create: %+v err=%v", got, err)
	}
	if got, err := aiDefaultResvResolve(nil, &cmn.LbServiceArg{}); err != nil || got != (aiDefaultResv{}) {
		t.Fatalf("create with nothing declared: %+v err=%v", got, err)
	}

	stored := &ruleEnt{aiDefaultResv: aiDefaultResv{mode: cmn.AIDefaultReservationRuleOn, maxTokens: 2000}}
	if got, _ := aiDefaultResvResolve(stored, &cmn.LbServiceArg{}); got != stored.aiDefaultResv {
		t.Fatalf("a replace that names neither field changed them: %+v", got)
	}
	got, _ = aiDefaultResvResolve(stored, &cmn.LbServiceArg{AIDefaultMaxTokens: 4000})
	if got != (aiDefaultResv{mode: cmn.AIDefaultReservationRuleOn, maxTokens: 4000}) {
		t.Fatalf("a replace naming only the value: %+v", got)
	}
	got, _ = aiDefaultResvResolve(stored, &cmn.LbServiceArg{AIDefaultReservation: "off"})
	if got != (aiDefaultResv{mode: cmn.AIDefaultReservationRuleOff, maxTokens: 2000}) {
		t.Fatalf("a replace naming only the switch: %+v", got)
	}
	got, _ = aiDefaultResvResolve(stored, &cmn.LbServiceArg{
		AIDefaultReservation: "inherit", AIDefaultReservationPresent: true,
		AIDefaultMaxTokensPresent: true,
	})
	if got != (aiDefaultResv{}) {
		t.Fatalf("inherit and an explicit 0 must reset both fields: %+v", got)
	}

	if _, err := aiDefaultResvResolve(nil, &cmn.LbServiceArg{AIDefaultReservation: "yes"}); err == nil {
		t.Fatal("an unknown switch word was accepted")
	}
	if _, err := aiDefaultResvResolve(nil, &cmn.LbServiceArg{AIDefaultMaxTokens: cmn.AIDefaultMaxTokensMax + 1}); err == nil {
		t.Fatal("a value above the bound was accepted")
	}
	if _, err := aiDefaultResvResolve(nil, &cmn.LbServiceArg{AIDefaultMaxTokens: cmn.AIDefaultMaxTokensMax}); err != nil {
		t.Fatalf("the bound itself was refused: %v", err)
	}
}

// TestAIDefaultResvInForce pins the resolution order - rule, environment,
// off - for both halves, the built-in value, and that the data plane gets
// 0 whenever the switch is off.
func TestAIDefaultResvInForce(t *testing.T) {
	on, off := cmn.AIDefaultReservationRuleOn, cmn.AIDefaultReservationRuleOff
	cases := []struct {
		name            string
		rule            aiDefaultResv
		envMode, envTok string
		wantTokens      uint32
		wantModeSrc     string
		wantTokSrc      string
	}{
		{"nothing declared anywhere", aiDefaultResv{}, "", "", 0, "default", "default"},
		{"rule on, built-in value", aiDefaultResv{mode: on}, "", "", 512, "rule", "default"},
		{"rule on, rule value", aiDefaultResv{mode: on, maxTokens: 2000}, "", "9000", 2000, "rule", "rule"},
		{"rule on, environment value", aiDefaultResv{mode: on}, "", "9000", 9000, "rule", "env"},
		{"environment on, built-in value", aiDefaultResv{}, "on", "", 512, "env", "default"},
		{"environment on, rule value", aiDefaultResv{maxTokens: 300}, "on", "", 300, "env", "rule"},
		{"rule off beats an enabling environment", aiDefaultResv{mode: off, maxTokens: 300}, "on", "9000", 0, "rule", "rule"},
		{"environment off", aiDefaultResv{}, "off", "9000", 0, "env", "env"},
		{"a rule value alone enables nothing", aiDefaultResv{maxTokens: 300}, "", "", 0, "default", "rule"},
		{"an environment word that is not a switch is unset", aiDefaultResv{}, "yes", "", 0, "default", "default"},
		{"an environment value out of range is unset", aiDefaultResv{mode: on}, "", "100000001", 512, "rule", "default"},
		{"an environment value of 0 is unset", aiDefaultResv{mode: on}, "", "0", 512, "rule", "default"},
		{"an environment value that is not a number is unset", aiDefaultResv{mode: on}, "", "-1", 512, "rule", "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(cmn.AIDefaultReservationEnv, c.envMode)
			t.Setenv(cmn.AIDefaultMaxTokensEnv, c.envTok)
			gotOn, tokens, modeSrc, tokSrc := c.rule.inForce()
			if tokens != c.wantTokens || modeSrc != c.wantModeSrc || tokSrc != c.wantTokSrc || gotOn != (c.wantTokens != 0) {
				t.Fatalf("on=%v tokens=%d sources=%s/%s, want tokens=%d sources=%s/%s",
					gotOn, tokens, modeSrc, tokSrc, c.wantTokens, c.wantModeSrc, c.wantTokSrc)
			}
			if got := c.rule.tokensInForce(); got != c.wantTokens {
				t.Fatalf("the data plane gets %d, want %d", got, c.wantTokens)
			}

			var eff cmn.FcEffectiveArg
			c.rule.fillEffective(&eff, net.ParseIP("192.0.2.77"), 2050)
			wantWord := "off"
			if c.wantTokens != 0 {
				wantWord = "on"
			}
			if eff.DefaultReservation != wantWord || eff.DefaultMaxTokens != c.wantTokens ||
				eff.Source.DefaultReservation != c.wantModeSrc || eff.Source.DefaultMaxTokens != c.wantTokSrc {
				t.Fatalf("read-back %+v / %+v", eff, eff.Source)
			}
		})
	}
}

func resvListenerRule(model, ip string, port uint16, proto uint8, sse bool, resv aiDefaultResv) *ruleEnt {
	return &ruleEnt{
		tuples: ruleTuples{
			l3Dst:     ruleIPTuple{net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(32, 32)}},
			l4Prot:    rule8Tuple{proto, 0xff},
			l4Dst:     rule16RTuple{port, port, true},
			modelName: model,
		},
		sseMode:       sse,
		aiDefaultResv: resv,
	}
}

// TestAIDefaultResvOnListener: the warning looks at the AI-gateway rules of
// one VIP, port and protocol, and at no other rule; with no data plane to
// read, it does not name a value in effect.
func TestAIDefaultResvOnListener(t *testing.T) {
	t.Setenv(cmn.AIDefaultReservationEnv, "")
	t.Setenv(cmn.AIDefaultMaxTokensEnv, "")
	savedZr, savedDp := mh.zr, mh.dpEbpf
	defer func() { mh.zr, mh.dpEbpf = savedZr, savedDp }()
	mh.dpEbpf = nil

	on := func(n uint32) aiDefaultResv {
		return aiDefaultResv{mode: cmn.AIDefaultReservationRuleOn, maxTokens: n}
	}
	R := &RuleH{}
	R.tables[RtLB].eMap = map[string]*ruleEnt{
		"a":         resvListenerRule("model-a", "192.0.2.80", 2050, 6, true, on(800)),
		"b":         resvListenerRule("model-b", "192.0.2.80", 2050, 6, true, on(256)),
		"c":         resvListenerRule("model-c", "192.0.2.80", 2050, 6, true, aiDefaultResv{}),
		"otherport": resvListenerRule("model-a", "192.0.2.80", 2051, 6, true, on(4000)),
		"othervip":  resvListenerRule("model-a", "192.0.2.81", 2050, 6, true, on(4000)),
		"otherprot": resvListenerRule("model-a", "192.0.2.80", 2050, 132, true, on(4000)),
		"notai":     resvListenerRule("", "192.0.2.80", 2050, 6, false, on(4000)),
	}
	mh.zr = &Zone{Rules: R}

	resolved, _, known := R.aiDefaultResvOnListener(net.ParseIP("192.0.2.80"), 2050, "tcp")
	slices.Sort(resolved)
	if known || !slices.Equal(resolved, []uint32{0, 256, 800}) {
		t.Fatalf("resolved=%v known=%v, want the three AI rules of the listener (0, 256, 800), unread", resolved, known)
	}
	if resolved, _, _ := R.aiDefaultResvOnListener(net.ParseIP("192.0.2.80"), 2051, "tcp"); !slices.Equal(resolved, []uint32{4000}) {
		t.Fatalf("the other port resolves %v, want its one rule", resolved)
	}
	if resolved, _, _ := R.aiDefaultResvOnListener(net.ParseIP("192.0.2.80"), 2050, "udp"); len(resolved) != 0 {
		t.Fatalf("a protocol with no rule resolves %v", resolved)
	}

	na := &NetAPIStruct{}
	msg := na.NetAIDefaultReservationWarning(&cmn.LbRuleMod{Serv: cmn.LbServiceArg{ServIP: "192.0.2.80", ServPort: 2050, Proto: "tcp"}})
	for _, want := range []string{"192.0.2.80:2050", "(off, 256 tokens, 800 tokens)", "the first-installed rule's value is in effect"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("warning lacks %q: %s", want, msg)
		}
	}
	for _, quiet := range []*cmn.LbRuleMod{
		nil,
		{Serv: cmn.LbServiceArg{ServIP: "192.0.2.80", ServPort: 2051, Proto: "tcp"}},
		{Serv: cmn.LbServiceArg{ServIP: "192.0.2.99", ServPort: 2050, Proto: "tcp"}},
		{Serv: cmn.LbServiceArg{ServIP: "2001:db8::1", ServPort: 2050, Proto: "tcp"}},
		{Serv: cmn.LbServiceArg{ServIP: "not-an-address", ServPort: 2050, Proto: "tcp"}},
	} {
		if msg := na.NetAIDefaultReservationWarning(quiet); msg != "" {
			t.Fatalf("%+v: got %q, want no warning", quiet, msg)
		}
	}

	for p, w := range map[uint8]string{6: "tcp", 17: "udp", 1: "icmp", 132: "sctp", 0: "none", 99: ""} {
		if got := lbProtoWord(p); got != w {
			t.Fatalf("lbProtoWord(%d) = %q, want %q", p, got, w)
		}
	}
}
