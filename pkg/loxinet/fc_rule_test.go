/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
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
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
)

// A replace keeps each gate field it omits and replaces each one it
// carries, zero and "inherit" included; a create takes the request as is.
// Out-of-range values are refused here too, for callers without the
// handler's checks (a snapshot restore, the Go API).
func TestFcRuleResolveOnReplace(t *testing.T) {
	stored := &ruleEnt{fcCfg: fcRuleCfg{mode: cmn.FcRuleModeEnforce, maxOutstanding: 8,
		epMaxInflight: 4, prefillMaxInflight: 2, decodeMaxInflight: 6, telemetryStaleMs: 60000}}
	cases := []struct {
		name  string
		eRule *ruleEnt
		serv  cmn.LbServiceArg
		want  fcRuleCfg
	}{
		{"create: declared", nil,
			cmn.LbServiceArg{FcMode: "observe", FcMaxOutstanding: 3, FcTelemetryStaleMs: 1000},
			fcRuleCfg{mode: cmn.FcRuleModeObserve, maxOutstanding: 3, telemetryStaleMs: 1000}},
		{"create: nothing inherits", nil, cmn.LbServiceArg{}, fcRuleCfg{}},
		{"replace: nothing present keeps all", stored, cmn.LbServiceArg{}, stored.fcCfg},
		{"replace: one ceiling changes, the rest kept", stored,
			cmn.LbServiceArg{FcMaxOutstanding: 16, FcMaxOutstandingPresent: true},
			fcRuleCfg{mode: cmn.FcRuleModeEnforce, maxOutstanding: 16, epMaxInflight: 4,
				prefillMaxInflight: 2, decodeMaxInflight: 6, telemetryStaleMs: 60000}},
		{"replace: explicit zero and inherit reset", stored,
			cmn.LbServiceArg{FcMode: "inherit", FcModePresent: true, FcDecodeMaxInflightPresent: true,
				FcTelemetryStaleMsPresent: true},
			fcRuleCfg{maxOutstanding: 8, epMaxInflight: 4, prefillMaxInflight: 2}},
		{"replace: the gate switched off", stored,
			cmn.LbServiceArg{FcMode: "off", FcModePresent: true},
			fcRuleCfg{mode: cmn.FcRuleModeOff, maxOutstanding: 8, epMaxInflight: 4,
				prefillMaxInflight: 2, decodeMaxInflight: 6, telemetryStaleMs: 60000}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := fcRuleResolve(c.eRule, &c.serv)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got != c.want {
				t.Fatalf("resolved %+v, want %+v", got, c.want)
			}
		})
	}

	for _, c := range []struct {
		name string
		serv cmn.LbServiceArg
	}{
		{"an unknown mode", cmn.LbServiceArg{FcMode: "yes"}},
		{"a ceiling above 100000", cmn.LbServiceArg{FcEpMaxInflight: cmn.FcCapMax + 1}},
		{"a window above an hour", cmn.LbServiceArg{FcTelemetryStaleMs: cmn.FcTelemetryStaleMsMax + 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := fcRuleResolve(nil, &c.serv)
			var invalid *cmn.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("err=%v, want a validation refusal", err)
			}
		})
	}
}

// The read-back carries the declaration back onto the service arguments;
// an inherited mode reads as empty, so it is omitted.
func TestFcRuleToServ(t *testing.T) {
	var s cmn.LbServiceArg
	fcRuleCfg{mode: cmn.FcRuleModeObserve, maxOutstanding: 3, decodeMaxInflight: 6}.toServ(&s)
	if s.FcMode != "observe" || s.FcMaxOutstanding != 3 || s.FcDecodeMaxInflight != 6 {
		t.Fatalf("read back %+v", s)
	}
	fcRuleCfg{}.toServ(&s)
	if s.FcMode != "" || s.FcMaxOutstanding != 0 {
		t.Fatalf("an inherited declaration read back %+v", s)
	}
}

