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

package common

import (
	"fmt"
	"strconv"
	"strings"
)

// FcQueuePairError holds the one rule that spans a service's two capacity
// queue fields: a depth without a wait window would park a request forever,
// so the window is required with it. It is judged on the values that will be
// stored, which on a replace or a PATCH means after the request is merged
// with the stored rule: an update may carry one field and keep the other.
func FcQueuePairError(depth, waitMs uint32) error {
	if depth > 0 && waitMs == 0 {
		return NewValidationError("fc_max_queue_wait_ms",
			"fc_max_queue_wait_ms must be greater than 0 when fc_max_queue_depth is set")
	}
	return nil
}

// FcCapMax bounds a rule's service and per-endpoint admission ceilings.
const FcCapMax = 100000

// FcTelemetryStaleMsMax bounds a rule's telemetry window (an hour).
const FcTelemetryStaleMsMax = 3600000

// FcWarmupMsMax bounds a rule's endpoint warm-up window (an hour).
const FcWarmupMsMax = 3600000

// FcTtftTargetMsMax bounds a rule's TTFT target (an hour).
const FcTtftTargetMsMax = 3600000

// FcTenantMaxSharePctMax bounds a rule's tenant share, a percentage.
const FcTenantMaxSharePctMax = 100

// The tenant-table size a model pool is created with (LLB_FC_TENANT_SLOTS):
// the same range and default as FC_TENANT_SLOTS, FC_TENANT_SLOTS_DEFAULT and
// FC_TENANT_SLOTS_MAX in the data plane's sockproxy_fc.h.
const (
	FcTenantSlotsMin     = 64
	FcTenantSlotsDefault = 256
	FcTenantSlotsMax     = 4096
)

// The data plane's encoding of a rule's adaptive switch (enum
// fc_rule_adaptive), shifted like the mode so a rule can say "off" under an
// environment that has it on.
const (
	FcRuleAdaptiveInherit uint8 = iota
	FcRuleAdaptiveOff
	FcRuleAdaptiveOn
)

// FcAdaptiveToRule maps a rule's fc_adaptive to the data plane's encoding.
// Empty and "inherit" both mean the process default; anything else is
// refused.
func FcAdaptiveToRule(v string) (uint8, error) {
	switch v {
	case "", "inherit":
		return FcRuleAdaptiveInherit, nil
	case "off":
		return FcRuleAdaptiveOff, nil
	case "on":
		return FcRuleAdaptiveOn, nil
	}
	return 0, NewValidationError("fc_adaptive",
		"fc_adaptive must be one of on, off or inherit")
}

// FcExposeHeadersToRule maps a rule's fc_expose_headers to the data plane's
// encoding (enum fc_rule_expose), shifted like the adaptive switch. Empty and
// "inherit" both mean the process default; anything else is refused.
func FcExposeHeadersToRule(v string) (uint8, error) {
	switch v {
	case "", "inherit":
		return FcRuleAdaptiveInherit, nil
	case "off":
		return FcRuleAdaptiveOff, nil
	case "on":
		return FcRuleAdaptiveOn, nil
	}
	return 0, NewValidationError("fc_expose_headers",
		"fc_expose_headers must be one of on, off or inherit")
}

// FcAdaptiveFromRule is FcAdaptiveToRule's inverse for the read-back: the
// process default reads as empty, so it is omitted.
func FcAdaptiveFromRule(a uint8) string {
	switch a {
	case FcRuleAdaptiveOff:
		return "off"
	case FcRuleAdaptiveOn:
		return "on"
	}
	return ""
}

// The data plane's encoding of a rule's gate mode (enum fc_rule_mode): 0
// runs on the process default, the others are shifted by one so a rule can
// say "off" under an enforcing environment.
const (
	FcRuleModeInherit uint8 = iota
	FcRuleModeOff
	FcRuleModeObserve
	FcRuleModeEnforce
)

// FcModeToRule maps a rule's fc_mode to the data plane's encoding. Empty
// and "inherit" both mean the process default; anything else is refused.
func FcModeToRule(mode string) (uint8, error) {
	switch mode {
	case "", "inherit":
		return FcRuleModeInherit, nil
	case "off":
		return FcRuleModeOff, nil
	case "observe":
		return FcRuleModeObserve, nil
	case "enforce":
		return FcRuleModeEnforce, nil
	}
	return 0, NewValidationError("fc_mode",
		"fc_mode must be one of off, observe, enforce or inherit")
}

// FcModeFromRule is FcModeToRule's inverse for the read-back: the process
// default reads as empty, so it is omitted.
func FcModeFromRule(m uint8) string {
	switch m {
	case FcRuleModeOff:
		return "off"
	case FcRuleModeObserve:
		return "observe"
	case FcRuleModeEnforce:
		return "enforce"
	}
	return ""
}

// The priority bands, indexed as the data plane indexes every band triple
// (FC_BAND_* in sockproxy_fc.h, the request class constants). The REST
// triples are written critical:standard:sheddable, highest band first.
const (
	FcBandStandard = iota
	FcBandCritical
	FcBandSheddable
	FcBands
)

