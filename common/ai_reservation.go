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

// The default reservation: what the admission gate claims against a
// tenant's token quota for the completion of a request that declares no
// max_tokens. Off, a request without the field claims its prompt estimate
// alone, so leaving the field out is the cheaper choice at admission. On,
// the default is claimed beside the prompt estimate as an elastic part: it
// is shrunk to what the quota still holds and is never the reason a request
// is refused. Nothing is written into the forwarded request.

// The switch (ai_default_reservation), as the API carries it and as a rule
// stores it. Unset runs on the environment, else off.
const (
	AIDefaultReservationOff     = "off"
	AIDefaultReservationOn      = "on"
	AIDefaultReservationInherit = "inherit"

	AIDefaultReservationRuleUnset uint8 = 0
	AIDefaultReservationRuleOff   uint8 = 1
	AIDefaultReservationRuleOn    uint8 = 2
)

const (
	// AIDefaultMaxTokensBuiltin is the default reservation when it is on and
	// neither the rule nor the environment gives a value. An estimate of a
	// typical completion, not a measured one.
	AIDefaultMaxTokensBuiltin uint32 = 512
	// AIDefaultMaxTokensMax bounds ai_default_max_tokens: the largest
	// ceiling the gate reads from a request body.
	AIDefaultMaxTokensMax uint32 = 100000000

	// The environment's two values, read where a rule leaves its own unset.
	AIDefaultReservationEnv = "LLB_AI_DEFAULT_RESERVATION"
	AIDefaultMaxTokensEnv   = "LLB_AI_DEFAULT_MAX_TOKENS"
)

// Where a value of the default reservation in force came from.
const (
	AIDefaultSourceRule    = "rule"
	AIDefaultSourceEnv     = "env"
	AIDefaultSourceDefault = "default"
	// AIDefaultSourceListener: the value in force is not this rule's own
	// but the listener's, set by the first rule installed on the VIP and
	// port. The gate reserves before a rule is selected.
	AIDefaultSourceListener = "listener"
)

// AIDefaultReservationToRule maps a rule's ai_default_reservation to what
// the rule stores. Empty and "inherit" both mean unset; anything else is
// refused.
func AIDefaultReservationToRule(v string) (uint8, error) {
	switch v {
	case "", AIDefaultReservationInherit:
		return AIDefaultReservationRuleUnset, nil
	case AIDefaultReservationOff:
		return AIDefaultReservationRuleOff, nil
	case AIDefaultReservationOn:
		return AIDefaultReservationRuleOn, nil
	}
	return 0, NewValidationError("ai_default_reservation",
		"ai_default_reservation must be one of on, off or inherit")
}

// AIDefaultReservationFromRule is AIDefaultReservationToRule's inverse for
// the read-back: unset reads as empty, so it is omitted.
func AIDefaultReservationFromRule(m uint8) string {
	switch m {
	case AIDefaultReservationRuleOff:
		return AIDefaultReservationOff
	case AIDefaultReservationRuleOn:
		return AIDefaultReservationOn
	}
	return ""
}

// AIDefaultMaxTokensError refuses an ai_default_max_tokens above the bound.
func AIDefaultMaxTokensError(v uint32) error {
	if v > AIDefaultMaxTokensMax {
		return NewValidationError("ai_default_max_tokens",
			"ai_default_max_tokens must be within 0..%d", AIDefaultMaxTokensMax)
	}
	return nil
}