// The adaptive switch, the warm-up window and the TTFT target follow the
// replace rules of the rest of the gate: omitted keeps, present replaces.
func TestFcRuleAdaptiveOnReplace(t *testing.T) {
	stored := &ruleEnt{fcCfg: fcRuleCfg{maxOutstanding: 8, adaptive: cmn.FcRuleAdaptiveOn,
		warmupMs: 20000, ttftTargetMs: 800}}
	for _, c := range []struct {
		name  string
		eRule *ruleEnt
		serv  cmn.LbServiceArg
		want  fcRuleCfg
	}{
		{"create: declared", nil,
			cmn.LbServiceArg{FcAdaptive: "on", FcWarmupMs: 5000, FcTtftTargetMs: 300},
			fcRuleCfg{adaptive: cmn.FcRuleAdaptiveOn, warmupMs: 5000, ttftTargetMs: 300}},
		{"replace: nothing present keeps all", stored, cmn.LbServiceArg{}, stored.fcCfg},
		{"replace: switched off, windows kept", stored,
			cmn.LbServiceArg{FcAdaptive: "off", FcAdaptivePresent: true},
			fcRuleCfg{maxOutstanding: 8, adaptive: cmn.FcRuleAdaptiveOff, warmupMs: 20000, ttftTargetMs: 800}},
		{"replace: inherit and explicit zeros reset", stored,
			cmn.LbServiceArg{FcAdaptive: "inherit", FcAdaptivePresent: true,
				FcWarmupMsPresent: true, FcTtftTargetMsPresent: true},
			fcRuleCfg{maxOutstanding: 8}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := fcRuleResolve(c.eRule, &c.serv)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got != c.want {
				t.Fatalf("resolved %+v, want %+v", got, c.want)
			}
		})
	}
	for _, c := range []struct {
		name string
		serv cmn.LbServiceArg
	}{
		{"an unknown switch", cmn.LbServiceArg{FcAdaptive: "yes"}},
		{"a warm-up above an hour", cmn.LbServiceArg{FcWarmupMs: cmn.FcWarmupMsMax + 1}},
		{"a TTFT target above an hour", cmn.LbServiceArg{FcTtftTargetMs: cmn.FcTtftTargetMsMax + 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := fcRuleResolve(nil, &c.serv)
			var invalid *cmn.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("err=%v, want a validation refusal", err)
			}
		})
	}

	var s cmn.LbServiceArg
	stored.fcCfg.toServ(&s)
	if s.FcAdaptive != "on" || s.FcWarmupMs != 20000 || s.FcTtftTargetMs != 800 {
		t.Fatalf("read back %+v", s)
	}
	fcRuleCfg{}.toServ(&s)
	if s.FcAdaptive != "" {
		t.Fatalf("an inherited switch read back %q", s.FcAdaptive)
	}
}

// Whether the pool adapts, and so whether the rule needs its scraper: the
// rule's own switch wins; with none, the process default decides.
func TestFcRuleAdaptiveInForce(t *testing.T) {
	t.Setenv("LLB_FC_ADAPTIVE", "on")
	if !(fcRuleCfg{}).adaptiveInForce() {
		t.Fatal("an inheriting rule under LLB_FC_ADAPTIVE=on does not adapt")
	}
	if (fcRuleCfg{adaptive: cmn.FcRuleAdaptiveOff}).adaptiveInForce() {
		t.Fatal("a rule that switched it off adapts")
	}
	t.Setenv("LLB_FC_ADAPTIVE", "")
	if (fcRuleCfg{}).adaptiveInForce() {
		t.Fatal("an inheriting rule adapts with no process default")
	}
	if !(fcRuleCfg{adaptive: cmn.FcRuleAdaptiveOn}).adaptiveInForce() {
		t.Fatal("a rule that switched it on does not adapt")
	}
}

// The tenant share follows the replace rules of the rest of the gate and is
// a percentage: omitted keeps, present replaces, above 100 is refused.
func TestFcRuleTenantShareOnReplace(t *testing.T) {
	stored := &ruleEnt{fcCfg: fcRuleCfg{maxOutstanding: 8, tenantSharePct: 25}}
	for _, c := range []struct {
		name  string
		eRule *ruleEnt
		serv  cmn.LbServiceArg
		want  fcRuleCfg
	}{
		{"create: declared", nil, cmn.LbServiceArg{FcTenantMaxSharePct: 40},
			fcRuleCfg{tenantSharePct: 40}},
		{"replace: omitted keeps", stored, cmn.LbServiceArg{FcMaxOutstanding: 9,
			FcMaxOutstandingPresent: true}, fcRuleCfg{maxOutstanding: 9, tenantSharePct: 25}},
		{"replace: present replaces", stored, cmn.LbServiceArg{FcTenantMaxSharePct: 100,
			FcTenantMaxSharePctPresent: true}, fcRuleCfg{maxOutstanding: 8, tenantSharePct: 100}},
		{"replace: explicit zero inherits", stored,
			cmn.LbServiceArg{FcTenantMaxSharePctPresent: true}, fcRuleCfg{maxOutstanding: 8}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := fcRuleResolve(c.eRule, &c.serv)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got != c.want {
				t.Fatalf("resolved %+v, want %+v", got, c.want)
			}
		})
	}
	_, err := fcRuleResolve(nil, &cmn.LbServiceArg{FcTenantMaxSharePct: cmn.FcTenantMaxSharePctMax + 1})
	var invalid *cmn.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("a share above 100: err=%v, want a validation refusal", err)
	}
	var s cmn.LbServiceArg
	stored.fcCfg.toServ(&s)
	if s.FcTenantMaxSharePct != 25 {
		t.Fatalf("read back %d", s.FcTenantMaxSharePct)
	}
}