// fcBandWireOrder maps a triple's positions, critical:standard:sheddable,
// onto the band index.
var fcBandWireOrder = [FcBands]int{FcBandCritical, FcBandStandard, FcBandSheddable}

// Bounds of a rule's band knobs: a weight is at most 62 (0 only in a
// 0:0:0 triple, judged in the rule layer) and a round, the sum of
// the three, at most 64 (FC_BAND_WEIGHT_MAX, FC_BAND_ROUND_MAX); a band's
// wait window at most an hour; a percentage at most 100.
const (
	FcBandWeightMax = 62
	FcBandRoundMax  = 64
	FcBandWaitMsMax = 3600000
	FcBandPctMax    = 100
)

// FcBandTripleParse reads a band triple, "c:s:b": exactly three
// non-negative whole numbers, each at most limit, separated by ':'. The
// result is indexed by band (FcBand*). Empty is three zeros, the reset
// value; anything else is refused with the field named.
func FcBandTripleParse(field, v string, limit uint32) ([FcBands]uint32, error) {
	var t [FcBands]uint32
	if v == "" {
		return t, nil
	}
	parts := strings.Split(v, ":")
	if len(parts) != FcBands {
		return t, NewValidationError(field,
			"%s must be three whole numbers critical:standard:sheddable, got %q", field, v)
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return t, NewValidationError(field,
				"%s must be three whole numbers critical:standard:sheddable, got %q", field, v)
		}
		if n > uint64(limit) {
			return t, NewValidationError(field,
				"%s: each value must be within 0..%d, got %q", field, limit, v)
		}
		t[fcBandWireOrder[i]] = uint32(n)
	}
	return t, nil
}

// FcBandTripleFormat is FcBandTripleParse's inverse for the read-back:
// three zeros, the reset value, read as empty, so they are omitted.
func FcBandTripleFormat(t [FcBands]uint32) string {
	if t == ([FcBands]uint32{}) {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d", t[FcBandCritical], t[FcBandStandard], t[FcBandSheddable])
}

// The data plane's encoding of a rule's wake policy (enum fc_rule_wake),
// shifted like the mode so a rule can say "strict" under an environment
// that has weighted.
const (
	FcRuleWakeInherit uint8 = iota
	FcRuleWakeStrict
	FcRuleWakeWeighted
)

// FcWakePolicyToRule maps a rule's fc_wake_policy to the data plane's
// encoding. Empty and "inherit" both mean the process default; anything
// else is refused.
func FcWakePolicyToRule(v string) (uint8, error) {
	switch v {
	case "", "inherit":
		return FcRuleWakeInherit, nil
	case "strict":
		return FcRuleWakeStrict, nil
	case "weighted":
		return FcRuleWakeWeighted, nil
	}
	return 0, NewValidationError("fc_wake_policy",
		"fc_wake_policy must be one of strict, weighted or inherit")
}

// FcWakePolicyFromRule is FcWakePolicyToRule's inverse for the read-back:
// the process default reads as empty, so it is omitted.
func FcWakePolicyFromRule(w uint8) string {
	switch w {
	case FcRuleWakeStrict:
		return "strict"
	case FcRuleWakeWeighted:
		return "weighted"
	}
	return ""
}

// The data plane's encoding of a rule's in-band order (enum
// fc_rule_tenant_wake), the same shift.
const (
	FcRuleTenantWakeInherit uint8 = iota
	FcRuleTenantWakeFifo
	FcRuleTenantWakeRR
)

// FcTenantWakeToRule maps a rule's fc_tenant_wake to the data plane's
// encoding. Empty and "inherit" both mean the process default; anything
// else is refused.
func FcTenantWakeToRule(v string) (uint8, error) {
	switch v {
	case "", "inherit":
		return FcRuleTenantWakeInherit, nil
	case "fifo":
		return FcRuleTenantWakeFifo, nil
	case "rr":
		return FcRuleTenantWakeRR, nil
	}
	return 0, NewValidationError("fc_tenant_wake",
		"fc_tenant_wake must be one of fifo, rr or inherit")
}

// FcTenantWakeFromRule is FcTenantWakeToRule's inverse for the read-back:
// the process default reads as empty, so it is omitted.
func FcTenantWakeFromRule(w uint8) string {
	switch w {
	case FcRuleTenantWakeFifo:
		return "fifo"
	case FcRuleTenantWakeRR:
		return "rr"
	}
	return ""
}

// FcSwitchToRule maps an on/off switch of the bands (fc_shed_queue,
// fc_llmd_headers) to the data plane's encoding (enum fc_rule_switch), the
// adaptive switch's: empty and "inherit" both mean the process default;
// anything else is refused with the field named. FcAdaptiveFromRule reads
// it back.
func FcSwitchToRule(field, v string) (uint8, error) {
	switch v {
	case "", "inherit":
		return FcRuleAdaptiveInherit, nil
	case "off":
		return FcRuleAdaptiveOff, nil
	case "on":
		return FcRuleAdaptiveOn, nil
	}
	return 0, NewValidationError(field, "%s must be one of on, off or inherit", field)
}