// The admission headers switch follows the adaptive switch's replace rules:
// omitted keeps, present replaces, "inherit" clears, anything else is refused.
func TestFcRuleExposeHeadersOnReplace(t *testing.T) {
	stored := &ruleEnt{fcCfg: fcRuleCfg{maxOutstanding: 8, exposeHeaders: cmn.FcRuleAdaptiveOn}}
	for _, c := range []struct {
		name  string
		eRule *ruleEnt
		serv  cmn.LbServiceArg
		want  fcRuleCfg
	}{
		{"create: on", nil, cmn.LbServiceArg{FcExposeHeaders: "on"},
			fcRuleCfg{exposeHeaders: cmn.FcRuleAdaptiveOn}},
		{"create: off", nil, cmn.LbServiceArg{FcExposeHeaders: "off"},
			fcRuleCfg{exposeHeaders: cmn.FcRuleAdaptiveOff}},
		{"replace: omitted keeps", stored, cmn.LbServiceArg{FcMaxOutstanding: 9,
			FcMaxOutstandingPresent: true},
			fcRuleCfg{maxOutstanding: 9, exposeHeaders: cmn.FcRuleAdaptiveOn}},
		{"replace: present replaces", stored, cmn.LbServiceArg{FcExposeHeaders: "off",
			FcExposeHeadersPresent: true},
			fcRuleCfg{maxOutstanding: 8, exposeHeaders: cmn.FcRuleAdaptiveOff}},
		{"replace: inherit clears", stored, cmn.LbServiceArg{FcExposeHeaders: "inherit",
			FcExposeHeadersPresent: true}, fcRuleCfg{maxOutstanding: 8}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := fcRuleResolve(c.eRule, &c.serv)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got != c.want {
				t.Fatalf("resolved %+v, want %+v", got, c.want)
			}
		})
	}
	_, err := fcRuleResolve(nil, &cmn.LbServiceArg{FcExposeHeaders: "yes"})
	var invalid *cmn.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("an unknown switch value: err=%v, want a validation refusal", err)
	}
	var s cmn.LbServiceArg
	stored.fcCfg.toServ(&s)
	if s.FcExposeHeaders != "on" {
		t.Fatalf("read back %q", s.FcExposeHeaders)
	}
}

// The headers are refused where the kernel may carry the responses, and only
// there; a restore replay is let through.
func TestFcExposeSockMapErr(t *testing.T) {
	on := fcRuleCfg{exposeHeaders: cmn.FcRuleAdaptiveOn}
	for _, c := range []struct {
		name    string
		cfg     fcRuleCfg
		code    uint8
		replay  bool
		refused bool
	}{
		{"on, sockmap off", on, 0, false, false},
		{"on, sockmap both", on, 1, false, true},
		{"on, sockmap request", on, 2, false, false},
		{"on, sockmap response", on, 3, false, true},
		{"off, sockmap both", fcRuleCfg{exposeHeaders: cmn.FcRuleAdaptiveOff}, 1, false, false},
		{"inherit, sockmap both", fcRuleCfg{}, 1, false, false},
		{"on, sockmap both, restore", on, 1, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			serv := cmn.LbServiceArg{ServIP: "10.0.0.1", ServPort: 80,
				SockMapMode: cmn.SockMapCodeToMode(c.code), RestoreReplay: c.replay}
			err := fcExposeSockMapErr(&serv, c.cfg, c.code)
			var invalid *cmn.ValidationError
			if c.refused != (err != nil) || (err != nil && !errors.As(err, &invalid)) {
				t.Fatalf("err=%v, want refused=%v as a validation error", err, c.refused)
			}
		})
	}
}

// A triple is written critical:standard:sheddable and indexed by band, as
// the data plane indexes it; the read-back writes it the way it was posted,
// and three zeros, the reset value, read as empty.
func TestFcBandTripleParseAndFormat(t *testing.T) {
	got, err := cmn.FcBandTripleParse("fc_band_weights", "8:3:1", cmn.FcBandWeightMax)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got[cmn.FcBandCritical] != 8 || got[cmn.FcBandStandard] != 3 || got[cmn.FcBandSheddable] != 1 {
		t.Fatalf("8:3:1 indexed as %v", got)
	}
	if got != ([cmn.FcBands]uint32{3, 8, 1}) {
		t.Fatalf("8:3:1 is %v by band index, want [3 8 1] (0 standard, 1 critical, 2 sheddable)", got)
	}
	for _, v := range []string{"8:3:1", "0:20:0", "3600000:0:1", "0:0:7"} {
		tr, err := cmn.FcBandTripleParse("fc_band_wait_ms", v, cmn.FcBandWaitMsMax)
		if err != nil {
			t.Fatalf("%s refused: %v", v, err)
		}
		if back := cmn.FcBandTripleFormat(tr); back != v {
			t.Fatalf("%s read back as %q", v, back)
		}
	}
	for _, v := range []string{"", "0:0:0"} {
		tr, err := cmn.FcBandTripleParse("fc_band_queue_pct", v, cmn.FcBandPctMax)
		if err != nil || tr != ([cmn.FcBands]uint32{}) {
			t.Fatalf("%q parsed as %v, %v; want three zeros", v, tr, err)
		}
		if back := cmn.FcBandTripleFormat(tr); back != "" {
			t.Fatalf("%q read back as %q, want empty", v, back)
		}
	}
	for _, v := range []string{"8:3", "8:3:1:0", "8::1", ":3:1", "8:3:", "-1:3:1", "+1:3:1",
		"a:b:c", "8: 3:1", " 8:3:1", "8,3,1", "1.5:3:1", "0x8:3:1", "4294967296:0:0"} {
		_, err := cmn.FcBandTripleParse("fc_band_weights", v, cmn.FcBandWeightMax)
		var invalid *cmn.ValidationError
		if !errors.As(err, &invalid) || invalid.Field != "fc_band_weights" {
			t.Fatalf("%q: err=%v, want a validation refusal naming fc_band_weights", v, err)
		}
	}
	_, err = cmn.FcBandTripleParse("fc_band_weights", "63:0:0", cmn.FcBandWeightMax)
	if err == nil {
		t.Fatal("a weight above 62 was accepted")
	}
}

// fcBandStored is a rule with every band knob declared.
func fcBandStored() *ruleEnt {
	return &ruleEnt{fcMaxQueueDepth: 4, fcCfg: fcRuleCfg{maxOutstanding: 8,
		wakePolicy: cmn.FcRuleWakeWeighted, bandWeight: [cmn.FcBands]uint32{3, 8, 1},
		bandMinSharePct: [cmn.FcBands]uint32{20, 0, 0}, criticalReservePct: 10,
		bandWaitMs: [cmn.FcBands]uint32{2000, 500, 1000}, bandQueuePct: [cmn.FcBands]uint32{50, 30, 20},
		shedQueue: cmn.FcRuleAdaptiveOn, tenantWake: cmn.FcRuleTenantWakeRR, llmdHeaders: cmn.FcRuleAdaptiveOn}}
}

// Each band knob follows the replace rules of the rest of the gate:
// omitted keeps the stored value, present replaces it, the reset value
// ("0:0:0", 0 or "inherit") clears it.
func TestFcRuleBandOnReplace(t *testing.T) {
	t.Setenv("LLB_FC_MAX_QUEUE_DEPTH", "")
	stored := fcBandStored()
	with := func(f func(*fcRuleCfg)) fcRuleCfg {
		c := stored.fcCfg
		f(&c)
		return c
	}
	for _, c := range []struct {
		name  string
		eRule *ruleEnt
		serv  cmn.LbServiceArg
		want  fcRuleCfg
	}{
		{"create: declared", nil,
			cmn.LbServiceArg{FcWakePolicy: "weighted", FcBandWeights: "8:3:1", FcBandMinSharePct: "0:20:0",
				FcCriticalReservePct: 10, FcBandWaitMs: "500:2000:1000", FcBandQueuePct: "30:50:20",
				FcShedQueue: "on", FcTenantWake: "rr", FcLlmdHeaders: "on", FcMaxQueueDepth: 4},
			with(func(c *fcRuleCfg) { c.maxOutstanding = 0 })},
		{"create: nothing declared is every default", nil, cmn.LbServiceArg{}, fcRuleCfg{}},
		{"replace: nothing present keeps all", stored, cmn.LbServiceArg{}, stored.fcCfg},
		{"replace: another field only, the bands kept", stored,
			cmn.LbServiceArg{FcMaxOutstanding: 9, FcMaxOutstandingPresent: true},
			with(func(c *fcRuleCfg) { c.maxOutstanding = 9 })},
		{"replace: wake policy strict, weights kept unused", stored,
			cmn.LbServiceArg{FcWakePolicy: "strict", FcWakePolicyPresent: true},
			with(func(c *fcRuleCfg) { c.wakePolicy = cmn.FcRuleWakeStrict })},
		{"replace: wake policy inherit clears", stored,
			cmn.LbServiceArg{FcWakePolicy: "inherit", FcWakePolicyPresent: true},
			with(func(c *fcRuleCfg) { c.wakePolicy = cmn.FcRuleWakeInherit })},
		{"replace: weights replaced", stored,
			cmn.LbServiceArg{FcBandWeights: "4:2:1", FcBandWeightsPresent: true},
			with(func(c *fcRuleCfg) { c.bandWeight = [cmn.FcBands]uint32{2, 4, 1} })},
		{"replace: weights reset beside strict", stored,
			cmn.LbServiceArg{FcWakePolicy: "strict", FcWakePolicyPresent: true,
				FcBandWeights: "0:0:0", FcBandWeightsPresent: true},
			with(func(c *fcRuleCfg) {
				c.wakePolicy = cmn.FcRuleWakeStrict
				c.bandWeight = [cmn.FcBands]uint32{}
			})},
		{"replace: floors replaced", stored,
			cmn.LbServiceArg{FcBandMinSharePct: "0:30:10", FcBandMinSharePctPresent: true},
			with(func(c *fcRuleCfg) { c.bandMinSharePct = [cmn.FcBands]uint32{30, 0, 10} })},
		{"replace: floors reset", stored,
			cmn.LbServiceArg{FcBandMinSharePct: "0:0:0", FcBandMinSharePctPresent: true},
			with(func(c *fcRuleCfg) { c.bandMinSharePct = [cmn.FcBands]uint32{} })},
		{"replace: floors reset by an empty value", stored,
			cmn.LbServiceArg{FcBandMinSharePctPresent: true},
			with(func(c *fcRuleCfg) { c.bandMinSharePct = [cmn.FcBands]uint32{} })},
		{"replace: reserve replaced", stored,
			cmn.LbServiceArg{FcCriticalReservePct: 5, FcCriticalReservePctPresent: true},
			with(func(c *fcRuleCfg) { c.criticalReservePct = 5 })},
		{"replace: reserve reset", stored,
			cmn.LbServiceArg{FcCriticalReservePctPresent: true},
			with(func(c *fcRuleCfg) { c.criticalReservePct = 0 })},
		{"replace: waits replaced", stored,
			cmn.LbServiceArg{FcBandWaitMs: "100:200:300", FcBandWaitMsPresent: true},
			with(func(c *fcRuleCfg) { c.bandWaitMs = [cmn.FcBands]uint32{200, 100, 300} })},
		{"replace: waits reset", stored,
			cmn.LbServiceArg{FcBandWaitMs: "0:0:0", FcBandWaitMsPresent: true},
			with(func(c *fcRuleCfg) { c.bandWaitMs = [cmn.FcBands]uint32{} })},
		{"replace: queue shares replaced", stored,
			cmn.LbServiceArg{FcBandQueuePct: "10:10:10", FcBandQueuePctPresent: true},
			with(func(c *fcRuleCfg) { c.bandQueuePct = [cmn.FcBands]uint32{10, 10, 10} })},
		{"replace: queue shares reset", stored,
			cmn.LbServiceArg{FcBandQueuePct: "0:0:0", FcBandQueuePctPresent: true},
			with(func(c *fcRuleCfg) { c.bandQueuePct = [cmn.FcBands]uint32{} })},
		{"replace: shed queue off", stored,
			cmn.LbServiceArg{FcShedQueue: "off", FcShedQueuePresent: true},
			with(func(c *fcRuleCfg) { c.shedQueue = cmn.FcRuleAdaptiveOff })},
		{"replace: shed queue inherit clears", stored,
			cmn.LbServiceArg{FcShedQueue: "inherit", FcShedQueuePresent: true},
			with(func(c *fcRuleCfg) { c.shedQueue = cmn.FcRuleAdaptiveInherit })},
		{"replace: tenant wake fifo", stored,
			cmn.LbServiceArg{FcTenantWake: "fifo", FcTenantWakePresent: true},
			with(func(c *fcRuleCfg) { c.tenantWake = cmn.FcRuleTenantWakeFifo })},
		{"replace: tenant wake inherit clears", stored,
			cmn.LbServiceArg{FcTenantWake: "inherit", FcTenantWakePresent: true},
			with(func(c *fcRuleCfg) { c.tenantWake = cmn.FcRuleTenantWakeInherit })},
		{"replace: llm-d headers off", stored,
			cmn.LbServiceArg{FcLlmdHeaders: "off", FcLlmdHeadersPresent: true},
			with(func(c *fcRuleCfg) { c.llmdHeaders = cmn.FcRuleAdaptiveOff })},
		{"replace: llm-d headers inherit clears", stored,
			cmn.LbServiceArg{FcLlmdHeaders: "inherit", FcLlmdHeadersPresent: true},
			with(func(c *fcRuleCfg) { c.llmdHeaders = cmn.FcRuleAdaptiveInherit })},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := fcRuleResolve(c.eRule, &c.serv)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got != c.want {
				t.Fatalf("resolved %+v, want %+v", got, c.want)
			}
		})
	}

	var s cmn.LbServiceArg
	stored.fcCfg.toServ(&s)
	if s.FcWakePolicy != "weighted" || s.FcBandWeights != "8:3:1" || s.FcBandMinSharePct != "0:20:0" ||
		s.FcCriticalReservePct != 10 || s.FcBandWaitMs != "500:2000:1000" || s.FcBandQueuePct != "30:50:20" ||
		s.FcShedQueue != "on" || s.FcTenantWake != "rr" || s.FcLlmdHeaders != "on" {
		t.Fatalf("read back %+v", s)
	}
	fcRuleCfg{}.toServ(&s)
	if s.FcWakePolicy != "" || s.FcBandWeights != "" || s.FcBandMinSharePct != "" ||
		s.FcCriticalReservePct != 0 || s.FcBandWaitMs != "" || s.FcBandQueuePct != "" ||
		s.FcShedQueue != "" || s.FcTenantWake != "" || s.FcLlmdHeaders != "" {
		t.Fatalf("a declaration with no band knob read back %+v", s)
	}
}

// The rules that span the band knobs are judged on the declaration that
// will be stored, each refusal naming its field; the rows the design
// accepts are accepted.
func TestFcRuleBandRefusals(t *testing.T) {
	t.Setenv("LLB_FC_MAX_QUEUE_DEPTH", "")
	strictNoWeights := fcBandStored()
	strictNoWeights.fcCfg.wakePolicy = cmn.FcRuleWakeStrict
	strictNoWeights.fcCfg.bandWeight = [cmn.FcBands]uint32{}
	for _, c := range []struct {
		name  string
		eRule *ruleEnt
		serv  cmn.LbServiceArg
		field string
	}{
		{"an unknown wake policy", nil, cmn.LbServiceArg{FcWakePolicy: "fair"}, "fc_wake_policy"},
		{"an unknown shed queue switch", nil, cmn.LbServiceArg{FcShedQueue: "yes"}, "fc_shed_queue"},
		{"an unknown tenant wake", nil, cmn.LbServiceArg{FcTenantWake: "lifo"}, "fc_tenant_wake"},
		{"an unknown llm-d headers switch", nil, cmn.LbServiceArg{FcLlmdHeaders: "yes"}, "fc_llmd_headers"},
		{"malformed weights", nil, cmn.LbServiceArg{FcBandWeights: "8:3"}, "fc_band_weights"},
		{"malformed floors", nil, cmn.LbServiceArg{FcBandMinSharePct: "a:b:c"}, "fc_band_min_share_pct"},
		{"malformed waits", nil, cmn.LbServiceArg{FcBandWaitMs: "1:2:3:4"}, "fc_band_wait_ms"},
		{"malformed queue shares", nil, cmn.LbServiceArg{FcBandQueuePct: "-1:0:0"}, "fc_band_queue_pct"},
		{"a weight above 62", nil, cmn.LbServiceArg{FcBandWeights: "63:1:0"}, "fc_band_weights"},
		{"a floor above 100", nil, cmn.LbServiceArg{FcBandMinSharePct: "0:101:0"}, "fc_band_min_share_pct"},
		{"a wait above an hour", nil, cmn.LbServiceArg{FcBandWaitMs: "0:0:3600001"}, "fc_band_wait_ms"},
		{"a queue share above 100", nil, cmn.LbServiceArg{FcBandQueuePct: "101:0:0"}, "fc_band_queue_pct"},
		{"a reserve above 100", nil, cmn.LbServiceArg{FcCriticalReservePct: 101}, "fc_critical_reserve_pct"},
		{"weights above a round of 64", nil, cmn.LbServiceArg{FcBandWeights: "62:2:1"}, "fc_band_weights"},
		{"weighted with a zero weight", nil,
			cmn.LbServiceArg{FcWakePolicy: "weighted", FcBandWeights: "8:3:0"}, "fc_band_weights"},
		{"weighted with no weights", nil, cmn.LbServiceArg{FcWakePolicy: "weighted"}, "fc_band_weights"},
		{"weighted with weights declared 0:0:0", nil,
			cmn.LbServiceArg{FcWakePolicy: "weighted", FcBandWeights: "0:0:0", FcBandWeightsPresent: true}, "fc_band_weights"},
		{"a zero weight under strict", nil,
			cmn.LbServiceArg{FcWakePolicy: "strict", FcBandWeights: "8:0:1"}, "fc_band_weights"},
		{"a zero weight under an inherited policy", nil, cmn.LbServiceArg{FcBandWeights: "0:3:1"}, "fc_band_weights"},
		{"a zero weight by replace", strictNoWeights,
			cmn.LbServiceArg{FcBandWeights: "8:3:0", FcBandWeightsPresent: true}, "fc_band_weights"},
		{"weighted by replace over no weights", strictNoWeights,
			cmn.LbServiceArg{FcWakePolicy: "weighted", FcWakePolicyPresent: true}, "fc_band_weights"},
		{"weights reset under weighted", fcBandStored(),
			cmn.LbServiceArg{FcBandWeights: "0:0:0", FcBandWeightsPresent: true}, "fc_band_weights"},
		{"floors above 100", nil, cmn.LbServiceArg{FcBandMinSharePct: "50:40:20"}, "fc_band_min_share_pct"},
		{"the reserve and the other two floors above 100", nil,
			cmn.LbServiceArg{FcCriticalReservePct: 60, FcBandMinSharePct: "0:30:20"}, "fc_critical_reserve_pct"},
		{"the reserve and the other two floors above 100, the critical floor not counted", nil,
			cmn.LbServiceArg{FcCriticalReservePct: 51, FcBandMinSharePct: "0:30:20"}, "fc_critical_reserve_pct"},
		{"the floors above 100 beside a reserve", nil,
			cmn.LbServiceArg{FcCriticalReservePct: 10, FcBandMinSharePct: "40:40:30"}, "fc_band_min_share_pct"},
		{"the stored reserve above 100 with new floors by replace", fcBandStored(),
			cmn.LbServiceArg{FcBandMinSharePct: "5:50:45", FcBandMinSharePctPresent: true}, "fc_critical_reserve_pct"},
		{"queue shares above 100, all three set", nil,
			cmn.LbServiceArg{FcBandQueuePct: "50:40:20"}, "fc_band_queue_pct"},
		{"shed queue on without a queue", nil, cmn.LbServiceArg{FcShedQueue: "on"}, "fc_shed_queue"},
		{"shed queue kept while a replace resets the depth", fcBandStored(),
			cmn.LbServiceArg{FcMaxQueueDepthPresent: true}, "fc_shed_queue"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := fcRuleResolve(c.eRule, &c.serv)
			var invalid *cmn.ValidationError
			if !errors.As(err, &invalid) || invalid.Field != c.field {
				t.Fatalf("err=%v, want a validation refusal naming %s", err, c.field)
			}
		})
	}

	for _, c := range []struct {
		name string
		env  string
		serv cmn.LbServiceArg
	}{
		{"weights declared under strict", "", cmn.LbServiceArg{FcWakePolicy: "strict", FcBandWeights: "8:3:1"}},
		{"weights declared under an inherited policy", "", cmn.LbServiceArg{FcBandWeights: "8:3:1"}},
		{"weighted on a full round", "", cmn.LbServiceArg{FcWakePolicy: "weighted", FcBandWeights: "62:1:1"}},
		{"floors at 100", "", cmn.LbServiceArg{FcBandMinSharePct: "50:30:20"}},
		{"the reserve and the other floors at 100", "", cmn.LbServiceArg{FcCriticalReservePct: 50, FcBandMinSharePct: "0:30:20"}},
		{"a reserve of 100 alone", "", cmn.LbServiceArg{FcCriticalReservePct: 100}},
		{"a reserve beside a critical floor", "", cmn.LbServiceArg{FcCriticalReservePct: 10, FcBandMinSharePct: "10:0:0"}},
		{"a critical floor above the reserve, the floors at 100", "", cmn.LbServiceArg{FcCriticalReservePct: 20, FcBandMinSharePct: "50:30:20"}},
		{"a floor on a band with a zero weight under weighted", "", cmn.LbServiceArg{FcWakePolicy: "weighted",
			FcBandWeights: "8:3:1", FcBandMinSharePct: "0:0:10"}},
		{"queue shares above 100 with one band shared", "", cmn.LbServiceArg{FcBandQueuePct: "60:60:0"}},
		{"queue shares at 100", "", cmn.LbServiceArg{FcBandQueuePct: "50:30:20"}},
		{"shed queue on with the rule's depth", "", cmn.LbServiceArg{FcShedQueue: "on", FcMaxQueueDepth: 4}},
		{"shed queue on with the process depth", "8", cmn.LbServiceArg{FcShedQueue: "on"}},
		{"band knobs with the gate off", "", cmn.LbServiceArg{FcMode: "off", FcWakePolicy: "weighted",
			FcBandWeights: "1:1:1", FcBandMinSharePct: "0:20:0", FcTenantWake: "rr"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LLB_FC_MAX_QUEUE_DEPTH", c.env)
			if _, err := fcRuleResolve(nil, &c.serv); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// The band knobs are refused where the kernel may carry the requests of a
// rule that is not an AI-gateway service, and only there; the refusal
// names the knob, a restore replay is let through, and fc_llmd_headers is
// not a band knob.
func TestFcBandSockMapErr(t *testing.T) {
	weighted := fcRuleCfg{wakePolicy: cmn.FcRuleWakeWeighted, bandWeight: [cmn.FcBands]uint32{1, 1, 1}}
	for _, c := range []struct {
		name   string
		cfg    fcRuleCfg
		code   uint8
		aiGw   bool
		replay bool
		field  string
	}{
		{"weighted, sockmap off", weighted, 0, false, false, ""},
		{"weighted, sockmap both", weighted, 1, false, false, "fc_wake_policy"},
		{"weighted, sockmap request", weighted, 2, false, false, "fc_wake_policy"},
		{"weighted, sockmap response", weighted, 3, false, false, ""},
		{"weighted, sockmap both, AI gateway", weighted, 1, true, false, ""},
		{"weighted, sockmap both, restore", weighted, 1, false, true, ""},
		{"weights", fcRuleCfg{bandWeight: [cmn.FcBands]uint32{1, 0, 0}}, 1, false, false, "fc_band_weights"},
		{"floors", fcRuleCfg{bandMinSharePct: [cmn.FcBands]uint32{0, 0, 5}}, 2, false, false, "fc_band_min_share_pct"},
		{"reserve", fcRuleCfg{criticalReservePct: 5}, 1, false, false, "fc_critical_reserve_pct"},
		{"waits", fcRuleCfg{bandWaitMs: [cmn.FcBands]uint32{0, 100, 0}}, 1, false, false, "fc_band_wait_ms"},
		{"queue shares", fcRuleCfg{bandQueuePct: [cmn.FcBands]uint32{0, 0, 10}}, 1, false, false, "fc_band_queue_pct"},
		{"shed queue on", fcRuleCfg{shedQueue: cmn.FcRuleAdaptiveOn}, 1, false, false, "fc_shed_queue"},
		{"tenant wake rr", fcRuleCfg{tenantWake: cmn.FcRuleTenantWakeRR}, 1, false, false, "fc_tenant_wake"},
		{"the defaults declared", fcRuleCfg{wakePolicy: cmn.FcRuleWakeStrict, shedQueue: cmn.FcRuleAdaptiveOff,
			tenantWake: cmn.FcRuleTenantWakeFifo}, 1, false, false, ""},
		{"llm-d headers on", fcRuleCfg{llmdHeaders: cmn.FcRuleAdaptiveOn}, 1, false, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			serv := cmn.LbServiceArg{ServIP: "10.0.0.1", ServPort: 80,
				SockMapMode: cmn.SockMapCodeToMode(c.code), RestoreReplay: c.replay}
			err := fcBandSockMapErr(&serv, c.cfg, c.code, c.aiGw)
			if c.field == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var invalid *cmn.ValidationError
			if !errors.As(err, &invalid) || invalid.Field != c.field {
				t.Fatalf("err=%v, want a validation refusal naming %s", err, c.field)
			}
		})
	}
}

// The read-back's floors are in units against the ceiling in force,
// rounded up, and each weight is its share of the round in percent.
func TestFcBandEffective(t *testing.T) {
	for _, c := range []struct{ ceiling, pct, want uint32 }{
		{10, 15, 2}, {10, 10, 1}, {7, 100, 7}, {3, 1, 1}, {0, 50, 0}, {10, 0, 0},
		{4, 25, 1}, {1, 50, 0}, {2, 99, 1}, {10, 100, 10},
	} {
		if got := fcFloorUnits(c.ceiling, c.pct); got != c.want {
			t.Fatalf("floor of %d%% of %d = %d, want %d", c.pct, c.ceiling, got, c.want)
		}
	}
	st := fcBandStat{
		inflight:    [cmn.FcBands]uint32{4, 2, 1},
		queued:      [cmn.FcBands]uint32{3, 0, 5},
		waitMs:      [cmn.FcBands]uint32{2000, 500, 0},
		queuePct:    [cmn.FcBands]uint32{50, 30, 20},
		minSharePct: [cmn.FcBands]uint32{20, 15, 0},
		weight:      [cmn.FcBands]uint32{3, 8, 1},
	}
	b := st.effective(10)
	if b.Critical != (cmn.FcBandArg{Inflight: 2, Queued: 0, WaitMs: 500, QueuePct: 30, MinSharePct: 15,
		FloorUnits: 2, Weight: 8, WeightPct: 67}) {
		t.Fatalf("critical %+v", b.Critical)
	}
	if b.Standard != (cmn.FcBandArg{Inflight: 4, Queued: 3, WaitMs: 2000, QueuePct: 50, MinSharePct: 20,
		FloorUnits: 2, Weight: 3, WeightPct: 25}) {
		t.Fatalf("standard %+v", b.Standard)
	}
	if b.Sheddable != (cmn.FcBandArg{Inflight: 1, Queued: 5, QueuePct: 20, Weight: 1, WeightPct: 8}) {
		t.Fatalf("sheddable %+v", b.Sheddable)
	}
	if none := (fcBandStat{}).effective(0); none != (cmn.FcBandsArg{}) {
		t.Fatalf("no bands read back %+v", none)
	}
}

// A rule from a 1.12 snapshot written before the band knobs existed has no
// band field: it resolves to every default, and replaying it over itself
// (a restore, or a replace with nothing band-related present) resolves to
// the same declaration, so a missing field is the default and not a change
// the in-place comparison would act on.
func TestFcRuleBandAbsentIsDefault(t *testing.T) {
	t.Setenv("LLB_FC_MAX_QUEUE_DEPTH", "")
	raw, err := os.ReadFile("../snapshot/testdata/snapshot-v1.12.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Domains struct {
			LoadBalancer []cmn.LbRuleMod `json:"loadbalancer"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Domains.LoadBalancer) == 0 {
		t.Fatal("the 1.12 document carries no load-balancer rule")
	}
	serv := doc.Domains.LoadBalancer[0].Serv
	serv.RestoreReplay = true
	first, err := fcRuleResolve(nil, &serv)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if first != (fcRuleCfg{}) || first.bandKnobDeclared() != "" {
		t.Fatalf("a rule without band knobs resolved to %+v", first)
	}
	again, err := fcRuleResolve(&ruleEnt{fcCfg: first}, &serv)
	if err != nil {
		t.Fatalf("refused on replay: %v", err)
	}
	if again != first {
		t.Fatalf("replayed as %+v, first %+v", again, first)
	}
	// Over a rule that declares the bands, the same document keeps them.
	stored := fcBandStored()
	kept, err := fcRuleResolve(stored, &serv)
	if err != nil {
		t.Fatalf("refused over a declared rule: %v", err)
	}
	if kept != stored.fcCfg {
		t.Fatalf("a document without band knobs changed the declared bands to %+v", kept)
	}
}

// The claims the read-back reports are the ones the data plane enforces:
// the same vectors as the C test of fc_floor_claims (test_fc_band_floor).
func TestFcFloorClaims(t *testing.T) {
	for _, c := range []struct {
		name       string
		ceiling    uint32
		pct        [cmn.FcBands]uint32 // indexed by band: standard, critical, sheddable
		reservePct uint32
		floor      [cmn.FcBands]uint32
		reserve    uint32
	}{
		{"rounding is clamped in rank order", 4, [cmn.FcBands]uint32{33, 34, 33}, 0, [cmn.FcBands]uint32{2, 2, 0}, 0},
		{"a reserve alone", 4, [cmn.FcBands]uint32{}, 25, [cmn.FcBands]uint32{}, 1},
		{"critical holds the larger of floor and reserve", 10, [cmn.FcBands]uint32{0, 10, 0}, 30, [cmn.FcBands]uint32{0, 1, 0}, 3},
		{"no ceiling", 0, [cmn.FcBands]uint32{10, 10, 10}, 10, [cmn.FcBands]uint32{}, 0},
		{"a reserve of the whole ceiling", 6, [cmn.FcBands]uint32{20, 0, 20}, 100, [cmn.FcBands]uint32{}, 6},
	} {
		t.Run(c.name, func(t *testing.T) {
			floor, reserve := fcFloorClaims(c.ceiling, c.pct, c.reservePct)
			if floor != c.floor || reserve != c.reserve {
				t.Fatalf("floor %v reserve %d, want %v %d", floor, reserve, c.floor, c.reserve)
			}
		})
	}
	// The read-back carries the reserve beside the bands.
	b := fcBandStat{reservePct: 25}.effective(4)
	if b.CriticalReservePct != 25 || b.CriticalReserveUnits != 1 {
		t.Fatalf("reserve %d%% = %d units", b.CriticalReservePct, b.CriticalReserveUnits)
	}
}

// A reserve above 100 is refused for being above 100 (its own rule), not only
// as the sum with the other floors.
func TestFcBandReserveAbove100NamesItsOwnRule(t *testing.T) {
	_, err := fcRuleResolve(nil, &cmn.LbServiceArg{FcCriticalReservePct: 101})
	if err == nil || !strings.Contains(err.Error(), "within 0..") {
		t.Fatalf("err=%v, want the reserve's own bound named", err)
	}
}
